package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- Excel ----------

func colName(c int) string { // 1 -> A
	s := ""
	for c > 0 {
		c--
		s = string(rune('A'+c%26)) + s
		c /= 26
	}
	return s
}

var cellRefRe = regexp.MustCompile(`^\$?([A-Za-z]{1,3})\$?(\d+)$`)

func parseRef(ref string) (col, row int, err error) {
	m := cellRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return 0, 0, fmt.Errorf("bad cell %q (use like B2 or Sheet1!B2)", ref)
	}
	for _, ch := range strings.ToUpper(m[1]) {
		col = col*26 + int(ch-'A'+1)
	}
	row, _ = strconv.Atoi(m[2])
	if row < 1 || row > 1048576 || col > 16384 {
		return 0, 0, fmt.Errorf("cell %q is outside the sheet", ref)
	}
	return col, row, nil
}

var (
	numLikeRe  = regexp.MustCompile(`^-?(0|[1-9]\d{0,14})(\.\d+)?$`)
	isoDateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	excelEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
)

// cellXML builds one cell; style is the s attribute to keep ("" = none).
// Styles in new workbooks: 1 bold, 2 date.
func cellXML(ref string, v any, style string, dateStyle string) string {
	s := ""
	if style != "" {
		s = ` s="` + style + `"`
	}
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		b := "0"
		if x {
			b = "1"
		}
		return `<c r="` + ref + `"` + s + ` t="b"><v>` + b + `</v></c>`
	case float64:
		return `<c r="` + ref + `"` + s + `><v>` + strconv.FormatFloat(x, 'f', -1, 64) + `</v></c>`
	case json.Number:
		return `<c r="` + ref + `"` + s + `><v>` + x.String() + `</v></c>`
	case string:
		switch {
		case strings.HasPrefix(x, "=") && len(x) > 1:
			return `<c r="` + ref + `"` + s + `><f>` + xmlEsc(x[1:]) + `</f></c>`
		case numLikeRe.MatchString(x):
			return `<c r="` + ref + `"` + s + `><v>` + x + `</v></c>`
		case isoDateRe.MatchString(x):
			if t, err := time.Parse("2006-01-02", x); err == nil {
				if style == "" && dateStyle != "" {
					s = ` s="` + dateStyle + `"`
				}
				return `<c r="` + ref + `"` + s + `><v>` + strconv.Itoa(int(t.Sub(excelEpoch).Hours()/24)) + `</v></c>`
			}
		}
		return `<c r="` + ref + `"` + s + ` t="inlineStr"><is><t xml:space="preserve">` + xmlEsc(x) + `</t></is></c>`
	default:
		return cellXML(ref, fmt.Sprint(x), style, dateStyle)
	}
}

func cellLen(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		if strings.HasPrefix(x, "=") {
			return 10
		}
		return len([]rune(x))
	default:
		return len(fmt.Sprint(x))
	}
}

