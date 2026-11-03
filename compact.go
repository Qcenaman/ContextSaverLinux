package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContextSaver exposes one small tool ("pc") instead of many large ones, so a local
// model's context isn't filled with tool schemas. Full arguments for a command are
// fetched on demand with c=help, and every result is capped; the rest is kept on
// disk and paged with c=more.

const compactDesc = `The user's Linux PC; you CAN act, so call this instead of refusing. c=command, a=args, absolute paths.
open{target} find{path[],name,text} ls{path} read{path,from_line,lines,pattern} write{path,content} edit{path,find,replace} run{command} web{query|url} ai{paths[],question} office{action,path} rag{query|text} sys{action} model{name} more{id}
git{action:commit|push|login|release|ci,path,message} check{path,action:build|test|compile,target} outline{path} new{kind,name} deps export
Also: plan files job saved browser window media. phone{action:install,app}=user's phone, not adb. help{c}=all args.
The user's own apps (AdbSaver RemoteAI TokenSaver...) are in D:\Claude Apps + github Qcenaman/<name>: never web-search them. me = about the user.`

// clineDesc: with "mcp --for cline" only what Cline lacks is advertised (Cline has its
// own read/write/edit/run/find/browser); every command still works.
const clineDesc = `The user's PC beyond Cline's own tools; you CAN act. c=command, a=args, absolute paths.
git{action:status|diff|log|commit|push|pull|clone|login|ci,path,message} check{path,action:check|build} after every code change
compile{path,target:linux|windows|android} outline{path} test{path,name} new{kind,name} deps{path,action,name} release{path,version} logcat{app}
rag{query|text} office{action,path} web{query|url} ai{paths[],question} model{name}
Also: window media sys saved job phone export more. help{c} = all args.`

var clineCommands = []string{"git", "check", "compile", "outline", "test", "new", "deps", "release", "logcat", "export", "rag", "office", "web", "ai", "model", "window", "media", "sys", "saved", "job", "phone", "more", "help"}

// The profile in use (set from "mcp --for <client>" before serving).
var (
	activeDesc     = compactDesc
	activeCommands = compactCommands
)

func setProfile(p string) {
	if strings.EqualFold(p, "cline") {
		activeDesc, activeCommands = clineDesc, clineCommands
	} else {
		activeDesc, activeCommands = compactDesc, compactCommands
	}
	compactTool = buildCompactTool()
}

// compactCommands is the one list of canonical pc commands. The schema enum, the
// unknown-c error and the tests are built from it so they can't drift apart.
// Aliases in compactAlias (and model names) still work for models that ignore the enum.
var compactCommands = []string{"open", "find", "ls", "read", "write", "edit", "run", "web", "ai", "office", "rag", "sys", "model", "more", "help", "files", "job", "saved", "browser", "window", "media", "git", "check", "plan", "phone", "me",
	"outline", "new", "export", "deps"}

var compactTool = buildCompactTool()

func buildCompactTool() json.RawMessage { return buildCompactToolFor(activeDesc, activeCommands) }

func buildCompactToolFor(desc string, commands []string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"name":        "pc",
		"description": desc,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"c": map[string]any{"type": "string", "enum": commands},
				"a": map[string]any{"type": "object", "description": `args for c, e.g. {"path":"~/x.txt"}`},
			},
			"required": []string{"c"},
		},
	})
	return b
}

// compact command -> full tool name
var compactAlias = map[string]string{
	"find": "find", "search": "find",
	"ls": "scan", "scan": "scan", "dir": "scan", "list": "scan",
	"read": "read", "cat": "read",
	"run": "run", "cmd": "run", "shell": "run", "exec": "run",
	"web": "web", "browser": "browser", "window": "window", "media": "media",
	"sys": "system", "system": "system",
	"ai": "local_ai", "local_ai": "local_ai",
	"office": "office", "doc": "office", "word": "office", "excel": "office", "xlsx": "office", "docx": "office", "pptx": "office", "pdf": "office", "powerpoint": "office",
	"rag": "rag", "memory": "rag", "notes": "rag",
	"job": "job", "jobs": "job", "files": "run_job", "run_job": "run_job", "saved": "saved",
	"git": "git", "check": "check", "lint": "check",
}

