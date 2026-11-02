package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf16"
)

// ---------- fonts ----------

type pdfRange struct {
	n      int
	lo, hi uint32
}

type pdfCIDRange struct {
	lo, hi uint32
	cid    int
}

type pdfFont struct {
	composite bool
	cs        []pdfRange
	uni       map[uint32]string
	simple    [256]string
	widths    map[int]float64
	dw        float64
	scale     float64
	cidChar   map[uint32]int
	cidRanges []pdfCIDRange
	uniCodes  bool
	vertical  bool
	approx    bool
	gnames    []string
	gload     func() []string
	cidToGID  []byte
}

type pdfCMap struct {
	cs        []pdfRange
	uni       map[uint32]string
	cidChar   map[uint32]int
	cidRanges []pdfCIDRange
	vertical  bool
}

// Code spaces of the common predefined CJK CMaps, for fonts that pair one with a ToUnicode map.
var pdfCJKSpaces = []struct {
	key string
	cs  []pdfRange
}{
	{"RKSJ", []pdfRange{{1, 0, 0x80}, {1, 0xA0, 0xDF}, {2, 0x8140, 0x9FFC}, {2, 0xE040, 0xFCFC}}},
	{"EUC", []pdfRange{{1, 0, 0x80}, {2, 0x8EA0, 0x8EDF}, {2, 0xA1A1, 0xFEFE}}},
	{"GBK", []pdfRange{{1, 0, 0x80}, {2, 0x8140, 0xFEFE}}},
	{"GB-", []pdfRange{{1, 0, 0x80}, {2, 0xA1A1, 0xFEFE}}},
	{"UHC", []pdfRange{{1, 0, 0x80}, {2, 0x8141, 0xFEFE}}},
	{"B5", []pdfRange{{1, 0, 0x80}, {2, 0xA140, 0xFEFE}}},
}

