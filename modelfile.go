package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The models ContextSaver can switch between live in data/models.json, so a new model
// needs no new build. The file is read on every use, so edits apply at once. An entry
// names either a start script (a Windows .bat from D: is converted), or a .gguf file plus
// llama-server settings; ContextSaver writes the Linux start script for both
// (data/models/<name>.sh), so everything downstream (port, alias, ownership, idle stop)
// works the same for every kind.

const defaultLlamaServer = `D:\llama.cpp\llama-server.exe`

type modelEntry struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Script        string   `json:"script,omitempty"`
	GGUF          string   `json:"gguf,omitempty"`
	LlamaServer   string   `json:"llama_server,omitempty"`
	Alias         string   `json:"alias,omitempty"`
	Host          string   `json:"host,omitempty"`
	Port          int      `json:"port,omitempty"`
	CtxSize       int      `json:"ctx_size,omitempty"`
	NGL           *int     `json:"ngl,omitempty"`
	FlashAttn     *bool    `json:"flash_attn,omitempty"`
	CacheType     string   `json:"cache_type,omitempty"`
	Temp          *float64 `json:"temp,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	MinP          *float64 `json:"min_p,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
	ExtraArgs     []string `json:"extra_args,omitempty"`
	LMStudio      string   `json:"lmstudio,omitempty"`
}

// defaultModelInfo is what the first models.json gets: the models built in until 1.17.
var defaultModelInfo = []modelEntry{
	{Name: "qwen", Description: "Qwen3.6 27B, thinking off", LMStudio: "qwen3.6-27b"},
	{Name: "qwen-think", Description: "Qwen3.6 27B, thinking on", LMStudio: "qwen3.6-27b"},
	{Name: "oss", Description: "gpt-oss 120B", LMStudio: "gpt-oss-120b"},
	{Name: "oss-fast", Description: "gpt-oss 120B (same script as oss: EAGLE3 measured slower)", LMStudio: "gpt-oss-120b"},
	{Name: "oss-free", Description: "gpt-oss 120B derestricted", LMStudio: "gpt-oss-120b-derestricted"},
	{Name: "gemma", Description: "Gemma 4 31B", LMStudio: "gemma-4-31b-it"},
}

type modelSet struct {
	order    []string              // usable models, in file order
	entries  map[string]modelEntry // usable models
	scripts  map[string]string     // name -> start script (made by ContextSaver for gguf entries)
	gen      map[string]string     // gguf entries: the start script's text
	server   map[string]string     // gguf entries: llama-server
	bat      map[string]string     // Windows .bat entries: the .bat converted into scripts[name]
	bad      map[string]string     // name -> what is wrong with its entry
	listed   []string              // every valid name in the file, usable or not (shortcuts)
	problems []string
}

var modelCache struct {
	sync.Mutex
	path string
	data []byte
	set  *modelSet
	err  error
}

func modelsPath() string { return filepath.Join(appHome, "models.json") }

func genScriptDir() string {
	if appHome == "" {
		return filepath.Join(os.TempDir(), "contextsaver-models")
	}
	return filepath.Join(appHome, "models")
}

// loadModels reads models.json, first creating it with the built-in models.
func loadModels() (*modelSet, error) {
	var b []byte
	p := ""
	if appHome == "" {
		b = seedModelFile()
	} else {
		p = modelsPath()
		var err error
		b, err = os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			b = seedModelFile()
			os.MkdirAll(appHome, 0o755)
			os.WriteFile(p, b, 0o644)
		} else if err != nil {
			return nil, fmt.Errorf("can't read %s: %v", p, err)
		}
	}
	modelCache.Lock()
	defer modelCache.Unlock()
	if (modelCache.set != nil || modelCache.err != nil) && modelCache.path == p && bytes.Equal(modelCache.data, b) {
		return modelCache.set, modelCache.err
	}
	set, err := parseModelFile(b, genScriptDir())
	if err != nil {
		name := p
		if name == "" {
			name = "models.json"
		}
		err = fmt.Errorf("%s: %v", name, err)
	}
	modelCache.path, modelCache.data, modelCache.set, modelCache.err = p, b, set, err
	return set, err
}

