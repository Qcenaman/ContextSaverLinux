package main

import (
	"regexp"
	"strconv"
)

// Fallbacks that read the embedded font program itself, for fonts whose
// ToUnicode map is missing or points ligatures at private-use code points.

var t1EncRe = regexp.MustCompile(`dup\s+(\d+)\s*/([^\s/\[\]{}()<>]+)\s+put`)

// type1Encoding returns the built-in encoding of an embedded Type 1 font (nil = StandardEncoding).
func type1Encoding(prog []byte) map[int]string {
	clear := prog[:min(len(prog), 1<<16)]
	i := indexBytes(clear, "/Encoding")
	if i < 0 {
		return nil
	}
	rest := clear[i+9:]
	l := &pdfLex{b: rest}
	if t, _ := l.token(); t == pdfKw("StandardEncoding") {
		return nil
	}
	out := map[int]string{}
	for _, m := range t1EncRe.FindAllSubmatch(rest, 256) {
		if c, err := strconv.Atoi(string(m[1])); err == nil && c < 256 {
			out[c] = string(m[2])
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func indexBytes(b []byte, s string) int {
	n := len(s)
	for i := 0; i+n <= len(b); i++ {
		if string(b[i:i+n]) == s {
			return i
		}
	}
	return -1
}

// ttGlyphNames returns the custom glyph names in a TrueType font's post table, by glyph id.
func ttGlyphNames(b []byte) []string {
	if len(b) < 12 {
		return nil
	}
	n := beInt(b[4:6])
	for i := 0; i < n; i++ {
		rec := 12 + 16*i
		if rec+16 > len(b) {
			return nil
		}
		if string(b[rec:rec+4]) != "post" {
			continue
		}
		off, ln := beInt(b[rec+8:rec+12]), beInt(b[rec+12:rec+16])
		if off < 0 || off+ln > len(b) || ln < 34 {
			return nil
		}
		p := b[off : off+ln]
		if beInt(p[0:4]) != 0x00020000 {
			return nil
		}
		ng := beInt(p[32:34])
		if 34+2*ng > len(p) {
			return nil
		}
		var extra []string
		for q := 34 + 2*ng; q < len(p); {
			l := int(p[q])
			q++
			if q+l > len(p) {
				break
			}
			extra = append(extra, string(p[q:q+l]))
			q += l
		}
		names := make([]string, ng)
		for g := 0; g < ng; g++ {
			if k := beInt(p[34+2*g : 36+2*g]); k >= 258 && k-258 < len(extra) {
				names[g] = extra[k-258]
			}
		}
		return names
	}
	return nil
}

func isPUA(t string) bool {
	for _, r := range t {
		return r >= 0xE000 && r <= 0xF8FF || r >= 0xF0000
	}
	return false
}

// glyphName is the font program's own name for a CID, loaded on first use.
func (f *pdfFont) glyphName(cid int) string {
	if f.gload != nil {
		f.gnames, f.gload = f.gload(), nil
	}
	gid := cid
	if f.cidToGID != nil {
		if 2*cid+1 >= len(f.cidToGID) {
			return ""
		}
		gid = int(f.cidToGID[2*cid])<<8 | int(f.cidToGID[2*cid+1])
	}
	if gid >= 0 && gid < len(f.gnames) {
		return f.gnames[gid]
	}
	return ""
}
