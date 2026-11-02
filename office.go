package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// office: create, edit, read and convert Word, Excel, PowerPoint and PDF files.
// Word, PowerPoint and PDF are written from simple Markdown; Excel from rows.
// Edits keep a copy of the old file in ContextSaver's backups folder.

type officeArgs struct {
	Action       string             `json:"action"`
	Path         string             `json:"path"`
	To           string             `json:"to"`
	Content      string             `json:"content"`
	Sheets       []officeSheet      `json:"sheets"`
	Sheet        string             `json:"sheet"`
	Cells        map[string]any     `json:"cells"`
	AppendRows   [][]any            `json:"append_rows"`
	Replace      []officeReplace    `json:"replace"`
	InsertAfter  string             `json:"insert_after"`
	DeleteParas  StrList            `json:"delete_paragraphs"`
	DeleteSlides string             `json:"delete_slides"`
	From         StrList            `json:"from"`
	Pages        string             `json:"pages"`
	ApplyTo      string             `json:"apply_to"`
	Rotate       int                `json:"rotate"`
	Stamp        string             `json:"stamp"`
	StampAt      string             `json:"stamp_at"`
	StampSize    float64            `json:"stamp_size"`
	Split        bool               `json:"split"`
	Overwrite    bool               `json:"overwrite"`
	MaxChars     int                `json:"max_chars"`
	Landscape    bool               `json:"landscape"`
	ColWidths    map[string]float64 `json:"col_widths"`
}

type officeSheet struct {
	Name string  `json:"name"`
	Rows [][]any `json:"rows"`
}

type officeReplace struct {
	Find string `json:"find"`
	With string `json:"with"`
}

var officeKinds = map[string]string{
	".docx": "docx", ".docm": "docx", ".xlsx": "xlsx", ".xlsm": "xlsx", ".pptx": "pptx", ".pdf": "pdf",
}

// Older or other formats Office opens; read converts them to a cached modern copy.
var legacyOffice = map[string]string{
	".doc": ".docx", ".rtf": ".docx", ".dot": ".docx", ".wpd": ".docx",
	".xls": ".xlsx", ".xlsb": ".xlsx", ".xlt": ".xlsx",
	".ppt": ".pptx", ".pps": ".pptx", ".ppsx": ".pptx", ".pot": ".pptx",
}

