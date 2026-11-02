package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// ---------- text extraction ----------

var errBinary = errors.New("binary file (not text)")

var docExts = map[string]string{
	".docx": "docx", ".docm": "docx", ".dotx": "docx", ".pptx": "pptx", ".xlsx": "xlsx", ".xlsm": "xlsx",
	".odt": "odf", ".ods": "odf", ".odp": "odf", ".pdf": "pdf",
}

var textExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields("txt md markdown csv tsv json jsonl xml html htm log ini cfg conf toml yaml yml go py js ts tsx jsx java kt c cpp h hpp cs php rb rs sh bat cmd ps1 sql srt vtt rtf tex css scss lua swift r m eml") {
		textExts["."+e] = true
	}
}

func isDocExt(ext string) bool { _, ok := docExts[strings.ToLower(ext)]; return ok }

func aiSupported(ext string) bool {
	ext = strings.ToLower(ext)
	return textExts[ext] || isDocExt(ext) || isImageExt(ext)
}

func decodeText(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return utf16Decode(b[2:], binary.LittleEndian), nil
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return utf16Decode(b[2:], binary.BigEndian), nil
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	}
	chk := b
	if len(chk) > 8000 {
		chk = chk[:8000]
	}
	if bytes.IndexByte(chk, 0) >= 0 {
		return "", errBinary
	}
	if utf8.Valid(b) || utf8.Valid(b[:max(0, len(b)-3)]) {
		return strings.ToValidUTF8(string(b), ""), nil
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r), nil
}

func utf16Decode(b []byte, bo binary.ByteOrder) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = bo.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

func readTextFile(p string, limit int) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return "", err
	}
	return decodeText(b)
}

// textOpts: how long OCR may run for scanned PDF pages and images (0 = only what's
// cached), which PDF pages to read (nil = all), and a context to stop waiting.
type textOpts struct {
	ocrWait time.Duration
	pages   map[int]bool
	ctx     context.Context
	ocrUpTo int  // only OCR scanned pages up to this page (0 = all)
	legacy  bool // open .doc/.xls/.ppt through the installed Office
}

func extractText(p string, limit int) (string, error) { return extractTextOpt(p, limit, textOpts{}) }

func extractTextOpt(p string, limit int, o textOpts) (string, error) {
	ext := strings.ToLower(filepath.Ext(p))
	if isImageExt(ext) {
		return imageText(p, o)
	}
	switch docExts[ext] {
	case "pdf":
		return pdfFileText(p, limit, o)
	case "docx":
		return zipText(p, func(n string) bool { return n == "word/document.xml" }, limit)
	case "pptx":
		return zipText(p, func(n string) bool { return strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml") }, limit)
	case "xlsx":
		return xlsxText(p, "", limit)
	case "odf":
		return zipText(p, func(n string) bool { return n == "content.xml" }, limit)
	}
	if _, ok := legacyOffice[ext]; ok {
		if !o.legacy {
			return "", errBinary
		}
		np, err := legacyCopy(p)
		if err != nil {
			return "", err
		}
		return extractTextOpt(np, limit, o)
	}
	t, err := readTextFile(p, limit)
	if err != nil {
		return "", err
	}
	if ext == ".html" || ext == ".htm" {
		t = stripHTML(t)
	}
	return t, nil
}

func zipText(p string, want func(string) bool, limit int) (string, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return "", fmt.Errorf("couldn't open document: %v", err)
	}
	defer zr.Close()
	var files []*zip.File
	for _, f := range zr.File {
		if want(f.Name) {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(a, b int) bool { return natLess(files[a].Name, files[b].Name) })
	var b strings.Builder
	for _, f := range files {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(rc)
		for b.Len() < limit {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.CharData:
				b.Write(t)
			case xml.EndElement:
				switch t.Name.Local {
				case "p", "si", "tr", "br", "h":
					b.WriteByte('\n')
				case "tc", "tab":
					b.WriteByte('\t')
				}
			}
		}
		rc.Close()
		b.WriteByte('\n')
	}
	return b.String(), nil
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blockTagRe    = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6])[^>]*>`)
	tagRe         = regexp.MustCompile(`<[^>]+>`)
	blankRe       = regexp.MustCompile(`\n\s*\n+`)
)

func stripHTML(s string) string {
	s = scriptStyleRe.ReplaceAllString(s, " ")
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(blankRe.ReplaceAllString(html.UnescapeString(s), "\n\n"))
}

func chunkText(t string, size int) []string {
	t = strings.TrimSpace(t)
	var out []string
	for len(t) > size {
		cut := strings.LastIndex(t[:size], "\n")
		if cut < size/2 {
			cut = strings.LastIndex(t[:size], " ")
		}
		if cut < size/2 {
			cut = size
		}
		for cut > 0 && !utf8.RuneStart(t[cut]) {
			cut--
		}
		out = append(out, t[:cut])
		t = strings.TrimSpace(t[cut:])
	}
	if t != "" {
		out = append(out, t)
	}
	return out
}

// ---------- local AI client ----------

type AIRequest struct {
	Action     string   `json:"action"`
	Paths      StrList  `json:"paths,omitempty"`
	Question   string   `json:"question,omitempty"`
	Categories []string `json:"categories,omitempty"`
	MoveTo     string   `json:"move_to,omitempty"`
	Model      string   `json:"model,omitempty"`
	Include    string   `json:"include,omitempty"`
	Recursive  *bool    `json:"recursive,omitempty"`
	MaxFiles   int      `json:"max_files,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Force      bool     `json:"force,omitempty"`
}

