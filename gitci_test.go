package main

import (
	"strings"
	"testing"
	"time"
)

func TestFailedTail(t *testing.T) {
	log := "2026-10-31T10:00:00.1234567Z --- FAIL: TestX\n2026-10-31T10:00:00.2Z x_test.go:9: want 2\n2026-10-31T10:00:01Z ##[error]Process completed with exit code 1.\n2026-10-31T10:00:02Z Post job cleanup.\n2026-10-31T10:00:02Z noise\n"
	got := failedTail(log, 25)
	if !strings.Contains(got, "x_test.go:9: want 2") || strings.Contains(got, "2026-10-31T") || strings.Contains(got, "noise") {
		t.Fatalf("%q", got)
	}
}

func TestGitCI(t *testing.T) {
	dir := t.TempDir()
	if _, code, err := gitExec(dir, 20*time.Second, "init", "-q"); err != nil || code != 0 {
		t.Skip("git not available")
	}
	gitExec(dir, 20*time.Second, "remote", "add", "origin", "https://github.com/me/app.git")
	gitExec(dir, 20*time.Second, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x")
	head, _, _ := gitExec(dir, 20*time.Second, "rev-parse", "HEAD")
	old := ghAPI
	t.Cleanup(func() { ghAPI = old })
	var paths []string
	ghAPI = func(_, path string, _ any) ([]byte, int, error) {
		paths = append(paths, path)
		switch {
		case strings.HasSuffix(path, "/logs"):
			return []byte("2026-10-31T10:00:00.1Z FAIL x\n"), 200, nil
		case strings.Contains(path, "/jobs"):
			return []byte(`{"jobs":[{"id":9,"name":"test","conclusion":"failure","steps":[{"name":"go test","conclusion":"failure"}]}]}`), 200, nil
		}
		return []byte(`{"workflow_runs":[{"id":7,"status":"completed","conclusion":"failure","name":"test","head_sha":"` + strings.TrimSpace(head) + `"}]}`), 200, nil
	}
	out, err := gitCI(dir, gitArgs{})
	if err != nil || !strings.Contains(out, "failure") || !strings.Contains(out, "failing job: test, step: go test") || !strings.Contains(out, "FAIL x") {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.HasPrefix(paths[0], "/repos/me/app/actions/runs?branch=") {
		t.Fatalf("api path %s", paths[0])
	}
}
