package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Local mode asks before anything risky: the first call returns a one-time token
// bound to the exact arguments; the model asks the user and calls again with it.

type pendingConfirm struct {
	hash    string
	expires time.Time
}

var (
	confirmMu sync.Mutex
	confirms  = map[string]pendingConfirm{}
)

func argsHash(c string, a map[string]any) string {
	b, _ := json.Marshal(a) // map keys are sorted
	h := sha256.Sum256(append([]byte(strings.ToLower(c)+"\x00"), b...))
	return hex.EncodeToString(h[:])
}

func newConfirm(c string, a map[string]any) string {
	var b [4]byte
	rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	confirmMu.Lock()
	for k, p := range confirms {
		if time.Now().After(p.expires) {
			delete(confirms, k)
		}
	}
	confirms[tok] = pendingConfirm{argsHash(c, a), time.Now().Add(10 * time.Minute)}
	confirmMu.Unlock()
	return tok
}

// useConfirm spends a token if it matches these exact arguments and hasn't expired.
func useConfirm(tok, c string, a map[string]any) bool {
	if tok == "" {
		return false
	}
	confirmMu.Lock()
	defer confirmMu.Unlock()
	p, ok := confirms[tok]
	if !ok || time.Now().After(p.expires) || p.hash != argsHash(c, a) {
		return false
	}
	delete(confirms, tok)
	return true
}

// riskyAction describes what a call would do if it changes, deletes, sends, runs or
// installs something; "" for read-only and harmless calls.
func riskyAction(c string, a map[string]any) string {
	if b, _ := a["dry_run"].(bool); b {
		return ""
	}
	act := strings.ToLower(str(a, "action"))
	short := func(s string) string { return clip1(s, 240) }
	args := func() string {
		b, _ := json.Marshal(a)
		return short(string(b))
	}
	tool := c
	if n, ok := compactAlias[c]; ok {
		tool = n
	}
	switch c {
	case "write", "save", "create":
		p := str(a, "path", "to", "file")
		if p != "" && fileExists(p) {
			return "replace existing file " + p
		}
		return ""
	case "edit", "replace":
		return "edit file in place " + str(a, "path", "file", "from")
	case "open", "launch", "start", "app":
		t := str(a, "target", "app", "path", "url", "name", "file")
		low := strings.ToLower(t)
		if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
			return ""
		}
		if fileExists(t) && !isExecutable(low) {
			return ""
		}
		return "start program " + t + " " + str(a, "args")
	case "remember", "load", "switch", "use", "help", "?", "more", "next", "model", "models", "me", "about", "user", "whoami":
		return ""
	case "phone", "android", "install_app", "apk", "mobile":
		return phoneRisky(a)
	case "export", "zip":
		if _, ok := a["files"]; ok {
			return "write files under " + str(a, "path")
		}
		return ""
	case "new", "scaffold":
		return "create project " + str(a, "name") + " and its GitHub repo"
	case "release":
		return "publish release " + str(a, "version") + " of " + str(a, "path")
	case "deps", "dependencies":
		if act != "" && act != "list" {
			return act + " dependency " + str(a, "name") + " in " + str(a, "path")
		}
		return ""
	case "compile", "build", "test", "tests":
		return "build or test " + str(a, "path")
	case "outline", "symbols", "logcat", "crash", "crashes", "login":
		return ""
	}
	switch tool {
	case "run":
		if b, _ := a["elevated"].(bool); b {
			return "run as ADMIN: " + short(str(a, "command", "cmd", "c", "script"))
		}
		return "run command: " + short(str(a, "command", "cmd", "c", "script"))
	case "run_job":
		return riskySteps(a)
	case "git":
		if act == "push" {
			return "git push " + str(a, "path")
		}
	case "check":
		if act == "build" || act == "test" || act == "compile" {
			return "build and run the tests of " + str(a, "path")
		}
	case "system":
		if act == "kill" {
			return "kill processes " + args()
		}
		if act == "setup" {
			return "install programs for ContextSaver (" + str(a, "what") + ")"
		}
	case "window":
		if act == "do" {
			return "click/type in a window " + args()
		}
	case "browser":
		if act == "click" || act == "fill" || act == "press" {
			return "browser " + act + " " + args()
		}
	case "office":
		if act == "edit" && str(a, "to") == "" {
			return "edit in place " + str(a, "path", "file")
		}
		if b, _ := a["overwrite"].(bool); b {
			return "overwrite " + str(a, "path", "file", "to")
		}
	case "saved":
		if act == "remove" {
			return "remove saved job " + str(a, "name")
		}
	case "job":
		if act == "undo" {
			return "undo job " + str(a, "id")
		}
	case "rag":
		if b, _ := a["confirm"].(bool); b {
			return "override the RAG index guard (" + act + "): " + args()
		}
		if act == "forget" {
			return "forget notes " + args()
		}
	case "local_ai":
		if act == "classify" && str(a, "move_to") != "" {
			return "move files into " + str(a, "move_to") + "\\<category>"
		}
	}
	return ""
}

func riskySteps(a map[string]any) string {
	steps, _ := a["steps"].([]any)
	var what []string
	for _, s := range steps {
		st, _ := s.(map[string]any)
		op := strings.ToLower(str(st, "op"))
		from := strings.Join(strList(st["from"]), ", ")
		to := str(st, "to")
		switch {
		case op == "delete":
			what = append(what, "delete "+from)
		case op == "move":
			what = append(what, "move "+from+" to "+to)
		case op == "rename":
			what = append(what, "rename in "+from)
		case op == "sync" && st["delete_extra"] == true:
			what = append(what, "sync "+from+" to "+to+" deleting extra files")
		case op == "edit":
			what = append(what, "edit "+from)
		case op == "write" && fileExists(to):
			what = append(what, "replace "+to)
		case op == "download":
			what = append(what, "download "+from)
		case str(st, "conflict") == "overwrite":
			what = append(what, op+" "+from+" overwriting files in "+to)
		}
	}
	if len(what) == 0 {
		return ""
	}
	return clip1(strings.Join(what, "; "), 400)
}

func strList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func isExecutable(low string) bool {
	for _, e := range []string{".exe", ".bat", ".cmd", ".ps1", ".msi", ".vbs", ".js", ".lnk", ".com", ".scr", ".sh", ".appimage", ".desktop", ".run", ".deb", ".py", ".bin"} {
		if strings.HasSuffix(low, e) {
			return true
		}
	}
	return false
}
