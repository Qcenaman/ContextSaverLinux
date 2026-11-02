package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LM Studio support. The front end (AnythingLLM or LM Studio) is the config choice
// "frontend"; empty means AnythingLLM when it is installed, else LM Studio. With LM
// Studio, models load with lms, the ai command uses LM Studio's loaded model, and
// Start/Stop Local AI start and stop LM Studio's server.

var lmsBase = "http://127.0.0.1:1234"

// Hooks so tests don't touch real models.
var (
	lmsCmd        = func(d time.Duration, args ...string) error { return runWithTimeout(d, lmsExe(), args...) }
	lmsStopOthers = stopOwnedOthers
	lmsGPUBusy    = gpuBusyExcept
	lmsVRAM       = vramWarning
)

// lmsNames maps ContextSaver's model names to LM Studio model keys.
var lmsNames = map[string]string{
	"qwen": "qwen3.6-27b", "qwen-think": "qwen3.6-27b",
	"gemma": "gemma-4-31b-it",
	"oss":   "gpt-oss-120b", "oss-fast": "gpt-oss-120b",
	"oss-free": "gpt-oss-120b-derestricted",
}

type lmsModel struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	State     string `json:"state"`
	LoadedCtx int    `json:"loaded_context_length"`
}

// useLMStudio: LM Studio is the chosen front end.
func useLMStudio() bool {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cfg.Frontend), " ", "")) {
	case "lmstudio", "lm-studio":
		return true
	case "anythingllm":
		return false
	}
	return !exists(anythingLLMExe()) && exists(lmsExe())
}

