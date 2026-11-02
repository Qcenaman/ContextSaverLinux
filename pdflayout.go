package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
)

// Laying out Markdown on Letter pages with the standard PDF fonts.

type pdfWord struct {
	text  string
	font  int // 1 regular, 2 bold, 3 italic, 4 bold italic, 5 mono
	size  float64
	link  string
	w     float64
	space bool // a space follows
}

var fontNames = map[int]string{1: "Helvetica", 2: "Helvetica-Bold", 3: "Helvetica-Oblique", 4: "Helvetica-BoldOblique", 5: "Courier"}

func fontWidths(f int) map[string]int {
	switch f {
	case 2, 4:
		return helvBW
	case 5:
		return nil
	}
	return helvW
}

type pdfLay struct {
	j       *pdfJob
	base    string
	total   *int
	pw, ph  float64
	m       float64
	y       float64
	ops     bytes.Buffer
	annots  pdfArr
	xobjs   pdfDict
	open    bool
	fontRes pdfDict
}

func (l *pdfLay) newPage() {
	l.flush()
	l.open = true
	l.y = l.ph - l.m
}

func (l *pdfLay) flush() {
	if !l.open {
		return
	}
	cs := l.j.w.add(&pdfStream{d: pdfDict{"Filter": pdfName("FlateDecode")}, raw: zlibBytes(l.ops.Bytes())})
	res := pdfDict{"Font": l.fontRes}
	if len(l.xobjs) > 0 {
		res["XObject"] = l.xobjs
	}
	pg := pdfDict{"Type": pdfName("Page"), "Parent": l.j.pages, "MediaBox": pdfArr{0, 0, l.pw, l.ph}, "Contents": cs, "Resources": res}
	if len(l.annots) > 0 {
		pg["Annots"] = l.annots
	}
	l.j.kids = append(l.j.kids, l.j.w.add(pg))
	*l.total++
	l.ops.Reset()
	l.annots, l.xobjs, l.open = nil, pdfDict{}, false
}

func (l *pdfLay) need(h float64) {
	if !l.open {
		l.newPage()
	}
	if l.y-h < l.m && l.y < l.ph-l.m-1 {
		l.newPage()
	}
}

func words(sp []mdSpan, size float64, forceBold bool) []pdfWord {
	var out []pdfWord
	for _, s := range sp {
		f := 1
		switch {
		case s.code:
			f = 5
		case (s.bold || forceBold) && s.italic:
			f = 4
		case s.bold || forceBold:
			f = 2
		case s.italic:
			f = 3
		}
		sz := size
		if f == 5 {
			sz = size * 0.92
		}
		parts := strings.Split(s.text, " ")
		for i, p := range parts {
			if p == "" {
				if len(out) > 0 && i == 0 && len(parts) > 1 {
					out[len(out)-1].space = true
				}
				continue
			}
			w := pdfWord{text: p, font: f, size: sz, link: s.link, w: textWidth(p, fontWidths(f), sz)}
			w.space = i < len(parts)-1
			out = append(out, w)
		}
	}
	return out
}

