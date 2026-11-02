package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------- zip package helpers (shared by Word, Excel, PowerPoint) ----------

type ooxmlPkg struct {
	names []string
	files map[string][]byte
}

func readPkg(p string) (*ooxmlPkg, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, fmt.Errorf("couldn't open %s: %v", filepath.Base(p), err)
	}
	defer zr.Close()
	k := &ooxmlPkg{files: map[string][]byte{}}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 512<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		k.names = append(k.names, f.Name)
		k.files[f.Name] = b
	}
	return k, nil
}

func (k *ooxmlPkg) get(n string) string { return string(k.files[n]) }

func (k *ooxmlPkg) set(n string, b []byte) {
	if _, ok := k.files[n]; !ok {
		k.names = append(k.names, n)
	}
	k.files[n] = b
}

func (k *ooxmlPkg) del(n string) {
	if _, ok := k.files[n]; !ok {
		return
	}
	delete(k.files, n)
	for i, x := range k.names {
		if x == n {
			k.names = append(k.names[:i], k.names[i+1:]...)
			break
		}
	}
}

func (k *ooxmlPkg) bytes() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// [Content_Types].xml first, as Office expects.
	order := append([]string{}, k.names...)
	for i, n := range order {
		if n == "[Content_Types].xml" && i > 0 {
			order = append([]string{n}, append(order[:i:i], order[i+1:]...)...)
			break
		}
	}
	now := time.Now()
	for _, n := range order {
		// Everything deflated: stored entries with data descriptors don't open in some readers.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: now})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(k.files[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ensureDefault adds a <Default Extension> content type if missing.
func (k *ooxmlPkg) ensureDefault(ext, ctype string) {
	ct := k.get("[Content_Types].xml")
	if strings.Contains(strings.ToLower(ct), `extension="`+ext+`"`) {
		return
	}
	ct = strings.Replace(ct, "<Default ", `<Default Extension="`+ext+`" ContentType="`+ctype+`"/><Default `, 1)
	k.set("[Content_Types].xml", []byte(ct))
}

func (k *ooxmlPkg) ensureOverride(part, ctype string) {
	ct := k.get("[Content_Types].xml")
	if strings.Contains(ct, `PartName="/`+part+`"`) {
		return
	}
	ct = strings.Replace(ct, "</Types>", `<Override PartName="/`+part+`" ContentType="`+ctype+`"/></Types>`, 1)
	k.set("[Content_Types].xml", []byte(ct))
}

func (k *ooxmlPkg) addRel(relsPart, id, typ, target string, external bool) {
	r := k.get(relsPart)
	if r == "" {
		r = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`
	}
	mode := ""
	if external {
		mode = ` TargetMode="External"`
	}
	r = strings.Replace(r, "</Relationships>", `<Relationship Id="`+id+`" Type="`+typ+`" Target="`+xmlEsc(target)+`"`+mode+`/></Relationships>`, 1)
	k.set(relsPart, []byte(r))
}

const (
	relImage = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/image"
	relLink  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"
)

var imageCT = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "jpg": "image/jpeg", "gif": "image/gif", "bmp": "image/bmp"}

// loadImage reads a picture and its size in EMU at 96 dpi, scaled to fit maxW x maxH.
func loadImage(p string, maxW, maxH int64) (data []byte, ext string, cx, cy int64, err error) {
	data, err = os.ReadFile(p)
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("image %s: %v", p, cleanErr(err))
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("image %s: only PNG, JPEG and GIF can be placed", p)
	}
	ext = format
	if ext == "jpeg" {
		ext = "jpg"
	}
	cx, cy = int64(cfg.Width)*9525, int64(cfg.Height)*9525
	if cx > maxW {
		cy = cy * maxW / cx
		cx = maxW
	}
	if cy > maxH {
		cx = cx * maxH / cy
		cy = maxH
	}
	return data, ext, cx, cy, nil
}

// ---------- Word ----------

const (
	nsW    = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	nsR    = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	nsWP   = `xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"`
	nsA    = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`
	nsPic  = `xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"`
	xmlHdr = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
)

type docxCtx struct {
	pkg     *ooxmlPkg
	base    string
	n       int
	nums    []string // <w:num> elements to add
	numBase int
	textW   int64 // usable width, EMU
	twips   int   // usable width, twips
}

func (c *docxCtx) id() string { c.n++; return fmt.Sprintf("rIdTs%d", c.n) }

func (c *docxCtx) runs(sp []mdSpan) string {
	var b strings.Builder
	for _, s := range sp {
		if s.text == "" {
			continue
		}
		var pr strings.Builder
		if s.link != "" {
			pr.WriteString(`<w:rStyle w:val="Hyperlink"/>`)
		}
		if s.code {
			pr.WriteString(`<w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/>`)
		}
		if s.bold {
			pr.WriteString(`<w:b/>`)
		}
		if s.italic {
			pr.WriteString(`<w:i/>`)
		}
		r := "<w:r>"
		if pr.Len() > 0 {
			r += "<w:rPr>" + pr.String() + "</w:rPr>"
		}
		r += `<w:t xml:space="preserve">` + xmlEsc(s.text) + "</w:t></w:r>"
		if s.link != "" {
			id := c.id()
			c.pkg.addRel("word/_rels/document.xml.rels", id, relLink, s.link, true)
			r = `<w:hyperlink r:id="` + id + `">` + r + "</w:hyperlink>"
		}
		b.WriteString(r)
	}
	return b.String()
}

func (c *docxCtx) para(style, extra string, sp []mdSpan) string {
	pPr := ""
	if style != "" || extra != "" {
		pPr = "<w:pPr>"
		if style != "" {
			pPr += `<w:pStyle w:val="` + style + `"/>`
		}
		pPr += extra + "</w:pPr>"
	}
	return "<w:p>" + pPr + c.runs(sp) + "</w:p>"
}

func (c *docxCtx) body(blocks []mdBlock) (string, error) {
	var b strings.Builder
	curNum := 0
	for i, bl := range blocks {
		if bl.kind != "ol" {
			curNum = 0
		}
		switch bl.kind {
		case "h":
			lv := min(bl.level, 6)
			b.WriteString(c.para(fmt.Sprintf("Heading%d", lv), "", bl.spans))
		case "p":
			b.WriteString(c.para("", "", bl.spans))
		case "ul":
			b.WriteString(c.para("ListParagraph", fmt.Sprintf(`<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="%d"/></w:numPr>`, min(bl.level, 8), c.numBase), bl.spans))
		case "ol":
			if curNum == 0 || (i > 0 && blocks[i-1].kind != "ol" && blocks[i-1].kind != "ul") {
				curNum = c.numBase + 1 + len(c.nums)
				c.nums = append(c.nums, fmt.Sprintf(`<w:num w:numId="%d"><w:abstractNumId w:val="%d"/><w:lvlOverride w:ilvl="0"><w:startOverride w:val="1"/></w:lvlOverride></w:num>`, curNum, c.numBase+1))
			}
			b.WriteString(c.para("ListParagraph", fmt.Sprintf(`<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="%d"/></w:numPr>`, min(bl.level, 8), curNum), bl.spans))
		case "code":
			for _, l := range bl.lines {
				b.WriteString(c.para("TsCode", "", []mdSpan{{text: l}}))
			}
		case "break":
			b.WriteString(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
		case "table":
			cols := 0
			for _, r := range bl.rows {
				cols = max(cols, len(r))
			}
			if cols == 0 {
				continue
			}
			w := c.twips / cols
			b.WriteString(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:w="0" w:type="auto"/><w:tblLook w:val="04A0" w:firstRow="1" w:lastRow="0" w:firstColumn="1" w:lastColumn="0" w:noHBand="0" w:noVBand="1"/></w:tblPr><w:tblGrid>`)
			for j := 0; j < cols; j++ {
				fmt.Fprintf(&b, `<w:gridCol w:w="%d"/>`, w)
			}
			b.WriteString("</w:tblGrid>")
			for ri, r := range bl.rows {
				b.WriteString("<w:tr>")
				if ri == 0 {
					b.WriteString(`<w:trPr><w:tblHeader/></w:trPr>`)
				}
				for j := 0; j < cols; j++ {
					var sp []mdSpan
					if j < len(r) {
						sp = r[j]
					}
					if ri == 0 {
						sp = append([]mdSpan{}, sp...)
						for k := range sp {
							sp[k].bold = true
						}
					}
					fmt.Fprintf(&b, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/></w:tcPr>%s</w:tc>`, w, c.para("", `<w:spacing w:after="0"/>`, sp))
				}
				b.WriteString("</w:tr>")
			}
			b.WriteString("</w:tbl><w:p/>")
		case "img":
			ip, err := officeImagePath(bl.path, c.base)
			if err != nil {
				return "", err
			}
			data, ext, cx, cy, err := loadImage(ip, c.textW, 8*914400)
			if err != nil {
				return "", err
			}
			c.n++
			name := fmt.Sprintf("ts%d_%s.%s", c.n, randHex(3), ext)
			c.pkg.set("word/media/"+name, data)
			c.pkg.ensureDefault(ext, imageCT[ext])
			id := c.id()
			c.pkg.addRel("word/_rels/document.xml.rels", id, relImage, "media/"+name, false)
			pid := 5000 + c.n
			fmt.Fprintf(&b, `<w:p><w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0"><wp:extent cx="%d" cy="%d"/><wp:docPr id="%d" name="Picture %d"/><a:graphic %s><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic %s><pic:nvPicPr><pic:cNvPr id="%d" name="%s"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`,
				cx, cy, pid, pid, nsA, nsPic, pid, name, id, cx, cy)
		}
	}
	return b.String(), nil
}

const docxStyleDefs = `<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:after="120"/></w:pPr><w:rPr><w:sz w:val="52"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="360" w:after="120"/><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:color w:val="1F3864"/><w:sz w:val="36"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="240" w:after="80"/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:color w:val="2F5496"/><w:sz w:val="30"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="200" w:after="60"/><w:outlineLvl w:val="2"/></w:pPr><w:rPr><w:b/><w:color w:val="2F5496"/><w:sz w:val="26"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading4"><w:name w:val="heading 4"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="160" w:after="40"/><w:outlineLvl w:val="3"/></w:pPr><w:rPr><w:b/><w:i/><w:sz w:val="24"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading5"><w:name w:val="heading 5"/><w:basedOn w:val="Heading4"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:outlineLvl w:val="4"/></w:pPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading6"><w:name w:val="heading 6"/><w:basedOn w:val="Heading4"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:outlineLvl w:val="5"/></w:pPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:after="60"/><w:ind w:left="720"/><w:contextualSpacing/></w:pPr></w:style>` +
	`<w:style w:type="paragraph" w:customStyle="1" w:styleId="TsCode"><w:name w:val="Code"/><w:basedOn w:val="Normal"/><w:pPr><w:shd w:val="clear" w:color="auto" w:fill="F2F2F2"/><w:spacing w:after="0" w:line="240" w:lineRule="auto"/></w:pPr><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/><w:sz w:val="20"/></w:rPr></w:style>` +
	`<w:style w:type="character" w:styleId="Hyperlink"><w:name w:val="Hyperlink"/><w:rPr><w:color w:val="0563C1"/><w:u w:val="single"/></w:rPr></w:style>` +
	`<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:tblPr><w:tblBorders><w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:left w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:bottom w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:right w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:insideH w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:insideV w:val="single" w:sz="4" w:space="0" w:color="auto"/></w:tblBorders><w:tblCellMar><w:left w:w="108" w:type="dxa"/><w:right w:w="108" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style>`

func docxAbstractNums(base int) string {
	var b strings.Builder
	bullets := []string{"•", "o", "▪"}
	fmt.Fprintf(&b, `<w:abstractNum w:abstractNumId="%d"><w:multiLevelType w:val="hybridMultilevel"/>`, base)
	for l := 0; l < 9; l++ {
		font := `<w:rFonts w:ascii="Symbol" w:hAnsi="Symbol" w:hint="default"/>`
		ch := ""
		if bullets[l%3] == "o" {
			font, ch = `<w:rFonts w:ascii="Courier New" w:hAnsi="Courier New" w:cs="Courier New" w:hint="default"/>`, "o"
		} else if bullets[l%3] == "▪" {
			font, ch = `<w:rFonts w:ascii="Wingdings" w:hAnsi="Wingdings" w:hint="default"/>`, ""
		}
		fmt.Fprintf(&b, `<w:lvl w:ilvl="%d"><w:start w:val="1"/><w:numFmt w:val="bullet"/><w:lvlText w:val="%s"/><w:lvlJc w:val="left"/><w:pPr><w:ind w:left="%d" w:hanging="360"/></w:pPr><w:rPr>%s</w:rPr></w:lvl>`, l, ch, 720*(l+1), font)
	}
	b.WriteString("</w:abstractNum>")
	fmt.Fprintf(&b, `<w:abstractNum w:abstractNumId="%d"><w:multiLevelType w:val="hybridMultilevel"/>`, base+1)
	fmts := []string{"decimal", "lowerLetter", "lowerRoman"}
	for l := 0; l < 9; l++ {
		fmt.Fprintf(&b, `<w:lvl w:ilvl="%d"><w:start w:val="1"/><w:numFmt w:val="%s"/><w:lvlText w:val="%%%d."/><w:lvlJc w:val="left"/><w:pPr><w:ind w:left="%d" w:hanging="360"/></w:pPr></w:lvl>`, l, fmts[l%3], l+1, 720*(l+1))
	}
	b.WriteString("</w:abstractNum>")
	return b.String()
}

func newDocxCtx(pkg *ooxmlPkg, base string, landscape bool, numBase int) *docxCtx {
	c := &docxCtx{pkg: pkg, base: base, numBase: numBase, textW: 6 * 914400, twips: 9360}
	if landscape {
		c.textW, c.twips = 9*914400, 12960
	}
	return c
}

func docxCreate(a officeArgs) ([]byte, error) {
	pkg := &ooxmlPkg{files: map[string][]byte{}}
	pkg.set("[Content_Types].xml", []byte(xmlHdr+`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/><Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/><Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/></Types>`))
	pkg.set("_rels/.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`))
	pkg.set("word/_rels/document.xml.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/settings" Target="settings.xml"/></Relationships>`))
	pkg.set("word/settings.xml", []byte(xmlHdr+`<w:settings `+nsW+`><w:defaultTabStop w:val="720"/><w:compat><w:compatSetting w:name="compatibilityMode" w:uri="http://schemas.microsoft.com/office/word" w:val="15"/></w:compat></w:settings>`))
	pkg.set("word/styles.xml", []byte(xmlHdr+`<w:styles `+nsW+`><w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:eastAsia="Calibri" w:cs="Calibri"/><w:sz w:val="22"/><w:szCs w:val="22"/><w:lang w:val="en-US"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:spacing w:after="160" w:line="259" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style>`+docxStyleDefs+`</w:styles>`))
	c := newDocxCtx(pkg, a.Path, a.Landscape, 1)
	body, err := c.body(parseMarkdown(a.Content))
	if err != nil {
		return nil, err
	}
	pkg.set("word/numbering.xml", []byte(xmlHdr+`<w:numbering `+nsW+`>`+docxAbstractNums(1)+`<w:num w:numId="1"><w:abstractNumId w:val="1"/></w:num>`+strings.Join(c.nums, "")+`</w:numbering>`))
	sect := `<w:sectPr><w:pgSz w:w="12240" w:h="15840"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="720" w:footer="720" w:gutter="0"/></w:sectPr>`
	if a.Landscape {
		sect = `<w:sectPr><w:pgSz w:w="15840" w:h="12240" w:orient="landscape"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="720" w:footer="720" w:gutter="0"/></w:sectPr>`
	}
	pkg.set("word/document.xml", []byte(xmlHdr+`<w:document `+nsW+` `+nsR+` `+nsWP+` `+nsA+` `+nsPic+`><w:body>`+body+sect+`</w:body></w:document>`))
	return pkg.bytes()
}

// ---------- editing Word ----------

var (
	wParaRe = regexp.MustCompile(`(?s)<w:p(?:\s[^>]*)?>.*?</w:p>|<w:p(?:\s[^>]*)?/>`)
	wTRe    = regexp.MustCompile(`(?s)<w:t(?:\s[^>]*)?>(.*?)</w:t>|<w:t(?:\s[^>]*)?/>`)
	attrRe  = regexp.MustCompile(`\s([\w:]+)="([^"]*)"`)
)

func xmlUnesc(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	d := xml.NewDecoder(strings.NewReader("<x>" + s + "</x>"))
	for {
		t, err := d.Token()
		if err != nil {
			break
		}
		if c, ok := t.(xml.CharData); ok {
			b.Write(c)
		}
	}
	return b.String()
}

// replaceInRuns replaces text that may be split over several runs, keeping each
// run's formatting: the replacement goes into the run where the match starts.
// tRe must capture the run text in group 1; open builds the opening tag.
func replaceInRuns(frag string, tRe *regexp.Regexp, open string, close string, reps []officeReplace, count *int) string {
	locs := tRe.FindAllStringSubmatchIndex(frag, -1)
	if len(locs) == 0 {
		return frag
	}
	texts := make([]string, len(locs))
	var joined strings.Builder
	var owner []int
	for i, m := range locs {
		if m[2] >= 0 {
			texts[i] = xmlUnesc(frag[m[2]:m[3]])
		}
		for range []rune(texts[i]) {
			owner = append(owner, i)
		}
		joined.WriteString(texts[i])
	}
	full := []rune(joined.String())
	changed := false
	for _, r := range reps {
		if r.Find == "" {
			continue
		}
		find := []rune(r.Find)
		if !strings.Contains(string(full), r.Find) {
			continue
		}
		var nf []rune
		var no []int
		for i := 0; i < len(full); {
			if i+len(find) <= len(full) && string(full[i:i+len(find)]) == r.Find {
				for _, c := range []rune(r.With) {
					nf = append(nf, c)
					no = append(no, owner[i])
				}
				i += len(find)
				*count++
				changed = true
				continue
			}
			nf = append(nf, full[i])
			no = append(no, owner[i])
			i++
		}
		full, owner = nf, no
	}
	if !changed {
		return frag
	}
	newTexts := make([]strings.Builder, len(locs))
	for i, c := range full {
		newTexts[owner[i]].WriteRune(c)
	}
	var b strings.Builder
	last := 0
	for i, m := range locs {
		b.WriteString(frag[last:m[0]])
		b.WriteString(open + xmlEsc(newTexts[i].String()) + close)
		last = m[1]
	}
	b.WriteString(frag[last:])
	return b.String()
}

func paraText(p string) string {
	var b strings.Builder
	for _, m := range wTRe.FindAllStringSubmatch(p, -1) {
		b.WriteString(xmlUnesc(m[1]))
	}
	return b.String()
}

func docxEdit(p string, a officeArgs) ([]byte, []string, error) {
	pkg, err := readPkg(p)
	if err != nil {
		return nil, nil, err
	}
	doc := pkg.get("word/document.xml")
	if doc == "" {
		return nil, nil, errors.New("not a Word document (no word/document.xml)")
	}
	var done []string
	// Replace in the body and in headers/footers.
	if len(a.Replace) > 0 {
		n := 0
		parts := []string{"word/document.xml"}
		for _, name := range pkg.names {
			if strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer") || name == "word/footnotes.xml" || name == "word/endnotes.xml" {
				parts = append(parts, name)
			}
		}
		for _, part := range parts {
			x := pkg.get(part)
			x = wParaRe.ReplaceAllStringFunc(x, func(para string) string {
				return replaceInRuns(para, wTRe, `<w:t xml:space="preserve">`, `</w:t>`, a.Replace, &n)
			})
			if part == "word/document.xml" {
				doc = x
			} else {
				pkg.set(part, []byte(x))
			}
		}
		done = append(done, fmt.Sprintf("%d replaced", n))
	}
	if len(a.DeleteParas) > 0 {
		n := 0
		doc = wParaRe.ReplaceAllStringFunc(doc, func(para string) string {
			t := paraText(para)
			for _, d := range a.DeleteParas {
				if d != "" && strings.Contains(t, d) {
					n++
					return ""
				}
			}
			return para
		})
		done = append(done, fmt.Sprintf("%d paragraphs deleted", n))
	}
	if strings.TrimSpace(a.Content) != "" {
		base := 9000
		c := newDocxCtx(pkg, p, strings.Contains(doc, `w:orient="landscape"`), base)
		if a.To != "" {
			c.base = a.To
		}
		body, err := c.body(parseMarkdown(a.Content))
		if err != nil {
			return nil, nil, err
		}
		docxEnsureStyles(pkg)
		docxEnsureNumbering(pkg, base, c.nums)
		if a.InsertAfter != "" {
			at := -1
			for _, m := range wParaRe.FindAllStringIndex(doc, -1) {
				if strings.Contains(paraText(doc[m[0]:m[1]]), a.InsertAfter) {
					at = m[1]
					break
				}
			}
			if at < 0 {
				return nil, nil, fmt.Errorf("no paragraph contains %q", a.InsertAfter)
			}
			doc = doc[:at] + body + doc[at:]
			done = append(done, "inserted after \""+clip(a.InsertAfter, 40)+"\"")
		} else {
			at := strings.LastIndex(doc, "<w:sectPr")
			if at < 0 {
				at = strings.LastIndex(doc, "</w:body>")
			}
			doc = doc[:at] + body + doc[at:]
			done = append(done, "appended")
		}
		doc = docxEnsureNS(doc)
	}
	pkg.set("word/document.xml", []byte(doc))
	b, err := pkg.bytes()
	return b, done, err
}

