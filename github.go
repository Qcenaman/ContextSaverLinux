package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GitHub through the GitHub CLI (gh): sign-in in the browser with a one-time code, and
// git's credential helper set to gh, so push, clone, ci and releases use that sign-in.

func ghPath() (string, error) {
	p, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("the GitHub CLI isn't installed: run contextsaver setup (or sudo apt install gh)")
	}
	return p, nil
}

func ghSignedIn() bool {
	gh, err := ghPath()
	if err != nil {
		return false
	}
	return exec.Command(gh, "auth", "status", "--hostname", "github.com").Run() == nil
}

// ghLoginArgs: workflow scope too, so pushes that change .github/workflows are allowed.
func ghLoginArgs() []string {
	return []string{"auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web", "--scopes", "workflow"}
}

func ghUser() string {
	out, err := runQuiet(15*time.Second, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

var (
	ghCodeRe = regexp.MustCompile(`([A-Z0-9]{4}-[A-Z0-9]{4})`)
	ghLogin  struct {
		sync.Mutex
		code    string
		running bool
		err     string
	}
)

// gitLogin signs in to GitHub: already signed in = who; else starts gh's browser login
// and returns the one-time code for the user. It finishes in the background.
func gitLogin() (string, error) {
	gh, err := ghPath()
	if err != nil {
		return "", err
	}
	if ghSignedIn() {
		exec.Command(gh, "auth", "setup-git").Run()
		return "signed in to GitHub as " + ghUser() + "; git uses it for push/pull/clone" + ensureGitIdentity(), nil
	}
	ghLogin.Lock()
	defer ghLogin.Unlock()
	if ghLogin.running && ghLogin.code != "" {
		return "waiting for the user: code " + ghLogin.code + " at https://github.com/login/device", nil
	}
	if ghLogin.err != "" {
		e := ghLogin.err
		ghLogin.err = ""
		return "", errors.New("last sign-in failed: " + e)
	}
	c := exec.Command(gh, ghLoginArgs()...)
	stdin, _ := c.StdinPipe()
	pr, pw := io.Pipe()
	c.Stdout, c.Stderr = pw, pw
	c.Env = append(os.Environ(), "GH_PROMPT_DISABLED=", "NO_COLOR=1")
	if err := c.Start(); err != nil {
		return "", err
	}
	codeCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sent, opened := false, false
		var tail []string
		for sc.Scan() {
			l := sc.Text()
			tail = append(tail, l)
			if m := ghCodeRe.FindStringSubmatch(l); m != nil && !sent {
				sent = true
				codeCh <- m[1]
			}
			lo := strings.ToLower(l)
			if strings.Contains(lo, "press enter") {
				stdin.Write([]byte("\n"))
				opened = true
			}
			if strings.Contains(lo, "github.com/login/device") && !opened {
				opened = true
				exec.Command("xdg-open", "https://github.com/login/device").Start()
			}
		}
		if !sent {
			codeCh <- ""
		}
		ghLogin.Lock()
		defer ghLogin.Unlock()
		ghLogin.running, ghLogin.code = false, ""
		if len(tail) > 3 {
			tail = tail[len(tail)-3:]
		}
		if c.ProcessState != nil && !c.ProcessState.Success() {
			ghLogin.err = strings.Join(tail, " ")
		}
	}()
	go func() {
		c.Wait()
		pw.Close()
		if c.ProcessState.Success() {
			exec.Command(gh, "auth", "setup-git").Run()
			ensureGitIdentity()
		}
	}()
	var code string
	select {
	case code = <-codeCh:
	case <-time.After(20 * time.Second):
	}
	if code == "" {
		c.Process.Kill()
		return "", errors.New("gh didn't give a sign-in code; the user can run in a terminal: gh " + strings.Join(ghLoginArgs(), " "))
	}
	ghLogin.running, ghLogin.code = true, code
	return "Tell the user: enter code " + code + " at https://github.com/login/device (opened in the browser) and approve. Then git{action:login} again confirms it.", nil
}

// publishRelease publishes vX as a GitHub Release with notes, unless the repo's own
// workflow does that when the version changes (then pushing was the release).
func publishRelease(root, v string, notes []string) (string, error) {
	if wf := releaseWorkflow(root); wf != "" {
		return "release " + v + " is published by " + wf + " on GitHub; git{action:ci} shows it", nil
	}
	gh, err := ghPath()
	if err != nil {
		return "", err
	}
	tag := "v" + strings.TrimPrefix(v, "v")
	if _, code, _ := gitExec(root, 20*time.Second, "rev-parse", "-q", "--verify", "refs/tags/"+tag); code != 0 {
		if out, code, err := gitExec(root, 20*time.Second, "tag", tag); err != nil || code != 0 {
			return "", fmt.Errorf("git tag failed: %s", firstErr(out, err))
		}
	}
	if out, code, err := gitExec(root, 2*time.Minute, append(append([]string{}, gitNetFlags...), "push", "origin", tag)...); err != nil || code != 0 {
		return "", fmt.Errorf("pushing tag %s failed: %s", tag, firstErr(out, err))
	}
	body := ""
	for _, n := range notes {
		body += "- " + strings.TrimLeft(strings.TrimSpace(n), "-* ") + "\n"
	}
	if body == "" {
		body = "Release " + tag
	}
	args := []string{"release", "create", tag, "--title", filepath.Base(root) + " " + strings.TrimPrefix(tag, "v"), "--notes", body}
	for _, f := range releaseFiles(root) {
		args = append(args, f)
	}
	c := exec.Command(gh, args...)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh release create failed: %s", clip(strings.TrimSpace(string(out)), 400))
	}
	return "released " + tag + ": " + strings.TrimSpace(string(out)), nil
}

// releaseWorkflow names a workflow that creates releases itself ("" = none).
func releaseWorkflow(root string) string {
	ws, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	for _, w := range ws {
		b, _ := os.ReadFile(w)
		s := string(b)
		if strings.Contains(s, "gh release create") || strings.Contains(s, "action-gh-release") || strings.Contains(s, "create-release") {
			return filepath.Base(w)
		}
	}
	return ""
}

// releaseFiles: what compile left in dist/ (attached to the release).
func releaseFiles(root string) []string {
	var out []string
	filepath.Walk(filepath.Join(root, "dist"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && fi.Size() > 0 {
			out = append(out, p)
		}
		return nil
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// ensureGitIdentity gives git a name and email when it has none: the GitHub login and its
// private no-reply address. It returns a note when it set them.
func ensureGitIdentity() string {
	name, _ := runQuiet(5*time.Second, "git", "config", "--global", "user.name")
	email, _ := runQuiet(5*time.Second, "git", "config", "--global", "user.email")
	if strings.TrimSpace(name) != "" && strings.TrimSpace(email) != "" {
		return ""
	}
	id, err1 := runQuiet(15*time.Second, "gh", "api", "user", "--jq", ".id")
	login, err2 := runQuiet(15*time.Second, "gh", "api", "user", "--jq", ".login")
	id, login = strings.TrimSpace(id), strings.TrimSpace(login)
	if err1 != nil || err2 != nil || login == "" {
		return ""
	}
	if strings.TrimSpace(name) == "" {
		exec.Command("git", "config", "--global", "user.name", login).Run()
	}
	if strings.TrimSpace(email) == "" {
		exec.Command("git", "config", "--global", "user.email", id+"+"+login+"@users.noreply.github.com").Run()
	}
	return "; git commits as " + login
}
