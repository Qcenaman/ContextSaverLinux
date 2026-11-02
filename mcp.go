package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

type rpcReq struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ContextSaver serves AnythingLLM's local models: one small tool, confirmations.
const localMode = true

const localInstructions = `Use the pc tool for anything on this PC; help{c} shows a command's args.
Any GitHub repo's code (his own: Qcenaman/<name>): pass owner/name[@ref] as path to find/read/outline/git compare; never fetch or web-search it.`

const serverInstructions = `ContextSaver works on this Linux PC and returns short results, so use it instead of built-in file, web-fetch, browser or computer-use tools whenever it can do the job. scan/find to look around; read for just the part you need (Office, PDF, and images or scans via OCR; image mode for a small picture); run_job for file operations on any number of files; run for commands (exit code and error lines only); system for PC state; web for search and page facts; browser for JavaScript or signed-in pages; media for audio/video as text; office to create, edit, read or convert Word, Excel, PowerPoint and PDF files; rag to search the user's files by meaning and keep or recall memory notes across chats; window to read on-screen text instead of screenshots; local_ai to have the PC's own model read, describe or sort many files and images; pc for the same in one small tool, and model to switch the local chat model. Dry-run deletes, renames and overwrites.`

func instructions() string {
	if localMode {
		return localInstructions + "\n" + aboutCard()
	}
	return serverInstructions
}

func serveMCP(in io.Reader, out io.Writer) {
	resumeNotifier()
	cleanReposSoon()
	wctx, stopWatcher := context.WithCancel(context.Background())
	startIdleWatcher(wctx)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var wmu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		out.Write(append(b, '\n'))
		wmu.Unlock()
	}
	mcpSend = send
	var wg sync.WaitGroup
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		wg.Add(1)
		go func(req rpcReq) {
			defer wg.Done()
			send(handleRPC(req))
		}(req)
	}
	wg.Wait()
	waitAllJobs()
	stopWatcher()
	// On exit only servers ContextSaver started that are idle, unbusy and unpinned stop;
	// everything else keeps running for RemoteAI and other tools.
	embedSrv.shutdown()
	idleSweep()
}

// mcpSend writes a message to the client (set while serving).
var mcpSend func(any)

