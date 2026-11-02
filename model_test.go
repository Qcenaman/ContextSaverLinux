package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunningAIJobs(t *testing.T) {
	appHome = t.TempDir()
	d := filepath.Join(jobsDir(), "20261027-000000000-abcd")
	os.MkdirAll(d, 0o755)
	write := func(status, kind string, age time.Duration) {
		s := `{"name":"local AI ask","kind":"` + kind + `","status":"` + status + `","updated":"` + time.Now().Add(-age).Format(time.RFC3339Nano) + `"}`
		os.WriteFile(filepath.Join(d, "state.json"), []byte(s), 0o644)
	}
	write("running", "ai", 0)
	if len(runningAIJobs()) != 1 {
		t.Fatal("running ai job not seen")
	}
	write("running", "files", 0)
	if len(runningAIJobs()) != 0 {
		t.Fatal("file job counted")
	}
	write("running", "ai", 5*time.Minute)
	if len(runningAIJobs()) != 0 {
		t.Fatal("stale job counted")
	}
	write("done", "ai", 0)
	if n := waitForAIJobs(time.Second); n != "" {
		t.Fatal("waited for a finished job:", n)
	}
}