func toolOffice(raw json.RawMessage) (string, error) {
	var a officeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	p, err := expandPath(a.Path)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(p))
	kind := officeKinds[ext]
	switch a.Action {
	case "read":
		if err := checkReadable(p); err != nil {
			return "", err
		}
		return officeRead(p, a)
	case "convert":
		return officeConvert(p, a)
	case "create":
		if kind == "" {
			return "", errors.New("path must end in .docx, .xlsx, .pptx or .pdf")
		}
		if err := checkAllowed(p); err != nil {
			return "", err
		}
		if exists(p) && !a.Overwrite {
			return "", fmt.Errorf("%s already exists; use edit, or overwrite:true", p)
		}
		var b []byte
		switch kind {
		case "docx":
			b, err = docxCreate(a)
		case "xlsx":
			b, err = xlsxCreate(a)
		case "pptx":
			b, err = pptxCreate(a)
		case "pdf":
			b, err = pdfCreate(a)
		}
		if err != nil {
			return "", err
		}
		note, err := officeSave(p, b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("created %s (%s)%s", p, hb(int64(len(b))), note), nil
	case "edit":
		if kind == "" {
			return "", errors.New("edit works on .docx, .xlsx, .pptx and .pdf (convert others first)")
		}
		if err := checkReadable(p); err != nil {
			return "", err
		}
		out := p
		if a.To != "" {
			if out, err = expandPath(a.To); err != nil {
				return "", err
			}
			if exists(out) && !a.Overwrite && !samePath(out, p) {
				return "", fmt.Errorf("%s already exists; overwrite:true to replace it", out)
			}
		}
		if err := checkAllowed(out); err != nil {
			return "", err
		}
		var b []byte
		var done []string
		switch kind {
		case "docx":
			b, done, err = docxEdit(p, a)
		case "xlsx":
			b, done, err = xlsxEdit(p, a)
		case "pptx":
			b, done, err = pptxEdit(p, a)
		case "pdf":
			if a.Split {
				return pdfSplit(p, a)
			}
			b, done, err = pdfEdit(p, a)
		}
		if err != nil {
			return "", err
		}
		if len(done) == 0 {
			return "", errors.New("nothing to change: give content, replace, cells, append_rows, pages, rotate, stamp, from…")
		}
		note, err := officeSave(out, b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("saved %s: %s%s", out, strings.Join(done, "; "), note), nil
	}
	return "", errors.New("action must be create, edit, read or convert")
}

// officeSave writes atomically; an existing file is copied to backups first.
func officeSave(p string, b []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	note := ""
	if exists(p) {
		dir := filepath.Join(appHome, "backups", time.Now().Format("20060102-150405"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		bk := filepath.Join(dir, filepath.Base(p))
		if err := copyFileTo(p, bk); err != nil {
			return "", fmt.Errorf("couldn't back up the old file: %v", err)
		}
		note = "; old copy: " + bk
	}
	tmp := p + ".ts-tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Rename(tmp, p); err == nil {
			return note, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.Remove(tmp)
	return "", fmt.Errorf("couldn't save (is it open in another program?): %v", cleanErr(err))
}

func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func officeRead(p string, a officeArgs) (string, error) {
	ext := strings.ToLower(filepath.Ext(p))
	shown := ""
	if _, ok := legacyOffice[ext]; ok {
		np, err := legacyCopy(p)
		if err != nil {
			return "", err
		}
		shown = filepath.Base(p)
		p, ext = np, strings.ToLower(filepath.Ext(np))
	}
	maxc := a.MaxChars
	if maxc <= 0 {
		maxc = 8000
	}
	var s string
	var err error
	switch officeKinds[ext] {
	case "docx":
		s, err = docxOutline(p)
	case "xlsx":
		s, err = xlsxText(p, a.Sheet, 1<<20)
	case "pptx":
		s, err = pptxOutline(p)
	case "pdf":
		s, err = pdfOutline(p, a.Pages)
	default:
		return "", errors.New("read works on Word, Excel, PowerPoint and PDF files")
	}
	if err != nil {
		return "", err
	}
	if shown != "" {
		s = strings.ReplaceAll(s, filepath.Base(p), shown)
	}
	out, _ := capText(s, maxc, filepath.Base(p))
	return out, nil
}

// ---------- convert (through the installed Office) ----------

type officeApp struct {
	app     string
	formats map[string]int
}

var officeApps = []officeApp{
	{"Word", map[string]int{".docx": 16, ".doc": 0, ".pdf": 17, ".rtf": 6, ".txt": 2, ".odt": 23, ".html": 8, ".htm": 8, ".docm": 13}},
	{"Excel", map[string]int{".xlsx": 51, ".xls": 56, ".csv": 6, ".pdf": -1, ".ods": 60, ".xlsm": 52, ".txt": 42, ".html": 44}},
	{"PowerPoint", map[string]int{".pptx": 24, ".ppt": 1, ".pdf": 32, ".odp": 35, ".pptm": 25, ".png": 18, ".jpg": 17}},
}

var officeSrcApp = map[string]string{
	".doc": "Word", ".docx": "Word", ".docm": "Word", ".dot": "Word", ".dotx": "Word", ".rtf": "Word", ".odt": "Word",
	".txt": "Word", ".htm": "Word", ".html": "Word", ".wpd": "Word", ".pdf": "Word",
	".xls": "Excel", ".xlsx": "Excel", ".xlsm": "Excel", ".xlsb": "Excel", ".xlt": "Excel", ".csv": "Excel", ".ods": "Excel",
	".ppt": "PowerPoint", ".pptx": "PowerPoint", ".pptm": "PowerPoint", ".pps": "PowerPoint", ".ppsx": "PowerPoint", ".pot": "PowerPoint", ".odp": "PowerPoint",
}

func officeConvert(p string, a officeArgs) (string, error) {
	if err := checkReadable(p); err != nil {
		return "", err
	}
	if !exists(p) {
		return "", errors.New("not found: " + p)
	}
	if a.To == "" {
		return "", errors.New("to: the new file, e.g. report.pdf (a folder or just an extension like .pdf keeps the name)")
	}
	to := a.To
	if strings.HasPrefix(to, ".") && !strings.ContainsAny(to, `\/`) {
		to = strings.TrimSuffix(p, filepath.Ext(p)) + strings.ToLower(to)
	}
	to, err := expandPath(to)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(to); err == nil && fi.IsDir() {
		return "", errors.New("to must be a file name with an extension, or just an extension like .pdf")
	}
	if err := checkAllowed(to); err != nil {
		return "", err
	}
	if exists(to) && !a.Overwrite {
		return "", fmt.Errorf("%s already exists; overwrite:true to replace it", to)
	}
	note := ""
	if err := runOfficeConvert(p, to); err != nil {
		se := strings.ToLower(filepath.Ext(p))
		if officeSrcApp[se] != "Excel" || strings.ToLower(filepath.Ext(to)) != ".pdf" {
			return "", err
		}
		// Excel's PDF export needs a printer installed; lay the sheets out ourselves instead.
		if err2 := sheetsToPDF(p, to); err2 != nil {
			return "", fmt.Errorf("%v; own layout also failed: %v", err, err2)
		}
		note = " (laid out by ContextSaver: Excel needs a printer installed to export PDF)"
	}
	fi, err := os.Stat(to)
	if err != nil {
		return "", errors.New("Office finished but no file was written")
	}
	return fmt.Sprintf("converted to %s (%s)%s", to, hb(fi.Size()), note), nil
}

func runOfficeConvert(src, dst string) error {
	if !isWin {
		return errors.New("convert needs Microsoft Office on Windows")
	}
	se, de := strings.ToLower(filepath.Ext(src)), strings.ToLower(filepath.Ext(dst))
	appName := officeSrcApp[se]
	if appName == "" {
		return fmt.Errorf("can't convert %s files", se)
	}
	var app officeApp
	for _, o := range officeApps {
		if o.app == appName {
			app = o
		}
	}
	code, ok := app.formats[de]
	if !ok {
		var list []string
		for k := range app.formats {
			list = append(list, k)
		}
		return fmt.Errorf("%s can't save %s; it can save %s", appName, de, strings.Join(sortedStrs(list), " "))
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	var ps string
	switch appName {
	case "Word":
		pre := ""
		if se == ".pdf" {
			// Word asks "will convert your PDF to an editable document" even when hidden;
			// a background watcher presses OK on that prompt (the first button).
			pre = `$wj=Start-Job{Add-Type -AssemblyName UIAutomationClient,UIAutomationTypes;$r=[Windows.Automation.AutomationElement]::RootElement;$bc=New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::ControlTypeProperty,[Windows.Automation.ControlType]::Button);for($i=0;$i -lt 200;$i++){foreach($id in (Get-Process PDFREFLOW -EA 0).Id){$c=New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::ProcessIdProperty,$id);foreach($w in $r.FindAll('Children',$c)){$b=$w.FindFirst('Descendants',$bc);if($b){try{$b.GetCurrentPattern([Windows.Automation.InvokePattern]::Pattern).Invoke()}catch{}}}};Start-Sleep -Milliseconds 500}};`
		}
		ps = fmt.Sprintf(`%s$a=New-Object -ComObject Word.Application;$a.Visible=$false;$a.DisplayAlerts=0;try{$d=$a.Documents.Open(%s,$false,$true,$false,'ts-none');$d.SaveAs2(%s,%d);$d.Close(0)}finally{$a.Quit();if($wj){Stop-Job $wj;Remove-Job $wj -Force}}`, pre, q(src), q(dst), code)
	case "Excel":
		save := fmt.Sprintf(`$w.SaveAs(%s,%d)`, q(dst), code)
		if code < 0 {
			save = fmt.Sprintf(`$w.ExportAsFixedFormat(0,%s)`, q(dst))
		}
		ps = fmt.Sprintf(`$a=New-Object -ComObject Excel.Application;$a.Visible=$false;$a.DisplayAlerts=$false;try{$w=$a.Workbooks.Open(%s,0,$true,[Type]::Missing,'ts-none');%s;$w.Close($false)}finally{$a.Quit()}`, q(src), save)
	case "PowerPoint":
		ps = fmt.Sprintf(`$a=New-Object -ComObject PowerPoint.Application;try{$d=$a.Presentations.Open(%s,-1,0,0);$d.SaveAs(%s,%d);$d.Close()}finally{$a.Quit()}`, q(src), q(dst), code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := quietCmd(psExe(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", ps)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	out, err := cmd.CombinedOutput()
	close(done)
	if err != nil || !exists(dst) {
		msg := strings.TrimSpace(string(out))
		if i := strings.Index(msg, "\n"); i > 0 {
			msg = msg[:i]
		}
		if ctx.Err() != nil {
			msg = "timed out after 2 minutes (a hidden Office dialog?)"
			exe := map[string]string{"Word": "WINWORD.EXE", "Excel": "EXCEL.EXE", "PowerPoint": "POWERPNT.EXE"}[appName]
			killProcs(nil, exe, "-Embedding")
		}
		return fmt.Errorf("%s couldn't convert it: %s", appName, clip(msg, 300))
	}
	return nil
}

// legacyCopy converts .doc/.xls/.ppt (and friends) to a cached modern copy.
func legacyCopy(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", errors.New(cleanErr(err) + ": " + p)
	}
	ext := strings.ToLower(filepath.Ext(p))
	sum := sha1.Sum([]byte(normCase(p) + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)))
	dst := filepath.Join(appHome, "cache", "office", hex.EncodeToString(sum[:10])+legacyOffice[ext])
	if exists(dst) {
		return dst, nil
	}
	if err := runOfficeConvert(p, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func sortedStrs(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// ---------- Markdown ----------

type mdSpan struct {
	text               string
	bold, italic, code bool
	link               string
}

type mdBlock struct {
	kind  string // h, p, ul, ol, table, img, break, code
	level int    // heading level, list depth
	spans []mdSpan
	rows  [][][]mdSpan
	path  string
	lines []string
}

var (
	mdHeadRe  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdUlRe    = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdOlRe    = regexp.MustCompile(`^(\s*)\d+[.)]\s+(.*)$`)
	mdImgRe   = regexp.MustCompile(`^!\[[^\]]*\]\(([^)]+)\)$`)
	mdRuleRe  = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
	mdSepRe   = regexp.MustCompile(`^\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?$`)
	mdInlineR = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__|\*([^*]+?)\*|\b_([^_]+?)_\b|` + "`([^`]+)`" + `|\[([^\]]+)\]\(([^)]+)\)`)
)

func mdInline(s string) []mdSpan {
	var out []mdSpan
	last := 0
	for _, m := range mdInlineR.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > last {
			out = append(out, mdSpan{text: s[last:m[0]]})
		}
		g := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return s[m[2*i]:m[2*i+1]]
		}
		switch {
		case m[2] >= 0 || m[4] >= 0:
			for _, sp := range mdInline(g(1) + g(2)) {
				sp.bold = true
				out = append(out, sp)
			}
		case m[6] >= 0 || m[8] >= 0:
			for _, sp := range mdInline(g(3) + g(4)) {
				sp.italic = true
				out = append(out, sp)
			}
		case m[10] >= 0:
			out = append(out, mdSpan{text: g(5), code: true})
		default:
			out = append(out, mdSpan{text: g(6), link: g(7)})
		}
		last = m[1]
	}
	if last < len(s) {
		out = append(out, mdSpan{text: s[last:]})
	}
	return out
}

func mdCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseMarkdown(src string) []mdBlock {
	var out []mdBlock
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var para []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, mdBlock{kind: "p", spans: mdInline(strings.Join(para, " "))})
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		raw := strings.TrimRight(lines[i], " \t")
		t := strings.TrimSpace(raw)
		switch {
		case t == "":
			flush()
		case strings.HasPrefix(t, "```"):
			flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, strings.TrimRight(lines[i], "\r"))
			}
			out = append(out, mdBlock{kind: "code", lines: code})
		case mdRuleRe.MatchString(t):
			flush()
			out = append(out, mdBlock{kind: "break"})
		case mdHeadRe.MatchString(t):
			flush()
			m := mdHeadRe.FindStringSubmatch(t)
			out = append(out, mdBlock{kind: "h", level: len(m[1]), spans: mdInline(m[2])})
		case mdImgRe.MatchString(t):
			flush()
			out = append(out, mdBlock{kind: "img", path: strings.TrimSpace(mdImgRe.FindStringSubmatch(t)[1])})
		case strings.HasPrefix(t, "|"):
			flush()
			var rows [][][]mdSpan
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				l := strings.TrimSpace(lines[i])
				if mdSepRe.MatchString(l) {
					continue
				}
				var row [][]mdSpan
				for _, c := range mdCells(l) {
					row = append(row, mdInline(c))
				}
				rows = append(rows, row)
			}
			i--
			out = append(out, mdBlock{kind: "table", rows: rows})
		case mdUlRe.MatchString(raw):
			flush()
			m := mdUlRe.FindStringSubmatch(raw)
			out = append(out, mdBlock{kind: "ul", level: len(strings.ReplaceAll(m[1], "\t", "  ")) / 2, spans: mdInline(m[2])})
		case mdOlRe.MatchString(raw):
			flush()
			m := mdOlRe.FindStringSubmatch(raw)
			out = append(out, mdBlock{kind: "ol", level: len(strings.ReplaceAll(m[1], "\t", "  ")) / 2, spans: mdInline(m[2])})
		default:
			para = append(para, t)
		}
	}
	flush()
	return out
}

func spansText(sp []mdSpan) string {
	var b strings.Builder
	for _, s := range sp {
		b.WriteString(s.text)
	}
	return b.String()
}

// officeImagePath resolves an image path, relative ones against the output's folder.
func officeImagePath(p, base string) (string, error) {
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") && base != "" {
		p = filepath.Join(filepath.Dir(base), p)
	}
	ep, err := expandPath(p)
	if err != nil {
		return "", err
	}
	if err := checkReadable(ep); err != nil {
		return "", err
	}
	return ep, nil
}

func xmlEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseRanges turns "1-3,5,8-" into page numbers within 1..n, in the order given.
func parseRanges(s string, n int) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		r := make([]int, n)
		for i := range r {
			r[i] = i + 1
		}
		return r, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi := part, part
		if i := strings.Index(part, "-"); i >= 0 {
			lo, hi = part[:i], part[i+1:]
		}
		a, b := 1, n
		var err error
		if strings.TrimSpace(lo) != "" {
			if a, err = strconv.Atoi(strings.TrimSpace(lo)); err != nil {
				return nil, fmt.Errorf("bad page range %q", part)
			}
		}
		if strings.TrimSpace(hi) != "" {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("bad page range %q", part)
			}
		}
		if a < 1 || b < 1 || a > n || b > n {
			return nil, fmt.Errorf("range %q is outside pages 1-%d", part, n)
		}
		if a <= b {
			for i := a; i <= b; i++ {
				out = append(out, i)
			}
		} else {
			for i := a; i >= b; i-- {
				out = append(out, i)
			}
		}
	}
	return out, nil
}