// progressFunc sends notifications/progress for a call that sent a progress token
// (clients reset their tool-call timeout on progress); nil without a token.
func progressFunc(tok any) func(string) {
	if tok == nil || mcpSend == nil {
		return nil
	}
	n := 0
	return func(msg string) {
		n++
		mcpSend(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": tok, "progress": n, "message": msg}})
	}
}

func handleRPC(req rpcReq) map[string]any {
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		json.Unmarshal(req.Params, &p)
		noteClient(p.ClientInfo.Name, p.ClientInfo.Version)
		v := p.ProtocolVersion
		if v == "" {
			v = "2025-06-18"
		}
		resp["result"] = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "contextsaver", "version": appVersion},
			"instructions":    instructions(),
		}
	case "ping":
		resp["result"] = map[string]any{}
	case "tools/list":
		resp["result"] = map[string]any{"tools": []json.RawMessage{compactTool}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken any `json:"progressToken"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp["error"] = map[string]any{"code": -32602, "message": "bad params"}
			return resp
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		var text string
		var imgs []toolImage
		var err error
		if p.Name == "pc" {
			text, imgs, err = callCompactP(p.Arguments, progressFunc(p.Meta.ProgressToken))
		} else {
			text, imgs, err = callTool(p.Name, p.Arguments)
		}
		isErr := err != nil
		if isErr {
			text, imgs = err.Error(), nil
		}
		content := []map[string]any{{"type": "text", "text": text}}
		for _, im := range imgs {
			content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(im.data), "mimeType": im.mime})
		}
		resp["result"] = map[string]any{"content": content, "isError": isErr}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
	}
	return resp
}

type toolImage struct {
	data []byte
	mime string
}

func callTool(name string, args json.RawMessage) (out string, imgs []toolImage, err error) {
	defer func() { out, err = shortRepoPaths(out), shortRepoErr(err) }()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	switch name {
	case "pc":
		return callCompact(args)
	case "scan":
		out, err = toolScan(args)
	case "find":
		out, err = toolFind(args)
	case "read":
		return toolRead(args)
	case "run_job":
		out, err = toolRunJob(args)
	case "job":
		out, err = toolJob(args)
	case "saved":
		out, err = toolSaved(args)
	case "local_ai":
		out, err = toolLocalAI(args)
	case "run":
		out, err = toolRun(args)
	case "system":
		out, err = toolSystem(args)
	case "web":
		out, err = toolWeb(args)
	case "window":
		return toolWindowFull(args)
	case "browser":
		out, err = toolBrowser(args)
	case "media":
		out, err = toolMedia(args)
	case "git":
		out, err = toolGit(args)
	case "check":
		var ca struct {
			Action string `json:"action"`
		}
		json.Unmarshal(args, &ca)
		switch ca.Action {
		case "build":
			out, err = toolBuild(args)
		case "compile":
			out, err = toolCompile(args)
		case "test":
			m := map[string]any{}
			json.Unmarshal(args, &m)
			out, err = toolTest(m)
		default:
			out, err = toolCodeCheck(args)
		}
	case "office":
		out, err = toolOffice(args)
	case "rag":
		out, err = toolRag(args)
	default:
		err = fmt.Errorf("unknown tool %q", name)
	}
	return
}

func waitSecs(p *int, def, maxV int) int {
	if p == nil {
		return def
	}
	return min(max(*p, 0), maxV)
}

func toolRunJob(raw json.RawMessage) (string, error) {
	var a struct {
		Steps  []Step `json:"steps"`
		Saved  string `json:"saved"`
		Name   string `json:"name"`
		DryRun bool   `json:"dry_run"`
		Notify bool   `json:"notify"`
		Wait   *int   `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	if a.Saved != "" {
		sj, err := loadSaved(a.Saved)
		if err != nil {
			return "", err
		}
		a.Steps, a.Name = sj.Steps, sj.Name
	}
	if err := validateSteps(a.Steps); err != nil {
		return "", err
	}
	j, err := newJob(JobSpec{Name: a.Name, Kind: "files", Steps: a.Steps, DryRun: a.DryRun, Source: "chat"})
	if err != nil {
		return "", err
	}
	j.start()
	waitJob(j, waitSecs(a.Wait, 15, 50))
	s := j.snapshot()
	return withNotify(formatState(&s), &s, a.Notify), nil
}

