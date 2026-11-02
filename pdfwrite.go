package main

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Writing PDFs: new ones from Markdown and pictures, and edits of existing ones
// (merge, pick and reorder pages, rotate, stamp text, split). Pages are copied
// object by object from the source, so text, fonts and pictures stay as they were.

type wRef int // a reference to an object in the file being written

type pdfW struct {
	objs [][]byte // index = object number - 1
}

func (w *pdfW) reserve() int { w.objs = append(w.objs, nil); return len(w.objs) }

func (w *pdfW) put(n int, v any) {
	var b bytes.Buffer
	if s, ok := v.(*pdfStream); ok {
		d := pdfDict{}
		for k, e := range s.d {
			d[k] = e
		}
		d["Length"] = len(s.raw)
		delete(d, "DL")
		pdfSer(&b, d)
		b.WriteString("\nstream\n")
		b.Write(s.raw)
		b.WriteString("\nendstream")
	} else {
		pdfSer(&b, v)
	}
	w.objs[n-1] = b.Bytes()
}

func (w *pdfW) add(v any) wRef {
	n := w.reserve()
	w.put(n, v)
	return wRef(n)
}

func pdfNum(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	s := strconv.FormatFloat(f, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" {
		s = "0"
	}
	return s
}

func pdfSer(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case float64:
		b.WriteString(pdfNum(x))
	case wRef:
		fmt.Fprintf(b, "%d 0 R", int(x))
	case pdfName:
		b.WriteByte('/')
		for i := 0; i < len(x); i++ {
			c := x[i]
			if c < 0x21 || c > 0x7e || pdfDelim(c) || c == '#' {
				fmt.Fprintf(b, "#%02X", c)
			} else {
				b.WriteByte(c)
			}
		}
	case []byte:
		b.WriteByte('<')
		const hx = "0123456789ABCDEF"
		for _, c := range x {
			b.WriteByte(hx[c>>4])
			b.WriteByte(hx[c&15])
		}
		b.WriteByte('>')
	case pdfLit: // already-escaped literal string
		b.WriteString("(" + string(x) + ")")
	case pdfArr:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(' ')
			}
			pdfSer(b, e)
		}
		b.WriteByte(']')
	case pdfDict:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("<<")
		for _, k := range keys {
			pdfSer(b, pdfName(k))
			b.WriteByte(' ')
			pdfSer(b, x[k])
		}
		b.WriteString(">>")
	case pdfKw:
		b.WriteString(string(x))
	case pdfRef: // should have been mapped; never point into nowhere
		b.WriteString("null")
	default:
		b.WriteString("null")
	}
}

type pdfLit string