type aiClient struct {
	kind, base, model string
	hc                *http.Client
	noThink           bool
	chunk             int
	vision            bool
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

func cleanAI(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

func isLocalURL(u string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := pu.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (c *aiClient) chat(system, user string, maxTok int) (string, error) {
	return c.chatImg(system, user, nil, maxTok)
}

// chatImg sends a prompt with an optional JPEG for vision models.
func (c *aiClient) chatImg(system, user string, img []byte, maxTok int) (string, error) {
	var msgs []map[string]any
	msgs = append(msgs, map[string]any{"role": "system", "content": system})
	switch {
	case img == nil:
		msgs = append(msgs, map[string]any{"role": "user", "content": user})
	case c.kind == "ollama":
		msgs = append(msgs, map[string]any{"role": "user", "content": user, "images": []string{base64.StdEncoding.EncodeToString(img)}})
	default:
		msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img)}},
			{"type": "text", "text": user}}})
	}
	var endpoint string
	var body map[string]any
	if c.kind == "ollama" {
		endpoint = c.base + "/api/chat"
		body = map[string]any{"model": c.model, "messages": msgs, "stream": false,
			"options": map[string]any{"temperature": 0.1, "num_ctx": 16384, "num_predict": maxTok + 1024}}
		if c.noThink {
			body["think"] = false
		}
	} else {
		endpoint = c.base + "/v1/chat/completions"
		body = map[string]any{"model": c.model, "messages": msgs, "temperature": 0.1, "max_tokens": maxTok + 1024, "stream": false}
	}
	b, _ := json.Marshal(body)
	resp, err := c.hc.Post(endpoint, "application/json", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("local AI didn't answer: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		if c.kind == "ollama" && c.noThink && bytes.Contains(bytes.ToLower(rb), []byte("think")) {
			c.noThink = false
			return c.chatImg(system, user, img, maxTok)
		}
		return "", fmt.Errorf("local AI error %d: %s", resp.StatusCode, clip(string(rb), 200))
	}
	var out string
	if c.kind == "ollama" {
		var r struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		json.Unmarshal(rb, &r)
		out = r.Message.Content
	} else {
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		json.Unmarshal(rb, &r)
		if len(r.Choices) > 0 {
			out = r.Choices[0].Message.Content
		}
	}
	return cleanAI(out), nil
}

