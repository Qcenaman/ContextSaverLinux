package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Code edits for small models: a find must match exactly once (unless count or all is
// given); a miss names the closest lines; the result is only the changed lines with two
// lines of context, never the whole file. CRLF files stay CRLF even when the model
// sends LF.

func normNL(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// checkEditMatches refuses an ambiguous or missing plain find before anything is written.
func checkEditMatches(text string, edits []TextEdit, all bool) error {
	t := normNL(text)
	for _, e := range edits {
		if e.Regex || e.Find == "" {
			continue
		}
		f := normNL(e.Find)
		n := strings.Count(t, f)
		switch {
		case n == 0:
			return fmt.Errorf("find not found: %q. Closest lines:\n%s\nRead those lines and copy the text exactly", clip(e.Find, 80), nearestLines(t, f, 3))
		case n > 1 && e.Count == 0 && !all:
			return fmt.Errorf("find matches %d times (lines %s): add surrounding lines so it matches once, or pass count or all:true", n, matchLines(t, f, 8))
		}
	}
	return nil
}

func matchLines(t, f string, max int) string {
	var ls []string
	from := 0
	for len(ls) < max {
		i := strings.Index(t[from:], f)
		if i < 0 {
			break
		}
		ls = append(ls, fmt.Sprint(strings.Count(t[:from+i], "\n")+1))
		from += i + max1(len(f))
	}
	return strings.Join(ls, ", ")
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// nearestLines lists the file lines most like the find's first non-empty line.
func nearestLines(t, f string, n int) string {
	key := ""
	for _, l := range strings.Split(f, "\n") {
		if strings.TrimSpace(l) != "" {
			key = strings.TrimSpace(l)
			break
		}
	}
	type cand struct {
		no    int
		line  string
		score float64
	}
	var cs []cand
	for i, l := range strings.Split(t, "\n") {
		s := strings.TrimSpace(l)
		if s == "" {
			continue
		}
		if sc := similarity(key, s); sc > 0.3 {
			cs = append(cs, cand{i + 1, l, sc})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	if len(cs) > n {
		cs = cs[:n]
	}
	if len(cs) == 0 {
		return "(nothing similar)"
	}
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%d: %s", c.no, clip(strings.TrimRight(c.line, "\r"), 140)))
	}
	return strings.Join(out, "\n")
}

// similarity is 1 - edit distance / longer length (0..1), on at most 200 chars.
func similarity(a, b string) float64 {
	ra, rb := []rune(clip(a, 200)), []rune(clip(b, 200))
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			c := 1
			if ra[i-1] == rb[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(max(len(ra), len(rb)))
}

// editSummary shows the changed region of the new text with two lines of context.
func editSummary(name, before, after string, changes int) string {
	a, b := strings.Split(normNL(before), "\n"), strings.Split(normNL(after), "\n")
	top := 0
	for top < len(a) && top < len(b) && a[top] == b[top] {
		top++
	}
	bot := 0
	for bot < len(a)-top && bot < len(b)-top && a[len(a)-1-bot] == b[len(b)-1-bot] {
		bot++
	}
	lastNew := len(b) - bot // exclusive
	from, to := max(0, top-2), min(len(b), lastNew+2)
	var out []string
	if lastNew > top {
		out = append(out, fmt.Sprintf("edited %s: lines %d-%d (%d change(s)); * = changed", name, top+1, lastNew, changes))
	} else {
		out = append(out, fmt.Sprintf("edited %s: %d line(s) removed at line %d", name, len(a)-len(b), top+1))
	}
	shown := 0
	for i := from; i < to; i++ {
		if shown == 40 {
			out = append(out, fmt.Sprintf("… %d more lines (read lines %d-%d)", to-i, i+1, to))
			break
		}
		mark := ":"
		if i >= top && i < lastNew {
			mark = "*"
		}
		out = append(out, fmt.Sprintf("%d%s %s", i+1, mark, clip(b[i], 200)))
		shown++
	}
	return strings.Join(out, "\n")
}

// codeEdit runs a one-file pc edit: checks matches first, then edits through a job (so
// undo keeps working) and returns the short summary. handled=false leaves the call to
// the generic job (several files, folders, globs).
func codeEdit(p string, st map[string]any, a map[string]any) (string, bool, error) {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || strings.ContainsAny(p, "*?") {
		return "", false, nil
	}
	_, text, _, err := readEditable(p)
	if err != nil {
		return "", true, err
	}
	var edits []TextEdit
	if e, ok := st["edits"]; ok {
		b, _ := json.Marshal(e)
		if err := json.Unmarshal(b, &edits); err != nil {
			return "", true, errors.New("edits must be [{find,replace}]")
		}
	}
	all, _ := a["all"].(bool)
	if err := checkEditMatches(text, edits, all); err != nil {
		return "", true, err
	}
	crlf := strings.Contains(text, "\r\n")
	nt, n := text, 0
	if len(edits) > 0 {
		if nt, n, err = applyEdits(text, edits, crlf); err != nil {
			return "", true, err
		}
	}
	if ap, ok := st["append"].(string); ok && ap != "" {
		if crlf {
			ap = toCRLF(ap)
		}
		if nt != "" && !strings.HasSuffix(nt, "\n") {
			if crlf {
				nt += "\r\n"
			} else {
				nt += "\n"
			}
		}
		nt += ap
		n++
	}
	if nt == text {
		return "no change: the replacement equals the current text", true, nil
	}
	out, _, err := compactJob([]map[string]any{st}, a)
	if err != nil {
		return "", true, err
	}
	if _, now, _, err := readEditable(p); err != nil || now != nt {
		return out, true, nil // the job reported something else; pass it on
	}
	return editSummary(filepath.Base(p), text, nt, n), true, nil
}