func (w *pdfW) bytes(root wRef, info wRef) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offs := make([]int, len(w.objs))
	for i, o := range w.objs {
		offs[i] = b.Len()
		if o == nil {
			o = []byte("null")
		}
		fmt.Fprintf(&b, "%d 0 obj\n", i+1)
		b.Write(o)
		b.WriteString("\nendobj\n")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(w.objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	tr := pdfDict{"Size": len(w.objs) + 1, "Root": root, "ID": pdfArr{[]byte(randHex(8)), []byte(randHex(8))}}
	if info > 0 {
		tr["Info"] = info
	}
	b.WriteString("trailer\n")
	pdfSer(&b, tr)
	fmt.Fprintf(&b, "\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

// ---------- copying pages from existing files ----------

type srcPage struct {
	ref  int // object number in the source (0 if direct)
	dict pdfDict
	inh  pdfDict
}

func (d *pdfDoc) pageList() []srcPage {
	var out []srcPage
	seen := map[int]bool{}
	var walk func(v any, inh pdfDict, depth int)
	walk = func(v any, inh pdfDict, depth int) {
		if depth > 64 {
			return
		}
		ref := 0
		if r, ok := v.(pdfRef); ok {
			if seen[r.n] {
				return
			}
			seen[r.n] = true
			ref = r.n
		}
		n := d.dict(v)
		if n == nil {
			return
		}
		kids := d.arr(n["Kids"])
		if typ := d.name(n["Type"]); typ == "Pages" || typ != "Page" && kids != nil {
			ni := pdfDict{}
			for k, e := range inh {
				ni[k] = e
			}
			for _, k := range []string{"Resources", "MediaBox", "CropBox", "Rotate"} {
				if e, ok := n[k]; ok {
					ni[k] = e
				}
			}
			for _, k := range kids {
				walk(k, ni, depth+1)
			}
			return
		}
		out = append(out, srcPage{ref: ref, dict: n, inh: inh})
	}
	if root := d.dict(d.trailer["Root"]); root != nil {
		walk(root["Pages"], pdfDict{}, 0)
	}
	return out
}

type pdfCopier struct {
	w       *pdfW
	d       *pdfDoc
	memo    map[int]wRef
	allowed map[int]bool // page objects being copied; refs to other pages become null
}

func (c *pdfCopier) val(v any) any {
	switch x := v.(type) {
	case pdfRef:
		if r, ok := c.memo[x.n]; ok {
			return r
		}
		o := c.d.load(x.n)
		if dd, ok := o.(pdfDict); ok {
			if t := c.d.name(dd["Type"]); t == "Pages" || (t == "Page" && !c.allowed[x.n]) {
				return nil
			}
		}
		if o == nil {
			return nil
		}
		n := c.w.reserve()
		c.memo[x.n] = wRef(n)
		c.w.put(n, c.val(o))
		return wRef(n)
	case pdfArr:
		out := make(pdfArr, len(x))
		for i, e := range x {
			out[i] = c.val(e)
		}
		return out
	case pdfDict:
		out := pdfDict{}
		for k, e := range x {
			out[k] = c.val(e)
		}
		return out
	case *pdfStream:
		d := pdfDict{}
		for k, e := range x.d {
			d[k] = c.val(e)
		}
		return &pdfStream{d: d, raw: x.raw}
	}
	return v
}

type pageEdit struct {
	rotate    int
	stamp     string
	stampAt   string
	stampSize float64
}

// box returns the visible box of a page in user space.
func (d *pdfDoc) box(pg srcPage) [4]float64 {
	get := func(k string) (pdfArr, bool) {
		if v, ok := pg.dict[k]; ok {
			a := d.arr(v)
			return a, len(a) == 4
		}
		if v, ok := pg.inh[k]; ok {
			a := d.arr(v)
			return a, len(a) == 4
		}
		return nil, false
	}
	a, ok := get("CropBox")
	if !ok {
		a, ok = get("MediaBox")
	}
	if !ok {
		return [4]float64{0, 0, 612, 792}
	}
	var r [4]float64
	for i := range r {
		r[i], _ = d.num(a[i])
	}
	if r[0] > r[2] {
		r[0], r[2] = r[2], r[0]
	}
	if r[1] > r[3] {
		r[1], r[3] = r[3], r[1]
	}
	return r
}

func (d *pdfDoc) rotation(pg srcPage) int {
	v, ok := pg.dict["Rotate"]
	if !ok {
		v = pg.inh["Rotate"]
	}
	r := d.int(v)
	return ((r % 360) + 360) % 360
}

type pdfJob struct {
	w      *pdfW
	kids   pdfArr
	pages  wRef
	fonts  map[string]wRef
	first  *pdfDoc
	firstC *pdfCopier
}

func newPDFJob() *pdfJob {
	j := &pdfJob{w: &pdfW{}, fonts: map[string]wRef{}}
	j.pages = wRef(j.w.reserve())
	return j
}

func (j *pdfJob) font(base string) wRef {
	if r, ok := j.fonts[base]; ok {
		return r
	}
	d := pdfDict{"Type": pdfName("Font"), "Subtype": pdfName("Type1"), "BaseFont": pdfName(base)}
	if base != "Symbol" && base != "ZapfDingbats" {
		d["Encoding"] = pdfName("WinAnsiEncoding")
	}
	r := j.w.add(d)
	j.fonts[base] = r
	return r
}

// addDoc copies the chosen pages (1-based, any order, repeats allowed) of d.
func (j *pdfJob) addDoc(d *pdfDoc, nums []int, edits map[int]pageEdit, total *int) error {
	all := d.pageList()
	c := &pdfCopier{w: j.w, d: d, memo: map[int]wRef{}, allowed: map[int]bool{}}
	for _, n := range nums {
		if n < 1 || n > len(all) {
			return fmt.Errorf("page %d doesn't exist (the PDF has %d)", n, len(all))
		}
		if all[n-1].ref > 0 {
			c.allowed[all[n-1].ref] = true
		}
	}
	if j.first == nil {
		j.first, j.firstC = d, c
	}
	used := map[int]bool{}
	for _, n := range nums {
		pg := all[n-1]
		var num int
		if pg.ref > 0 && !used[pg.ref] {
			if r, ok := c.memo[pg.ref]; ok { // already referenced (e.g. by an annotation)
				num = int(r)
			} else {
				num = j.w.reserve()
				c.memo[pg.ref] = wRef(num)
			}
			used[pg.ref] = true
		} else {
			num = j.w.reserve()
		}
		nd := pdfDict{}
		for k, e := range pg.inh {
			nd[k] = e
		}
		for k, e := range pg.dict {
			nd[k] = e
		}
		delete(nd, "Parent")
		delete(nd, "B") // article beads point at other pages
		delete(nd, "PieceInfo")
		out := c.val(nd).(pdfDict)
		out["Type"] = pdfName("Page")
		out["Parent"] = j.pages
		if _, ok := out["MediaBox"]; !ok {
			out["MediaBox"] = pdfArr{0, 0, 612, 792}
		}
		*total++
		if e, ok := edits[n]; ok {
			j.applyEdit(d, pg, out, e, *total)
		}
		j.w.put(num, out)
		j.kids = append(j.kids, wRef(num))
	}
	return nil
}

func (j *pdfJob) applyEdit(d *pdfDoc, pg srcPage, out pdfDict, e pageEdit, n int) {
	rot := d.rotation(pg)
	if e.rotate != 0 {
		rot = (((rot + e.rotate) % 360) + 360) % 360
		out["Rotate"] = rot
	}
	if e.stamp == "" {
		return
	}
	bx := d.box(pg)
	// Resources for this page only, with our fonts added.
	res := pdfDict{}
	if r, ok := out["Resources"].(wRef); ok {
		res = j.resolveW(r)
	} else if r, ok := out["Resources"].(pdfDict); ok {
		for k, v := range r {
			res[k] = v
		}
	}
	fonts := pdfDict{}
	if f, ok := res["Font"].(wRef); ok {
		fonts = j.resolveW(f)
	} else if f, ok := res["Font"].(pdfDict); ok {
		for k, v := range f {
			fonts[k] = v
		}
	}
	fonts["TsF1"] = j.font("Helvetica")
	fonts["TsF2"] = j.font("Helvetica-Bold")
	res["Font"] = fonts
	out["Resources"] = res

	var contents pdfArr
	switch c := out["Contents"].(type) {
	case pdfArr:
		contents = c
	case nil:
	default:
		contents = pdfArr{c}
	}
	pre := j.w.add(&pdfStream{d: pdfDict{}, raw: []byte("q\n")})
	post := j.w.add(&pdfStream{d: pdfDict{}, raw: []byte("\nQ\n" + stampOps(e, bx, rot, n))})
	out["Contents"] = append(append(pdfArr{pre}, contents...), post)
}

// resolveW re-reads a dictionary we already wrote (Resources shared by pages).
func (j *pdfJob) resolveW(r wRef) pdfDict {
	b := j.w.objs[int(r)-1]
	l := &pdfLex{b: b}
	v, _ := l.token()
	d, _ := v.(pdfDict)
	if d == nil {
		return pdfDict{}
	}
	return remapRefs(d).(pdfDict)
}

// remapRefs turns references parsed back from our own output into wRefs.
func remapRefs(v any) any {
	switch x := v.(type) {
	case pdfRef:
		return wRef(x.n)
	case pdfArr:
		for i := range x {
			x[i] = remapRefs(x[i])
		}
	case pdfDict:
		for k := range x {
			x[k] = remapRefs(x[k])
		}
	}
	return v
}

func stampOps(e pageEdit, bx [4]float64, rot int, n int) string {
	w, h := bx[2]-bx[0], bx[3]-bx[1]
	var m [6]float64
	vw, vh := w, h
	switch rot {
	case 90:
		m = [6]float64{0, 1, -1, 0, bx[2], bx[1]}
		vw, vh = h, w
	case 180:
		m = [6]float64{-1, 0, 0, -1, bx[2], bx[3]}
	case 270:
		m = [6]float64{0, -1, 1, 0, bx[0], bx[3]}
		vw, vh = h, w
	default:
		m = [6]float64{1, 0, 0, 1, bx[0], bx[1]}
	}
	text := strings.ReplaceAll(e.stamp, "{n}", strconv.Itoa(n))
	at := strings.ToLower(e.stampAt)
	if at == "" {
		at = "bottom"
	}
	size := e.stampSize
	var b strings.Builder
	fmt.Fprintf(&b, "q %s %s %s %s %s %s cm\n", pdfNum(m[0]), pdfNum(m[1]), pdfNum(m[2]), pdfNum(m[3]), pdfNum(m[4]), pdfNum(m[5]))
	if at == "center" || at == "watermark" {
		if size <= 0 {
			size = math.Min(72, 1.6*math.Hypot(vw, vh)/math.Max(1, textWidth(text, helvBW, 1)*1.15))
		}
		tw := textWidth(text, helvBW, size)
		ang := math.Atan2(vh, vw)
		cs, sn := math.Cos(ang), math.Sin(ang)
		fmt.Fprintf(&b, "0.82 0.82 0.82 rg BT /TsF2 %s Tf %s %s %s %s %s %s Tm %s Tj ET Q\n", pdfNum(size), pdfNum(cs), pdfNum(sn), pdfNum(-sn), pdfNum(cs),
			pdfNum(vw/2-cs*tw/2+sn*size*0.35), pdfNum(vh/2-sn*tw/2-cs*size*0.35), pdfLitStr(text))
		return b.String()
	}
	if size <= 0 {
		size = 10
	}
	tw := textWidth(text, helvW, size)
	margin := 28.0
	x := (vw - tw) / 2
	if strings.HasSuffix(at, "left") {
		x = margin
	} else if strings.HasSuffix(at, "right") {
		x = vw - margin - tw
	}
	y := margin - size*0.25
	if strings.HasPrefix(at, "top") {
		y = vh - margin - size*0.75
	}
	fmt.Fprintf(&b, "0 g BT /TsF1 %s Tf %s %s Td %s Tj ET Q\n", pdfNum(size), pdfNum(x), pdfNum(y), pdfLitStr(text))
	return b.String()
}

func (j *pdfJob) finish(info pdfDict) []byte {
	j.w.put(int(j.pages), pdfDict{"Type": pdfName("Pages"), "Kids": j.kids, "Count": len(j.kids)})
	cat := pdfDict{"Type": pdfName("Catalog"), "Pages": j.pages}
	if j.first != nil {
		if root := j.first.dict(j.first.trailer["Root"]); root != nil {
			for _, k := range []string{"AcroForm", "Outlines", "Names", "Dests", "PageMode", "PageLayout", "ViewerPreferences", "Lang", "MarkInfo", "Metadata", "OCProperties"} {
				if v, ok := root[k]; ok {
					cat[k] = j.firstC.val(v)
				}
			}
		}
		if info == nil {
			if src := j.first.dict(j.first.trailer["Info"]); src != nil {
				info = pdfDict{}
				for k, v := range src {
					if _, ok := v.([]byte); ok {
						info[k] = v
					}
				}
			}
		}
	}
	if info == nil {
		info = pdfDict{}
	}
	info["Producer"] = []byte("ContextSaver")
	info["ModDate"] = []byte(time.Now().Format("D:20060102150405"))
	root := j.w.add(cat)
	return j.w.bytes(root, j.w.add(info))
}

// pdfSource: "file.pdf", "file.pdf:2-5", or a picture.
var pdfRangeSuffix = regexp.MustCompile(`(?i)^(.+\.pdf):([\d,\-\s]+)$`)

func (j *pdfJob) addSource(src, base string, total *int) error {
	rng := ""
	if m := pdfRangeSuffix.FindStringSubmatch(src); m != nil {
		src, rng = m[1], m[2]
	}
	p, err := officeImagePath(src, base)
	if err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(p))
	if isImageExt(ext) {
		return j.addImagePage(p, total)
	}
	if ext != ".pdf" {
		return fmt.Errorf("%s: only PDFs and pictures can be added (convert Office files first)", filepath.Base(p))
	}
	d, err := openPDF(p)
	if err != nil {
		return fmt.Errorf("%s: %v", filepath.Base(p), err)
	}
	nums, err := parseRanges(rng, len(d.pageList()))
	if err != nil {
		return fmt.Errorf("%s: %v", filepath.Base(p), err)
	}
	return j.addDoc(d, nums, nil, total)
}

func (j *pdfJob) addImagePage(p string, total *int) error {
	xo, iw, ih, err := j.imageXObject(p)
	if err != nil {
		return err
	}
	pw, ph := 612.0, 792.0
	if iw > ih {
		pw, ph = ph, pw
	}
	m := 36.0
	s := math.Min((pw-2*m)/iw, (ph-2*m)/ih)
	s = math.Min(s, 1.5)
	dw, dh := iw*s, ih*s
	ops := fmt.Sprintf("q %s 0 0 %s %s %s cm /Im1 Do Q", pdfNum(dw), pdfNum(dh), pdfNum((pw-dw)/2), pdfNum((ph-dh)/2))
	cs := j.w.add(&pdfStream{d: pdfDict{}, raw: []byte(ops)})
	num := j.w.reserve()
	j.w.put(num, pdfDict{"Type": pdfName("Page"), "Parent": j.pages, "MediaBox": pdfArr{0, 0, pw, ph}, "Contents": cs,
		"Resources": pdfDict{"XObject": pdfDict{"Im1": xo}}})
	j.kids = append(j.kids, wRef(num))
	*total++
	return nil
}

// imageXObject embeds a picture: JPEG as is, others as compressed RGB (+ alpha mask).
func (j *pdfJob) imageXObject(p string) (wRef, float64, float64, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("image %s: %v", p, cleanErr(err))
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("image %s: only PNG, JPEG and GIF can be placed", filepath.Base(p))
	}
	if format == "jpeg" {
		csn := "DeviceRGB"
		switch cfg.ColorModel {
		case color.GrayModel:
			csn = "DeviceGray"
		case color.CMYKModel:
			csn = "DeviceCMYK"
		}
		d := pdfDict{"Type": pdfName("XObject"), "Subtype": pdfName("Image"), "Width": cfg.Width, "Height": cfg.Height,
			"ColorSpace": pdfName(csn), "BitsPerComponent": 8, "Filter": pdfName("DCTDecode")}
		if csn == "DeviceCMYK" {
			d["Decode"] = pdfArr{1, 0, 1, 0, 1, 0, 1, 0}
		}
		return j.w.add(&pdfStream{d: d, raw: data}), float64(cfg.Width), float64(cfg.Height), nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("image %s: %v", filepath.Base(p), err)
	}
	r := img.Bounds()
	rgb := make([]byte, 0, r.Dx()*r.Dy()*3)
	alpha := make([]byte, 0, r.Dx()*r.Dy())
	hasAlpha := false
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			rgb = append(rgb, c.R, c.G, c.B)
			alpha = append(alpha, c.A)
			if c.A != 255 {
				hasAlpha = true
			}
		}
	}
	d := pdfDict{"Type": pdfName("XObject"), "Subtype": pdfName("Image"), "Width": r.Dx(), "Height": r.Dy(),
		"ColorSpace": pdfName("DeviceRGB"), "BitsPerComponent": 8, "Filter": pdfName("FlateDecode")}
	if hasAlpha {
		d["SMask"] = j.w.add(&pdfStream{d: pdfDict{"Type": pdfName("XObject"), "Subtype": pdfName("Image"), "Width": r.Dx(), "Height": r.Dy(),
			"ColorSpace": pdfName("DeviceGray"), "BitsPerComponent": 8, "Filter": pdfName("FlateDecode")}, raw: zlibBytes(alpha)})
	}
	return j.w.add(&pdfStream{d: d, raw: zlibBytes(rgb)}), float64(r.Dx()), float64(r.Dy()), nil
}

func zlibBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// ---------- entry points ----------

func pdfCreate(a officeArgs) ([]byte, error) {
	if strings.TrimSpace(a.Content) == "" && len(a.From) == 0 {
		return nil, errors.New("give content (Markdown) and/or from (PDFs to merge, \"file.pdf:2-5\" for some pages, or pictures)")
	}
	j := newPDFJob()
	total := 0
	if strings.TrimSpace(a.Content) != "" {
		if err := j.layoutMarkdown(parseMarkdown(a.Content), a.Path, a.Landscape, &total); err != nil {
			return nil, err
		}
	}
	for _, src := range a.From {
		if err := j.addSource(src, a.Path, &total); err != nil {
			return nil, err
		}
	}
	return j.finishEdited(a, total, true)
}

// finishEdited applies rotate/stamp to a finished set of pages built from scratch.
func (j *pdfJob) finishEdited(a officeArgs, total int, fresh bool) ([]byte, error) {
	if fresh && (a.Rotate != 0 || a.Stamp != "") {
		// Rebuild through the editor so rotation and stamps go through one path.
		b := j.finish(nil)
		d, err := parsePDF(b)
		if err != nil {
			return nil, err
		}
		j2 := newPDFJob()
		nums, _ := parseRanges("", total)
		edits, err := pageEdits(a, total)
		if err != nil {
			return nil, err
		}
		t := 0
		if err := j2.addDoc(d, nums, edits, &t); err != nil {
			return nil, err
		}
		return j2.finish(nil), nil
	}
	return j.finish(nil), nil
}

