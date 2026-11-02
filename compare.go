package main

// git compare: two folders or GitHub repos (owner/name[@ref]) side by side, worked out
// here so only the short answer reaches the model: files only on one side, changed
// files by changed lines, and how many are the same. CRLF vs LF, a UTF-8 BOM and a
// missing last newline don't count as changes.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// compareSkip: folders never compared (git data, dependencies, build output).
var compareSkip = map[string]bool{".git": true, "vendor": true, "node_modules": true, "dist": true, "build": true,
	"out": true, "bin": true, "obj": true, "target": true, ".gradle": true, ".dart_tool": true, "__pycache__": true,
	".venv": true, "venv": true, ".next": true, ".idea": true, "Pods": true, "coverage": true}

const (
	compareMaxFiles = 50000
	compareMaxBytes = 8 << 20 // larger files are compared by hash only
)

// platformSuffixes pair a port's files: x_windows.go <-> x_linux.go or x.go.
var platformSuffixes = []string{"_windows", "_linux", "_darwin", "_unix", "_posix", "_win", "_mac"}

type changedFile struct {
	A, B     string // paths relative to each side (differ for paired platform files)
	PA, PB   string // the files on the PC
	Add, Del int
	Binary   bool
}

func (c changedFile) name() string {
	if c.A == c.B {
		return c.A
	}
	return c.A + " <-> " + c.B
}

type compareResult struct {
	FilesA, FilesB int
	Same           int
	OnlyA, OnlyB   []string
	Changed        []changedFile // most changed lines first
}

func gitCompare(a gitArgs, maxChars int) (string, error) {
	if strings.TrimSpace(a.Path) == "" || strings.TrimSpace(a.To) == "" {
		return "", errors.New("compare needs path and to: folders or owner/name[@ref]")
	}
	pa, err := expandRead(a.Path)
	if err != nil {
		return "", err
	}
	pb, err := expandRead(a.To)
	if err != nil {
		return "", err
	}
	for _, p := range []string{pa, pb} {
		if err := checkReadable(p); err != nil {
			return "", err
		}
	}
	sa, err := os.Stat(pa)
	if err != nil {
		return "", fmt.Errorf("path not found: %s", pa)
	}
	sb, err := os.Stat(pb)
	if err != nil {
		return "", fmt.Errorf("to not found: %s", pb)
	}
	n := a.N
	if n <= 0 {
		n = 25
	}
	var r compareResult
	switch {
	case sa.IsDir() && sb.IsDir():
		keep := compareKeep(a.Paths, a.Pair, shortRepoPaths(pa), shortRepoPaths(pb), filepath.ToSlash(pa), filepath.ToSlash(pb))
		if r, err = compareTrees(pa, pb, a.Pair, keep); err != nil {
			return "", err
		}
	case !sa.IsDir() && !sb.IsDir():
		r = compareResult{FilesA: 1, FilesB: 1}
		c, same := compareFiles(pa, pb)
		if same {
			r.Same = 1
		} else {
			c.A, c.B, c.PA, c.PB = filepath.Base(pa), filepath.Base(pb), pa, pb
			r.Changed = []changedFile{c}
		}
	default:
		return "", errors.New("compare two folders or two files")
	}
	return renderCompare(r, pa, pb, n, a.Full, maxChars), nil
}

// compareTree lists the files under root as slash paths relative to it.
func compareTree(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if p != root && (d.Name() == ".git" || d.IsDir() && compareSkip[d.Name()]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil // a worktree's .git file
		}
		if d.IsDir() || !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = p
		if len(files) > compareMaxFiles {
			return fmt.Errorf("more than %d files under %s: compare a subfolder", compareMaxFiles, root)
		}
		return nil
	})
	return files, err
}

