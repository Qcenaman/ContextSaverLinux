package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model servers are shared: llama.cpp on 8080 also serves RemoteAI and other tools,
// the embedding model on 8091 also serves TokenSaver, LM Studio serves RemoteAI over
// Tailscale. ContextSaver therefore stops a server only when
//   - it launched that server itself ("owned", recorded in data\servers.json),
//   - the server isn't processing anything (llama.cpp /slots, else /metrics),
//   - it isn't pinned (config keep_model_running), and
//   - on its own (idle stop, app exit) only after model_idle_minutes (default 15)
//     without requests from any client.
// A model switch or stop that would need to stop an external or busy server is
// refused with the reason instead.

const (
	ownerNone          = "none"
	ownerCS            = "contextsaver"
	ownerExt           = "external"
	defaultIdleMinutes = 15
	embedIdleMinutes   = 3
)

// Hooks so tests don't touch real processes.
var (
	guardHost        = "127.0.0.1"
	guardPIDOnPort   = func(port int) int { return pidOnPort(port) }
	guardPIDAlive    = func(pid int) bool { return pid > 0 && pidAlive(pid) }
	guardKill        = func(pid int) { killTree(pid) }
	guardNow         = time.Now
	guardTick        = 30 * time.Second
	guardHC          = &http.Client{Timeout: 3 * time.Second}
	guardStartScript = startScript
	guardForeign     = foreignModelServers
	modelGPUBusy     = gpuBusyExcept
)

// ---------- registry of servers ContextSaver launched ----------

type ownedServer struct {
	Port        int       `json:"port"`
	PID         int       `json:"pid"`        // the server listening on Port
	ScriptPID   int       `json:"script_pid"` // its start script (cmd /c …bat)
	Model       string    `json:"model,omitempty"`
	Script      string    `json:"script,omitempty"`
	Started     time.Time `json:"started"`
	LastActive  time.Time `json:"last_active"`
	Sig         string    `json:"sig,omitempty"` // last activity signature from /slots or /metrics
	IdleMinutes int       `json:"idle_minutes,omitempty"`
}

type guardReg struct {
	Servers   map[string]*ownedServer `json:"servers"`
	LMSModel  string                  `json:"lms_model,omitempty"`  // LM Studio model ContextSaver loaded
	LMSServer bool                    `json:"lms_server,omitempty"` // LM Studio's server was started by ContextSaver
}

var regMu sync.Mutex

func regPath() string { return filepath.Join(appHome, "servers.json") }

func loadRegLocked() *guardReg {
	r := &guardReg{}
	if b, err := os.ReadFile(regPath()); err == nil {
		json.Unmarshal(b, r)
	}
	if r.Servers == nil {
		r.Servers = map[string]*ownedServer{}
	}
	return r
}

func saveRegLocked(r *guardReg) { writeJSONAtomic(regPath(), r) }

// editReg runs f on the registry and saves it.
func editReg(f func(r *guardReg)) {
	regMu.Lock()
	defer regMu.Unlock()
	r := loadRegLocked()
	f(r)
	saveRegLocked(r)
}

func readReg() *guardReg {
	regMu.Lock()
	defer regMu.Unlock()
	return loadRegLocked()
}

func registerOwned(e ownedServer) {
	now := guardNow()
	e.Started, e.LastActive = now, now
	editReg(func(r *guardReg) { r.Servers[strconv.Itoa(e.Port)] = &e })
}

func unregister(port int) {
	editReg(func(r *guardReg) { delete(r.Servers, strconv.Itoa(port)) })
}

// touchOwned records activity now on an owned server (ContextSaver just used it).
func touchOwned(port int) {
	editReg(func(r *guardReg) {
		if e := r.Servers[strconv.Itoa(port)]; e != nil {
			e.LastActive = guardNow()
		}
	})
}

// ---------- settings (read fresh: several ContextSaver processes share them) ----------

