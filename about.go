package main

import (
	"os"
	"path/filepath"
	"strings"
)

// about.md: a short card about the user that every chat can reach (pc me, and the MCP
// instructions), so the local model knows his own apps instead of web-searching them.
// The user can edit data\about.md; details live in rag notes tagged "about".

const defaultAbout = `About the user: lives in the Philippines (learning Tagalog). Desktop PC: RTX 5090 32 GB, 64 GB RAM; also a laptop and a Samsung phone running AdbSaver.
His own apps are in D:\Claude Apps\<name> and github.com/Qcenaman/<name> (releases there): ContextSaver (this pc tool), TokenSaver (Claude's twin), AdbSaver (phone engine; pc phone uses it), RemoteAI (his phone chat app), AdminHelper. Never web-search them. Install one on the phone: phone{action:install,app:<name>}.
Asking = permission: act, don't ask first. Code: a few tests + GitHub Actions, no docs unless asked.
More: rag{query,tag:"about"} (devices, apps, ComfyUI, LoRA training, WhatsApp recovery).`

func aboutPath() string { return filepath.Join(appHome, "about.md") }

// aboutCard returns data\about.md, creating it with the default card the first time.
func aboutCard() string {
	if appHome == "" {
		return defaultAbout
	}
	if b, err := os.ReadFile(aboutPath()); err == nil {
		if s := strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf")); s != "" {
			return s
		}
	}
	os.MkdirAll(appHome, 0o755)
	os.WriteFile(aboutPath(), []byte(defaultAbout+"\n"), 0o644)
	return defaultAbout
}