const defaultMaxOut = 1800

func maxOut() int {
	if cfg.MaxOut > 0 {
		return cfg.MaxOut
	}
	return defaultMaxOut
}

// callCompact runs one "pc" call.
func callCompact(raw json.RawMessage) (string, []toolImage, error) {
	return callCompactP(raw, nil)
}

// callCompactP runs one pc call; progress (may be nil) sends MCP progress notes while it
// blocks on an approval.
func callCompactP(raw json.RawMessage, progress func(string)) (string, []toolImage, error) {
	start := time.Now()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", nil, fmt.Errorf("bad arguments: %v", err)
	}
	c := strings.ToLower(strings.TrimSpace(jsonStr(top["c"])))
	if c == "" {
		c = strings.ToLower(strings.TrimSpace(jsonStr(top["cmd"])))
	}
	args := map[string]any{}
	if a, ok := top["a"]; ok && len(a) > 0 && string(a) != "null" {
		if err := json.Unmarshal(a, &args); err != nil {
			// some models send a as a JSON string
			var s string
			if json.Unmarshal(a, &s) == nil && json.Unmarshal([]byte(s), &args) == nil {
			} else {
				return "", nil, errors.New(`a must be an object, e.g. {"c":"read","a":{"path":"~/x.txt"}}`)
			}
		}
	} else {
		// args given flat next to c
		for k, v := range top {
			if k == "c" || k == "cmd" {
				continue
			}
			var x any
			json.Unmarshal(v, &x)
			args[k] = x
		}
	}
	if c == "" {
		return "", nil, errors.New("give c (a command). " + firstLine(activeDesc))
	}
	// an install's APK is fetched and checked before any approval is asked
	if err := phonePreflight(c, args); err != nil {
		return "", nil, errors.New(clip1(err.Error(), 600))
	}
	if localMode {
		confirm := str(args, "confirm")
		if confirm == "" {
			confirm = jsonStr(top["confirm"])
		}
		// confirm:true (a bool) is a tool argument, e.g. overriding the rag index guards;
		// confirm:"<token>" is the user's approval for a risky call.
		guard, _ := args["confirm"].(bool)
		delete(args, "confirm")
		// run's confirmed:true (lets the admin helper run a destructive command) is set
		// only by an approval below, never by the model
		delete(args, "confirmed")
		confirmed, note := false, ""
		if guard {
			args["confirm"] = true
		} else if confirm != "" {
			// the token approved a call that had confirm:true; give it back
			args["confirm"] = true
			if confirmed, note = spendConfirm(confirm, c, args); !confirmed {
				delete(args, "confirm")
			}
		}
		if note != "" { // a pending or denied approval for exactly this call
			return note, nil, nil
		}
		approved := confirmed
		if what := approvalAction(c, args); what != "" && !confirmed {
			if confirm == "" && approvalKind() == "wait" {
				if ok, msg := waitApproval(c, args, what, progress); !ok {
					return msg, nil, nil
				}
			} else if ok, n := spendConfirm(confirm, c, args); !ok {
				if n == "" {
					n = note
				}
				return askApproval(c, args, what, n), nil, nil
			}
			approved = true
		}
		if approved && (c == "run" || compactAlias[c] == "run") {
			args["confirmed"] = true
		}
	}
	notes := repoUpdateNotes(c, args)
	out, imgs, err := runLimited(start, func() (string, []toolImage, error) { return compactDispatch(c, args) })
	out, err = shortRepoPaths(out), shortRepoErr(err)
	if err != nil {
		return "", nil, errors.New(clip1(err.Error(), 600))
	}
	return capOut(notes + out), imgs, nil
}