func (c *aiClient) models() ([]string, error) {
	qc := &http.Client{Timeout: 3 * time.Second}
	path := "/v1/models"
	if c.kind == "ollama" {
		path = "/api/tags"
	}
	resp, err := qc.Get(c.base + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range r.Models {
		out = append(out, m.Name)
	}
	for _, m := range r.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

func pickModel(ms []string, want string) (string, error) {
	var chat []string
	for _, m := range ms {
		if !strings.Contains(strings.ToLower(m), "embed") {
			chat = append(chat, m)
		}
	}
	for _, w := range []string{want, cfg.AIModel} {
		if w == "" {
			continue
		}
		for _, m := range chat {
			if strings.EqualFold(m, w) {
				return m, nil
			}
		}
		for _, m := range chat {
			if strings.Contains(strings.ToLower(m), strings.ToLower(w)) {
				return m, nil
			}
		}
		if w == want {
			return "", fmt.Errorf("model %q isn't available; available: %s", want, strings.Join(chat, ", "))
		}
	}
	for _, m := range chat {
		if strings.Contains(strings.ToLower(m), "qwen") {
			return m, nil
		}
	}
	if len(chat) > 0 {
		return chat[0], nil
	}
	return "", errors.New("no chat model loaded")
}

// healthy reports whether a llama.cpp-style server is up and its model is loaded.
func (c *aiClient) healthy() bool {
	qc := &http.Client{Timeout: 2 * time.Second}
	resp, err := qc.Get(c.base + "/health")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return true
		}
		if resp.StatusCode == 503 {
			return false
		}
	}
	_, err = c.models()
	return err == nil
}

