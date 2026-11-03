package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// One desktop icon per model: "contextsaver start <name>" warns when a model or a chat is
// already running and asks first; on yes it closes those chat windows, unloads what runs,
// loads <name> and opens its chat in the icon's own terminal window.

type aiRunning struct {
	chats   []procInfo // ContextSaver chat windows (contextsaver chat / start)
	servers []procInfo // model servers (llama-server and friends), not the search index
	served  []string   // models answering on the chat port
	port    int
}

func findRunningAI() aiRunning {
	_, port := scriptAddr(firstScript())
	r := aiRunning{port: port, served: uniq(servedModels(guardHost, port))}
	ps, _ := procs()
	self, parent := os.Getpid(), os.Getppid()
	embed := guardPIDOnPort(ragPort)
	for _, p := range ps {
		if p.pid == self || p.pid == parent {
			continue
		}
		n := strings.ToLower(p.name)
		f := strings.Fields(p.cmd)
		if strings.HasPrefix(n, "contextsaver") && len(f) > 1 && (f[1] == "chat" || f[1] == "start") {
			r.chats = append(r.chats, p)
			continue
		}
		if p.pid != embed && isModelProc(p.name) {
			r.servers = append(r.servers, p)
		}
	}
	return r
}

func isModelProc(name string) bool {
	n := strings.ToLower(name)
	for _, w := range localModelProcs {
		if strings.HasPrefix(n, w) {
			return true
		}
	}
	return false
}

// loadedAI finds the model loaded right now, whoever started it: on the chat port first,
// then on the port of any other model server that runs (not the search index). nil when
// none is; host and port are then the chat port's.
func loadedAI(host string, port int, say func(string)) (string, int, []string) {
	type addr struct {
		host string
		port int
	}
	cands := []addr{{host, port}}
	seen := map[int]bool{port: true, ragPort: true}
	embed := guardPIDOnPort(ragPort)
	ps, _ := procs()
	for _, p := range ps {
		if p.pid == embed || !isModelProc(p.name) {
			continue
		}
		if h, pt := addrIn(p.cmd); !seen[pt] {
			seen[pt] = true
			cands = append(cands, addr{h, pt})
		}
	}
	for _, c := range cands {
		if ms := waitServed(c.host, c.port, say); len(ms) > 0 {
			return c.host, c.port, ms
		}
	}
	return host, port, nil
}

var servedPoll = 2 * time.Second

// waitServed: the models answering on host:port, after waiting (up to 10 minutes) while
// the server there is still loading its model; nil when nothing serves there.
func waitServed(host string, port int, say func(string)) []string {
	ms := servedModels(host, port)
	if !serverLoading(host, port) {
		return ms
	}
	say(fmt.Sprintf("The model on port %d is still loading; waiting for it…", port))
	for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); {
		time.Sleep(servedPoll)
		ms = servedModels(host, port)
		if !serverLoading(host, port) {
			return ms
		}
	}
	return ms
}

func (r aiRunning) any() bool { return len(r.chats) > 0 || len(r.servers) > 0 || len(r.served) > 0 }

func (r aiRunning) describe() string {
	var b strings.Builder
	if len(r.served) > 0 {
		fmt.Fprintf(&b, "  model on port %d: %s\n", r.port, strings.Join(shortModelNames(r.served), ", "))
	}
	for _, p := range r.servers {
		m := ""
		if i := strings.Index(p.cmd, ".gguf"); i > 0 {
			m = " " + filepath.Base(p.cmd[strings.LastIndexAny(p.cmd[:i], " /")+1:i+5])
		}
		fmt.Fprintf(&b, "  %s (pid %d)%s, %s RAM\n", p.name, p.pid, m, hb(p.mem))
	}
	for _, p := range r.chats {
		fmt.Fprintf(&b, "  chat window (pid %d)\n", p.pid)
	}
	return b.String()
}

func shortModelNames(ms []string) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = filepath.Base(m)
	}
	return out
}

