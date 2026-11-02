package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	partSuffix = ".cspart"
	trashName  = ".contextsaver-trash"
	oldTrash   = ".claudessistent-trash"
)

var errCanceled = errors.New("canceled")

type Step struct {
	Op           string     `json:"op"`
	From         StrList    `json:"from,omitempty"`
	To           string     `json:"to,omitempty"`
	ContentsOnly bool       `json:"contents_only,omitempty"`
	Flatten      bool       `json:"flatten,omitempty"`
	Conflict     string     `json:"conflict,omitempty"`
	DeleteExtra  bool       `json:"delete_extra,omitempty"`
	Rules        []Rule     `json:"rules,omitempty"`
	OnConflict   string     `json:"on_conflict,omitempty"`
	Counter      string     `json:"counter,omitempty"`
	Sort         string     `json:"sort,omitempty"`
	Start        *int       `json:"start,omitempty"`
	Folders      bool       `json:"folders,omitempty"`
	Include      string     `json:"include,omitempty"`
	Exclude      string     `json:"exclude,omitempty"`
	Recursive    *bool      `json:"recursive,omitempty"`
	MinSize      string     `json:"min_size,omitempty"`
	MaxSize      string     `json:"max_size,omitempty"`
	NewerThan    string     `json:"newer_than,omitempty"`
	OlderThan    string     `json:"older_than,omitempty"`
	Verify       string     `json:"verify,omitempty"`
	Workers      int        `json:"workers,omitempty"`
	RemoveEmpty  bool       `json:"remove_empty_dirs,omitempty"`
	Name         string     `json:"name,omitempty"`
	SHA256       string     `json:"sha256,omitempty"`
	Edits        []TextEdit `json:"edits,omitempty"`
	Append       string     `json:"append,omitempty"`
	Content      *string    `json:"content,omitempty"`
}

var validOps = map[string]bool{"copy": true, "move": true, "sync": true, "delete": true, "rename": true, "mkdir": true,
	"download": true, "extract": true, "edit": true, "write": true}

func validateSteps(steps []Step) error {
	if len(steps) == 0 {
		return errors.New("no steps given")
	}
	for i := range steps {
		s := &steps[i]
		s.Op = strings.ToLower(strings.TrimSpace(s.Op))
		if !validOps[s.Op] {
			return fmt.Errorf("step %d: unknown op %q (use copy, move, sync, delete, rename, mkdir, download, extract, edit, write)", i+1, s.Op)
		}
		switch s.Op {
		case "copy", "move", "sync":
			if len(s.From) == 0 || s.To == "" {
				return fmt.Errorf("step %d (%s): needs from and to", i+1, s.Op)
			}
		case "delete":
			if len(s.From) == 0 {
				return fmt.Errorf("step %d (delete): needs from", i+1)
			}
		case "rename":
			if len(s.From) == 0 || len(s.Rules) == 0 {
				return fmt.Errorf("step %d (rename): needs from and rules", i+1)
			}
		case "download", "extract":
			if len(s.From) == 0 || s.To == "" {
				return fmt.Errorf("step %d (%s): needs from and to", i+1, s.Op)
			}
		case "edit":
			if len(s.From) == 0 || (len(s.Edits) == 0 && s.Append == "") {
				return fmt.Errorf("step %d (edit): needs from and edits or append", i+1)
			}
		case "write":
			if s.To == "" || s.Content == nil {
				return fmt.Errorf("step %d (write): needs to and content", i+1)
			}
		case "mkdir":
			if s.To == "" && len(s.From) == 0 {
				return fmt.Errorf("step %d (mkdir): needs to", i+1)
			}
		}
	}
	return nil
}

// ---------- selection ----------

type filter struct {
	inc, exc         []string
	minSize, maxSize int64
	after, before    time.Time
	recursive        bool
	active           bool
}