func sheetXML(rows [][]any, widths map[string]float64) string {
	header := len(rows) > 1
	if len(rows) > 0 {
		for _, v := range rows[0] {
			if _, ok := v.(string); !ok && v != nil {
				header = false
			}
		}
	}
	maxLen := map[int]int{}
	var data strings.Builder
	for ri, r := range rows {
		fmt.Fprintf(&data, `<row r="%d">`, ri+1)
		for ci, v := range r {
			st := ""
			if ri == 0 && header {
				st = "1"
			}
			data.WriteString(cellXML(colName(ci+1)+strconv.Itoa(ri+1), v, st, "2"))
			maxLen[ci] = max(maxLen[ci], cellLen(v))
		}
		data.WriteString("</row>")
	}
	var b strings.Builder
	b.WriteString(xmlHdr + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	if header {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	}
	if len(maxLen) > 0 || len(widths) > 0 {
		b.WriteString("<cols>")
		ncol := 0
		for c := range maxLen {
			ncol = max(ncol, c+1)
		}
		for c := 0; c < ncol; c++ {
			w := math.Min(math.Max(float64(maxLen[c])*1.15+2, 8), 60)
			if x, ok := widths[colName(c+1)]; ok {
				w = x
			}
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, c+1, c+1, w)
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>" + data.String() + "</sheetData></worksheet>")
	return b.String()
}

const xlsxStyles = `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd"/></numFmts><fonts count="2"><font><sz val="11"/><name val="Calibri"/><family val="2"/></font><font><b/><sz val="11"/><name val="Calibri"/><family val="2"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/><xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`

func cleanSheetName(n string, i int) string {
	n = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(n))
	if n == "" {
		n = fmt.Sprintf("Sheet%d", i)
	}
	return string([]rune(n)[:min(31, len([]rune(n)))])
}

func xlsxCreate(a officeArgs) ([]byte, error) {
	sheets := a.Sheets
	if len(sheets) == 0 && strings.TrimSpace(a.Content) != "" {
		rows, err := csvRows(a.Content)
		if err != nil {
			return nil, err
		}
		sheets = []officeSheet{{Name: "Sheet1", Rows: rows}}
	}
	if len(sheets) == 0 {
		return nil, errors.New("give sheets: [{name, rows: [[...], ...]}] (or content as CSV)")
	}
	pkg := &ooxmlPkg{files: map[string][]byte{}}
	var ct, wbSheets, wbRels strings.Builder
	seen := map[string]bool{}
	for i, s := range sheets {
		name := cleanSheetName(s.Name, i+1)
		if seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("two sheets are named %q", name)
		}
		seen[strings.ToLower(name)] = true
		pkg.set(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), []byte(sheetXML(s.Rows, a.ColWidths)))
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1)
		fmt.Fprintf(&wbSheets, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEsc(name), i+1, i+10)
		fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i+10, i+1)
	}
	pkg.set("[Content_Types].xml", []byte(xmlHdr+`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`+ct.String()+`</Types>`))
	pkg.set("_rels/.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`))
	pkg.set("xl/workbook.xml", []byte(xmlHdr+`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><bookViews><workbookView/></bookViews><sheets>`+wbSheets.String()+`</sheets><calcPr calcId="191029" fullCalcOnLoad="1"/></workbook>`))
	pkg.set("xl/_rels/workbook.xml.rels", []byte(xmlHdr+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`+wbRels.String()+`</Relationships>`))
	pkg.set("xl/styles.xml", []byte(xmlHdr+xlsxStyles))
	return pkg.bytes()
}

func csvRows(s string) ([][]any, error) {
	r := csv.NewReader(strings.NewReader(s))
	r.FieldsPerRecord = -1
	if strings.Count(strings.SplitN(s, "\n", 2)[0], "\t") > 0 {
		r.Comma = '\t'
	}
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("content isn't valid CSV: %v", err)
	}
	var rows [][]any
	for _, rec := range recs {
		row := make([]any, len(rec))
		for i, v := range rec {
			row[i] = v
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ---------- workbook structure ----------

type xlSheet struct{ name, part string }

var (
	wbSheetRe = regexp.MustCompile(`<sheet\b[^>]*>`)
	relTagRe  = regexp.MustCompile(`<Relationship\b[^>]*>`)
)

func attrMap(tag string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRe.FindAllStringSubmatch(tag, -1) {
		k := a[1]
		if i := strings.Index(k, ":"); i >= 0 && k[:i] != "r" {
			k = k[i+1:]
		}
		m[k] = xmlUnesc(a[2])
	}
	return m
}

func relTargets(pkg *ooxmlPkg, relsPart, baseDir string) map[string]string {
	out := map[string]string{}
	for _, tag := range relTagRe.FindAllString(pkg.get(relsPart), -1) {
		m := attrMap(tag)
		t := m["Target"]
		if strings.HasPrefix(t, "/") {
			t = strings.TrimPrefix(t, "/")
		} else {
			t = path.Clean(path.Join(baseDir, t))
		}
		out[m["Id"]] = t
	}
	return out
}

func xlSheets(pkg *ooxmlPkg) []xlSheet {
	rels := relTargets(pkg, "xl/_rels/workbook.xml.rels", "xl")
	var out []xlSheet
	for _, tag := range wbSheetRe.FindAllString(pkg.get("xl/workbook.xml"), -1) {
		m := attrMap(tag)
		out = append(out, xlSheet{name: m["name"], part: rels[m["r:id"]]})
	}
	return out
}

func xlFindSheet(sheets []xlSheet, name string) (xlSheet, error) {
	if name == "" && len(sheets) > 0 {
		return sheets[0], nil
	}
	for _, s := range sheets {
		if strings.EqualFold(s.name, name) {
			return s, nil
		}
	}
	var names []string
	for _, s := range sheets {
		names = append(names, s.name)
	}
	return xlSheet{}, fmt.Errorf("no sheet %q (sheets: %s)", name, strings.Join(names, ", "))
}

// ---------- reading ----------

func xlSharedStrings(pkg *ooxmlPkg) []string {
	var out []string
	x := pkg.get("xl/sharedStrings.xml")
	for _, si := range regexp.MustCompile(`(?s)<si>.*?</si>|<si/>`).FindAllString(x, -1) {
		si = regexp.MustCompile(`(?s)<rPh\b.*?</rPh>`).ReplaceAllString(si, "")
		var b strings.Builder
		for _, m := range xTRe.FindAllStringSubmatch(si, -1) {
			b.WriteString(xmlUnesc(m[1]))
		}
		out = append(out, b.String())
	}
	return out
}

var xTRe = regexp.MustCompile(`(?s)<t(?:\s[^>]*)?>(.*?)</t>`)

// dateStyles: which cellXfs indexes show dates.
func xlDateStyles(pkg *ooxmlPkg) map[int]bool {
	st := pkg.get("xl/styles.xml")
	custom := map[int]bool{}
	for _, tag := range regexp.MustCompile(`<numFmt\b[^>]*>`).FindAllString(st, -1) {
		m := attrMap(tag)
		id, _ := strconv.Atoi(m["numFmtId"])
		code := strings.ToLower(regexp.MustCompile(`"[^"]*"|\[[^\]]*\]`).ReplaceAllString(m["formatCode"], ""))
		if strings.ContainsAny(code, "dy") || (strings.Contains(code, "m") && !strings.Contains(code, "0")) {
			custom[id] = true
		}
	}
	out := map[int]bool{}
	i := strings.Index(st, "<cellXfs")
	if i < 0 {
		return out
	}
	j := strings.Index(st[i:], "</cellXfs>")
	if j < 0 {
		return out
	}
	for n, tag := range regexp.MustCompile(`<xf\b[^>]*>`).FindAllString(st[i:i+j], -1) {
		id, _ := strconv.Atoi(attrMap(tag)["numFmtId"])
		if (id >= 14 && id <= 22) || (id >= 45 && id <= 47) || custom[id] {
			out[n] = true
		}
	}
	return out
}