// compareTrees compares the files keep accepts; pair also matches platform files.
func compareTrees(ra, rb string, pair bool, keep func(string) bool) (compareResult, error) {
	var r compareResult
	fa, err := compareTree(ra)
	if err != nil {
		return r, err
	}
	fb, err := compareTree(rb)
	if err != nil {
		return r, err
	}
	add := func(x, y string) {
		c, same := compareFiles(fa[x], fb[y])
		if same {
			r.Same++
			return
		}
		c.A, c.B, c.PA, c.PB = x, y, fa[x], fb[y]
		r.Changed = append(r.Changed, c)
	}
	for rel := range fa {
		if !keep(rel) {
			continue
		}
		r.FilesA++
		if _, ok := fb[rel]; ok {
			add(rel, rel)
		} else {
			r.OnlyA = append(r.OnlyA, rel)
		}
	}
	for rel := range fb {
		if !keep(rel) {
			continue
		}
		r.FilesB++
		if _, ok := fa[rel]; !ok {
			r.OnlyB = append(r.OnlyB, rel)
		}
	}
	sort.Strings(r.OnlyA)
	sort.Strings(r.OnlyB)
	if pair {
		var pairs [][2]string
		pairs, r.OnlyA, r.OnlyB = pairPlatform(r.OnlyA, r.OnlyB)
		for _, p := range pairs {
			add(p[0], p[1])
		}
	}
	sort.Slice(r.Changed, func(i, j int) bool {
		ci, cj := r.Changed[i], r.Changed[j]
		if ci.Add+ci.Del != cj.Add+cj.Del {
			return ci.Add+ci.Del > cj.Add+cj.Del
		}
		return ci.name() < cj.name()
	})
	return r, nil
}

// platformStem is rel without its platform suffix (x_windows.go -> x.go).
func platformStem(rel string) (string, bool) {
	dir, file := path.Split(rel)
	ext := path.Ext(file)
	base := strings.TrimSuffix(file, ext)
	for _, s := range platformSuffixes {
		if len(base) > len(s) && strings.HasSuffix(base, s) {
			return dir + strings.TrimSuffix(base, s) + ext, true
		}
	}
	return rel, false
}

// pairPlatform matches files left over on each side by platform stem: first two
// platform files (x_windows.go <-> x_linux.go), then a platform file and a plain one.
func pairPlatform(onlyA, onlyB []string) (pairs [][2]string, restA, restB []string) {
	byStem := map[string][]string{}
	for _, y := range onlyB {
		s, _ := platformStem(y)
		byStem[s] = append(byStem[s], y)
	}
	usedA, usedB := map[string]bool{}, map[string]bool{}
	for pass := 0; pass < 2; pass++ {
		for _, x := range onlyA {
			if usedA[x] {
				continue
			}
			s, sx := platformStem(x)
			for _, y := range byStem[s] {
				_, sy := platformStem(y)
				if usedB[y] || pass == 0 && !(sx && sy) || !sx && !sy {
					continue
				}
				pairs = append(pairs, [2]string{x, y})
				usedA[x], usedB[y] = true, true
				break
			}
		}
	}
	for _, x := range onlyA {
		if !usedA[x] {
			restA = append(restA, x)
		}
	}
	for _, y := range onlyB {
		if !usedB[y] {
			restB = append(restB, y)
		}
	}
	return pairs, restA, restB
}

// compareKeep limits a compare to paths[] (files or folders, relative to either side;
// a leading owner/name or folder of either side is dropped). Empty: everything.
func compareKeep(paths []string, pair bool, prefixes ...string) func(string) bool {
	var want []string
	for _, p := range paths {
		p = strings.Trim(filepath.ToSlash(strings.TrimSpace(p)), "/")
		for _, pre := range prefixes {
			if pre = strings.Trim(pre, "/"); pre != "" && strings.HasPrefix(p, pre+"/") {
				p = strings.TrimPrefix(p, pre+"/")
				break
			}
		}
		if p != "" && p != "." {
			want = append(want, p)
		}
	}
	if len(want) == 0 {
		return func(string) bool { return true }
	}
	in := func(rel, w string) bool { return rel == w || strings.HasPrefix(rel, w+"/") }
	return func(rel string) bool {
		for _, w := range want {
			if in(rel, w) {
				return true
			}
			if pair {
				rs, _ := platformStem(rel)
				ws, _ := platformStem(w)
				if rs == ws {
					return true
				}
			}
		}
		return false
	}
}

// compareContent reads a file for comparing; a file over compareMaxBytes becomes its hash.
func compareContent(p string) (data []byte, big bool, err error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, false, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(p)
		return []byte("symlink -> " + t), false, err
	}
	if fi.Size() > compareMaxBytes {
		f, err := os.Open(p)
		if err != nil {
			return nil, true, err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return nil, true, err
		}
		return h.Sum(nil), true, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\n")), false, nil
}

func isBinaryData(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\n")
}

// compareFiles reports whether two files are the same, or how many lines changed.
func compareFiles(pa, pb string) (changedFile, bool) {
	var c changedFile
	a, bigA, errA := compareContent(pa)
	b, bigB, errB := compareContent(pb)
	if errA == nil && errB == nil && bigA == bigB && bytes.Equal(a, b) {
		return c, true
	}
	if errA != nil || errB != nil || bigA || bigB || isBinaryData(a) || isBinaryData(b) {
		c.Binary = true
		return c, false
	}
	for _, o := range diffLines(splitLines(a), splitLines(b)) {
		if o.kind == '+' {
			c.Add++
		} else {
			c.Del++
		}
	}
	return c, false
}

