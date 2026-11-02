package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Job-end signals. Claude's cloud workspace can't reach this PC, but both reach GitHub.
// When a watched job ends (done, failed, canceled or interrupted), ContextSaver adds a tiny
// commit to the "signals" branch of the app's own repo; a watcher in Claude's workspace
// waits for that commit in a shell loop, so nothing polls this PC and no tokens are spent.

const signalBranch = "signals"

var (
	notifierOnce sync.Once
	signalMu     sync.Mutex
	slugRe       = regexp.MustCompile(`github\.com[:/]+([^/]+/[^/]+?)(?:\.git)?/?$`)
)

func notifyDir() string { return filepath.Join(appHome, "notify") }

func signalRepo() string {
	if cfg.NotifyRepo != "" {
		return cfg.NotifyRepo
	}
	return appRoot()
}

func gitRun(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", append([]string{"-C", signalRepo()}, args...)...)
	c.SysProcAttr = quietCmd("git").SysProcAttr
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never",
		"GIT_AUTHOR_NAME=ContextSaver", "GIT_AUTHOR_EMAIL=contextsaver@localhost",
		"GIT_COMMITTER_NAME=ContextSaver", "GIT_COMMITTER_EMAIL=contextsaver@localhost")
	c.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], clip(msg, 300))
	}
	return strings.TrimSpace(out.String()), nil
}

func repoSlug() (string, error) {
	u, err := gitRun("", "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	m := slugRe.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", fmt.Errorf("origin of %s isn't a GitHub repo (%s)", signalRepo(), u)
	}
	return m[1], nil
}

// sendSignal adds one commit (empty tree, message = msg) on top of the signals branch and pushes it.
func sendSignal(msg string) error {
	signalMu.Lock()
	defer signalMu.Unlock()
	tree, err := gitRun("", "mktree")
	if err != nil {
		return err
	}
	ref := "refs/remotes/origin/" + signalBranch
	var last error
	for try := 0; try < 4; try++ {
		args := []string{"commit-tree", tree, "-m", msg}
		if _, err := gitRun("", "fetch", "-q", "origin", "+refs/heads/"+signalBranch+":"+ref); err == nil {
			if p, err := gitRun("", "rev-parse", "--verify", "-q", ref); err == nil && p != "" {
				args = append(args, "-p", p)
			}
		}
		sha, err := gitRun("", args...)
		if err != nil {
			return err
		}
		if _, err := gitRun("", "push", "-q", "origin", sha+":refs/heads/"+signalBranch); err != nil {
			last = err
			time.Sleep(time.Duration(2+try*3) * time.Second)
			continue
		}
		gitRun("", "update-ref", ref, sha)
		return nil
	}
	return last
}

func signalText(s *JobState) string {
	lines := strings.Split(formatState(s), "\n")
	var keep []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "Log: ") || strings.HasPrefix(l, "Full output: ") {
			continue
		}
		keep = append(keep, l)
		if len(keep) >= 5 {
			break
		}
	}
	return fmt.Sprintf("ts %s %s: %s", s.ID, s.Status, clip(strings.Join(keep, " · "), 600))
}

// watchCommand is what Claude runs in its own workspace (in the background) to be told when the job ends.
func watchCommand(slug, id string) string {
	return fmt.Sprintf(`until m=$(gh api "repos/%s/commits?sha=%s&per_page=50" --jq '.[].commit.message' 2>/dev/null | grep -m1 "^ts %s "); do sleep 20; done; echo "$m"`, slug, signalBranch, id)
}

// requestNotify records that job id should send a signal when it ends.
func requestNotify(id string) (string, error) {
	slug, err := repoSlug()
	if err != nil {
		return "", fmt.Errorf("can't notify: %v", err)
	}
	if err := os.MkdirAll(notifyDir(), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(notifyDir(), id), []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		return "", err
	}
	startNotifier()
	return fmt.Sprintf("Notify on: when job %s ends (done, failed, canceled or interrupted) ContextSaver commits \"ts %s <status>: <summary>\" to the %s branch of github.com/%s.\n"+
		"To be told without checking the PC, run this in your own workspace shell with Bash run_in_background (timeout 7200000; the repo must be attached to the session; re-run it if it times out):\n%s",
		id, id, signalBranch, slug, watchCommand(slug, id)), nil
}

// withNotify adds the notify instructions to a tool result when the job is still running.
func withNotify(text string, s *JobState, notify bool) string {
	if !notify {
		return text
	}
	if s.Status != "running" {
		return text + "\n(notify: the job already ended, nothing to wait for)"
	}
	msg, err := requestNotify(s.ID)
	if err != nil {
		return text + "\nnotify failed: " + err.Error()
	}
	return text + "\n" + msg
}

func startNotifier() {
	notifierOnce.Do(func() {
		go func() {
			tries := map[string]int{}
			next := map[string]time.Time{}
			for {
				checkNotifies(tries, next)
				time.Sleep(10 * time.Second)
			}
		}()
	})
}

func checkNotifies(tries map[string]int, next map[string]time.Time) {
	ents, err := os.ReadDir(notifyDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		id := e.Name()
		if time.Now().Before(next[id]) {
			continue
		}
		src := filepath.Join(notifyDir(), id)
		s, err := loadState(id)
		if err != nil {
			moveNotify(src, "failed", id)
			continue
		}
		if s.Status == "running" {
			continue
		}
		if err := sendSignal(signalText(s)); err != nil {
			tries[id]++
			next[id] = time.Now().Add(time.Duration(min(tries[id], 10)) * time.Minute)
			os.WriteFile(src+".error.txt", []byte(time.Now().Format(time.RFC3339)+" "+err.Error()), 0o644)
			if tries[id] >= 30 {
				moveNotify(src, "failed", id)
			}
			continue
		}
		moveNotify(src, "sent", id)
		delete(tries, id)
		delete(next, id)
	}
}

func moveNotify(src, sub, id string) {
	dir := filepath.Join(notifyDir(), sub)
	os.MkdirAll(dir, 0o755)
	os.Rename(src, filepath.Join(dir, id))
	if exists(src + ".error.txt") {
		os.Rename(src+".error.txt", filepath.Join(dir, id+".error.txt"))
	}
}

// resumeNotifier starts the watcher at startup when requests were left from an earlier run.
func resumeNotifier() {
	ents, err := os.ReadDir(notifyDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".txt") {
			startNotifier()
			return
		}
	}
}