func lmsModels() ([]lmsModel, bool) {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get(lmsBase + "/api/v0/models")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var r struct {
		Data []lmsModel `json:"data"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil, false
	}
	return r.Data, true
}

// lmsLoaded is the chat model LM Studio has loaded ("" if none).
func lmsLoaded() string {
	ms, _ := lmsModels()
	for _, m := range ms {
		if m.State == "loaded" && m.Type != "embeddings" {
			return m.ID
		}
	}
	return ""
}

// lmsMode: model{} works through LM Studio (the chosen front end, or its server
// answers while the llama.cpp port doesn't).
func lmsMode() bool {
	if cfg.AIEndpoint != "" {
		return false
	}
	if useLMStudio() {
		return true
	}
	if _, ok := lmsModels(); !ok {
		return false
	}
	host, port := scriptAddr(firstScript())
	return len(servedModels(host, port)) == 0
}

func lmsExe() string {
	h, _ := os.UserHomeDir()
	if p := filepath.Join(h, ".lmstudio", "bin", "lms"); exists(p) {
		return p
	}
	return "lms"
}

// lmsKey resolves a ContextSaver name or an LM Studio key to an LM Studio key.
func lmsKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if k := lmsMapped(name); k != "" {
		return k
	}
	ms, _ := lmsModels()
	for _, m := range ms {
		if strings.EqualFold(m.ID, name) && m.Type != "embeddings" {
			return m.ID
		}
	}
	return ""
}

func lmsStatus() string {
	cur := lmsLoaded()
	var b strings.Builder
	b.WriteString("LM Studio models:\n")
	for _, n := range lmsNameList() {
		mark := ""
		if lmsMapped(n) == cur && cur != "" {
			mark = " [loaded]"
		}
		fmt.Fprintf(&b, "%s = %s%s%s\n", n, lmsMapped(n), mark, modelDesc(n))
	}
	b.WriteString(modelFileNotes())
	if cur == "" {
		b.WriteString("nothing loaded")
	}
	return strings.TrimSpace(b.String())
}

// lmsServerStart starts LM Studio's server; one ContextSaver started is its own to stop.
func lmsServerStart() error {
	_, wasUp := lmsModels()
	args := []string{"server", "start"}
	if cfg.LMSBind != "" {
		args = append(args, "--bind", cfg.LMSBind)
	}
	if err := lmsCmd(60*time.Second, args...); err != nil {
		return fmt.Errorf("couldn't start LM Studio's server: %v", err)
	}
	if !wasUp {
		editReg(func(r *guardReg) { r.LMSServer = true })
	}
	return nil
}

// lmsUse loads a model in LM Studio. The model LM Studio has loaded is unloaded first
// only when ContextSaver loaded it (or force); LM Studio can't report whether it is
// answering a request, so a model loaded elsewhere (RemoteAI, LM Studio itself) is
// never replaced on its own.
func lmsUse(name string, force bool) (string, error) {
	if err := modelFileCheck(name); err != nil {
		return "", err
	}
	if _, up := lmsModels(); !up {
		if err := lmsServerStart(); err != nil {
			return "", err
		}
	}
	key := lmsKey(name)
	if key == "" {
		return "", fmt.Errorf("unknown model %q; have: %s", name, strings.Join(lmsNameList(), ", "))
	}
	if s, ok := models()[strings.ToLower(name)]; ok {
		setAIScript(s) // Start Local AI loads the last-used model
	}
	cur := lmsLoaded()
	if cur == key {
		return name + " is already loaded in LM Studio", nil
	}
	if cur != "" && readReg().LMSModel != cur && !force {
		return "", fmt.Errorf("not switching to %s: LM Studio has %s loaded, which ContextSaver didn't load (RemoteAI or LM Studio itself may be using it). It was left loaded; force:true replaces it", name, cur)
	}
	_, port := scriptAddr(firstScript())
	if w := lmsGPUBusy(port); w != "" {
		return "", errors.New("not switching: " + w + ". Stop that first")
	}
	waitNote, err := waitAIJobsOrRefuse(60 * time.Second)
	if err != nil {
		return "", fmt.Errorf("not switching to %s: %v", name, err)
	}
	var stopped []string
	if cur != "" {
		if err := lmsCmd(30*time.Second, "unload", cur); err != nil {
			return "", fmt.Errorf("couldn't unload %s: %v", cur, err)
		}
		stopped = append(stopped, "LM Studio "+cur)
		editReg(func(r *guardReg) { r.LMSModel = "" })
	}
	others, left := lmsStopOthers(port)
	stopped = append(stopped, others...)
	waitNote += lmsVRAM(name, key)
	err = lmsCmd(time.Duration(modelLoadWait+30)*time.Second, "load", key, "-y")
	note := stoppedNote(stopped) + leftNote(left) + waitNote
	if lmsLoaded() == key {
		editReg(func(r *guardReg) { r.LMSModel = key })
		return name + " loaded in LM Studio" + note, nil
	}
	if err != nil {
		return "", fmt.Errorf("LM Studio couldn't load %s: %v", key, err)
	}
	editReg(func(r *guardReg) { r.LMSModel = key })
	return name + " is loading in LM Studio" + note + "; check with model{action:list}", nil
}

// startModelName: the last-used model (config ai_script), else qwen.
func startModelName() string {
	if n := modelForScript(cfg.AIScript); lmsMapped(n) != "" {
		return n
	}
	return "qwen"
}

// startLMStudio: Start Local AI with LM Studio = its server, then the configured model.
func startLMStudio() (string, error) {
	if err := lmsServerStart(); err != nil {
		return "", err
	}
	if cur := lmsLoaded(); cur != "" {
		return "LM Studio server on; using the loaded model " + cur, nil
	}
	out, err := lmsUse(startModelName(), false)
	if err != nil {
		return "", err
	}
	return "LM Studio server on; " + out, nil
}

// stopLMStudio: Stop Local AI with LM Studio = unload the model ContextSaver loaded,
// and stop LM Studio's server when ContextSaver started it and nothing else is loaded.
// Models and a server started elsewhere stay (force:true unloads and stops them too).
func stopLMStudio(force bool) string {
	ms, up := lmsModels()
	if !up {
		return "LM Studio server isn't running"
	}
	reg := readReg()
	var out, left []string
	for _, m := range ms {
		if m.State != "loaded" {
			continue
		}
		if m.ID != reg.LMSModel && !force {
			left = append(left, m.ID)
			continue
		}
		if err := lmsCmd(30*time.Second, "unload", m.ID); err != nil {
			out = append(out, "unload "+m.ID+" failed: "+err.Error())
			left = append(left, m.ID)
		} else {
			out = append(out, "unloaded "+m.ID)
		}
	}
	editReg(func(r *guardReg) { r.LMSModel = "" })
	switch {
	case len(left) > 0:
		out = append(out, "left loaded (not loaded by ContextSaver): "+strings.Join(left, ", ")+"; LM Studio's server stays on")
	case !reg.LMSServer && !force:
		out = append(out, "LM Studio's server stays on (ContextSaver didn't start it)")
	default:
		if err := lmsCmd(30*time.Second, "server", "stop"); err != nil {
			out = append(out, "server stop failed: "+err.Error())
		} else {
			out = append(out, "server stopped")
			editReg(func(r *guardReg) { r.LMSServer = false })
		}
	}
	return strings.Join(out, "; ")
}

// lmStudioMCPPath is LM Studio's mcp.json ("" when LM Studio isn't installed).
func lmStudioMCPPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	d := filepath.Join(h, ".lmstudio")
	if !exists(d) {
		return ""
	}
	return filepath.Join(d, "mcp.json")
}

// connectLMStudio adds contextsaver to LM Studio's mcp.json, keeping its other servers.
func connectLMStudio(target string) (string, error) {
	p := lmStudioMCPPath()
	if p == "" || configHasEntry(p, target) {
		return p, nil
	}
	return p, editClaudeConfig(p, func(s map[string]any) {
		s["contextsaver"] = map[string]any{"command": target, "args": []string{"mcp"}}
	})
}

// disconnectLMStudio removes contextsaver from LM Studio's mcp.json.
func disconnectLMStudio() (string, error) {
	p := lmStudioMCPPath()
	if p == "" || !exists(p) {
		return "", nil
	}
	return p, editClaudeConfig(p, func(s map[string]any) { delete(s, "contextsaver") })
}