func docxEnsureNS(doc string) string {
	i := strings.Index(doc, "<w:document")
	if i < 0 {
		return doc
	}
	j := strings.Index(doc[i:], ">") + i
	tag := doc[i:j]
	for _, ns := range []string{nsR, nsWP, nsA, nsPic} {
		pfx := ns[:strings.Index(ns, "=")]
		if !strings.Contains(tag, pfx+"=") {
			tag += " " + ns
		}
	}
	return doc[:i] + tag + doc[j:]
}

var styleIDRe = regexp.MustCompile(`w:styleId="([^"]+)"`)

func docxEnsureStyles(pkg *ooxmlPkg) {
	st := pkg.get("word/styles.xml")
	if st == "" {
		return
	}
	have := map[string]bool{}
	for _, m := range styleIDRe.FindAllStringSubmatch(st, -1) {
		have[m[1]] = true
	}
	var add strings.Builder
	for _, def := range strings.SplitAfter(docxStyleDefs, "</w:style>") {
		if m := styleIDRe.FindStringSubmatch(def); m != nil && !have[m[1]] {
			add.WriteString(def)
		}
	}
	if add.Len() > 0 {
		pkg.set("word/styles.xml", []byte(strings.Replace(st, "</w:styles>", add.String()+"</w:styles>", 1)))
	}
}

