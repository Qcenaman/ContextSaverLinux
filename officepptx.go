package main

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------- PowerPoint ----------

const (
	nsP     = `xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`
	pptW    = 12192000 // 13.333 in, 16:9
	pptH    = 6858000  // 7.5 in
	emuIn   = 914400
	relSlid = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"
	relLay  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout"
	ctSlide = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"
)

type pptSlide struct {
	title  []mdSpan
	blocks []mdBlock
}

// splitSlides: a slide per --- line, or per # / ## heading once a slide has a title.
func splitSlides(blocks []mdBlock) []pptSlide {
	var out []pptSlide
	cur := pptSlide{}
	started := false
	push := func() {
		if started && (cur.title != nil || len(cur.blocks) > 0) {
			out = append(out, cur)
		}
		cur, started = pptSlide{}, true
	}
	started = true
	for _, b := range blocks {
		switch {
		case b.kind == "break":
			push()
		case b.kind == "h" && cur.title == nil && len(cur.blocks) == 0:
			cur.title = b.spans
		case b.kind == "h" && b.level <= 2:
			push()
			cur.title = b.spans
		default:
			cur.blocks = append(cur.blocks, b)
		}
	}
	push()
	return out
}

type pptCtx struct {
	pkg   *ooxmlPkg
	base  string
	slide string // part name of the slide being built
	n     int
	shape int
}

func (c *pptCtx) runs(sp []mdSpan, sz int) string {
	var b strings.Builder
	for _, s := range sp {
		if s.text == "" {
			continue
		}
		attrs := `lang="en-US" dirty="0"`
		if sz > 0 {
			attrs += fmt.Sprintf(` sz="%d"`, sz)
		}
		if s.bold {
			attrs += ` b="1"`
		}
		if s.italic {
			attrs += ` i="1"`
		}
		inner := ""
		if s.code {
			inner = `<a:latin typeface="Consolas"/><a:cs typeface="Consolas"/>`
		}
		if s.link != "" {
			id := "rIdTs" + randHex(4)
			c.pkg.addRel(relsOf(c.slide), id, relLink, s.link, true)
			inner += `<a:hlinkClick r:id="` + id + `"/>`
		}
		if inner != "" {
			fmt.Fprintf(&b, `<a:r><a:rPr %s>%s</a:rPr><a:t>%s</a:t></a:r>`, attrs, inner, xmlEsc(s.text))
		} else {
			fmt.Fprintf(&b, `<a:r><a:rPr %s/><a:t>%s</a:t></a:r>`, attrs, xmlEsc(s.text))
		}
	}
	return b.String()
}

func relsOf(part string) string {
	return path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
}

func (c *pptCtx) id() int { c.shape++; return c.shape }