// seedModelFile: an older config.json "models" list if there is one, else the Windows
// ContextSaver's models.json on the Windows partition, else the built-ins.
func seedModelFile() []byte {
	if len(cfg.Models) == 0 && appHome != "" {
		if p, err := winToLinux(`D:\Claude Apps\ContextSaver\data\models.json`); err == nil {
			if b, err := os.ReadFile(p); err == nil {
				if _, err := parseModelFile(b, filepath.Join(os.TempDir(), "contextsaver-seed")); err == nil {
					return b
				}
			}
		}
	}
	var es []modelEntry
	if len(cfg.Models) > 0 {
		var ns []string
		for n := range cfg.Models {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		for _, n := range ns {
			es = append(es, modelEntry{Name: n, Script: cfg.Models[n]})
		}
	} else {
		for _, e := range defaultModelInfo {
			e.Script = defaultModels[e.Name]
			es = append(es, e)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		LlamaServer string       `json:"llama_server"`
		Models      []modelEntry `json:"models"`
	}{defaultLlamaServer, es})
	return buf.Bytes()
}

func parseModelFile(b []byte, dir string) (*modelSet, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var f struct {
		LlamaServer string            `json:"llama_server"`
		Models      []json.RawMessage `json:"models"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New(jsonProblem(b, err, true))
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("there is more text after the closing }")
	}
	if len(f.Models) == 0 {
		return nil, errors.New(`no models: the "models" list is missing or empty`)
	}
	set := &modelSet{entries: map[string]modelEntry{}, scripts: map[string]string{}, gen: map[string]string{},
		server: map[string]string{}, bad: map[string]string{}, bat: map[string]string{}}
	first := map[string]int{}
	dups := map[string]bool{}
	for i, raw := range f.Models {
		var e modelEntry
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		derr := d.Decode(&e)
		if derr != nil {
			var n struct {
				Name string `json:"name"`
			}
			json.Unmarshal(raw, &n)
			e.Name = n.Name
		}
		e.Name = strings.ToLower(strings.TrimSpace(e.Name))
		label := fmt.Sprintf("entry #%d", i+1)
		if e.Name != "" {
			label += " (" + e.Name + ")"
		}
		named := modelNameErr(e.Name) == ""
		if named {
			if j, ok := first[e.Name]; ok {
				msg := fmt.Sprintf("entries #%d and #%d are both named %q; each name must be unique", j, i+1, e.Name)
				set.problems = append(set.problems, msg)
				set.bad[e.Name] = msg
				dups[e.Name] = true
				continue
			}
			first[e.Name] = i + 1
			set.listed = append(set.listed, e.Name)
		}
		msg := ""
		if derr != nil {
			msg = jsonProblem(raw, derr, false)
		} else {
			msg = checkModelEntry(e)
		}
		if msg != "" {
			set.problems = append(set.problems, label+": "+msg)
			if named {
				set.bad[e.Name] = msg
			}
			continue
		}
		if e.GGUF != "" {
			e.GGUF = hostPath(cleanModelPath(e.GGUF))
			srv := e.LlamaServer
			if srv == "" {
				srv = f.LlamaServer
			}
			srv = linuxLlamaServer(hostPath(cleanModelPath(srv)))
			txt := genModelScript(e, srv)
			p := filepath.Join(dir, e.Name+".sh")
			set.gen[e.Name], set.server[e.Name], set.scripts[e.Name] = txt, srv, p
			writeGenScript(p, txt)
		} else if e.Script = cleanModelPath(e.Script); isBatScript(e.Script) {
			p := filepath.Join(dir, e.Name+".sh")
			set.bat[e.Name], set.scripts[e.Name] = e.Script, p
			writeBatScript(e.Script, p) // when its drive is mounted; resolveModel says why not
		} else {
			e.Script = hostPath(e.Script)
			set.scripts[e.Name] = e.Script
		}
		set.entries[e.Name] = e
		set.order = append(set.order, e.Name)
	}
	if len(dups) > 0 {
		var keep []string
		for _, n := range set.order {
			if dups[n] {
				delete(set.entries, n)
				delete(set.scripts, n)
				delete(set.gen, n)
				delete(set.server, n)
				continue
			}
			keep = append(keep, n)
		}
		set.order = keep
	}
	// start scripts of entries that are gone or broken now
	if es, err := os.ReadDir(dir); err == nil {
		for _, de := range es {
			n := de.Name()
			if filepath.Ext(n) != ".sh" {
				continue
			}
			k := strings.TrimSuffix(n, ".sh")
			if _, ok := set.gen[k]; ok {
				continue
			}
			if _, ok := set.bat[k]; ok {
				continue
			}
			p := filepath.Join(dir, n)
			if old, err := os.ReadFile(p); err == nil && bytes.Contains(old, []byte(genMarker)) {
				os.Remove(p)
			}
		}
	}
	return set, nil
}

var modelNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]*$`)

var reservedModelNames = map[string]bool{"list": true, "status": true, "stop": true, "pin": true,
	"unpin": true, "idle": true, "shortcuts": true, "unload": true}

func modelNameErr(n string) string {
	switch {
	case n == "":
		return `has no "name"`
	case !modelNameRe.MatchString(n):
		return fmt.Sprintf("name %q can only have letters, digits and . _ + - (no spaces)", n)
	case reservedModelNames[n]:
		return fmt.Sprintf("name %q is taken by the command \"contextsaver model %s\"", n, n)
	}
	return ""
}

var cacheTypes = map[string]bool{"f32": true, "f16": true, "bf16": true, "q8_0": true, "q4_0": true,
	"q4_1": true, "iq4_nl": true, "q5_0": true, "q5_1": true}

// ownArgs are llama-server flags that have their own setting in an entry.
var ownArgs = map[string]string{"-m": "gguf", "--model": "gguf", "--port": "port", "--host": "host",
	"--alias": "alias", "-c": "ctx_size", "--ctx-size": "ctx_size", "-ngl": "ngl", "--gpu-layers": "ngl",
	"--n-gpu-layers": "ngl", "-fa": "flash_attn", "--flash-attn": "flash_attn", "-ctk": "cache_type",
	"--cache-type-k": "cache_type", "-ctv": "cache_type", "--cache-type-v": "cache_type"}

func checkModelEntry(e modelEntry) string {
	if m := modelNameErr(e.Name); m != "" {
		return m
	}
	script, gguf := strings.TrimSpace(e.Script), strings.TrimSpace(e.GGUF)
	switch {
	case script != "" && gguf != "":
		return `has both "script" and "gguf"; use one`
	case script == "" && gguf == "":
		return `needs "gguf" (the path of a .gguf model file) or "script" (a start script)`
	}
	if script != "" {
		if e.LlamaServer != "" || e.Alias != "" || e.Host != "" || e.Port != 0 || e.CtxSize != 0 || e.NGL != nil ||
			e.FlashAttn != nil || e.CacheType != "" || e.Temp != nil || e.TopP != nil || e.TopK != nil ||
			e.MinP != nil || e.RepeatPenalty != nil || len(e.ExtraArgs) > 0 {
			return `llama-server settings only work with "gguf"; for a "script" entry put them in the script`
		}
		return ""
	}
	if !strings.EqualFold(filepath.Ext(gguf), ".gguf") {
		return `"gguf" must be the path of a .gguf file, not ` + gguf
	}
	if e.Port < 0 || e.Port > 65535 {
		return fmt.Sprintf(`"port" %d isn't a port (1-65535)`, e.Port)
	}
	if e.CtxSize < 0 {
		return `"ctx_size" can't be negative`
	}
	if e.CacheType != "" && !cacheTypes[strings.ToLower(e.CacheType)] {
		return fmt.Sprintf(`"cache_type" %q isn't one llama-server knows (q8_0, q4_0, f16, ...)`, e.CacheType)
	}
	for _, a := range e.ExtraArgs {
		if s, ok := ownArgs[strings.ToLower(strings.SplitN(a, "=", 2)[0])]; ok {
			return fmt.Sprintf(`"extra_args" has %s; use the %q setting instead`, a, s)
		}
	}
	return ""
}

func jsonProblem(b []byte, err error, top bool) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		line, col := lineCol(b, se.Offset)
		msg := fmt.Sprintf("invalid JSON at line %d, column %d: %s", line, col, strings.TrimPrefix(se.Error(), "json: "))
		if strings.Contains(se.Error(), "escape") {
			msg += `; in JSON write each \ in a path as \\ (or use /)`
		}
		return msg
	case errors.As(err, &te):
		if te.Field == "" {
			if top {
				return "the file must be a JSON object: { \"models\": [ ... ] }"
			}
			return "must be an object { \"name\": ..., ... }"
		}
		return fmt.Sprintf("%q must be %s (found %s)", te.Field, jsonTypeName(te.Type), te.Value)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the file is empty or stops before its closing }"
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		msg := "unknown setting " + strings.TrimPrefix(err.Error(), "json: unknown field ")
		if top {
			msg += ` (outside the models list only "llama_server" and "models" go)`
		}
		return msg
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}

