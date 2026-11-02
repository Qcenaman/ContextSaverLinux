package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type scanArgs struct {
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
	Include   string `json:"include"`
	Exclude   string `json:"exclude"`
}

type agg struct {
	n    int64
	size int64
}

func toolScan(raw json.RawMessage) (string, error) {
	var a scanArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	p, err := expandRead(a.Path)
	if err != nil {
		return "", err
	}
	if err := checkReadable(p); err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", errors.New(cleanErr(err) + ": " + p)
	}
	if !info.IsDir() {
		return fileInfoText(p, info), nil
	}
	rec := true
	if a.Recursive != nil {
		rec = *a.Recursive
	}
	f := &filter{inc: splitPatterns(a.Include), exc: splitPatterns(a.Exclude), recursive: rec}
	byExt, top := map[string]*agg{}, map[string]*agg{}
	var files, dirs, total, denied, directN int64
	var direct []string
	var largest []entry
	var oldest, newest time.Time
	deadline := time.Now().Add(25 * time.Second)
	partial := false
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err != nil {
			if q != p {
				denied++
			}
			if d != nil && d.IsDir() && q != p {
				return fs.SkipDir
			}
			return nil
		}
		if q == p {
			return nil
		}
		if time.Now().After(deadline) {
			partial = true
			return fs.SkipAll
		}
		name := d.Name()
		rel, _ := filepath.Rel(p, q)
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if isTrash(name) || !f.dirOK(name, rel) {
				return fs.SkipDir
			}
			dirs++
			if !rec {
				return fs.SkipDir
			}
			return nil
		}
		fi, e := d.Info()
		if e != nil || !f.fileOK(name, rel, fi) {
			return nil
		}
		files++
		total += fi.Size()
		ext := strings.ToLower(filepath.Ext(name))
		if ext == "" {
			ext = "(no ext)"
		}
		if byExt[ext] == nil {
			byExt[ext] = &agg{}
		}
		byExt[ext].n++
		byExt[ext].size += fi.Size()
		if parts := strings.SplitN(filepath.ToSlash(rel), "/", 2); len(parts) == 2 {
			if top[parts[0]] == nil {
				top[parts[0]] = &agg{}
			}
			top[parts[0]].n++
			top[parts[0]].size += fi.Size()
		} else {
			directN++
			if len(direct) < 8 {
				direct = append(direct, name)
			}
		}
		mt := fi.ModTime()
		if oldest.IsZero() || mt.Before(oldest) {
			oldest = mt
		}
		if mt.After(newest) {
			newest = mt
		}
		if len(largest) < 5 || fi.Size() > largest[len(largest)-1].info.Size() {
			largest = append(largest, entry{rel: rel, info: fi})
			sort.Slice(largest, func(x, y int) bool { return largest[x].info.Size() > largest[y].info.Size() })
			if len(largest) > 5 {
				largest = largest[:5]
			}
		}
		return nil
	})
	var b strings.Builder
	scope := "with subfolders"
	if !rec {
		scope = "top level only"
	}
	fmt.Fprintf(&b, "%s: %s files, %s, %s folders (%s)", p, hc(files), hb(total), hc(dirs), scope)
	if partial {
		b.WriteString(" — stopped after 25s, numbers are partial")
	}
	if files > 0 {
		b.WriteString("\nTypes: " + topAgg(byExt, 10, func(k string, a *agg) string {
			return fmt.Sprintf("%s %s (%s)", k, hc(a.n), hb(a.size))
		}))
		fmt.Fprintf(&b, "\nModified: %s → %s", oldest.Format("2006-01-02"), newest.Format("2006-01-02"))
		var ls []string
		for _, e := range largest {
			ls = append(ls, hb(e.info.Size())+" "+e.rel)
		}
		b.WriteString("\nLargest: " + strings.Join(ls, " · "))
	}
	if len(top) > 0 {
		b.WriteString("\nSubfolders: " + topAgg(top, 12, func(k string, a *agg) string {
			return fmt.Sprintf("%s (%s files, %s)", k, hc(a.n), hb(a.size))
		}))
	}
	if directN > 0 {
		fmt.Fprintf(&b, "\nDirectly inside: %s files (%s", hc(directN), strings.Join(direct, ", "))
		if directN > int64(len(direct)) {
			b.WriteString(", …")
		}
		b.WriteString(")")
	}
	if denied > 0 {
		fmt.Fprintf(&b, "\nCouldn't read %s items (no permission)", hc(denied))
	}
	return b.String(), nil
}