func (c *pptCtx) textShape(ph string, x, y, w, h int64, paras string) string {
	id := c.id()
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="Shape %d"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr>%s</p:nvPr></p:nvSpPr><p:spPr><a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm></p:spPr><p:txBody><a:bodyPr><a:normAutofit/></a:bodyPr><a:lstStyle/>%s</p:txBody></p:sp>`, id, id, ph, x, y, w, h, paras)
}

func (c *pptCtx) table(rows [][][]mdSpan, x, y, w int64) (string, int64) {
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return "", 0
	}
	sz := 1400
	if len(rows) > 8 {
		sz = 1100
	}
	rowH := int64(370840)
	if sz < 1400 {
		rowH = 300000
	}
	cw := w / int64(cols)
	id := c.id()
	var b strings.Builder
	fmt.Fprintf(&b, `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="%d" name="Table %d"/><p:cNvGraphicFramePr><a:graphicFrameLocks noGrp="1"/></p:cNvGraphicFramePr><p:nvPr/></p:nvGraphicFramePr><p:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></p:xfrm><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblPr firstRow="1" bandRow="1"><a:tableStyleId>{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}</a:tableStyleId></a:tblPr><a:tblGrid>`, id, id, x, y, cw*int64(cols), rowH*int64(len(rows)))
	for i := 0; i < cols; i++ {
		fmt.Fprintf(&b, `<a:gridCol w="%d"/>`, cw)
	}
	b.WriteString("</a:tblGrid>")
	for _, r := range rows {
		fmt.Fprintf(&b, `<a:tr h="%d">`, rowH)
		for i := 0; i < cols; i++ {
			var sp []mdSpan
			if i < len(r) {
				sp = r[i]
			}
			fmt.Fprintf(&b, `<a:tc><a:txBody><a:bodyPr/><a:lstStyle/><a:p>%s<a:endParaRPr lang="en-US" sz="%d" dirty="0"/></a:p></a:txBody><a:tcPr/></a:tc>`, c.runs(sp, sz), sz)
		}
		b.WriteString("</a:tr>")
	}
	b.WriteString("</a:tbl></a:graphicData></a:graphic></p:graphicFrame>")
	return b.String(), rowH * int64(len(rows))
}

func (c *pptCtx) picture(p string, x, y, w, h int64) (string, error) {
	ip, err := officeImagePath(p, c.base)
	if err != nil {
		return "", err
	}
	data, ext, cx, cy, err := loadImage(ip, w, h)
	if err != nil {
		return "", err
	}
	c.n++
	name := fmt.Sprintf("ts%s_%d.%s", randHex(3), c.n, ext)
	c.pkg.set("ppt/media/"+name, data)
	c.pkg.ensureDefault(ext, imageCT[ext])
	rid := "rIdTs" + randHex(4)
	c.pkg.addRel(relsOf(c.slide), rid, relImage, "../media/"+name, false)
	id := c.id()
	x += (w - cx) / 2
	y += (h - cy) / 2
	return fmt.Sprintf(`<p:pic><p:nvPicPr><p:cNvPr id="%d" name="Picture %d"/><p:cNvPicPr><a:picLocks noChangeAspect="1"/></p:cNvPicPr><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></p:blipFill><p:spPr><a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr></p:pic>`, id, id, rid, x, y, cx, cy), nil
}

// slideXML lays out one slide. titleLayout: first slide with only a title and a line or two.
func (c *pptCtx) slideXML(s pptSlide, titleLayout bool, W, H int64) (string, error) {
	c.shape = 1
	var shapes strings.Builder
	mx := W * 5 / 133 // ~0.5 in at 16:9
	if titleLayout {
		var sub strings.Builder
		for _, b := range s.blocks {
			sub.WriteString(`<a:p><a:pPr marL="0" indent="0" algn="ctr"><a:buNone/></a:pPr>` + c.runs(b.spans, 0) + "</a:p>")
		}
		shapes.WriteString(c.textShape(`<p:ph type="ctrTitle"/>`, mx*2, H*2/7, W-mx*4, H*2/7, `<a:p><a:pPr algn="ctr"/>`+c.runs(s.title, 0)+"</a:p>"))
		if sub.Len() > 0 {
			shapes.WriteString(c.textShape(`<p:ph type="subTitle" idx="1"/>`, mx*2, H*17/28, W-mx*4, H/5, sub.String()))
		}
		return c.wrap(shapes.String()), nil
	}
	top := H / 15
	if s.title != nil {
		shapes.WriteString(c.textShape(`<p:ph type="title"/>`, mx, top, W-2*mx, H*2/15, "<a:p>"+c.runs(s.title, 0)+"</a:p>"))
		top = H * 7 / 30
	}
	bodyH := H - top - H/15
	var text []mdBlock
	var tables, imgs []mdBlock
	for _, b := range s.blocks {
		switch b.kind {
		case "table":
			tables = append(tables, b)
		case "img":
			imgs = append(imgs, b)
		default:
			text = append(text, b)
		}
	}
	textW := W - 2*mx
	sideX := mx
	if len(text) > 0 && (len(tables) > 0 || len(imgs) > 0) {
		textW = (W - 2*mx) * 11 / 20
		sideX = mx + textW + mx/2
	}
	if len(text) > 0 {
		lines := 0
		for _, b := range text {
			if b.kind == "code" {
				lines += len(b.lines)
			} else {
				lines += 1 + len(spansText(b.spans))*int(W/textW)/70
			}
		}
		sz := 2400
		switch {
		case lines > 16:
			sz = 1200
		case lines > 12:
			sz = 1400
		case lines > 9:
			sz = 1600
		case lines > 6:
			sz = 2000
		}
		var paras strings.Builder
		for _, b := range text {
			switch b.kind {
			case "ul":
				fmt.Fprintf(&paras, `<a:p><a:pPr lvl="%d"/>%s</a:p>`, min(b.level, 4), c.runs(b.spans, sz-200*min(b.level, 2)))
			case "ol":
				fmt.Fprintf(&paras, `<a:p><a:pPr marL="%d" lvl="%d" indent="-342900"><a:buFont typeface="+mj-lt"/><a:buAutoNum type="arabicPeriod"/></a:pPr>%s</a:p>`, 342900+min(b.level, 4)*457200, min(b.level, 4), c.runs(b.spans, sz-200*min(b.level, 2)))
			case "h":
				sp := append([]mdSpan{}, b.spans...)
				for i := range sp {
					sp[i].bold = true
				}
				fmt.Fprintf(&paras, `<a:p><a:pPr marL="0" indent="0"><a:buNone/></a:pPr>%s</a:p>`, c.runs(sp, sz))
			case "code":
				for _, l := range b.lines {
					fmt.Fprintf(&paras, `<a:p><a:pPr marL="0" indent="0"><a:buNone/></a:pPr>%s</a:p>`, c.runs([]mdSpan{{text: l, code: true}}, sz-400))
				}
			default:
				fmt.Fprintf(&paras, `<a:p><a:pPr marL="0" indent="0"><a:buNone/></a:pPr>%s</a:p>`, c.runs(b.spans, sz))
			}
		}
		shapes.WriteString(c.textShape(`<p:ph idx="1"/>`, mx, top, textW, bodyH, paras.String()))
	}
	sideW := W - sideX - mx
	y := top
	for _, t := range tables {
		x, h := c.table(t.rows, sideX, y, sideW)
		shapes.WriteString(x)
		y += h + H/30
	}
	if len(imgs) > 0 {
		availH := top + bodyH - y
		if availH < H/5 {
			availH = H / 5
		}
		each := sideW / int64(len(imgs))
		if len(text) == 0 && len(tables) == 0 && len(imgs) > 1 {
			each = (W - 2*mx - int64(len(imgs)-1)*mx/2) / int64(len(imgs))
		}
		for i, im := range imgs {
			x, err := c.picture(im.path, sideX+int64(i)*(each+mx/2), y, each, availH)
			if err != nil {
				return "", err
			}
			shapes.WriteString(x)
		}
	}
	return c.wrap(shapes.String()), nil
}

func (c *pptCtx) wrap(shapes string) string {
	return xmlHdr + `<p:sld ` + nsA + ` ` + nsR + ` ` + nsP + `><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>` + shapes + `</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`
}

func isTitleOnly(s pptSlide) bool {
	if s.title == nil || len(s.blocks) > 2 {
		return false
	}
	for _, b := range s.blocks {
		if b.kind != "p" {
			return false
		}
	}
	return true
}

const pptTheme = `<a:theme ` + nsA + ` name="ContextSaver"><a:themeElements><a:clrScheme name="Office"><a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1><a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1><a:dk2><a:srgbClr val="1F3864"/></a:dk2><a:lt2><a:srgbClr val="E7E6E6"/></a:lt2><a:accent1><a:srgbClr val="2F5496"/></a:accent1><a:accent2><a:srgbClr val="ED7D31"/></a:accent2><a:accent3><a:srgbClr val="A5A5A5"/></a:accent3><a:accent4><a:srgbClr val="FFC000"/></a:accent4><a:accent5><a:srgbClr val="5B9BD5"/></a:accent5><a:accent6><a:srgbClr val="70AD47"/></a:accent6><a:hlink><a:srgbClr val="0563C1"/></a:hlink><a:folHlink><a:srgbClr val="954F72"/></a:folHlink></a:clrScheme><a:fontScheme name="Office"><a:majorFont><a:latin typeface="Calibri Light"/><a:ea typeface=""/><a:cs typeface=""/></a:majorFont><a:minorFont><a:latin typeface="Calibri"/><a:ea typeface=""/><a:cs typeface=""/></a:minorFont></a:fontScheme><a:fmtScheme name="Office"><a:fillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:fillStyleLst><a:lnStyleLst><a:ln w="6350"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln><a:ln w="12700"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln><a:ln w="19050"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln></a:lnStyleLst><a:effectStyleLst><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle></a:effectStyleLst><a:bgFillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:bgFillStyleLst></a:fmtScheme></a:themeElements></a:theme>`

func pptLvls(base int, bullets bool) string {
	var b strings.Builder
	for l := 1; l <= 5; l++ {
		sz := base - (l-1)*200
		if bullets {
			fmt.Fprintf(&b, `<a:lvl%dpPr marL="%d" indent="-228600"><a:spcBef><a:spcPts val="600"/></a:spcBef><a:buFont typeface="Arial"/><a:buChar char="•"/><a:defRPr sz="%d"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill><a:latin typeface="+mn-lt"/></a:defRPr></a:lvl%dpPr>`, l, 228600+(l-1)*457200, sz, l)
		} else {
			fmt.Fprintf(&b, `<a:lvl%dpPr marL="%d"><a:defRPr sz="%d"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill><a:latin typeface="+mn-lt"/></a:defRPr></a:lvl%dpPr>`, l, (l-1)*457200, sz, l)
		}
	}
	return b.String()
}

func pptxCreate(a officeArgs) ([]byte, error) {
	slides := splitSlides(parseMarkdown(a.Content))
	if len(slides) == 0 {
		return nil, errors.New("content is empty: give Markdown, a slide per --- or # heading")
	}
	pkg := &ooxmlPkg{files: map[string][]byte{}}
	tree := `<p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>%s</p:spTree></p:cSld>`
	ph := func(id int, name, phTag string, x, y, w, h int64) string {
		return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr>%s</p:nvPr></p:nvSpPr><p:spPr><a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm></p:spPr><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:endParaRPr lang="en-US"/></a:p></p:txBody></p:sp>`, id, name, phTag, x, y, w, h)
	}
	W, H := int64(pptW), int64(pptH)
	mx := W * 5 / 133
	masterShapes := ph(2, "Title", `<p:ph type="title"/>`, mx, H/15, W-2*mx, H*2/15) + ph(3, "Body", `<p:ph type="body" idx="1"/>`, mx, H*7/30, W-2*mx, H*7/10-H/15)
	pkg.set("ppt/slideMasters/slideMaster1.xml", []byte(xmlHdr+`<p:sldMaster `+nsA+` `+nsR+` `+nsP+`>`+fmt.Sprintf(tree, masterShapes)+`<p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/><p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/><p:sldLayoutId id="2147483650" r:id="rId2"/></p:sldLayoutIdLst><p:txStyles><p:titleStyle><a:lvl1pPr algn="l"><a:defRPr sz="3600" b="1"><a:solidFill><a:schemeClr val="tx2"/></a:solidFill><a:latin typeface="+mj-lt"/></a:defRPr></a:lvl1pPr></p:titleStyle><p:bodyStyle>`+pptLvls(2400, true)+`</p:bodyStyle><p:otherStyle>`+pptLvls(1800, false)+`</p:otherStyle></p:txStyles></p:sldMaster>`))
	pkg.set("ppt/slideMasters/_rels/slideMaster1.xml.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="`+relLay+`" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="`+relLay+`" Target="../slideLayouts/slideLayout2.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/></Relationships>`))
	layoutRels := xmlHdr + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/></Relationships>`
	titleShapes := ph(2, "Title", `<p:ph type="ctrTitle"/>`, mx*2, H*2/7, W-mx*4, H*2/7) + ph(3, "Subtitle", `<p:ph type="subTitle" idx="1"/>`, mx*2, H*17/28, W-mx*4, H/5)
	pkg.set("ppt/slideLayouts/slideLayout1.xml", []byte(xmlHdr+`<p:sldLayout `+nsA+` `+nsR+` `+nsP+` type="title" preserve="1"><p:cSld name="Title Slide"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>`+titleShapes+`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`))
	pkg.set("ppt/slideLayouts/slideLayout2.xml", []byte(xmlHdr+`<p:sldLayout `+nsA+` `+nsR+` `+nsP+` type="obj" preserve="1"><p:cSld name="Title and Content"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>`+ph(2, "Title", `<p:ph type="title"/>`, mx, H/15, W-2*mx, H*2/15)+ph(3, "Content", `<p:ph idx="1"/>`, mx, H*7/30, W-2*mx, H*7/10-H/15)+`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`))
	pkg.set("ppt/slideLayouts/_rels/slideLayout1.xml.rels", []byte(layoutRels))
	pkg.set("ppt/slideLayouts/_rels/slideLayout2.xml.rels", []byte(layoutRels))
	pkg.set("ppt/theme/theme1.xml", []byte(xmlHdr+pptTheme))
	pkg.set("ppt/presProps.xml", []byte(xmlHdr+`<p:presentationPr `+nsA+` `+nsR+` `+nsP+`/>`))
	pkg.set("ppt/viewProps.xml", []byte(xmlHdr+`<p:viewPr `+nsA+` `+nsR+` `+nsP+`><p:normalViewPr><p:restoredLeft sz="15620"/><p:restoredTop sz="94660"/></p:normalViewPr><p:gridSpacing cx="76200" cy="76200"/></p:viewPr>`))
	pkg.set("ppt/tableStyles.xml", []byte(xmlHdr+`<a:tblStyleLst `+nsA+` def="{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}"/>`))
	ct := `<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/><Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/><Override PartName="/ppt/slideLayouts/slideLayout2.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/><Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/><Override PartName="/ppt/presProps.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presProps+xml"/><Override PartName="/ppt/viewProps.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.viewProps+xml"/><Override PartName="/ppt/tableStyles.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.tableStyles+xml"/>`
	pkg.set("[Content_Types].xml", []byte(xmlHdr+`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>`+ct+`</Types>`))
	pkg.set("_rels/.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`))
	pkg.set("ppt/_rels/presentation.xml.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="theme/theme1.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/presProps" Target="presProps.xml"/><Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/viewProps" Target="viewProps.xml"/><Relationship Id="rId5" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/tableStyles" Target="tableStyles.xml"/></Relationships>`))
	pkg.set("ppt/presentation.xml", []byte(xmlHdr+`<p:presentation `+nsA+` `+nsR+` `+nsP+` saveSubsetFonts="1"><p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst><p:sldIdLst></p:sldIdLst><p:sldSz cx="12192000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/><p:defaultTextStyle>`+pptLvls(1800, false)+`</p:defaultTextStyle></p:presentation>`))
	layouts := map[string]string{"title": "../slideLayouts/slideLayout1.xml", "obj": "../slideLayouts/slideLayout2.xml"}
	if _, err := pptAddSlides(pkg, slides, a.Path, layouts, true, W, H); err != nil {
		return nil, err
	}
	return pkg.bytes()
}

// pptAddSlides appends slides; layouts maps "title"/"obj" to layout targets relative to ppt/slides.
func pptAddSlides(pkg *ooxmlPkg, slides []pptSlide, base string, layouts map[string]string, firstCanBeTitle bool, W, H int64) (int, error) {
	pres := pkg.get("ppt/presentation.xml")
	maxID := 255
	for _, m := range regexp.MustCompile(`<p:sldId\b[^>]*\bid="(\d+)"`).FindAllStringSubmatch(pres, -1) {
		n, _ := strconv.Atoi(m[1])
		maxID = max(maxID, n)
	}
	var ids strings.Builder
	c := &pptCtx{pkg: pkg, base: base}
	for i, s := range slides {
		n := 1
		for pkg.files[fmt.Sprintf("ppt/slides/slide%d.xml", n)] != nil {
			n++
		}
		part := fmt.Sprintf("ppt/slides/slide%d.xml", n)
		c.slide = part
		title := firstCanBeTitle && i == 0 && isTitleOnly(s)
		lay := layouts["obj"]
		if title && layouts["title"] != "" {
			lay = layouts["title"]
		}
		pkg.set(relsOf(part), []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="`+relLay+`" Target="`+lay+`"/></Relationships>`))
		x, err := c.slideXML(s, title, W, H)
		if err != nil {
			return 0, err
		}
		pkg.set(part, []byte(x))
		pkg.ensureOverride(part, ctSlide)
		rid := "rIdTs" + randHex(4)
		pkg.addRel("ppt/_rels/presentation.xml.rels", rid, relSlid, fmt.Sprintf("slides/slide%d.xml", n), false)
		maxID++
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="%s"/>`, maxID, rid)
	}
	switch {
	case strings.Contains(pres, "</p:sldIdLst>"):
		pres = strings.Replace(pres, "</p:sldIdLst>", ids.String()+"</p:sldIdLst>", 1)
	case strings.Contains(pres, "<p:sldIdLst/>"):
		pres = strings.Replace(pres, "<p:sldIdLst/>", "<p:sldIdLst>"+ids.String()+"</p:sldIdLst>", 1)
	default:
		pres = strings.Replace(pres, "</p:sldMasterIdLst>", "</p:sldMasterIdLst><p:sldIdLst>"+ids.String()+"</p:sldIdLst>", 1)
		if !strings.Contains(pres, "<p:sldIdLst>") {
			return 0, errors.New("couldn't find where slides go in presentation.xml")
		}
	}
	pkg.set("ppt/presentation.xml", []byte(pres))
	return len(slides), nil
}

// pptOrder: slide parts in show order.
func pptOrder(pkg *ooxmlPkg) []string {
	rels := relTargets(pkg, "ppt/_rels/presentation.xml.rels", "ppt")
	var out []string
	for _, m := range regexp.MustCompile(`<p:sldId\b[^>]*>`).FindAllString(pkg.get("ppt/presentation.xml"), -1) {
		out = append(out, rels[attrMap(m)["r:id"]])
	}
	return out
}

var (
	aParaRe = regexp.MustCompile(`(?s)<a:p>.*?</a:p>`)
	aTRe    = regexp.MustCompile(`(?s)<a:t>(.*?)</a:t>|<a:t/>`)
)

func pptxEdit(p string, a officeArgs) ([]byte, []string, error) {
	pkg, err := readPkg(p)
	if err != nil {
		return nil, nil, err
	}
	if pkg.get("ppt/presentation.xml") == "" {
		return nil, nil, errors.New("not a PowerPoint file")
	}
	var done []string
	order := pptOrder(pkg)
	if len(a.Replace) > 0 {
		n := 0
		for _, name := range pkg.names {
			if (strings.HasPrefix(name, "ppt/slides/slide") || strings.HasPrefix(name, "ppt/notesSlides/")) && strings.HasSuffix(name, ".xml") {
				x := aParaRe.ReplaceAllStringFunc(pkg.get(name), func(para string) string {
					return replaceInRuns(para, aTRe, `<a:t>`, `</a:t>`, a.Replace, &n)
				})
				pkg.set(name, []byte(x))
			}
		}
		done = append(done, fmt.Sprintf("%d replaced", n))
	}
	if a.DeleteSlides != "" {
		nums, err := parseRanges(a.DeleteSlides, len(order))
		if err != nil {
			return nil, nil, err
		}
		del := map[string]bool{}
		for _, n := range nums {
			del[order[n-1]] = true
		}
		if len(del) == len(order) {
			return nil, nil, errors.New("that would delete every slide")
		}
		rels := relTargets(pkg, "ppt/_rels/presentation.xml.rels", "ppt")
		pres := pkg.get("ppt/presentation.xml")
		prels := pkg.get("ppt/_rels/presentation.xml.rels")
		for id, target := range rels {
			if !del[target] {
				continue
			}
			pres = regexp.MustCompile(`<p:sldId\b[^>]*r:id="`+regexp.QuoteMeta(id)+`"[^>]*/>`).ReplaceAllString(pres, "")
			prels = regexp.MustCompile(`<Relationship\b[^>]*Id="`+regexp.QuoteMeta(id)+`"[^>]*/>`).ReplaceAllString(prels, "")
			pkg.del(target)
			pkg.del(relsOf(target))
			pkg.set("[Content_Types].xml", []byte(strings.Replace(pkg.get("[Content_Types].xml"), `<Override PartName="/`+target+`" ContentType="`+ctSlide+`"/>`, "", 1)))
		}
		pkg.set("ppt/presentation.xml", []byte(pres))
		pkg.set("ppt/_rels/presentation.xml.rels", []byte(prels))
		done = append(done, fmt.Sprintf("%d slides deleted", len(del)))
	}
	if strings.TrimSpace(a.Content) != "" {
		slides := splitSlides(parseMarkdown(a.Content))
		layouts := map[string]string{}
		for _, name := range pkg.names {
			if strings.HasPrefix(name, "ppt/slideLayouts/slideLayout") && strings.HasSuffix(name, ".xml") {
				m := regexp.MustCompile(`<p:sldLayout\b[^>]*\btype="(\w+)"`).FindStringSubmatch(pkg.get(name))
				if m != nil && layouts[m[1]] == "" {
					layouts[m[1]] = "../slideLayouts/" + path.Base(name)
				}
			}
		}
		if layouts["obj"] == "" {
			for _, k := range []string{"tx", "titleOnly", "title", "blank"} {
				if layouts[k] != "" {
					layouts["obj"] = layouts[k]
					break
				}
			}
		}
		if layouts["obj"] == "" {
			var names []string
			for _, name := range pkg.names {
				if strings.HasPrefix(name, "ppt/slideLayouts/slideLayout") && strings.HasSuffix(name, ".xml") {
					names = append(names, name)
				}
			}
			if len(names) == 0 {
				return nil, nil, errors.New("this file has no slide layouts to build on")
			}
			sort.Strings(names)
			layouts["obj"] = "../slideLayouts/" + path.Base(names[0])
		}
		W, H := int64(pptW), int64(pptH)
		if m := regexp.MustCompile(`<p:sldSz\b[^>]*cx="(\d+)"[^>]*cy="(\d+)"`).FindStringSubmatch(pkg.get("ppt/presentation.xml")); m != nil {
			W, _ = strconv.ParseInt(m[1], 10, 64)
			H, _ = strconv.ParseInt(m[2], 10, 64)
		}
		base := p
		if a.To != "" {
			base = a.To
		}
		n, err := pptAddSlides(pkg, slides, base, layouts, false, W, H)
		if err != nil {
			return nil, nil, err
		}
		done = append(done, fmt.Sprintf("%d slides added", n))
	}
	b, err := pkg.bytes()
	return b, done, err
}

// pptxOutline: each slide's title, text by level, tables and pictures, and notes.
func pptxOutline(p string) (string, error) {
	pkg, err := readPkg(p)
	if err != nil {
		return "", err
	}
	order := pptOrder(pkg)
	var out strings.Builder
	fmt.Fprintf(&out, "%s — %d slides\n", path.Base(strings.ReplaceAll(p, `\`, "/")), len(order))
	lvlRe := regexp.MustCompile(`<a:pPr\b[^>]*\blvl="(\d)"`)
	for i, part := range order {
		x := pkg.get(part)
		title := ""
		var body []string
		for _, sp := range regexp.MustCompile(`(?s)<p:sp>.*?</p:sp>|<p:graphicFrame>.*?</p:graphicFrame>|<p:pic>.*?</p:pic>`).FindAllString(x, -1) {
			switch {
			case strings.HasPrefix(sp, "<p:pic>"):
				body = append(body, "[picture]")
			case strings.HasPrefix(sp, "<p:graphicFrame>"):
				for _, tr := range regexp.MustCompile(`(?s)<a:tr\b.*?</a:tr>`).FindAllString(sp, -1) {
					var cells []string
					for _, tc := range regexp.MustCompile(`(?s)<a:tc\b.*?</a:tc>`).FindAllString(tr, -1) {
						cells = append(cells, strings.TrimSpace(paraTextA(tc)))
					}
					body = append(body, "| "+strings.Join(cells, " | ")+" |")
				}
				if !strings.Contains(sp, "<a:tbl>") {
					body = append(body, "[chart or object]")
				}
			default:
				isTitle := regexp.MustCompile(`<p:ph\b[^>]*type="(title|ctrTitle)"`).MatchString(sp)
				for _, para := range aParaRe.FindAllString(sp, -1) {
					t := strings.TrimSpace(paraTextA(para))
					if t == "" {
						continue
					}
					if isTitle {
						title = strings.TrimSpace(title + " " + t)
						continue
					}
					lv := 0
					if m := lvlRe.FindStringSubmatch(para); m != nil {
						lv, _ = strconv.Atoi(m[1])
					}
					pre := "- "
					if strings.Contains(para, "<a:buNone/>") {
						pre = ""
					}
					body = append(body, strings.Repeat("  ", lv)+pre+t)
				}
			}
		}
		fmt.Fprintf(&out, "\n## Slide %d: %s\n", i+1, title)
		for _, l := range body {
			out.WriteString(l + "\n")
		}
		for _, t := range relTargets(pkg, relsOf(part), path.Dir(part)) {
			if strings.Contains(t, "notesSlide") {
				var notes []string
				for _, para := range aParaRe.FindAllString(pkg.get(t), -1) {
					if s := strings.TrimSpace(paraTextA(para)); s != "" && !regexp.MustCompile(`^\d+$`).MatchString(s) {
						notes = append(notes, s)
					}
				}
				if len(notes) > 0 {
					out.WriteString("Notes: " + strings.Join(notes, " ") + "\n")
				}
			}
		}
	}
	return out.String(), nil
}

func paraTextA(x string) string {
	var b strings.Builder
	for _, m := range aTRe.FindAllStringSubmatch(x, -1) {
		b.WriteString(xmlUnesc(m[1]))
	}
	return b.String()
}
