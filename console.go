package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

var stdin = bufio.NewReader(os.Stdin)

func prompt(q string) string {
	fmt.Print(q)
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

func askYes(q string, def bool) bool {
	a := strings.ToLower(prompt(q))
	if a == "" {
		return def
	}
	return a == "y" || a == "yes"
}

func waitEnter(msg string, timeout time.Duration) {
	fmt.Print(msg)
	ch := make(chan struct{})
	go func() { stdin.ReadString('\n'); close(ch) }()
	if timeout <= 0 {
		<-ch
		return
	}
	select {
	case <-ch:
	case <-time.After(timeout):
	}
}

func installPath() string {
	return filepath.Join(appRoot(), "app", "contextsaver")
}

func consoleMenu() {
	exe, _ := os.Executable()
	if !samePath(exe, installPath()) {
		fmt.Printf("ContextSaver %s for Linux\n\n", appVersion)
		fmt.Println("This installs ContextSaver for your Linux account and connects it to AnythingLLM")
		fmt.Println("and LM Studio, so your local AI can use this PC with as little context as possible.")
		fmt.Println()
		if askYes("Install now? [Y/n] ", true) {
			consoleInstall()
		}
		waitEnter("\nPress Enter to close.", 0)
		return
	}
	terminal()
}

func consoleRunSaved(name string) int {
	sj, err := loadSaved(name)
	if err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	fmt.Printf("ContextSaver — %s\n%s\n\n", sj.Name, describeSteps(sj.Steps))
	if hasRisky(sj.Steps) {
		fmt.Println("This job deletes, renames or replaces files. Checking what it would do…")
		s := runConsoleJob(JobSpec{Name: sj.Name, Kind: "files", Steps: sj.Steps, DryRun: true, Source: "you"})
		fmt.Println(formatState(s))
		if s.Status != "done" {
			waitEnter("\nPress Enter to close.", 0)
			return 1
		}
		if !askYes("\nGo ahead? [y/N] ", false) {
			return 1
		}
	}
	s := runConsoleJob(JobSpec{Name: sj.Name, Kind: "files", Steps: sj.Steps, Source: "you"})
	fmt.Println(formatState(s))
	ok := s.Status == "done"
	for _, r := range s.Steps {
		if r.ErrorCount > 0 {
			ok = false
		}
	}
	if ok {
		waitEnter("\nDone. This window closes in 15 seconds (or press Enter).", 15*time.Second)
		return 0
	}
	waitEnter("\nPress Enter to close.", 0)
	return 1
}

func runConsoleJob(spec JobSpec) *JobState {
	j, err := newJob(spec)
	if err != nil {
		return &JobState{Status: "failed", Error: err.Error()}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	j.start()
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-sig:
			fmt.Print("\rStopping…" + strings.Repeat(" ", 100))
			j.cancel()
		case <-j.done:
			fmt.Print("\r" + strings.Repeat(" ", 110) + "\r")
			s := j.snapshot()
			return &s
		case <-t.C:
			s := j.snapshot()
			line := progressLine(&s)
			if s.Current != "" {
				line += " · " + filepath.Base(s.Current)
			}
			fmt.Printf("\r%-110s", clip(line, 109))
		}
	}
}

func editClaudeConfig(path string, edit func(servers map[string]any)) error {
	m := map[string]any{}
	b, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s isn't valid JSON, so I left it untouched", path)
		}
		os.WriteFile(path+".bak", b, 0o644)
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	edit(servers)
	m["mcpServers"] = servers
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func consoleInstall() {
	migrateOldInstall()
	target := installPath()
	exe, _ := os.Executable()
	if !samePath(exe, target) {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			fmt.Println("Couldn't create", filepath.Dir(target)+":", err)
			return
		}
		if exists(target) {
			old := target + ".old"
			if exists(old) {
				keep := filepath.Join(appRoot(), "old", "ContextSaver program")
				os.MkdirAll(keep, 0o755)
				os.Rename(old, filepath.Join(keep, "contextsaver.old."+time.Now().Format("20060102150405")))
			}
			if err := os.Rename(target, old); err != nil {
				fmt.Println("Couldn't replace the installed copy (is it in use?):", err)
				return
			}
		}
		if err := copyPlain(exe, target); err != nil {
			fmt.Println("Couldn't copy the app:", err)
			return
		}
		os.Chmod(target, 0o755)
		fmt.Println("Installed to", target)
	}
	linkOnPath(target)
	connected := 0
	var todo []string
	for _, p := range anythingConfigPaths() {
		if configHasEntry(p, target) {
			fmt.Println("Already connected to AnythingLLM in", p)
			connected++
			continue
		}
		todo = append(todo, p)
	}
	if false {
		fmt.Println("\nClaude is open. It rewrites its settings when it closes, which would undo this.")
		for claudeRunning() {
			a := prompt("Quit Claude completely (right-click its icon near the clock → Quit), then press Enter. Type s to skip: ")
			if strings.EqualFold(a, "s") {
				todo = nil
				break
			}
		}
	}
	for _, p := range todo {
		err := editClaudeConfig(p, func(s map[string]any) {
			// ContextSaver replaces these, whose tool lists fill a local model's context.
			delete(s, "tokensaver")
			delete(s, "windows-mcp")
			s["contextsaver"] = map[string]any{"command": target, "args": []string{"mcp"}}
		})
		if err != nil {
			fmt.Println("Couldn't update", p+":", err)
			continue
		}
		fmt.Println("Connected to AnythingLLM in", p)
		connected++
	}
	if p, err := connectLMStudio(target); err != nil {
		fmt.Println("Couldn't update", p+":", err)
	} else if p != "" {
		fmt.Println("Connected to LM Studio in", p)
		connected++
	}
	if connected == 0 && len(anythingConfigPaths()) == 0 && lmStudioMCPPath() == "" {
		fmt.Println("No AnythingLLM or LM Studio here: chat with the local model through a \"Local AI - <model>\" icon (ContextSaver Chat).")
	} else if connected == 0 {
		fmt.Printf("\nAdd this to the \"mcpServers\" section of AnythingLLM's anythingllm_mcp_servers.json or LM Studio's mcp.json by hand:\n  \"contextsaver\": {\"command\": %q, \"args\": [\"mcp\"]}\n", target)
	}
	refreshHelper()
	os.MkdirAll(jobsFolder(), 0o755)
	writeLauncher(filepath.Join(jobsFolder(), "Open ContextSaver.desktop"), "Open ContextSaver", "ContextSaver's terminal", "", []string{target}, true, 0)
	writeModelShortcuts(target)
	if n := syncFrontends(); n != "" {
		fmt.Println(strings.TrimPrefix(n, "; "))
	}
	writeDesktopShortcuts(target)
	if isTerminal(os.Stdin) && setupNeeded() {
		if askYes("\nInstall the programs ContextSaver uses (llama.cpp, GitHub CLI, compilers, OCR...)? It asks before each change. [Y/n] ", true) {
			consoleSetup()
		}
	}
	if !helperInstalled() {
		fmt.Println("\n⚠ " + helperHowTo())
	}
	if len(anythingConfigPaths()) > 0 {
		fmt.Println("\nAll set. In AnythingLLM open Agent Skills and refresh the MCP servers (or restart AnythingLLM).")
	} else {
		fmt.Println("\nAll set. Double-click a \"Local AI - <model>\" icon on the Desktop to chat.")
	}
	fmt.Println("Everything ContextSaver keeps is in", appRoot())
}

