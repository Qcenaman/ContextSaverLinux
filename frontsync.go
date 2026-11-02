package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Front-end sync: after a model loads or the front end changes, AnythingLLM and Cline are
// pointed at the active server, key, model and context so they never talk to a stale one.

type frontInfo struct {
	Base, Key, Model string
	Ctx              int
}

// Paths (vars so tests can point them at temp files).
var (
	anythingEnvPath = func() string {
		return filepath.Join(anythingStorage(), ".env")
	}
	clineDataDir = func() string {
		d := cfg.ClineDir
		if d == "" {
			d = os.Getenv("CLINE_DIR")
		}
		if d == "" {
			if p := `D:\Claude Apps\VSCode\cline-data`; isWin && exists(p) {
				d = p
			}
		}
		if d == "" {
			return ""
		}
		return filepath.Join(d, "data")
	}
	frontRunning = processRunning
)

// activeFront reads what is loaded now; ok=false when no model is loaded.
func activeFront() (frontInfo, bool) {
	if lmsMode() {
		ms, _ := lmsModels()
		for _, m := range ms {
			if m.State == "loaded" && m.Type != "embeddings" {
				return frontInfo{Base: lmsBase + "/v1", Key: cfg.LMSToken, Model: m.ID, Ctx: m.LoadedCtx}, true
			}
		}
		return frontInfo{}, false
	}
	host, port := scriptAddr(firstScript())
	ms := servedModels(host, port)
	if len(ms) == 0 {
		return frontInfo{}, false
	}
	f := frontInfo{Base: fmt.Sprintf("http://%s:%d/v1", host, port), Key: "none", Model: ms[0]}
	if r, err := (&http.Client{Timeout: 3 * time.Second}).Get(fmt.Sprintf("http://%s:%d/props", host, port)); err == nil {
		var p struct {
			Def struct {
				NCtx int `json:"n_ctx"`
			} `json:"default_generation_settings"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		r.Body.Close()
		f.Ctx = p.Def.NCtx
	}
	return f, true
}

// syncFrontends updates both apps and returns a short note ("" when nothing changed).
func syncFrontends() string {
	f, ok := activeFront()
	if !ok {
		return ""
	}
	return applyFrontends(f)
}

func applyFrontends(f frontInfo) string {
	if f.Key == "" {
		f.Key = "none"
	}
	var changed, restart []string
	if ok, err := syncAnythingEnv(anythingEnvPath(), f); err == nil && ok {
		changed = append(changed, "AnythingLLM")
		if frontRunning(anythingProc) {
			restart = append(restart, "AnythingLLM")
		}
	}
	if d := clineDataDir(); d != "" {
		if ok, err := syncCline(d, f); err == nil && ok {
			changed = append(changed, "Cline")
			if frontRunning("code") {
				restart = append(restart, "VS Code (Cline)")
			}
		}
	}
	if len(changed) == 0 {
		return ""
	}
	note := "; " + strings.Join(changed, ", ") + " set to " + f.Model
	if len(restart) > 0 {
		note += " (restart " + strings.Join(restart, ", ") + " to apply)"
	}
	return note
}

// syncAnythingEnv sets the generic-openai lines in AnythingLLM's .env.
func syncAnythingEnv(path string, f frontInfo) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	want := []struct{ k, v string }{
		{"LLM_PROVIDER", "generic-openai"},
		{"GENERIC_OPEN_AI_BASE_PATH", f.Base},
		{"GENERIC_OPEN_AI_API_KEY", f.Key},
		{"GENERIC_OPEN_AI_MODEL_PREF", f.Model},
	}
	if f.Ctx > 0 {
		want = append(want, struct{ k, v string }{"GENERIC_OPEN_AI_MODEL_TOKEN_LIMIT", fmt.Sprint(f.Ctx)})
	}
	s := string(b)
	nl := "\n"
	if strings.Contains(s, "\r\n") {
		nl = "\r\n"
	}
	out := s
	for _, w := range want {
		line := w.k + "='" + w.v + "'"
		re := regexp.MustCompile(`(?m)^` + w.k + `=.*?(\r?)$`)
		if re.MatchString(out) {
			out = re.ReplaceAllLiteralString(out, line)
			if nl == "\r\n" {
				out = strings.ReplaceAll(out, line+"\n", line+"\r\n")
			}
		} else {
			if out != "" && !strings.HasSuffix(out, "\n") {
				out += nl
			}
			out += line + nl
		}
	}
	if out == s {
		return false, nil
	}
	return true, writeFileAtomic(path, []byte(out))
}

// syncCline sets Cline's OpenAI-compatible provider settings.
func syncCline(dataDir string, f frontInfo) (bool, error) {
	gp := filepath.Join(dataDir, "globalState.json")
	gs := map[string]any{}
	b, err := os.ReadFile(gp)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(b), "\uFEFF")), &gs); err != nil {
		return false, err
	}
	before, _ := json.Marshal(gs)
	gs["actModeApiProvider"], gs["planModeApiProvider"] = "openai", "openai"
	gs["openAiBaseUrl"] = f.Base
	gs["actModeOpenAiModelId"], gs["planModeOpenAiModelId"] = f.Model, f.Model
	for _, k := range []string{"actModeOpenAiModelInfo", "planModeOpenAiModelInfo"} {
		info, _ := gs[k].(map[string]any)
		if info == nil {
			info = map[string]any{"maxTokens": 8192, "supportsImages": false, "supportsPromptCache": false, "inputPrice": 0, "outputPrice": 0}
		}
		if f.Ctx > 0 {
			info["contextWindow"] = f.Ctx
		}
		gs[k] = info
	}
	after, _ := json.Marshal(gs)
	changed := string(before) != string(after)
	if changed {
		out, _ := json.MarshalIndent(gs, "", "  ")
		if err := writeFileAtomic(gp, out); err != nil {
			return false, err
		}
	}
	sp := filepath.Join(dataDir, "secrets.json")
	sec := map[string]any{}
	if b, err := os.ReadFile(sp); err == nil {
		json.Unmarshal([]byte(strings.TrimPrefix(string(b), "\uFEFF")), &sec)
	}
	if sec["openAiApiKey"] != f.Key {
		sec["openAiApiKey"] = f.Key
		out, _ := json.MarshalIndent(sec, "", "  ")
		if err := writeFileAtomic(sp, out); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
