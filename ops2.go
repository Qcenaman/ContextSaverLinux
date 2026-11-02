package main

import (
	"archive/zip"
	"bytes"
	"context"

	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type TextEdit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
	Regex   bool   `json:"regex,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// permErr marks errors that retrying won't fix.
type permErr struct{ msg string }

func (e permErr) Error() string { return e.msg }

// ---------- download ----------

var dlClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 90 * time.Second,
	TLSHandshakeTimeout:   30 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}}

func prepReq(req *http.Request) {
	req.Header.Set("User-Agent", "contextsaver/"+appVersion)
	h := strings.ToLower(req.URL.Hostname())
	if strings.HasSuffix(h, "huggingface.co") || h == "hf.co" {
		if t := os.Getenv("HF_TOKEN"); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	if strings.HasSuffix(h, "civitai.com") {
		if t := os.Getenv("CIVITAI_TOKEN"); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
}

func httpErr(resp *http.Response) error {
	switch resp.StatusCode {
	case 401, 403:
		return permErr{fmt.Sprintf("access denied (%d): this file needs a login or accepted license; set HF_TOKEN or CIVITAI_TOKEN as a Windows environment variable and restart Claude", resp.StatusCode)}
	case 404:
		return permErr{"not found (404): check the URL"}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return permErr{fmt.Sprintf("server refused (%s)", resp.Status)}
	}
	return fmt.Errorf("server error (%s)", resp.Status)
}

type dlInfo struct {
	name   string
	size   int64
	ranges bool
	final  string
}

func dispositionName(h string) string {
	if h == "" {
		return ""
	}
	_, p, err := mime.ParseMediaType(h)
	if err != nil {
		return ""
	}
	return p["filename"]
}

func urlName(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return ""
	}
	n, _ := url.PathUnescape(path.Base(pu.Path))
	if n == "." || n == "/" {
		return ""
	}
	return n
}

func probe(ctx context.Context, u string) (dlInfo, error) {
	info := dlInfo{size: -1, final: u}
	req, err := http.NewRequestWithContext(ctx, "HEAD", u, nil)
	if err != nil {
		return info, permErr{"bad URL: " + u}
	}
	prepReq(req)
	resp, err := dlClient.Do(req)
	if err != nil {
		return info, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		if resp.StatusCode == 405 || resp.StatusCode == 501 {
			return info, nil
		}
		return info, httpErr(resp)
	}
	info.size = resp.ContentLength
	info.ranges = strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
	info.name = dispositionName(resp.Header.Get("Content-Disposition"))
	info.final = resp.Request.URL.String()
	return info, nil
}

func (j *Job) fetch(u, part string, info *dlInfo, counted *int64) error {
	var start int64
	if fi, err := os.Stat(part); err == nil && info.ranges {
		start = fi.Size()
	}
	ctx, cancel := context.WithCancel(j.ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return permErr{"bad URL: " + u}
	}
	prepReq(req)
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := dlClient.Do(req)
	if err != nil {
		if j.ctx.Err() != nil {
			return errCanceled
		}
		return err
	}
	defer resp.Body.Close()
	flag := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == 206 && start > 0:
		flag |= os.O_APPEND
	case resp.StatusCode == 416 && start > 0 && start == info.size:
		return nil
	case resp.StatusCode == 200:
		start = 0
		flag |= os.O_TRUNC
		if info.size < 0 {
			info.size = resp.ContentLength
			if info.size > 0 {
				j.addTotals(0, info.size)
			}
		}
		if info.name == "" {
			info.name = dispositionName(resp.Header.Get("Content-Disposition"))
		}
	default:
		return httpErr(resp)
	}
	j.addBytes(start - *counted)
	*counted = start
	f, err := os.OpenFile(part, flag, 0o666)
	if err != nil {
		return err
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stalled := atomic.Bool{}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > 120*time.Second {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return permErr{"couldn't write: " + cleanErr(werr)}
			}
			last.Store(time.Now().UnixNano())
			j.addBytes(int64(n))
			*counted += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			if j.ctx.Err() != nil {
				return errCanceled
			}
			if stalled.Load() {
				return errors.New("download stalled for 2 minutes")
			}
			return rerr
		}
	}
	return f.Close()
}

func fileSHA256(p string) (string, error) {
	b, err := hashFile(p)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (j *Job) downloadOne(u, dir, forced, sum string, res *StepResult) error {
	info, err := probe(j.ctx, u)
	if err != nil {
		return err
	}
	name := forced
	if name == "" {
		name = info.name
	}
	if name == "" {
		name = urlName(info.final)
	}
	if name == "" {
		name = urlName(u)
	}
	if name = sanitizeName(name); name == "" {
		name = "download"
	}
	dst := filepath.Join(dir, name)
	sum = strings.ToLower(strings.TrimSpace(sum))
	if fi, err := os.Stat(dst); err == nil && !fi.IsDir() {
		if sum != "" {
			if h, _ := fileSHA256(dst); h == sum {
				res.add("skipped (already downloaded)", 1)
				return nil
			}
		} else if info.size > 0 && fi.Size() == info.size {
			res.add("skipped (already downloaded)", 1)
			return nil
		}
		dst = uniqueName(dst, nil)
	}
	part := dst + partSuffix
	if info.size > 0 {
		j.addTotals(1, info.size)
	} else {
		j.addTotals(1, 0)
	}
	j.setCurrent(u)
	var counted int64
	err = retry(j, func() error { return j.fetch(u, part, &info, &counted) })
	j.fileDone()
	if err != nil {
		if errors.Is(err, errCanceled) {
			return err
		}
		return fmt.Errorf("%s (partial file kept, running it again resumes)", err.Error())
	}
	fi, err := os.Stat(part)
	if err != nil {
		return err
	}
	if info.size > 0 && fi.Size() != info.size {
		return fmt.Errorf("incomplete: got %s of %s (running it again resumes)", hb(fi.Size()), hb(info.size))
	}
	if sum != "" {
		h, err := fileSHA256(part)
		if err != nil {
			return err
		}
		if h != sum {
			os.Remove(part)
			return permErr{"checksum doesn't match, file removed (sha256 " + h + ")"}
		}
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	j.journalCopy(dst)
	j.log("download", u, dst, "")
	res.add("downloaded", 1)
	res.addBytes(fi.Size())
	res.sample(dst + " (" + hb(fi.Size()) + ")")
	return nil
}

func stepDownload(j *Job, st *Step, res *StepResult) error {
	dir, err := expandPath(st.To)
	if err != nil {
		return err
	}
	if err := checkAllowed(dir); err != nil {
		return err
	}
	var urls []string
	for _, u := range st.From {
		u = strings.TrimSpace(u)
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			res.err("not a web address: " + u)
			continue
		}
		urls = append(urls, u)
	}
	forced := ""
	if len(urls) == 1 {
		forced = st.Name
	}
	if j.Spec.DryRun {
		for _, u := range urls {
			info, err := probe(j.ctx, u)
			if err != nil {
				res.err(u + ": " + err.Error())
				continue
			}
			n := forced
			if n == "" {
				n = info.name
			}
			if n == "" {
				n = urlName(info.final)
			}
			sz := "size unknown"
			if info.size > 0 {
				sz = hb(info.size)
				res.addBytes(info.size)
			}
			res.add("would download", 1)
			res.sample(filepath.Join(dir, sanitizeName(n)) + " (" + sz + ")")
		}
		return nil
	}
	if err := j.mkdirAll(dir); err != nil {
		return err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for _, u := range urls {
		if j.ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := j.downloadOne(u, dir, forced, st.SHA256, res); err != nil && !errors.Is(err, errCanceled) {
				res.err(u + ": " + cleanErr(err))
				j.log("error", u, dir, cleanErr(err))
			}
		}(u)
	}
	wg.Wait()
	return nil
}

// ---------- extract ----------

func find7z() string {
	for _, c := range []string{filepath.Join(os.Getenv("ProgramFiles"), "7-Zip", "7z.exe"), filepath.Join(os.Getenv("ProgramFiles(x86)"), "7-Zip", "7z.exe")} {
		if c != "" && exists(c) {
			return c
		}
	}
	for _, n := range []string{"7z", "7za"} {
		if p, err := lookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func stepExtract(j *Job, st *Step, res *StepResult) error {
	to, err := expandPath(st.To)
	if err != nil {
		return err
	}
	if err := checkAllowed(to); err != nil {
		return err
	}
	conflict := strings.ToLower(st.Conflict)
	if conflict == "" {
		conflict = "keep_both"
	}
	for _, raw := range st.From {
		src, err := expandPath(raw)
		if err != nil {
			return err
		}
		fi, err := os.Stat(src)
		if err != nil || fi.IsDir() {
			res.err("not an archive file: " + src)
			continue
		}
		if strings.EqualFold(filepath.Ext(src), ".zip") {
			if err := j.unzip(src, to, conflict, res); err != nil {
				if errors.Is(err, errCanceled) {
					return nil
				}
				res.err(src + ": " + cleanErr(err))
			}
			continue
		}
		if j.Spec.DryRun {
			res.add("would extract", 1)
			res.sample(src + " → " + to)
			continue
		}
		if err := j.extractExternal(src, to); err != nil {
			res.err(src + ": " + err.Error())
			continue
		}
		res.add("extracted archives", 1)
		res.note("archives other than .zip can't be undone automatically")
	}
	return nil
}

func (j *Job) unzip(src, to, conflict string, res *StepResult) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	type item struct {
		f      *zip.File
		target string
	}
	var items []item
	var total int64
	var replace []string
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		clean := strings.TrimPrefix(path.Clean("/"+name), "/")
		if clean == "" {
			continue
		}
		target := filepath.Join(to, filepath.FromSlash(clean))
		if !hasPathPrefix(target, to) {
			res.err("unsafe path skipped: " + f.Name)
			continue
		}
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			items = append(items, item{f, target})
			continue
		}
		if fi, err := os.Stat(target); err == nil {
			if fi.IsDir() {
				res.err("a folder is in the way: " + target)
				continue
			}
			if fi.Size() == int64(f.UncompressedSize64) {
				d := fi.ModTime().Sub(f.Modified)
				if d < 2*time.Second && d > -2*time.Second {
					res.add("skipped (identical)", 1)
					continue
				}
			}
			switch conflict {
			case "skip":
				res.add("skipped (exists)", 1)
				continue
			case "overwrite", "newer":
				if conflict == "newer" && !f.Modified.After(fi.ModTime()) {
					res.add("skipped (existing is newer)", 1)
					continue
				}
				replace = append(replace, target)
			default:
				target = uniqueName(target, nil)
			}
		}
		total += int64(f.UncompressedSize64)
		items = append(items, item{f, target})
	}
	if j.Spec.DryRun {
		n := 0
		for _, it := range items {
			if !it.f.FileInfo().IsDir() {
				n++
			}
		}
		res.add("would extract files", n)
		if len(replace) > 0 {
			res.add("would replace (old to Trash)", len(replace))
		}
		res.addBytes(total)
		res.sample(src + " → " + to)
		return nil
	}
	if len(replace) > 0 {
		for _, r := range j.discardPaths(replace) {
			if r.err != nil {
				return fmt.Errorf("couldn't move %s out of the way: %v", r.orig, r.err)
			}
		}
	}
	j.addTotals(int64(len(items)), total)
	for _, it := range items {
		if j.ctx.Err() != nil {
			return errCanceled
		}
		if it.f.FileInfo().IsDir() || strings.HasSuffix(it.f.Name, "/") {
			j.mkdirAll(it.target)
			j.fileDone()
			continue
		}
		j.setCurrent(it.target)
		if err := j.mkdirAll(filepath.Dir(it.target)); err != nil {
			res.err(it.target + ": " + cleanErr(err))
			continue
		}
		if err := j.writeZipEntry(it.f, it.target); err != nil {
			res.err(it.target + ": " + cleanErr(err))
			j.fileDone()
			continue
		}
		j.journalCopy(it.target)
		j.fileDone()
		res.add("extracted files", 1)
		res.addBytes(int64(it.f.UncompressedSize64))
	}
	j.log("extract", src, to, "")
	return nil
}

func (j *Job) writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := target + partSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	if _, err := io.CopyBuffer(out, rc, *bp); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if !f.Modified.IsZero() {
		os.Chtimes(tmp, f.Modified, f.Modified)
	}
	return os.Rename(tmp, target)
}

func (j *Job) extractExternal(src, to string) error {
	if err := j.mkdirAll(to); err != nil {
		return err
	}
	j.setCurrent("extracting " + src)
	var out string
	var err error
	if exe := find7z(); exe != "" {
		out, err = runQuietCtx(j.ctx, exe, "x", "-y", "-o"+to, src)
	} else {
		out, err = runQuietCtx(j.ctx, "tar", "-xf", src, "-C", to)
	}
	if err != nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) > 4 {
			lines = lines[len(lines)-4:]
		}
		msg := strings.Join(lines, " | ")
		if find7z() == "" {
			msg += " (installing 7-Zip handles more archive types)"
		}
		return errors.New(clip(msg, 400))
	}
	j.log("extract", src, to, "")
	return nil
}

// ---------- edit / write ----------

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func applyEdits(text string, edits []TextEdit, crlf bool) (string, int, error) {
	total := 0
	for _, e := range edits {
		if e.Find == "" {
			return "", 0, errors.New("an edit has an empty find")
		}
		if e.Regex {
			re, err := regexp.Compile(e.Find)
			if err != nil {
				return "", 0, fmt.Errorf("bad regex %q: %v", e.Find, err)
			}
			rep := e.Replace
			locs := re.FindAllStringSubmatchIndex(text, -1)
			if len(locs) == 0 {
				return "", 0, fmt.Errorf("not found: %q", clip(e.Find, 80))
			}
			if e.Count > 0 && len(locs) > e.Count {
				locs = locs[:e.Count]
			}
			var b strings.Builder
			prev := 0
			for _, l := range locs {
				b.WriteString(text[prev:l[0]])
				b.Write(re.ExpandString(nil, rep, text, l))
				prev = l[1]
			}
			b.WriteString(text[prev:])
			text = b.String()
			total += len(locs)
			continue
		}
		find, rep := e.Find, e.Replace
		if crlf {
			find, rep = toCRLF(find), toCRLF(rep)
		}
		n := strings.Count(text, find)
		if n == 0 {
			return "", 0, fmt.Errorf("not found: %q", clip(e.Find, 80))
		}
		lim := -1
		if e.Count > 0 {
			lim = e.Count
			n = min(n, e.Count)
		}
		text = strings.Replace(text, find, rep, lim)
		total += n
	}
	return text, total, nil
}

func diffSample(p, a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(la), len(lb)); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return fmt.Sprintf("%s L%d: «%s» → «%s»", filepath.Base(p), i+1, clip(strings.TrimRight(x, "\r"), 120), clip(strings.TrimRight(y, "\r"), 120))
		}
	}
	return filepath.Base(p)
}

var bomUTF8 = []byte{0xEF, 0xBB, 0xBF}

func readEditable(p string) ([]byte, string, bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, "", false, err
	}
	if fi.Size() > 20<<20 {
		return nil, "", false, errors.New("too big to edit (over 20 MB)")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, "", false, err
	}
	if bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || bytes.HasPrefix(b, []byte{0xFE, 0xFF}) {
		return nil, "", false, errors.New("UTF-16 files can't be edited")
	}
	bom := bytes.HasPrefix(b, bomUTF8)
	t := b
	if bom {
		t = b[3:]
	}
	if !utf8.Valid(t) || bytes.IndexByte(t, 0) >= 0 {
		return nil, "", false, errors.New("not a text file")
	}
	return b, string(t), bom, nil
}

func (j *Job) replaceText(p string, old []byte, text string, bom bool) error {
	bdir := filepath.Join(j.Dir, "backups")
	os.MkdirAll(bdir, 0o755)
	backup := filepath.Join(bdir, randHex(4)+"-"+filepath.Base(p))
	if old != nil {
		if err := os.WriteFile(backup, old, 0o644); err != nil {
			return err
		}
	}
	data := []byte(text)
	if bom {
		data = append(append([]byte{}, bomUTF8...), data...)
	}
	tmp := p + ".csedit"
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if old != nil {
		h, _ := fileSHA256(p)
		j.journal(jrec{Op: "edit", Path: p, Held: backup, Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Hash: h})
	} else {
		j.journal(jrec{Op: "copy", Path: p, Size: fi.Size(), MTime: fi.ModTime().UnixNano()})
	}
	return nil
}

func stepEdit(j *Job, st *Step, res *StepResult) error {
	f, err := newFilter(st, false)
	if err != nil {
		return err
	}
	c, err := collect(j, st, f, collectOpts{contents: true, skipBlocked: true})
	if err != nil {
		return err
	}
	res.collectNotes(c)
	for _, e := range c.files {
		p := e.src
		old, text, bom, err := readEditable(p)
		if err != nil {
			res.err(p + ": " + err.Error())
			continue
		}
		crlf := strings.Contains(text, "\r\n")
		nt, n, err := text, 0, error(nil)
		if len(st.Edits) > 0 {
			if nt, n, err = applyEdits(text, st.Edits, crlf); err != nil {
				res.err(p + ": " + err.Error() + " (file left unchanged)")
				continue
			}
		}
		if st.Append != "" {
			a := st.Append
			if crlf {
				a = toCRLF(a)
			}
			if nt != "" && !strings.HasSuffix(nt, "\n") {
				if crlf {
					nt += "\r\n"
				} else {
					nt += "\n"
				}
			}
			nt += a
			n++
		}
		if nt == text {
			res.add("unchanged", 1)
			continue
		}
		if j.Spec.DryRun {
			res.add("would edit", 1)
			res.sample(diffSample(p, text, nt))
			continue
		}
		if err := j.replaceText(p, old, nt, bom); err != nil {
			res.err(p + ": " + cleanErr(err))
			continue
		}
		j.log("edit", p, "", fmt.Sprintf("%d changes", n))
		res.add("edited files", 1)
		res.add("changes", n)
		res.sample(diffSample(p, text, nt))
	}
	return nil
}

func stepWrite(j *Job, st *Step, res *StepResult) error {
	p, err := expandPath(st.To)
	if err != nil {
		return err
	}
	if err := checkAllowed(p); err != nil {
		return err
	}
	var old []byte
	bom := false
	if fi, err := os.Stat(p); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s is a folder", p)
		}
		if old, err = os.ReadFile(p); err != nil {
			return err
		}
		if string(old) == *st.Content {
			res.add("unchanged", 1)
			return nil
		}
	}
	if j.Spec.DryRun {
		if old != nil {
			res.add("would replace (old version kept for undo)", 1)
		} else {
			res.add("would create", 1)
		}
		res.sample(p)
		return nil
	}
	if err := j.mkdirAll(filepath.Dir(p)); err != nil {
		return err
	}
	if err := j.replaceText(p, old, *st.Content, bom); err != nil {
		return err
	}
	j.log("write", p, "", "")
	if old != nil {
		res.add("replaced", 1)
	} else {
		res.add("created", 1)
	}
	return nil
}

// ---------- JSON values ----------

type valCount struct{ n, files, last int }

func readValues(p string, info fs.FileInfo, keys []string, ext string) (string, error) {
	if len(keys) == 0 && ext == "" {
		return "", errors.New("values needs keys and/or ext")
	}
	want := map[string]string{}
	for _, k := range keys {
		want[strings.ToLower(k)] = k
	}
	var exts []string
	for _, e := range splitPatterns(ext) {
		exts = append(exts, strings.TrimPrefix(e, "*"))
	}
	var files []string
	if info.IsDir() {
		filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
			if err != nil || len(files) >= 1000 {
				return nil
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(q), ".json") {
				files = append(files, q)
			}
			return nil
		})
	} else {
		files = []string{p}
	}
	counts := map[string]map[string]*valCount{}
	var order []string
	rec := func(key, v string, fi int) {
		m := counts[key]
		if m == nil {
			m = map[string]*valCount{}
			counts[key] = m
			order = append(order, key)
		}
		c := m[v]
		if c == nil {
			c = &valCount{last: -1}
			m[v] = c
		}
		c.n++
		if c.last != fi {
			c.files++
			c.last = fi
		}
	}
	var walk func(v any, fi int)
	walk = func(v any, fi int) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if name, ok := want[strings.ToLower(k)]; ok {
					switch y := x.(type) {
					case string:
						if y != "" {
							rec(name, y, fi)
						}
					case float64:
						rec(name, fmt.Sprint(y), fi)
					case bool:
						rec(name, fmt.Sprint(y), fi)
					case []any:
						for _, z := range y {
							if s, ok := z.(string); ok && s != "" {
								rec(name, s, fi)
							}
						}
					}
				}
				walk(x, fi)
			}
		case []any:
			for _, x := range t {
				walk(x, fi)
			}
		case string:
			ls := strings.ToLower(t)
			for _, e := range exts {
				if strings.HasSuffix(ls, e) {
					rec("files ("+ext+")", t, fi)
					break
				}
			}
		}
	}
	bad := 0
	for i, f := range files {
		fi, err := os.Stat(f)
		if err != nil || fi.Size() > 30<<20 {
			bad++
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			bad++
			continue
		}
		var v any
		if err := json.Unmarshal(bytes.TrimPrefix(b, bomUTF8), &v); err != nil {
			bad++
			continue
		}
		walk(v, i)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Read %d JSON file(s)", len(files)-bad)
	if bad > 0 {
		fmt.Fprintf(&b, ", %d couldn't be read", bad)
	}
	if len(order) == 0 {
		b.WriteString("; no matching values.")
		return b.String(), nil
	}
	for _, k := range order {
		m := counts[k]
		vals := make([]string, 0, len(m))
		for v := range m {
			vals = append(vals, v)
		}
		sort.Slice(vals, func(a, c int) bool {
			if m[vals[a]].n != m[vals[c]].n {
				return m[vals[a]].n > m[vals[c]].n
			}
			return vals[a] < vals[c]
		})
		fmt.Fprintf(&b, "\n%s (%d distinct): ", k, len(vals))
		var parts []string
		for i, v := range vals {
			if i >= 80 {
				parts = append(parts, fmt.Sprintf("+%d more", len(vals)-80))
				break
			}
			s := clip(v, 100)
			if m[v].n > 1 {
				s += fmt.Sprintf(" ×%d", m[v].n)
			}
			if len(files) > 1 {
				s += fmt.Sprintf(" [%d files]", m[v].files)
			}
			parts = append(parts, s)
		}
		b.WriteString(strings.Join(parts, " · "))
	}
	return b.String(), nil
}
