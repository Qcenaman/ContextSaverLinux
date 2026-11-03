package main

// git tool: repository work in one call, always in the user's own session
// (never elevated: the Git credential store does not work as admin) and with
// the GitHub sign-in sent straight to the browser instead of the chooser box.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type gitArgs struct {
	Action   string   `json:"action"`
	Path     string   `json:"path"`
	Message  string   `json:"message"`
	Ref      string   `json:"ref"`
	URL      string   `json:"url"`
	To       string   `json:"to"`
	Version  string   `json:"version"`
	Notes    []string `json:"notes"`
	Paths    []string `json:"paths"`
	N        int      `json:"n"`
	Full     bool     `json:"full"`
	NoPush   bool     `json:"no_push"`
	DryRun   bool     `json:"dry_run"`
	MaxChars int      `json:"max_chars"`
	Wait     *int     `json:"wait_seconds"`
	Pair     bool     `json:"pair"`
}

// gitNetFlags makes Git Credential Manager open the browser sign-in directly
// (no auth-mode chooser window) and never ask on the console.
var gitNetFlags = []string{"-c", "credential.gitHubAuthModes=browser", "-c", "credential.interactive=auto"}

func gitExec(dir string, timeout time.Duration, args ...string) (string, int, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", -1, errors.New("git is not installed or not on PATH")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=auto")
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			code = 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		return strings.TrimSpace(stripCtl(out.String())), code, nil
	case <-time.After(timeout):
		cmd.Process.Kill()
		return strings.TrimSpace(out.String()), -1, fmt.Errorf("git %s timed out after %s", args[0], timeout)
	}
}

// gitRoot resolves the repo folder: the given path or the folder holding it.
func gitRoot(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("give path (the repo folder)")
	}
	p = filepath.Clean(p)
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("path not found: %s", p)
	}
	if !st.IsDir() {
		p = filepath.Dir(p)
	}
	out, code, err := gitExec(p, 20*time.Second, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("not a git repository: %s", p)
	}
	return filepath.FromSlash(strings.TrimSpace(out)), nil
}

func gitLastTag(root string) string {
	out, code, err := gitExec(root, 20*time.Second, "describe", "--tags", "--abbrev=0")
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// Version holders a release bumps. Group 1 keeps the prefix, group 3 the suffix.
var versionPatterns = []struct {
	glob string
	re   *regexp.Regexp
}{
	{"main.go", regexp.MustCompile(`(appVersion\s*=\s*")([^"]+)(")`)},
	{"version.go", regexp.MustCompile(`((?:appVersion|Version|version)\s*=\s*")([^"]+)(")`)},
	{"package.json", regexp.MustCompile(`("version"\s*:\s*")([^"]+)(")`)},
	{"pyproject.toml", regexp.MustCompile(`((?m)^version\s*=\s*")([^"]+)(")`)},
	{"app/build.gradle", regexp.MustCompile(`(versionName\s*=?\s*")([^"]+)(")`)},
	{"app/build.gradle.kts", regexp.MustCompile(`(versionName\s*=\s*")([^"]+)(")`)},
	{"Cargo.toml", regexp.MustCompile(`((?m)^version\s*=\s*")([^"]+)(")`)},
}

// bumpVersion rewrites the first version holder found under root. Returns the
// file changed and the old version, or "" when none was found.
func bumpVersion(root, version string, apply bool) (file, old string, err error) {
	for _, vp := range versionPatterns {
		p := filepath.Join(root, filepath.FromSlash(vp.glob))
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		m := vp.re.FindSubmatchIndex(b)
		if m == nil {
			continue
		}
		old = string(b[m[4]:m[5]])
		if apply && old != version {
			nb := append([]byte{}, b[:m[4]]...)
			nb = append(nb, version...)
			nb = append(nb, b[m[5]:]...)
			if werr := os.WriteFile(p, nb, 0o644); werr != nil {
				return vp.glob, old, werr
			}
		}
		return vp.glob, old, nil
	}
	return "", "", nil
}

var funcVersionRe = regexp.MustCompile(`(?m)^\*\*Version [^*]+\*\*`)

// "## What's new" with any apostrophe (straight or curly), plus its newline.
var whatsNewRe = regexp.MustCompile(`(?m)^## What.{1,3}s new[ \t]*\n`)

// updateFunctionsMD sets the version line and adds a "### <version>" block of
// notes under "## What's new" in FUNCTIONS.md (created when missing).
func updateFunctionsMD(root, version string, notes []string, apply bool) (string, error) {
	p := filepath.Join(root, "FUNCTIONS.md")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", nil
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if funcVersionRe.MatchString(s) {
		s = funcVersionRe.ReplaceAllString(s, "**Version "+version+"**")
	}
	if len(notes) > 0 {
		var blk strings.Builder
		fmt.Fprintf(&blk, "### %s\n", version)
		for _, n := range notes {
			n = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(n), "-*"))
			if n != "" {
				fmt.Fprintf(&blk, "- %s\n", n)
			}
		}
		if m := whatsNewRe.FindStringIndex(s); m != nil {
			at := m[1]
			if !strings.Contains(s[at:], "### "+version+"\n") {
				s = s[:at] + blk.String() + s[at:]
			}
		} else {
			s = strings.TrimRight(s, "\n") + "\n\n## What's new\n" + blk.String()
		}
	}
	if apply {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			return "FUNCTIONS.md", err
		}
	}
	return "FUNCTIONS.md", nil
}

