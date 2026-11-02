package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/ascii85"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
)

// A small PDF reader: just enough structure to pull the text out of a file.
// Handles xref tables and streams, object streams, damaged xrefs (rebuilt by
// scanning the file), the common filters, and the standard security handler
// with an empty user password (most "protected" PDFs only restrict copying).

type (
	pdfName string
	pdfKw   string
	pdfArr  []any
	pdfDict map[string]any
	pdfRef  struct{ n, g int }
)

type pdfStream struct {
	d    pdfDict
	raw  []byte
	n, g int
}

var (
	errPDFPassword = errors.New("this PDF needs a password to open")
	errPDFBad      = errors.New("damaged PDF")
)

// ---------- lexer ----------

type pdfLex struct {
	b []byte
	p int
}

func pdfWS(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}

func pdfDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func (l *pdfLex) skip() {
	for l.p < len(l.b) {
		c := l.b[l.p]
		if pdfWS(c) {
			l.p++
		} else if c == '%' {
			for l.p < len(l.b) && l.b[l.p] != '\n' && l.b[l.p] != '\r' {
				l.p++
			}
		} else {
			return
		}
	}
}

// token reads the next object or keyword; arrays and dictionaries come back whole.
func (l *pdfLex) token() (any, error) { return l.tok(0) }

func (l *pdfLex) tok(depth int) (any, error) {
	l.skip()
	if l.p >= len(l.b) {
		return nil, io.EOF
	}
	if depth > 100 {
		return nil, errPDFBad
	}
	c := l.b[l.p]
	switch {
	case c == '/':
		l.p++
		s := l.p
		for l.p < len(l.b) && !pdfWS(l.b[l.p]) && !pdfDelim(l.b[l.p]) {
			l.p++
		}
		return pdfName(pdfUnescapeName(l.b[s:l.p])), nil
	case c == '(':
		return l.literal(), nil
	case c == '<':
		if l.p+1 < len(l.b) && l.b[l.p+1] == '<' {
			l.p += 2
			items := l.until(">>", depth)
			d := pdfDict{}
			for i := 0; i+1 < len(items); i++ {
				if k, ok := items[i].(pdfName); ok {
					d[string(k)] = items[i+1]
					i++
				}
			}
			return d, nil
		}
		return l.hex(), nil
	case c == '[':
		l.p++
		return pdfArr(l.until("]", depth)), nil
	case c == '>':
		if l.p+1 < len(l.b) && l.b[l.p+1] == '>' {
			l.p += 2
			return pdfKw(">>"), nil
		}
		l.p++
		return pdfKw(">"), nil
	case c == ']' || c == ')' || c == '{' || c == '}':
		l.p++
		return pdfKw(string(c)), nil
	case c == '+' || c == '-' || c == '.' || (c >= '0' && c <= '9'):
		return l.number(), nil
	}
	s := l.p
	for l.p < len(l.b) && !pdfWS(l.b[l.p]) && !pdfDelim(l.b[l.p]) {
		l.p++
	}
	switch w := string(l.b[s:l.p]); w {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	default:
		return pdfKw(w), nil
	}
}

// until collects objects up to the end keyword, turning "n g R" into references.
func (l *pdfLex) until(end string, depth int) []any {
	var items []any
	for {
		t, err := l.tok(depth + 1)
		if err != nil {
			return items
		}
		if k, ok := t.(pdfKw); ok {
			if string(k) == end {
				return items
			}
			if k == "R" && len(items) >= 2 {
				n, ok1 := items[len(items)-2].(int)
				g, ok2 := items[len(items)-1].(int)
				if ok1 && ok2 {
					items = append(items[:len(items)-2], pdfRef{n, g})
					continue
				}
			}
			switch k {
			case "endobj", "stream", "endstream", ">>", "]", "obj":
				l.p -= len(k) // unclosed: leave it for the caller
				return items
			}
		}
		items = append(items, t)
	}
}

func (l *pdfLex) number() any {
	neg := false
	for l.p < len(l.b) && (l.b[l.p] == '+' || l.b[l.p] == '-') {
		neg = l.b[l.p] == '-'
		l.p++
	}
	s, dot := l.p, false
	for l.p < len(l.b) {
		c := l.b[l.p]
		if c >= '0' && c <= '9' {
			l.p++
		} else if c == '.' && !dot {
			dot = true
			l.p++
		} else {
			break
		}
	}
	digits := string(l.b[s:l.p])
	if !dot {
		if v, err := strconv.Atoi(digits); err == nil {
			if neg {
				v = -v
			}
			return v
		}
	}
	f, _ := strconv.ParseFloat(digits, 64)
	if neg {
		f = -f
	}
	return f
}

