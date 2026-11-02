package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontendSync(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("SERVER_PORT=3001\r\nGENERIC_OPEN_AI_BASE_PATH='http://127.0.0.1:8080/v1'\r\nGENERIC_OPEN_AI_MODEL_PREF='gpt-oss-120b'\r\n"), 0o644)
	cd := filepath.Join(dir, "cline", "data")
	os.MkdirAll(cd, 0o755)
	os.WriteFile(filepath.Join(cd, "globalState.json"), []byte(`{"keep":1,"actModeOpenAiModelInfo":{"maxTokens":8192,"contextWindow":32768}}`), 0o644)
	ae, cdd, fr := anythingEnvPath, clineDataDir, frontRunning
	t.Cleanup(func() { anythingEnvPath, clineDataDir, frontRunning = ae, cdd, fr })
	anythingEnvPath = func() string { return env }
	clineDataDir = func() string { return cd }
	frontRunning = func(string) bool { return false }

	f := frontInfo{Base: "http://127.0.0.1:1234/v1", Key: "sk-lm-x", Model: "qwen3.6-27b", Ctx: 196608}
	if note := applyFrontends(f); !strings.Contains(note, "AnythingLLM, Cline") {
		t.Fatalf("note: %q", note)
	}
	b, _ := os.ReadFile(env)
	s := string(b)
	for _, w := range []string{"SERVER_PORT=3001\r\n", "GENERIC_OPEN_AI_BASE_PATH='http://127.0.0.1:1234/v1'\r\n", "GENERIC_OPEN_AI_MODEL_PREF='qwen3.6-27b'", "GENERIC_OPEN_AI_API_KEY='sk-lm-x'", "GENERIC_OPEN_AI_MODEL_TOKEN_LIMIT='196608'"} {
		if !strings.Contains(s, w) {
			t.Errorf(".env misses %q:\n%s", w, s)
		}
	}
	var gs map[string]any
	b, _ = os.ReadFile(filepath.Join(cd, "globalState.json"))
	json.Unmarshal(b, &gs)
	info := gs["actModeOpenAiModelInfo"].(map[string]any)
	if gs["openAiBaseUrl"] != f.Base || gs["planModeOpenAiModelId"] != f.Model || info["contextWindow"] != float64(196608) || info["maxTokens"] != float64(8192) || gs["keep"] != float64(1) {
		t.Fatalf("globalState: %v", gs)
	}
	b, _ = os.ReadFile(filepath.Join(cd, "secrets.json"))
	if !strings.Contains(string(b), "sk-lm-x") {
		t.Fatal("key not set")
	}
	// nothing to change the second time
	if note := applyFrontends(f); note != "" {
		t.Fatalf("second sync changed something: %q", note)
	}
	// a running app gets a restart note
	frontRunning = func(string) bool { return true }
	f.Model = "gemma-4-31b-it"
	if note := applyFrontends(f); !strings.Contains(note, "restart AnythingLLM, VS Code (Cline)") {
		t.Fatalf("note: %q", note)
	}
}