func compactDispatch(c string, a map[string]any) (string, []toolImage, error) {
	switch c {
	case "open", "launch", "start", "app":
		out, err := openTarget(str(a, "target", "app", "path", "url", "name", "file"), str(a, "args"))
		return out, nil, err
	case "help", "?":
		c := str(a, "c", "cmd", "command")
		if full, _ := a["full"].(bool); full {
			return compactHelp(c), nil, nil
		}
		k := strings.ToLower(c)
		if t, ok := compactAlias[k]; ok {
			if sk, ok := helpKey[t]; ok {
				k = sk
			}
		}
		if h, ok := shortHelp[k]; ok {
			return h + "\n(help{c,full:true} = every option)", nil, nil
		}
		return compactHelp(c), nil, nil
	case "more", "next":
		if o, im, err, ok := moreSlow(str(a, "id")); ok {
			if err != nil {
				return "", nil, err
			}
			return capOut(o), im, nil
		}
		return moreOut(str(a, "id"), intArg(a, "from")), nil, nil
	case "plan", "todo", "tasks":
		return toolPlan(a), nil, nil
	case "phone", "android", "install_app", "apk", "mobile":
		out, err := toolPhoneCmd(a)
		return out, nil, err
	case "me", "about", "user", "whoami":
		return aboutCard(), nil, nil
	case "remember":
		a["action"] = "remember"
		fixArgs("rag", a)
		b, _ := json.Marshal(a)
		out, err := toolRag(b)
		return out, nil, err
	case "load", "switch", "use":
		a["action"] = "use"
		if str(a, "name", "model") == "" {
			a["name"] = str(a, "target", "to")
		}
		out, err := toolModel(a)
		return out, nil, err
	case "model", "models":
		out, err := toolModel(a)
		return out, nil, err
	case "sys", "system":
		if strings.EqualFold(str(a, "action"), "setup") {
			out, err := toolSetup(a)
			return out, nil, err
		}
	case "setup", "install":
		out, err := toolSetup(a)
		return out, nil, err
	case "outline", "symbols":
		out, err := toolOutline(a)
		return out, nil, err
	case "test", "tests":
		out, err := toolTest(a)
		return out, nil, err
	case "compile", "build":
		if c == "build" && str(a, "target") == "" {
			b, _ := json.Marshal(a)
			out, err := toolBuild(b)
			return out, nil, err
		}
		b, _ := json.Marshal(a)
		out, err := toolCompile(b)
		return out, nil, err
	case "new", "scaffold":
		out, err := toolNew(a)
		return out, nil, err
	case "export", "zip":
		return toolExport(a)
	case "deps", "dependencies":
		out, err := toolDeps(a)
		return out, nil, err
	case "release":
		a["action"] = "release"
		b, _ := json.Marshal(a)
		out, err := toolGit(b)
		return out, nil, err
	case "compare":
		a["action"] = "compare"
		fixArgs("git", a)
		if str(a, "path") == "" {
			if v := str(a, "a", "from"); v != "" {
				a["path"] = v
			}
		}
		b, _ := json.Marshal(a)
		out, err := toolGit(b)
		return out, nil, err
	case "login":
		out, err := gitLogin()
		return out, nil, err
	case "logcat", "crash", "crashes":
		out, err := toolLogcat(a)
		return out, nil, err
	case "write", "save", "create":
		p := str(a, "path", "to", "file")
		if p == "" {
			return "", nil, errors.New("write needs path and content")
		}
		content, _ := a["content"].(string)
		if content == "" {
			content, _ = a["text"].(string)
		}
		return compactJob([]map[string]any{{"op": "write", "to": p, "content": content}}, a)
	case "edit", "replace":
		p := str(a, "path", "file", "from")
		if p == "" {
			return "", nil, errors.New("edit needs path, find, replace (or edits[] / append)")
		}
		st := map[string]any{"op": "edit", "from": []string{p}}
		if e, ok := a["edits"]; ok {
			st["edits"] = e
		} else if f := str(a, "find", "old"); f != "" {
			r, _ := a["replace"].(string)
			if r == "" {
				r, _ = a["new"].(string)
			}
			ed := map[string]any{"find": f, "replace": r}
			if b, ok := a["regex"].(bool); ok {
				ed["regex"] = b
			}
			if n := intArg(a, "count"); n > 0 {
				ed["count"] = n
			}
			st["edits"] = []any{ed}
		}
		if ap, ok := a["append"].(string); ok {
			st["append"] = ap
		}
		if st["edits"] == nil && st["append"] == nil {
			return "", nil, errors.New("edit needs find+replace, edits[] or append")
		}
		if out, handled, err := codeEdit(p, st, a); handled {
			return out, nil, err
		}
		return compactJob([]map[string]any{st}, a)
	}
	if _, isModel := models()[c]; isModel {
		out, err := useModel(c, modelLoadWait, a["force"] == true)
		if err == nil {
			out += syncFrontends()
		}
		return out, nil, err
	}
	name, ok := compactAlias[c]
	if !ok {
		return "", nil, fmt.Errorf("unknown c %q. Commands: %s", c, strings.Join(compactCommands, " "))
	}
	fixArgs(name, a)
	note, _ := a["_note"].(string)
	delete(a, "_note")
	b, _ := json.Marshal(a)
	out, imgs, err := callTool(name, b)
	if err == nil {
		out = squeeze(out)
	}
	if err == nil && note != "" {
		out = note + out
	}
	if err == nil && name == "run" {
		out = trimRunOut(out)
	}
	if name == "run" && adbHint(str(a, "command")) {
		if err != nil {
			err = errors.New(err.Error() + "\n" + phoneNudge)
		} else {
			out += "\n" + phoneNudge
		}
	}
	if err == nil {
		out = logLineRe.ReplaceAllString(out, "")
	}
	return out, imgs, err
}

