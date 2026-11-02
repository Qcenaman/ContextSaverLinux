package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLlama is a llama.cpp server: /health, /slots and /v1/models.
type fakeLlama struct {
	srv   *httptest.Server
	port  int
	busy  atomic.Bool
	task  atomic.Int64
	slots bool
}

func newFakeLlama(t *testing.T, model string, slots bool) *fakeLlama {
	f := &fakeLlama{slots: slots}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/slots":
			if !f.slots {
				http.Error(w, "slots disabled", 501)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{{"id": 0, "id_task": f.task.Load(), "is_processing": f.busy.Load()}})
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	u, _ := url.Parse(f.srv.URL)
	f.port, _ = strconv.Atoi(u.Port())
	return f
}

type guardFake struct {
	pids    map[int]int        // port -> pid listening
	servers map[int]*fakeLlama // pid -> server (closed when killed)
	killed  []int
	now     time.Time
	started int
}

// guardEnv swaps every process/clock hook for fakes and gives ContextSaver an empty data folder.
func guardEnv(t *testing.T) *guardFake {
	g := &guardFake{pids: map[int]int{}, servers: map[int]*fakeLlama{}, now: time.Now()}
	o1, o2, o3, o4, o5, o6, o7, o8 := guardPIDOnPort, guardPIDAlive, guardKill, guardNow, guardForeign, modelGPUBusy, guardStartScript, lmsBase
	oc, oh := cfg, appHome
	t.Cleanup(func() {
		guardPIDOnPort, guardPIDAlive, guardKill, guardNow, guardForeign, modelGPUBusy, guardStartScript, lmsBase = o1, o2, o3, o4, o5, o6, o7, o8
		cfg, appHome = oc, oh
	})
	appHome = t.TempDir()
	os.MkdirAll(filepath.Join(appHome, "jobs"), 0o755)
	cfg = Config{Frontend: "anythingllm"}
	lmsBase = "http://127.0.0.1:1" // no LM Studio
	guardPIDOnPort = func(port int) int { return g.pids[port] }
	guardPIDAlive = func(pid int) bool {
		for _, p := range g.pids {
			if p == pid && pid > 0 {
				return true
			}
		}
		return false
	}
	guardKill = func(pid int) {
		g.killed = append(g.killed, pid)
		for port, p := range g.pids {
			if p == pid {
				delete(g.pids, port)
			}
		}
		if s := g.servers[pid]; s != nil {
			s.srv.Close()
		}
	}
	guardNow = func() time.Time { return g.now }
	guardForeign = func(int) []string { return nil }
	modelGPUBusy = func(int) string { return "" }
	guardStartScript = func(string) (*aiProc, error) {
		g.started++
		return &aiProc{done: make(chan struct{})}, nil
	}
	return g
}

// serve puts f on its port under pid; owned records it as started by ContextSaver.
func (g *guardFake) serve(f *fakeLlama, pid int, owned bool) {
	g.pids[f.port], g.servers[pid] = pid, f
	if owned {
		registerOwned(ownedServer{Port: f.port, PID: pid, Model: "qwen"})
	}
}