var (
	xCellRe = regexp.MustCompile(`(?s)<c\b([^>]*?)(?:/>|>(.*?)</c>)`)
	xVRe    = regexp.MustCompile(`(?s)<v>(.*?)</v>`)
	xFRe    = regexp.MustCompile(`(?s)<f\b[^>]*?(?:/>|>(.*?)</f>)`)
)

func xlCellValue(attrs, inner string, ss []string, dates map[int]bool) (val, formula string) {
	m := attrMap(attrs)
	if f := xFRe.FindStringSubmatch(inner); f != nil {
		formula = xmlUnesc(f[1])
	}
	v := ""
	if x := xVRe.FindStringSubmatch(inner); x != nil {
		v = xmlUnesc(x[1])
	}
	switch m["t"] {
	case "s":
		if i, err := strconv.Atoi(v); err == nil && i < len(ss) {
			v = ss[i]
		}
	case "inlineStr":
		var b strings.Builder
		for _, t := range xTRe.FindAllStringSubmatch(inner, -1) {
			b.WriteString(xmlUnesc(t[1]))
		}
		v = b.String()
	case "b":
		if v == "1" {
			v = "TRUE"
		} else if v == "0" {
			v = "FALSE"
		}
	case "", "n":
		if s, err := strconv.Atoi(m["s"]); err == nil && dates[s] && v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				t := excelEpoch.Add(time.Duration(f * 24 * float64(time.Hour)))
				if t.Hour() == 0 && t.Minute() == 0 {
					v = t.Format("2006-01-02")
				} else {
					v = t.Format("2006-01-02 15:04")
				}
			}
		} else if f, err := strconv.ParseFloat(v, 64); err == nil && strings.ContainsAny(v, ".eE") {
			v = strconv.FormatFloat(f, 'g', 12, 64)
		}
	}
	return v, formula
}