func toolJob(raw json.RawMessage) (string, error) {
	var a struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		DryRun bool   `json:"dry_run"`
		Wait   *int   `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	switch strings.ToLower(a.Action) {
	case "list":
		return listJobs(15), nil
	case "log", "tail":
		id, err := resolveJobID(a.ID)
		if err != nil {
			return "", err
		}
		return jobLog(id, 60)
	case "cancel":
		id, err := resolveJobID(a.ID)
		if err != nil {
			return "", err
		}
		return cancelJob(id), nil
	case "undo":
		if a.ID == "" {
			return "", errors.New("undo needs the job id")
		}
		id, err := resolveJobID(a.ID)
		if err != nil {
			return "", err
		}
		j, err := newJob(JobSpec{Name: "undo " + id, Kind: "undo", UndoOf: id, DryRun: a.DryRun, Source: "chat"})
		if err != nil {
			return "", err
		}
		j.start()
		waitJob(j, waitSecs(a.Wait, 20, 50))
		s := j.snapshot()
		return formatState(&s), nil
	case "notify":
		id, err := resolveJobID(a.ID)
		if err != nil {
			return "", err
		}
		s, err := loadState(id)
		if err != nil {
			return "", err
		}
		return withNotify(formatState(s), s, true), nil
	case "", "status":
		id, err := resolveJobID(a.ID)
		if err != nil {
			return "", err
		}
		if v, ok := running.Load(id); ok {
			waitJob(v.(*Job), waitSecs(a.Wait, 0, 50))
		}
		s, err := loadState(id)
		if err != nil {
			return "", err
		}
		return formatState(s), nil
	}
	return "", errors.New("action must be status, list, cancel, undo or notify")
}

func toolSaved(raw json.RawMessage) (string, error) {
	var a struct {
		Action      string `json:"action"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Steps       []Step `json:"steps"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	switch strings.ToLower(a.Action) {
	case "", "list":
		list := listSaved()
		if len(list) == 0 {
			return "No saved jobs yet.", nil
		}
		var b strings.Builder
		for _, s := range list {
			fmt.Fprintf(&b, "%s — %s", s.Name, clip(describeSteps(s.Steps), 160))
			if s.Description != "" {
				b.WriteString(" (" + s.Description + ")")
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "show":
		s, err := loadSaved(a.Name)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(s.Steps)
		return s.Name + ": " + string(b), nil
	case "save":
		p, err := saveJob(SavedJob{Name: a.Name, Description: a.Description, Steps: a.Steps})
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("Saved %q. Run it with run_job saved=%q.", strings.TrimSpace(a.Name), strings.TrimSpace(a.Name))
		if p != "" {
			msg += " The user can run it by double-clicking " + p
		}
		return msg, nil
	case "remove":
		if err := removeSaved(a.Name); err != nil {
			return "", err
		}
		return "Removed saved job " + a.Name + ".", nil
	}
	return "", errors.New("action must be list, show, save or remove")
}

func toolLocalAI(raw json.RawMessage) (string, error) {
	var a struct {
		AIRequest
		DryRun bool `json:"dry_run"`
		Notify bool `json:"notify"`
		Wait   *int `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	req := a.AIRequest
	req.Action = strings.ToLower(req.Action)
	switch req.Action {
	case "models":
		return aiModelsText(), nil
	case "ask":
		if req.Question == "" {
			return "", errors.New("ask needs question")
		}
	case "summarize":
	case "classify":
		if len(req.Categories) < 2 {
			return "", errors.New("classify needs at least 2 categories")
		}
	default:
		return "", errors.New("action must be ask, summarize, classify or models")
	}
	if len(req.Paths) == 0 {
		return "", errors.New("give paths: files or folders to read")
	}
	if !req.Force {
		if w := gpuWarning(); w != "" {
			return w, nil
		}
	}
	j, err := newJob(JobSpec{Name: "local AI " + req.Action, Kind: "ai", AI: &req, DryRun: a.DryRun, Source: "chat"})
	if err != nil {
		return "", err
	}
	j.start()
	waitJob(j, waitSecs(a.Wait, 40, 50))
	s := j.snapshot()
	return withNotify(formatState(&s), &s, a.Notify), nil
}