func topAgg(m map[string]*agg, n int, f func(string, *agg) string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return m[keys[a]].size > m[keys[b]].size })
	var parts []string
	for i, k := range keys {
		if i >= n {
			parts = append(parts, fmt.Sprintf("+%d more", len(keys)-n))
			break
		}
		parts = append(parts, f(k, m[k]))
	}
	return strings.Join(parts, " · ")
}

func fileInfoText(p string, info fs.FileInfo) string {
	ext := filepath.Ext(p)
	s := fmt.Sprintf("%s: %s, modified %s, created %s, type %s", p, hb(info.Size()),
		info.ModTime().Format("2006-01-02 15:04"), creationTime(info).Format("2006-01-02 15:04"), category(ext))
	if c := category(ext); c == "Images" || c == "Videos" {
		if t := dateTaken(p, info); !t.Equal(info.ModTime()) {
			s += ", taken " + t.Format("2006-01-02 15:04")
		}
	}
	return s
}

// ---------- find ----------

type findArgs struct {
	Path          StrList `json:"path"`
	Name          string  `json:"name"`
	Text          string  `json:"text"`
	Regex         bool    `json:"regex"`
	CaseSensitive bool    `json:"case_sensitive"`
	Include       string  `json:"include"`
	Exclude       string  `json:"exclude"`
	MinSize       string  `json:"min_size"`
	MaxSize       string  `json:"max_size"`
	NewerThan     string  `json:"newer_than"`
	OlderThan     string  `json:"older_than"`
	Recursive     *bool   `json:"recursive"`
	Limit         int     `json:"limit"`
	All           bool    `json:"all"`
	Lines         int     `json:"lines"`
}

func toolFind(raw json.RawMessage) (string, error) {
	var a findArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	if len(a.Path) == 0 {
		return "", errors.New("give path: one or more folders to search")
	}
	st := &Step{Include: a.Include, Exclude: a.Exclude, MinSize: a.MinSize, MaxSize: a.MaxSize, NewerThan: a.NewerThan, OlderThan: a.OlderThan, Recursive: a.Recursive}
	f, err := newFilter(st, true)
	if err != nil {
		return "", err
	}
	nameMatch := func(name, rel string) bool { return true }
	if a.Name != "" {
		if isPathGlob(a.Name) {
			re, err := globRe(a.Name)
			if err != nil {
				return "", fmt.Errorf("bad name glob: %v", err)
			}
			nameMatch = func(name, rel string) bool { return re.MatchString(filepath.ToSlash(rel)) }
		} else if strings.ContainsAny(a.Name, "*?[") {
			pats := splitPatterns(a.Name)
			nameMatch = func(n, _ string) bool { return matchPats(pats, n, n) }
		} else {
			want := strings.ToLower(a.Name)
			nameMatch = func(n, _ string) bool { return strings.Contains(strings.ToLower(n), want) }
		}
	}
	var lineMatch func(string) bool
	if a.Text != "" {
		if a.Regex {
			pat := a.Text
			if !a.CaseSensitive {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return "", fmt.Errorf("bad regex: %v", err)
			}
			lineMatch = re.MatchString
		} else if a.CaseSensitive {
			lineMatch = func(l string) bool { return strings.Contains(l, a.Text) }
		} else {
			want := strings.ToLower(a.Text)
			lineMatch = func(l string) bool { return strings.Contains(strings.ToLower(l), want) }
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	perFile := a.Lines
	if perFile <= 0 {
		perFile = 3
	}
	perFile = min(perFile, 50)
	deadline := time.Now().Add(40 * time.Second)
	var roots []string
	for _, raw := range a.Path {
		root, err := expandRead(raw)
		if err != nil {
			return "", err
		}
		if err := checkReadable(root); err != nil {
			return "", err
		}
		roots = append(roots, root)
	}
	ignored := map[string]bool{}
	if !a.All {
		for _, r := range roots {
			for k := range gitIgnored(r) {
				ignored[k] = true
			}
		}
	}
	var (
		mu                 sync.Mutex
		out                []string
		hitLimit, timedOut atomic.Bool
		scanned, skipped   atomic.Int64
	)
	full := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(out) >= limit
	}
	visit := func(root, dir string, ents []fs.DirEntry) []string {
		var sub []string
		for _, d := range ents {
			if full() {
				hitLimit.Store(true)
				return nil
			}
			if time.Now().After(deadline) {
				timedOut.Store(true)
				return nil
			}
			name := d.Name()
			q := filepath.Join(dir, name)
			rel, _ := filepath.Rel(root, q)
			if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				continue
			}
			if d.IsDir() {
				if !f.recursive || isTrash(name) || !f.dirOK(name, rel) || checkReadable(q) != nil {
					continue
				}
				if !a.All && (isHeavyDir(name, q) || isBuildOutDir(name, q) || ignored[pathKey(q)]) {
					skipped.Add(1)
					continue
				}
				sub = append(sub, q)
				continue
			}
			if !nameMatch(name, rel) || ignored[pathKey(q)] {
				continue
			}
			fi, e := d.Info()
			if e != nil || !f.fileOK(name, rel, fi) {
				continue
			}
			line := fmt.Sprintf("%s · %s · %s", q, hb(fi.Size()), fi.ModTime().Format("2006-01-02"))
			if lineMatch != nil {
				scanned.Add(1)
				hits, total := grepFile(q, fi, lineMatch, perFile)
				if total == 0 {
					continue
				}
				for _, h := range hits {
					line += "\n   " + h
				}
				if more := total - len(hits); more > 0 {
					line += fmt.Sprintf("\n   (+%d more matching lines; raise lines, or read mode grep for all)", more)
				}
			}
			mu.Lock()
			if len(out) < limit {
				out = append(out, line)
			} else {
				hitLimit.Store(true)
			}
			mu.Unlock()
		}
		return sub
	}
	parallelWalk(roots, visit, func() bool { return hitLimit.Load() || timedOut.Load() })
	sort.Strings(out)
	var b strings.Builder
	if len(out) == 0 {
		b.WriteString("No matches.")
	} else {
		fmt.Fprintf(&b, "%d match(es):\n%s", len(out), strings.Join(out, "\n"))
	}
	if hitLimit.Load() {
		fmt.Fprintf(&b, "\nStopped at the limit of %d; narrow the search or raise limit.", limit)
	}
	if timedOut.Load() {
		b.WriteString("\nStopped after 40s; results are partial. Narrow path, or add name/include.")
	}
	if n := skipped.Load(); n > 0 {
		fmt.Fprintf(&b, "\n(skipped %s cache/build/gitignored folders like node_modules, .git, dist, Temp; all:true searches them)", hc(n))
	}
	if lineMatch != nil {
		fmt.Fprintf(&b, "\n(searched inside %s files)", hc(scanned.Load()))
	}
	return b.String(), nil
}