func splitPatterns(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' }) {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newFilter(st *Step, defRec bool) (*filter, error) {
	f := &filter{inc: splitPatterns(st.Include), exc: splitPatterns(st.Exclude), recursive: defRec}
	if st.Recursive != nil {
		f.recursive = *st.Recursive
	}
	var err error
	if f.minSize, err = parseSize(st.MinSize); err != nil {
		return nil, err
	}
	if f.maxSize, err = parseSize(st.MaxSize); err != nil {
		return nil, err
	}
	if st.NewerThan != "" {
		if f.after, err = parseWhen(st.NewerThan); err != nil {
			return nil, err
		}
	}
	if st.OlderThan != "" {
		if f.before, err = parseWhen(st.OlderThan); err != nil {
			return nil, err
		}
	}
	f.active = len(f.inc) > 0 || len(f.exc) > 0 || f.minSize > 0 || f.maxSize > 0 || !f.after.IsZero() || !f.before.IsZero()
	return f, nil
}

func matchPats(pats []string, name, rel string) bool {
	ln := strings.ToLower(name)
	lr := strings.ToLower(filepath.ToSlash(rel))
	for _, p := range pats {
		target := ln
		if strings.ContainsAny(p, `/\`) {
			target = lr
			p = strings.ReplaceAll(p, `\`, "/")
		}
		if ok, _ := path.Match(p, target); ok {
			return true
		}
	}
	return false
}

func (f *filter) fileOK(name, rel string, info fs.FileInfo) bool {
	if len(f.inc) > 0 && !matchPats(f.inc, name, rel) {
		return false
	}
	if len(f.exc) > 0 && matchPats(f.exc, name, rel) {
		return false
	}
	if f.minSize > 0 && info.Size() < f.minSize {
		return false
	}
	if f.maxSize > 0 && info.Size() > f.maxSize {
		return false
	}
	mt := info.ModTime()
	if !f.after.IsZero() && !mt.After(f.after) {
		return false
	}
	if !f.before.IsZero() && !mt.Before(f.before) {
		return false
	}
	return true
}

func (f *filter) dirOK(name, rel string) bool {
	return !(len(f.exc) > 0 && matchPats(f.exc, name, rel))
}

type entry struct {
	src  string
	rel  string
	info fs.FileInfo
	dir  bool
	unit bool
}

type collectOpts struct {
	dirUnit, contents, flatten, wantDirs, folders, skipBlocked bool
}

type collected struct {
	files, dirs, roots []entry
	warns              []string
	warnCount, links   int
}

func (c *collected) warn(s string) {
	c.warnCount++
	if len(c.warns) < 10 {
		c.warns = append(c.warns, s)
	}
}

func collect(j *Job, st *Step, f *filter, o collectOpts) (*collected, error) {
	c := &collected{}
	for _, raw := range st.From {
		p, err := expandPath(raw)
		if err != nil {
			return nil, err
		}
		if o.skipBlocked {
			if err := checkAllowed(p); err != nil {
				return nil, err
			}
		}
		var matches []string
		wild := strings.ContainsAny(filepath.Base(p), "*?[")
		if wild {
			parent := filepath.Dir(p)
			ents, err := os.ReadDir(parent)
			if err != nil {
				return nil, fmt.Errorf("cannot open %s: %v", parent, cleanErr(err))
			}
			pat := strings.ToLower(filepath.Base(p))
			for _, e := range ents {
				if ok, _ := path.Match(pat, strings.ToLower(e.Name())); ok && !isTrash(e.Name()) {
					matches = append(matches, filepath.Join(parent, e.Name()))
				}
			}
			if len(matches) == 0 {
				c.warn("nothing matched " + p)
			}
		} else {
			matches = []string{p}
		}
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil {
				c.warn("not found: " + m)
				continue
			}
			if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				c.links++
				continue
			}
			if !info.IsDir() {
				if !o.folders && f.fileOK(info.Name(), info.Name(), info) {
					c.files = append(c.files, entry{src: m, rel: info.Name(), info: info})
				}
				continue
			}
			if o.folders && wild {
				c.files = append(c.files, entry{src: m, rel: info.Name(), info: info, dir: true})
				continue
			}
			if o.dirUnit && !f.active {
				c.files = append(c.files, entry{src: m, rel: info.Name(), info: info, dir: true, unit: true})
				continue
			}
			prefix := info.Name()
			if o.contents {
				prefix = ""
			}
			if err := walkInto(j, m, prefix, f, o, c); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

func walkInto(j *Job, root, prefix string, f *filter, o collectOpts, c *collected) error {
	c.roots = append(c.roots, entry{src: root, rel: prefix, dir: true})
	n := 0
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if j != nil && j.ctx.Err() != nil {
			return errCanceled
		}
		if err != nil {
			if p == root {
				return err
			}
			c.warn("cannot read " + p)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		name := d.Name()
		rel, _ := filepath.Rel(root, p)
		outRel := filepath.Join(prefix, rel)
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			c.links++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if isTrash(name) || !f.dirOK(name, rel) {
				return fs.SkipDir
			}
			if o.skipBlocked && checkAllowed(p) != nil {
				return fs.SkipDir
			}
			if o.folders {
				if info, e := d.Info(); e == nil {
					c.files = append(c.files, entry{src: p, rel: outRel, info: info, dir: true})
				}
			}
			if !f.recursive {
				return fs.SkipDir
			}
			if o.wantDirs {
				c.dirs = append(c.dirs, entry{src: p, rel: outRel, dir: true})
			}
			return nil
		}
		if o.folders || strings.HasSuffix(name, partSuffix) {
			return nil
		}
		info, e := d.Info()
		if e != nil || !f.fileOK(name, rel, info) {
			return nil
		}
		if o.flatten {
			outRel = name
		}
		c.files = append(c.files, entry{src: p, rel: outRel, info: info})
		if n++; j != nil && n%2000 == 0 {
			j.setCurrent("scanning " + p)
		}
		return nil
	})
}

func (r *StepResult) collectNotes(c *collected) {
	for _, w := range c.warns {
		r.note(w)
	}
	if c.warnCount > len(c.warns) {
		r.note(fmt.Sprintf("+%d more warnings", c.warnCount-len(c.warns)))
	}
	if c.links > 0 {
		r.note(fmt.Sprintf("skipped %d shortcuts/links", c.links))
	}
}

// ---------- tasks ----------

type task struct {
	src, dst  string
	size      int64
	info      fs.FileInfo
	kind      string // copy, move, movedir
	overwrite bool
	renamed   bool
}

func runStep(j *Job, st *Step, res *StepResult) error {
	switch st.Op {
	case "copy":
		return stepCopyMove(j, st, res, false)
	case "move":
		return stepCopyMove(j, st, res, true)
	case "sync":
		return stepSync(j, st, res)
	case "delete":
		return stepDelete(j, st, res)
	case "rename":
		return stepRename(j, st, res)
	case "mkdir":
		return stepMkdir(j, st, res)
	case "download":
		return stepDownload(j, st, res)
	case "extract":
		return stepExtract(j, st, res)
	case "edit":
		return stepEdit(j, st, res)
	case "write":
		return stepWrite(j, st, res)
	}
	return fmt.Errorf("unknown op %s", st.Op)
}

func verifyMode(s string) string {
	switch strings.ToLower(s) {
	case "hash", "none":
		return strings.ToLower(s)
	}
	return "size"
}

func workersFor(st *Step) int {
	w := cfg.Workers
	if st.Workers > 0 {
		w = st.Workers
	}
	return min(max(w, 1), 16)
}

func sameFile(a, b fs.FileInfo) bool {
	if a.Size() != b.Size() {
		return false
	}
	d := a.ModTime().Sub(b.ModTime())
	return d < 2*time.Second && d > -2*time.Second
}

func uniqueName(p string, reserved map[string]bool) string {
	dir, name := filepath.Split(p)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if !reserved[normCase(c)] && !exists(c) {
			return c
		}
	}
}

func stepCopyMove(j *Job, st *Step, res *StepResult, isMove bool) error {
	f, err := newFilter(st, true)
	if err != nil {
		return err
	}
	tmpl := st.To
	hasTok := strings.Contains(tmpl, "{")
	var toFixed string
	if !hasTok {
		if toFixed, err = expandPath(tmpl); err != nil {
			return err
		}
		if err := checkAllowed(toFixed); err != nil {
			return err
		}
		if info, err := os.Stat(toFixed); err == nil && !info.IsDir() {
			return fmt.Errorf("destination %s is a file, not a folder", toFixed)
		}
	} else if _, err := expandPath(tmpl[:strings.Index(tmpl, "{")] + "x"); err != nil {
		return fmt.Errorf("destination must start with an absolute path: %v", err)
	}
	conflict := strings.ToLower(st.Conflict)
	if conflict == "" {
		conflict = "keep_both"
	}
	switch conflict {
	case "keep_both", "skip", "newer", "overwrite":
	default:
		return errors.New("conflict must be keep_both, skip, newer or overwrite")
	}
	verify := verifyMode(st.Verify)
	o := collectOpts{contents: st.ContentsOnly, flatten: st.Flatten, wantDirs: true, skipBlocked: isMove,
		dirUnit: isMove && !st.Flatten && !hasTok && !st.ContentsOnly}
	c, err := collect(j, st, f, o)
	if err != nil {
		return err
	}
	res.collectNotes(c)
	if !hasTok {
		for _, r := range c.roots {
			if hasPathPrefix(toFixed, r.src) {
				return fmt.Errorf("destination %s is inside the source folder %s", toFixed, r.src)
			}
		}
	}
	verb, done := "copy", "copied"
	if isMove {
		verb, done = "move", "moved"
	}
	var tasks []task
	reserved := map[string]bool{}
	plan := func(e entry) {
		dstDir := toFixed
		if hasTok {
			d, err := expandPath(expandTokens(tmpl, tokFor(e.src, e.info, 0)))
			if err != nil || checkAllowed(d) != nil {
				res.err(e.src + ": destination not allowed")
				return
			}
			dstDir = d
		}
		dst := filepath.Join(dstDir, e.rel)
		if samePath(dst, e.src) {
			res.add("skipped (already there)", 1)
			return
		}
		t := task{src: e.src, dst: dst, size: e.info.Size(), info: e.info, kind: verb}
		if reserved[normCase(dst)] {
			t.dst, t.renamed = uniqueName(dst, reserved), true
		} else if di, err := os.Stat(dst); err == nil {
			switch {
			case di.IsDir():
				if conflict == "skip" {
					res.add("skipped (exists)", 1)
					return
				}
				t.dst, t.renamed = uniqueName(dst, reserved), true
			case sameFile(e.info, di):
				res.add("skipped (identical)", 1)
				return
			case conflict == "skip":
				res.add("skipped (exists)", 1)
				return
			case conflict == "newer":
				if !e.info.ModTime().After(di.ModTime().Add(2 * time.Second)) {
					res.add("skipped (destination same or newer)", 1)
					return
				}
				t.overwrite = true
			case conflict == "overwrite":
				t.overwrite = true
			default:
				t.dst, t.renamed = uniqueName(dst, reserved), true
			}
		}
		reserved[normCase(t.dst)] = true
		tasks = append(tasks, t)
	}
	var extra collected
	for _, e := range c.files {
		if e.unit {
			dst := filepath.Join(toFixed, e.rel)
			if hasPathPrefix(dst, e.src) {
				return fmt.Errorf("can't move %s into itself", e.src)
			}
			if !exists(dst) && sameVolume(e.src, dst) && !reserved[normCase(dst)] {
				reserved[normCase(dst)] = true
				tasks = append(tasks, task{src: e.src, dst: dst, kind: "movedir", info: e.info})
				continue
			}
			if err := walkInto(j, e.src, e.rel, f, o, &extra); err != nil {
				return err
			}
			continue
		}
		plan(e)
	}
	for _, e := range extra.files {
		plan(e)
	}
	allDirs := append(c.dirs, extra.dirs...)
	allRoots := append(c.roots, extra.roots...)
	var total int64
	for _, t := range tasks {
		total += t.size
	}
	if j.Spec.DryRun {
		n := 0
		for _, t := range tasks {
			if t.kind == "movedir" {
				n++
			}
		}
		if n > 0 {
			res.add("would move whole folders", n)
		}
		if len(tasks)-n > 0 {
			res.add("would "+verb, len(tasks)-n)
		}
		res.addBytes(total)
		shown := 0
		for _, t := range tasks {
			if t.overwrite {
				res.sample("replace (old one to Trash): " + t.dst)
				shown++
			} else if t.renamed {
				res.sample("name taken, saved as: " + t.dst)
				shown++
			}
		}
		for _, t := range tasks {
			if !t.overwrite && !t.renamed && shown < 6 {
				res.sample(t.src + " → " + t.dst)
				shown++
			}
		}
		return nil
	}
	j.addTotals(int64(len(tasks)), total)
	if !st.Flatten && !hasTok {
		for _, d := range allDirs {
			if err := j.mkdirAll(filepath.Join(toFixed, d.rel)); err != nil {
				res.err(fmt.Sprintf("%s: %v", d.rel, cleanErr(err)))
			}
		}
	}
	runTasks(j, tasks, workersFor(st), func(t task) {
		j.setCurrent(t.src)
		var err error
		if isMove {
			err = retry(j, func() error { return j.moveOne(t, verify) })
		} else {
			err = retry(j, func() error { return j.copyOne(t, verify) })
		}
		j.fileDone()
		if err != nil {
			if !errors.Is(err, errCanceled) {
				res.err(fmt.Sprintf("%s: %s", t.src, cleanErr(err)))
				j.log("error", t.src, t.dst, cleanErr(err))
			}
			return
		}
		if t.kind == "movedir" {
			res.add("moved whole folders", 1)
		} else {
			res.add(done, 1)
			res.addBytes(t.size)
		}
		if t.overwrite {
			res.add("replaced older versions", 1)
		}
		if t.renamed {
			res.add("saved under a new name", 1)
		}
		j.log(verb, t.src, t.dst, "")
	})
	if isMove && j.ctx.Err() == nil {
		var ds []string
		for _, d := range allDirs {
			ds = append(ds, d.src)
		}
		for _, r := range allRoots {
			if r.rel != "" {
				ds = append(ds, r.src)
			}
		}
		j.removeEmptyDirs(ds)
	}
	return nil
}

func stepSync(j *Job, st *Step, res *StepResult) error {
	if len(st.From) != 1 {
		return errors.New("sync needs exactly one source folder")
	}
	src, err := expandPath(st.From[0])
	if err != nil {
		return err
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", src)
	}
	to, err := expandPath(st.To)
	if err != nil {
		return err
	}
	if err := checkAllowed(to); err != nil {
		return err
	}
	if hasPathPrefix(to, src) || hasPathPrefix(src, to) {
		return errors.New("source and destination can't be inside each other")
	}
	f, err := newFilter(st, true)
	if err != nil {
		return err
	}
	verify := verifyMode(st.Verify)
	c := &collected{}
	if err := walkInto(j, src, "", f, collectOpts{wantDirs: true}, c); err != nil {
		return err
	}
	res.collectNotes(c)
	srcSet, dirSet := map[string]bool{}, map[string]bool{}
	for _, d := range c.dirs {
		dirSet[normCase(d.rel)] = true
	}
	var tasks []task
	var total int64
	nNew, nUpd := 0, 0
	for _, e := range c.files {
		srcSet[normCase(e.rel)] = true
		dst := filepath.Join(to, e.rel)
		t := task{src: e.src, dst: dst, size: e.info.Size(), info: e.info, kind: "copy"}
		if di, err := os.Stat(dst); err == nil {
			if di.IsDir() {
				res.err(dst + ": a folder is in the way")
				continue
			}
			if sameFile(e.info, di) {
				res.add("unchanged", 1)
				continue
			}
			t.overwrite = true
			nUpd++
		} else {
			nNew++
		}
		total += t.size
		tasks = append(tasks, t)
	}
	var extras, extraDirs []string
	if st.DeleteExtra {
		if _, err := os.Stat(to); err == nil {
			dc := &collected{}
			if err := walkInto(j, to, "", f, collectOpts{wantDirs: true}, dc); err != nil {
				return err
			}
			for _, e := range dc.files {
				if !srcSet[normCase(e.rel)] {
					extras = append(extras, e.src)
				}
			}
			for _, d := range dc.dirs {
				if !dirSet[normCase(d.rel)] {
					extraDirs = append(extraDirs, d.src)
				}
			}
		}
	}
	if j.Spec.DryRun {
		if nNew > 0 {
			res.add("would copy new", nNew)
		}
		if nUpd > 0 {
			res.add("would update changed", nUpd)
		}
		if len(extras) > 0 {
			res.add("would remove extra (to Trash)", len(extras))
		}
		res.addBytes(total)
		for i, t := range tasks {
			if i >= 5 {
				break
			}
			res.sample(t.src + " → " + t.dst)
		}
		for i, x := range extras {
			if i >= 5 {
				break
			}
			res.sample("remove: " + x)
		}
		return nil
	}
	j.addTotals(int64(len(tasks)+len(extras)), total)
	for _, d := range c.dirs {
		j.mkdirAll(filepath.Join(to, d.rel))
	}
	j.mkdirAll(to)
	runTasks(j, tasks, workersFor(st), func(t task) {
		j.setCurrent(t.src)
		err := retry(j, func() error { return j.copyOne(t, verify) })
		j.fileDone()
		if err != nil {
			if !errors.Is(err, errCanceled) {
				res.err(fmt.Sprintf("%s: %s", t.src, cleanErr(err)))
				j.log("error", t.src, t.dst, cleanErr(err))
			}
			return
		}
		if t.overwrite {
			res.add("updated", 1)
		} else {
			res.add("copied new", 1)
		}
		res.addBytes(t.size)
		j.log("sync", t.src, t.dst, "")
	})
	if j.ctx.Err() == nil && len(extras) > 0 {
		j.discardInto(res, extras, "removed extra")
		j.removeEmptyDirs(extraDirs)
	}
	return nil
}

func stepDelete(j *Job, st *Step, res *StepResult) error {
	f, err := newFilter(st, true)
	if err != nil {
		return err
	}
	c, err := collect(j, st, f, collectOpts{dirUnit: true, skipBlocked: true, wantDirs: true})
	if err != nil {
		return err
	}
	res.collectNotes(c)
	var paths []string
	var total int64
	for _, e := range c.files {
		if err := checkDeletable(e.src); err != nil {
			return err
		}
		paths = append(paths, e.src)
		if e.unit {
			total += dirSize(e.src)
		} else {
			total += e.info.Size()
		}
	}
	if j.Spec.DryRun {
		res.add("would delete (recoverable)", len(paths))
		res.addBytes(total)
		for i, p := range paths {
			if i >= 8 {
				break
			}
			res.sample(p)
		}
		return nil
	}
	j.addTotals(int64(len(paths)), 0)
	j.discardInto(res, paths, "deleted")
	if st.RemoveEmpty && f.active && j.ctx.Err() == nil {
		var ds []string
		for _, d := range c.dirs {
			ds = append(ds, d.src)
		}
		j.removeEmptyDirs(ds)
	}
	return nil
}

func stepMkdir(j *Job, st *Step, res *StepResult) error {
	list := append([]string{}, st.From...)
	if st.To != "" {
		list = append(list, st.To)
	}
	for _, raw := range list {
		p, err := expandPath(raw)
		if err != nil {
			return err
		}
		if err := checkAllowed(p); err != nil {
			return err
		}
		if exists(p) {
			res.add("already existed", 1)
			continue
		}
		if j.Spec.DryRun {
			res.add("would create", 1)
			continue
		}
		if err := j.mkdirAll(p); err != nil {
			res.err(fmt.Sprintf("%s: %s", p, cleanErr(err)))
			continue
		}
		res.add("created", 1)
	}
	return nil
}

func dirSize(p string) int64 {
	var n int64
	deadline := time.Now().Add(10 * time.Second)
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fs.SkipAll
		}
		if !d.IsDir() {
			if i, e := d.Info(); e == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

func runTasks(j *Job, tasks []task, workers int, fn func(task)) {
	small := make(chan task, 256)
	large := make(chan task, 8)
	var wg sync.WaitGroup
	worker := func(ch chan task) {
		defer wg.Done()
		for t := range ch {
			if j.ctx.Err() == nil {
				fn(t)
			}
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker(small)
	}
	wg.Add(1)
	go worker(large)
	for _, t := range tasks {
		if j.ctx.Err() != nil {
			break
		}
		if t.size >= 32<<20 {
			large <- t
		} else {
			small <- t
		}
	}
	close(small)
	close(large)
	wg.Wait()
}

func retry(j *Job, fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		err = fn()
		var pe permErr
		if err == nil || errors.Is(err, errCanceled) || errors.Is(err, fs.ErrNotExist) || errors.As(err, &pe) || j.ctx.Err() != nil {
			return err
		}
		if i < 2 {
			time.Sleep([]time.Duration{400 * time.Millisecond, 1500 * time.Millisecond}[i])
		}
	}
	return err
}

// ---------- file operations ----------

func (j *Job) mkdirAll(dir string) error {
	k := normCase(dir)
	if _, ok := j.madeDirs.Load(k); ok {
		return nil
	}
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		j.journal(jrec{Op: "mkdir", Path: missing[i]})
	}
	j.madeDirs.Store(k, true)
	return nil
}

func (j *Job) removeEmptyDirs(dirs []string) {
	sort.Slice(dirs, func(a, b int) bool { return len(dirs[a]) > len(dirs[b]) })
	for _, d := range dirs {
		if err := os.Remove(d); err == nil {
			j.journal(jrec{Op: "rmdir", Path: d})
		}
	}
}

func (j *Job) copyOne(t task, verify string) error {
	if err := j.mkdirAll(filepath.Dir(t.dst)); err != nil {
		return err
	}
	if t.overwrite && exists(t.dst) {
		if r := j.discardPaths([]string{t.dst}); r[0].err != nil {
			return fmt.Errorf("couldn't move the old version out of the way: %v", r[0].err)
		}
	}
	if err := copyFileData(j, t.src, t.dst, verify, false); err != nil {
		return err
	}
	j.journalCopy(t.dst)
	return nil
}

func (j *Job) journalCopy(p string) {
	if fi, err := os.Stat(p); err == nil {
		j.journal(jrec{Op: "copy", Path: p, Size: fi.Size(), MTime: fi.ModTime().UnixNano()})
	}
}

func (j *Job) moveOne(t task, verify string) error {
	if err := j.mkdirAll(filepath.Dir(t.dst)); err != nil {
		return err
	}
	if t.overwrite && exists(t.dst) {
		if r := j.discardPaths([]string{t.dst}); r[0].err != nil {
			return fmt.Errorf("couldn't move the old version out of the way: %v", r[0].err)
		}
	}
	if t.kind == "movedir" || sameVolume(t.src, t.dst) {
		err := os.Rename(t.src, t.dst)
		if err == nil {
			j.addBytes(t.size)
			j.journal(jrec{Op: "move", From: t.src, To: t.dst})
			return nil
		}
		if !isCrossDevice(err) || t.kind == "movedir" {
			return err
		}
	}
	if err := copyFileData(j, t.src, t.dst, verify, true); err != nil {
		return err
	}
	if err := os.Remove(t.src); err != nil {
		j.journalCopy(t.dst)
		return fmt.Errorf("copied, but couldn't remove the original: %s", cleanErr(err))
	}
	j.journal(jrec{Op: "move", From: t.src, To: t.dst})
	return nil
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 4<<20); return &b }}

func copyFileData(j *Job, src, dst, verify string, durable bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + partSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	var written int64
	fail := func(e error) error {
		out.Close()
		os.Remove(tmp)
		j.addBytes(-written)
		return e
	}
	var h hash.Hash
	if verify == "hash" {
		h = sha256.New()
	}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		if j.ctx.Err() != nil {
			return fail(errCanceled)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fail(werr)
			}
			if h != nil {
				h.Write(buf[:n])
			}
			written += int64(n)
			j.addBytes(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(rerr)
		}
	}
	if durable {
		if err := out.Sync(); err != nil {
			return fail(err)
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		j.addBytes(-written)
		return err
	}
	if verify != "none" {
		vi, err := os.Stat(tmp)
		if err != nil || vi.Size() != st.Size() {
			os.Remove(tmp)
			j.addBytes(-written)
			return errors.New("verify failed: copied size doesn't match")
		}
	}
	if h != nil {
		sum, err := hashFile(tmp)
		if err != nil || !bytes.Equal(sum, h.Sum(nil)) {
			os.Remove(tmp)
			j.addBytes(-written)
			return errors.New("verify failed: copy doesn't match the original")
		}
	}
	setFileTimes(tmp, st)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		j.addBytes(-written)
		return err
	}
	setFileAttrs(dst, st)
	return nil
}

func hashFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// ---------- discard (Trash / holding folder) ----------

type discardRes struct {
	orig, method, held string
	err                error
}

func (j *Job) discardPaths(paths []string) []discardRes {
	res := make([]discardRes, len(paths))
	var recyc []int
	for i, p := range paths {
		res[i].orig = p
		if useRecycleBin(p) {
			recyc = append(recyc, i)
			continue
		}
		res[i] = j.holdPath(p)
	}
	for s := 0; s < len(recyc); s += 200 {
		idx := recyc[s:min(s+200, len(recyc))]
		batch := make([]string, len(idx))
		for k, i := range idx {
			batch[k] = paths[i]
		}
		berr := recycleBatch(batch)
		for _, i := range idx {
			p := paths[i]
			if exists(p) {
				if e := recycleBatch([]string{p}); e != nil || exists(p) {
					if e == nil {
						e = berr
					}
					if e == nil {
						e = errors.New("still there after sending to the Trash")
					}
					res[i].err = fmt.Errorf("couldn't send to the Trash: %v", e)
					continue
				}
			}
			res[i].method = "recycle"
		}
	}
	for _, r := range res {
		if r.err == nil {
			j.journal(jrec{Op: "discard", Path: r.orig, Method: r.method, Held: r.held})
			j.log("discard-"+r.method, r.orig, r.held, "")
		}
	}
	return res
}

func (j *Job) holdPath(p string) discardRes {
	dst := filepath.Join(holdRoot(p), j.ID, stripVol(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return discardRes{orig: p, err: err}
	}
	if err := os.Rename(p, dst); err != nil {
		if !isCrossDevice(err) {
			return discardRes{orig: p, err: fmt.Errorf("couldn't move to the holding folder: %s", cleanErr(err))}
		}
		if err := moveAcross(j, p, dst); err != nil {
			return discardRes{orig: p, err: fmt.Errorf("couldn't move to the holding folder: %s", cleanErr(err))}
		}
	}
	return discardRes{orig: p, method: "hold", held: dst}
}

func (j *Job) discardInto(res *StepResult, paths []string, label string) {
	for s := 0; s < len(paths); s += 200 {
		if j.ctx.Err() != nil {
			return
		}
		batch := paths[s:min(s+200, len(paths))]
		j.setCurrent(batch[0])
		for _, r := range j.discardPaths(batch) {
			j.fileDone()
			if r.err != nil {
				res.err(fmt.Sprintf("%s: %v", r.orig, r.err))
				continue
			}
			if r.method == "recycle" {
				res.add(label+" (to Trash)", 1)
			} else {
				res.add(label+" (to holding folder "+filepath.Join(holdRoot(r.orig), j.ID)+")", 1)
			}
		}
	}
}

func isTrash(name string) bool { return name == trashName || name == oldTrash }
