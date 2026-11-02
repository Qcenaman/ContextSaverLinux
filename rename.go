package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Rule struct {
	Replace    *string `json:"replace,omitempty"`
	With       string  `json:"with,omitempty"`
	Regex      bool    `json:"regex,omitempty"`
	IgnoreCase bool    `json:"ignore_case,omitempty"`
	Template   string  `json:"template,omitempty"`
	Case       string  `json:"case,omitempty"`
	Prefix     string  `json:"prefix,omitempty"`
	Suffix     string  `json:"suffix,omitempty"`
	Ext        *string `json:"ext,omitempty"`
	ExtCase    string  `json:"ext_case,omitempty"`
	Trim       bool    `json:"trim,omitempty"`
}

type tokCtx struct {
	path, base, ext string
	info            fs.FileInfo
	n               int
}

func tokFor(p string, info fs.FileInfo, n int) *tokCtx {
	name := info.Name()
	base, ext := splitExt(name, info.IsDir())
	return &tokCtx{path: p, base: base, ext: ext, info: info, n: n}
}

func splitExt(name string, dir bool) (string, string) {
	if dir {
		return name, ""
	}
	ext := filepath.Ext(name)
	if ext == name {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

var tokRe = regexp.MustCompile(`\{([a-zA-Z]+)(?::([^{}]*))?\}`)

func expandTokens(t string, c *tokCtx) string {
	return tokRe.ReplaceAllStringFunc(t, func(m string) string {
		sm := tokRe.FindStringSubmatch(m)
		arg := sm[2]
		switch strings.ToLower(sm[1]) {
		case "name":
			return c.base
		case "ext":
			return strings.TrimPrefix(c.ext, ".")
		case "file":
			return c.info.Name()
		case "parent":
			return filepath.Base(filepath.Dir(c.path))
		case "type":
			if c.info.IsDir() {
				return "Folders"
			}
			return category(c.ext)
		case "n":
			pad, _ := strconv.Atoi(arg)
			return fmt.Sprintf("%0*d", pad, c.n)
		case "date", "modified":
			return fmtDate(c.info.ModTime(), arg)
		case "created":
			return fmtDate(creationTime(c.info), arg)
		case "taken":
			return fmtDate(dateTaken(c.path, c.info), arg)
		}
		return m
	})
}

func fmtDate(t time.Time, f string) string {
	if f == "" {
		f = "YYYY-MM-DD"
	}
	var b strings.Builder
	for i := 0; i < len(f); {
		r := f[i:]
		switch {
		case strings.HasPrefix(r, "YYYY"):
			fmt.Fprintf(&b, "%04d", t.Year())
			i += 4
		case strings.HasPrefix(r, "YY"):
			fmt.Fprintf(&b, "%02d", t.Year()%100)
			i += 2
		case strings.HasPrefix(r, "MMMM"):
			b.WriteString(t.Month().String())
			i += 4
		case strings.HasPrefix(r, "MMM"):
			b.WriteString(t.Month().String()[:3])
			i += 3
		case strings.HasPrefix(r, "MM"):
			fmt.Fprintf(&b, "%02d", int(t.Month()))
			i += 2
		case strings.HasPrefix(r, "DD"):
			fmt.Fprintf(&b, "%02d", t.Day())
			i += 2
		case strings.HasPrefix(r, "hh"):
			fmt.Fprintf(&b, "%02d", t.Hour())
			i += 2
		case strings.HasPrefix(r, "mm"):
			fmt.Fprintf(&b, "%02d", t.Minute())
			i += 2
		case strings.HasPrefix(r, "ss"):
			fmt.Fprintf(&b, "%02d", t.Second())
			i += 2
		default:
			b.WriteByte(f[i])
			i++
		}
	}
	return b.String()
}

func applyCase(s, mode string) string {
	switch strings.ToLower(mode) {
	case "lower":
		return strings.ToLower(s)
	case "upper":
		return strings.ToUpper(s)
	case "title":
		rs := []rune(strings.ToLower(s))
		start := true
		for i, r := range rs {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				if start {
					rs[i] = unicode.ToUpper(r)
				}
				start = false
			} else {
				start = r == ' ' || r == '_' || r == '-' || r == '.' || r == '('
			}
		}
		return string(rs)
	case "sentence":
		rs := []rune(strings.ToLower(s))
		for i, r := range rs {
			if unicode.IsLetter(r) {
				rs[i] = unicode.ToUpper(r)
				break
			}
		}
		return string(rs)
	}
	return s
}

var spaceRe = regexp.MustCompile(`\s+`)
var reCache = map[string]*regexp.Regexp{}

func replaceFold(s, old, with string) string {
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(old))
	return re.ReplaceAllLiteralString(s, with)
}