func gitShort(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… %d more lines", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

func toolGit(raw json.RawMessage) (string, error) {
	var a gitArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		action = "status"
	}
	maxChars := a.MaxChars
	if maxChars <= 0 {
		maxChars = 6000
	}

	if action == "login" || action == "signin" || action == "auth" {
		return gitLogin()
	}
	if action == "clone" {
		return gitClone(a)
	}
	if action == "compare" {
		return gitCompare(a, maxChars)
	}

	p := a.Path
	if s, ok := parseRepoPath(p); ok {
		q, err := repoLocal(s)
		if err != nil {
			return "", err
		}
		p = q
	} else if q, err := expandPath(p); err == nil {
		p = q
	}
	root, err := gitRoot(p)
	if err != nil {
		return "", err
	}
	if isRepoCopy(root) {
		switch action {
		case "commit", "push", "release":
			return "", fmt.Errorf("%s is a temporary read-only copy of a GitHub repo: %s refused (clone it to a folder of your own first: git{action:clone,url,to})", root, action)
		case "pull": // the user wants the copy updated
			owner, name := repoOfCopy(root)
			msg, err := updateRepoCopy(owner, name)
			if err != nil {
				return "", err
			}
			if s, ok := parseRepoPath(a.Path); ok && s.Ref != "" {
				if wt, err := ensureRepo(owner, name, s.Ref); err == nil {
					root = wt
				}
			}
			return msg + "\n" + repoInfo(root), nil
		}
	}
	var b strings.Builder
	net := 5 * time.Minute

	switch action {
	case "ci", "actions":
		return gitCI(root, a)
	case "status":
		out, _, err := gitExec(root, 30*time.Second, "status", "--short", "--branch")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s\n%s\n", root, gitShort(out, 40))
		log, _, _ := gitExec(root, 30*time.Second, "log", "--oneline", "-5")
		if log != "" {
			fmt.Fprintf(&b, "Last commits:\n%s\n", log)
		}
		if t := gitLastTag(root); t != "" {
			fmt.Fprintf(&b, "Last tag: %s\n", t)
		}
		return b.String(), nil

	case "log":
		n := a.N
		if n <= 0 {
			n = 15
		}
		args := []string{"log", "--oneline", "--date=short", "--format=%h %ad %s", fmt.Sprintf("-%d", n)}
		if a.Ref != "" {
			args = append(args, a.Ref)
		}
		if len(a.Paths) > 0 {
			args = append(append(args, "--"), a.Paths...)
		}
		out, _, err := gitExec(root, 30*time.Second, args...)
		return out, err

	case "diff":
		args := []string{"diff", "--stat"}
		if a.Ref != "" {
			args = append(args, a.Ref)
		} else {
			args = append(args, "HEAD")
		}
		if len(a.Paths) > 0 {
			args = append(append(args, "--"), a.Paths...)
		}
		stat, _, err := gitExec(root, 60*time.Second, args...)
		if err != nil {
			return "", err
		}
		if stat == "" {
			stat = "(no changes)"
		}
		b.WriteString(stat)
		if a.Full {
			fargs := append([]string{"diff"}, args[2:]...)
			full, _, _ := gitExec(root, 60*time.Second, fargs...)
			if len(full) > maxChars {
				full = full[:maxChars] + fmt.Sprintf("\n… cut at %d chars (max_chars)", maxChars)
			}
			b.WriteString("\n\n" + full)
		}
		return b.String(), nil

	case "commit":
		if strings.TrimSpace(a.Message) == "" {
			return "", errors.New("commit needs message")
		}
		add := []string{"add", "-A"}
		if len(a.Paths) > 0 {
			add = append([]string{"add", "--"}, a.Paths...)
		}
		if out, code, err := gitExec(root, time.Minute, add...); err != nil || code != 0 {
			return "", fmt.Errorf("git add failed: %s", firstErr(out, err))
		}
		staged, _, _ := gitExec(root, time.Minute, "diff", "--cached", "--stat")
		if strings.TrimSpace(staged) == "" {
			return "nothing to commit", nil
		}
		if a.DryRun {
			return "would commit:\n" + staged, nil
		}
		out, code, err := gitExec(root, time.Minute, "commit", "-q", "-m", a.Message)
		if err != nil || code != 0 {
			return "", fmt.Errorf("git commit failed: %s", firstErr(out, err))
		}
		h, _, _ := gitExec(root, 20*time.Second, "rev-parse", "--short", "HEAD")
		fmt.Fprintf(&b, "committed %s: %s\n%s", h, a.Message, staged)
		if !a.NoPush && a.Full {
			p, err := gitPush(root, net)
			if err != nil {
				return b.String(), err
			}
			b.WriteString("\n" + p)
		}
		return b.String(), nil

	case "push":
		return gitPush(root, net)

	case "pull":
		args := append(append([]string{}, gitNetFlags...), "pull", "--ff-only")
		out, code, err := gitExec(root, net, args...)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", fmt.Errorf("git pull failed:\n%s", gitShort(out, 15))
		}
		return gitShort(out, 20), nil

	case "release":
		v := strings.TrimPrefix(strings.TrimSpace(a.Version), "v")
		if v == "" {
			return "", errors.New("release needs version (e.g. 1.19.0)")
		}
		apply := !a.DryRun
		file, old, err := bumpVersion(root, v, apply)
		if err != nil {
			return "", err
		}
		if file == "" {
			fmt.Fprintf(&b, "no version holder found (main.go appVersion, package.json, pyproject.toml, build.gradle); only FUNCTIONS.md and the commit carry %s\n", v)
		} else {
			fmt.Fprintf(&b, "%s: %s -> %s\n", file, old, v)
		}
		if f, err := updateFunctionsMD(root, v, a.Notes, apply); err != nil {
			return "", err
		} else if f != "" {
			fmt.Fprintf(&b, "%s: version line and %d note(s) under What's new\n", f, len(a.Notes))
		}
		msg := strings.TrimSpace(a.Message)
		if msg == "" && len(a.Notes) > 0 {
			msg = strings.TrimSpace(strings.TrimLeft(a.Notes[0], "-* "))
		}
		if msg == "" {
			msg = "release"
		}
		if !strings.HasPrefix(msg, v) {
			msg = v + ": " + msg
		}
		if a.DryRun {
			staged, _, _ := gitExec(root, time.Minute, "status", "--short")
			fmt.Fprintf(&b, "would commit %q and push:\n%s", msg, staged)
			return b.String(), nil
		}
		if out, code, err := gitExec(root, time.Minute, "add", "-A"); err != nil || code != 0 {
			return b.String(), fmt.Errorf("git add failed: %s", firstErr(out, err))
		}
		if out, code, err := gitExec(root, time.Minute, "commit", "-q", "-m", msg); err != nil || code != 0 {
			return b.String(), fmt.Errorf("git commit failed: %s", firstErr(out, err))
		}
		h, _, _ := gitExec(root, 20*time.Second, "rev-parse", "--short", "HEAD")
		fmt.Fprintf(&b, "committed %s: %s\n", h, msg)
		if a.NoPush {
			b.WriteString("not pushed (no_push)")
			return b.String(), nil
		}
		p, err := gitPush(root, net)
		if err != nil {
			return b.String(), err
		}
		b.WriteString(p + "\n")
		r, err := publishRelease(root, v, a.Notes)
		if err != nil {
			return b.String(), err
		}
		b.WriteString(r)
		return b.String(), nil

	case "handoff":
		return gitHandoff(root, a, maxChars)
	}
	return "", fmt.Errorf("unknown action %q (status, log, diff, compare, commit, push, pull, clone, release, handoff)", action)
}