func guardSettings() (pinned bool, idle time.Duration) {
	var c struct {
		Keep *bool `json:"keep_model_running"`
		Idle int   `json:"model_idle_minutes"`
	}
	if b, err := os.ReadFile(filepath.Join(appHome, "config.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Keep != nil {
		pinned = *c.Keep
	} else {
		pinned = cfg.KeepModelRunning
	}
	m := c.Idle
	if m <= 0 {
		m = cfg.ModelIdleMinutes
	}
	if m <= 0 {
		m = defaultIdleMinutes
	}
	return pinned, time.Duration(m) * time.Minute
}

func setGuardSetting(key string, v any) {
	p := filepath.Join(appHome, "config.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	m[key] = v
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, b, 0o644)
	switch key {
	case "keep_model_running":
		cfg.KeepModelRunning, _ = v.(bool)
	case "model_idle_minutes":
		cfg.ModelIdleMinutes, _ = v.(int)
	}
}

// ---------- probing ----------

type srvProbe struct {
	Up, Loading     bool
	Busy, BusyKnown bool
	Processing      int
	Sig             string
}

// probeServer asks /health, then /slots (or /metrics) whether anything is processing.
func probeServer(port int) srvProbe {
	var p srvProbe
	base := fmt.Sprintf("http://%s:%d", guardHost, port)
	resp, err := guardHC.Get(base + "/health")
	if err != nil {
		return p
	}
	resp.Body.Close()
	p.Up = true
	if resp.StatusCode == 503 { // loading a model: someone is starting it, hands off
		p.Loading, p.Busy, p.BusyKnown = true, true, true
		return p
	}
	if r, err := guardHC.Get(base + "/slots"); err == nil {
		var slots []map[string]any
		if r.StatusCode == 200 && json.NewDecoder(r.Body).Decode(&slots) == nil {
			p.BusyKnown = true
			var sig []string
			for _, s := range slots {
				proc := s["is_processing"] == true || fmt.Sprint(s["state"]) == "1"
				if proc {
					p.Processing++
				}
				dec := ""
				if nt, ok := s["next_token"].(map[string]any); ok {
					dec = fmt.Sprint(nt["n_decoded"])
				}
				sig = append(sig, fmt.Sprintf("%v:%v:%v:%s", s["id"], s["id_task"], proc, dec))
			}
			p.Sig = "slots " + strings.Join(sig, ",")
		}
		r.Body.Close()
	}
	if !p.BusyKnown {
		if r, err := guardHC.Get(base + "/metrics"); err == nil {
			if r.StatusCode == 200 {
				var sig []string
				sc := bufio.NewScanner(r.Body)
				for sc.Scan() {
					f := strings.Fields(sc.Text())
					if len(f) != 2 || strings.HasPrefix(f[0], "#") {
						continue
					}
					switch f[0] {
					case "llamacpp:requests_processing":
						p.BusyKnown = true
						v, _ := strconv.ParseFloat(f[1], 64)
						p.Processing = int(v)
					case "llamacpp:prompt_tokens_total", "llamacpp:tokens_predicted_total", "llamacpp:n_decode_total":
						sig = append(sig, f[0]+"="+f[1])
					}
				}
				p.Sig = "metrics " + strings.Join(sig, ",")
			}
			r.Body.Close()
		}
	}
	p.Busy = p.Processing > 0
	return p
}

// ---------- state ----------

type srvState struct {
	Port            int
	Up, Loading     bool
	Owner           string
	Pinned          bool
	Busy, BusyKnown bool
	Processing      int
	IdleFor         time.Duration
	IdleLimit       time.Duration
}

// inspectServer probes port, works out who owns it and records activity on owned
// servers (any change in /slots or /metrics counts, from whichever client).
func inspectServer(port int) (srvState, *ownedServer) {
	p := probeServer(port)
	pinned, idle := guardSettings()
	st := srvState{Port: port, Up: p.Up, Loading: p.Loading, Pinned: pinned, Busy: p.Busy,
		BusyKnown: p.BusyKnown, Processing: p.Processing, IdleLimit: idle, Owner: ownerNone}
	pid := guardPIDOnPort(port)
	now := guardNow()
	var own *ownedServer
	editReg(func(r *guardReg) {
		k := strconv.Itoa(port)
		e := r.Servers[k]
		if e == nil {
			if p.Up || pid > 0 {
				st.Owner = ownerExt
			}
			return
		}
		alive := guardPIDAlive(e.PID) || guardPIDAlive(e.ScriptPID)
		switch {
		case pid > 0 && e.PID == pid:
		case pid > 0 && e.PID == 0 && guardPIDAlive(e.ScriptPID): // started by us, port seen only now
			e.PID = pid
		case pid <= 0 && alive: // no port lookup on this system, or still starting
		default: // gone, or another server took the port
			delete(r.Servers, k)
			if p.Up || pid > 0 {
				st.Owner = ownerExt
			}
			return
		}
		st.Owner = ownerCS
		if p.Busy || (p.Sig != "" && p.Sig != e.Sig) || e.LastActive.IsZero() {
			e.LastActive = now
		}
		if p.Sig != "" {
			e.Sig = p.Sig
		}
		if e.IdleMinutes > 0 {
			st.IdleLimit = time.Duration(e.IdleMinutes) * time.Minute
		}
		st.IdleFor = now.Sub(e.LastActive)
		c := *e
		own = &c
	})
	return st, own
}

type stopMode int

const (
	autoStop   stopMode = iota // idle stop, app exit: ContextSaver decides on its own
	manualStop                 // model stop / switch / Stop Local AI: the user asked
)

// stopVerdict says whether a server may be stopped, and why not.
func stopVerdict(st srvState, mode stopMode, force bool) (bool, string) {
	if !st.Up && st.Owner == ownerNone {
		return true, ""
	}
	if st.Loading {
		return false, "it is still loading a model"
	}
	if st.Busy {
		return false, fmt.Sprintf("it is busy (%d request(s) processing)", max(st.Processing, 1))
	}
	if mode == autoStop {
		switch {
		case st.Owner != ownerCS:
			return false, "it wasn't started by ContextSaver"
		case st.Pinned:
			return false, "it is pinned to keep running"
		case !st.BusyKnown:
			return false, "its /slots and /metrics don't answer, so ContextSaver can't tell whether it's in use"
		case st.IdleFor < st.IdleLimit:
			return false, fmt.Sprintf("it was used %s ago (idle stop after %s)", st.IdleFor.Round(time.Second), st.IdleLimit)
		}
		return true, ""
	}
	if force {
		return true, ""
	}
	switch {
	case st.Owner != ownerCS:
		return false, "it wasn't started by ContextSaver (RemoteAI or other tools may be using it); force:true stops it anyway when it's idle"
	case st.Pinned:
		return false, "it is pinned to keep running (model{action:unpin}, or force:true)"
	case !st.BusyKnown:
		return false, "ContextSaver can't tell whether it's in use (no /slots or /metrics); force:true stops it anyway"
	}
	return true, ""
}

// killServer stops the server on port: an owned one by its recorded PIDs only; an
// external one (force) by the PID listening on the port.
func killServer(port int, e *ownedServer) {
	if e != nil {
		if e.PID > 0 && (guardPIDOnPort(port) == e.PID || guardPIDAlive(e.PID)) {
			guardKill(e.PID)
		}
		if e.ScriptPID > 0 && guardPIDAlive(e.ScriptPID) {
			guardKill(e.ScriptPID)
		}
	} else if pid := guardPIDOnPort(port); pid > 0 {
		guardKill(pid)
	}
	unregister(port)
	for i := 0; i < 8 && guardPIDOnPort(port) > 0; i++ {
		time.Sleep(time.Second)
	}
}

// guardStop applies the rules to the server on port and stops it when allowed.
func guardStop(port int, mode stopMode, force bool) (stopped bool, why string) {
	st, e := inspectServer(port)
	if !st.Up && st.Owner == ownerNone {
		return false, "not running"
	}
	ok, why := stopVerdict(st, mode, force)
	if !ok {
		return false, why
	}
	killServer(port, e)
	return true, ""
}

// ---------- idle stop ----------

var watcherOnce sync.Once

// startIdleWatcher stops owned, idle, unpinned servers in the background (MCP process).
func startIdleWatcher(ctx context.Context) {
	watcherOnce.Do(func() {
		go func() {
			t := time.NewTicker(guardTick)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					idleSweep()
				}
			}
		}()
	})
}