// xlsxText: every sheet as tab-separated rows, with column letters and row numbers.
func xlsxText(p, only string, limit int) (string, error) {
	pkg, err := readPkg(p)
	if err != nil {
		return "", err
	}
	sheets := xlSheets(pkg)
	if only != "" {
		s, err := xlFindSheet(sheets, only)
		if err != nil {
			return "", err
		}
		sheets = []xlSheet{s}
	}
	ss := xlSharedStrings(pkg)
	dates := xlDateStyles(pkg)
	var out strings.Builder
	for _, sh := range sheets {
		x := pkg.get(sh.part)
		grid := map[int]map[int]string{}
		maxC, maxR := 0, 0
		for _, m := range xCellRe.FindAllStringSubmatch(x, -1) {
			am := attrMap(m[1])
			col, row, err := parseRef(am["r"])
			if err != nil {
				continue
			}
			v, f := xlCellValue(m[1], m[2], ss, dates)
			if f != "" {
				if v == "" {
					v = "=" + f
				} else {
					v = v + " (=" + f + ")"
				}
			}
			if v == "" {
				continue
			}
			if grid[row] == nil {
				grid[row] = map[int]string{}
			}
			grid[row][col] = strings.NewReplacer("\t", " ", "\n", " ", "\r", "").Replace(v)
			maxC, maxR = max(maxC, col), max(maxR, row)
		}
		if maxR == 0 {
			fmt.Fprintf(&out, "## %s (empty)\n\n", sh.name)
			continue
		}
		fmt.Fprintf(&out, "## %s (A1:%s%d)\n", sh.name, colName(maxC), maxR)
		out.WriteString("#")
		for c := 1; c <= maxC; c++ {
			out.WriteString("\t" + colName(c))
		}
		out.WriteString("\n")
		var rows []int
		for r := range grid {
			rows = append(rows, r)
		}
		sort.Ints(rows)
		for _, r := range rows {
			out.WriteString(strconv.Itoa(r))
			for c := 1; c <= maxC; c++ {
				out.WriteString("\t" + grid[r][c])
			}
			out.WriteString("\n")
			if out.Len() > limit {
				out.WriteString("…\n")
				return out.String(), nil
			}
		}
		out.WriteString("\n")
	}
	return out.String(), nil
}

// ---------- editing ----------

type xlRow struct {
	num   int
	open  string
	cells []xlCell
}

type xlCell struct {
	col int
	xml string
}

var (
	xRowRe   = regexp.MustCompile(`(?s)<row\b[^>]*?(?:/>|>.*?</row>)`)
	xSpansRe = regexp.MustCompile(`\s(spans|r)="[^"]*"`)
)

type xlSheetData struct {
	before, after string
	rows          []*xlRow
}

func parseSheetData(x string) (*xlSheetData, error) {
	i := strings.Index(x, "<sheetData")
	if i < 0 {
		return nil, errors.New("sheet has no data section")
	}
	j := strings.Index(x[i:], ">") + i + 1
	sd := &xlSheetData{}
	if strings.HasSuffix(x[i:j], "/>") {
		sd.before, sd.after = x[:i]+"<sheetData>", "</sheetData>"+x[j:]
		return sd, nil
	}
	k := strings.Index(x, "</sheetData>")
	sd.before, sd.after = x[:j], x[k:]
	next := 1
	for _, rm := range xRowRe.FindAllString(x[j:k], -1) {
		end := strings.Index(rm, ">") + 1
		open := rm[:end]
		r := &xlRow{num: next}
		if n, err := strconv.Atoi(attrMap(open)["r"]); err == nil {
			r.num = n
		}
		next = r.num + 1
		open = strings.TrimSuffix(xSpansRe.ReplaceAllString(open, ""), "/>")
		r.open = strings.TrimSuffix(open, ">")
		ncol := 1
		for _, cm := range xCellRe.FindAllStringSubmatch(rm, -1) {
			c := xlCell{col: ncol, xml: cm[0]}
			if col, _, err := parseRef(attrMap(cm[1])["r"]); err == nil {
				c.col = col
			}
			ncol = c.col + 1
			r.cells = append(r.cells, c)
		}
		sd.rows = append(sd.rows, r)
	}
	return sd, nil
}

