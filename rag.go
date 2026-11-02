package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// rag: meaning-based search over the user's files, plus memory notes Claude
// keeps across chats. A small embedding model (llama.cpp) turns passages into
// vectors; the index is plain files under the app folder and is searched by
// brute force, which is fast enough for a few million passages.

const (
	ragDim       = 384  // vector size kept (the model's output is shortened and renormalized)
	ragChunk     = 1200 // characters per passage
	ragOverlap   = 150
	ragPort      = 8091
	ragQueryInst = "Instruct: Given a search query, retrieve passages from the user's files and notes that answer it\nQuery: "
)

const defaultEmbedModel = "Qwen/Qwen3-Embedding-0.6B-GGUF:Q8_0"

// ragDir: the search index and memory notes. By default ContextSaver shares
// TokenSaver's, so Claude and the local agents search the same index and notes.
func ragDir() string {
	if cfg.RagDir != "" {
		return cfg.RagDir
	}
	if isWin {
		return `D:\Claude Apps\TokenSaver\data\rag`
	}
	return filepath.Join(appHome, "rag")
}

type ragConfig struct {
	Roots   []string `json:"roots"`
	Exclude []string `json:"exclude"` // folder paths or folder names
	Model   string   `json:"model,omitempty"`
	Updated string   `json:"updated,omitempty"`
	Types   []string `json:"types,omitempty"` // only these extensions (empty = all readable types)
}