// idleSweep checks every server ContextSaver launched and stops the idle ones.
// The embedding model this process runs itself is left to its own idle timer.
func idleSweep() []string {
	var ports []int
	for _, e := range readReg().Servers {
		ports = append(ports, e.Port)
	}
	sort.Ints(ports)
	embedSrv.mu.Lock()
	ownEmbed := embedSrv.proc != nil
	embedSrv.mu.Unlock()
	var stopped []string
	for _, port := range ports {
		if port == ragPort && ownEmbed {
			continue
		}
		if ok, _ := guardStop(port, autoStop, false); ok {
			stopped = append(stopped, fmt.Sprintf("port %d", port))
		}
	}
	return stopped
}

// ---------- other model servers ----------

// stopOwnedOthers stops idle model servers ContextSaver launched on other ports (not
// the embedding model) and lists everything it leaves running.
func stopOwnedOthers(keepPort int) (stopped, left []string) {
	var ports []int
	for _, e := range readReg().Servers {
		if e.Port != keepPort && e.Port != ragPort {
			ports = append(ports, e.Port)
		}
	}
	sort.Ints(ports)
	for _, port := range ports {
		if ok, why := guardStop(port, manualStop, false); ok {
			stopped = append(stopped, fmt.Sprintf("server on port %d", port))
		} else if why != "not running" {
			left = append(left, fmt.Sprintf("server on port %d (%s)", port, why))
		}
	}
	left = append(left, guardForeign(keepPort)...)
	return stopped, left
}

