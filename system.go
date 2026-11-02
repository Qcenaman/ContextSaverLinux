package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type procInfo struct {
	pid  int
	name string
	mem  int64
	cmd  string
}

type portInfo struct {
	port int
	addr string
	pid  int
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }

func runQuietCtx(ctx context.Context, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	hideWindow(c)
	out, err := c.CombinedOutput()
	s, derr := decodeText(out)
	if derr != nil {
		s = strings.ToValidUTF8(string(out), "?")
	}
	return strings.TrimSpace(s), err
}

func runQuiet(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runQuietCtx(ctx, name, args...)
}

func toolSystem(raw json.RawMessage) (string, error) {
	var a struct {
		Action string `json:"action"`
		Filter string `json:"filter"`
		Port   int    `json:"port"`
		PIDs   []int  `json:"pids"`
		Name   string `json:"name"`
		Match  string `json:"match"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	switch strings.ToLower(a.Action) {
	case "", "info":
		return sysInfo(), nil
	case "processes":
		return procList(a.Filter)
	case "ports":
		return portList(a.Port)
	case "kill":
		return killProcs(a.PIDs, a.Name, a.Match)
	}
	return "", errors.New("action must be info, processes, ports or kill")
}

var cudaRe = regexp.MustCompile(`CUDA Version:\s*([0-9.]+)`)

func sysInfo() string {
	var lines []string
	var gpu, cuda string
	tools := make([]string, len(toolChecks))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out, err := runQuiet(15*time.Second, "nvidia-smi", "--query-gpu=name,driver_version,memory.total,memory.used,utilization.gpu,temperature.gpu", "--format=csv,noheader")
		if err != nil {
			if g := sysGPUInfo(); g != "" {
				gpu = "GPU: " + g
			} else {
				gpu = "GPU: none found"
			}
			return
		}
		var gs []string
		for _, l := range strings.Split(out, "\n") {
			f := strings.Split(l, ",")
			if len(f) >= 6 {
				gs = append(gs, fmt.Sprintf("%s, driver %s, %s VRAM (%s used), %s busy, %s°C",
					strings.TrimSpace(f[0]), strings.TrimSpace(f[1]), strings.TrimSpace(f[2]), strings.TrimSpace(f[3]), strings.TrimSpace(f[4]), strings.TrimSpace(f[5])))
			}
		}
		gpu = "GPU: " + strings.Join(gs, " | ")
		if o, err := runQuiet(15*time.Second, "nvidia-smi"); err == nil {
			if m := cudaRe.FindStringSubmatch(o); m != nil {
				cuda = m[1]
			}
		}
	}()
	go func() {
		defer wg.Done()
		var tw sync.WaitGroup
		for i, t := range toolChecks {
			tw.Add(1)
			go func(i int, t toolCheck) {
				defer tw.Done()
				tools[i] = checkTool(t)
			}(i, t)
		}
		tw.Wait()
	}()
	lines = append(lines, platformInfo()...)
	wg.Wait()
	if cuda != "" {
		gpu += " · driver supports CUDA up to " + cuda
	}
	lines = append(lines, gpu)
	var found, missing []string
	for i, t := range tools {
		if t == "" {
			missing = append(missing, toolChecks[i].name)
		} else {
			found = append(found, t)
		}
	}
	if z := find7z(); z != "" {
		found = append(found, "7-Zip ("+z+")")
	} else {
		missing = append(missing, "7-Zip")
	}
	lines = append(lines, "Tools: "+strings.Join(found, " · "))
	if len(missing) > 0 {
		lines = append(lines, "Not found: "+strings.Join(missing, ", "))
	}
	return strings.Join(lines, "\n")
}

type toolCheck struct {
	name  string
	args  []string
	pick  string
	extra []string
}

var toolChecks = []toolCheck{
	{name: "python", args: []string{"--version"}},
	{name: "python3", args: []string{"--version"}},
	{name: "git", args: []string{"--version"}},
	{name: "uv", args: []string{"--version"}},
	{name: "conda", args: []string{"--version"}, extra: []string{"~/anaconda3/bin/conda", "~/miniconda3/bin/conda", "~/miniforge3/bin/conda"}},
	{name: "node", args: []string{"--version"}},
	{name: "nvcc", args: []string{"--version"}, pick: "release"},
	{name: "ffmpeg", args: []string{"-version"}},
	{name: "go", args: []string{"version"}},
	{name: "gh", args: []string{"--version"}},
	{name: "ctags", args: []string{"--version"}},
	{name: "gcc", args: []string{"--version"}},
	{name: "cargo", args: []string{"--version"}},
	{name: "java", args: []string{"-version"}},
	{name: "x86_64-w64-mingw32-gcc", args: []string{"--version"}},
	{name: "llama-server", args: []string{"--version"}, extra: []string{"~/llama.cpp/llama-server"}},
	{name: "tesseract", args: []string{"--version"}},
	{name: "soffice", args: []string{"--version"}},
	{name: "adb", args: []string{"version"}},
}

func checkTool(t toolCheck) string {
	p, err := lookPath(t.name)
	if err != nil {
		for _, e := range t.extra {
			if x, err := expandPath(e); err == nil && exists(x) {
				p, err = x, nil
				break
			}
		}
		if p == "" {
			return ""
		}
	}
	if strings.Contains(strings.ToLower(p), `\windowsapps\`) && (t.name == "python" || t.name == "py") {
		return t.name + " (only the Microsoft Store shortcut, not a real install)"
	}
	out, err := runQuiet(10*time.Second, p, t.args...)
	ver := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if t.pick == "" || strings.Contains(l, t.pick) {
			ver = l
			break
		}
	}
	if err != nil && ver == "" {
		ver = "found, but it didn't run"
	}
	return fmt.Sprintf("%s: %s (%s)", t.name, clip(ver, 80), p)
}

func procList(filter string) (string, error) {
	ps, err := procs()
	if err != nil {
		return "", err
	}
	f := strings.ToLower(strings.TrimSpace(filter))
	var out []procInfo
	for _, p := range ps {
		if f == "" || strings.Contains(strings.ToLower(p.name), f) || strings.Contains(strings.ToLower(p.cmd), f) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].mem > out[b].mem })
	if len(out) == 0 {
		return "No matching processes.", nil
	}
	limit := 40
	if f == "" {
		limit = 25
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d process(es)", len(out))
	if f == "" {
		b.WriteString(", biggest first")
	}
	b.WriteString(":")
	for i, p := range out {
		if i >= limit {
			fmt.Fprintf(&b, "\n+%d more", len(out)-limit)
			break
		}
		fmt.Fprintf(&b, "\n%d %s %s", p.pid, p.name, hb(p.mem))
		if p.cmd != "" {
			b.WriteString(" — " + clip(p.cmd, 200))
		}
	}
	return b.String(), nil
}

func portList(port int) (string, error) {
	ls, err := listening()
	if err != nil {
		return "", err
	}
	names := procNames()
	var b strings.Builder
	n := 0
	sort.Slice(ls, func(a, c int) bool { return ls[a].port < ls[c].port })
	seen := map[string]bool{}
	for _, l := range ls {
		if port > 0 && l.port != port {
			continue
		}
		k := fmt.Sprint(l.port, l.pid)
		if seen[k] {
			continue
		}
		seen[k] = true
		n++
		if n > 60 {
			continue
		}
		fmt.Fprintf(&b, "\nport %d (%s) — pid %d %s", l.port, l.addr, l.pid, names[l.pid])
	}
	if n == 0 {
		if port > 0 {
			return fmt.Sprintf("Nothing is listening on port %d.", port), nil
		}
		return "No listening ports found.", nil
	}
	if n > 60 {
		fmt.Fprintf(&b, "\n+%d more", n-60)
	}
	return fmt.Sprintf("%d listening:", n) + b.String(), nil
}

var protectedProcs = map[string]bool{
	"system": true, "idle": true, "registry": true, "smss.exe": true, "csrss.exe": true, "wininit.exe": true,
	"winlogon.exe": true, "services.exe": true, "lsass.exe": true, "svchost.exe": true, "explorer.exe": true,
	"dwm.exe": true, "fontdrvhost.exe": true, "sihost.exe": true, "claude.exe": true, "contextsaver.exe": true, "claudessistent.exe": true,
	"init": true, "systemd": true, "systemd-journald": true, "systemd-logind": true, "systemd-udevd": true, "dbus-daemon": true,
	"dbus-broker": true, "xorg": true, "xwayland": true, "gnome-shell": true, "gdm3": true, "sddm": true, "kwin_wayland": true,
	"plasmashell": true, "sshd": true, "networkmanager": true, "pipewire": true, "wireplumber": true, "polkitd": true,
	"claude": true, "contextsaver": true, "anythingllm-desktop": true,
}

func killProcs(pids []int, name, match string) (string, error) {
	ps, err := procs()
	if err != nil {
		return "", err
	}
	byPID := map[int]procInfo{}
	for _, p := range ps {
		byPID[p.pid] = p
	}
	var targets []procInfo
	if len(pids) > 0 {
		for _, id := range pids {
			p, ok := byPID[id]
			if !ok {
				continue
			}
			targets = append(targets, p)
		}
	} else if name != "" {
		want := strings.ToLower(strings.TrimSpace(name))
		for _, p := range ps {
			n := strings.ToLower(p.name)
			if n != want && strings.TrimSuffix(n, ".exe") != strings.TrimSuffix(want, ".exe") {
				continue
			}
			if match != "" && !strings.Contains(strings.ToLower(p.cmd), strings.ToLower(match)) {
				continue
			}
			targets = append(targets, p)
		}
	} else {
		return "", errors.New("give pids, or name (and optionally match)")
	}
	if len(targets) == 0 {
		return "No matching processes are running.", nil
	}
	self := os.Getpid()
	var stopped, refused []string
	for _, p := range targets {
		if p.pid == self || p.pid <= 4 || protectedProcs[strings.ToLower(p.name)] {
			refused = append(refused, fmt.Sprintf("%d %s", p.pid, p.name))
			continue
		}
		killTree(p.pid)
		stopped = append(stopped, fmt.Sprintf("%d %s", p.pid, p.name))
	}
	time.Sleep(700 * time.Millisecond)
	var still []string
	for _, s := range stopped {
		var id int
		fmt.Sscan(s, &id)
		if pidAlive(id) {
			still = append(still, s)
		}
	}
	var b strings.Builder
	if len(stopped) > 0 {
		b.WriteString("Stopped: " + strings.Join(stopped, ", "))
	}
	if len(still) > 0 {
		b.WriteString("\nStill running (may need admin rights): " + strings.Join(still, ", "))
	}
	if len(refused) > 0 {
		b.WriteString("\nLeft alone (system or Claude processes): " + strings.Join(refused, ", "))
	}
	return strings.TrimSpace(b.String()), nil
}

func fmtDisk(letter, kind string, free, total uint64) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%s %s free of %s (%s)", letter, hb(int64(free)), hb(int64(total)), kind)
}

func appExeDir() string {
	e, _ := os.Executable()
	return filepath.Dir(e)
}