func utf16BE(b []byte) string {
	if len(b) == 1 {
		return string(rune(b[0]))
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

func parseCMap(b []byte) *pdfCMap {
	cm := &pdfCMap{uni: map[uint32]string{}}
	l := &pdfLex{b: b}
	var prev []any
	next := func() (any, bool) {
		t, err := l.token()
		if err != nil {
			return nil, false
		}
		if k, ok := t.(pdfKw); ok && strings.HasPrefix(string(k), "end") {
			return nil, false
		}
		return t, true
	}
	for {
		t, err := l.token()
		if err != nil {
			break
		}
		kw, isKw := t.(pdfKw)
		if !isKw {
			prev = append(prev, t)
			if len(prev) > 8 {
				prev = prev[len(prev)-4:]
			}
			continue
		}
		switch kw {
		case "begincodespacerange":
			for {
				a, ok1 := next()
				z, ok2 := next()
				if !ok1 || !ok2 {
					break
				}
				lo, _ := a.([]byte)
				hi, _ := z.([]byte)
				if len(lo) > 0 && len(lo) <= 4 {
					cm.cs = append(cm.cs, pdfRange{len(lo), uint32(beInt(lo)), uint32(beInt(hi))})
				}
			}
		case "beginbfchar":
			for {
				a, ok1 := next()
				v, ok2 := next()
				if !ok1 || !ok2 {
					break
				}
				src, _ := a.([]byte)
				if len(src) == 0 || len(src) > 4 {
					continue
				}
				switch x := v.(type) {
				case []byte:
					cm.uni[uint32(beInt(src))] = utf16BE(x)
				case pdfName:
					cm.uni[uint32(beInt(src))] = pdfGlyphText(string(x))
				}
			}
		case "beginbfrange":
			for {
				a, ok1 := next()
				z, ok2 := next()
				v, ok3 := next()
				if !ok1 || !ok2 || !ok3 {
					break
				}
				lo, _ := a.([]byte)
				hi, _ := z.([]byte)
				if len(lo) == 0 || len(lo) > 4 {
					continue
				}
				from, to := uint32(beInt(lo)), uint32(beInt(hi))
				if to < from || to-from > 65535 {
					continue
				}
				switch x := v.(type) {
				case []byte:
					if len(x) == 0 {
						continue
					}
					base := append([]byte(nil), x...)
					if len(base) == 1 {
						base = []byte{0, base[0]}
					}
					last := uint32(beInt(base[len(base)-2:]))
					for c := from; c <= to; c++ {
						v := last + (c - from)
						base[len(base)-2], base[len(base)-1] = byte(v>>8), byte(v)
						cm.uni[c] = utf16BE(base)
					}
				case pdfArr:
					for i, e := range x {
						if s, ok := e.([]byte); ok && from+uint32(i) <= to {
							cm.uni[from+uint32(i)] = utf16BE(s)
						}
					}
				}
			}
		case "begincidchar":
			if cm.cidChar == nil {
				cm.cidChar = map[uint32]int{}
			}
			for {
				a, ok1 := next()
				v, ok2 := next()
				if !ok1 || !ok2 {
					break
				}
				if src, ok := a.([]byte); ok {
					if cid, ok := v.(int); ok {
						cm.cidChar[uint32(beInt(src))] = cid
					}
				}
			}
		case "begincidrange":
			for {
				a, ok1 := next()
				z, ok2 := next()
				v, ok3 := next()
				if !ok1 || !ok2 || !ok3 {
					break
				}
				lo, _ := a.([]byte)
				hi, _ := z.([]byte)
				if cid, ok := v.(int); ok && len(lo) > 0 {
					cm.cidRanges = append(cm.cidRanges, pdfCIDRange{uint32(beInt(lo)), uint32(beInt(hi)), cid})
				}
			}
		case "def":
			if len(prev) >= 2 && prev[len(prev)-2] == pdfName("WMode") && prev[len(prev)-1] == 1 {
				cm.vertical = true
			}
		case "usecmap":
			if len(prev) > 0 && prev[len(prev)-1] == pdfName("Identity-H") && len(cm.cs) == 0 {
				cm.cs = []pdfRange{{2, 0, 0xFFFF}}
			}
		}
		prev = prev[:0]
	}
	return cm
}

// approxWidth guesses a glyph width (in 1/1000 em) from its shape class, for fonts without widths.
func approxWidth(t string) float64 {
	w := 0.0
	for _, r := range t {
		switch {
		case r == ' ':
			w += 250
		case strings.ContainsRune("ijlt.,;:'!|fI()[]", r):
			w += 280
		case strings.ContainsRune("mwMW", r):
			w += 830
		case unicode.IsUpper(r):
			w += 680
		case r > 0x2E80:
			w += 1000
		default:
			w += 520
		}
	}
	if w == 0 {
		w = 500
	}
	return w
}

func (d *pdfDoc) font(v any) *pdfFont {
	key := -1
	if r, ok := v.(pdfRef); ok {
		key = r.n
		if f, ok := d.fonts[key]; ok {
			return f
		}
	}
	f := &pdfFont{scale: 0.001, widths: map[int]float64{}}
	fd := d.dict(v)
	if key >= 0 {
		d.fonts[key] = f
	}
	if fd == nil {
		f.simple = pdfBaseEncoding("StandardEncoding")
		f.approx = true
		return f
	}
	if d.name(fd["Subtype"]) == "Type0" {
		f.composite = true
		f.dw = 1000
		var desc pdfDict
		if ds := d.arr(fd["DescendantFonts"]); len(ds) > 0 {
			desc = d.dict(ds[0])
		}
		if desc != nil {
			if w, ok := d.num(desc["DW"]); ok {
				f.dw = w
			}
			if d.name(desc["Subtype"]) == "CIDFontType2" {
				if ff, ok := d.resolve(d.dict(desc["FontDescriptor"])["FontFile2"]).(*pdfStream); ok {
					f.gload = func() []string {
						data, _ := d.decode(ff)
						return ttGlyphNames(data)
					}
				}
				if m, ok := d.resolve(desc["CIDToGIDMap"]).(*pdfStream); ok {
					f.cidToGID, _ = d.decode(m)
				}
			}
			ws := d.arr(desc["W"])
			for i := 0; i < len(ws); {
				first := d.int(ws[i])
				if i+1 < len(ws) {
					if list := d.arr(ws[i+1]); list != nil {
						for k, w := range list {
							f.widths[first+k], _ = d.num(w)
						}
						i += 2
						continue
					}
				}
				if i+2 < len(ws) {
					last := d.int(ws[i+1])
					w, _ := d.num(ws[i+2])
					for c := first; c <= last && c-first < 65536; c++ {
						f.widths[c] = w
					}
				}
				i += 3
			}
		}
		switch e := d.resolve(fd["Encoding"]).(type) {
		case pdfName:
			name := string(e)
			f.vertical = strings.HasSuffix(name, "-V")
			switch {
			case strings.HasPrefix(name, "Identity"):
				f.cs = []pdfRange{{2, 0, 0xFFFF}}
			case strings.Contains(name, "UCS2") || strings.Contains(name, "UTF16"):
				f.uniCodes = true
				f.cs = []pdfRange{{2, 0, 0xFFFF}}
			default:
				for _, s := range pdfCJKSpaces {
					if strings.Contains(name, s.key) {
						f.cs = s.cs
						break
					}
				}
			}
		case *pdfStream:
			data, _ := d.decode(e)
			cm := parseCMap(data)
			f.cs, f.cidChar, f.cidRanges, f.vertical = cm.cs, cm.cidChar, cm.cidRanges, cm.vertical
		}
	} else {
		if d.name(fd["Subtype"]) == "Type3" {
			if fm := d.arr(fd["FontMatrix"]); len(fm) > 0 {
				if s, ok := d.num(fm[0]); ok && s != 0 {
					f.scale = s
				}
			}
		}
		first := d.int(fd["FirstChar"])
		ws := d.arr(fd["Widths"])
		for i, w := range ws {
			f.widths[first+i], _ = d.num(w)
		}
		desc := d.dict(fd["FontDescriptor"])
		f.dw, _ = d.num(desc["MissingWidth"])
		f.approx = len(ws) == 0
		base := ""
		var diffs pdfArr
		switch e := d.resolve(fd["Encoding"]).(type) {
		case pdfName:
			base = string(e)
		case pdfDict:
			base = d.name(e["BaseEncoding"])
			diffs = d.arr(e["Differences"])
		}
		var builtin map[int]string
		if base == "" {
			if ff, ok := d.resolve(desc["FontFile"]).(*pdfStream); ok {
				data, _ := d.decode(ff)
				builtin = type1Encoding(data)
			}
		}
		if base == "" && builtin == nil {
			bf := d.name(fd["BaseFont"])
			switch {
			case strings.Contains(bf, "Symbol"):
				base = "Symbol"
			case strings.Contains(bf, "Dingbats"):
				base = "ZapfDingbats"
			case d.name(fd["Subtype"]) == "TrueType":
				base = "WinAnsiEncoding"
			default:
				base = "StandardEncoding"
			}
		}
		f.simple = pdfBaseEncoding(base)
		for c, n := range builtin {
			f.simple[c] = pdfGlyphText(n)
		}
		code := 0
		for _, e := range diffs {
			switch x := d.resolve(e).(type) {
			case int:
				code = x
			case float64:
				code = int(x)
			case pdfName:
				if code >= 0 && code < 256 {
					f.simple[code] = pdfGlyphText(string(x))
				}
				code++
			}
		}
	}
	if tu, ok := d.resolve(fd["ToUnicode"]).(*pdfStream); ok {
		data, _ := d.decode(tu)
		cm := parseCMap(data)
		if len(cm.uni) > 0 {
			f.uni = cm.uni
		}
		if f.composite && len(f.cs) == 0 {
			f.cs = cm.cs
		}
	}
	if f.composite && len(f.cs) == 0 {
		f.cs = []pdfRange{{2, 0, 0xFFFF}}
	}
	return f
}

func (f *pdfFont) code(b []byte) (int, uint32) {
	var c uint32
	for n := 1; n <= 4 && n <= len(b); n++ {
		c = c<<8 | uint32(b[n-1])
		for _, r := range f.cs {
			if r.n == n && c >= r.lo && c <= r.hi {
				return n, c
			}
		}
	}
	n := f.cs[0].n
	for _, r := range f.cs {
		n = min(n, r.n)
	}
	n = min(max(n, 1), len(b))
	c = 0
	for k := 0; k < n; k++ {
		c = c<<8 | uint32(b[k])
	}
	return n, c
}

func (f *pdfFont) cid(code uint32) int {
	if cid, ok := f.cidChar[code]; ok {
		return cid
	}
	for _, r := range f.cidRanges {
		if code >= r.lo && code <= r.hi {
			return r.cid + int(code-r.lo)
		}
	}
	return int(code)
}

// each calls fn for every character code in s: its text, width (glyph units) and whether it's a single-byte space.
func (f *pdfFont) each(s []byte, fn func(t string, w float64, space bool)) {
	if !f.composite {
		for _, c := range s {
			t, ok := f.uni[uint32(c)]
			if !ok {
				t = f.simple[c]
			}
			w, ok := f.widths[int(c)]
			if !ok || w == 0 && f.approx {
				w = f.dw
				if f.approx {
					w = approxWidth(t)
				}
			}
			fn(t, w, c == 32)
		}
		return
	}
	for i := 0; i < len(s); {
		n, code := f.code(s[i:])
		i += n
		w, ok := f.widths[f.cid(code)]
		if !ok {
			w = f.dw
		}
		t, ok := f.uni[code]
		if (!ok || isPUA(t)) && (f.gload != nil || f.gnames != nil) {
			if g := pdfGlyphText(f.glyphName(f.cid(code))); g != "" {
				t, ok = g, true
			}
		}
		if !ok && f.uniCodes {
			r := rune(code)
			if code >= 0xD800 && code <= 0xDBFF && i+1 < len(s) {
				lo := rune(s[i])<<8 | rune(s[i+1])
				if lo >= 0xDC00 && lo <= 0xDFFF {
					r = utf16.DecodeRune(r, lo)
					i += 2
				}
			}
			t = string(r)
		}
		fn(t, w, n == 1 && code == 32)
	}
}

// ---------- content streams ----------

type pdfMatrix [6]float64

var pdfIdent = pdfMatrix{1, 0, 0, 1, 0, 0}

// mmul returns m then n (row-vector convention, as in the PDF spec).
func mmul(m, n pdfMatrix) pdfMatrix {
	return pdfMatrix{
		m[0]*n[0] + m[1]*n[2], m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2], m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4], m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

func (m pdfMatrix) apply(x, y float64) (float64, float64) {
	return x*m[0] + y*m[2] + m[4], x*m[1] + y*m[3] + m[5]
}

type pdfGS struct {
	ctm                    pdfMatrix
	font                   *pdfFont
	fs, tc, tw, th, tl, ts float64
}

// pdfSink lays text out in drawing order, adding spaces and line breaks from positions.
type pdfSink struct {
	b              strings.Builder
	has, full      bool
	lx, ly, sx, sy float64
	ux, uy, lh     float64
	last           string
	limit, images  int
}

func (s *pdfSink) gap(g string) {
	if str := s.b.String(); len(str) > 0 && str[len(str)-1] != ' ' && str[len(str)-1] != '\n' {
		s.b.WriteString(g)
	}
}

func (s *pdfSink) add(x0, y0, x1, y1, ux, uy, h float64, t string) {
	if t == "" || s.full {
		return
	}
	if h <= 0 || math.IsNaN(h) {
		h = 1
	}
	if s.has {
		if t == s.last && math.Abs(x0-s.sx) < 0.1*h && math.Abs(y0-s.sy) < 0.1*h {
			return // the same text drawn again on top of itself (fake bold, shadows)
		}
		dx, dy := x0-s.lx, y0-s.ly
		along := dx*s.ux + dy*s.uy
		across := dy*s.ux - dx*s.uy
		big, small := math.Max(s.lh, h), math.Min(s.lh, h)
		switch {
		case math.Abs(across) > 0.5*big:
			s.b.WriteByte('\n')
			if math.Abs(across) > 2*big {
				s.b.WriteByte('\n')
			}
		case along > 1.5*small:
			s.gap("  ")
		case along < -0.5*small:
			s.gap(" ")
		case along > 0.15*small && !strings.HasPrefix(t, " "):
			s.gap(" ")
		}
	}
	s.b.WriteString(t)
	s.has, s.last = true, t
	s.lx, s.ly, s.sx, s.sy = x1, y1, x0, y0
	s.ux, s.uy, s.lh = ux, uy, h
	if s.limit > 0 && s.b.Len() >= s.limit {
		s.full = true
	}
}

func (d *pdfDoc) runContent(data []byte, res pdfDict, gs pdfGS, s *pdfSink, depth int) {
	if depth > 8 || s.full {
		return
	}
	fonts := d.dict(res["Font"])
	xobjs := d.dict(res["XObject"])
	l := &pdfLex{b: data}
	var ops []any
	var stack []pdfGS
	tm, tlm := pdfIdent, pdfIdent
	// marked content with /ActualText replaces the glyphs drawn inside it (ligatures, odd fonts)
	type mcItem struct {
		actual, started           bool
		text                      string
		x0, y0, x1, y1, ux, uy, h float64
	}
	var mc []mcItem
	num := func(k int) float64 { // k-th operand from the end (1 = last)
		if k > len(ops) {
			return 0
		}
		v, _ := d.num(ops[len(ops)-k])
		return v
	}
	move := func(tx, ty float64) {
		tlm = mmul(pdfMatrix{1, 0, 0, 1, tx, ty}, tlm)
		tm = tlm
	}
	show := func(str []byte) {
		f := gs.font
		m := mmul(tm, gs.ctm)
		x0, y0 := m.apply(0, gs.ts)
		h := math.Abs(gs.fs) * math.Hypot(m[2], m[3])
		ux, uy := m[0], m[1]
		if n := math.Hypot(ux, uy); n > 0 {
			ux, uy = ux/n, uy/n
		} else {
			ux, uy = 1, 0
		}
		if gs.fs*gs.th < 0 {
			ux, uy = -ux, -uy
		}
		var sb strings.Builder
		f.each(str, func(t string, w float64, space bool) {
			sb.WriteString(t)
			adv := w*f.scale*gs.fs + gs.tc
			if space {
				adv += gs.tw
			}
			if f.vertical {
				tm = mmul(pdfMatrix{1, 0, 0, 1, 0, -adv}, tm)
			} else {
				tm = mmul(pdfMatrix{1, 0, 0, 1, adv * gs.th, 0}, tm)
			}
		})
		x1, y1 := mmul(tm, gs.ctm).apply(0, gs.ts)
		if f.vertical {
			ux, uy = -uy, ux
		}
		for i := len(mc) - 1; i >= 0; i-- {
			if it := &mc[i]; it.actual {
				if !it.started {
					it.started, it.x0, it.y0, it.ux, it.uy, it.h = true, x0, y0, ux, uy, h
				}
				it.x1, it.y1 = x1, y1
				return
			}
		}
		s.add(x0, y0, x1, y1, ux, uy, h, sb.String())
	}
	for !s.full {
		t, err := l.token()
		if err != nil {
			break
		}
		kw, ok := t.(pdfKw)
		if !ok {
			if len(ops) < 64 {
				ops = append(ops, t)
			}
			continue
		}
		switch kw {
		case "q":
			if len(stack) < 256 {
				stack = append(stack, gs)
			}
		case "Q":
			if n := len(stack); n > 0 {
				gs, stack = stack[n-1], stack[:n-1]
			}
		case "cm":
			if len(ops) >= 6 {
				gs.ctm = mmul(pdfMatrix{num(6), num(5), num(4), num(3), num(2), num(1)}, gs.ctm)
			}
		case "BT":
			tm, tlm = pdfIdent, pdfIdent
		case "Tf":
			if len(ops) >= 2 {
				if n, ok := ops[len(ops)-2].(pdfName); ok {
					gs.font = d.font(fonts[string(n)])
				}
				gs.fs = num(1)
			}
		case "Tc":
			gs.tc = num(1)
		case "Tw":
			gs.tw = num(1)
		case "Tz":
			gs.th = num(1) / 100
		case "TL":
			gs.tl = num(1)
		case "Ts":
			gs.ts = num(1)
		case "Td":
			move(num(2), num(1))
		case "TD":
			gs.tl = -num(1)
			move(num(2), num(1))
		case "Tm":
			if len(ops) >= 6 {
				tlm = pdfMatrix{num(6), num(5), num(4), num(3), num(2), num(1)}
				tm = tlm
			}
		case "T*":
			move(0, -gs.tl)
		case "Tj", "'", "\"":
			if kw == "\"" && len(ops) >= 3 {
				gs.tw, gs.tc = num(3), num(2)
			}
			if kw != "Tj" {
				move(0, -gs.tl)
			}
			if len(ops) > 0 {
				if str, ok := ops[len(ops)-1].([]byte); ok {
					show(str)
				}
			}
		case "TJ":
			if len(ops) == 0 {
				break
			}
			arr, _ := ops[len(ops)-1].(pdfArr)
			for _, e := range arr {
				switch x := e.(type) {
				case []byte:
					show(x)
				case int, float64:
					v, _ := d.num(x)
					adj := -v / 1000 * gs.fs
					if gs.font != nil && gs.font.vertical {
						tm = mmul(pdfMatrix{1, 0, 0, 1, 0, adj}, tm)
					} else {
						tm = mmul(pdfMatrix{1, 0, 0, 1, adj * gs.th, 0}, tm)
					}
				}
			}
		case "Do":
			if len(ops) == 0 {
				break
			}
			n, _ := ops[len(ops)-1].(pdfName)
			xo, ok := d.resolve(xobjs[string(n)]).(*pdfStream)
			if !ok {
				break
			}
			switch d.name(xo.d["Subtype"]) {
			case "Image":
				s.images++
			case "Form":
				fdata, _ := d.decode(xo)
				fres := d.dict(xo.d["Resources"])
				if fres == nil {
					fres = res
				}
				fgs := gs
				if m, ok := d.matrix(xo.d["Matrix"]); ok {
					fgs.ctm = mmul(m, gs.ctm)
				}
				d.runContent(fdata, fres, fgs, s, depth+1)
			}
		case "BMC":
			if len(mc) < 64 {
				mc = append(mc, mcItem{})
			}
		case "BDC":
			if len(mc) >= 64 {
				break
			}
			var it mcItem
			if len(ops) > 0 {
				props := d.dict(ops[len(ops)-1])
				if n, ok := ops[len(ops)-1].(pdfName); ok {
					props = d.dict(d.dict(res["Properties"])[string(n)])
				}
				if at, ok := d.resolve(props["ActualText"]).([]byte); ok {
					it.actual, it.text = true, pdfTextString(at)
				}
			}
			mc = append(mc, it)
		case "EMC":
			if n := len(mc); n > 0 {
				it := mc[n-1]
				mc = mc[:n-1]
				if it.actual && it.started {
					s.add(it.x0, it.y0, it.x1, it.y1, it.ux, it.uy, it.h, it.text)
				}
			}
		case "BI":
			s.images++
			for {
				t, err := l.token()
				if err != nil || t == pdfKw("ID") {
					break
				}
			}
			p := l.p + 1
			for {
				j := bytes.Index(l.b[min(p, len(l.b)):], []byte("EI"))
				if j < 0 {
					l.p = len(l.b)
					break
				}
				e := p + j
				if pdfWS(l.b[e-1]) && (e+2 >= len(l.b) || pdfWS(l.b[e+2])) {
					l.p = e + 2
					break
				}
				p = e + 2
			}
		}
		ops = ops[:0]
	}
}

func (d *pdfDoc) pageContent(pg pdfDict) []byte {
	var parts []any
	switch c := d.resolve(pg["Contents"]).(type) {
	case *pdfStream:
		parts = []any{c}
	case pdfArr:
		parts = c
	}
	var b bytes.Buffer
	for _, p := range parts {
		if s, ok := d.resolve(p).(*pdfStream); ok {
			data, _ := d.decode(s)
			b.Write(data)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

var pdfLigatures = strings.NewReplacer("ﬀ", "ff", "ﬁ", "fi", "ﬂ", "fl", "ﬃ", "ffi", "ﬄ", "ffl", "ﬅ", "st", "ﬆ", "st", " ", " ", "­", "")

func cleanPDFText(s string) string {
	s = pdfLigatures.Replace(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0xFFFD || r == 0x7F || (r >= 0xE000 && r <= 0xF8FF):
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// pageText returns a page's text and how many images it draws.
func (d *pdfDoc) pageText(pg pdfDict, limit int) (text string, images int) {
	defer func() {
		if r := recover(); r != nil {
			text = fmt.Sprintf("[page unreadable: %v]", r)
		}
	}()
	s := &pdfSink{limit: limit}
	d.runContent(d.pageContent(pg), d.dict(pg["Resources"]), pdfGS{ctm: pdfIdent, th: 1, font: d.font(nil)}, s, 0)
	text = cleanPDFText(s.b.String())
	var fields []string
	for _, a := range d.arr(pg["Annots"]) {
		ad := d.dict(a)
		switch d.name(ad["Subtype"]) {
		case "Widget":
			if v := d.str(ad["V"]); len(v) > 0 {
				name := pdfTextString(d.str(ad["T"]))
				if name == "" {
					name = pdfTextString(d.str(d.dict(ad["Parent"])["T"]))
				}
				fields = append(fields, strings.TrimSpace(name+": "+pdfTextString(v)))
			}
		case "FreeText", "Text":
			if v := d.str(ad["Contents"]); len(v) > 0 {
				fields = append(fields, "note: "+pdfTextString(v))
			}
		}
	}
	if len(fields) > 0 {
		if text != "" {
			text += "\n"
		}
		text += strings.Join(fields, "\n")
	}
	return text, s.images
}

// ---------- whole documents ----------

type pdfText struct {
	pages  []string // text of each page; "" when the page has none
	images []int    // images drawn on each page
	title  string
}

// scanned lists the pages (1-based) that have images but no text: candidates for OCR.
func (t *pdfText) scanned() []int {
	var out []int
	for i, p := range t.pages {
		if len(strings.TrimSpace(p)) < 3 && t.images[i] > 0 {
			out = append(out, i+1)
		}
	}
	return out
}

// readPDFText extracts the text of the wanted pages (nil = all), stopping near limit bytes.
func readPDFText(d *pdfDoc, want map[int]bool, limit int) *pdfText {
	pages := d.pages()
	t := &pdfText{pages: make([]string, len(pages)), images: make([]int, len(pages))}
	if info := d.dict(d.trailer["Info"]); info != nil {
		t.title = strings.TrimSpace(pdfTextString(d.str(info["Title"])))
	}
	total := 0
	for i, pg := range pages {
		if want != nil && !want[i+1] {
			continue
		}
		if limit > 0 && total >= limit {
			break
		}
		t.pages[i], t.images[i] = d.pageText(pg, limit-total)
		total += len(t.pages[i])
	}
	return t
}

func (d *pdfDoc) pages() []pdfDict {
	var out []pdfDict
	seen := map[int]bool{}
	var walk func(v any, inh pdfDict, depth int)
	walk = func(v any, inh pdfDict, depth int) {
		if depth > 64 || len(out) > 200000 {
			return
		}
		if r, ok := v.(pdfRef); ok {
			if seen[r.n] {
				return
			}
			seen[r.n] = true
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
		pg := pdfDict{}
		for k, e := range inh {
			pg[k] = e
		}
		for k, e := range n {
			pg[k] = e
		}
		out = append(out, pg)
	}
	if root := d.dict(d.trailer["Root"]); root != nil {
		walk(root["Pages"], pdfDict{}, 0)
	}
	if len(out) == 0 {
		for _, n := range d.objNums() {
			if dct, ok := d.load(n).(pdfDict); ok && d.name(dct["Type"]) == "Page" {
				out = append(out, dct)
			}
		}
	}
	return out
}
