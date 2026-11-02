package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// gpuUser is a program that is likely holding the GPU.
type gpuUser struct {
	what string
	pids []int
}

var gpuApps = []struct {
	what  string
	match func(name, cmd string) bool
}{
	{"LoRA training (AI Toolkit)", func(n, c string) bool {
		return strings.Contains(c, "ai-toolkit") || strings.Contains(c, "ai_toolkit") || (strings.Contains(c, `\toolkit\`) || strings.Contains(c, "/toolkit/")) && strings.Contains(c, "run.py")
	}},
	{"ComfyUI", func(n, c string) bool { return strings.Contains(n, "comfyui") || strings.Contains(c, "comfyui") }},
}

// gpuBusy reports what is using the GPU right now. It returns "" when the GPU looks free.
func gpuBusy() string {
	var reasons []string
	if ps, err := procs(); err == nil {
		found := map[string]*gpuUser{}
		var order []string
		for _, p := range ps {
			n, c := strings.ToLower(p.name), strings.ToLower(p.cmd)
			if strings.Contains(c, "contextsaver") || strings.Contains(n, "contextsaver") || strings.Contains(c, "claudessistent") || strings.Contains(n, "claudessistent") {
				continue
			}
			for _, a := range gpuApps {
				if a.match(n, c) {
					u := found[a.what]
					if u == nil {
						u = &gpuUser{what: a.what}
						found[a.what] = u
						order = append(order, a.what)
					}
					u.pids = append(u.pids, p.pid)
					break
				}
			}
		}
		for _, w := range order {
			u := found[w]
			ids := make([]string, 0, len(u.pids))
			for i, id := range u.pids {
				if i >= 3 {
					ids = append(ids, "…")
					break
				}
				ids = append(ids, strconv.Itoa(id))
			}
			reasons = append(reasons, fmt.Sprintf("%s is running (pid %s)", u.what, strings.Join(ids, ", ")))
		}
	}
	out, err := runQuiet(15*time.Second, "nvidia-smi", "--query-gpu=memory.used,memory.total,utilization.gpu", "--format=csv,noheader,nounits")
	if err == nil {
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(l, ",")
			if len(f) < 3 {
				continue
			}
			used, _ := strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
			total, _ := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
			util, _ := strconv.Atoi(strings.TrimSpace(f[2]))
			// Windows and a browser alone hold a few GB; more than that or a busy GPU means real work.
			if used > 6144 || util >= 40 || len(reasons) > 0 {
				reasons = append(reasons, fmt.Sprintf("GPU has %.1f of %.1f GB VRAM in use and is %d%% busy", used/1024, total/1024, util))
			}
		}
	}
	if err != nil {
		if b := sysGPUBusy(); b != "" {
			reasons = append(reasons, b)
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	return strings.Join(reasons, "; ")
}

// aiWouldLoad reports whether running a local AI job would load a model onto the GPU.
// A llama.cpp server that is already up is reused, so nothing new is loaded.
func aiWouldLoad() bool {
	if cfg.AIEndpoint == "" && cfg.AIScript != "" && exists(cfg.AIScript) {
		host, port := scriptAddr(cfg.AIScript)
		c := &aiClient{kind: "openai", base: fmt.Sprintf("http://%s:%d", host, port)}
		return !c.healthy()
	}
	return true
}

// gpuWarning returns a warning to show instead of loading the local AI, or "" if it is fine to go ahead.
func gpuWarning() string {
	if aiServerUp() {
		return ""
	}
	if !aiWouldLoad() {
		return ""
	}
	busy := gpuBusy()
	if busy == "" {
		return ""
	}
	return "WARNING: the local AI model was NOT loaded because the GPU looks busy: " + busy +
		". Loading it now would compete for VRAM and could slow down or crash that work. Tell the user and ask whether to go ahead; only if they say yes, call local_ai again with the same arguments plus force:true."
}