type diffOp struct {
	kind byte // '-' only in A (line la), '+' only in B (line lb)
	line int
	text string
}

// diffLines returns the lines removed from a and added in b: an exact
// longest-common-subsequence diff after trimming the common start and end, or for a
// very large changed middle, lines matched as a multiset (positions still exact).
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(am), len(bm)
	var ops []diffOp
	if n*m <= 4_000_000 {
		w := m + 1
		t := make([]int32, (n+1)*w) // t[i*w+j] = LCS of am[i:] and bm[j:]
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				switch x, y := t[(i+1)*w+j], t[i*w+j+1]; {
				case am[i] == bm[j]:
					t[i*w+j] = t[(i+1)*w+j+1] + 1
				case x >= y:
					t[i*w+j] = x
				default:
					t[i*w+j] = y
				}
			}
		}
		for i, j := 0, 0; i < n || j < m; {
			switch {
			case i < n && j < m && am[i] == bm[j]:
				i, j = i+1, j+1
			case i < n && (j == m || t[(i+1)*w+j] >= t[i*w+j+1]):
				ops = append(ops, diffOp{'-', pre + i + 1, am[i]})
				i++
			default:
				ops = append(ops, diffOp{'+', pre + j + 1, bm[j]})
				j++
			}
		}
		return ops
	}
	count := func(xs []string) map[string]int {
		c := map[string]int{}
		for _, s := range xs {
			c[s]++
		}
		return c
	}
	inB, inA := count(bm), count(am)
	for i, s := range am {
		if inB[s] > 0 {
			inB[s]--
		} else {
			ops = append(ops, diffOp{'-', pre + i + 1, s})
		}
	}
	for j, s := range bm {
		if inA[s] > 0 {
			inA[s]--
		} else {
			ops = append(ops, diffOp{'+', pre + j + 1, s})
		}
	}
	return ops
}

func renderCompare(r compareResult, pa, pb string, n int, full bool, maxChars int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A %s (%d files) vs B %s (%d files)\n", pa, r.FilesA, pb, r.FilesB)
	fmt.Fprintf(&b, "same %d, changed %d, only in A %d, only in B %d\n", r.Same, len(r.Changed), len(r.OnlyA), len(r.OnlyB))
	if len(r.Changed) > 0 {
		b.WriteString("changed (+added -removed lines):\n")
		for i, c := range r.Changed {
			if i == n {
				fmt.Fprintf(&b, " ... %d more (n)\n", len(r.Changed)-n)
				break
			}
			if c.Binary {
				fmt.Fprintf(&b, " %s (binary or large)\n", c.name())
			} else {
				fmt.Fprintf(&b, " %s +%d -%d\n", c.name(), c.Add, c.Del)
			}
		}
	}
	list := func(side string, xs []string) {
		if len(xs) == 0 {
			return
		}
		more := ""
		if len(xs) > n {
			more = fmt.Sprintf(" ... %d more (n)", len(xs)-n)
			xs = xs[:n]
		}
		fmt.Fprintf(&b, "only in %s: %s%s\n", side, strings.Join(xs, ", "), more)
	}
	list("A", r.OnlyA)
	list("B", r.OnlyB)
	if !full {
		if len(r.Changed) > 0 {
			b.WriteString("(changed lines of a file: paths[file], full:true)\n")
		}
		return b.String()
	}
	b.WriteString(renderChanges(r.Changed, maxChars))
	return b.String()
}

// renderChanges lists the changed lines: -N (line N of A) and +N (line N of B).
func renderChanges(cs []changedFile, maxChars int) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "== %s", c.name())
		if c.Binary {
			b.WriteString(" (binary or large)\n")
			continue
		}
		fmt.Fprintf(&b, " +%d -%d\n", c.Add, c.Del)
		x, _, _ := compareContent(c.PA)
		y, _, _ := compareContent(c.PB)
		for _, o := range diffLines(splitLines(x), splitLines(y)) {
			t := o.text
			if len(t) > 200 {
				t = t[:200] + "..."
			}
			line := fmt.Sprintf("%c%d: %s\n", o.kind, o.line, t)
			if b.Len()+len(line) > maxChars {
				fmt.Fprintf(&b, "... cut at %d chars (max_chars)\n", maxChars)
				return b.String()
			}
			b.WriteString(line)
		}
	}
	return b.String()
}