func jsonTypeName(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil {
		return "something else"
	}
	switch t.Kind() {
	case reflect.String:
		return "text in quotes"
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Float64, reflect.Float32:
		return "a number"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice:
		return "a list [ ... ]"
	case reflect.Struct, reflect.Map:
		return "an object { ... }"
	}
	return t.String()
}

func lineCol(b []byte, off int64) (int, int) {
	if off > int64(len(b)) {
		off = int64(len(b))
	}
	if off < 0 {
		off = 0
	}
	pre := b[:off]
	return bytes.Count(pre, []byte("\n")) + 1, int(off) - bytes.LastIndexByte(pre, '\n')
}

func cleanModelPath(p string) string {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
	return filepath.Clean(filepath.FromSlash(p))
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// modelArgs: the llama-server arguments of a gguf entry after -m.
func modelArgs(e modelEntry) []string {
	host, port, alias := e.Host, e.Port, e.Alias
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 8080
	}
	if alias == "" {
		alias = e.Name
	}
	a := []string{"--alias", alias}
	if e.CtxSize > 0 {
		a = append(a, "--ctx-size", strconv.Itoa(e.CtxSize))
	}
	if e.NGL != nil {
		a = append(a, "-ngl", strconv.Itoa(*e.NGL))
	}
	if e.FlashAttn != nil {
		v := "off"
		if *e.FlashAttn {
			v = "on"
		}
		a = append(a, "-fa", v)
	}
	if e.CacheType != "" {
		ct := strings.ToLower(e.CacheType)
		a = append(a, "-ctk", ct, "-ctv", ct)
	}
	for _, f := range []struct {
		flag string
		v    *float64
	}{{"--temp", e.Temp}, {"--top-p", e.TopP}, {"--min-p", e.MinP}, {"--repeat-penalty", e.RepeatPenalty}} {
		if f.v != nil {
			a = append(a, f.flag, fmtNum(*f.v))
		}
	}
	if e.TopK != nil {
		a = append(a, "--top-k", strconv.Itoa(*e.TopK))
	}
	a = append(a, e.ExtraArgs...)
	return append(a, "--host", host, "--port", strconv.Itoa(port))
}