func (sd *xlSheetData) row(n int) *xlRow {
	for i, r := range sd.rows {
		if r.num == n {
			return r
		}
		if r.num > n {
			nr := &xlRow{num: n, open: "<row"}
			sd.rows = append(sd.rows[:i], append([]*xlRow{nr}, sd.rows[i:]...)...)
			return nr
		}
	}
	nr := &xlRow{num: n, open: "<row"}
	sd.rows = append(sd.rows, nr)
	return nr
}

func (sd *xlSheetData) set(col, row int, v any) {
	r := sd.row(row)
	ref := colName(col) + strconv.Itoa(row)
	for i, c := range r.cells {
		if c.col == col {
			style := attrMap(xCellRe.FindStringSubmatch(c.xml)[1])["s"]
			r.cells[i].xml = cellXML(ref, v, style, "")
			return
		}
		if c.col > col {
			r.cells = append(r.cells[:i], append([]xlCell{{col, cellXML(ref, v, "", "")}}, r.cells[i:]...)...)
			return
		}
	}
	r.cells = append(r.cells, xlCell{col, cellXML(ref, v, "", "")})
}

func (sd *xlSheetData) lastRow() int {
	for i := len(sd.rows) - 1; i >= 0; i-- {
		for _, c := range sd.rows[i].cells {
			if c.xml != "" && strings.Contains(c.xml, "</c>") {
				return sd.rows[i].num
			}
		}
	}
	return 0
}

func (sd *xlSheetData) String() string {
	var b strings.Builder
	b.WriteString(sd.before)
	for _, r := range sd.rows {
		fmt.Fprintf(&b, `%s r="%d">`, r.open, r.num)
		for _, c := range r.cells {
			b.WriteString(c.xml)
		}
		b.WriteString("</row>")
	}
	b.WriteString(sd.after)
	return regexp.MustCompile(`<dimension\b[^>]*/>`).ReplaceAllString(b.String(), "")
}