func docxEnsureNumbering(pkg *ooxmlPkg, base int, nums []string) {
	num := fmt.Sprintf(`<w:num w:numId="%d"><w:abstractNumId w:val="%d"/></w:num>`, base, base) + strings.Join(nums, "")
	abs := docxAbstractNums(base)
	nx := pkg.get("word/numbering.xml")
	if nx == "" {
		pkg.set("word/numbering.xml", []byte(xmlHdr+`<w:numbering `+nsW+`>`+abs+num+`</w:numbering>`))
		pkg.ensureOverride("word/numbering.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml")
		pkg.addRel("word/_rels/document.xml.rels", "rIdTsNum", "http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering", "numbering.xml", false)
		return
	}
	if strings.Contains(nx, fmt.Sprintf(`w:abstractNumId="%d"`, base)) {
		// Already added by an earlier edit: only add the new list numbers.
		nx = strings.Replace(nx, "</w:numbering>", strings.Join(nums, "")+"</w:numbering>", 1)
	} else {
		if i := strings.Index(nx, "<w:num "); i >= 0 {
			nx = nx[:i] + abs + nx[i:]
		} else {
			nx = strings.Replace(nx, "</w:numbering>", abs+"</w:numbering>", 1)
		}
		nx = strings.Replace(nx, "</w:numbering>", num+"</w:numbering>", 1)
	}
	pkg.set("word/numbering.xml", []byte(nx))
}