func defaultRagRoots() []string {
	if isWin {
		return []string{`C:\`, `D:\`}
	}
	h, _ := os.UserHomeDir()
	roots := []string{h}
	for _, d := range windowsMounts() { // the Windows data drives (D:), not Windows itself
		if !isWindowsSystem(d) {
			roots = append(roots, d)
		}
	}
	return roots
}

func loadRagConfig() ragConfig {
	var c ragConfig
	if b, err := os.ReadFile(filepath.Join(ragDir(), "config.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	if len(c.Roots) == 0 {
		c.Roots = defaultRagRoots()
	}
	if c.Model == "" {
		c.Model = defaultEmbedModel
	}
	return c
}

// saveRagConfig: see rag_guard.go (keeps backups, refuses temp roots for the real index).

// ---------- what gets indexed ----------

var ragSkipNames = map[string]bool{
	"appdata": true, "program files": true, "program files (x86)": true, "programdata": true, "windows": true,
	"$recycle.bin": true, "system volume information": true, "recovery": true, "perflogs": true,
	"dist": true, "build": true, ".next": true, ".nuxt": true, "target": true, "out": true, "bin": true, "obj": true,
	"env": true, ".env": true, ".idea": true, ".vscode": true, ".vs": true, "coverage": true, "vendor": true,
	"python_embeded": true, "python_embedded": true, "lib": true, "libs": true, "include": true, "scripts": true,
	"steamapps": true, "steamlibrary": true, "cache": true, "caches": true, "temp": true, "tmp": true, "logs": true,
	"models": true, "checkpoints": true, "loras": true, "output": true, "outputs": true, "input": true, "backups": true,
}

// "lib", "scripts", "include", "bin" are usually installed tools (Python, games);
// a folder with these names right inside a project root is rare enough to accept.

var ragDataExts = map[string]bool{".csv": true, ".tsv": true, ".json": true, ".jsonl": true, ".xml": true}

func ragWanted(p string, size int64) bool {
	ext := strings.ToLower(filepath.Ext(p))
	base := strings.ToLower(filepath.Base(p))
	if !textExts[ext] && !isDocExt(ext) || ext == ".log" {
		return false
	}
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") || strings.HasSuffix(base, ".lock") ||
		base == "package-lock.json" || base == "pnpm-lock.yaml" || base == "yarn.lock" || strings.HasSuffix(base, ".map") {
		return false
	}
	switch {
	case size == 0:
		return false
	case ragDataExts[ext]:
		return size <= 200<<10
	case isDocExt(ext):
		return size <= 60<<20
	default:
		return size <= 3<<20
	}
}

type ragExcl struct {
	names map[string]bool
	paths []string
	exts  map[string]bool // from "*.html" entries
	only  map[string]bool // config types; empty = all
}

func ragExclusions(c ragConfig) ragExcl {
	e := ragExcl{names: map[string]bool{}, exts: map[string]bool{}, only: map[string]bool{}}
	for _, t := range c.Types {
		e.only["."+strings.ToLower(strings.TrimLeft(strings.TrimSpace(t), "*."))] = true
	}
	for k := range ragSkipNames {
		e.names[k] = true
	}
	e.paths = append(e.paths, appHome, ragDir(), appRoot())
	if isWin {
		e.paths = append(e.paths, `D:\Claude Apps\TokenSaver`)
	}
	e.paths = append(e.paths, filepath.Join(llamaDir(), "models"), genScriptDir())
	e.paths = append(e.paths, blockedRoots()...)
	for _, x := range c.Exclude {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if strings.HasPrefix(x, "*.") {
			e.exts[strings.ToLower(x[1:])] = true
			continue
		}
		if strings.ContainsAny(x, `\/:`) {
			if p, err := expandPath(x); err == nil {
				e.paths = append(e.paths, p)
			}
		} else if strings.HasPrefix(x, "+") { // "+name" removes a default skipped name
			delete(e.names, strings.ToLower(x[1:]))
		} else {
			e.names[strings.ToLower(x)] = true
		}
	}
	return e
}

func (e ragExcl) skip(name, full string) bool {
	if e.names[strings.ToLower(name)] || isHeavyDir(name, full) || strings.HasPrefix(name, ".") && len(name) > 1 {
		return true
	}
	for _, p := range e.paths {
		if samePath(full, p) || hasPathPrefix(full, p) {
			return true
		}
	}
	return false
}

type ragCand struct {
	path  string
	size  int64
	mtime int64
}

func ragWalk(ctx context.Context, roots []string, e ragExcl) []ragCand {
	var mu sync.Mutex
	var out []ragCand
	parallelWalk(roots, func(root, dir string, ents []fs.DirEntry) []string {
		var subs []string
		for _, de := range ents {
			full := filepath.Join(dir, de.Name())
			if de.IsDir() {
				if !e.skip(de.Name(), full) {
					subs = append(subs, full)
				}
				continue
			}
			if !de.Type().IsRegular() {
				continue
			}
			fi, err := de.Info()
			if err != nil || !ragWanted(full, fi.Size()) || e.exts[strings.ToLower(filepath.Ext(full))] ||
				len(e.only) > 0 && !e.only[strings.ToLower(filepath.Ext(full))] {
				continue
			}
			mu.Lock()
			out = append(out, ragCand{full, fi.Size(), fi.ModTime().UnixNano()})
			mu.Unlock()
		}
		return subs
	}, func() bool { return ctx.Err() != nil })
	sort.Slice(out, func(a, b int) bool { return out[a].path < out[b].path })
	return out
}

// ---------- index on disk ----------

type ragFile struct {
	Path  string
	Size  int64
	MTime int64
	First int32 // first chunk
	N     int32 // chunks (0 = no text)
}

type ragMeta struct {
	Files   []ragFile
	FileOf  []int32 // per chunk
	Line    []int32 // start line, or -page for PDFs
	TextOff []int64
	TextLen []int32
	Dim     int
	Model   string
	Updated time.Time
}

type ragIndex struct {
	meta  ragMeta
	vecs  []int8
	scale []float32
	text  *os.File
}

func (x *ragIndex) n() int { return len(x.meta.FileOf) }

var (
	ragMu     sync.Mutex
	ragLoaded *ragIndex
	ragUsed   time.Time
	ragStamp  int64
)

func ragMetaPath() string { return filepath.Join(ragDir(), "index.gob") }

// openRagIndex loads the index (cached in memory for 10 minutes after last use).
func openRagIndex() (*ragIndex, error) {
	ragMu.Lock()
	defer ragMu.Unlock()
	fi, err := os.Stat(ragMetaPath())
	if err != nil {
		return nil, errors.New("no index yet: run rag update (dry_run first to see what it covers)")
	}
	if ragLoaded != nil && ragStamp == fi.ModTime().UnixNano() {
		ragUsed = time.Now()
		return ragLoaded, nil
	}
	if ragLoaded != nil {
		ragLoaded = nil
	}
	x, err := readRagIndex()
	if err != nil {
		return nil, err
	}
	ragLoaded, ragStamp, ragUsed = x, fi.ModTime().UnixNano(), time.Now()
	go func() {
		for {
			time.Sleep(time.Minute)
			ragMu.Lock()
			if ragLoaded == nil || time.Since(ragUsed) > 10*time.Minute {
				if ragLoaded != nil {
					ragLoaded = nil
				}
				ragMu.Unlock()
				return
			}
			ragMu.Unlock()
		}
	}()
	return x, nil
}

func readRagIndex() (*ragIndex, error) {
	f, err := os.Open(ragMetaPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	x := &ragIndex{}
	if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&x.meta); err != nil {
		return nil, fmt.Errorf("index is damaged (%v); run rag update to rebuild", err)
	}
	n := x.n()
	vb, err := os.ReadFile(filepath.Join(ragDir(), "vectors.bin"))
	if err != nil || len(vb) != n*(x.meta.Dim+4) {
		return nil, errors.New("index vectors are missing or damaged; run rag update to rebuild")
	}
	x.vecs = make([]int8, n*x.meta.Dim)
	x.scale = make([]float32, n)
	for i := 0; i < n; i++ {
		x.scale[i] = math.Float32frombits(binary.LittleEndian.Uint32(vb[i*4:]))
	}
	off := n * 4
	for i := range x.vecs {
		x.vecs[i] = int8(vb[off+i])
	}
	return x, nil
}

// chunkText reads one passage. The text file is opened only while it is read
// (or held by an update), so a running update can always replace it on Windows.
func (x *ragIndex) chunkText(i int) string {
	b := make([]byte, x.meta.TextLen[i])
	f := x.text
	if f == nil {
		var err error
		if f, err = os.Open(filepath.Join(ragDir(), "text.bin")); err != nil {
			return ""
		}
		defer f.Close()
	}
	f.ReadAt(b, x.meta.TextOff[i])
	return string(b)
}

// ragWriter builds a new index next to the old one, then swaps it in.
type ragWriter struct {
	meta  ragMeta
	vecs  bytes.Buffer
	scale []float32
	text  *bufio.Writer
	tf    *os.File
	toff  int64
}

func newRagWriter(model string) (*ragWriter, error) {
	os.MkdirAll(ragDir(), 0o755)
	tf, err := os.Create(filepath.Join(ragDir(), "text.bin.new"))
	if err != nil {
		return nil, err
	}
	return &ragWriter{meta: ragMeta{Dim: ragDim, Model: model}, tf: tf, text: bufio.NewWriterSize(tf, 1<<20)}, nil
}

func (w *ragWriter) addFile(f ragFile) int32 {
	f.First = int32(len(w.meta.FileOf))
	f.N = 0
	w.meta.Files = append(w.meta.Files, f)
	return int32(len(w.meta.Files) - 1)
}

func (w *ragWriter) addChunk(fi int32, line int32, text string, vec []int8, scale float32) {
	if w.meta.Files[fi].N == 0 {
		w.meta.Files[fi].First = int32(len(w.meta.FileOf))
	}
	w.meta.FileOf = append(w.meta.FileOf, fi)
	w.meta.Line = append(w.meta.Line, line)
	w.meta.TextOff = append(w.meta.TextOff, w.toff)
	w.meta.TextLen = append(w.meta.TextLen, int32(len(text)))
	w.text.WriteString(text)
	w.toff += int64(len(text))
	for _, v := range vec {
		w.vecs.WriteByte(byte(v))
	}
	w.scale = append(w.scale, scale)
	w.meta.Files[fi].N++
}

// truncate drops files from index nf on (and their passages): files whose
// passages weren't all embedded when an update stopped.
func (w *ragWriter) truncate(nf int) {
	if nf >= len(w.meta.Files) {
		return
	}
	nc := len(w.meta.FileOf)
	for i := nf; i < len(w.meta.Files); i++ {
		if w.meta.Files[i].N > 0 {
			nc = min(nc, int(w.meta.Files[i].First))
		}
	}
	w.meta.Files = w.meta.Files[:nf]
	w.meta.FileOf, w.meta.Line = w.meta.FileOf[:nc], w.meta.Line[:nc]
	w.meta.TextOff, w.meta.TextLen = w.meta.TextOff[:nc], w.meta.TextLen[:nc]
	w.scale = w.scale[:nc]
	w.vecs.Truncate(nc * w.meta.Dim)
}

// commit writes everything and replaces the live index.
func (w *ragWriter) commit() error {
	if err := w.text.Flush(); err != nil {
		return err
	}
	w.tf.Sync()
	w.meta.Updated = time.Now()
	vf, err := os.Create(filepath.Join(ragDir(), "vectors.bin.new"))
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(vf, 1<<20)
	var b4 [4]byte
	for _, s := range w.scale {
		binary.LittleEndian.PutUint32(b4[:], math.Float32bits(s))
		bw.Write(b4[:])
	}
	bw.Write(w.vecs.Bytes())
	if err := bw.Flush(); err != nil {
		vf.Close()
		return err
	}
	vf.Close()
	mf, err := os.Create(ragMetaPath() + ".new")
	if err != nil {
		return err
	}
	mb := bufio.NewWriter(mf)
	if err := gob.NewEncoder(mb).Encode(&w.meta); err != nil {
		mf.Close()
		return err
	}
	mb.Flush()
	mf.Close()
	// The text file must stay open for writing until the end; swap copies the
	// current state so a checkpoint doesn't stop the writer.
	ragMu.Lock()
	defer ragMu.Unlock()
	if ragLoaded != nil {
		ragLoaded = nil
	}
	if err := copyFileTo(filepath.Join(ragDir(), "text.bin.new"), filepath.Join(ragDir(), "text.bin.tmp")); err != nil {
		return err
	}
	for _, s := range []string{"text.bin", "vectors.bin"} {
		src := filepath.Join(ragDir(), s+".new")
		if s == "text.bin" {
			src = filepath.Join(ragDir(), "text.bin.tmp")
		}
		if err := renameRetry(src, filepath.Join(ragDir(), s)); err != nil {
			return err
		}
	}
	return renameRetry(ragMetaPath()+".new", ragMetaPath())
}

// renameRetry: a search may hold the file for a moment.
func renameRetry(a, b string) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(a, b); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func (w *ragWriter) close() {
	w.tf.Close()
	os.Remove(filepath.Join(ragDir(), "text.bin.new"))
}

// ---------- the embedding model ----------

type embedServer struct {
	mu      sync.Mutex
	proc    *exec.Cmd
	done    chan struct{}
	users   int
	idle    *time.Timer
	cpu     bool
	adopted bool // reusing a model another ContextSaver process left running
}

var embedSrv = &embedServer{}

func embedBase() string { return fmt.Sprintf("http://127.0.0.1:%d", ragPort) }

func embedHealthy() bool {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(embedBase() + "/health")
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == 200
}

// acquire starts the model if needed (on the CPU when cpu is true) and returns a release func.
// The model stays loaded 3 minutes after the last use, then stops if nothing (TokenSaver
// included) is using it. One another program started is used and never stopped here.
func (s *embedServer) acquire(ctx context.Context, cpu, big bool, status func(string)) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if s.proc != nil && s.cpu && !cpu && !probeServer(ragPort).Busy { // move to the GPU for big jobs
		s.stopLocked()
	}
	if s.proc == nil || !embedHealthy() {
		if s.proc != nil {
			s.stopLocked()
		}
		if err := s.startLocked(ctx, cpu, big, status); err != nil {
			return nil, err
		}
	}
	s.users++
	if s.proc != nil {
		touchOwned(ragPort)
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.users--
		if s.proc != nil {
			touchOwned(ragPort)
		}
		if s.users == 0 {
			s.armIdleLocked()
		}
	}, nil
}

// armIdleLocked stops the model after embedIdleMinutes unused; busy (another client)
// or pinned waits another round.
func (s *embedServer) armIdleLocked() {
	s.idle = time.AfterFunc(embedIdleMinutes*time.Minute, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.users != 0 {
			return
		}
		if pinned, _ := guardSettings(); s.proc != nil && (pinned || probeServer(ragPort).Busy) {
			s.armIdleLocked()
			return
		}
		s.stopLocked()
	})
}

func llamaDir() string {
	if isWin {
		if cfg.AIScript != "" {
			return filepath.Dir(cfg.AIScript)
		}
		return `D:\llama.cpp`
	}
	return filepath.Dir(linuxLlamaServer(""))
}

func (s *embedServer) startLocked(ctx context.Context, cpu, big bool, status func(string)) error {
	exe := filepath.Join(llamaDir(), "llama-server.exe")
	if !isWin {
		exe = filepath.Join(llamaDir(), "llama-server")
	}
	if !exists(exe) {
		return errors.New(llamaServerHint(exe))
	}
	if embedHealthy() { // already running (another ContextSaver window, TokenSaver): use it
		// A big job moves it to the GPU only when a ContextSaver started it and it's idle;
		// one started elsewhere is used as it is and never stopped.
		if cpu || !big {
			s.adopted = true
			return nil
		}
		if ok, _ := guardStop(ragPort, manualStop, false); !ok {
			s.adopted = true
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	model := loadRagConfig().Model
	ngl := "99"
	if cpu {
		ngl = "0"
	}
	ctxN, par, ub := "8192", "4", "2048" // ~1 GB of VRAM with the model
	if cpu {                             // small buffers: on the CPU it would otherwise hold ~12 GB of RAM
		ctxN, par, ub = "4096", "2", "2048"
	}
	args := []string{"--embedding", "--pooling", "last", "-ngl", ngl, "-c", ctxN, "--parallel", par,
		"-b", ub, "-ub", ub, "--host", "127.0.0.1", "--port", strconv.Itoa(ragPort)}
	if strings.HasSuffix(strings.ToLower(model), ".gguf") {
		args = append([]string{"-m", model}, args...)
	} else {
		args = append([]string{"-hf", model}, args...)
	}
	cmd := quietCmd(exe, args...)
	cmd.Dir = llamaDir()
	cmd.Env = append(os.Environ(), "LLAMA_CACHE="+filepath.Join(llamaDir(), "models"))
	if !isWin { // llama.cpp's Linux build keeps its libraries next to the program
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+llamaDir()+":"+os.Getenv("LD_LIBRARY_PATH"))
	}
	logf, _ := os.Create(filepath.Join(ragDir(), "embed-server.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); logf.Close(); close(done) }()
	s.proc, s.done, s.cpu = cmd, done, cpu
	registerOwned(ownedServer{Port: ragPort, PID: cmd.Process.Pid, Model: "embed", IdleMinutes: embedIdleMinutes})
	where := "GPU"
	if cpu {
		where = "CPU"
	}
	status("loading the embedding model on the " + where + " (first time: downloading it, ~640 MB)…")
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			s.proc = nil
			tail, _ := os.ReadFile(filepath.Join(ragDir(), "embed-server.log"))
			return fmt.Errorf("the embedding model didn't start: %s", clip(lastLines(string(tail), 3), 400))
		case <-ctx.Done():
			s.stopLocked()
			return errCanceled
		default:
		}
		if embedHealthy() {
			return nil
		}
		time.Sleep(time.Second)
	}
	s.stopLocked()
	return errors.New("the embedding model wasn't ready after 20 minutes")
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

// restart starts the model again after it stopped in the middle of an update.
// On the GPU when there is room; if the GPU is no longer usable, the update stops
// with its progress kept rather than crawling on on the CPU.
func (s *embedServer) restart(ctx context.Context, cpu bool, status func(string)) error {
	if !cpu {
		if blocked, why := ragGPUBlocked(); blocked {
			return fmt.Errorf("the embedding model stopped and the GPU can't take it back now (%s); progress so far is kept, run update again when the GPU is free (or cpu:true)", why)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	return s.startLocked(ctx, cpu, true, status)
}

// shutdown stops the model this process started when ContextSaver exits, unless it is
// busy (TokenSaver using it) or pinned: then it stays up, recorded as ContextSaver's,
// and another ContextSaver stops it once it is idle. One it adopted is left alone.
func (s *embedServer) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if s.proc != nil {
		if pinned, _ := guardSettings(); pinned || probeServer(ragPort).Busy {
			s.proc, s.adopted = nil, false
			return
		}
	}
	s.stopLocked()
}

// stopLocked stops the model this process started; an adopted one (started by another
// program) is only let go, never stopped.
func (s *embedServer) stopLocked() {
	s.adopted = false
	if s.proc != nil && s.proc.Process != nil {
		unregister(ragPort)
		if isWin {
			quietCmd("taskkill", "/PID", strconv.Itoa(s.proc.Process.Pid), "/T", "/F").Run()
		} else {
			killTree(s.proc.Process.Pid)
		}
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
		}
	}
	s.proc = nil
}

var embedClient = &http.Client{Timeout: 5 * time.Minute}

// embed returns unit vectors shortened to ragDim, quantized to int8 with a scale.
func embed(ctx context.Context, texts []string) ([][]int8, []float32, error) {
	body, _ := json.Marshal(map[string]any{"input": texts, "model": "embed"})
	req, _ := http.NewRequestWithContext(ctx, "POST", embedBase()+"/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := embedClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return nil, nil, fmt.Errorf("embedding model: %s %s", resp.Status, string(b))
	}
	var r struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, nil, err
	}
	if len(r.Data) != len(texts) {
		return nil, nil, fmt.Errorf("embedding model returned %d of %d vectors", len(r.Data), len(texts))
	}
	vecs := make([][]int8, len(texts))
	scales := make([]float32, len(texts))
	for _, d := range r.Data {
		v := d.Embedding
		if len(v) > ragDim {
			v = v[:ragDim]
		}
		var norm, mx float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			norm = 1
		}
		for _, x := range v {
			mx = math.Max(mx, math.Abs(float64(x)/norm))
		}
		if mx == 0 {
			mx = 1
		}
		q := make([]int8, ragDim)
		for i, x := range v {
			q[i] = int8(math.Round(float64(x) / norm / mx * 127))
		}
		vecs[d.Index] = q
		scales[d.Index] = float32(mx / 127)
	}
	return vecs, scales, nil
}

// ---------- chunking ----------

type ragPiece struct {
	text string
	line int32
}

// splitChunks cuts text into overlapping passages on line breaks. PDF text has
// "[page N]" markers; those passages carry -N as their line.
func splitChunks(name, t string) []ragPiece {
	t = strings.ReplaceAll(t, "\r\n", "\n")
	lines := strings.Split(t, "\n")
	var out []ragPiece
	var cur strings.Builder
	start, page := 1, 0
	curPage := 0
	head := "File: " + name + "\n"
	flush := func(next int) {
		s := strings.TrimSpace(cur.String())
		if len([]rune(s)) >= 20 {
			ln := int32(start)
			if curPage > 0 {
				ln = int32(-curPage)
			}
			out = append(out, ragPiece{head + s, ln})
		}
		// keep the end of this passage as the start of the next
		tail := cur.String()
		cur.Reset()
		if len(tail) > ragOverlap {
			tail = tail[len(tail)-ragOverlap:]
			if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
				tail = tail[i+1:]
			}
		}
		cur.WriteString(tail)
		start = next
		curPage = page
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "[page ") && strings.HasSuffix(l, "]") {
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(l, "[page "), "]")); err == nil {
				page = n
				if cur.Len() == 0 || strings.TrimSpace(cur.String()) == "" {
					curPage = n
				}
				continue
			}
		}
		for len(l) > ragChunk { // very long lines (minified text, CSV)
			cur.WriteString(l[:ragChunk])
			l = l[ragChunk:]
			flush(i + 1)
		}
		cur.WriteString(l)
		cur.WriteByte('\n')
		if cur.Len() >= ragChunk {
			flush(i + 2)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		flush(len(lines))
	}
	return out
}

// ---------- update (a background job) ----------

type ragRequest struct {
	Paths   []string `json:"paths,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
	CPU     bool     `json:"cpu,omitempty"`
	Confirm bool     `json:"confirm,omitempty"` // allow dropping over half the index
	Busy    string   `json:"busy,omitempty"`    // set when the CPU was chosen because the GPU is busy
}

func runRagUpdate(j *Job) error {
	req := j.Spec.Rag
	c := loadRagConfig()
	roots := c.Roots
	partial := len(req.Paths) > 0
	if partial {
		roots = nil
		for _, p := range req.Paths {
			ep, err := expandPath(p)
			if err != nil {
				return err
			}
			roots = append(roots, ep)
		}
	}
	if !partial && !req.Confirm {
		if err := ragRootsTempCheck(roots, ragDir()); err != nil {
			return err
		}
	}
	j.setCurrent("listing files on " + strings.Join(roots, ", ") + "…")
	cands := ragWalk(j.ctx, roots, ragExclusions(c))
	if j.ctx.Err() != nil {
		return errCanceled
	}
	old, _ := readRagIndex()
	oldBy := map[string]int{}
	if old != nil {
		for i, f := range old.meta.Files {
			oldBy[normCase(f.Path)] = i
		}
	}
	inScope := func(p string) bool {
		for _, r := range roots {
			if hasPathPrefix(p, r) || samePath(p, r) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	candBy := map[string]ragCand{}
	for _, cd := range cands {
		candBy[normCase(cd.path)] = cd
	}
	var todo []ragCand
	kept, removed := 0, 0
	var bytesTodo int64
	for _, cd := range cands {
		k := normCase(cd.path)
		seen[k] = true
		if i, ok := oldBy[k]; ok && old.meta.Files[i].Size == cd.size && old.meta.Files[i].MTime == cd.mtime {
			kept++
			continue
		}
		todo = append(todo, cd)
		bytesTodo += cd.size
	}
	if old != nil {
		for _, f := range old.meta.Files {
			k := normCase(f.Path)
			if !seen[k] && (!partial || inScope(f.Path)) {
				removed++
			}
		}
	}
	res := j.newStep("index")
	res.mu.Lock()
	res.Counts = map[string]int{"files": len(cands), "unchanged": kept, "to index": len(todo), "removed": removed}
	res.Order = []string{"files", "unchanged", "to index", "removed"}
	res.mu.Unlock()
	oldN := 0
	if old != nil {
		oldN = len(old.meta.Files)
	}
	mass := ragMassRemoval(removed, oldN)
	if req.DryRun {
		out := ragPlanText(cands, todo, bytesTodo)
		if mass {
			out = ragMassRemovalText(removed, oldN, c.Roots, true) + "\n\n" + out
		}
		j.setOutput(out)
		return nil
	}
	if mass && !req.Confirm {
		j.setOutput(ragMassRemovalText(removed, oldN, c.Roots, false))
		return nil
	}
	if req.Busy != "" && bytesTodo > 100<<20 {
		// Too big for the CPU (hours or days): the user decides.
		j.setOutput(fmt.Sprintf("WARNING: NOT started. The GPU is busy (%s) and %s of files is too much for the CPU (it would take very long). Tell the user and let them choose: free the GPU and run update again, GPU anyway (force:true, may slow or disturb that work), CPU anyway (cpu:true, very slow), or a smaller part first (paths).", req.Busy, hb(bytesTodo)))
		return nil
	}
	if len(todo) == 0 && removed == 0 && old != nil {
		j.setOutput("Index is up to date.")
		return nil
	}
	release, err := embedSrv.acquire(j.ctx, req.CPU, true, j.setCurrent)
	if err != nil {
		return err
	}
	defer release()

	w, err := newRagWriter(c.Model)
	if err != nil {
		return err
	}
	defer w.close()
	// Carry over unchanged files (and, for a partial update, everything outside its folders).
	if old != nil {
		old.text, _ = os.Open(filepath.Join(ragDir(), "text.bin"))
		j.setCurrent("keeping unchanged files…")
		for _, f := range old.meta.Files {
			k := normCase(f.Path)
			keep := partial && !inScope(f.Path)
			if cd, ok := candBy[k]; ok && cd.size == f.Size && cd.mtime == f.MTime {
				keep = true // unchanged
			}
			if !keep {
				continue
			}
			fi := w.addFile(f)
			for ci := f.First; ci < f.First+f.N; ci++ {
				d := old.meta.Dim
				v := old.vecs[int(ci)*d : int(ci)*d+d]
				w.addChunk(fi, old.meta.Line[ci], old.chunkText(int(ci)), v, old.scale[ci])
			}
		}
	}
	if old != nil && old.text != nil {
		old.text.Close() // done with the old passages; the file can be replaced now
		old.text = nil
	}
	j.addTotals(int64(len(todo)), bytesTodo)

	// Read files on several workers, embed in batches on one.
	type doc struct {
		cd     ragCand
		pieces []ragPiece
	}
	docs := make(chan doc, 64)
	paths := make(chan ragCand)
	var wg sync.WaitGroup
	var noText atomic.Int64
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cd := range paths {
				t, err := extractTextOpt(cd.path, 4<<20, textOpts{ctx: j.ctx})
				var ps []ragPiece
				if err == nil {
					ps = splitChunks(filepath.Base(cd.path), t)
				}
				if len(ps) == 0 {
					noText.Add(1)
				}
				select {
				case docs <- doc{cd, ps}:
				case <-j.ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(paths)
		for _, cd := range todo {
			select {
			case paths <- cd:
			case <-j.ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(docs) }()

	type pend struct {
		fi    int32
		piece ragPiece
	}
	var batch []pend
	chunks := 0
	lastSave := time.Now()
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		texts := make([]string, len(batch))
		for i, p := range batch {
			texts[i] = p.piece.text
		}
		var vecs [][]int8
		var scales []float32
		var err error
		for try := 0; try < 4; try++ {
			if vecs, scales, err = embed(j.ctx, texts); err == nil {
				break
			}
			if j.ctx.Err() != nil {
				return errCanceled
			}
			time.Sleep(2 * time.Second)
			if !embedHealthy() { // the model stopped (e.g. another model took the VRAM): start it again
				if rerr := embedSrv.restart(j.ctx, req.CPU, j.setCurrent); rerr != nil {
					return rerr
				}
			}
		}
		if err != nil {
			return err
		}
		for i, p := range batch {
			w.addChunk(p.fi, p.piece.line, p.piece.text, vecs[i], scales[i])
		}
		chunks += len(batch)
		batch = batch[:0]
		return nil
	}
	// Passages are embedded in batches of 64 across files; a file counts as
	// indexed only once all its passages are in.
	pendFrom := -1 // first file whose passages aren't all embedded yet
	for d := range docs {
		if j.ctx.Err() != nil {
			break
		}
		fi := w.addFile(ragFile{Path: d.cd.path, Size: d.cd.size, MTime: d.cd.mtime})
		if pendFrom < 0 {
			pendFrom = int(fi)
		}
		for _, p := range d.pieces {
			batch = append(batch, pend{fi, p})
			if len(batch) >= 64 {
				if err := flushBatch(); err != nil {
					w.truncate(pendFrom)
					w.commit()
					return err
				}
				pendFrom = int(fi) // earlier files are complete
			}
		}
		if len(batch) == 0 {
			pendFrom = -1
		}
		j.fileDone()
		j.addBytes(d.cd.size)
		j.setCurrent(fmt.Sprintf("%d passages so far; %s", chunks, filepath.Base(d.cd.path)))
		if time.Since(lastSave) > 10*time.Minute { // checkpoint: an interrupted update keeps its progress
			if err := flushBatch(); err != nil {
				w.truncate(pendFrom)
				w.commit()
				return err
			}
			pendFrom = -1
			if err := w.commit(); err != nil {
				return err
			}
			lastSave = time.Now()
		}
	}
	if j.ctx.Err() == nil {
		if err := flushBatch(); err != nil {
			w.truncate(pendFrom)
			w.commit()
			return err
		}
		pendFrom = -1
	}
	if j.ctx.Err() != nil {
		if pendFrom >= 0 {
			w.truncate(pendFrom)
		}
		w.commit() // keep what was done
		return errCanceled
	}
	if err := w.commit(); err != nil {
		return err
	}
	c.Updated = time.Now().Format("2006-01-02 15:04")
	saveRagConfig(c)
	j.setOutput(fmt.Sprintf("Index: %d files, %d passages (%d new passages; %d files had no text, e.g. scans).", len(w.meta.Files), len(w.meta.FileOf), chunks, noText.Load()))
	return nil
}

func ragPlanText(cands, todo []ragCand, bytesTodo int64) string {
	type agg struct {
		n     int
		bytes int64
	}
	byFolder := map[string]*agg{}
	byExt := map[string]*agg{}
	for _, cd := range todo {
		parts := strings.Split(cd.path, string(filepath.Separator))
		key := strings.Join(parts[:min(len(parts)-1, 3)], string(filepath.Separator))
		if byFolder[key] == nil {
			byFolder[key] = &agg{}
		}
		byFolder[key].n++
		byFolder[key].bytes += cd.size
		e := strings.ToLower(filepath.Ext(cd.path))
		if byExt[e] == nil {
			byExt[e] = &agg{}
		}
		byExt[e].n++
		byExt[e].bytes += cd.size
	}
	top := func(m map[string]*agg, n int) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool { return m[keys[a]].n > m[keys[b]].n })
		var out []string
		for i, k := range keys {
			if i >= n {
				out = append(out, fmt.Sprintf("+%d more", len(keys)-n))
				break
			}
			out = append(out, fmt.Sprintf("%s %s files (%s)", k, hc(int64(m[k].n)), hb(m[k].bytes)))
		}
		return strings.Join(out, "\n  ")
	}
	est := bytesTodo / 1500 // rough: documents are partly markup
	return fmt.Sprintf("Dry run: %s files would be indexed (%s, roughly %s passages). Biggest folders:\n  %s\nTypes:\n  %s\nExclude folders with rag config exclude (a path, or a folder name to skip everywhere).",
		hc(int64(len(todo))), hb(bytesTodo), hc(est), top(byFolder, 15), top(byExt, 12))
}

// ---------- search ----------

type ragHit struct {
	score float32
	idx   int
	note  *ragNote
}

// ragAutoCPU: the embedding model goes on the CPU when ComfyUI or training holds
// the GPU, so nothing clashes and nobody has to be asked. force keeps the GPU.
func ragAutoCPU(force, cpu bool) (bool, string) {
	return ragAutoCPUFor(force, cpu, false)
}

// big: an index update, which always checks the GPU (a model already loaded
// for searches may be the CPU one).
func ragAutoCPUFor(force, cpu, big bool) (bool, string) {
	if force || cpu {
		return cpu, ""
	}
	embedSrv.mu.Lock()
	onGPU := embedSrv.proc != nil && !embedSrv.cpu
	running := embedSrv.proc != nil
	embedSrv.mu.Unlock()
	if !big && (running || embedHealthy()) {
		return false, ""
	}
	if onGPU {
		return false, ""
	}
	return ragGPUBlocked()
}

// ragGPUBlocked: the embedding model (~1 GB) stays off the GPU while ComfyUI or
// LoRA training runs (their VRAM use jumps around), or when too little VRAM is free.
// A loaded LLM is fine as long as there is room next to it.
func ragGPUBlocked() (bool, string) {
	if ps, err := procs(); err == nil {
		for _, p := range ps {
			n, c := strings.ToLower(p.name), strings.ToLower(p.cmd)
			if strings.Contains(c, "contextsaver") || strings.Contains(c, "tokensaver") || strings.Contains(c, "comfy-mcp") {
				continue
			}
			for _, a := range gpuApps {
				if a.match(n, c) {
					return true, a.what + " is running"
				}
			}
		}
	}
	out, err := runQuiet(15*time.Second, "nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return false, ""
	}
	free, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0]), 64)
	if err == nil && free < 2048 {
		return true, fmt.Sprintf("only %.1f GB of VRAM is free", free/1024)
	}
	return false, ""
}

func queryVector(ctx context.Context, q string, cpu bool) ([]float32, error) {
	release, err := embedSrv.acquire(ctx, cpu, false, func(string) {})
	if err != nil {
		return nil, err
	}
	defer release()
	v, s, err := embed(ctx, []string{ragQueryInst + q})
	if err != nil {
		return nil, err
	}
	out := make([]float32, ragDim)
	for i, x := range v[0] {
		out[i] = float32(x) * s[0]
	}
	return out, nil
}

func dotI8(q []float32, v []int8) float32 {
	var s float32
	for i := range v {
		s += q[i] * float32(v[i])
	}
	return s
}

func queryWords(q string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		if len([]rune(w)) >= 3 {
			out = append(out, w)
		}
	}
	return out
}

type ragSearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k"`
	Under string `json:"under"`
	Ext   string `json:"ext"`
	Scope string `json:"scope"` // all, files, notes
	Tag   string `json:"tag"`
	Force bool   `json:"force"`
	CPU   bool   `json:"cpu"`
}

func ragSearch(a ragSearchArgs) (string, error) {
	if strings.TrimSpace(a.Query) == "" {
		return "", errors.New("give query")
	}
	k := a.K
	if k <= 0 {
		k = 8
	}
	k = min(k, 30)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	a.CPU, _ = ragAutoCPU(a.Force, a.CPU)
	qv, err := queryVector(ctx, a.Query, a.CPU)
	if err != nil {
		return "", err
	}
	words := queryWords(a.Query)
	var hits []ragHit
	scope := strings.ToLower(a.Scope)
	if scope == "" {
		scope = "all"
	}
	var x *ragIndex
	if scope != "notes" {
		x, err = openRagIndex()
		if err != nil && scope == "files" {
			return "", err
		}
	}
	if x != nil {
		under := ""
		if a.Under != "" {
			if under, err = expandPath(a.Under); err != nil {
				return "", err
			}
		}
		exts := map[string]bool{}
		for _, e := range strings.FieldsFunc(strings.ToLower(a.Ext), func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
			exts["."+strings.TrimPrefix(e, ".")] = true
		}
		okFile := make([]bool, len(x.meta.Files))
		for i, f := range x.meta.Files {
			okFile[i] = (under == "" || hasPathPrefix(f.Path, under) || samePath(f.Path, under)) &&
				(len(exts) == 0 || exts[strings.ToLower(filepath.Ext(f.Path))])
		}
		// Score every passage on a few goroutines; keep the best 200 for re-ranking.
		n, d := x.n(), x.meta.Dim
		const keep = 200
		parts := 8
		results := make([][]ragHit, parts)
		var wg sync.WaitGroup
		for p := 0; p < parts; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				var best []ragHit
				for i := p; i < n; i += parts {
					if !okFile[x.meta.FileOf[i]] {
						continue
					}
					s := dotI8(qv[:d], x.vecs[i*d:i*d+d]) * x.scale[i]
					if len(best) < keep {
						best = append(best, ragHit{score: s, idx: i})
						if len(best) == keep {
							sort.Slice(best, func(a, b int) bool { return best[a].score > best[b].score })
						}
					} else if s > best[keep-1].score {
						best[keep-1] = ragHit{score: s, idx: i}
						for j := keep - 1; j > 0 && best[j].score > best[j-1].score; j-- {
							best[j], best[j-1] = best[j-1], best[j]
						}
					}
				}
				results[p] = best
			}(p)
		}
		wg.Wait()
		var cand []ragHit
		for _, r := range results {
			cand = append(cand, r...)
		}
		sort.Slice(cand, func(a, b int) bool { return cand[a].score > cand[b].score })
		if len(cand) > keep {
			cand = cand[:keep]
		}
		// Small bonus for passages that contain the query's words.
		for i := range cand {
			if len(words) == 0 {
				break
			}
			t := strings.ToLower(x.chunkText(cand[i].idx))
			hit := 0
			for _, w := range words {
				if strings.Contains(t, w) {
					hit++
				}
			}
			cand[i].score += 0.06 * float32(hit) / float32(len(words))
		}
		hits = append(hits, cand...)
	}
	if scope != "files" {
		for _, nh := range searchNotes(qv, words, a.Tag) {
			hits = append(hits, nh)
		}
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	// At most 2 passages per file, so one big file doesn't fill the results.
	var b strings.Builder
	perFile := map[int32]int{}
	shown := 0
	for _, h := range hits {
		if shown >= k {
			break
		}
		if h.note != nil {
			fmt.Fprintf(&b, "[note %s, %s%s] %.2f\n  %s\n", h.note.ID, h.note.Created[:10], tagList(h.note.Tags), h.score, clip(squashWS(h.note.Text), 600))
			shown++
			continue
		}
		fi := x.meta.FileOf[h.idx]
		if perFile[fi] >= 2 {
			continue
		}
		perFile[fi]++
		f := x.meta.Files[fi]
		loc := fmt.Sprintf("line %d", x.meta.Line[h.idx])
		if x.meta.Line[h.idx] < 0 {
			loc = fmt.Sprintf("page %d", -x.meta.Line[h.idx])
		}
		t := x.chunkText(h.idx)
		if i := strings.IndexByte(t, '\n'); i >= 0 && strings.HasPrefix(t, "File: ") {
			t = t[i+1:]
		}
		fmt.Fprintf(&b, "%s (%s) %.2f\n  %s\n", f.Path, loc, h.score, clip(squashWS(t), 420))
		shown++
	}
	if shown == 0 {
		if x == nil && scope != "notes" {
			return "No notes match, and there is no file index yet (rag update builds it).", nil
		}
		return "Nothing found.", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func squashWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func tagList(t []string) string {
	if len(t) == 0 {
		return ""
	}
	return " #" + strings.Join(t, " #")
}

// ---------- memory notes ----------

type ragNote struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Tags    []string `json:"tags,omitempty"`
	Created string   `json:"created"`
	Vec     []int8   `json:"vec"`
	Scale   float32  `json:"scale"`
}

var notesMu sync.Mutex

func notesPath() string { return filepath.Join(ragDir(), "notes.jsonl") }

func loadNotes() []ragNote {
	f, err := os.Open(notesPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ragNote
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var n ragNote
		if json.Unmarshal(sc.Bytes(), &n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func saveNotes(ns []ragNote) error {
	var b bytes.Buffer
	for _, n := range ns {
		x, _ := json.Marshal(n)
		b.Write(x)
		b.WriteByte('\n')
	}
	tmp := notesPath() + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, notesPath())
}

func searchNotes(qv []float32, words []string, tag string) []ragHit {
	notesMu.Lock()
	ns := loadNotes()
	notesMu.Unlock()
	var out []ragHit
	for i := range ns {
		n := &ns[i]
		if tag != "" && !containsFold(n.Tags, tag) || len(n.Vec) != ragDim {
			continue
		}
		s := dotI8(qv, n.Vec) * n.Scale
		t := strings.ToLower(n.Text)
		hit := 0
		for _, w := range words {
			if strings.Contains(t, w) {
				hit++
			}
		}
		if len(words) > 0 {
			s += 0.06 * float32(hit) / float32(len(words))
		}
		out = append(out, ragHit{score: s + 0.03, note: n}) // notes are written for this: a slight edge
	}
	return out
}

func ragRemember(text string, tags []string, force, cpu bool) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("give text to remember")
	}
	cpu, _ = ragAutoCPU(force, cpu)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	release, err := embedSrv.acquire(ctx, cpu, false, func(string) {})
	if err != nil {
		return "", err
	}
	defer release()
	tagLine := ""
	if len(tags) > 0 {
		tagLine = "\nTags: " + strings.Join(tags, ", ")
	}
	v, s, err := embed(ctx, []string{"Note: " + text + tagLine})
	if err != nil {
		return "", err
	}
	notesMu.Lock()
	defer notesMu.Unlock()
	os.MkdirAll(ragDir(), 0o755)
	n := ragNote{ID: time.Now().Format("060102") + "-" + randHex(2), Text: text, Tags: tags, Created: time.Now().Format("2006-01-02 15:04"), Vec: v[0], Scale: s[0]}
	f, err := os.OpenFile(notesPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	x, _ := json.Marshal(n)
	if _, err := f.Write(append(x, '\n')); err != nil {
		return "", err
	}
	return "remembered as note " + n.ID, nil
}

// ragForget moves notes to notes-forgotten.jsonl (kept, not deleted).
func ragForget(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", errors.New("give ids")
	}
	notesMu.Lock()
	defer notesMu.Unlock()
	ns := loadNotes()
	var keep, gone []ragNote
	for _, n := range ns {
		if containsFold(ids, n.ID) {
			gone = append(gone, n)
		} else {
			keep = append(keep, n)
		}
	}
	if len(gone) == 0 {
		return "", errors.New("no note has those ids")
	}
	f, err := os.OpenFile(filepath.Join(ragDir(), "notes-forgotten.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	for _, n := range gone {
		x, _ := json.Marshal(n)
		f.Write(append(x, '\n'))
	}
	f.Close()
	if err := saveNotes(keep); err != nil {
		return "", err
	}
	return fmt.Sprintf("forgot %d note(s) (moved to notes-forgotten.jsonl)", len(gone)), nil
}

func ragNotesList(tag string, limit int) string {
	notesMu.Lock()
	ns := loadNotes()
	notesMu.Unlock()
	if limit <= 0 {
		limit = 30
	}
	var b strings.Builder
	shown := 0
	for i := len(ns) - 1; i >= 0 && shown < limit; i-- {
		n := ns[i]
		if tag != "" && !containsFold(n.Tags, tag) {
			continue
		}
		fmt.Fprintf(&b, "[%s %s%s] %s\n", n.ID, n.Created, tagList(n.Tags), clip(squashWS(n.Text), 300))
		shown++
	}
	if shown == 0 {
		return "No notes yet."
	}
	return fmt.Sprintf("%d notes (newest first):\n", len(ns)) + strings.TrimRight(b.String(), "\n")
}

// ragGPUWarning: loading the embedding model while ComfyUI or LoRA training uses
// the GPU is the user's call. Nothing is loaded when the model is already up.

// ---------- tool ----------

func toolRag(raw json.RawMessage) (string, error) {
	var a struct {
		ragSearchArgs
		Action  string   `json:"action"`
		Text    string   `json:"text"`
		Tags    []string `json:"tags"`
		IDs     []string `json:"ids"`
		Paths   []string `json:"paths"`
		Roots   []string `json:"roots"`
		Exclude []string `json:"exclude"`
		Include []string `json:"include"`
		Types   []string `json:"types"`
		Limit   int      `json:"limit"`
		DryRun  bool     `json:"dry_run"`
		Confirm bool     `json:"confirm"`
		Notify  bool     `json:"notify"`
		Wait    *int     `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	a.ragSearchArgs.CPU = a.CPU
	switch a.Action {
	case "search":
		return ragSearch(a.ragSearchArgs)
	case "remember":
		return ragRemember(a.Text, a.Tags, a.Force, a.CPU)
	case "notes":
		return ragNotesList(a.Tag, a.Limit), nil
	case "forget":
		return ragForget(a.IDs)
	case "status":
		return ragStatus(), nil
	case "config":
		c := loadRagConfig()
		if len(a.Roots) > 0 {
			if !a.Confirm {
				if err := ragRootsReplaceCheck(ragSavedRoots(), a.Roots); err != nil {
					return "", err
				}
			}
			c.Roots = a.Roots
		}
		if a.Types != nil {
			c.Types = a.Types
		}
		for _, e := range a.Exclude {
			if !containsFold(c.Exclude, e) {
				c.Exclude = append(c.Exclude, e)
			}
		}
		for _, e := range a.Include { // undo an exclude
			var keep []string
			for _, x := range c.Exclude {
				if !strings.EqualFold(x, e) {
					keep = append(keep, x)
				}
			}
			c.Exclude = keep
		}
		if err := saveRagConfigOpt(c, a.Confirm); err != nil {
			return "", err
		}
		return ragStatus(), nil
	case "update":
		if !a.DryRun {
			if other := runningJobsOfKind(otherJobDirs(), "rag"); len(other) > 0 {
				return "", errors.New("an index update is already running in TokenSaver; the index is shared, so wait for it to finish (or cancel it there) and try again")
			}
		}
		cpuNote, busyNote := "", ""
		if !a.DryRun {
			var busy string
			if a.CPU, busy = ragAutoCPUFor(a.Force, a.CPU, true); busy != "" {
				cpuNote, busyNote = "Indexing on the CPU (slower, no clash) because the GPU is busy: "+busy+"\n", busy
			}
		}
		j, err := newJob(JobSpec{Name: "rag update", Kind: "rag", DryRun: a.DryRun, Source: "chat", Rag: &ragRequest{Paths: a.Paths, DryRun: a.DryRun, CPU: a.CPU, Busy: busyNote, Confirm: a.Confirm}})
		if err != nil {
			return "", err
		}
		j.start()
		waitJob(j, waitSecs(a.Wait, 40, 50))
		s := j.snapshot()
		return cpuNote + withNotify(formatState(&s), &s, a.Notify), nil
	}
	return "", errors.New("action must be search, remember, notes, forget, update, status or config")
}

func ragStatus() string {
	c := loadRagConfig()
	var b strings.Builder
	fmt.Fprintf(&b, "Folders: %s\n", strings.Join(c.Roots, ", "))
	if len(c.Exclude) > 0 {
		fmt.Fprintf(&b, "Also excluded: %s\n", strings.Join(c.Exclude, ", "))
	}
	if len(c.Types) > 0 {
		fmt.Fprintf(&b, "Only types: %s\n", strings.Join(c.Types, " "))
	}
	fmt.Fprintf(&b, "Model: %s\n", c.Model)
	if f, err := os.Open(ragMetaPath()); err == nil {
		var m ragMeta
		if gob.NewDecoder(bufio.NewReader(f)).Decode(&m) == nil {
			var size int64
			for _, s := range []string{"index.gob", "vectors.bin", "text.bin"} {
				if fi, err := os.Stat(filepath.Join(ragDir(), s)); err == nil {
					size += fi.Size()
				}
			}
			fmt.Fprintf(&b, "Index: %s files, %s passages, %s, updated %s\n", hc(int64(len(m.Files))), hc(int64(len(m.FileOf))), hb(size), m.Updated.Format("2006-01-02 15:04"))
		}
		f.Close()
	} else {
		b.WriteString("Index: none yet\n")
	}
	fmt.Fprintf(&b, "Notes: %d", len(loadNotes()))
	return b.String()
}