func pageEdits(a officeArgs, total int) (map[int]pageEdit, error) {
	edits := map[int]pageEdit{}
	if a.Rotate == 0 && a.Stamp == "" {
		return edits, nil
	}
	if a.Rotate%90 != 0 {
		return nil, errors.New("rotate must be 90, 180, 270 (or negative)")
	}
	nums, err := parseRanges(a.ApplyTo, total)
	if err != nil {
		return nil, err
	}
	for _, n := range nums {
		edits[n] = pageEdit{rotate: a.Rotate, stamp: strings.ReplaceAll(a.Stamp, "{total}", strconv.Itoa(total)), stampAt: a.StampAt, stampSize: a.StampSize}
	}
	return edits, nil
}

func pdfEdit(p string, a officeArgs) ([]byte, []string, error) {
	d, err := openPDF(p)
	if err != nil {
		return nil, nil, err
	}
	n := len(d.pageList())
	if n == 0 {
		return nil, nil, errors.New("no pages found in this PDF")
	}
	nums, err := parseRanges(a.Pages, n)
	if err != nil {
		return nil, nil, err
	}
	var done []string
	if a.Pages != "" {
		done = append(done, fmt.Sprintf("kept pages %s (%d)", a.Pages, len(nums)))
	}
	j := newPDFJob()
	total := 0
	if err := j.addDoc(d, nums, nil, &total); err != nil {
		return nil, nil, err
	}
	if a.Rotate != 0 {
		done = append(done, fmt.Sprintf("rotated %d°", a.Rotate))
	}
	if a.Stamp != "" {
		done = append(done, "stamped \""+clip(a.Stamp, 30)+"\"")
	}
	for _, src := range a.From {
		if err := j.addSource(src, p, &total); err != nil {
			return nil, nil, err
		}
	}
	if len(a.From) > 0 {
		done = append(done, fmt.Sprintf("added %d file(s)", len(a.From)))
	}
	if strings.TrimSpace(a.Content) != "" {
		before := total
		if err := j.layoutMarkdown(parseMarkdown(a.Content), p, a.Landscape, &total); err != nil {
			return nil, nil, err
		}
		done = append(done, fmt.Sprintf("added %d new page(s)", total-before))
	}
	if a.Pages == "" && a.Rotate == 0 && a.Stamp == "" && len(a.From) == 0 && strings.TrimSpace(a.Content) == "" {
		return nil, nil, nil
	}
	done = append(done, fmt.Sprintf("%d pages", total))
	b, err := j.finishEdited(a, total, true)
	return b, done, err
}