func applyRules(rules []Rule, c *tokCtx) (string, string, error) {
	b, x := c.base, c.ext
	for _, ru := range rules {
		if ru.Replace != nil && *ru.Replace != "" {
			if ru.Regex {
				pat := *ru.Replace
				if ru.IgnoreCase {
					pat = "(?i)" + pat
				}
				re, ok := reCache[pat]
				if !ok {
					var err error
					if re, err = regexp.Compile(pat); err != nil {
						return "", "", fmt.Errorf("bad regex %q: %v", *ru.Replace, err)
					}
					reCache[pat] = re
				}
				b = re.ReplaceAllString(b, ru.With)
			} else if ru.IgnoreCase {
				b = replaceFold(b, *ru.Replace, ru.With)
			} else {
				b = strings.ReplaceAll(b, *ru.Replace, ru.With)
			}
		}
		if ru.Template != "" {
			b = expandTokens(ru.Template, c)
		}
		if ru.Case != "" {
			b = applyCase(b, ru.Case)
		}
		if ru.Prefix != "" {
			b = expandTokens(ru.Prefix, c) + b
		}
		if ru.Suffix != "" {
			b += expandTokens(ru.Suffix, c)
		}
		if ru.Ext != nil {
			if e := strings.TrimPrefix(strings.TrimSpace(*ru.Ext), "."); e == "" {
				x = ""
			} else {
				x = "." + e
			}
		}
		switch strings.ToLower(ru.ExtCase) {
		case "lower":
			x = strings.ToLower(x)
		case "upper":
			x = strings.ToUpper(x)
		}
		if ru.Trim {
			b = strings.TrimSpace(spaceRe.ReplaceAllString(b, " "))
		}
	}
	return b, x, nil
}

var reservedNames = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	out := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	stem := strings.ToLower(strings.SplitN(out, ".", 2)[0])
	if reservedNames[stem] || (len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) && stem[3] >= '1' && stem[3] <= '9') {
		out = "_" + out
	}
	return out
}

func natLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		ca, cb := a[0], b[0]
		if isDigit(ca) && isDigit(cb) {
			na, ra := leadingNum(a)
			nb, rb := leadingNum(b)
			if len(strings.TrimLeft(na, "0")) != len(strings.TrimLeft(nb, "0")) {
				return len(strings.TrimLeft(na, "0")) < len(strings.TrimLeft(nb, "0"))
			}
			if x, y := strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0"); x != y {
				return x < y
			}
			a, b = ra, rb
			continue
		}
		if ca != cb {
			return ca < cb
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func leadingNum(s string) (string, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

type rnItem struct {
	e           entry
	dir         string
	cur, target string
	skip        bool
}

func stepRename(j *Job, st *Step, res *StepResult) error {
	f, err := newFilter(st, false)
	if err != nil {
		return err
	}
	c, err := collect(j, st, f, collectOpts{contents: true, folders: st.Folders, skipBlocked: true})
	if err != nil {
		return err
	}
	res.collectNotes(c)
	items := make([]*rnItem, 0, len(c.files))
	for _, e := range c.files {
		items = append(items, &rnItem{e: e, dir: filepath.Dir(e.src), cur: e.src})
	}
	if len(items) == 0 {
		return nil
	}
	sortKey := strings.ToLower(st.Sort)
	less := func(a, b *rnItem) bool {
		switch sortKey {
		case "date", "modified":
			return a.e.info.ModTime().Before(b.e.info.ModTime())
		case "taken":
			return dateTaken(a.e.src, a.e.info).Before(dateTaken(b.e.src, b.e.info))
		case "size":
			return a.e.info.Size() < b.e.info.Size()
		}
		return natLess(a.e.info.Name(), b.e.info.Name())
	}
	global := strings.ToLower(st.Counter) == "global"
	sort.SliceStable(items, func(a, b int) bool {
		if !global && items[a].dir != items[b].dir {
			return items[a].dir < items[b].dir
		}
		return less(items[a], items[b])
	})
	start := 1
	if st.Start != nil {
		start = *st.Start
	}
	n, lastDir := start, ""
	for _, it := range items {
		if !global && it.dir != lastDir {
			n, lastDir = start, it.dir
		}
		ctx := tokFor(it.e.src, it.e.info, n)
		n++
		b, x, err := applyRules(st.Rules, ctx)
		if err != nil {
			return err
		}
		name := sanitizeName(b + x)
		if name == "" || name == x {
			res.err(it.e.src + ": the new name would be empty")
			it.skip = true
			continue
		}
		it.target = filepath.Join(it.dir, name)
	}
	moving := map[string]bool{}
	for _, it := range items {
		if !it.skip && it.target != it.e.src {
			moving[normCase(it.e.src)] = true
		}
	}
	onConf := strings.ToLower(st.OnConflict)
	if onConf == "" {
		onConf = "stop"
	}
	taken := map[string]int{}
	var clashes []string
	var todo []*rnItem
	unchanged := 0
	for _, it := range items {
		if it.skip {
			continue
		}
		if it.target == it.e.src {
			unchanged++
			continue
		}
		k := normCase(it.target)
		clash := taken[k] > 0 || (exists(it.target) && !moving[k])
		if clash {
			switch onConf {
			case "number":
				it.target = uniqueRename(it.target, taken, moving)
				k = normCase(it.target)
			case "skip":
				clashes = append(clashes, it.e.src+" → "+filepath.Base(it.target))
				it.skip = true
				continue
			default:
				clashes = append(clashes, it.e.src+" → "+filepath.Base(it.target))
			}
		}
		taken[k]++
		todo = append(todo, it)
	}
	if unchanged > 0 {
		res.add("already named right", unchanged)
	}
	if len(clashes) > 0 {
		label := "name clashes"
		if onConf == "skip" {
			label = "skipped (name clash)"
		}
		res.add(label, len(clashes))
		for i, s := range clashes {
			if i >= 6 {
				break
			}
			res.sample("clash: " + s)
		}
		if onConf == "stop" && !j.Spec.DryRun {
			return fmt.Errorf("%d new names clash with each other or with existing files, so nothing was renamed; use on_conflict number or skip, or change the rules", len(clashes))
		}
	}
	if j.Spec.DryRun {
		res.add("would rename", len(todo))
		for i, it := range todo {
			if i >= 8 {
				break
			}
			res.sample(it.e.info.Name() + " → " + filepath.Base(it.target))
		}
		return nil
	}
	// deepest first so renaming a folder never breaks the paths of things inside it
	sort.SliceStable(todo, func(a, b int) bool {
		return strings.Count(todo[a].e.src, string(filepath.Separator)) > strings.Count(todo[b].e.src, string(filepath.Separator))
	})
	j.addTotals(int64(len(todo)), 0)
	for _, it := range todo {
		if moving[normCase(it.target)] && normCase(it.target) != normCase(it.e.src) {
			tmp := filepath.Join(it.dir, ".csrn-"+randHex(3)+"-"+it.e.info.Name())
			if err := os.Rename(it.cur, tmp); err != nil {
				res.err(fmt.Sprintf("%s: %s", it.e.src, cleanErr(err)))
				it.skip = true
				continue
			}
			j.journal(jrec{Op: "rename", From: it.cur, To: tmp})
			it.cur = tmp
		}
	}
	for _, it := range todo {
		if j.ctx.Err() != nil {
			break
		}
		if it.skip {
			j.fileDone()
			continue
		}
		j.setCurrent(it.e.src)
		err := os.Rename(it.cur, it.target)
		j.fileDone()
		if err != nil {
			res.err(fmt.Sprintf("%s: %s", it.e.src, cleanErr(err)))
			if it.cur != it.e.src && os.Rename(it.cur, it.e.src) == nil {
				j.journal(jrec{Op: "rename", From: it.cur, To: it.e.src})
			}
			continue
		}
		j.journal(jrec{Op: "rename", From: it.cur, To: it.target})
		j.log("rename", it.e.src, it.target, "")
		res.add("renamed", 1)
	}
	return nil
}

func uniqueRename(p string, taken map[string]int, moving map[string]bool) string {
	dir, name := filepath.Split(p)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; i < 100000; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		k := normCase(c)
		if taken[k] == 0 && (!exists(c) || moving[k]) {
			return c
		}
	}
	return p
}

var errNoRules = errors.New("no rules")