func grepFile(p string, fi fs.FileInfo, match func(string) bool, maxHits int) ([]string, int) {
	var hits []string
	total := 0
	try := func(n int, l string) {
		if match(l) {
			total++
			if len(hits) < maxHits {
				hits = append(hits, fmt.Sprintf("%d: %s", n, clip(strings.TrimSpace(l), 160)))
			}
		}
	}
	if isDocExt(filepath.Ext(p)) || fi.Size() <= 20<<20 {
		text, err := extractText(p, 20<<20)
		if err != nil {
			return nil, 0
		}
		lines := strings.Split(text, "\n")
		if strings.EqualFold(filepath.Ext(p), ".pdf") {
			pages := pageOfLines(lines)
			for i, l := range lines {
				if !strings.HasPrefix(l, "[page ") && match(l) {
					total++
					if len(hits) < maxHits {
						hits = append(hits, fmt.Sprintf("%d (p%d): %s", i+1, pages[i], clip(strings.TrimSpace(l), 160)))
					}
				}
			}
			return hits, total
		}
		for i, l := range lines {
			try(i+1, l)
		}
		return hits, total
	}
	if fi.Size() > 200<<20 {
		return nil, 0
	}
	fh, err := os.Open(p)
	if err != nil {
		return nil, 0
	}
	defer fh.Close()
	head := make([]byte, 8000)
	n, _ := io.ReadFull(fh, head)
	if strings.IndexByte(string(head[:n]), 0) >= 0 {
		return nil, 0
	}
	fh.Seek(0, io.SeekStart)
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for i := 1; sc.Scan(); i++ {
		try(i, sc.Text())
	}
	return hits, total
}

// ---------- read ----------

type readArgs struct {
	Path     string   `json:"path"`
	Mode     string   `json:"mode"`
	Lines    int      `json:"lines"`
	FromLine int      `json:"from_line"`
	Pattern  string   `json:"pattern"`
	Regex    bool     `json:"regex"`
	Context  *int     `json:"context"`
	MaxChars int      `json:"max_chars"`
	Keys     []string `json:"keys"`
	Ext      string   `json:"ext"`
	Pages    string   `json:"pages"`
	MaxPx    int      `json:"max_px"`
	Changes  bool     `json:"changes"`
}

func toolRead(raw json.RawMessage) (string, []toolImage, error) {
	var a readArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", nil, err
	}
	if strings.ToLower(a.Mode) == "image" {
		return readImage(a)
	}
	s, err := readText(a)
	return s, nil, err
}

