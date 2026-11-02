package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLMS stands in for LM Studio: its models API, and the lms command (recorded).
func fakeLMS(t *testing.T, loaded string) *[]string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := []map[string]string{{"id": "qwen3.6-27b", "type": "llm", "state": "not-loaded"}}
		if loaded != "" {
			data = append(data, map[string]string{"id": loaded, "type": "llm", "state": "loaded"})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	b, c, s, g, v := lmsBase, lmsCmd, lmsStopOthers, lmsGPUBusy, lmsVRAM
	fe, script, home := cfg.Frontend, cfg.AIScript, appHome
	t.Cleanup(func() {
		lmsBase, lmsCmd, lmsStopOthers, lmsGPUBusy, lmsVRAM = b, c, s, g, v
		cfg.Frontend, cfg.AIScript, appHome = fe, script, home
	})
	appHome = t.TempDir()
	cfg.Frontend = "lmstudio"
	lmsBase = srv.URL
	calls := &[]string{}
	lmsCmd = func(_ time.Duration, args ...string) error {
		*calls = append(*calls, strings.Join(args, " "))
		return nil
	}
	lmsStopOthers = func(int) ([]string, []string) { return nil, nil }
	lmsGPUBusy = func(int) string { return "" }
	lmsVRAM = func(string, string) string { return "" }
	return calls
}

func TestStartLMStudioLoadsConfigModel(t *testing.T) {
	calls := fakeLMS(t, "")
	cfg.AIScript = models()["oss"]
	if _, err := startLMStudio(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, " | "); got != "server start | load gpt-oss-120b -y" {
		t.Fatalf("lms calls: %s", got)
	}
}

func TestStartLMStudioBind(t *testing.T) {
	calls := fakeLMS(t, "qwen3.6-27b")
	old := cfg.LMSBind
	t.Cleanup(func() { cfg.LMSBind = old })
	cfg.LMSBind = "0.0.0.0"
	startLMStudio()
	if got := strings.Join(*calls, " | "); got != "server start --bind 0.0.0.0" {
		t.Fatalf("lms calls: %s", got)
	}
}

func TestStartLMStudioKeepsLoadedModel(t *testing.T) {
	calls := fakeLMS(t, "gemma-4-31b-it")
	out, err := startLMStudio()
	if err != nil || !strings.Contains(out, "gemma-4-31b-it") {
		t.Fatalf("%q %v", out, err)
	}
	if got := strings.Join(*calls, " | "); got != "server start" {
		t.Fatalf("lms calls: %s", got)
	}
}

// Stop Local AI leaves a model ContextSaver didn't load (RemoteAI's) and the server.
func TestStopLMStudioLeavesExternal(t *testing.T) {
	calls := fakeLMS(t, "qwen3.6-27b")
	out := stopLMStudio(false)
	if len(*calls) != 0 || !strings.Contains(out, "left loaded") {
		t.Fatalf("lms calls: %v; %s", *calls, out)
	}
}

func TestStopLMStudioOwned(t *testing.T) {
	calls := fakeLMS(t, "qwen3.6-27b")
	editReg(func(r *guardReg) { r.LMSModel, r.LMSServer = "qwen3.6-27b", true })
	stopLMStudio(false)
	if got := strings.Join(*calls, " | "); got != "unload qwen3.6-27b | server stop" {
		t.Fatalf("lms calls: %s", got)
	}
}

// A model loaded elsewhere in LM Studio isn't replaced by a switch.
func TestLMStudioSwitchRefusesExternal(t *testing.T) {
	calls := fakeLMS(t, "gemma-4-31b-it")
	if _, err := lmsUse("qwen", false); err == nil || !strings.Contains(err.Error(), "not switching") {
		t.Fatalf("switched over an external model: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("lms calls: %v", *calls)
	}
}

func TestFrontendChoice(t *testing.T) {
	old := cfg.Frontend
	t.Cleanup(func() { cfg.Frontend = old })
	cfg.Frontend = "LM Studio"
	if !useLMStudio() || !lmsMode() {
		t.Fatal("lmstudio front end not used")
	}
	cfg.Frontend = "anythingllm"
	if useLMStudio() {
		t.Fatal("anythingllm front end ignored")
	}
}

func TestLMStudioMCPConnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".lmstudio"), 0o755)
	p := filepath.Join(home, ".lmstudio", "mcp.json")
	os.WriteFile(p, []byte(`{"mcpServers":{"other":{"command":"x.exe"}}}`), 0o644)
	target := filepath.Join(home, "app", "contextsaver.exe")
	if _, err := connectLMStudio(target); err != nil {
		t.Fatal(err)
	}
	if !configHasEntry(p, target) || !hasServer(t, p, "other") {
		t.Fatal("contextsaver not added or other server lost")
	}
	if _, err := disconnectLMStudio(); err != nil {
		t.Fatal(err)
	}
	if hasServer(t, p, "contextsaver") || !hasServer(t, p, "other") {
		t.Fatal("uninstall didn't remove only contextsaver")
	}
}

func hasServer(t *testing.T, p, name string) bool {
	b, _ := os.ReadFile(p)
	var m struct {
		S map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	_, ok := m.S[name]
	return ok
}

// Requests through LM Studio's API get no approval prompt: risky calls must
// still return a confirm token instead of running.
func TestRiskyCallNeedsToken(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"c": "run", "a": map[string]any{"command": "del C:\\x.txt"}})
	out, _, err := callCompact(b)
	if err != nil || !strings.HasPrefix(out, "CONFIRM ") {
		t.Fatalf("risky call ran without a token: %q %v", out, err)
	}
}