var toolDefs = []json.RawMessage{
	json.RawMessage(`{"name":"git","description":"Git on the PC in one call, never as admin, GitHub sign-in goes straight to the browser. path = repo folder. status (branch, changes, last commits, last tag), log (n, ref, paths), diff (ref e.g. v1.0..HEAD or empty for working tree; full:true adds the text, max_chars), commit (message; paths or everything; full:true also pushes), push (sets upstream if missing), pull (--ff-only), clone (url or owner/name, to, ref), release (version: bumps main.go appVersion / package.json / pyproject / build.gradle, puts notes under What's new in FUNCTIONS.md, commits 'version: message' and pushes; no_push, dry_run), handoff (notes for another project from commits+diff since ref or last tag, summarized by the local AI; to = file to write; message = extra context), compare (path vs to, folders or owner/name[@ref]: files only on one side, changed files by changed lines, same count; pair:true pairs x_windows.go with x_linux.go or x.go; paths[] limits, full:true adds changed lines; n caps lists). path may be owner/name[@ref][/sub]: a temporary read-only copy of that GitHub repo (commit/push/release refused); clone without to makes one and updates a destination that already is a clone.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["status","log","diff","compare","commit","push","pull","clone","release","handoff"]},"path":{"type":"string"},"pair":{"type":"boolean"},"message":{"type":"string"},"ref":{"type":"string"},"url":{"type":"string"},"to":{"type":"string"},"version":{"type":"string"},"notes":{"type":"array","items":{"type":"string"}},"paths":{"type":"array","items":{"type":"string"}},"n":{"type":"integer"},"full":{"type":"boolean"},"no_push":{"type":"boolean"},"dry_run":{"type":"boolean"},"max_chars":{"type":"integer"},"wait_seconds":{"type":"integer"}},"required":["action","path"]}}`),
	json.RawMessage(`{"name":"check","description":"Lint and type-check code with Ruff + mypy (Python), ESLint + tsc (JS/TS), go vet + staticcheck (Go modules) and Android lint (Gradle projects, Kotlin/Java), skipping gitignored files; returns issues as file:line:col tool code message. Run it on code Claude wrote or changed before handing it over. source: pc (default; path = file or folder), phone (path on the phone, e.g. /sdcard/Projects/app, pulled over ADB; device: IP:port to connect wirelessly), repo (path = owner/name or git URL; ref = branch or tag; shallow clone). Uses the project's own ESLint config, tsconfig, ruff and mypy settings when present, strict defaults otherwise. fix:true applies safe autofixes (for phone or repo only to the local copy). only: python|js|go|android. max: issues shown (40). action:setup installs or updates the tools (also done on first use). action:build compiles and tests the project at path (go build -trimpath + go test, npm run build + npm test, gradlew assembleDebug + unit tests, python compileall + pytest) and returns only the failing lines; no_test:true skips tests.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["check","setup","build"]},"source":{"type":"string","enum":["pc","phone","repo"]},"path":{"type":"string"},"ref":{"type":"string"},"device":{"type":"string"},"fix":{"type":"boolean"},"only":{"type":"string","enum":["python","js","go","android"]},"max":{"type":"integer"}}}}`),
	json.RawMessage(`{"name":"scan","description":"Folder overview: file count, size, types, dates, largest files, subfolders. Use instead of listing.","inputSchema":{"type":"object","properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"include":{"type":"string"},"exclude":{"type":"string"}},"required":["path"]}}`),
	json.RawMessage(`{"name":"find","description":"Find files by name, size, date and/or text inside (text, code, Office, PDF) in many folders at once; returns matches with up to lines (3) matching lines per file and a count of the rest. all:true also searches cache folders.","inputSchema":{"type":"object","properties":{"path":{"type":"array","items":{"type":"string"}},"name":{"type":"string"},"text":{"type":"string"},"regex":{"type":"boolean"},"case_sensitive":{"type":"boolean"},"include":{"type":"string"},"exclude":{"type":"string"},"min_size":{"type":"string"},"max_size":{"type":"string"},"newer_than":{"type":"string"},"older_than":{"type":"string"},"recursive":{"type":"boolean"},"limit":{"type":"integer"},"lines":{"type":"integer"},"all":{"type":"boolean"}},"required":["path"]}}`),
	json.RawMessage(`{"name":"read","description":"Read part of a file: info, head, tail, lines, grep, text (Office/HTML/PDF as text; scans and pictures via OCR). pages: PDF pages, e.g. 3-5. image: small copy of a picture or PDF page (max_px, default 800). values: values of keys in JSON files plus strings ending in ext (e.g. a ComfyUI workflow's nodes and models). changes:true: only lines new or gone since your last changes read.","inputSchema":{"type":"object","properties":{"path":{"type":"string"},"mode":{"type":"string","enum":["info","head","tail","lines","grep","text","image","values"]},"lines":{"type":"integer"},"from_line":{"type":"integer"},"pattern":{"type":"string"},"regex":{"type":"boolean"},"context":{"type":"integer"},"max_chars":{"type":"integer"},"keys":{"type":"array","items":{"type":"string"}},"ext":{"type":"string"},"pages":{"type":"string"},"max_px":{"type":"integer"},"changes":{"type":"boolean"}},"required":["path"]}}`),
	json.RawMessage(`{"name":"run_job","description":"File operations as a background job, any number of files; steps run in order. Ops: copy/move {from,to} (conflict keep_both|skip|newer|overwrite; contents_only; flatten), sync {from,to,delete_extra}, delete {from} (Trash only), rename {from,rules: replace+with (regex), template, case, prefix, suffix, ext; on_conflict; counter; sort}, mkdir {to}, download {from: URLs, to, name, sha256}, extract {from, to}, edit {from, edits:[{find,replace,regex,count}], append}, write {to, content}. Filters: include/exclude (wildcards; separated), recursive, min_size/max_size, newer_than/older_than (30d, 6m, a date). Name tokens: {name} {ext} {parent} {n:3} {date:YYYY-MM-DD} {taken:...}. Dry-run first for delete, rename, overwrite and delete_extra. saved runs a saved job; notify signals when it ends.","inputSchema":{"type":"object","properties":{"steps":{"type":"array","items":{"type":"object","properties":{"op":{"type":"string","enum":["copy","move","sync","delete","rename","mkdir","download","extract","edit","write"]},"from":{"type":"array","items":{"type":"string"}},"to":{"type":"string"},"contents_only":{"type":"boolean"},"flatten":{"type":"boolean"},"conflict":{"type":"string","enum":["keep_both","skip","newer","overwrite"]},"delete_extra":{"type":"boolean"},"rules":{"type":"array","items":{"type":"object","properties":{"replace":{"type":"string"},"with":{"type":"string"},"regex":{"type":"boolean"},"ignore_case":{"type":"boolean"},"template":{"type":"string"},"case":{"type":"string","enum":["lower","upper","title","sentence"]},"prefix":{"type":"string"},"suffix":{"type":"string"},"ext":{"type":"string"},"ext_case":{"type":"string","enum":["lower","upper"]},"trim":{"type":"boolean"}}}},"on_conflict":{"type":"string","enum":["stop","number","skip"]},"counter":{"type":"string","enum":["folder","global"]},"sort":{"type":"string","enum":["name","date","taken","size"]},"start":{"type":"integer"},"folders":{"type":"boolean"},"include":{"type":"string"},"exclude":{"type":"string"},"recursive":{"type":"boolean"},"min_size":{"type":"string"},"max_size":{"type":"string"},"newer_than":{"type":"string"},"older_than":{"type":"string"},"verify":{"type":"string","enum":["size","hash","none"]},"workers":{"type":"integer"},"remove_empty_dirs":{"type":"boolean"},"name":{"type":"string"},"sha256":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"find":{"type":"string"},"replace":{"type":"string"},"regex":{"type":"boolean"},"count":{"type":"integer"}},"required":["find"]}},"append":{"type":"string"},"content":{"type":"string"}},"required":["op"]}},"saved":{"type":"string"},"name":{"type":"string"},"dry_run":{"type":"boolean"},"wait_seconds":{"type":"integer"},"notify":{"type":"boolean"}}}}`),
	json.RawMessage(`{"name":"job","description":"Jobs: status (no id = latest; wait_seconds), list, cancel, undo (restores moved/renamed/deleted/replaced files; dry_run), notify (signal through GitHub when it ends, with a shell command to wait for it instead of polling).","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["status","list","cancel","undo","notify"]},"id":{"type":"string"},"dry_run":{"type":"boolean"},"wait_seconds":{"type":"integer"}}}}`),
	json.RawMessage(`{"name":"saved","description":"Saved jobs the user can re-run by double-click: list, show, save (name, steps like run_job, description), remove. Run one with run_job saved=name.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["list","show","save","remove"]},"name":{"type":"string"},"description":{"type":"string"},"steps":{"type":"array","items":{"type":"object"}}},"required":["action"]}}`),
	json.RawMessage(`{"name":"local_ai","description":"The PC's own model reads files so their text stays out of the chat: ask (answer + which files), summarize, classify (optionally move files into move_to\\<category>; dry_run), models. Reads text, code, Office, PDF and pictures. Check key facts with read. If it returns a GPU-busy WARNING, tell the user and retry with force:true only if they agree.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["ask","summarize","classify","models"]},"paths":{"type":"array","items":{"type":"string"}},"question":{"type":"string"},"categories":{"type":"array","items":{"type":"string"}},"move_to":{"type":"string"},"include":{"type":"string"},"recursive":{"type":"boolean"},"max_files":{"type":"integer"},"detail":{"type":"string","enum":["brief","normal"]},"model":{"type":"string"},"dry_run":{"type":"boolean"},"force":{"type":"boolean"},"wait_seconds":{"type":"integer"},"notify":{"type":"boolean"}},"required":["action"]}}`),
	json.RawMessage(`{"name":"run","description":"Run a command (bash) as a background job; returns exit code, error lines and last lines (tail; 0 = none), full log stays on the PC. No input possible: pass -y flags. wait_for: regex to return early (e.g. a server's ready line); keep_running for servers. elevated:true runs as root without a password prompt; destructive admin commands (deleting, registry, services, disks, firewall...) are refused unless confirmed:true, set only after the user said yes to that exact command.","inputSchema":{"type":"object","properties":{"command":{"type":"string"},"elevated":{"type":"boolean"},"confirmed":{"type":"boolean"},"shell":{"type":"string","enum":["bash","sh","pwsh"]},"cwd":{"type":"string"},"env":{"type":"object","additionalProperties":{"type":"string"}},"timeout_minutes":{"type":"integer"},"keep_running":{"type":"boolean"},"wait_for":{"type":"string"},"wait_seconds":{"type":"integer"},"tail":{"type":"integer"},"name":{"type":"string"},"notify":{"type":"boolean"}},"required":["command"]}}`),
	json.RawMessage(`{"name":"system","description":"PC state. info: Linux, CPU, RAM, free space per disk, GPU/driver/CUDA, installed tools. processes (filter), ports, kill (pids, or name + match; system and Claude processes are refused).","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["info","processes","ports","kill"]},"filter":{"type":"string"},"port":{"type":"integer"},"pids":{"type":"array","items":{"type":"integer"}},"name":{"type":"string"},"match":{"type":"string"}}}}`),
	json.RawMessage(`{"name":"web","description":"Search the web and read pages on the PC, text only (no JavaScript; use browser for that). search: titles, URLs, snippets. find: only lines matching pattern in urls or the top results of query. read: outline and first lines; from_line/lines for more. links (pattern). ask: the local AI answers question from the pages (same GPU rule as local_ai). PDFs read as text. Cached 30 min (fresh:true refetches).","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["search","find","read","links","ask"]},"query":{"type":"string"},"urls":{"type":"array","items":{"type":"string"}},"url":{"type":"string"},"pattern":{"type":"string"},"regex":{"type":"boolean"},"context":{"type":"integer"},"question":{"type":"string"},"pages":{"type":"integer"},"results":{"type":"integer"},"from_line":{"type":"integer"},"lines":{"type":"integer"},"max_chars":{"type":"integer"},"fresh":{"type":"boolean"},"model":{"type":"string"},"force":{"type":"boolean"},"notify":{"type":"boolean"},"wait_seconds":{"type":"integer"}},"required":["action"]}}`),
	json.RawMessage(`{"name":"window","description":"On-screen text instead of screenshots. list: windows (title, program, pid, size; filter). text: a window's text via accessibility in screen order, buttons as [Name]; OCR fallback. ocr: a window, or area x,y,w,h; boxes:true adds @x,y per line for clicking. copy: select all + copy, clipboard restored. do: steps, e.g. [{\"click\":\"Save\"},{\"type\":\"x\",\"into\":\"File name\"},{\"press\":\"enter\"},{\"wait\":1}]; also click_text (OCR text, nth), click_at [x,y], move [x,y], drag [x1,y1,x2,y2], scroll (+up/-down notches, at [x,y]), hold (keys, secs); button left|right|middle|double; x,y are in the window unless screen:true. then:\"text\" returns the window after. wait: until the window opens or pattern shows (wait, max 50 s). clipboard: get, or set with text. screenshot: small JPEG of a window or area (max_px 1000), only when text isn't enough; question: the local AI answers about it instead (no image). window: title, program or pid (default: front window). Long text cut at max_chars (4000) and saved to a file; summary:true summarizes locally; changes:true only what changed.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["list","text","ocr","copy","do","wait","clipboard","screenshot"]},"window":{"type":"string"},"filter":{"type":"string"},"x":{"type":"integer"},"y":{"type":"integer"},"w":{"type":"integer"},"h":{"type":"integer"},"boxes":{"type":"boolean"},"max_chars":{"type":"integer"},"max_px":{"type":"integer"},"text":{"type":"string"},"question":{"type":"string"},"force":{"type":"boolean"},"summary":{"type":"boolean"},"steps":{"type":"array","items":{"type":"object"}},"then":{"type":"string","enum":["text"]},"pattern":{"type":"string"},"wait":{"type":"number"},"changes":{"type":"boolean"}},"required":["action"]}}`),
	json.RawMessage(`{"name":"browser","description":"Real Chrome with its own ContextSaver profile (stays signed in) for JavaScript or signed-in pages, text only. open/text (url), find (pattern, context), links (pattern), click (target: button/link text), fill (target: field label/placeholder, value), press (value, default Enter), wait (pattern, wait), close. show:true opens it visibly, e.g. for the user to sign in once.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["open","text","find","links","click","fill","press","wait","close"]},"url":{"type":"string"},"target":{"type":"string"},"value":{"type":"string"},"pattern":{"type":"string"},"context":{"type":"integer"},"wait":{"type":"number"},"show":{"type":"boolean"},"max_chars":{"type":"integer"}},"required":["action"]}}`),
	json.RawMessage(`{"name":"media","description":"Audio and video as text. info: length and streams. transcript: speech to text with local Whisper (language optional, timestamps:true for times), cached; long ones finish in the background, call again for the result; same GPU-busy rule as local_ai (force). frames: stills saved to a folder (frames count or every seconds) with their OCR text.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["info","transcript","frames"]},"path":{"type":"string"},"language":{"type":"string"},"timestamps":{"type":"boolean"},"frames":{"type":"integer"},"every":{"type":"number"},"force":{"type":"boolean"},"max_chars":{"type":"integer"}},"required":["action","path"]}}`),
	json.RawMessage(`{"name":"office","description":"Word, Excel, PowerPoint, PDF on the PC. create: .docx/.pdf from Markdown content (# headings, - lists, 1. lists, | tables |, **bold**, *italic*, ` + "`code`" + `, [link](url), ![](picture), --- page break); .pptx: a slide per --- or #/## heading (first heading = title, then bullets, tables, pictures); .xlsx: sheets [{name, rows}] (numbers stay numbers, \"=SUM(A1:A3)\" formulas, 2026-01-31 dates) or CSV content; .pdf also merges from (\"a.pdf\", \"b.pdf:2-5\", pictures). edit (old copy kept in backups; to = save as): docx: replace [{find,with}], delete_paragraphs, content appended or insert_after text; pptx: replace, delete_slides \"2,4\", content = new slides; xlsx: cells {\"B2\":5,\"Sheet2!C1\":\"=B2*2\"}, append_rows (sheet), sheets adds, replace; pdf: pages keeps/reorders \"3,1-2\", rotate 90 and stamp text ({n} {total}; stamp_at bottom|top|center watermark|top-right...) on apply_to pages, from appends files, content appends pages, split:true one file per page or per pages group. read: Markdown-like outline (docx/pptx), cells with A1 refs and formulas (xlsx), PDF text by page (pages); .doc/.xls/.ppt/.rtf read via LibreOffice. convert: to another format through LibreOffice (to: \".pdf\" or a file name): doc/docx/rtf/pdf<->docx/pdf/doc/rtf/odt/html, xls/xlsx/csv->xlsx/xls/csv/pdf/ods, ppt/pptx->pptx/ppt/pdf/odp.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["create","edit","read","convert"]},"path":{"type":"string"},"to":{"type":"string"},"content":{"type":"string"},"sheets":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"rows":{"type":"array","items":{"type":"array"}}}}},"sheet":{"type":"string"},"cells":{"type":"object"},"append_rows":{"type":"array","items":{"type":"array"}},"col_widths":{"type":"object","additionalProperties":{"type":"number"}},"replace":{"type":"array","items":{"type":"object","properties":{"find":{"type":"string"},"with":{"type":"string"}},"required":["find"]}},"insert_after":{"type":"string"},"delete_paragraphs":{"type":"array","items":{"type":"string"}},"delete_slides":{"type":"string"},"from":{"type":"array","items":{"type":"string"}},"pages":{"type":"string"},"apply_to":{"type":"string"},"rotate":{"type":"integer"},"stamp":{"type":"string"},"stamp_at":{"type":"string"},"stamp_size":{"type":"number"},"split":{"type":"boolean"},"landscape":{"type":"boolean"},"overwrite":{"type":"boolean"},"max_chars":{"type":"integer"}},"required":["action","path"]}}`),
	json.RawMessage(`{"name":"rag","description":"Meaning-based search over the PC's files plus memory notes across chats. search: query → best passages (path, line/page, text); under (folder), ext (\"pdf,docx\"), scope all|files|notes, tag, k (8). remember: text (+tags) as a note: decisions, facts, summaries worth recalling later. notes: list (tag, limit). forget: ids (archived, not deleted). update: (re)index changed files only when asked; paths limits it to folders; dry_run shows what it would cover; runs in the background (notify). Uses the GPU when there is room; the CPU by itself while ComfyUI or training runs (force:true keeps the GPU). status. config: roots, types (only these extensions, e.g. [\"pdf\",\"docx\"]; [] = all), exclude (path, folder name, or *.ext; \"+name\" re-includes a default-skipped name), include (undo exclude); an update dropping over half the index, roots in the temp folder, or replacing every root at once are refused unless confirm:true (only after the user agrees).","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["search","remember","notes","forget","update","status","config"]},"query":{"type":"string"},"k":{"type":"integer"},"under":{"type":"string"},"ext":{"type":"string"},"scope":{"type":"string","enum":["all","files","notes"]},"tag":{"type":"string"},"text":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},"ids":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer"},"paths":{"type":"array","items":{"type":"string"}},"roots":{"type":"array","items":{"type":"string"}},"exclude":{"type":"array","items":{"type":"string"}},"include":{"type":"array","items":{"type":"string"}},"types":{"type":"array","items":{"type":"string"}},"dry_run":{"type":"boolean"},"force":{"type":"boolean"},"confirm":{"type":"boolean"},"cpu":{"type":"boolean"},"notify":{"type":"boolean"},"wait_seconds":{"type":"integer"}},"required":["action"]}}`),
}