func (l *pdfLex) literal() []byte {
	l.p++
	var out []byte
	depth := 1
	for l.p < len(l.b) {
		c := l.b[l.p]
		l.p++
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return out
			}
		case '\\':
			if l.p >= len(l.b) {
				return out
			}
			e := l.b[l.p]
			l.p++
			switch e {
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case '\r':
				if l.p < len(l.b) && l.b[l.p] == '\n' {
					l.p++
				}
				continue
			case '\n':
				continue
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for k := 0; k < 2 && l.p < len(l.b) && l.b[l.p] >= '0' && l.b[l.p] <= '7'; k++ {
						v = v*8 + int(l.b[l.p]-'0')
						l.p++
					}
					c = byte(v)
				} else {
					c = e
				}
			}
		}
		out = append(out, c)
	}
	return out
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func (l *pdfLex) hex() []byte {
	l.p++
	var out []byte
	hi := -1
	for l.p < len(l.b) {
		c := l.b[l.p]
		l.p++
		if c == '>' {
			break
		}
		v := hexVal(c)
		if v < 0 {
			continue
		}
		if hi < 0 {
			hi = v
		} else {
			out = append(out, byte(hi<<4|v))
			hi = -1
		}
	}
	if hi >= 0 {
		out = append(out, byte(hi<<4))
	}
	return out
}

func pdfUnescapeName(b []byte) string {
	if bytes.IndexByte(b, '#') < 0 {
		return string(b)
	}
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] == '#' && i+2 < len(b) && hexVal(b[i+1]) >= 0 && hexVal(b[i+2]) >= 0 {
			out = append(out, byte(hexVal(b[i+1])<<4|hexVal(b[i+2])))
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return string(out)
}

// ---------- document ----------

type pdfXref struct{ typ, a, b int } // typ 1: offset a, gen b; typ 2: object stream a, index b

type pdfObjStm struct {
	data  []byte
	first int
	offs  map[int]int
}

type pdfDoc struct {
	b       []byte
	hdr     int
	xref    map[int]pdfXref
	trailer pdfDict
	cache   map[int]any
	objstms map[int]*pdfObjStm
	crypt   *pdfCrypt
	encRef  int
	depth   int
	rebuilt bool
	stmIdx  bool
	fonts   map[int]*pdfFont
}