// readImage sends a small copy of a picture or a PDF page for Claude to look at.
func readImage(a readArgs) (string, []toolImage, error) {
	p, err := expandRead(a.Path)
	if err != nil {
		return "", nil, err
	}
	if err := checkReadable(p); err != nil {
		return "", nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", nil, errors.New(cleanErr(err) + ": " + p)
	}
	if fi.IsDir() {
		return "", nil, errors.New("that's a folder; use scan")
	}
	maxPx := a.MaxPx
	if maxPx <= 0 {
		maxPx = 800
	}
	maxPx = min(max(maxPx, 64), 1568)
	var pv *preview
	label := filepath.Base(p)
	switch ext := strings.ToLower(filepath.Ext(p)); {
	case ext == ".pdf":
		pg := 1
		if set, err := parsePages(a.Pages); err != nil {
			return "", nil, err
		} else if len(set) > 0 {
			pg = 1 << 30
			for n := range set {
				pg = min(pg, n)
			}
		}
		pv, err = pdfPagePreview(p, pg, maxPx)
		label = fmt.Sprintf("page %d of %s", pg, label)
	case isImageExt(ext):
		pv, err = imagePreview(p, maxPx)
	default:
		return "", nil, errors.New("image mode works on pictures and PDF pages")
	}
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%s: %d×%d, sent as %d×%d", label, pv.ow, pv.oh, pv.w, pv.h), []toolImage{{data: pv.jpg, mime: "image/jpeg"}}, nil
}