func compactJob(steps []map[string]any, a map[string]any) (string, []toolImage, error) {
	j := map[string]any{"steps": steps}
	if d, ok := a["dry_run"]; ok {
		j["dry_run"] = d
	}
	b, _ := json.Marshal(j)
	out, err := toolRunJob(b)
	return squeeze(logLineRe.ReplaceAllString(out, "")), nil, err
}

// fixArgs accepts the common mistakes small models make: a string where a list
// is expected, and alternate names for the main argument.
func fixArgs(tool string, a map[string]any) {
	rename := func(want string, alts ...string) {
		if _, ok := a[want]; ok {
			return
		}
		for _, k := range alts {
			if v, ok := a[k]; ok {
				a[want] = v
				delete(a, k)
				return
			}
		}
	}
	listify := func(k string) {
		if s, ok := a[k].(string); ok {
			a[k] = []string{s}
		}
	}
	switch tool {
	case "find":
		rename("path", "paths", "folder", "dir", "in")
		listify("path")
		rename("name", "pattern", "glob")
		rename("text", "query", "contains", "grep")
	case "scan", "read", "media":
		rename("path", "file", "folder", "dir")
		if l, ok := a["path"].([]any); ok && len(l) > 0 {
			a["path"] = l[0]
		}
		if tool == "read" {
			rename("from_line", "start", "line", "offset")
			rename("lines", "count", "n", "limit")
			if _, ok := a["mode"]; !ok {
				switch {
				case a["pattern"] != nil:
					a["mode"] = "grep"
				case a["from_line"] != nil:
					a["mode"] = "lines"
				}
			}
			if _, ok := a["max_chars"]; !ok {
				a["max_chars"] = maxOut()
			}
		}
	case "git":
		rename("to", "with", "against", "other", "b")
		listify("paths")
	case "run":
		rename("command", "cmd", "c", "script")
		if _, ok := a["tail"]; !ok {
			a["tail"] = 12
		}
	case "local_ai":
		rename("paths", "path", "files", "file")
		listify("paths")
		rename("question", "q", "query")
		if _, ok := a["action"]; !ok {
			if a["question"] != nil {
				a["action"] = "ask"
			} else {
				a["action"] = "summarize"
			}
		}
	case "web":
		rename("query", "q", "search")
		listify("urls")
		if _, ok := a["action"]; !ok {
			if a["url"] != nil || a["urls"] != nil {
				a["action"] = "read"
			} else {
				a["action"] = "search"
			}
		}
		if _, ok := a["max_chars"]; !ok {
			a["max_chars"] = maxOut()
		}
	case "window", "browser":
		if tool == "window" && strings.EqualFold(str(a, "action"), "screenshot") && a["question"] == nil && !localVision() {
			// the loaded model can't see pictures: give it the window's text instead
			a["action"] = "text"
			a["_note"] = "(the loaded model can't read images, so here is the window's text)\n"
		}
		if _, ok := a["max_chars"]; !ok {
			a["max_chars"] = maxOut()
		}
	case "office":
		rename("path", "file", "target")
		if _, ok := a["action"]; !ok {
			if a["content"] != nil || a["sheets"] != nil {
				a["action"] = "create"
			} else if a["replace"] != nil || a["cells"] != nil || a["append_rows"] != nil || a["stamp"] != nil {
				a["action"] = "edit"
			} else {
				a["action"] = "read"
			}
		}
		if _, ok := a["max_chars"]; !ok {
			a["max_chars"] = maxOut()
		}
	case "rag":
		rename("query", "q", "search")
		rename("text", "note", "content")
		listify("ids")
		listify("paths")
		if _, ok := a["action"]; !ok {
			switch {
			case a["query"] != nil:
				a["action"] = "search"
			case a["text"] != nil:
				a["action"] = "remember"
			default:
				a["action"] = "status"
			}
		}
	case "system":
		if _, ok := a["action"]; !ok {
			a["action"] = "info"
		}
	}
}