func firstErr(out string, err error) string {
	if err != nil {
		return err.Error()
	}
	return gitShort(out, 8)
}

// gitPush pushes the current branch, setting the upstream when it has none.
// Runs without admin and sends any GitHub sign-in straight to the browser.
func gitPush(root string, timeout time.Duration) (string, error) {
	args := append(append([]string{}, gitNetFlags...), "push")
	if _, code, _ := gitExec(root, 20*time.Second, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); code != 0 {
		args = append(args, "-u", "origin", "HEAD")
	}
	out, code, err := gitExec(root, timeout, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		hint := ""
		lo := strings.ToLower(out)
		if strings.Contains(lo, "authentication") || strings.Contains(lo, "could not read username") || strings.Contains(lo, "403") {
			hint = "\nSign-in needed: git{action:login}, then retry."
		}
		return "", fmt.Errorf("git push failed:\n%s%s", gitShort(out, 15), hint)
	}
	if strings.TrimSpace(out) == "" {
		out = "pushed"
	}
	return gitShort(out, 12), nil
}

// gitHandoff writes a handoff for another project/chat from the commits and
// diff since ref (default: last tag), summarized by the local AI when it can.
func gitHandoff(root string, a gitArgs, maxChars int) (string, error) {
	since := strings.TrimSpace(a.Ref)
	if since == "" {
		since = gitLastTag(root)
	}
	if since == "" {
		n := a.N
		if n <= 0 {
			n = 10
		}
		since = fmt.Sprintf("HEAD~%d", n)
	}
	rng := since + "..HEAD"
	log, _, _ := gitExec(root, time.Minute, "log", "--format=%h %s", rng)
	stat, _, _ := gitExec(root, time.Minute, "diff", "--stat", rng)
	if strings.TrimSpace(log) == "" && strings.TrimSpace(stat) == "" {
		return "", fmt.Errorf("no changes since %s", since)
	}
	full, _, _ := gitExec(root, 2*time.Minute, "diff", rng, "--", ".", ":(exclude)*.lock", ":(exclude)*.sum", ":(exclude)*.min.js")
	if len(full) > 120000 {
		full = full[:120000] + "\n… diff cut here"
	}
	repo := filepath.Base(root)
	head := fmt.Sprintf("# Handoff: %s (%s)\n\nCommits:\n%s\n\nFiles:\n%s\n", repo, rng, log, stat)

	dir := filepath.Join(appHome, "cache", "handoff")
	os.MkdirAll(dir, 0o755)
	src := filepath.Join(dir, slug(repo+"-"+since)+".txt")
	body := head
	if err := os.WriteFile(src, []byte(head+"\nDiff:\n"+full), 0o644); err == nil {
		q := "Write a handoff note for another developer/AI who will continue in a related project. Use this exact Markdown layout: '## What changed' (grouped by feature, plain language), '## New or changed interfaces' (tool names, actions, parameters, file formats, config keys, ports — exact names), '## Decisions and constraints' (things that must stay the same), '## Still open'. Be concrete and complete but short; no praise, no filler."
		if a.Message != "" {
			q += " Extra context from the user: " + a.Message
		}
		args, _ := json.Marshal(map[string]any{"action": "ask", "paths": []string{src}, "question": q, "wait_seconds": waitSecs(a.Wait, 50, 50)})
		if s, err := toolLocalAI(args); err == nil && !strings.Contains(s, "WARNING") && strings.TrimSpace(s) != "" {
			body = head + "\n" + s + "\n"
		} else if err == nil && strings.Contains(s, "WARNING") {
			body = head + "\n(local AI not used: " + strings.TrimSpace(gitShort(s, 3)) + ")\n"
		}
	}
	if a.To != "" {
		if err := os.WriteFile(a.To, []byte(body), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("handoff written to %s (%d chars)\n%s", a.To, len(body), clip(body, maxChars)), nil
	}
	return clip(body, maxChars), nil
}

// gitClone: url = owner/name or a git URL. Without to (or path) a GitHub repo becomes a
// temporary read-only copy; a destination that already is a clone of url is updated
// (pull --ff-only) instead of failing. Answers with path, branch and last commit.
func gitClone(a gitArgs) (string, error) {
	u := strings.TrimSpace(a.URL)
	to := strings.TrimSpace(a.To)
	if to == "" {
		to = strings.TrimSpace(a.Path)
	}
	if u == "" && isRepoPath(to) {
		u, to = to, ""
	}
	if u == "" {
		return "", errors.New("clone needs url (owner/name or a git URL)")
	}
	if to == "" {
		s, ok := parseRepoPath(u)
		if !ok {
			return "", errors.New("clone needs to (target folder) for a repo that isn't on GitHub")
		}
		if s.Ref == "" {
			s.Ref = strings.TrimSpace(a.Ref)
		}
		s.Sub = ""
		dir, err := repoLocal(s)
		if err != nil {
			return "", err
		}
		return "temporary read-only copy (updated only when the user wants it: git pull on it; removed after 24 h unused)\n" + repoInfo(dir), nil
	}
	if !strings.Contains(u, "://") && !strings.HasPrefix(u, "git@") {
		if s, ok := parseRepoPath(u); ok {
			u = "https://github.com/" + s.Owner + "/" + s.Name
			if a.Ref == "" {
				a.Ref = s.Ref
			}
		}
	}
	dest, err := expandPath(to)
	if err != nil {
		return "", err
	}
	if isGitWork(dest) {
		origin, _, _ := gitExec(dest, 20*time.Second, "remote", "get-url", "origin")
		if normRemote(origin) != normRemote(u) {
			return "", fmt.Errorf("%s is already a clone of %s, not of %s", dest, strings.TrimSpace(origin), u)
		}
		args := append(append([]string{}, gitNetFlags...), "pull", "--ff-only", "--quiet")
		out, code, err := gitExec(dest, 5*time.Minute, args...)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", fmt.Errorf("already cloned there; git pull --ff-only failed:\n%s", gitShort(out, 10))
		}
		return "already cloned: updated (pull --ff-only)\n" + repoInfo(dest), nil
	}
	cmd := "git " + strings.Join(gitNetFlags, " ") + " clone --progress"
	if a.Ref != "" {
		cmd += " --branch " + a.Ref
	}
	cmd += fmt.Sprintf(" %q %q", u, dest)
	rargs, _ := json.Marshal(map[string]any{"command": cmd, "name": "git clone", "tail": 5, "wait_seconds": waitSecs(a.Wait, 45, 50), "env": map[string]string{"GIT_TERMINAL_PROMPT": "0"}})
	out, err := toolRun(rargs)
	if err == nil && isGitWork(dest) {
		if _, code, _ := gitExec(dest, 20*time.Second, "rev-parse", "--verify", "-q", "HEAD"); code == 0 {
			out += "\n" + repoInfo(dest)
		}
	}
	return out, err
}
