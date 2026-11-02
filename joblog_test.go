package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestJobLogIncremental(t *testing.T) {
	appHome = t.TempDir()
	id := "20261031-000000000-beef"
	os.MkdirAll(filepath.Join(jobsDir(), id), 0o755)
	p := outputPath(id)
	os.WriteFile(p, []byte("one\ntwo\npart"), 0o644)
	out, _ := jobLog(id, 60)
	if !strings.Contains(out, "2 new line(s)") || !strings.Contains(out, "one\ntwo") || strings.Contains(out, "part") {
		t.Fatalf("first: %q", out)
	}
	if out, _ := jobLog(id, 60); !strings.HasPrefix(out, "no new output") {
		t.Fatalf("second: %q", out)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("ial\nthree\n")
	f.Close()
	out, _ = jobLog(id, 60)
	if !strings.Contains(out, "partial\nthree") || strings.Contains(out, "one") {
		t.Fatalf("third: %q", out)
	}
}

// job cancel stops a keep_running command and the processes it started.
func TestJobCancelStopsTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows process tree")
	}
	appHome = t.TempDir()
	b, _ := json.Marshal(map[string]any{"command": "cmd /c ping -n 120 127.0.0.1", "keep_running": true, "wait_seconds": 2})
	out, err := toolRun(b)
	if err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`\d{8}-\d{9}-[0-9a-f]{4}`).FindString(out)
	if id == "" {
		t.Fatalf("no job id: %s", out)
	}
	st, _ := loadState(id)
	if st == nil || st.PID == 0 {
		t.Fatalf("no pid: %+v", st)
	}
	time.Sleep(time.Second)
	kids := pingPIDs()
	if len(kids) == 0 {
		t.Fatal("child ping.exe not found")
	}
	cancelJob(id)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		alive := pidAlive(st.PID)
		for _, k := range kids {
			alive = alive || pidAlive(k)
		}
		if !alive {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("process tree still running after cancel (pid %d, children %v)", st.PID, kids)
}

// pingPIDs finds the test's child (ping -n 120), started by cmd /c.
func pingPIDs() []int {
	ps, err := procs()
	if err != nil {
		return nil
	}
	var out []int
	for _, p := range ps {
		if strings.EqualFold(p.name, "PING.EXE") && strings.Contains(p.cmd, "-n 120") {
			out = append(out, int(p.pid))
		}
	}
	return out
}