// wrap breaks words into lines no wider than width (long words are cut).
func wrap(ws []pdfWord, width float64) [][]pdfWord {
	var lines [][]pdfWord
	var cur []pdfWord
	x := 0.0
	for _, w := range ws {
		for w.w > width && len([]rune(w.text)) > 1 {
			// cut an over-long word
			r := []rune(w.text)
			n := len(r) - 1
			for n > 1 && textWidth(string(r[:n]), fontWidths(w.font), w.size) > width {
				n--
			}
			head := w
			head.text, head.w, head.space = string(r[:n]), textWidth(string(r[:n]), fontWidths(w.font), w.size), false
			if len(cur) > 0 {
				lines = append(lines, cur)
				cur, x = nil, 0
			}
			lines = append(lines, []pdfWord{head})
			w.text = string(r[n:])
			w.w = textWidth(w.text, fontWidths(w.font), w.size)
		}
		sp := 0.0
		if len(cur) > 0 && cur[len(cur)-1].space {
			sp = textWidth(" ", fontWidths(cur[len(cur)-1].font), cur[len(cur)-1].size)
		}
		if len(cur) > 0 && x+sp+w.w > width {
			lines = append(lines, cur)
			cur, x, sp = nil, 0, 0
		}
		cur = append(cur, w)
		x += sp + w.w
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

func (l *pdfLay) drawLine(line []pdfWord, x, baseline float64) {
	for i, w := range line {
		if w.link != "" {
			l.ops.WriteString("0 0.35 0.75 rg ")
		}
		fmt.Fprintf(&l.ops, "BT /F%d %s Tf %s %s Td %s Tj ET\n", w.font, pdfNum(w.size), pdfNum(x), pdfNum(baseline), pdfLitStr(w.text))
		if w.link != "" {
			l.ops.WriteString("0 g ")
			fmt.Fprintf(&l.ops, "0 0.35 0.75 RG 0.6 w %s %s m %s %s l S 0 G\n", pdfNum(x), pdfNum(baseline-1.5), pdfNum(x+w.w), pdfNum(baseline-1.5))
			l.annots = append(l.annots, pdfDict{"Type": pdfName("Annot"), "Subtype": pdfName("Link"), "Border": pdfArr{0, 0, 0},
				"Rect": pdfArr{x, baseline - 3, x + w.w, baseline + w.size}, "A": pdfDict{"S": pdfName("URI"), "URI": []byte(w.link)}})
		}
		x += w.w
		if w.space && i < len(line)-1 {
			x += textWidth(" ", fontWidths(w.font), w.size)
		}
	}
}

// text lays out a paragraph at x with the given width; returns nothing, moves y.
func (l *pdfLay) text(ws []pdfWord, x, width, size, leading float64, marker string) {
	lines := wrap(ws, width)
	for i, line := range lines {
		l.need(leading)
		base := l.y - size
		if i == 0 && marker != "" {
			fmt.Fprintf(&l.ops, "BT /F1 %s Tf %s %s Td %s Tj ET\n", pdfNum(size), pdfNum(x-textWidth(marker, helvW, size)-5), pdfNum(base), pdfLitStr(marker))
		}
		l.drawLine(line, x, base)
		l.y -= leading
	}
}

func (j *pdfJob) layoutMarkdown(blocks []mdBlock, base string, landscape bool, total *int) error {
	l := &pdfLay{j: j, base: base, total: total, pw: 612, ph: 792, m: 60, xobjs: pdfDict{}, fontRes: pdfDict{}}
	if landscape {
		l.pw, l.ph = 792, 612
	}
	for i, n := range fontNames {
		l.fontRes[fmt.Sprintf("F%d", i)] = j.font(n)
	}
	l.newPage()
	W := l.pw - 2*l.m
	counters := map[int]int{}
	for i, b := range blocks {
		if b.kind != "ol" && b.kind != "ul" {
			counters = map[int]int{}
		}
		top := l.y >= l.ph-l.m-1
		switch b.kind {
		case "h":
			sizes := []float64{22, 17, 14, 12.5, 11.5, 11}
			sz := sizes[min(b.level, 6)-1]
			if !top {
				l.y -= sz * 0.6
			}
			l.need(sz*1.3 + 30) // keep a heading with what follows
			l.text(words(b.spans, sz, true), l.m, W, sz, sz*1.25, "")
			if b.level <= 1 {
				fmt.Fprintf(&l.ops, "0.75 G 0.8 w %s %s m %s %s l S 0 G\n", pdfNum(l.m), pdfNum(l.y+2), pdfNum(l.m+W), pdfNum(l.y+2))
				l.y -= 6
			} else {
				l.y -= 3
			}
		case "p":
			l.text(words(b.spans, 11, false), l.m, W, 11, 15, "")
			l.y -= 7
		case "ul", "ol":
			ind := 18 + 18*float64(min(b.level, 6))
			marker := "•"
			if b.level%2 == 1 {
				marker = "–"
			}
			if b.kind == "ol" {
				for k := range counters {
					if k > b.level {
						delete(counters, k)
					}
				}
				counters[b.level]++
				marker = fmt.Sprintf("%d.", counters[b.level])
			}
			l.text(words(b.spans, 11, false), l.m+ind, W-ind, 11, 15, marker)
			l.y -= 2
			if i+1 >= len(blocks) || (blocks[i+1].kind != "ul" && blocks[i+1].kind != "ol") {
				l.y -= 6
			}
		case "code":
			for _, line := range b.lines {
				l.need(12)
				fmt.Fprintf(&l.ops, "0.95 g %s %s %s %s re f 0 g\n", pdfNum(l.m), pdfNum(l.y-12), pdfNum(W), pdfNum(12))
				ws := []pdfWord{{text: line, font: 5, size: 9, w: textWidth(line, nil, 9)}}
				for _, ln := range wrap(ws, W-8) {
					l.need(12)
					l.drawLine(ln, l.m+4, l.y-9.5)
					l.y -= 12
				}
			}
			l.y -= 8
		case "break":
			if !top {
				l.newPage()
			}
		case "table":
			l.table(b.rows, W)
			l.y -= 10
		case "img":
			ip, err := officeImagePath(b.path, l.base)
			if err != nil {
				return err
			}
			xo, iw, ih, err := j.imageXObject(ip)
			if err != nil {
				return err
			}
			s := math.Min(W/iw, 0.75) // 96 dpi pictures at their natural size
			if ih*s > l.ph-2*l.m {
				s = (l.ph - 2*l.m) / ih
			}
			dw, dh := iw*s, ih*s
			l.need(dh + 6)
			name := fmt.Sprintf("Im%d", len(l.xobjs)+1)
			l.xobjs[name] = xo
			fmt.Fprintf(&l.ops, "q %s 0 0 %s %s %s cm /%s Do Q\n", pdfNum(dw), pdfNum(dh), pdfNum(l.m+(W-dw)/2), pdfNum(l.y-dh), name)
			l.y -= dh + 10
		}
	}
	l.flush()
	return nil
}

func (l *pdfLay) table(rows [][][]mdSpan, W float64) {
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return
	}
	size, lead, pad := 10.0, 13.0, 4.0
	nat := make([]float64, cols)
	for ri, r := range rows {
		for c := 0; c < len(r); c++ {
			nat[c] = math.Max(nat[c], math.Min(textWidth(spansText(r[c]), fontWidths(map[bool]int{true: 2, false: 1}[ri == 0]), size)+2*pad, W*0.6))
		}
	}
	sum := 0.0
	for _, v := range nat {
		sum += math.Max(v, 30)
	}
	cw := make([]float64, cols)
	for c := range cw {
		cw[c] = math.Max(nat[c], 30)
		if sum > W {
			cw[c] = cw[c] * W / sum
		}
	}
	tw := 0.0
	for _, v := range cw {
		tw += v
	}
	drawRow := func(r [][]mdSpan, header bool) {
		cells := make([][][]pdfWord, cols)
		lines := 1
		for c := 0; c < cols; c++ {
			var sp []mdSpan
			if c < len(r) {
				sp = r[c]
			}
			cells[c] = wrap(words(sp, size, header), cw[c]-2*pad)
			lines = max(lines, len(cells[c]))
		}
		h := float64(lines)*lead + 2*pad - 2
		l.need(h)
		if l.y-h < l.m { // taller than a page: let it run off rather than loop
			h = l.y - l.m
		}
		if header {
			fmt.Fprintf(&l.ops, "0.92 g %s %s %s %s re f 0 g\n", pdfNum(l.m), pdfNum(l.y-h), pdfNum(tw), pdfNum(h))
		}
		x := l.m
		for c := 0; c < cols; c++ {
			for i, ln := range cells[c] {
				l.drawLine(ln, x+pad, l.y-pad-size+1-float64(i)*lead)
			}
			fmt.Fprintf(&l.ops, "0.6 G 0.5 w %s %s %s %s re S 0 G\n", pdfNum(x), pdfNum(l.y-h), pdfNum(cw[c]), pdfNum(h))
			x += cw[c]
		}
		l.y -= h
	}
	for ri, r := range rows {
		if ri > 0 {
			// Repeat the header row when a table continues on a new page.
			hlines := 1
			for c := 0; c < cols && c < len(r); c++ {
				hlines = max(hlines, len(wrap(words(r[c], size, false), cw[c]-2*pad)))
			}
			if l.y-(float64(hlines)*lead+2*pad) < l.m {
				l.newPage()
				drawRow(rows[0], true)
			}
		}
		drawRow(r, ri == 0)
	}
}