func xlsxEdit(p string, a officeArgs) ([]byte, []string, error) {
	pkg, err := readPkg(p)
	if err != nil {
		return nil, nil, err
	}
	if pkg.get("xl/workbook.xml") == "" {
		return nil, nil, errors.New("not an Excel workbook")
	}
	sheets := xlSheets(pkg)
	var done []string
	edited := map[string]*xlSheetData{}
	load := func(name string) (*xlSheetData, xlSheet, error) {
		sh, err := xlFindSheet(sheets, name)
		if err != nil {
			return nil, sh, err
		}
		if sd, ok := edited[sh.part]; ok {
			return sd, sh, nil
		}
		sd, err := parseSheetData(pkg.get(sh.part))
		if err != nil {
			return nil, sh, err
		}
		edited[sh.part] = sd
		return sd, sh, nil
	}
	// New sheets first, so cells and rows can go into them.
	for _, s := range a.Sheets {
		name := cleanSheetName(s.Name, len(sheets)+1)
		if _, err := xlFindSheet(sheets, name); err == nil {
			sd, _, err := load(name)
			if err != nil {
				return nil, nil, err
			}
			for ri, r := range s.Rows {
				for ci, v := range r {
					sd.set(ci+1, ri+1, v)
				}
			}
			done = append(done, fmt.Sprintf("%d rows written to %s", len(s.Rows), name))
			continue
		}
		n := 1
		for pkg.files[fmt.Sprintf("xl/worksheets/sheet%d.xml", n)] != nil {
			n++
		}
		part := fmt.Sprintf("xl/worksheets/sheet%d.xml", n)
		pkg.set(part, []byte(sheetXML(s.Rows, a.ColWidths)))
		pkg.ensureOverride(part, "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml")
		rid := "rIdTs" + randHex(3)
		pkg.addRel("xl/_rels/workbook.xml.rels", rid, "http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet", fmt.Sprintf("worksheets/sheet%d.xml", n), false)
		wb := pkg.get("xl/workbook.xml")
		maxID := 0
		for _, tag := range wbSheetRe.FindAllString(wb, -1) {
			id, _ := strconv.Atoi(attrMap(tag)["sheetId"])
			maxID = max(maxID, id)
		}
		wb = strings.Replace(wb, "</sheets>", fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="%s"/></sheets>`, xmlEsc(name), maxID+1, rid), 1)
		pkg.set("xl/workbook.xml", []byte(wb))
		sheets = append(sheets, xlSheet{name: name, part: part})
		done = append(done, "sheet "+name+" added")
	}
	if len(a.Cells) > 0 {
		keys := make([]string, 0, len(a.Cells))
		for k := range a.Cells {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sheet, ref := a.Sheet, k
			if i := strings.LastIndex(k, "!"); i >= 0 {
				sheet, ref = strings.Trim(k[:i], "'"), k[i+1:]
			}
			col, row, err := parseRef(ref)
			if err != nil {
				return nil, nil, err
			}
			sd, _, err := load(sheet)
			if err != nil {
				return nil, nil, err
			}
			sd.set(col, row, a.Cells[k])
		}
		done = append(done, fmt.Sprintf("%d cells set", len(a.Cells)))
	}
	if len(a.AppendRows) > 0 {
		sd, sh, err := load(a.Sheet)
		if err != nil {
			return nil, nil, err
		}
		start := sd.lastRow() + 1
		for i, r := range a.AppendRows {
			for ci, v := range r {
				sd.set(ci+1, start+i, v)
			}
		}
		done = append(done, fmt.Sprintf("%d rows added to %s at row %d", len(a.AppendRows), sh.name, start))
	}
	for part, sd := range edited {
		pkg.set(part, []byte(sd.String()))
	}
	if len(a.Replace) > 0 {
		n := 0
		sst := pkg.get("xl/sharedStrings.xml")
		if sst != "" {
			sst = regexp.MustCompile(`(?s)<si>.*?</si>`).ReplaceAllStringFunc(sst, func(si string) string {
				return replaceInRuns(si, xTRe, `<t xml:space="preserve">`, `</t>`, a.Replace, &n)
			})
			pkg.set("xl/sharedStrings.xml", []byte(sst))
		}
		for _, sh := range sheets {
			x := pkg.get(sh.part)
			x2 := regexp.MustCompile(`(?s)<is>.*?</is>`).ReplaceAllStringFunc(x, func(is string) string {
				return replaceInRuns(is, xTRe, `<t xml:space="preserve">`, `</t>`, a.Replace, &n)
			})
			if x2 != x {
				pkg.set(sh.part, []byte(x2))
			}
		}
		done = append(done, fmt.Sprintf("%d replaced", n))
	}
	// Formulas may have changed: drop the stale calculation chain, recalculate on open.
	if pkg.files["xl/calcChain.xml"] != nil {
		pkg.del("xl/calcChain.xml")
		pkg.set("[Content_Types].xml", []byte(regexp.MustCompile(`<Override PartName="/xl/calcChain.xml"[^>]*/>`).ReplaceAllString(pkg.get("[Content_Types].xml"), "")))
		pkg.set("xl/_rels/workbook.xml.rels", []byte(regexp.MustCompile(`<Relationship\b[^>]*calcChain[^>]*/>`).ReplaceAllString(pkg.get("xl/_rels/workbook.xml.rels"), "")))
	}
	wb := pkg.get("xl/workbook.xml")
	if strings.Contains(wb, "<calcPr") {
		if !strings.Contains(wb, "fullCalcOnLoad") {
			wb = strings.Replace(wb, "<calcPr", `<calcPr fullCalcOnLoad="1"`, 1)
		}
	} else {
		wb = strings.Replace(wb, "</workbook>", `<calcPr fullCalcOnLoad="1"/></workbook>`, 1)
		if i, j := strings.Index(wb, "<calcPr"), strings.Index(wb, "</sheets>"); i > j && j > 0 {
			// calcPr must follow sheets/definedNames; move it right after them if other parts follow.
			wb = strings.Replace(wb, `<calcPr fullCalcOnLoad="1"/></workbook>`, "</workbook>", 1)
			at := j + len("</sheets>")
			if k := strings.Index(wb, "</definedNames>"); k > 0 {
				at = k + len("</definedNames>")
			}
			wb = wb[:at] + `<calcPr fullCalcOnLoad="1"/>` + wb[at:]
		}
	}
	pkg.set("xl/workbook.xml", []byte(wb))
	b, err := pkg.bytes()
	return b, done, err
}