func openPDF(p string) (*pdfDoc, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if fi.Size() > 1<<30 {
		return nil, errors.New("PDF is over 1 GB")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return parsePDF(b)
}

func parsePDF(b []byte) (*pdfDoc, error) {
	hdr := bytes.Index(b[:min(len(b), 1024)], []byte("%PDF-"))
	if hdr < 0 {
		return nil, errors.New("not a PDF file")
	}
	d := &pdfDoc{b: b, hdr: hdr, xref: map[int]pdfXref{}, cache: map[int]any{}, objstms: map[int]*pdfObjStm{}, encRef: -1, fonts: map[int]*pdfFont{}}
	if err := d.readXref(); err != nil || d.trailer["Root"] == nil {
		d.rebuild()
	}
	if d.trailer == nil {
		return nil, errPDFBad
	}
	if enc := d.trailer["Encrypt"]; enc != nil {
		if r, ok := enc.(pdfRef); ok {
			d.encRef = r.n
		}
		ed := d.dict(enc)
		if ed == nil {
			return nil, errPDFBad
		}
		if err := d.setupCrypt(ed); err != nil {
			return nil, err
		}
		d.cache = map[int]any{}
		d.objstms = map[int]*pdfObjStm{}
	}
	if d.dict(d.trailer["Root"]) == nil {
		if !d.rebuilt {
			d.rebuild()
		}
		if d.dict(d.trailer["Root"]) == nil {
			d.findRoot()
		}
	}
	return d, nil
}

func (d *pdfDoc) readXref() error {
	tail := d.b[max(0, len(d.b)-1<<16):]
	i := bytes.LastIndex(tail, []byte("startxref"))
	if i < 0 {
		return errPDFBad
	}
	l := &pdfLex{b: tail, p: i + len("startxref")}
	t, _ := l.token()
	off, ok := t.(int)
	if !ok {
		return errPDFBad
	}
	seen := map[int]bool{}
	for n := 0; off > 0 && off < len(d.b) && !seen[off] && n < 500; n++ {
		seen[off] = true
		tr, err := d.xrefSection(off)
		if err != nil && d.hdr > 0 {
			tr, err = d.xrefSection(off + d.hdr)
		}
		if err != nil {
			if d.trailer == nil {
				return err
			}
			break
		}
		if d.trailer == nil {
			d.trailer = pdfDict{}
		}
		for k, v := range tr {
			if _, have := d.trailer[k]; !have {
				d.trailer[k] = v
			}
		}
		if xs, ok := tr["XRefStm"].(int); ok {
			d.xrefSection(xs)
		}
		prev, ok := tr["Prev"].(int)
		if !ok {
			break
		}
		off = prev
	}
	if d.trailer == nil {
		return errPDFBad
	}
	return nil
}

func (d *pdfDoc) xrefSection(off int) (pdfDict, error) {
	if off < 0 || off >= len(d.b) {
		return nil, errPDFBad
	}
	l := &pdfLex{b: d.b, p: off}
	l.skip()
	if bytes.HasPrefix(d.b[l.p:], []byte("xref")) {
		l.p += 4
		for {
			t, err := l.token()
			if err != nil {
				return nil, err
			}
			if kw, ok := t.(pdfKw); ok && kw == "trailer" {
				t, err := l.token()
				tr, ok := t.(pdfDict)
				if err != nil || !ok {
					return nil, errPDFBad
				}
				return tr, nil
			}
			start, ok := t.(int)
			t2, _ := l.token()
			count, ok2 := t2.(int)
			if !ok || !ok2 || count < 0 {
				return nil, errPDFBad
			}
			for k := 0; k < count; k++ {
				a, _ := l.token()
				g, _ := l.token()
				typ, _ := l.token()
				o, ok1 := a.(int)
				gen, ok2 := g.(int)
				kw, ok3 := typ.(pdfKw)
				if !ok1 || !ok2 || !ok3 {
					return nil, errPDFBad
				}
				if _, have := d.xref[start+k]; !have && kw == "n" && o > 0 {
					d.xref[start+k] = pdfXref{1, o, gen}
				}
			}
		}
	}
	obj, _, _, err := d.parseIndirect(l.p)
	if err != nil {
		return nil, err
	}
	s, ok := obj.(*pdfStream)
	if !ok || d.name(s.d["Type"]) != "XRef" {
		return nil, errPDFBad
	}
	data, err := d.decode(s)
	if err != nil && len(data) == 0 {
		return nil, err
	}
	w := d.ints(s.d["W"])
	if len(w) < 3 || w[0] < 0 || w[1] < 0 || w[2] < 0 {
		return nil, errPDFBad
	}
	idx := d.ints(s.d["Index"])
	if len(idx) < 2 {
		idx = []int{0, d.int(s.d["Size"])}
	}
	es := w[0] + w[1] + w[2]
	if es == 0 {
		return nil, errPDFBad
	}
	p := 0
	for i := 0; i+1 < len(idx); i += 2 {
		for k := 0; k < idx[i+1] && p+es <= len(data); k++ {
			typ := 1
			if w[0] > 0 {
				typ = beInt(data[p : p+w[0]])
			}
			f2 := beInt(data[p+w[0] : p+w[0]+w[1]])
			f3 := beInt(data[p+w[0]+w[1] : p+es])
			p += es
			n := idx[i] + k
			if _, have := d.xref[n]; have {
				continue
			}
			switch {
			case typ == 1 && f2 > 0:
				d.xref[n] = pdfXref{1, f2, f3}
			case typ == 2:
				d.xref[n] = pdfXref{2, f2, f3}
			}
		}
	}
	return s.d, nil
}

func beInt(b []byte) int {
	v := 0
	for _, c := range b {
		v = v<<8 | int(c)
	}
	return v
}

// parseIndirect reads "n g obj ... endobj" at off.
func (d *pdfDoc) parseIndirect(off int) (any, int, int, error) {
	if off < 0 || off >= len(d.b) {
		return nil, 0, 0, errPDFBad
	}
	l := &pdfLex{b: d.b, p: off}
	t1, _ := l.token()
	t2, _ := l.token()
	t3, _ := l.token()
	n, ok1 := t1.(int)
	g, ok2 := t2.(int)
	kw, ok3 := t3.(pdfKw)
	if !ok1 || !ok2 || !ok3 || kw != "obj" {
		return nil, 0, 0, errPDFBad
	}
	v, err := l.token()
	if err != nil {
		return nil, 0, 0, err
	}
	dct, ok := v.(pdfDict)
	if !ok {
		if k, isKw := v.(pdfKw); isKw && k == "endobj" {
			v = nil
		}
		return v, n, g, nil
	}
	save := l.p
	if t, _ := l.token(); t != pdfKw("stream") {
		l.p = save
		return dct, n, g, nil
	}
	p := l.p
	if p < len(d.b) && d.b[p] == '\r' {
		p++
	}
	if p < len(d.b) && d.b[p] == '\n' {
		p++
	}
	ln := -1
	switch x := dct["Length"].(type) {
	case int:
		ln = x
	case pdfRef:
		if x.n != n && d.depth < 8 {
			if v, ok := d.resolve(x).(int); ok {
				ln = v
			}
		}
	}
	end := -1
	if ln >= 0 && p+ln <= len(d.b) {
		k := p + ln
		for k < len(d.b) && pdfWS(d.b[k]) {
			k++
		}
		if bytes.HasPrefix(d.b[k:], []byte("endstream")) {
			end = p + ln
		}
	}
	if end < 0 {
		i := bytes.Index(d.b[p:], []byte("endstream"))
		if i < 0 {
			end = len(d.b)
		} else {
			end = p + i
			if end > p && d.b[end-1] == '\n' {
				end--
			}
			if end > p && d.b[end-1] == '\r' {
				end--
			}
		}
	}
	return &pdfStream{d: dct, raw: d.b[p:end], n: n, g: g}, n, g, nil
}

var pdfObjRe = regexp.MustCompile(`(\d{1,10})[ \t\r\n\f\x00]+(\d{1,5})[ \t\r\n\f\x00]+obj\b`)

// rebuild finds every object by scanning the file, for PDFs whose xref is wrong.
func (d *pdfDoc) rebuild() {
	d.rebuilt = true
	d.cache = map[int]any{}
	d.objstms = map[int]*pdfObjStm{}
	for _, m := range pdfObjRe.FindAllSubmatchIndex(d.b, -1) {
		if m[0] > 0 && !pdfWS(d.b[m[0]-1]) && !pdfDelim(d.b[m[0]-1]) {
			continue
		}
		n, _ := strconv.Atoi(string(d.b[m[2]:m[3]]))
		g, _ := strconv.Atoi(string(d.b[m[4]:m[5]]))
		d.xref[n] = pdfXref{1, m[0], g}
	}
	tr := pdfDict{}
	if i := bytes.LastIndex(d.b, []byte("trailer")); i >= 0 {
		l := &pdfLex{b: d.b, p: i + 7}
		if t, err := l.token(); err == nil {
			if td, ok := t.(pdfDict); ok {
				tr = td
			}
		}
	}
	if tr["Root"] == nil {
		for _, n := range d.objNums() {
			if s, ok := d.load(n).(*pdfStream); ok && d.name(s.d["Type"]) == "XRef" {
				for k, v := range s.d {
					tr[k] = v
				}
			}
		}
	}
	if d.trailer == nil {
		d.trailer = pdfDict{}
	}
	for k, v := range tr {
		if _, have := d.trailer[k]; !have || k == "Root" {
			d.trailer[k] = v
		}
	}
	d.cache = map[int]any{}
}

func (d *pdfDoc) objNums() []int {
	ns := make([]int, 0, len(d.xref))
	for n := range d.xref {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	return ns
}

// indexObjStms adds the objects inside object streams (after a rebuild).
func (d *pdfDoc) indexObjStms() {
	d.stmIdx = true
	for _, n := range d.objNums() {
		e := d.xref[n]
		if e.typ != 1 {
			continue
		}
		s, ok := d.load(n).(*pdfStream)
		if !ok || d.name(s.d["Type"]) != "ObjStm" {
			continue
		}
		os := d.objStm(n)
		if os == nil {
			continue
		}
		for on := range os.offs {
			if _, have := d.xref[on]; !have {
				d.xref[on] = pdfXref{2, n, 0}
			}
		}
	}
}

func (d *pdfDoc) findRoot() {
	if !d.stmIdx {
		d.indexObjStms()
	}
	for _, n := range d.objNums() {
		if dct, ok := d.load(n).(pdfDict); ok && d.name(dct["Type"]) == "Catalog" {
			d.trailer["Root"] = pdfRef{n, d.xref[n].b}
			return
		}
	}
}

func (d *pdfDoc) load(n int) any {
	if v, ok := d.cache[n]; ok {
		return v
	}
	e, ok := d.xref[n]
	if !ok {
		if d.rebuilt && !d.stmIdx {
			d.indexObjStms()
			if e, ok = d.xref[n]; !ok {
				return nil
			}
		} else {
			return nil
		}
	}
	if d.depth > 48 {
		return nil
	}
	d.depth++
	defer func() { d.depth-- }()
	d.cache[n] = nil
	var v any
	switch e.typ {
	case 1:
		obj, num, gen, err := d.parseIndirect(e.a)
		if (err != nil || num != n) && d.hdr > 0 {
			obj, num, gen, err = d.parseIndirect(e.a + d.hdr)
		}
		if err != nil || num != n {
			if !d.rebuilt {
				d.rebuild()
				delete(d.cache, n)
				return d.load(n)
			}
			return nil
		}
		if d.crypt != nil && n != d.encRef {
			obj = d.decryptObj(obj, num, gen)
		}
		v = obj
	case 2:
		if os := d.objStm(e.a); os != nil {
			if off, ok := os.offs[n]; ok {
				l := &pdfLex{b: os.data, p: os.first + off}
				v, _ = l.token()
			}
		}
	}
	d.cache[n] = v
	return v
}

func (d *pdfDoc) objStm(sn int) *pdfObjStm {
	if os, ok := d.objstms[sn]; ok {
		return os
	}
	d.objstms[sn] = nil
	s, ok := d.load(sn).(*pdfStream)
	if !ok {
		return nil
	}
	data, err := d.decode(s)
	if err != nil && len(data) == 0 {
		return nil
	}
	os := &pdfObjStm{data: data, first: d.int(s.d["First"]), offs: map[int]int{}}
	l := &pdfLex{b: data}
	for i := 0; i < d.int(s.d["N"]); i++ {
		a, err1 := l.token()
		b, err2 := l.token()
		on, ok1 := a.(int)
		oo, ok2 := b.(int)
		if err1 != nil || err2 != nil || !ok1 || !ok2 {
			break
		}
		os.offs[on] = oo
	}
	d.objstms[sn] = os
	return os
}

func (d *pdfDoc) resolve(v any) any {
	for i := 0; i < 16; i++ {
		r, ok := v.(pdfRef)
		if !ok {
			return v
		}
		v = d.load(r.n)
	}
	return nil
}

func (d *pdfDoc) dict(v any) pdfDict {
	switch x := d.resolve(v).(type) {
	case pdfDict:
		return x
	case *pdfStream:
		return x.d
	}
	return nil
}

func (d *pdfDoc) arr(v any) pdfArr {
	a, _ := d.resolve(v).(pdfArr)
	return a
}

func (d *pdfDoc) name(v any) string {
	n, _ := d.resolve(v).(pdfName)
	return string(n)
}

func (d *pdfDoc) str(v any) []byte {
	s, _ := d.resolve(v).([]byte)
	return s
}

func (d *pdfDoc) num(v any) (float64, bool) {
	switch x := d.resolve(v).(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func (d *pdfDoc) int(v any) int {
	f, _ := d.num(v)
	return int(f)
}

func (d *pdfDoc) ints(v any) []int {
	var out []int
	for _, e := range d.arr(v) {
		out = append(out, d.int(e))
	}
	return out
}

func (d *pdfDoc) matrix(v any) (pdfMatrix, bool) {
	a := d.arr(v)
	if len(a) != 6 {
		return pdfMatrix{}, false
	}
	var m pdfMatrix
	for i := range m {
		m[i], _ = d.num(a[i])
	}
	return m, true
}

// ---------- filters ----------

func (d *pdfDoc) decode(s *pdfStream) ([]byte, error) {
	var filters []string
	switch x := d.resolve(s.d["Filter"]).(type) {
	case pdfName:
		filters = []string{string(x)}
	case pdfArr:
		for _, e := range x {
			filters = append(filters, d.name(e))
		}
	}
	parms := d.resolve(s.d["DecodeParms"])
	if parms == nil {
		parms = d.resolve(s.d["DP"])
	}
	data := s.raw
	for i, f := range filters {
		var p pdfDict
		switch x := parms.(type) {
		case pdfDict:
			p = x
		case pdfArr:
			if i < len(x) {
				p = d.dict(x[i])
			}
		}
		var err error
		switch f {
		case "FlateDecode", "Fl":
			data, err = inflateAll(data)
			data = d.predict(data, p)
		case "LZWDecode", "LZW":
			early := true
			if p != nil {
				if v, ok := p["EarlyChange"].(int); ok {
					early = v != 0
				}
			}
			data = lzwDecode(data, early)
			data = d.predict(data, p)
		case "ASCIIHexDecode", "AHx":
			l := &pdfLex{b: append([]byte{'<'}, data...)}
			data = l.hex()
		case "ASCII85Decode", "A85":
			data = a85Decode(data)
		case "RunLengthDecode", "RL":
			data = rleDecode(data)
		case "Crypt":
		default:
			return data, fmt.Errorf("unsupported filter %s", f)
		}
		if err != nil && len(data) == 0 {
			return nil, err
		}
	}
	return data, nil
}

func inflateAll(b []byte) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(b)); err == nil {
		out, _ := io.ReadAll(zr)
		if len(out) > 0 {
			return out, nil
		}
	}
	for _, skip := range []int{0, 2} {
		if len(b) <= skip {
			break
		}
		out, _ := io.ReadAll(flate.NewReader(bytes.NewReader(b[skip:])))
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, errors.New("bad compressed data")
}

func (d *pdfDoc) predict(b []byte, p pdfDict) []byte {
	if p == nil {
		return b
	}
	pred := d.int(p["Predictor"])
	if pred < 2 {
		return b
	}
	colors := max(d.int(p["Colors"]), 1)
	bpc := d.int(p["BitsPerComponent"])
	if bpc == 0 {
		bpc = 8
	}
	cols := max(d.int(p["Columns"]), 1)
	bpp := max(colors*bpc/8, 1)
	row := (colors*bpc*cols + 7) / 8
	if pred == 2 {
		if bpc != 8 {
			return b
		}
		out := append([]byte(nil), b...)
		for r := 0; r+row <= len(out); r += row {
			for i := bpp; i < row; i++ {
				out[r+i] += out[r+i-bpp]
			}
		}
		return out
	}
	out := make([]byte, 0, len(b))
	prev := make([]byte, row)
	for i := 0; i < len(b); i += row + 1 {
		ft := b[i]
		cur := make([]byte, row)
		copy(cur, b[min(i+1, len(b)):min(i+1+row, len(b))])
		for k := 0; k < row; k++ {
			var a, c byte
			if k >= bpp {
				a, c = cur[k-bpp], prev[k-bpp]
			}
			up := prev[k]
			switch ft {
			case 1:
				cur[k] += a
			case 2:
				cur[k] += up
			case 3:
				cur[k] += byte((int(a) + int(up)) / 2)
			case 4:
				pa, pb, pc := absInt(int(up)-int(c)), absInt(int(a)-int(c)), absInt(int(a)+int(up)-2*int(c))
				switch {
				case pa <= pb && pa <= pc:
					cur[k] += a
				case pb <= pc:
					cur[k] += up
				default:
					cur[k] += c
				}
			}
		}
		out = append(out, cur...)
		prev = cur
	}
	return out
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func lzwDecode(b []byte, early bool) []byte {
	var out []byte
	table := make([][]byte, 0, 4096)
	reset := func() {
		table = table[:0]
		for i := 0; i < 256; i++ {
			table = append(table, []byte{byte(i)})
		}
		table = append(table, nil, nil)
	}
	reset()
	e := 0
	if early {
		e = 1
	}
	width, bits, pos := 9, 0, 0
	var buf uint32
	var prev []byte
	for {
		for bits < width && pos < len(b) {
			buf = buf<<8 | uint32(b[pos])
			pos++
			bits += 8
		}
		if bits < width {
			return out
		}
		code := int(buf>>(bits-width)) & (1<<width - 1)
		bits -= width
		buf &= 1<<bits - 1
		if code == 256 {
			reset()
			width, prev = 9, nil
			continue
		}
		if code == 257 {
			return out
		}
		var entry []byte
		switch {
		case code < len(table) && table[code] != nil:
			entry = table[code]
			if prev != nil {
				table = append(table, append(append([]byte(nil), prev...), entry[0]))
			}
		case code == len(table) && prev != nil:
			entry = append(append([]byte(nil), prev...), prev[0])
			table = append(table, entry)
		default:
			return out
		}
		out = append(out, entry...)
		prev = entry
		if len(table)+e >= 1<<width && width < 12 {
			width++
		}
	}
}

func a85Decode(b []byte) []byte {
	var clean []byte
	for _, c := range b {
		if !pdfWS(c) {
			clean = append(clean, c)
		}
	}
	clean = bytes.TrimPrefix(clean, []byte("<~"))
	if i := bytes.Index(clean, []byte("~>")); i >= 0 {
		clean = clean[:i]
	}
	out := make([]byte, 4*len(clean)+4)
	n, _, _ := ascii85.Decode(out, clean, true)
	return out[:n]
}

func rleDecode(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b); {
		n := int(b[i])
		i++
		switch {
		case n < 128:
			end := min(i+n+1, len(b))
			out = append(out, b[i:end]...)
			i = end
		case n > 128:
			if i < len(b) {
				out = append(out, bytes.Repeat(b[i:i+1], 257-n)...)
			}
			i++
		default:
			return out
		}
	}
	return out
}

// ---------- encryption (standard security handler, empty user password) ----------

type pdfCrypt struct {
	r                int
	key              []byte
	stmAES, strAES   bool
	stmNone, strNone bool
}

var pdfPad = []byte("\x28\xBF\x4E\x5E\x4E\x75\x8A\x41\x64\x00\x4E\x56\xFF\xFA\x01\x08\x2E\x2E\x00\xB6\xD0\x68\x3E\x80\x2F\x0C\xA9\xFE\x64\x53\x69\x7A")

func rc4x(key, data []byte) []byte {
	c, err := rc4.NewCipher(key)
	if err != nil {
		return nil
	}
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}

func (d *pdfDoc) setupCrypt(enc pdfDict) error {
	if f := d.name(enc["Filter"]); f != "Standard" {
		return fmt.Errorf("this PDF is encrypted with %q, which ContextSaver can't open", f)
	}
	v, r := d.int(enc["V"]), d.int(enc["R"])
	o, u := d.str(enc["O"]), d.str(enc["U"])
	p := uint32(int32(d.int(enc["P"])))
	var id0 []byte
	if ids := d.arr(d.trailer["ID"]); len(ids) > 0 {
		id0 = d.str(ids[0])
	}
	c := &pdfCrypt{r: r}
	method := func(n string) (aesOn, none bool) {
		if v < 4 {
			return false, false
		}
		if n == "" || n == "Identity" {
			return false, true
		}
		switch d.name(d.dict(d.dict(enc["CF"])[n])["CFM"]) {
		case "AESV2", "AESV3":
			return true, false
		case "None":
			return false, true
		}
		return false, false
	}
	c.stmAES, c.stmNone = method(d.name(enc["StmF"]))
	c.strAES, c.strNone = method(d.name(enc["StrF"]))
	if r >= 5 {
		if len(u) < 48 {
			return errPDFBad
		}
		var hash, ikey []byte
		if r == 5 {
			h1, h2 := sha256.Sum256(u[32:40]), sha256.Sum256(u[40:48])
			hash, ikey = h1[:], h2[:]
		} else {
			hash, ikey = pdfHash2B(nil, u[32:40], nil), pdfHash2B(nil, u[40:48], nil)
		}
		if !bytes.Equal(hash, u[:32]) {
			return errPDFPassword
		}
		ue := d.str(enc["UE"])
		if len(ue) < 32 {
			return errPDFBad
		}
		blk, err := aes.NewCipher(ikey)
		if err != nil {
			return err
		}
		c.key = make([]byte, 32)
		cipher.NewCBCDecrypter(blk, make([]byte, 16)).CryptBlocks(c.key, ue[:32])
		c.stmAES, c.strAES = !c.stmNone, !c.strNone
		d.crypt = c
		return nil
	}
	n := 5
	if r >= 3 {
		if l := d.int(enc["Length"]); l >= 40 && l <= 128 {
			n = l / 8
		}
	}
	if v >= 4 {
		n = 16
	}
	h := md5.New()
	h.Write(pdfPad)
	h.Write(o[:min(len(o), 32)])
	h.Write([]byte{byte(p), byte(p >> 8), byte(p >> 16), byte(p >> 24)})
	h.Write(id0)
	if r >= 4 {
		if em, ok := d.resolve(enc["EncryptMetadata"]).(bool); ok && !em {
			h.Write([]byte{0xff, 0xff, 0xff, 0xff})
		}
	}
	key := h.Sum(nil)
	if r >= 3 {
		for i := 0; i < 50; i++ {
			s := md5.Sum(key[:n])
			key = s[:]
		}
	}
	key = key[:n]
	ok := false
	if r <= 2 {
		ok = len(u) >= 32 && bytes.Equal(rc4x(key, pdfPad), u[:32])
	} else {
		s := md5.Sum(append(append([]byte(nil), pdfPad...), id0...))
		x := rc4x(key, s[:])
		k := make([]byte, len(key))
		for i := 1; i <= 19; i++ {
			for j := range key {
				k[j] = key[j] ^ byte(i)
			}
			x = rc4x(k, x)
		}
		ok = len(u) >= 16 && bytes.Equal(x, u[:16])
	}
	if !ok {
		return errPDFPassword
	}
	c.key = key
	d.crypt = c
	return nil
}

func pdfHash2B(pw, salt, udata []byte) []byte {
	h := sha256.New()
	h.Write(pw)
	h.Write(salt)
	h.Write(udata)
	k := h.Sum(nil)
	var e []byte
	for i := 0; i < 64 || int(e[len(e)-1]) > i-32; i++ {
		k1 := make([]byte, 0, 64*(len(pw)+len(k)+len(udata)))
		for j := 0; j < 64; j++ {
			k1 = append(append(append(k1, pw...), k...), udata...)
		}
		blk, _ := aes.NewCipher(k[:16])
		e = make([]byte, len(k1))
		cipher.NewCBCEncrypter(blk, k[16:32]).CryptBlocks(e, k1)
		sum := 0
		for _, b := range e[:16] {
			sum += int(b)
		}
		switch sum % 3 {
		case 0:
			s := sha256.Sum256(e)
			k = s[:]
		case 1:
			s := sha512.Sum384(e)
			k = s[:]
		default:
			s := sha512.Sum512(e)
			k = s[:]
		}
	}
	return k[:32]
}

func (c *pdfCrypt) decrypt(b []byte, n, g int, useAES bool) []byte {
	key := c.key
	if c.r < 5 {
		k := append(append([]byte(nil), c.key...), byte(n), byte(n>>8), byte(n>>16), byte(g), byte(g>>8))
		if useAES {
			k = append(k, 's', 'A', 'l', 'T')
		}
		s := md5.Sum(k)
		key = s[:min(len(c.key)+5, 16)]
	}
	if !useAES {
		return rc4x(key, b)
	}
	if len(b) < 32 {
		return nil
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	data := b[16 : 16+(len(b)-16)/16*16]
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(blk, b[:16]).CryptBlocks(out, data)
	if len(out) > 0 {
		if pad := int(out[len(out)-1]); pad >= 1 && pad <= 16 {
			out = out[:len(out)-pad]
		}
	}
	return out
}

func (d *pdfDoc) decryptObj(v any, n, g int) any {
	c := d.crypt
	switch x := v.(type) {
	case []byte:
		if c.strNone {
			return x
		}
		return c.decrypt(x, n, g, c.strAES)
	case pdfArr:
		for i := range x {
			x[i] = d.decryptObj(x[i], n, g)
		}
	case pdfDict:
		for k, e := range x {
			x[k] = d.decryptObj(e, n, g)
		}
	case *pdfStream:
		for k, e := range x.d {
			x.d[k] = d.decryptObj(e, n, g)
		}
		if t, _ := x.d["Type"].(pdfName); t == "XRef" || c.stmNone {
			return x
		}
		if f, _ := x.d["Filter"].(pdfArr); len(f) > 0 && f[0] == pdfName("Crypt") {
			return x
		}
		x.raw = c.decrypt(x.raw, n, g, c.stmAES)
	}
	return v
}