func (c *aiClient) loadCtx() {
	c.chunk = 12000
	qc := &http.Client{Timeout: 3 * time.Second}
	resp, err := qc.Get(c.base + "/props")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var r struct {
		NCtx int `json:"n_ctx"`
		Def  struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		Modalities struct {
			Vision bool `json:"vision"`
		} `json:"modalities"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return
	}
	c.vision = r.Modalities.Vision
	n := r.Def.NCtx
	if n == 0 {
		n = r.NCtx
	}
	if n > 0 {
		c.chunk = min(max((n-1500)*3, 3000), 24000)
	}
}

var (
	portRe    = regexp.MustCompile(`(?i)--port[ =]+"?(\d+)`)
	hostRe    = regexp.MustCompile(`(?i)--host[ =]+"?([^\s"]+)`)
	setPortRe = regexp.MustCompile(`(?i)set\s+"?port=(\d+)`)
)

func scriptAddr(script string) (string, int) {
	host, port := "127.0.0.1", 8080
	b, err := os.ReadFile(script)
	if err != nil {
		return host, port
	}
	s := string(b)
	if m := portRe.FindStringSubmatch(s); m != nil {
		port, _ = strconv.Atoi(m[1])
	} else if m := setPortRe.FindStringSubmatch(s); m != nil {
		port, _ = strconv.Atoi(m[1])
	}
	if m := hostRe.FindStringSubmatch(s); m != nil && m[1] != "0.0.0.0" && !strings.Contains(m[1], "%") {
		host = m[1]
	}
	return host, port
}

var aiMu sync.Mutex

// openAI returns a client and a release function. The model server is shared
// (RemoteAI and other tools use it too), so release never stops it: a server this
// app started is recorded as ContextSaver's and stops only after its idle time,
// when nothing is processing (serverguard.go). A server already on the port, even
// one still loading, is used as it is and never stopped.
func openAI(j *Job, want string) (*aiClient, func(), error) {
	noop := func() {}
	hc := &http.Client{Timeout: 10 * time.Minute}
	if cfg.AIEndpoint == "" && cfg.AIScript != "" && exists(cfg.AIScript) {
		host, port := scriptAddr(cfg.AIScript)
		c := &aiClient{kind: "openai", base: fmt.Sprintf("http://%s:%d", host, port), hc: hc}
		finish := func(stop func()) (*aiClient, func(), error) {
			ms, err := c.models()
			if err == nil {
				c.model, err = pickModel(ms, want)
			}
			if err != nil && len(ms) == 0 {
				c.model, err = "local", nil
			}
			if err != nil {
				stop()
				return nil, noop, err
			}
			c.loadCtx()
			return c, stop, nil
		}
		release := func() { touchOwned(port) }
		if c.healthy() {
			touchOwned(port)
			return finish(release)
		}
		// Something already holds the port (loading, or starting up): wait for it,
		// never start a second copy and never stop it.
		if probeServer(port).Up || guardPIDOnPort(port) > 0 {
			j.setCurrent(fmt.Sprintf("waiting for the local AI server on port %d to be ready…", port))
			deadline := time.Now().Add(5 * time.Minute)
			for time.Now().Before(deadline) {
				if j.ctx.Err() != nil {
					return nil, noop, errCanceled
				}
				if c.healthy() {
					touchOwned(port)
					return finish(release)
				}
				time.Sleep(time.Second)
			}
			return nil, noop, fmt.Errorf("the server on port %d wasn't ready after 5 minutes; it was left running", port)
		}
		// LM Studio has a model loaded: use it rather than starting a second model.
		if id := lmsLoaded(); id != "" {
			lc := &aiClient{kind: "openai", base: lmsBase, hc: hc, model: id, noThink: true}
			lc.loadCtx()
			return lc, noop, nil
		}
		if useLMStudio() {
			return nil, noop, errors.New("no model is loaded in LM Studio; load one with model{name}")
		}
		j.setCurrent("starting the local AI model…")
		p, err := guardStartScript(cfg.AIScript)
		if err != nil {
			return nil, noop, fmt.Errorf("couldn't start %s: %v", cfg.AIScript, err)
		}
		// If it never gets ready, stop only what this job started: the script, and the
		// server on the port only while it is still the one that came up for it.
		ownPID := 0
		stop := func() { stopOwnLocalAI(p, port, ownPID); unregister(port) }
		deadline := time.Now().Add(5 * time.Minute)
		var exitedAt time.Time
		for time.Now().Before(deadline) {
			if j.ctx.Err() != nil {
				stop()
				return nil, noop, errCanceled
			}
			if pid := guardPIDOnPort(port); ownPID == 0 && pid > 0 {
				ownPID = pid // the port was free before the start: the first listener is ours
			}
			if c.healthy() {
				spid := 0
				if p.cmd != nil && p.cmd.Process != nil {
					spid = p.cmd.Process.Pid
				}
				registerOwned(ownedServer{Port: port, PID: ownPID, ScriptPID: spid, Model: modelForScript(cfg.AIScript), Script: cfg.AIScript})
				return finish(release)
			}
			if exitedAt.IsZero() && p.exited() {
				exitedAt = time.Now()
			}
			if !exitedAt.IsZero() && time.Since(exitedAt) > 45*time.Second {
				stop()
				return nil, noop, errors.New("the local AI script closed before the model was ready; run it once by hand to see the error")
			}
			time.Sleep(time.Second)
		}
		stop()
		return nil, noop, errors.New("the local AI model wasn't ready after 5 minutes")
	}
	type cand struct{ kind, base string }
	cands := []cand{{"ollama", "http://127.0.0.1:11434"}, {"openai", "http://127.0.0.1:1234"}}
	if cfg.AIEndpoint != "" {
		u := strings.TrimRight(cfg.AIEndpoint, "/")
		if !isLocalURL(u) {
			return nil, noop, errors.New("ai_endpoint must be on this PC (localhost)")
		}
		k := "openai"
		if strings.Contains(u, ":11434") {
			k = "ollama"
		}
		cands = []cand{{k, u}}
	}
	var lastErr error = errors.New("nothing answered")
	for _, cd := range cands {
		c := &aiClient{kind: cd.kind, base: cd.base, hc: hc, noThink: true, chunk: 12000}
		ms, err := c.models()
		if err != nil {
			lastErr = err
			continue
		}
		m, err := pickModel(ms, want)
		if err != nil {
			lastErr = err
			continue
		}
		c.model = m
		if c.kind == "ollama" {
			c.vision = c.ollamaVision()
		} else {
			c.loadCtx()
		}
		return c, noop, nil
	}
	return nil, noop, fmt.Errorf("no local AI found (set ai_script in config, or start Ollama / LM Studio): %v", lastErr)
}

// ---------- AI jobs ----------

func aiGather(j *Job, req *AIRequest) ([]string, []string, error) {
	rec := true
	if req.Recursive != nil {
		rec = *req.Recursive
	}
	f := &filter{inc: splitPatterns(req.Include), recursive: rec}
	maxFiles := req.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 200
	}
	maxFiles = min(maxFiles, 2000)
	var files, notes []string
	seen, unsupported := 0, 0
	add := func(p string, info fs.FileInfo) {
		ext := strings.ToLower(filepath.Ext(p))
		if !aiSupported(ext) {
			unsupported++
			return
		}
		seen++
		if len(files) < maxFiles {
			files = append(files, p)
		}
	}
	for _, raw := range req.Paths {
		p, err := expandPath(raw)
		if err != nil {
			return nil, nil, err
		}
		if err := checkAllowed(p); err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, nil, errors.New(cleanErr(err) + ": " + p)
		}
		if !info.IsDir() {
			add(p, info)
			continue
		}
		c := &collected{}
		if err := walkInto(j, p, "", f, collectOpts{}, c); err != nil {
			return nil, nil, err
		}
		for _, e := range c.files {
			add(e.src, e.info)
		}
	}
	if seen > len(files) {
		notes = append(notes, fmt.Sprintf("only the first %d of %d files were read; narrow it with include or raise max_files", len(files), seen))
	}
	if unsupported > 0 {
		notes = append(notes, fmt.Sprintf("skipped %d files that aren't text, documents or images", unsupported))
	}
	return files, notes, nil
}

