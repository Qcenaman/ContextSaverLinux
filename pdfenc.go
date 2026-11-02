package main

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

// Encodings and glyph names, for PDF fonts that come without a ToUnicode map.

var pdfWinAnsiHigh = [32]rune{ // 0x80-0x9F
	0x20AC, 0x2022, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x2022, 0x017D, 0x2022,
	0x2022, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x2022, 0x017E, 0x0178,
}

var pdfMacRomanHigh = [128]rune{ // 0x80-0xFF
	0xC4, 0xC5, 0xC7, 0xC9, 0xD1, 0xD6, 0xDC, 0xE1, 0xE0, 0xE2, 0xE4, 0xE3, 0xE5, 0xE7, 0xE9, 0xE8,
	0xEA, 0xEB, 0xED, 0xEC, 0xEE, 0xEF, 0xF1, 0xF3, 0xF2, 0xF4, 0xF6, 0xF5, 0xFA, 0xF9, 0xFB, 0xFC,
	0x2020, 0xB0, 0xA2, 0xA3, 0xA7, 0x2022, 0xB6, 0xDF, 0xAE, 0xA9, 0x2122, 0xB4, 0xA8, 0x2260, 0xC6, 0xD8,
	0x221E, 0xB1, 0x2264, 0x2265, 0xA5, 0xB5, 0x2202, 0x2211, 0x220F, 0x3C0, 0x222B, 0xAA, 0xBA, 0x3A9, 0xE6, 0xF8,
	0xBF, 0xA1, 0xAC, 0x221A, 0x192, 0x2248, 0x2206, 0xAB, 0xBB, 0x2026, 0x20, 0xC0, 0xC3, 0xD5, 0x152, 0x153,
	0x2013, 0x2014, 0x201C, 0x201D, 0x2018, 0x2019, 0xF7, 0x25CA, 0xFF, 0x178, 0x2044, 0xA4, 0x2039, 0x203A, 0xFB01, 0xFB02,
	0x2021, 0xB7, 0x201A, 0x201E, 0x2030, 0xC2, 0xCA, 0xC1, 0xCB, 0xC8, 0xCD, 0xCE, 0xCF, 0xCC, 0xD3, 0xD4,
	0, 0xD2, 0xDA, 0xDB, 0xD9, 0x131, 0x2C6, 0x2DC, 0xAF, 0x2D8, 0x2D9, 0x2DA, 0xB8, 0x2DD, 0x2DB, 0x2C7,
}

var pdfStdHigh = map[int]rune{
	0xA1: 0xA1, 0xA2: 0xA2, 0xA3: 0xA3, 0xA4: 0x2044, 0xA5: 0xA5, 0xA6: 0x192, 0xA7: 0xA7, 0xA8: 0xA4, 0xA9: 0x27,
	0xAA: 0x201C, 0xAB: 0xAB, 0xAC: 0x2039, 0xAD: 0x203A, 0xAE: 0xFB01, 0xAF: 0xFB02, 0xB1: 0x2013, 0xB2: 0x2020,
	0xB3: 0x2021, 0xB4: 0xB7, 0xB6: 0xB6, 0xB7: 0x2022, 0xB8: 0x201A, 0xB9: 0x201E, 0xBA: 0x201D, 0xBB: 0xBB,
	0xBC: 0x2026, 0xBD: 0x2030, 0xBF: 0xBF, 0xC1: 0x60, 0xC2: 0xB4, 0xC3: 0x2C6, 0xC4: 0x2DC, 0xC5: 0xAF,
	0xC6: 0x2D8, 0xC7: 0x2D9, 0xC8: 0xA8, 0xCA: 0x2DA, 0xCB: 0xB8, 0xCD: 0x2DD, 0xCE: 0x2DB, 0xCF: 0x2C7,
	0xD0: 0x2014, 0xE1: 0xC6, 0xE3: 0xAA, 0xE8: 0x141, 0xE9: 0xD8, 0xEA: 0x152, 0xEB: 0xBA, 0xF1: 0xE6,
	0xF5: 0x131, 0xF8: 0x142, 0xF9: 0xF8, 0xFA: 0x153, 0xFB: 0xDF,
}

// PDFDocEncoding differs from Latin-1 only in 0x18-0x1F and 0x80-0xA0.
var pdfDocHigh = [33]rune{
	0x2022, 0x2020, 0x2021, 0x2026, 0x2014, 0x2013, 0x192, 0x2044, 0x2039, 0x203A, 0x2212, 0x2030, 0x201E, 0x201C, 0x201D, 0x2018,
	0x2019, 0x201A, 0x2122, 0xFB01, 0xFB02, 0x141, 0x152, 0x160, 0x178, 0x17D, 0x131, 0x142, 0x153, 0x161, 0x17E, 0xFFFD, 0x20AC,
}