var (
	runHeadRe  = regexp.MustCompile(`(?m)^\$ .*\n`)
	runBlankRe = regexp.MustCompile(`\n{3,}`)
	logLineRe  = regexp.MustCompile(`(?m)^Log: .*\n?`)
)

// trimRunOut drops the echoed command line; the model knows what it ran.
func trimRunOut(s string) string {
	s = runHeadRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\n  ", "\n")
	return runBlankRe.ReplaceAllString(s, "\n\n")
}

// ---------- help ----------

func compactHelp(c string) string {
	if c == "" {
		return activeDesc
	}
	switch strings.ToLower(c) {
	case "open":
		return "open{target,args}: start an app (firefox, gnome-calculator, a menu name like Files, a full path), open a file/folder in its default app, or a URL in the browser."
	case "write":
		return "write{path,content}: create or replace a text file (old version kept for job undo)."
	case "edit":
		return "edit{path,find,replace,all,count,regex}: find must match once (copy it exactly, with a neighbour line if needed); returns changed lines. Or edits:[{find,replace}], or append:text."
	case "model":
		return "model{action:list|use|stop|pin|unpin|idle,name,force,minutes}: list = models, which is loaded, server owner/busy/idle; use name=" + strings.Join(modelNames(), "|") + " loads it on port 8080; stop unloads it. A server ContextSaver didn't start or that is busy is never stopped (force:true for an idle external one); pin keeps it on; idle minutes = auto-stop delay."
	case "help":
		return "help{c,full}: short args for command c; full:true = every option. No c = the command list."
	case "more":
		return "more{id,from}: next part of a cut result; from = character offset."
	}
	if phoneCommands[strings.ToLower(c)] {
		return phoneHelp
	}
	switch strings.ToLower(c) {
	case "me", "about", "user", "whoami":
		return "me{}: who the user is, his own apps and how he wants things done (data/about.md); details: rag{query,tag:\"about\"}."
	}
	name, ok := compactAlias[strings.ToLower(c)]
	if !ok {
		return "unknown command " + c
	}
	for _, d := range toolDefs {
		var t struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Schema      struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"inputSchema"`
		}
		if json.Unmarshal(d, &t) != nil || t.Name != name {
			continue
		}
		var ks []string
		for k, v := range t.Schema.Properties {
			var p struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			}
			json.Unmarshal(v, &p)
			s := k
			if len(p.Enum) > 0 {
				s += ":" + strings.Join(p.Enum, "|")
			} else if p.Type != "" && p.Type != "string" {
				s += "(" + p.Type + ")"
			}
			ks = append(ks, s)
		}
		sortStrings(ks)
		return c + ": " + t.Description + "\nargs: " + strings.Join(ks, " ") + "\nrequired: " + strings.Join(t.Schema.Required, ",")
	}
	return "no help for " + c
}

// ---------- output cap + paging ----------

var (
	outMu   sync.Mutex
	outLast string
	outPos  = map[string]int{}
)

func outDir() string { return filepath.Join(appHome, "out") }

func capOut(s string) string {
	s = strings.TrimSpace(s)
	limit := maxOut()
	if len(s) <= limit+200 {
		return s
	}
	outMu.Lock()
	id := "r" + strconv.FormatInt(time.Now().UnixMilli()%60466176, 36)
	outLast = id
	outPos[id] = cutAt(s, limit)
	outMu.Unlock()
	os.MkdirAll(outDir(), 0o755)
	os.WriteFile(filepath.Join(outDir(), id+".txt"), []byte(s), 0o644)
	n := cutAt(s, limit)
	return s[:n] + fmt.Sprintf("\n…[cut, %d more chars: more{id:%s}]", len(s)-n, id)
}

func moreOut(id string, from int) string {
	if id == "" {
		outMu.Lock()
		id = outLast
		outMu.Unlock()
		if id == "" {
			return "nothing to continue; give id"
		}
	}
	b, err := os.ReadFile(filepath.Join(outDir(), filepath.Base(id)+".txt"))
	if err != nil {
		return "no saved output " + id
	}
	s := string(b)
	outMu.Lock()
	if from <= 0 {
		from = outPos[id]
	}
	if from >= len(s) {
		outMu.Unlock()
		return "end of " + id
	}
	n := from + cutAt(s[from:], maxOut())
	outPos[id] = n
	outMu.Unlock()
	out := s[from:n]
	if n < len(s) {
		out += fmt.Sprintf("\n…[%d more: more{id:%s}]", len(s)-n, id)
	}
	return out
}

// cutAt picks a cut point at or before limit, preferring a line end.
func cutAt(s string, limit int) int {
	if len(s) <= limit {
		return len(s)
	}
	if i := strings.LastIndexByte(s[:limit], '\n'); i > limit/2 {
		return i
	}
	for limit > 0 && limit < len(s) && (s[limit]&0xC0) == 0x80 {
		limit--
	}
	return limit
}

// ---------- small helpers ----------

func jsonStr(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return ""
}

func str(a map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := a[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func intArg(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip1(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:cutAt(s, n)] + "…"
}

func sortStrings(x []string) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j] < x[j-1]; j-- {
			x[j], x[j-1] = x[j-1], x[j]
		}
	}
}

// openTarget starts an app, file, folder or URL the way the Start menu would, and
// returns at once.
func openTarget(t, args string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", errors.New("open needs target: an app (gedit), file, folder or URL")
	}
	if !isWin {
		return openLinux(t, args)
	}
	cmdArgs := []string{"/c", "start", "", t}
	if args != "" {
		cmdArgs = append(cmdArgs, strings.Fields(args)...)
	}
	if err := quietCmd("cmd.exe", cmdArgs...).Run(); err != nil {
		return "", fmt.Errorf("couldn't open %s: %v", t, err)
	}
	return "opened " + t, nil
}

// shortHelp: what a local model needs to call each command, in as few tokens as
// possible. help{c,full:true} returns the complete description.
var shortHelp = map[string]string{
	"find":    "find{path[],name,text,regex,lines(3),limit,include,exclude,min_size,max_size,newer_than,older_than}: files by name/size/date/text, with matching lines.",
	"ls":      "ls{path,recursive,include,exclude}: folder overview: counts, sizes, types, biggest files.",
	"read":    "read{path,mode:info|head|tail|lines|grep|text|image,from_line,lines,pattern,context,pages:\"3-5\",changes}: part of a file; Office/PDF as text, pictures via OCR.",
	"run":     "run{command,shell:bash|sh|pwsh,cwd,elevated,wait_for,keep_running,tail}: exit code, error lines, last lines.",
	"web":     "web{action:search|read|find|links|ask,query,url,urls[],pattern,question,from_line,lines}: pages as text; ask = answer from pages.",
	"ai":      "ai{action:ask|summarize|classify,paths[],question,categories[],move_to}: local AI reads files and pictures.",
	"office":  "office{action,path}\ncreate: .docx/.pdf from Markdown content; .pptx slides split by --- or headings; .xlsx sheets:[{name,rows}] or CSV content; .pdf from:[files, \"a.pdf:2-5\"].\nedit (backup kept; to=save as): replace:[{find,with}], delete_paragraphs, delete_slides:\"2,4\", content, insert_after; xlsx cells:{\"B2\":5}, append_rows; pdf pages:\"3,1-2\", rotate, stamp:\"{n}/{total}\", split.\nread. convert{to:\".pdf\" or file} via LibreOffice.",
	"rag":     "rag: search{query,k,under,ext,scope:all|files|notes,tag} remember{text,tags[]} notes{tag} forget{ids[]} update{paths[],dry_run} status config{roots[],types[],exclude[]}",
	"sys":     "sys{action:info|processes|ports|kill|setup,filter,port,pids[],name,what}: setup installs what ContextSaver needs, what=all|tools|compilers|office|chromium|llama|whisper|android|ndk|github (github gives a sign-in code for the user).",
	"files":   "files{steps:[{op,from[],to}],dry_run}: ops copy move sync delete(Trash) rename{rules:[{replace,with,template,prefix,suffix,case,ext}]} mkdir download extract edit{edits:[{find,replace}]} write{content}; per step: include exclude recursive newer_than min_size conflict:keep_both|skip|newer|overwrite.",
	"job":     "job{action:status|list|log|cancel|undo,id,wait_seconds}: log = new output since the last log (dev servers); cancel stops the whole process tree.",
	"saved":   "saved{action:list|show|save|remove,name,steps}",
	"browser": "browser{action:open|text|find|links|click|fill|press|wait|console|close,url,target,value,pattern,show}: real Chrome, text only; localhost:PORT works; console = page errors since the last call.",
	"window":  "window{action,window:title|program|pid}: list{filter} text ocr{x,y,w,h,boxes} copy wait{pattern} clipboard{text} screenshot{question}\ndo{steps:[{click:name}{type,into}{press}{wait}{click_text,nth}{click_at:[x,y]}{move:[x,y]}{drag:[x1,y1,x2,y2]}{scroll:n}{hold,secs}],button,then:\"text\"}",
	"media":   "media{action:info|transcript|frames,path,language,timestamps}: audio/video as text.",
	"git":     "git{action:status|diff|log|compare|commit|push|pull|clone|login|ci,path,to,message,paths[],full,ref,n,url,pair,wait_seconds}: never as admin; path may be owner/name[@ref] (read-only GitHub copy; pull on it updates it); compare = path vs to (folders or owner/name[@ref]): only-in, changed by lines, same; pair:true pairs x_windows.go/x_linux.go; paths[]+full:true = changed lines; clone without to = temp copy, an existing clone is updated; login = GitHub sign-in (gives a code for the user); ci = latest GitHub Actions run, failing lines only.",
	"check":   "check{path,action,fix,name,target,out}: check = lint/type-check; build = build+tests; test = tests (name filters); compile = runnable files for target linux|windows|android into dist/<target>. Failing lines only. Run after every code change.",
	"compile": "compile{path,target:linux|windows|android,out,release}: runnable files into out (default <project>/dist/<target>): Go, Gradle (.apk), Flutter, Rust, CMake/Make/C/C++, .NET, Electron, Python; one source file works too.",
	"test":    "test{path,name}: run the tests (only those matching name); failing lines only.",
	"outline": "outline{path,name}: functions/types with line numbers for a file or project; name filters. Read only the lines you need after.",
	"new":     "new{kind:go|android|web|python|rust|c,name,path,public,repo}: project with tests + GitHub Actions in ~/Projects/<name>, committed, pushed to a private GitHub repo.",
	"export":  "export{path,files:{\"rel/name\":\"text\"}}: write many files at once (undo: job undo). export{path,zip:true,out}: the project as a .zip (no .git, deps, builds).",
	"deps":    "deps{path,action:list|add|update|remove,name}: go/npm/cargo/pip(.venv)/gradle; returns the manifest lines that changed.",
	"release": "release{path,version,notes[]}: bump the version, commit, push, tag and publish the GitHub Release with notes (or let the repo's release workflow).",
	"logcat":  "logcat{app,lines,device}: crash and error lines of one app on the phone (adb; device IP:port for wireless).",
	"plan":    "plan{set:[steps]|add:step|done:n|show}: short task list for this session; set it first, mark each step done.",
	"phone":   phoneHelp,
}

const phoneHelp = `phone{action}: the user's Android phone (no adb needed). status (route, engine, battery) | apps{filter} | launch{app or package} | uninstall{app or package} | install: app (name of one of the user's own apps, from its GitHub releases), or repo (owner/name; tag optional), url (direct .apk link), path (.apk on the PC); test:true for debug builds | logcat{app,lines}: its crash/error lines (adb). The user approves installs/uninstalls in RemoteAI; just call it.
e.g. {"action":"install","app":"AdbSaver"}`

var helpKey = map[string]string{"scan": "ls", "system": "sys", "local_ai": "ai", "run_job": "files", "find": "find", "read": "read", "run": "run", "web": "web", "office": "office", "rag": "rag", "job": "job", "saved": "saved", "browser": "browser", "window": "window", "media": "media", "git": "git", "check": "check"}

var (
	jobHeadRe = regexp.MustCompile(`(?m)^Job \d{8}-\d{9}-[0-9a-f]{4}(?: "[^"\n]*")?: (done|exited|failed|canceled)`)
	exitedRe  = regexp.MustCompile(`exited with code (-?\d+) after ([\w.]+)`)
	lastAllRe = regexp.MustCompile(`(?m)^Last (\d+) of (\d+) lines:\n`)
	fullOutRe = regexp.MustCompile(`(?m)^Full output: .*\n?`)
)

// squeeze drops boilerplate from finished jobs: the job id (only a running job needs
// it), "Last N of N lines:", and the log path when every line was shown.
func squeeze(s string) string {
	s = jobHeadRe.ReplaceAllString(s, "$1")
	s = exitedRe.ReplaceAllString(s, "exit $1 ($2)")
	truncated := false
	s = lastAllRe.ReplaceAllStringFunc(s, func(m string) string {
		p := lastAllRe.FindStringSubmatch(m)
		if p[1] == p[2] {
			return ""
		}
		truncated = true
		return m
	})
	if !truncated && !strings.Contains(s, "Still running") {
		s = fullOutRe.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}
