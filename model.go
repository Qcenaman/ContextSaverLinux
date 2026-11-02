package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Model switching: every model has a llama.cpp start script on the same port, so
// AnythingLLM keeps talking to the same address whichever model is loaded.

var defaultModels = map[string]string{
	"qwen": `D:\llama.cpp\start-qwen3.6-27b-nothink.bat`,
	"oss":  `D:\llama.cpp\start-gpt-oss-120b.bat`,
	// oss-fast (EAGLE3 SPEC) measured slower than plain gpt-oss with experts on the CPU,
	// so it points to the plain script.
	"oss-fast":   `D:\llama.cpp\start-gpt-oss-120b.bat`,
	"gemma":      `D:\llama.cpp\start-gemma-4-31b.bat`,
	"qwen-think": `D:\llama.cpp\start-qwen3.6-27b.bat`,
	"oss-free":   `D:\llama.cpp\start-gpt-oss-120b-derestricted.bat`,
}

var aliasRe = regexp.MustCompile(`(?i)--alias[ =]+"?([^\s"^]+)`)

func scriptAlias(script string) string {
	b, err := os.ReadFile(script)
	if err != nil {
		return ""
	}
	if m := aliasRe.FindStringSubmatch(string(b)); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// servedModels returns the model ids answering on port, or nil when nothing runs.
func servedModels(host string, port int) []string {
	c := &aiClient{kind: "openai", base: fmt.Sprintf("http://%s:%d", host, port), hc: &http.Client{Timeout: 3 * time.Second}}
	ms, err := c.models()
	if err != nil {
		return nil
	}
	return ms
}

func serverLoading(host string, port int) bool {
	qc := &http.Client{Timeout: 2 * time.Second}
	resp, err := qc.Get(fmt.Sprintf("http://%s:%d/health", host, port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 503
}

// currentModel names the configured model that is loaded right now ("" if none).
func currentModel() (name, state string) {
	for _, n := range modelNames() {
		s := models()[n]
		host, port := scriptAddr(s)
		ms := servedModels(host, port)
		al := scriptAlias(s)
		for _, m := range ms {
			if al != "" && strings.EqualFold(m, al) {
				if samePath(s, cfg.AIScript) || name == "" {
					name = n
				}
			}
		}
	}
	if name != "" {
		return name, "loaded"
	}
	host, port := scriptAddr(firstScript())
	if ms := servedModels(host, port); len(ms) > 0 {
		return strings.Join(ms, ","), "loaded"
	}
	if serverLoading(host, port) {
		return modelForScript(cfg.AIScript), "loading"
	}
	return "", "none"
}

func modelForScript(s string) string {
	for n, p := range models() {
		if samePath(p, s) {
			return n
		}
	}
	return filepath.Base(s)
}

func toolModel(a map[string]any) (string, error) {
	act := strings.ToLower(str(a, "action"))
	name := strings.ToLower(str(a, "name", "model"))
	if act == "" {
		if name != "" {
			act = "use"
		} else {
			act = "list"
		}
	}
	switch act {
	case "list", "status":
		return modelStatus(), nil
	case "use", "load", "switch":
		out, err := useModel(name, modelLoadWait, a["force"] == true)
		if err == nil {
			out += syncFrontends()
		}
		return out, err
	case "stop", "unload":
		return stopModel(a["force"] == true), nil
	case "pin", "keep":
		setGuardSetting("keep_model_running", true)
		return "pinned: nothing stops the model server on its own now\n" + modelStatus(), nil
	case "unpin":
		setGuardSetting("keep_model_running", false)
		return "unpinned: a server ContextSaver started stops after its idle time\n" + modelStatus(), nil
	case "idle":
		m := intArg(a, "minutes")
		if m <= 0 {
			return "", errors.New("idle needs minutes (1 or more)")
		}
		setGuardSetting("model_idle_minutes", m)
		return fmt.Sprintf("idle stop after %d min\n", m) + modelStatus(), nil
	}
	return "", errors.New("action must be list, use, stop, pin, unpin or idle")
}

func useModel(name string, waitSecs int, force bool) (string, error) {
	if lmsMode() {
		return lmsUse(name, force)
	}
	s, err := resolveModel(name)
	if err != nil {
		return "", err
	}
	if !exists(s) {
		return "", errors.New("script not found: " + s)
	}
	host, port := scriptAddr(s)
	al := scriptAlias(s)
	served := servedModels(host, port)
	for _, m := range served {
		if al != "" && strings.EqualFold(m, al) {
			setAIScript(s)
			return name + " is already loaded", nil
		}
	}
	if w := modelGPUBusy(port); w != "" {
		return "", errors.New("not switching: " + w + ". Stop that first or switch from the ContextSaver shortcut")
	}
	// The server now on the port has to go first: only when ContextSaver owns it and it's idle.
	what := fmt.Sprintf("the server on port %d", port)
	if len(served) > 0 {
		what += " (" + strings.Join(uniq(served), ",") + ")"
	}
	refuse := func(why string) (string, error) {
		if d := guardProcDesc(port); d != "" {
			what += " [" + d + "]"
		}
		return "", fmt.Errorf("not switching to %s: %s %s. It was left running", name, what, why)
	}
	if st, _ := inspectServer(port); st.Up || st.Owner != ownerNone {
		if ok, why := stopVerdict(st, manualStop, force); !ok {
			return refuse(why)
		}
	}
	waitNote, err := waitAIJobsOrRefuse(60 * time.Second)
	if err != nil {
		return refuse("is in use: " + err.Error())
	}
	var stopped []string
	if st, e := inspectServer(port); st.Up || st.Owner != ownerNone {
		if ok, why := stopVerdict(st, manualStop, force); !ok { // re-checked after the wait
			return refuse(why)
		}
		killServer(port, e)
		stopped = append(stopped, strings.TrimPrefix(what, "the "))
		if guardPIDOnPort(port) > 0 || probeServer(port).Up {
			return "", fmt.Errorf("couldn't free port %d for %s", port, name)
		}
	}
	others, left := stopOwnedOthers(port)
	stopped = append(stopped, others...)
	waitNote += vramWarning(name, s)
	setAIScript(s)
	p, err := guardStartScript(s)
	if err != nil {
		return "", fmt.Errorf("couldn't start %s: %v", s, err)
	}
	claimServer(p, port, name, s, waitSecs)
	note := stoppedNote(stopped) + leftNote(left) + waitNote
	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if len(servedModels(host, port)) > 0 && !serverLoading(host, port) {
			return name + " loaded on port " + fmt.Sprint(port) + note, nil
		}
	}
	return name + " is loading on port " + fmt.Sprint(port) + note + "; check with model{action:list}", nil
}

// modelLoadWait is how long a tool call waits for a model to load (measured 10-30 s).
const modelLoadWait = 60

// bigModelVRAMGB is the free VRAM Qwen and Gemma need at full context (~31.7-32 of 32.6 GB);
// with less they spill into shared memory and run ~5x slower.
const bigModelVRAMGB = 31.0

// lmsBigModelVRAMGB: in LM Studio Qwen loads at 196K and Gemma at 131K context (~30.4 GB
// in use with the desktop's own share), leaving headroom for other apps' VRAM.
const lmsBigModelVRAMGB = 29.0

func bigModelNeedGB() float64 {
	if useLMStudio() {
		return lmsBigModelVRAMGB
	}
	return bigModelVRAMGB
}

func needsFullGPU(name, script string) bool {
	x := strings.ToLower(name + " " + filepath.Base(script))
	return strings.Contains(x, "qwen") || strings.Contains(x, "gemma")
}

// vramWarning waits a few seconds for VRAM freed by stopped models, then warns when a
// full-GPU model won't fit.
func vramWarning(name, script string) string {
	if !needsFullGPU(name, script) {
		return ""
	}
	var free float64
	for i := 0; i < 8; i++ {
		f, ok := freeVRAMGB()
		if !ok {
			return ""
		}
		if free = f; free >= bigModelNeedGB() {
			return ""
		}
		time.Sleep(time.Second)
	}
	w := fmt.Sprintf("; WARNING: only %.1f GB VRAM free, %s needs ~%.0f GB and will spill into shared memory (~5-10x slower)", free, name, bigModelNeedGB())
	if b := gpuBusy(); b != "" {
		return w + ": " + b
	}
	return w + "; close games or other GPU programs, then load it again"
}

func freeVRAMGB() (float64, bool) {
	out, err := runQuiet(15*time.Second, "nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0]), 64)
	if err != nil {
		return 0, false
	}
	return f / 1024, true
}

// stopModel is the user's stop (model{stop}, Stop model, Stop Local AI): it stops the
// model server only when ContextSaver started it, it isn't busy and isn't pinned
// (force:true also stops an external or pinned one, never a busy one).
func stopModel(force bool) string {
	_, port := scriptAddr(firstScript())
	waitNote, err := waitAIJobsOrRefuse(60 * time.Second)
	if err != nil {
		return "nothing stopped: " + err.Error()
	}
	var stopped, left []string
	served := uniq(servedModels(guardHost, port))
	what := fmt.Sprintf("server on port %d", port)
	if len(served) > 0 {
		what = strings.Join(served, ",") + " on port " + fmt.Sprint(port)
	}
	if ok, why := guardStop(port, manualStop, force); ok {
		stopped = append(stopped, what)
	} else if why != "not running" {
		left = append(left, what+" ("+why+")")
	}
	o, l := stopOwnedOthers(port)
	stopped, left = append(stopped, o...), append(left, l...)
	if len(stopped) == 0 && len(left) == 0 {
		return "nothing loaded"
	}
	out := "nothing stopped"
	if len(stopped) > 0 {
		out = "stopped " + strings.Join(stopped, ", ") + "; VRAM freed"
	}
	return out + leftNote(left) + waitNote
}

func setAIScript(s string) {
	if samePath(cfg.AIScript, s) && cfg.AIEndpoint == "" {
		return
	}
	cfg.AIScript = s
	cfg.AIEndpoint = ""
	saveConfig()
}

func saveConfig() {
	p := filepath.Join(appHome, "config.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	m["ai_script"] = cfg.AIScript
	delete(m, "ai_endpoint")
	if len(cfg.Models) > 0 {
		m["models"] = cfg.Models
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, b, 0o644)
}

// gpuBusyExcept reports other GPU work (ComfyUI, training) but not the model server on port.
func gpuBusyExcept(port int) string {
	ps, err := procs()
	if err != nil {
		return ""
	}
	for _, p := range ps {
		n, c := strings.ToLower(p.name), strings.ToLower(p.cmd)
		if strings.Contains(c, "contextsaver") || strings.Contains(c, "llama-server") {
			continue
		}
		for _, a := range gpuApps {
			if a.match(n, c) {
				return a.what + " is running"
			}
		}
	}
	return ""
}

// aiServerUp reports a model already loaded, so using it needs no extra VRAM.
func aiServerUp() bool {
	if cfg.AIEndpoint != "" || cfg.AIScript == "" {
		return false
	}
	host, port := scriptAddr(cfg.AIScript)
	return len(servedModels(host, port)) > 0
}

func portOpen(port int) bool { return pidOnPort(port) > 0 }

func stoppedNote(stopped []string) string {
	if len(stopped) == 0 {
		return ""
	}
	return " (stopped " + strings.Join(stopped, ", ") + ")"
}

// localModelProcs are programs that serve a local model and hold VRAM while running.
var localModelProcs = []string{"llama-server", "llama-cli", "llama-run", "koboldcpp", "llamafile", "text-generation-launcher", "lemonade-server"}

func runWithTimeout(d time.Duration, name string, args ...string) error {
	_, err := runQuiet(d, name, args...)
	return err
}

// AI jobs of TokenSaver (Claude's tool) and ContextSaver read through the loaded model.
// Switching mid-job would make them fail, so a switch waits for them first.
func aiJobDirs() []string {
	dirs := []string{jobsDir()}
	if isWin {
		dirs = append(dirs, `D:\Claude Apps\TokenSaver\data\jobs`)
	}
	return dirs
}

// runningAIJobs names AI jobs that are running now (kind ai, or web for web ask).
func runningAIJobs() []string { return runningJobsOfKind(aiJobDirs(), "ai", "web") }

// otherJobDirs: TokenSaver's jobs folder.
func otherJobDirs() []string {
	if isWin {
		return []string{`D:\Claude Apps\TokenSaver\data\jobs`}
	}
	return nil
}

// runningJobsOfKind names jobs of these kinds that are running now in dirs.
func runningJobsOfKind(dirs []string, kinds ...string) []string {
	var out []string
	for _, d := range dirs {
		es, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		// newest jobs last; only recent ones can still be running
		for i := len(es) - 1; i >= 0 && i >= len(es)-40; i-- {
			b, err := os.ReadFile(filepath.Join(d, es[i].Name(), "state.json"))
			if err != nil {
				continue
			}
			var st struct {
				Name    string    `json:"name"`
				Kind    string    `json:"kind"`
				Status  string    `json:"status"`
				Updated time.Time `json:"updated"`
			}
			if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &st) != nil {
				continue
			}
			if st.Status == "running" && containsFold(kinds, st.Kind) && time.Since(st.Updated) < 2*time.Minute {
				out = append(out, st.Name)
			}
		}
	}
	return out
}

// waitForAIJobs waits up to max for running AI jobs to finish. It returns a note
// for the result when it waited or gave up waiting.
func waitForAIJobs(max time.Duration) string {
	jobs := runningAIJobs()
	if len(jobs) == 0 {
		return ""
	}
	start := time.Now()
	for time.Since(start) < max {
		time.Sleep(2 * time.Second)
		if jobs = runningAIJobs(); len(jobs) == 0 {
			return fmt.Sprintf("; waited %ds for an AI job to finish first", int(time.Since(start).Seconds()))
		}
	}
	return "; switched anyway after waiting " + max.String() + " for " + strings.Join(jobs, ", ") + " (it may fail)"
}

// localVision reports whether the loaded model accepts pictures (llama.cpp /props).
func localVision() bool {
	host, port := scriptAddr(firstScript())
	c := &aiClient{kind: "openai", base: fmt.Sprintf("http://%s:%d", host, port), hc: &http.Client{Timeout: 3 * time.Second}}
	if len(servedModels(host, port)) == 0 {
		return false
	}
	c.loadCtx()
	return c.vision
}