// docxOutline shows the document as Markdown: headings, lists, tables, pictures.
func docxOutline(p string) (string, error) {
	pkg, err := readPkg(p)
	if err != nil {
		return "", err
	}
	doc := pkg.get("word/document.xml")
	if doc == "" {
		return "", errors.New("not a Word document")
	}
	d := xml.NewDecoder(strings.NewReader(doc))
	var out strings.Builder
	var para strings.Builder
	style, list, inT, tblDepth, pics := "", false, false, 0, 0
	var row []string
	var cell strings.Builder
	paras, tables := 0, 0
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				para.Reset()
				style, list = "", false
			case "pStyle":
				for _, at := range t.Attr {
					if at.Name.Local == "val" {
						style = at.Value
					}
				}
			case "numPr":
				list = true
			case "t":
				inT = true
			case "tab":
				para.WriteByte('\t')
			case "br", "cr":
				para.WriteByte(' ')
			case "tbl":
				tblDepth++
				if tblDepth == 1 {
					tables++
				}
			case "tr":
				if tblDepth == 1 {
					row = nil
				}
			case "tc":
				if tblDepth == 1 {
					cell.Reset()
				}
			case "drawing", "pict":
				pics++
				para.WriteString("[picture]")
			}
		case xml.CharData:
			if inT {
				para.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "p":
				txt := strings.TrimSpace(para.String())
				if tblDepth > 0 {
					if txt != "" {
						if cell.Len() > 0 {
							cell.WriteByte(' ')
						}
						cell.WriteString(txt)
					}
					continue
				}
				paras++
				if txt == "" {
					continue
				}
				ls := strings.ToLower(style)
				switch {
				case strings.HasPrefix(ls, "heading") && len(ls) == 8:
					out.WriteString(strings.Repeat("#", int(ls[7]-'0')) + " ")
				case ls == "title":
					out.WriteString("# ")
				case list || strings.HasPrefix(ls, "list"):
					out.WriteString("- ")
				}
				out.WriteString(txt + "\n\n")
			case "tc":
				if tblDepth == 1 {
					row = append(row, strings.ReplaceAll(cell.String(), "|", "/"))
				}
			case "tr":
				if tblDepth == 1 {
					out.WriteString("| " + strings.Join(row, " | ") + " |\n")
				}
			case "tbl":
				tblDepth--
				if tblDepth == 0 {
					out.WriteString("\n")
				}
			}
		}
	}
	head := fmt.Sprintf("%s — %d paragraphs, %d tables, %d pictures\n\n", filepath.Base(p), paras, tables, pics)
	return head + out.String(), nil
}