const aiSystem = "You read documents on behalf of another assistant. Use only the text you are given. Be brief and exact: keep names, numbers and dates as written."

func isNone(s string) bool {
	u := strings.ToUpper(strings.TrimSpace(strings.Trim(s, ".*\"' ")))
	return u == "NONE" || strings.HasPrefix(u, "NONE") && len(u) < 12
}

func runAI(j *Job) error {
	req := j.Spec.AI
	if req == nil {
		return errors.New("no AI request")
	}
	aiMu.Lock()
	defer aiMu.Unlock()
	files, notes, err := aiGather(j, req)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no readable files found there (" + strings.Join(notes, "; ") + ")")
	}
	if !req.Force && j.Spec.Source != "chat" {
		if w := gpuWarning(); w != "" {
			return errors.New(strings.Replace(w, "call local_ai again with the same arguments plus force:true", "run it again with force set", 1))
		}
	}
	c, stop, err := openAI(j, req.Model)
	if err != nil {
		return err
	}
	defer stop()
	j.addTotals(int64(len(files)), 0)
	var out string
	switch req.Action {
	case "ask":
		out, err = aiAsk(j, c, files, req.Question)
	case "summarize":
		out, err = aiSummarize(j, c, files, req.Detail)
	case "classify":
		out, err = aiClassify(j, c, files, req)
	default:
		err = fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		return err
	}
	head := fmt.Sprintf("Local AI (%s) read %d file(s).", c.model, len(files))
	if c.vision {
		head = fmt.Sprintf("Local AI (%s, sees images) read %d file(s).", c.model, len(files))
	}
	for _, n := range notes {
		head += "\nnote: " + n
	}
	j.setOutput(head + "\n" + out)
	return nil
}

type aiFail struct{ n, streak int }

func (f *aiFail) hit(err error) error {
	f.n++
	f.streak++
	if f.streak >= 3 {
		return fmt.Errorf("local AI keeps failing: %v", err)
	}
	return nil
}

// aiDoc is one thing the local AI reads: a file, or a web page's text. image is set
// for pictures when the model can see them.
type aiDoc struct {
	label, ref string
	load       func() (string, error)
	image      func() ([]byte, error)
}