func pdfSplit(p string, a officeArgs) (string, error) {
	d, err := openPDF(p)
	if err != nil {
		return "", err
	}
	n := len(d.pageList())
	var groups [][]int
	if strings.TrimSpace(a.Pages) == "" {
		for i := 1; i <= n; i++ {
			groups = append(groups, []int{i})
		}
	} else {
		for _, g := range strings.Split(a.Pages, ",") {
			r, err := parseRanges(g, n)
			if err != nil {
				return "", err
			}
			if len(r) > 0 {
				groups = append(groups, r)
			}
		}
	}
	dir := filepath.Dir(p)
	if a.To != "" {
		if dir, err = expandPath(a.To); err != nil {
			return "", err
		}
	}
	if err := checkAllowed(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	var made []string
	for _, g := range groups {
		name := fmt.Sprintf("%s_p%d.pdf", stem, g[0])
		if len(g) > 1 {
			name = fmt.Sprintf("%s_p%d-%d.pdf", stem, g[0], g[len(g)-1])
		}
		out := filepath.Join(dir, name)
		if exists(out) && !a.Overwrite {
			return "", fmt.Errorf("%s already exists; overwrite:true to replace", out)
		}
		d2, _ := openPDF(p) // fresh copy: copying caches objects per document
		j := newPDFJob()
		t := 0
		if err := j.addDoc(d2, g, nil, &t); err != nil {
			return "", err
		}
		if _, err := officeSave(out, j.finish(nil)); err != nil {
			return "", err
		}
		made = append(made, name)
	}
	return fmt.Sprintf("split into %d files in %s: %s", len(made), dir, clip(strings.Join(made, ", "), 400)), nil
}

func pdfOutline(p, pages string) (string, error) {
	d, err := openPDF(p)
	if err != nil {
		return "", err
	}
	pl := d.pageList()
	sizes := map[string]int{}
	var order []string
	for _, pg := range pl {
		b := d.box(pg)
		w, h := b[2]-b[0], b[3]-b[1]
		if r := d.rotation(pg); r == 90 || r == 270 {
			w, h = h, w
		}
		k := fmt.Sprintf("%.1fx%.1f in", w/72, h/72)
		if sizes[k] == 0 {
			order = append(order, k)
		}
		sizes[k]++
	}
	var sz []string
	for _, k := range order {
		sz = append(sz, fmt.Sprintf("%s ×%d", k, sizes[k]))
	}
	head := fmt.Sprintf("%s — %d pages (%s)", filepath.Base(p), len(pl), strings.Join(sz, ", "))
	if info := d.dict(d.trailer["Info"]); info != nil {
		if t := strings.TrimSpace(pdfTextString(d.str(info["Title"]))); t != "" {
			head += "; title: " + t
		}
	}
	if root := d.dict(d.trailer["Root"]); root != nil {
		if af := d.dict(root["AcroForm"]); af != nil && len(d.arr(af["Fields"])) > 0 {
			head += fmt.Sprintf("; form with %d fields", len(d.arr(af["Fields"])))
		}
	}
	o := textOpts{ocrWait: 40 * time.Second, ocrUpTo: 5}
	if pages != "" {
		set, err := parsePages(pages)
		if err != nil {
			return "", err
		}
		o.pages, o.ocrUpTo = set, 0
	}
	t, err := pdfFileText(p, 30<<20, o)
	if err != nil {
		return head, nil
	}
	return head + "\n\n" + t, nil
}

// ---------- text helpers for the standard fonts ----------

var winAnsiRev = func() map[rune]byte {
	m := map[rune]byte{}
	for i, r := range pdfWinAnsiHigh {
		if r != 0x2022 || i == 0x15 {
			m[r] = byte(0x80 + i)
		}
	}
	return m
}()

var winAnsiNames = pdfBaseEncoding("WinAnsiEncoding")

func toWinAnsi(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			out = append(out, ' ', ' ', ' ', ' ')
		case r < 0x20:
		case r < 0x80 || (r >= 0xA0 && r <= 0xFF):
			out = append(out, byte(r))
		default:
			if b, ok := winAnsiRev[r]; ok {
				out = append(out, b)
			} else if r == 0x2212 {
				out = append(out, '-')
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

func textWidth(s string, widths map[string]int, size float64) float64 {
	if widths == nil { // Courier
		return float64(len(toWinAnsi(s))) * 0.6 * size
	}
	t := 0
	for _, c := range toWinAnsi(s) {
		name := winAnsiNames[c]
		if c == ' ' || c == 0xA0 {
			name = "space"
		}
		w, ok := widths[name]
		if !ok {
			w = 556
		}
		t += w
	}
	return float64(t) * size / 1000
}

func pdfLitStr(s string) string {
	var b strings.Builder
	b.WriteByte('(')
	for _, c := range toWinAnsi(s) {
		switch {
		case c == '(' || c == ')' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x80:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(')')
	return b.String()
}