func readText(a readArgs) (string, error) {
	p, err := expandRead(a.Path)
	if err != nil {
		return "", err
	}
	if err := checkReadable(p); err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", errors.New(cleanErr(err) + ": " + p)
	}
	if strings.ToLower(a.Mode) == "values" {
		return readValues(p, fi, a.Keys, a.Ext)
	}
	if fi.IsDir() {
		return "", errors.New("that's a folder; use scan")
	}
	maxc := a.MaxChars
	if maxc <= 0 {
		maxc = 6000
	}
	maxc = min(maxc, 30000)
	n := a.Lines
	if n <= 0 {
		n = 40
	}
	mode := strings.ToLower(a.Mode)
	if mode == "" {
		mode = "head"
	}
	ext := strings.ToLower(filepath.Ext(p))
	pageSet, err := parsePages(a.Pages)
	if err != nil {
		return "", err
	}
	if mode == "info" && ext == ".pdf" {
		return pdfInfoText(p, fi)
	}
	if mode == "info" && isImageExt(ext) {
		return imageInfoText(p, fi), nil
	}
	if mode == "info" {
		s := fileInfoText(p, fi)
		if fi.Size() <= 50<<20 {
			if t, err := extractText(p, 50<<20); err == nil {
				s += fmt.Sprintf(", %s lines, %s characters of text", hc(int64(strings.Count(t, "\n")+1)), hc(int64(len([]rune(t)))))
			} else if errors.Is(err, errBinary) {
				s += ", binary file"
			}
		}
		return s, nil
	}
	var lines []string
	offset := 0
	if mode == "tail" && fi.Size() > 30<<20 && !isDocExt(ext) && !isImageExt(ext) {
		fh, err := os.Open(p)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		fh.Seek(-4<<20, io.SeekEnd)
		b, _ := io.ReadAll(fh)
		t, err := decodeText(b)
		if err != nil {
			return "", err
		}
		lines = strings.Split(t, "\n")[1:]
		offset = -1
	} else {
		o := textOpts{ocrWait: 40 * time.Second, pages: pageSet, legacy: true}
		if (mode == "head" || mode == "text") && pageSet == nil {
			o.ocrUpTo = 5
		}
		t, err := extractTextOpt(p, 30<<20, o)
		if err != nil {
			return "", err
		}
		lines = strings.Split(strings.ReplaceAll(t, "\r\n", "\n"), "\n")
	}
	if a.Changes {
		out, _ := capText(changesSince("file:"+p, strings.Join(lines, "\n")), maxc, filepath.Base(p)+" changes")
		return out, nil
	}
	var pageOf []int
	if ext == ".pdf" && mode == "grep" {
		pageOf = pageOfLines(lines)
	}
	total := len(lines)
	var b strings.Builder
	emit := func(i int) bool {
		num := "?"
		if offset == 0 {
			num = fmt.Sprint(i + 1)
		}
		if pageOf != nil {
			num += fmt.Sprintf(" (p%d)", pageOf[i])
		}
		s := num + ": " + clip(lines[i], 500) + "\n"
		if b.Len()+len(s) > maxc {
			b.WriteString("[output cut at max_chars]\n")
			return false
		}
		b.WriteString(s)
		return true
	}
	switch mode {
	case "head", "text":
		for i := 0; i < min(n, total); i++ {
			if !emit(i) {
				break
			}
		}
		if total > n {
			fmt.Fprintf(&b, "[%s more lines]", hc(int64(total-n)))
		}
	case "tail":
		for i := max(0, total-n); i < total; i++ {
			if !emit(i) {
				break
			}
		}
	case "lines":
		from := max(a.FromLine, 1) - 1
		for i := from; i < min(from+n, total); i++ {
			if !emit(i) {
				break
			}
		}
		fmt.Fprintf(&b, "[file has %s lines]", hc(int64(total)))
	case "grep":
		if a.Pattern == "" {
			return "", errors.New("grep needs pattern")
		}
		match := func(l string) bool { return strings.Contains(strings.ToLower(l), strings.ToLower(a.Pattern)) }
		if a.Regex {
			re, err := regexp.Compile("(?i)" + a.Pattern)
			if err != nil {
				return "", fmt.Errorf("bad regex: %v", err)
			}
			match = re.MatchString
		}
		ctx := 2
		if a.Context != nil {
			ctx = max(*a.Context, 0)
		}
		last, hits := -1, 0
		for i := 0; i < total; i++ {
			if !match(lines[i]) {
				continue
			}
			hits++
			from := max(i-ctx, last+1)
			if last >= 0 && from > last+1 {
				b.WriteString("--\n")
			}
			stop := false
			for k := from; k <= min(i+ctx, total-1); k++ {
				if !emit(k) {
					stop = true
					break
				}
				last = k
			}
			if stop {
				break
			}
		}
		if hits == 0 {
			b.WriteString("No matching lines.")
		} else {
			fmt.Fprintf(&b, "[%d matching lines]", hits)
		}
	default:
		return "", errors.New("mode must be info, head, tail, lines, grep, text or values")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

var heavyDirNames = map[string]bool{
	"node_modules": true, ".git": true, ".svn": true, ".hg": true, "__pycache__": true,
	".venv": true, "venv": true, "site-packages": true, ".cache": true, ".npm": true,
	".nuget": true, ".gradle": true, ".m2": true, ".cargo": true, ".rustup": true,
	".pytest_cache": true, ".mypy_cache": true, ".tox": true, "windows.old": true,
	"$windows.~bt": true, "$windows.~ws": true, "$sysreset": true, "msocache": true,
}

// isHeavyDir: folders full of small tool/cache files nobody searches for by hand.
func isHeavyDir(name, full string) bool {
	if heavyDirNames[strings.ToLower(name)] {
		return true
	}
	if !isWin {
		h, _ := os.UserHomeDir()
		for _, s := range []string{".cache", ".local/share/Trash", ".npm", ".gradle/caches", ".cargo/registry", "go/pkg/mod", "snap", ".var/app", ".local/share/flatpak", ".mozilla", ".config/google-chrome", ".config/chromium"} {
			if full == filepath.Join(h, s) {
				return true
			}
		}
	}
	if isWin {
		l := strings.ToLower(full)
		for _, s := range []string{`\appdata\local\temp`, `\appdata\local\packages`, `\appdata\local\microsoft`,
			`\appdata\local\google\chrome\user data`, `\appdata\roaming\microsoft`, `\appdata\local\pip\cache`,
			`\appdata\local\npm-cache`, `\appdata\local\nvidia\dxcache`, `\appdata\local\d3dscache`} {
			if strings.HasSuffix(l, s) {
				return true
			}
		}
	}
	return false
}

// parallelWalk reads directories on several goroutines; visit returns subfolders to descend into.
func parallelWalk(roots []string, visit func(root, dir string, ents []fs.DirEntry) []string, stop func() bool) {
	type item struct{ root, dir string }
	var (
		mu      sync.Mutex
		cond    = sync.NewCond(&mu)
		queue   []item
		pending int
	)
	for _, r := range roots {
		queue = append(queue, item{r, r})
		pending++
	}
	workers := min(max(runtime.NumCPU()*2, 8), 32)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				for len(queue) == 0 && pending > 0 {
					cond.Wait()
				}
				if pending == 0 {
					mu.Unlock()
					return
				}
				it := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				mu.Unlock()
				var sub []string
				if !stop() {
					if ents, err := os.ReadDir(it.dir); err == nil {
						sub = visit(it.root, it.dir, ents)
					}
				}
				mu.Lock()
				for _, s := range sub {
					queue = append(queue, item{it.root, s})
				}
				pending += len(sub) - 1
				cond.Broadcast()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}