func pdfBaseEncoding(name string) [256]string {
	var t [256]string
	if name == "Symbol" || name == "ZapfDingbats" {
		t[0x20] = " "
		t[0xB7] = "•"
		return t
	}
	for c := 0x20; c < 0x7F; c++ {
		t[c] = string(rune(c))
	}
	switch name {
	case "WinAnsiEncoding":
		for i, r := range pdfWinAnsiHigh {
			t[0x80+i] = string(r)
		}
		for c := 0xA1; c <= 0xFF; c++ {
			t[c] = string(rune(c))
		}
		t[0xA0], t[0xAD], t[0x7F] = " ", "-", "•"
	case "MacRomanEncoding":
		for i, r := range pdfMacRomanHigh {
			if r != 0 {
				t[0x80+i] = string(r)
			}
		}
	default: // StandardEncoding
		t[0x27], t[0x60] = "’", "‘"
		for c, r := range pdfStdHigh {
			t[c] = string(r)
		}
	}
	return t
}

// pdfTextString decodes a PDF text string (bookmarks, form fields, document info).
func pdfTextString(b []byte) string {
	if len(b) >= 2 && (b[0] == 0xFE && b[1] == 0xFF || b[0] == 0xFF && b[1] == 0xFE) {
		be := b[0] == 0xFE
		u := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			if be {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		return string(utf16.Decode(u))
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return strings.ToValidUTF8(string(b[3:]), "")
	}
	r := make([]rune, 0, len(b))
	for _, c := range b {
		switch {
		case c >= 0x80 && c <= 0xA0:
			r = append(r, pdfDocHigh[c-0x80])
		case c < 0x20 && c != '\n' && c != '\r' && c != '\t':
		default:
			r = append(r, rune(c))
		}
	}
	return string(r)
}

const pdfGlyphList = `space 20 exclam 21 quotedbl 22 numbersign 23 dollar 24 percent 25 ampersand 26 quotesingle 27
quoteright 2019 parenleft 28 parenright 29 asterisk 2A plus 2B comma 2C hyphen 2D period 2E slash 2F zero 30 one 31
two 32 three 33 four 34 five 35 six 36 seven 37 eight 38 nine 39 colon 3A semicolon 3B less 3C equal 3D greater 3E
question 3F at 40 bracketleft 5B backslash 5C bracketright 5D asciicircum 5E underscore 5F grave 60 quoteleft 2018
braceleft 7B bar 7C braceright 7D asciitilde 7E exclamdown A1 cent A2 sterling A3 currency A4 yen A5 brokenbar A6
section A7 dieresis A8 copyright A9 ordfeminine AA guillemotleft AB logicalnot AC registered AE macron AF degree B0
plusminus B1 twosuperior B2 threesuperior B3 acute B4 mu B5 paragraph B6 periodcentered B7 middot B7 cedilla B8
onesuperior B9 ordmasculine BA guillemotright BB onequarter BC onehalf BD threequarters BE questiondown BF
Agrave C0 Aacute C1 Acircumflex C2 Atilde C3 Adieresis C4 Aring C5 AE C6 Ccedilla C7 Egrave C8 Eacute C9
Ecircumflex CA Edieresis CB Igrave CC Iacute CD Icircumflex CE Idieresis CF Eth D0 Ntilde D1 Ograve D2 Oacute D3
Ocircumflex D4 Otilde D5 Odieresis D6 multiply D7 Oslash D8 Ugrave D9 Uacute DA Ucircumflex DB Udieresis DC
Yacute DD Thorn DE germandbls DF agrave E0 aacute E1 acircumflex E2 atilde E3 adieresis E4 aring E5 ae E6
ccedilla E7 egrave E8 eacute E9 ecircumflex EA edieresis EB igrave EC iacute ED icircumflex EE idieresis EF eth F0
ntilde F1 ograve F2 oacute F3 ocircumflex F4 otilde F5 odieresis F6 divide F7 oslash F8 ugrave F9 uacute FA
ucircumflex FB udieresis FC yacute FD thorn FE ydieresis FF Amacron 100 amacron 101 Abreve 102 abreve 103
Aogonek 104 aogonek 105 Cacute 106 cacute 107 Ccaron 10C ccaron 10D Dcaron 10E dcaron 10F Dcroat 110 dcroat 111
Emacron 112 emacron 113 Edotaccent 116 edotaccent 117 Eogonek 118 eogonek 119 Ecaron 11A ecaron 11B Gbreve 11E
gbreve 11F Gcommaaccent 122 gcommaaccent 123 Imacron 12A imacron 12B Iogonek 12E iogonek 12F Idotaccent 130
dotlessi 131 Kcommaaccent 136 kcommaaccent 137 Lacute 139 lacute 13A Lcommaaccent 13B lcommaaccent 13C Lcaron 13D
lcaron 13E Lslash 141 lslash 142 Nacute 143 nacute 144 Ncommaaccent 145 ncommaaccent 146 Ncaron 147 ncaron 148
Omacron 14C omacron 14D Ohungarumlaut 150 ohungarumlaut 151 OE 152 oe 153 Racute 154 racute 155 Rcommaaccent 156
rcommaaccent 157 Rcaron 158 rcaron 159 Sacute 15A sacute 15B Scedilla 15E scedilla 15F Scaron 160 scaron 161
Tcommaaccent 162 tcommaaccent 163 Tcaron 164 tcaron 165 Umacron 16A umacron 16B Uring 16E uring 16F
Uhungarumlaut 170 uhungarumlaut 171 Uogonek 172 uogonek 173 Ydieresis 178 Zacute 179 zacute 17A Zdotaccent 17B
zdotaccent 17C Zcaron 17D zcaron 17E florin 192 Scommaaccent 218 scommaaccent 219 circumflex 2C6 caron 2C7
breve 2D8 dotaccent 2D9 ring 2DA ogonek 2DB tilde 2DC hungarumlaut 2DD Alpha 391 Beta 392 Gamma 393 Delta 394
Epsilon 395 Zeta 396 Eta 397 Theta 398 Iota 399 Kappa 39A Lambda 39B Mu 39C Nu 39D Xi 39E Omicron 39F Pi 3A0
Rho 3A1 Sigma 3A3 Tau 3A4 Upsilon 3A5 Phi 3A6 Chi 3A7 Psi 3A8 Omega 3A9 alpha 3B1 beta 3B2 gamma 3B3 delta 3B4
epsilon 3B5 zeta 3B6 eta 3B7 theta 3B8 iota 3B9 kappa 3BA lambda 3BB nu 3BD xi 3BE omicron 3BF pi 3C0 rho 3C1
sigma1 3C2 sigma 3C3 tau 3C4 upsilon 3C5 phi 3C6 chi 3C7 psi 3C8 omega 3C9 endash 2013 emdash 2014
quotesinglbase 201A quotedblleft 201C quotedblright 201D quotedblbase 201E dagger 2020 daggerdbl 2021 bullet 2022
ellipsis 2026 perthousand 2030 minute 2032 second 2033 guilsinglleft 2039 guilsinglright 203A fraction 2044
Euro 20AC trademark 2122 minus 2212 fi FB01 fl FB02 ff FB00 ffi FB03 ffl FB04 nbspace A0 sfthyphen AD
overscore AF infinity 221E lessequal 2264 greaterequal 2265 notequal 2260 approxequal 2248 partialdiff 2202
summation 2211 product 220F integral 222B radical 221A lozenge 25CA arrowleft 2190 arrowup 2191 arrowright 2192
arrowdown 2193 arrowboth 2194 element 2208 proportional 221D angle 2220 logicaland 2227 logicalor 2228
intersection 2229 union 222A therefore 2234 similar 223C congruent 2245 equivalence 2261 propersubset 2282
propersuperset 2283 reflexsubset 2286 reflexsuperset 2287 circleplus 2295 circlemultiply 2297 perpendicular 22A5
dotmath 22C5 universal 2200 existential 2203 emptyset 2205 gradient 2207 suchthat 220B asteriskmath 2217
aleph 2135 checkmark 2713 club 2663 diamond 2666 heart 2665 spade 2660`

var pdfGlyphs = map[string]string{}

func init() {
	f := strings.Fields(pdfGlyphList)
	for i := 0; i+1 < len(f); i += 2 {
		if v, err := strconv.ParseUint(f[i+1], 16, 32); err == nil {
			pdfGlyphs[f[i]] = string(rune(v))
		}
	}
}

func pdfGlyphText(name string) string {
	if t, ok := pdfGlyphs[name]; ok {
		return t
	}
	if len(name) == 1 && (name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return name
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		return pdfGlyphText(name[:i])
	}
	if strings.Contains(name, "_") {
		var b strings.Builder
		for _, p := range strings.Split(name, "_") {
			b.WriteString(pdfGlyphText(p))
		}
		return b.String()
	}
	if strings.HasPrefix(name, "uni") && len(name) >= 7 && (len(name)-3)%4 == 0 {
		var u []uint16
		for i := 3; i+4 <= len(name); i += 4 {
			v, err := strconv.ParseUint(name[i:i+4], 16, 16)
			if err != nil {
				return ""
			}
			u = append(u, uint16(v))
		}
		return string(utf16.Decode(u))
	}
	if len(name) >= 5 && len(name) <= 7 && name[0] == 'u' {
		if v, err := strconv.ParseUint(name[1:], 16, 32); err == nil && v <= 0x10FFFF {
			return string(rune(v))
		}
	}
	return ""
}