// linkOnPath puts a contextsaver command in ~/.local/bin (on Ubuntu's PATH), so the
// hints "run: contextsaver ..." work in any terminal.
func linkOnPath(target string) {
	h, _ := os.UserHomeDir()
	l := filepath.Join(h, ".local", "bin", "contextsaver")
	if cur, err := os.Readlink(l); err == nil && cur == target {
		return
	}
	os.MkdirAll(filepath.Dir(l), 0o755)
	os.Remove(l)
	if os.Symlink(target, l) == nil {
		fmt.Println("Command: contextsaver (in ~/.local/bin)")
	}
}

func consoleUninstall() {
	for _, p := range anythingConfigPaths() {
		if !exists(p) {
			continue
		}
		if err := editClaudeConfig(p, func(s map[string]any) { delete(s, "contextsaver"); delete(s, "claudessistent") }); err != nil {
			fmt.Println(err)
			continue
		}
		fmt.Println("Disconnected from AnythingLLM in", p)
	}
	if p, err := disconnectLMStudio(); err != nil {
		fmt.Println(err)
	} else if p != "" {
		fmt.Println("Disconnected from LM Studio in", p)
	}
	fmt.Println("Restart AnythingLLM or LM Studio to finish. Your saved jobs and logs are kept in", appHome)
}

func copyPlain(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func configHasEntry(path, target string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	if _, old := m.MCPServers["claudessistent"]; old {
		return false
	}
	e, ok := m.MCPServers["contextsaver"]
	return ok && samePath(e.Command, target) && len(e.Args) == 1 && e.Args[0] == "mcp"
}

// migrateOldInstall moves everything older versions left outside appRoot into it:
// data into data\, and old program copies and Desktop shortcut folders into old\.
// Nothing is deleted; it is moved.
func migrateOldInstall() {
	if !isWin {
		migrateHiddenRoot()
		return
	}
	la := os.Getenv("LOCALAPPDATA")
	old := filepath.Join(appRoot(), "old")
	moves := []struct{ src, dst string }{
		{filepath.Join(la, "ContextSaver"), appHome},
		{filepath.Join(la, "Claudessistent"), filepath.Join(old, "Claudessistent data")},
		{filepath.Join(la, "Programs", "ContextSaver"), filepath.Join(old, "ContextSaver program")},
		{filepath.Join(la, "Programs", "Claudessistent"), filepath.Join(old, "Claudessistent program")},
		{filepath.Join(desktopDir(), "ContextSaver Jobs"), filepath.Join(old, "Desktop ContextSaver Jobs")},
		{filepath.Join(desktopDir(), "Claudessistent Jobs"), filepath.Join(old, "Desktop Claudessistent Jobs")},
	}
	for _, m := range moves {
		if !exists(m.src) || samePath(m.src, m.dst) {
			continue
		}
		if err := moveTree(m.src, m.dst); err != nil {
			fmt.Printf("Couldn't move %s to %s: %v\n", m.src, m.dst, err)
		} else {
			fmt.Println("Moved", m.src, "→", m.dst)
		}
	}
	for _, sj := range listSaved() {
		writeShortcut(sj.Name)
	}
}

// anythingConfigPaths lists AnythingLLM Desktop's MCP server files (none when it isn't installed).
func anythingConfigPaths() []string {
	if !exists(anythingStorage()) && !exists(anythingLLMExe()) {
		return nil
	}
	return []string{filepath.Join(anythingStorage(), "plugins", "anythingllm_mcp_servers.json")}
}