// sheetsToPDF lays every sheet out as a table on landscape pages.
func sheetsToPDF(src, dst string) error {
	p := src
	if _, ok := legacyOffice[strings.ToLower(filepath.Ext(src))]; ok {
		np, err := legacyCopy(src)
		if err != nil {
			return err
		}
		p = np
	}
	t, err := xlsxText(p, "", 20<<20)
	if err != nil {
		return err
	}
	var md strings.Builder
	first := true
	for _, line := range strings.Split(t, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			if !first {
				md.WriteString("\n---\n\n")
			}
			first = false
			name := strings.TrimPrefix(line, "## ")
			if i := strings.LastIndex(name, " ("); i > 0 {
				name = name[:i]
			}
			md.WriteString("## " + name + "\n\n")
		case strings.HasPrefix(line, "#\t"):
		case line == "":
		default:
			cells := strings.Split(line, "\t")[1:]
			for i, c := range cells {
				if j := strings.Index(c, " (="); j > 0 {
					c = c[:j]
				}
				cells[i] = strings.ReplaceAll(c, "|", "/")
			}
			md.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
	}
	b, err := pdfCreate(officeArgs{Content: md.String(), Path: dst, Landscape: true})
	if err != nil {
		return err
	}
	_, err = officeSave(dst, b)
	return err
}