// aiFileText reads a file for the local AI; scanned pages and images go through OCR.
func aiFileText(j *Job, f string, limit int) (string, error) {
	return extractTextOpt(f, limit, textOpts{ocrWait: 20 * time.Minute, ctx: j.ctx})
}

// aiImage is a picture sized for a local vision model, or nil when the model can't see.
func aiImage(c *aiClient, f string) []byte {
	if !c.vision || !isImageExt(filepath.Ext(f)) {
		return nil
	}
	pv, err := imagePreview(f, 1024)
	if err != nil {
		return nil
	}
	return pv.jpg
}

func (c *aiClient) ollamaVision() bool {
	b, _ := json.Marshal(map[string]any{"model": c.model})
	qc := &http.Client{Timeout: 5 * time.Second}
	resp, err := qc.Post(c.base+"/api/show", "application/json", bytes.NewReader(b))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var r struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return false
	}
	for _, x := range r.Capabilities {
		if x == "vision" {
			return true
		}
	}
	return false
}

func aiAsk(j *Job, c *aiClient, files []string, q string) (string, error) {
	docs := make([]aiDoc, len(files))
	for i, f := range files {
		f := f
		docs[i] = aiDoc{label: filepath.Base(f), ref: f, load: func() (string, error) { return aiFileText(j, f, 400000) }}
		if c.vision && isImageExt(filepath.Ext(f)) {
			docs[i].image = func() ([]byte, error) {
				if img := aiImage(c, f); img != nil {
					return img, nil
				}
				return nil, errors.New("no image")
			}
		}
	}
	return aiAskDocs(j, c, docs, q, "Files", "files", "file")
}

