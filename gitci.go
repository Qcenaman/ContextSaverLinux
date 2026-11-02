package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// git ci: the branch's latest GitHub Actions run through the GitHub API, signed in with
// the token Git already stores for github.com (Git Credential Manager), so no extra
// login is needed. Waits up to wait_seconds; on failure only the failing step's last
// lines come back.

type ghRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Name       string `json:"name"`
	Sha        string `json:"head_sha"`
	URL        string `json:"html_url"`
}

var (
	ghTokenOnce sync.Once
	ghToken     string
)

// githubToken reads the github.com password Git has stored, without prompting.
func githubToken() string {
	ghTokenOnce.Do(func() {
		if t := os.Getenv("GH_TOKEN"); t != "" {
			ghToken = t
			return
		}
		cmd := exec.Command("git", "credential", "fill")
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
		hideWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			return
		}
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "password="); ok {
				ghToken = v
			}
		}
	})
	return ghToken
}

// ghAPI calls the GitHub REST API (a var so tests can fake it).
var ghAPI = func(method, path string, body any) ([]byte, int, error) {
	tok := githubToken()
	if tok == "" {
		return nil, 0, errors.New("no GitHub sign-in stored for git: push once (git{action:push}) so Git signs in, then retry")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://api.github.com"+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, nil
}

var ghRemoteRe = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/\s]+?)(?:\.git)?/?$`)

func ghRepoOf(root string) (string, error) {
	u, _, err := gitExec(root, 20*time.Second, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	m := ghRemoteRe.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", fmt.Errorf("origin isn't a GitHub repo: %s", strings.TrimSpace(u))
	}
	return m[1] + "/" + m[2], nil
}

var ciPoll = 10 * time.Second

func gitCI(root string, a gitArgs) (string, error) {
	repo, err := ghRepoOf(root)
	if err != nil {
		return "", err
	}
	branch := a.Ref
	if branch == "" {
		b, _, err := gitExec(root, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", err
		}
		branch = strings.TrimSpace(b)
	}
	head, _, _ := gitExec(root, 20*time.Second, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	wait := waitSecs(a.Wait, 0, 600)
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	var r ghRun
	for {
		b, code, err := ghAPI("GET", fmt.Sprintf("/repos/%s/actions/runs?branch=%s&per_page=1", repo, branch), nil)
		if err != nil {
			return "", err
		}
		if code != 200 {
			return "", fmt.Errorf("GitHub API %d: %s", code, clip1(string(b), 200))
		}
		var rs struct {
			Runs []ghRun `json:"workflow_runs"`
		}
		json.Unmarshal(b, &rs)
		if len(rs.Runs) == 0 {
			if time.Now().After(deadline) {
				return "no Actions runs for branch " + branch + " (is there a .github/workflows file, and was it pushed?)", nil
			}
		} else {
			r = rs.Runs[0]
			fresh := head == "" || r.Sha == head
			if fresh && r.Status == "completed" || time.Now().After(deadline) {
				break
			}
		}
		time.Sleep(ciPoll)
	}
	sha := r.Sha
	if len(sha) > 7 {
		sha = sha[:7]
	}
	stale := ""
	if head != "" && r.Sha != head {
		stale = " (for an older commit; the latest push hasn't started a run yet)"
	}
	if r.Status != "completed" {
		return fmt.Sprintf("%s %s: %s%s. Check again with git{action:ci,wait_seconds:120}", r.Name, sha, r.Status, stale), nil
	}
	res := fmt.Sprintf("%s %s: %s%s", r.Name, sha, r.Conclusion, stale)
	if r.Conclusion == "success" {
		return res, nil
	}
	if tail := ciFailure(repo, r.ID); tail != "" {
		return res + "\n" + tail, nil
	}
	return res + "\n" + r.URL, nil
}

// ciFailure: the failing job's failing step and the last lines of that job's log.
func ciFailure(repo string, runID int64) string {
	b, code, err := ghAPI("GET", fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", repo, runID), nil)
	if err != nil || code != 200 {
		return ""
	}
	var js struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	json.Unmarshal(b, &js)
	for _, j := range js.Jobs {
		if j.Conclusion != "failure" {
			continue
		}
		step := ""
		for _, s := range j.Steps {
			if s.Conclusion == "failure" {
				step = s.Name
				break
			}
		}
		log, code, err := ghAPI("GET", fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo, j.ID), nil)
		head := "failing job: " + j.Name
		if step != "" {
			head += ", step: " + step
		}
		if err != nil || code != 200 {
			return head
		}
		return head + "\n" + failedTail(string(log), 25)
	}
	return ""
}

var logStampRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)

// failedTail keeps the last n useful lines of a job log, without timestamps and the
// runner's cleanup noise after the error.
func failedTail(log string, n int) string {
	lines := strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n")
	var out []string
	for _, l := range lines {
		if parts := strings.SplitN(l, "\t", 3); len(parts) == 3 {
			l = parts[2]
		}
		l = logStampRe.ReplaceAllString(l, "")
		if strings.HasPrefix(l, "Post job cleanup") || strings.HasPrefix(l, "Cleaning up orphan") {
			break
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "##[group]") && !strings.HasPrefix(l, "##[endgroup]") {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return strings.Join(out, "\n")
}