func writeGenScript(p, txt string) error {
	if old, err := os.ReadFile(p); err == nil && string(old) == txt {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(txt), 0o755)
}

// models maps names to start scripts; with an unreadable models.json the built-ins stand
// in so the rest of ContextSaver keeps working (switching is refused, see resolveModel).
func models() map[string]string {
	if set, err := loadModels(); err == nil {
		return set.scripts
	}
	if len(cfg.Models) > 0 {
		return cfg.Models
	}
	return defaultModels
}

// modelNames lists the models in models.json order.
func modelNames() []string {
	if set, err := loadModels(); err == nil {
		return append([]string(nil), set.order...)
	}
	var ns []string
	for n := range models() {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

func firstScript() string {
	if cfg.AIScript != "" {
		return cfg.AIScript
	}
	if ns := modelNames(); len(ns) > 0 {
		return models()[ns[0]]
	}
	return defaultAIScript
}

// resolveModel gives the start script for name, or says plainly what is wrong.
func resolveModel(name string) (string, error) {
	if err := modelFileCheck(name); err != nil {
		return "", err
	}
	set, _ := loadModels()
	s, ok := set.scripts[name]
	if !ok {
		return "", fmt.Errorf("unknown model %q; have: %s (the list is in %s)", name, strings.Join(set.order, ", "), modelsPath())
	}
	if e := set.entries[name]; e.GGUF != "" {
		if !exists(e.GGUF) {
			if isWinPath(e.GGUF) {
				if _, err := winToLinux(e.GGUF); err != nil {
					return "", fmt.Errorf("can't use %s: %v", name, err)
				}
			}
			return "", fmt.Errorf("can't use %s: its GGUF file isn't there: %s (fix \"gguf\" in models.json)", name, e.GGUF)
		}
		if srv := set.server[name]; !exists(srv) {
			return "", fmt.Errorf("can't use %s: %s", name, llamaServerHint(srv))
		}
		if err := writeGenScript(s, set.gen[name]); err != nil {
			return "", fmt.Errorf("can't write the start script for %s: %v", name, err)
		}
	} else if bat := set.bat[name]; bat != "" {
		if err := writeBatScript(bat, s); err != nil {
			return "", fmt.Errorf("can't use %s: %v", name, err)
		}
		if srv := linuxLlamaServer(""); !exists(srv) {
			return "", fmt.Errorf("can't use %s: %s", name, llamaServerHint(srv))
		}
	}
	return s, nil
}

func modelStatus() string {
	if lmsMode() {
		return lmsStatus()
	}
	set, _ := loadModels()
	cur, st := currentModel()
	var b strings.Builder
	for _, n := range modelNames() {
		s := models()[n]
		mark := ""
		if n == cur {
			mark = " [" + st + "]"
		}
		var e modelEntry
		if set != nil {
			e = set.entries[n]
		}
		switch {
		case e.GGUF != "" && !exists(e.GGUF):
			mark += " (GGUF missing)"
		case set != nil && set.bat[n] != "" && !exists(hostPath(set.bat[n])):
			mark += " (script missing)"
		case e.GGUF == "" && (set == nil || set.bat[n] == "") && !exists(s):
			mark += " (script missing)"
		}
		if e.Description != "" {
			mark += " - " + e.Description
		}
		fmt.Fprintf(&b, "%s%s\n", n, mark)
	}
	b.WriteString(modelFileNotes())
	if cur == "" {
		b.WriteString("nothing loaded\n")
	} else if st == "loading" {
		b.WriteString("loading " + cur + "\n")
	}
	_, port := scriptAddr(firstScript())
	b.WriteString(guardStatusLine("model server", port))
	if embedHealthy() {
		b.WriteString("\n" + guardStatusLine("embedding", ragPort))
	}
	return strings.TrimSpace(b.String())
}

// modelFileNotes: what is wrong with models.json, for model lists ("" when all is well).
func modelFileNotes() string {
	set, err := loadModels()
	if err != nil {
		return fmt.Sprintf("%v\nuntil it is fixed the built-in list is shown and switching is refused\n", err)
	}
	if len(set.problems) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("problems in models.json (not usable until fixed):\n")
	for _, p := range set.problems {
		b.WriteString("  " + p + "\n")
	}
	return b.String()
}

// modelFileCheck refuses a switch when models.json is broken or name's entry has a problem.
func modelFileCheck(name string) error {
	set, err := loadModels()
	if err != nil {
		return fmt.Errorf("not switching, the models file has a problem: %v", err)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if msg, ok := set.bad[name]; ok {
		return fmt.Errorf("can't use %s, its entry in %s has a problem: %s", name, modelsPath(), msg)
	}
	return nil
}

// modelDesc is " - <description>" for list lines.
func modelDesc(n string) string {
	if set, err := loadModels(); err == nil && set.entries[n].Description != "" {
		return " - " + set.entries[n].Description
	}
	return ""
}

// guardProcDesc names the program listening on port, for refusals.
var guardProcDesc = func(port int) string {
	pid := guardPIDOnPort(port)
	if pid <= 0 {
		return ""
	}
	if ps, err := procs(); err == nil {
		for _, p := range ps {
			if p.pid != pid {
				continue
			}
			c := strings.Join(strings.Fields(p.cmd), " ")
			if r := []rune(c); len(r) > 140 {
				c = string(r[:140]) + "..."
			}
			if c == "" {
				return fmt.Sprintf("%s, pid %d", p.name, pid)
			}
			return fmt.Sprintf("%s, pid %d: %s", p.name, pid, c)
		}
	}
	return fmt.Sprintf("pid %d", pid)
}

// consoleModel: contextsaver model [<name>|list|stop|pin|unpin|shortcuts] [--force]
func consoleModel(args []string) int {
	if len(args) == 0 || args[0] == "list" || args[0] == "status" {
		fmt.Println(modelStatus())
		if _, err := loadModels(); err != nil {
			return 1
		}
		return 0
	}
	force := len(args) > 1 && args[1] == "--force"
	switch args[0] {
	case "stop":
		fmt.Println(stopModel(force))
		return 0
	case "pin", "unpin":
		out, _ := toolModel(map[string]any{"action": args[0]})
		fmt.Println(out)
		return 0
	case "shortcuts":
		exe, err := os.Executable()
		if err != nil {
			fmt.Println(err)
			return 1
		}
		if r, e := filepath.EvalSymlinks(exe); e == nil {
			exe = r
		}
		out, err := writeModelShortcuts(exe)
		if err != nil {
			fmt.Println(err)
			return 1
		}
		fmt.Println(out)
		return 0
	}
	name := strings.ToLower(args[0])
	fmt.Println("Loading", name, "...")
	out, err := useModel(name, 600, force)
	if err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	fmt.Println(out + syncFrontends())
	return 0
}

// writeModelShortcuts makes one "Use model - <name>" launcher per model in models.json in
// the app folder and removes the ones for models no longer listed.
func writeModelShortcuts(exe string) (string, error) {
	set, err := loadModels()
	if err != nil {
		return "", fmt.Errorf("shortcuts left as they were: %v", err)
	}
	dir := appRoot()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	keep := map[string]bool{}
	for _, n := range set.listed {
		f := "Use model - " + n + ".desktop"
		keep[f] = true
		if err := writeLauncher(filepath.Join(dir, f), "Use model - "+n, "Load "+n+" as the local model", "", []string{exe, "model", n}, true, 4); err != nil {
			return "", err
		}
	}
	var removed []string
	old, _ := filepath.Glob(filepath.Join(dir, "Use model - *.desktop"))
	for _, p := range old {
		base := filepath.Base(p)
		if keep[base] {
			continue
		}
		if os.Remove(p) == nil {
			removed = append(removed, strings.TrimSuffix(strings.TrimPrefix(base, "Use model - "), ".desktop"))
		}
	}
	writeLauncher(filepath.Join(dir, "Stop model.desktop"), "Stop model", "Stop the local model", "", []string{exe, "model", "stop"}, true, 3)
	writeModelIcons(exe, set.listed)
	out := "shortcuts in " + dir + ": " + strings.Join(set.listed, ", ")
	if len(removed) > 0 {
		out += "\nremoved: " + strings.Join(removed, ", ")
	}
	if len(set.problems) > 0 {
		out += "\nsome entries have problems; see: contextsaver model list"
	}
	return out, nil
}

// lmsMapped is the LM Studio key for a model: its "lmstudio" setting, else the built-in map.
func lmsMapped(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	if set, err := loadModels(); err == nil {
		if e, ok := set.entries[n]; ok && e.LMStudio != "" {
			return e.LMStudio
		}
	}
	return lmsNames[n]
}

func lmsNameList() []string {
	seen := map[string]bool{}
	var ns []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			ns = append(ns, n)
		}
	}
	for n := range lmsNames {
		add(n)
	}
	if set, err := loadModels(); err == nil {
		for _, n := range set.order {
			if set.entries[n].LMStudio != "" {
				add(n)
			}
		}
	}
	sort.Strings(ns)
	return ns
}