// aiAskDocs asks each doc (in chunks) for facts that answer q, then combines them.
// plural/lower/one name the docs in the output ("Files", "files", "file").
func aiAskDocs(j *Job, c *aiClient, docs []aiDoc, q, plural, lower, one string) (string, error) {
	if strings.TrimSpace(q) == "" {
		return "", errors.New("ask needs a question")
	}
	type hit struct{ ref, label, text string }
	var hits []hit
	var fails aiFail
	for _, d := range docs {
		if j.ctx.Err() != nil {
			return "", errCanceled
		}
		j.setCurrent(d.ref)
		if d.image != nil {
			if img, err := d.image(); err == nil {
				j.fileDone()
				ans, err := c.chatImg(aiSystem, fmt.Sprintf("Question: %s\n\nImage: %s\n\nIf this image helps answer the question, reply with the relevant facts in at most 3 short sentences, including any text in it that matters. If it does not, reply with exactly NONE.", q, d.label), img, 300)
				if err != nil {
					if e := fails.hit(err); e != nil {
						return "", e
					}
					continue
				}
				fails.streak = 0
				if !isNone(ans) && ans != "" {
					hits = append(hits, hit{d.ref, d.label, ans})
				}
				continue
			}
		}
		text, err := d.load()
		j.fileDone()
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		chunks := chunkText(text, c.chunk)
		if len(chunks) > 12 {
			chunks = chunks[:12]
		}
		var found []string
		for ci, ch := range chunks {
			prompt := fmt.Sprintf("Question: %s\n\nDocument: %s (part %d of %d)\n<<<\n%s\n>>>\n\nIf this text helps answer the question, reply with the relevant facts in at most 3 short sentences. If it does not, reply with exactly NONE.",
				q, d.label, ci+1, len(chunks), ch)
			ans, err := c.chat(aiSystem, prompt, 300)
			if err != nil {
				if e := fails.hit(err); e != nil {
					return "", e
				}
				continue
			}
			fails.streak = 0
			if !isNone(ans) && ans != "" {
				found = append(found, ans)
			}
		}
		if len(found) > 0 {
			hits = append(hits, hit{d.ref, d.label, strings.Join(found, " ")})
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No %s seemed to answer: %s", one, q), nil
	}
	var fb strings.Builder
	for i, h := range hits {
		if i >= 40 {
			break
		}
		fmt.Fprintf(&fb, "[%d] %s: %s\n", i+1, h.label, clip(h.text, 700))
	}
	final, err := c.chat(aiSystem, fmt.Sprintf("Question: %s\n\nFindings from %s:\n%s\nAnswer the question in at most 6 sentences, citing %s like [1]. Say so if the findings conflict or don't fully answer it.", q, lower, fb.String(), lower), 500)
	if err != nil {
		final = "(couldn't combine the findings: " + err.Error() + ")"
	}
	var b strings.Builder
	b.WriteString("Answer: " + final + "\n" + plural + ":")
	for i, h := range hits {
		if i >= 15 {
			fmt.Fprintf(&b, "\n+%d more %s with findings", len(hits)-15, lower)
			break
		}
		fmt.Fprintf(&b, "\n[%d] %s — %s", i+1, h.ref, clip(h.text, 220))
	}
	if fails.n > 0 {
		fmt.Fprintf(&b, "\nnote: %d read(s) failed", fails.n)
	}
	return b.String(), nil
}

func aiSummarize(j *Job, c *aiClient, files []string, detail string) (string, error) {
	n := 3
	if strings.ToLower(detail) == "normal" {
		n = 6
	}
	var b strings.Builder
	var fails aiFail
	for _, f := range files {
		if j.ctx.Err() != nil {
			return "", errCanceled
		}
		j.setCurrent(f)
		if img := aiImage(c, f); img != nil {
			j.fileDone()
			sum, err := c.chatImg(aiSystem, fmt.Sprintf("Image: %s\n\nDescribe this image in at most %d sentences: what it shows, and any text, names, dates or amounts in it.", filepath.Base(f), n), img, 350)
			if err != nil {
				if e := fails.hit(err); e != nil {
					return b.String(), e
				}
				fmt.Fprintf(&b, "• %s — (failed: %v)\n", f, err)
				continue
			}
			fails.streak = 0
			fmt.Fprintf(&b, "• %s — %s\n", f, strings.ReplaceAll(sum, "\n", " "))
			continue
		}
		text, err := aiFileText(j, f, 400000)
		j.fileDone()
		if err != nil || strings.TrimSpace(text) == "" {
			fmt.Fprintf(&b, "• %s — (no readable text)\n", f)
			continue
		}
		chunks := chunkText(text, c.chunk)
		if len(chunks) > 10 {
			chunks = chunks[:10]
		}
		var sum string
		if len(chunks) == 1 {
			sum, err = c.chat(aiSystem, fmt.Sprintf("Document: %s\n<<<\n%s\n>>>\n\nSummarize this document in at most %d sentences: what it is and its key facts (names, dates, amounts).", filepath.Base(f), chunks[0], n), 350)
		} else {
			var parts []string
			for ci, ch := range chunks {
				s, e := c.chat(aiSystem, fmt.Sprintf("Document: %s (part %d of %d)\n<<<\n%s\n>>>\n\nSummarize this part in at most 3 sentences.", filepath.Base(f), ci+1, len(chunks), ch), 250)
				if e == nil {
					parts = append(parts, s)
				}
			}
			sum, err = c.chat(aiSystem, fmt.Sprintf("These are summaries of the parts of %s:\n%s\n\nWrite one summary of the whole document in at most %d sentences.", filepath.Base(f), strings.Join(parts, "\n"), n), 350)
		}
		if err != nil {
			if e := fails.hit(err); e != nil {
				return b.String(), e
			}
			fmt.Fprintf(&b, "• %s — (failed: %v)\n", f, err)
			continue
		}
		fails.streak = 0
		fmt.Fprintf(&b, "• %s — %s\n", f, strings.ReplaceAll(sum, "\n", " "))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func aiClassify(j *Job, c *aiClient, files []string, req *AIRequest) (string, error) {
	var cats []string
	for _, x := range req.Categories {
		if x = strings.TrimSpace(x); x != "" {
			cats = append(cats, x)
		}
	}
	if len(cats) < 2 {
		return "", errors.New("classify needs at least 2 categories")
	}
	groups := map[string][]string{}
	var fails aiFail
	for _, f := range files {
		if j.ctx.Err() != nil {
			return "", errCanceled
		}
		j.setCurrent(f)
		cat := "Other"
		if img := aiImage(c, f); img != nil {
			j.fileDone()
			ans, err := c.chatImg(aiSystem, fmt.Sprintf("Categories: %s\n\nImage: %s\n\nWhich one category fits this image best? Reply with only the category name exactly as written above, or Other if none fits.", strings.Join(cats, " | "), filepath.Base(f)), img, 30)
			if err != nil {
				if e := fails.hit(err); e != nil {
					return "", e
				}
			} else {
				fails.streak = 0
				cat = matchCategory(ans, cats)
			}
			groups[cat] = append(groups[cat], f)
			continue
		}
		text, err := aiFileText(j, f, 200000)
		j.fileDone()
		if err == nil && strings.TrimSpace(text) != "" {
			ans, err := c.chat(aiSystem, fmt.Sprintf("Categories: %s\n\nDocument: %s\n<<<\n%s\n>>>\n\nWhich one category fits this document best? Reply with only the category name exactly as written above, or Other if none fits.",
				strings.Join(cats, " | "), filepath.Base(f), clip(text, min(c.chunk, 8000))), 30)
			if err != nil {
				if e := fails.hit(err); e != nil {
					return "", e
				}
			} else {
				fails.streak = 0
				cat = matchCategory(ans, cats)
			}
		}
		groups[cat] = append(groups[cat], f)
	}
	var b strings.Builder
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %d file(s)\n", k, len(groups[k]))
		for i, f := range groups[k] {
			if i >= 10 {
				fmt.Fprintf(&b, "   +%d more\n", len(groups[k])-10)
				break
			}
			b.WriteString("   " + filepath.Base(f) + "\n")
		}
	}
	if req.MoveTo != "" {
		base, err := expandPath(req.MoveTo)
		if err != nil {
			return "", err
		}
		for _, k := range keys {
			if k == "Other" && !containsFold(cats, "Other") {
				continue
			}
			st := Step{Op: "move", From: groups[k], To: filepath.Join(base, sanitizeName(k)), Conflict: "keep_both"}
			res := j.newStep(stepTitle(&st))
			if err := runStep(j, &st, res); err != nil {
				res.err(err.Error())
			}
		}
		if j.Spec.DryRun {
			b.WriteString("(dry run: files were not moved)")
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func matchCategory(ans string, cats []string) string {
	a := strings.ToLower(strings.Trim(strings.TrimSpace(ans), ".*\"'"))
	for _, c := range cats {
		if strings.ToLower(c) == a {
			return c
		}
	}
	for _, c := range cats {
		if strings.Contains(a, strings.ToLower(c)) {
			return c
		}
	}
	return "Other"
}

func containsFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func aiModelsText() string {
	var b strings.Builder
	if cfg.AIScript != "" {
		host, port := scriptAddr(cfg.AIScript)
		state := "not running (the app starts it when a job needs it, then shuts it down)"
		c := &aiClient{kind: "openai", base: fmt.Sprintf("http://%s:%d", host, port)}
		if !exists(cfg.AIScript) {
			state = "script not found"
		} else if c.healthy() {
			ms, _ := c.models()
			state = "running: " + strings.Join(ms, ", ")
		}
		fmt.Fprintf(&b, "llama.cpp script %s (%s:%d): %s\n", cfg.AIScript, host, port, state)
	}
	for _, cd := range []struct{ kind, base string }{{"ollama", "http://127.0.0.1:11434"}, {"openai", "http://127.0.0.1:1234"}} {
		c := &aiClient{kind: cd.kind, base: cd.base}
		if ms, err := c.models(); err == nil {
			fmt.Fprintf(&b, "%s: %s\n", cd.base, strings.Join(ms, ", "))
		}
	}
	if b.Len() == 0 {
		return "No local AI found."
	}
	return strings.TrimRight(b.String(), "\n")
}