// consoleStartModel: the per-model desktop icon.
func consoleStartModel(name string) int {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := modelFileCheck(name); err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	if _, ok := models()[name]; !ok {
		fmt.Printf("No model called %s in %s (have: %s)\n", name, modelsPath(), strings.Join(modelNames(), ", "))
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	if msg := startHelper(); msg != "" && !strings.HasSuffix(msg, "ready") {
		fmt.Println(msg) // chat shows it again at the top
	}
	r := findRunningAI()
	if r.any() {
		fmt.Println("A local AI is running already:")
		fmt.Print(r.describe())
		if !askYes("Unload it, close its chat and load "+name+modelDesc(name)+"? [y/N] ", false) {
			fmt.Println("Left everything as it was.")
			time.Sleep(2 * time.Second)
			return 0
		}
		if err := stopRunningAI(r); err != nil {
			fmt.Println(err)
			waitEnter("Press Enter to close.", 0)
			return 1
		}
	}
	fmt.Println("Loading", name+modelDesc(name), "…")
	out, err := useModel(name, 600, true)
	if err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	fmt.Println(out)
	return consoleChat(nil)
}

// stopRunningAI closes the chat windows and stops every model server, as the user
// just agreed to (busy or started elsewhere included); the search index model stays.
func stopRunningAI(r aiRunning) error {
	for _, p := range r.chats {
		syscall.Kill(p.pid, syscall.SIGTERM) // the chat saves itself; its terminal window closes with it
	}
	for i := 0; i < 15 && anyAlive(r.chats); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range r.chats {
		if pidAlive(p.pid) {
			syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
	if len(r.chats) > 0 {
		fmt.Println("Closed", len(r.chats), "chat window(s)")
	}
	if pid := guardPIDOnPort(r.port); pid > 0 || len(r.served) > 0 {
		_, own := inspectServer(r.port)
		killServer(r.port, own)
	}
	for _, p := range r.servers {
		if pidAlive(p.pid) {
			killTree(p.pid)
		}
	}
	editReg(func(g *guardReg) {
		for k, e := range g.Servers {
			if e.Port != ragPort {
				delete(g.Servers, k)
			}
		}
	})
	for i := 0; i < 20 && (guardPIDOnPort(r.port) > 0 || anyAlive(r.servers)); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if guardPIDOnPort(r.port) > 0 {
		return fmt.Errorf("port %d is still in use; nothing new was loaded", r.port)
	}
	fmt.Println("Unloaded; VRAM freed")
	return nil
}

func anyAlive(ps []procInfo) bool {
	for _, p := range ps {
		if pidAlive(p.pid) {
			return true
		}
	}
	return false
}

// writeModelIcons puts "Local AI - <name>" on the Desktop for every model in models.json
// and removes the icons of models no longer listed.
func writeModelIcons(exe string, names []string) {
	dir := desktopDir()
	keep := map[string]bool{}
	for _, n := range names {
		f := "Local AI - " + n + ".desktop"
		keep[f] = true
		desc := strings.TrimPrefix(modelDesc(n), " - ")
		if desc == "" {
			desc = n
		}
		writeLauncher(filepath.Join(dir, f), "Local AI - "+n, "Load "+desc+" and open its chat (asks first if another model runs)",
			appIcon("contextsaver"), []string{exe, "start", n}, true, 0)
	}
	old, _ := filepath.Glob(filepath.Join(dir, "Local AI - *.desktop"))
	for _, p := range old {
		if !keep[filepath.Base(p)] && strings.Contains(desktopEntryValue(p, "Exec"), " start ") {
			os.Remove(p)
		}
	}
	if p := filepath.Join(dir, "Local AI.desktop"); exists(p) && len(names) > 0 {
		os.Remove(p) // replaced by one icon per model
	}
}

// killChats closes ContextSaver chat windows (Stop Local AI).
func killChats() int {
	r := findRunningAI()
	for _, p := range r.chats {
		syscall.Kill(p.pid, syscall.SIGTERM)
	}
	return len(r.chats)
}

var _ = strconv.Itoa
