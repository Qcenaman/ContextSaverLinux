package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Desktop icons: "Local AI" loads the last-used model and opens AnythingLLM;
// "Stop Local AI" stops the local model ContextSaver started (when idle; servers
// started elsewhere stay up for RemoteAI and others) and closes AnythingLLM, which also ends
// ContextSaver. The admin helper starts on demand and stops by itself.

// anythingLLMExe: the AppImage its installer puts in ~ (or ANYTHING_LLM_INSTALL_DIR),
// else what its menu entry runs.
func anythingLLMExe() string {
	h, _ := os.UserHomeDir()
	for _, d := range []string{os.Getenv("ANYTHING_LLM_INSTALL_DIR"), h} {
		if d != "" {
			if p := filepath.Join(d, "AnythingLLMDesktop.AppImage"); exists(p) {
				return p
			}
		}
	}
	if ex := desktopEntryValue(filepath.Join(h, ".local", "share", "applications", "anythingllmdesktop.desktop"), "Exec"); ex != "" {
		if f := batTokens(ex); len(f) > 0 && exists(f[0]) {
			return f[0]
		}
	}
	return filepath.Join(h, "AnythingLLMDesktop.AppImage")
}

const anythingProc = "anythingllm" // the AppImage runs as anythingllm-desktop

func consoleStartLocalAI() int {
	if msg := startHelper(); msg != "" {
		fmt.Println(msg)
	}
	spawnApprovals()
	if useLMStudio() {
		out, err := startLMStudio()
		fmt.Println(out + syncFrontends())
		if err != nil {
			fmt.Println(err)
			waitEnter("Press Enter to close.", 0)
			return 1
		}
		time.Sleep(2 * time.Second)
		return 0
	}
	name := modelForScript(cfg.AIScript)
	if _, ok := models()[name]; !ok {
		name = "qwen"
	}
	if cur, st := currentModel(); st == "loaded" && cur != "" {
		fmt.Println("Using the loaded model:", cur)
	} else {
		fmt.Println("Loading", name, "…")
		out, err := useModel(name, 600, false)
		if err != nil {
			fmt.Println(err)
			waitEnter("Press Enter to close.", 0)
			return 1
		}
		fmt.Println(out)
	}
	if exe := anythingLLMExe(); exists(exe) {
		if !processRunning(anythingProc) {
			c := exec.Command(exe)
			c.Dir = filepath.Dir(exe)
			detach(c, false)
			if err := c.Start(); err != nil {
				fmt.Println("Couldn't open AnythingLLM:", err)
			}
		}
	} else {
		return consoleChat(nil) // no AnythingLLM here: ContextSaver's own chat, in this window
	}
	time.Sleep(2 * time.Second)
	return 0
}

func consoleStopLocalAI() int {
	if useLMStudio() {
		fmt.Println(stopLMStudio(false))
	} else {
		fmt.Println(stopModel(false))
	}
	if !useLMStudio() && processRunning(anythingProc) {
		signalProcs(anythingProc, syscall.SIGTERM) // ask it to close first
		for i := 0; i < 20 && processRunning(anythingProc); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		if processRunning(anythingProc) {
			signalProcs(anythingProc, syscall.SIGKILL)
		}
		fmt.Println("AnythingLLM closed")
	}
	if n := killChats(); n > 0 {
		fmt.Println("Closed", n, "chat window(s)")
	}
	if stopOtherContextSavers() > 0 {
		fmt.Println("ContextSaver closed")
	}
	if msg := stopHelper(); msg != "" {
		fmt.Println(msg)
	}
	time.Sleep(2 * time.Second)
	return 0
}

// signalProcs signals this user's processes whose name contains part.
func signalProcs(part string, sig syscall.Signal) {
	ps, _ := procs()
	for _, p := range ps {
		if n := strings.ToLower(p.name); (n == part || strings.HasPrefix(n, part+"-")) && p.pid != os.Getpid() {
			syscall.Kill(p.pid, sig)
		}
	}
}

// stopOtherContextSavers ends ContextSaver tool processes (not this one, not the
// admin helper, which stops by itself).
func stopOtherContextSavers() int {
	ps, err := procs()
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range ps {
		if strings.HasPrefix(strings.ToLower(p.name), "contextsaver") && p.pid != os.Getpid() && !strings.Contains(strings.ToLower(p.cmd), " helper") {
			if syscall.Kill(p.pid, syscall.SIGTERM) == nil {
				n++
			}
		}
	}
	return n
}

// processRunning: a program named exe (".exe" ignored), or exe-something, is running
// (AnythingLLM's AppImage runs as anythingllm-desktop).
func processRunning(exe string) bool {
	exe = strings.TrimSuffix(strings.ToLower(exe), ".exe")
	ps, err := procs()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if n := strings.ToLower(p.name); n == exe || strings.HasPrefix(n, exe+"-") {
			return true
		}
	}
	return false
}

// anythingStorage: AnythingLLM Desktop's data folder on Linux.
func anythingStorage() string {
	d := os.Getenv("XDG_CONFIG_HOME")
	if d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".config")
	}
	return filepath.Join(d, "anythingllm-desktop", "storage")
}

// writeDesktopShortcuts puts the two icons on the Desktop (with AnythingLLM's icon).
func writeDesktopShortcuts(exe string) {
	icon := appIcon("anythingllm")
	startDesc, stopDesc := "Load the local model and open AnythingLLM", "Stop the local model and close AnythingLLM"
	if !exists(anythingLLMExe()) {
		startDesc, stopDesc = "Load the local model and chat with it (ContextSaver Chat)", "Stop the local model"
	}
	if useLMStudio() {
		icon = appIcon("lm-studio", "lmstudio", "lm studio")
		startDesc, stopDesc = "Start LM Studio's server and load the local model", "Unload the local model and stop LM Studio's server"
	}
	var failed error
	icons := []struct{ name, arg, desc string }{{"Local AI", "start", startDesc}, {"Stop Local AI", "stop", stopDesc}}
	if !exists(anythingLLMExe()) && !useLMStudio() {
		icons = icons[1:] // one "Local AI - <model>" icon per model instead (writeModelIcons)
		stopDesc = "Close the chat windows and stop the local model"
		icons[0].desc = stopDesc
	}
	for _, s := range icons {
		if err := writeLauncher(filepath.Join(desktopDir(), s.name+".desktop"), s.name, s.desc, icon, []string{exe, s.arg}, true, 0); err != nil {
			failed = err
		}
	}
	if failed != nil {
		fmt.Println("Couldn't create the desktop icons:", failed)
		return
	}
	h, _ := os.UserHomeDir()
	chatDesc := "Chat with the local AI that's loaded now, with ContextSaver's tools (loads the last one used when none is)"
	writeLauncher(filepath.Join(desktopDir(), "ContextSaver Chat.desktop"), "ContextSaver Chat", chatDesc, "", []string{exe, "chat"}, true, 0)
	writeLauncher(filepath.Join(h, ".local", "share", "applications", "contextsaver-chat.desktop"), "ContextSaver Chat",
		chatDesc, "", []string{exe, "chat"}, true, 0)
	fmt.Println("Desktop icons: ContextSaver Chat, Local AI - <model> for each model, Stop Local AI; app menu: ContextSaver Chat")
}