func setConfig(t *testing.T, kv map[string]any) {
	b, _ := json.Marshal(kv)
	if err := os.WriteFile(filepath.Join(appHome, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExternalServerNeverStopped(t *testing.T) {
	g := guardEnv(t)
	f := newFakeLlama(t, "qwen3.6-27b", true)
	g.serve(f, 4242, false) // started by RemoteAI / by hand
	switchSetup(t, g, f.port)
	g.now = g.now.Add(24 * time.Hour)
	if st, _ := inspectServer(f.port); st.Owner != ownerExt {
		t.Fatalf("owner %q, want external", st.Owner)
	}
	if ok, _ := guardStop(f.port, autoStop, false); ok {
		t.Fatal("idle stop stopped an external server")
	}
	if ok, why := guardStop(f.port, manualStop, false); ok || !strings.Contains(why, "wasn't started by ContextSaver") {
		t.Fatalf("manual stop of an external server: %v %q", ok, why)
	}
	idleSweep() // app exit
	if out := stopModel(false); !strings.Contains(out, "nothing stopped") {
		t.Fatalf("stopModel: %s", out)
	}
	if len(g.killed) != 0 {
		t.Fatalf("killed %v", g.killed)
	}
}

func TestBusyServerNeverStopped(t *testing.T) {
	g := guardEnv(t)
	f := newFakeLlama(t, "qwen3.6-27b", true)
	g.serve(f, 100, true)
	f.busy.Store(true)
	g.now = g.now.Add(time.Hour)
	for _, c := range []struct {
		mode  stopMode
		force bool
	}{{autoStop, false}, {manualStop, false}, {manualStop, true}} {
		if ok, why := guardStop(f.port, c.mode, c.force); ok || !strings.Contains(why, "busy") {
			t.Fatalf("mode %d force %v: %v %q", c.mode, c.force, ok, why)
		}
	}
	idleSweep()
	if len(g.killed) != 0 {
		t.Fatalf("killed %v", g.killed)
	}
}

func TestIdleTimeoutStopsOnlyOwnedIdle(t *testing.T) {
	g := guardEnv(t)
	own := newFakeLlama(t, "qwen3.6-27b", true)
	ext := newFakeLlama(t, "gpt-oss-120b", true)
	g.serve(own, 100, true)
	g.serve(ext, 200, false)
	setConfig(t, map[string]any{"model_idle_minutes": 15})
	inspectServer(own.port) // first look: activity now

	g.now = g.now.Add(10 * time.Minute)
	own.task.Add(1) // another client (RemoteAI) sent a request: idle time starts over
	if s := idleSweep(); len(s) != 0 {
		t.Fatalf("stopped too early: %v", s)
	}
	g.now = g.now.Add(14 * time.Minute)
	if s := idleSweep(); len(s) != 0 {
		t.Fatalf("stopped 14 min after the last request: %v", s)
	}
	g.now = g.now.Add(2 * time.Minute)
	if s := idleSweep(); len(s) != 1 {
		t.Fatalf("owned idle server not stopped: %v", s)
	}
	if fmt.Sprint(g.killed) != "[100]" {
		t.Fatalf("killed %v, want only the owned server", g.killed)
	}
	if _, ok := readReg().Servers[strconv.Itoa(own.port)]; ok {
		t.Fatal("stopped server still registered")
	}
}

// A server whose /slots and /metrics don't answer can't be checked for other clients.
func TestUnknownBusyNotAutoStopped(t *testing.T) {
	g := guardEnv(t)
	f := newFakeLlama(t, "qwen3.6-27b", false)
	g.serve(f, 100, true)
	g.now = g.now.Add(time.Hour)
	if ok, why := guardStop(f.port, autoStop, false); ok || !strings.Contains(why, "can't tell") {
		t.Fatalf("%v %q", ok, why)
	}
}

func TestPinBlocksAutoStop(t *testing.T) {
	g := guardEnv(t)
	f := newFakeLlama(t, "qwen3.6-27b", true)
	g.serve(f, 100, true)
	inspectServer(f.port)
	if _, err := toolModel(map[string]any{"action": "pin"}); err != nil {
		t.Fatal(err)
	}
	g.now = g.now.Add(time.Hour)
	if s := idleSweep(); len(s) != 0 {
		t.Fatalf("pinned server stopped: %v", s)
	}
	if ok, why := guardStop(f.port, manualStop, false); ok || !strings.Contains(why, "pinned") {
		t.Fatalf("%v %q", ok, why)
	}
	if !strings.Contains(guardStatusLine("model server", f.port), "pinned yes") {
		t.Fatal("status doesn't show the pin")
	}
	toolModel(map[string]any{"action": "unpin"})
	if s := idleSweep(); len(s) != 1 {
		t.Fatalf("unpinned idle server not stopped: %v", s)
	}
}

func switchSetup(t *testing.T, g *guardFake, port int) {
	s := filepath.Join(t.TempDir(), "start-qwen.bat")
	os.WriteFile(s, []byte(fmt.Sprintf("llama-server.exe -m q.gguf --alias qwen-new --port %d\r\n", port)), 0o644)
	cfg.Models = map[string]string{"qwen": s}
	srv := filepath.Join(t.TempDir(), "llama-server") // the .bat runs as a converted Linux script
	os.WriteFile(srv, nil, 0o755)
	cfg.LlamaServer = srv
}

func TestModelSwitchRefusesBusyOrExternal(t *testing.T) {
	t.Run("external", func(t *testing.T) {
		g := guardEnv(t)
		f := newFakeLlama(t, "remote-model", true)
		g.serve(f, 4242, false)
		switchSetup(t, g, f.port)
		for _, force := range []bool{false} {
			_, err := useModel("qwen", 0, force)
			if err == nil || !strings.Contains(err.Error(), "not switching") || !strings.Contains(err.Error(), "wasn't started by ContextSaver") {
				t.Fatalf("switch over an external server: %v", err)
			}
		}
		if len(g.killed) != 0 || g.started != 0 {
			t.Fatalf("killed %v, started %d", g.killed, g.started)
		}
	})
	t.Run("busy", func(t *testing.T) {
		g := guardEnv(t)
		f := newFakeLlama(t, "old-model", true)
		g.serve(f, 100, true)
		f.busy.Store(true)
		switchSetup(t, g, f.port)
		for _, force := range []bool{false, true} {
			if _, err := useModel("qwen", 0, force); err == nil || !strings.Contains(err.Error(), "busy") {
				t.Fatalf("force %v: switch over a busy server: %v", force, err)
			}
		}
		if len(g.killed) != 0 || g.started != 0 {
			t.Fatalf("killed %v, started %d", g.killed, g.started)
		}
	})
	t.Run("owned idle switches", func(t *testing.T) {
		g := guardEnv(t)
		f := newFakeLlama(t, "old-model", true)
		g.serve(f, 100, true)
		switchSetup(t, g, f.port)
		if _, err := useModel("qwen", 0, false); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(g.killed) != "[100]" || g.started != 1 {
			t.Fatalf("killed %v, started %d", g.killed, g.started)
		}
	})
}

func TestStatusShowsOwnerBusyIdle(t *testing.T) {
	g := guardEnv(t)
	f := newFakeLlama(t, "qwen3.6-27b", true)
	g.serve(f, 100, true)
	inspectServer(f.port)
	g.now = g.now.Add(5 * time.Minute)
	line := guardStatusLine("model server", f.port)
	for _, w := range []string{"running qwen3.6-27b", "owner contextsaver", "pinned no", "busy no", "idle stop in 10m0s"} {
		if !strings.Contains(line, w) {
			t.Fatalf("%q missing %q", line, w)
		}
	}
	g.pids[f.port] = 999 // someone else's server now
	unregister(f.port)
	if line := guardStatusLine("model server", f.port); !strings.Contains(line, "owner external") || !strings.Contains(line, "never (external)") {
		t.Fatal(line)
	}
}