// foreignModelServers names model servers ContextSaver didn't start (never stopped).
func foreignModelServers(keepPort int) []string {
	skip := map[int]bool{guardPIDOnPort(keepPort): true, guardPIDOnPort(ragPort): true}
	for _, e := range readReg().Servers {
		skip[e.PID], skip[e.ScriptPID] = true, true
	}
	var out []string
	if ps, err := procs(); err == nil {
		self := os.Getpid()
		for _, p := range ps {
			if p.pid == self || skip[p.pid] {
				continue
			}
			n := strings.ToLower(strings.TrimSuffix(strings.ToLower(p.name), ".exe"))
			for _, w := range localModelProcs {
				if strings.HasPrefix(n, w) {
					out = append(out, fmt.Sprintf("%s pid %d", p.name, p.pid))
					break
				}
			}
		}
	}
	if ms := ollamaLoaded(); len(ms) > 0 {
		out = append(out, "Ollama "+strings.Join(ms, ","))
	}
	if !useLMStudio() {
		if id := lmsLoaded(); id != "" {
			out = append(out, "LM Studio "+id)
		}
	}
	return out
}

func ollamaLoaded() []string {
	resp, err := guardHC.Get("http://127.0.0.1:11434/api/ps")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil
	}
	var out []string
	for _, m := range r.Models {
		out = append(out, m.Name)
	}
	return out
}

func leftNote(left []string) string {
	if len(left) == 0 {
		return ""
	}
	return "; left running: " + strings.Join(left, "; ")
}

// waitAIJobsOrRefuse waits for ContextSaver/TokenSaver AI jobs; still running after max = error.
func waitAIJobsOrRefuse(max time.Duration) (string, error) {
	jobs := runningAIJobs()
	if len(jobs) == 0 {
		return "", nil
	}
	start := time.Now()
	for time.Since(start) < max {
		time.Sleep(2 * time.Second)
		if jobs = runningAIJobs(); len(jobs) == 0 {
			return fmt.Sprintf("; waited %ds for an AI job to finish first", int(time.Since(start).Seconds())), nil
		}
	}
	return "", fmt.Errorf("an AI job is still using the model after %s (%s); try again when it's done", max, strings.Join(jobs, ", "))
}

// claimServer records the server a start script brought up on port as owned.
// The port was free before the start, so the first listener is the script's.
func claimServer(p *aiProc, port int, model, script string, waitSecs int) {
	spid := 0
	if p != nil && p.cmd != nil && p.cmd.Process != nil {
		spid = p.cmd.Process.Pid
	}
	pid := 0
	for i := 0; i < min(waitSecs, 30)*2 && pid <= 0; i++ {
		if pid = guardPIDOnPort(port); pid <= 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	registerOwned(ownedServer{Port: port, PID: max(pid, 0), ScriptPID: spid, Model: model, Script: script})
}

// ---------- status ----------

func guardStatusLine(label string, port int) string {
	st, _ := inspectServer(port)
	if !st.Up && st.Owner == ownerNone {
		pin := "no"
		if st.Pinned {
			pin = "yes"
		}
		return fmt.Sprintf("%s :%d: not running | pinned %s | idle stop after %s", label, port, pin, st.IdleLimit)
	}
	run := "running"
	if st.Loading {
		run = "loading"
	}
	if ms := servedModels(guardHost, port); len(ms) > 0 {
		run += " " + strings.Join(uniq(ms), ",")
	}
	pin, busy := "no", "no"
	if st.Pinned {
		pin = "yes"
	}
	switch {
	case st.Busy:
		busy = fmt.Sprintf("yes (%d processing)", max(st.Processing, 1))
	case !st.BusyKnown:
		busy = "unknown (no /slots or /metrics)"
	}
	var auto string
	switch {
	case st.Owner != ownerCS:
		auto = "never (external)"
	case st.Pinned:
		auto = "off (pinned)"
	case !st.BusyKnown:
		auto = "off (can't see if busy)"
	case st.Busy:
		auto = fmt.Sprintf("after %s idle", st.IdleLimit)
	default:
		auto = "in " + max(st.IdleLimit-st.IdleFor, 0).Round(time.Second).String()
	}
	return fmt.Sprintf("%s :%d: %s | owner %s | pinned %s | busy %s | idle stop %s", label, port, run, st.Owner, pin, busy, auto)
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
