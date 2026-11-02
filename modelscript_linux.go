package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Model start scripts on Linux. models.json entries with "gguf" get a bash script; a
// "script" entry that is a Windows .bat (the ones on D:\llama.cpp) is turned into a bash
// script that runs the Linux llama-server with the same arguments, its Windows paths
// mapped to the mounted Windows partition. Both land in data/models/<name>.sh.

const genMarker = "# Made by ContextSaver"

// linuxLlamaServer: pref when it is a Linux program, else config llama_server, else the
// usual places (setup puts it in ~/llama.cpp), else PATH.
func linuxLlamaServer(pref string) string {
	if pref != "" && !isWinPath(pref) && !strings.EqualFold(filepath.Ext(pref), ".exe") {
		return pref
	}
	if cfg.LlamaServer != "" {
		if p, err := expandPath(cfg.LlamaServer); err == nil {
			return p
		}
	}
	h, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(h, "llama.cpp", "llama-server"), filepath.Join(h, "llama.cpp", "build", "bin", "llama-server"),
		filepath.Join(h, "llama.cpp", "bin", "llama-server"), "/usr/local/bin/llama-server"} {
		if exists(p) {
			return p
		}
	}
	if p, err := exec.LookPath("llama-server"); err == nil {
		return p
	}
	return filepath.Join(h, "llama.cpp", "llama-server")
}

func llamaServerHint(p string) string {
	return fmt.Sprintf("llama-server for Linux not found: %s (contextsaver setup installs it, or set \"llama_server\" in %s)", p, filepath.Join(appHome, "config.json"))
}

// hostPath: a Windows path mapped to this system (left as it is when its drive isn't mounted).
func hostPath(p string) string {
	if isWinPath(p) {
		if q, err := winToLinux(p); err == nil {
			return q
		}
	}
	return p
}

var shSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shArg quotes one argument for bash, only when it needs it.
func shArg(s string) string {
	if s != "" && shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shLine(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shArg(a)
	}
	return strings.Join(q, " ")
}

func scriptHead(from string) string {
	return "#!/bin/bash\n" + genMarker + " from " + from + "; edit that, not this file.\n"
}

func libPath(server string) string {
	return "export LD_LIBRARY_PATH=" + shArg(filepath.Dir(server)) + "${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\n"
}

// genModelScript is the start script for a gguf entry: one llama-server line.
func genModelScript(e modelEntry, server string) string {
	a := []string{server, "-m", e.GGUF}
	a = append(a, modelArgs(e)...)
	desc := e.Description
	if desc == "" {
		desc = e.Name
	}
	return scriptHead("models.json (entry \""+e.Name+"\")") + libPath(server) +
		"cd " + shArg(filepath.Dir(server)) + "\n" +
		"echo " + shArg(desc) + "\n" +
		"exec " + shLine(a) + "\n"
}

// batPathFlags take a file path as their value; relative ones are resolved like cmd would.
var batPathFlags = map[string]bool{"-m": true, "--model": true, "-mm": true, "--mmproj": true, "-md": true, "--model-draft": true,
	"--chat-template-file": true, "--lora": true, "--grammar-file": true, "--slot-save-path": true, "--log-file": true,
	"-mu": false, "--system-prompt-file": true, "--lora-scaled": true}

var (
	batSetRe = regexp.MustCompile(`(?i)^set\s+"?([^=\s"]+)=(.*?)"?$`)
	batCdRe  = regexp.MustCompile(`(?i)^(?:cd|chdir|pushd)(?:\s+/d)?\s+"?([^"]+?)"?\s*$`)
	batVarRe = regexp.MustCompile(`%(~dp0|[A-Za-z0-9_()]+)%?`)
)

// batToSh turns a Windows llama.cpp start script into a bash script for this system.
func batToSh(bat string) (string, error) {
	src := hostPath(bat)
	b, err := os.ReadFile(src)
	if err != nil {
		if isWinPath(bat) && src == bat {
			_, merr := winToLinux(bat)
			return "", merr
		}
		return "", fmt.Errorf("start script isn't there: %s", bat)
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(b), "\xef\xbb\xbf"), "\r\n", "\n")
	var lines []string
	cur := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimRight(l, " \t")
		if strings.HasSuffix(t, "^") {
			cur += strings.TrimSuffix(t, "^") + " "
			continue
		}
		lines = append(lines, cur+t)
		cur = ""
	}
	dir := filepath.Dir(src)
	vars := map[string]string{}
	expand := func(s string) string {
		s = strings.ReplaceAll(s, "%%", "\x00")
		s = batVarRe.ReplaceAllStringFunc(s, func(m string) string {
			k := strings.ToUpper(strings.Trim(m, "%"))
			switch {
			case k == "~DP0":
				return dir + "/"
			case k == "CD":
				return dir
			}
			if v, ok := vars[k]; ok {
				return v
			}
			if v := os.Getenv(k); v != "" {
				return v
			}
			return m
		})
		return strings.ReplaceAll(s, "\x00", "%")
	}
	cwd := dir
	for _, raw := range lines {
		l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
		low := strings.ToLower(l)
		if l == "" || strings.HasPrefix(low, "rem ") || strings.HasPrefix(low, "::") || strings.HasPrefix(low, "echo") {
			continue
		}
		if m := batSetRe.FindStringSubmatch(l); m != nil {
			vars[strings.ToUpper(m[1])] = expand(m[2])
			continue
		}
		if m := batCdRe.FindStringSubmatch(l); m != nil {
			cwd = linuxArgPath(expand(m[1]), cwd)
			continue
		}
		if !strings.Contains(low, "llama-server") {
			continue
		}
		toks := batTokens(expand(l))
		i := -1
		for k, t := range toks {
			if strings.HasPrefix(strings.ToLower(filepath.Base(strings.ReplaceAll(t, `\`, "/"))), "llama-server") {
				i = k
				break
			}
		}
		if i < 0 {
			continue
		}
		server := linuxLlamaServer("")
		args := []string{server}
		rest := toks[i+1:]
		for k := 0; k < len(rest); k++ {
			t := rest[k]
			lt := strings.ToLower(t)
			if (lt == "--device" || lt == "-dev") && k+1 < len(rest) && strings.Contains(strings.ToLower(rest[k+1]), "cuda") {
				k++ // CUDA device names don't exist on this GPU
				continue
			}
			if batPathFlags[lt] && k+1 < len(rest) {
				args = append(args, t, linuxArgPath(rest[k+1], cwd))
				k++
				continue
			}
			if f, v, ok := strings.Cut(t, "="); ok && strings.HasPrefix(f, "-") && batPathFlags[strings.ToLower(f)] {
				t = f + "=" + linuxArgPath(v, cwd)
			} else if isWinPath(t) {
				t = hostPath(t)
			}
			args = append(args, t)
		}
		var env strings.Builder
		for k, v := range vars {
			if strings.HasPrefix(k, "LLAMA_") || strings.HasPrefix(k, "GGML_") {
				fmt.Fprintf(&env, "export %s=%s\n", k, shArg(v))
			}
		}
		return scriptHead(bat) + env.String() + libPath(server) + "cd " + shArg(cwd) + "\nexec " + shLine(args) + "\n", nil
	}
	return "", fmt.Errorf("%s has no llama-server line, so it can't run on Linux; give the model a \"gguf\" entry in models.json instead", bat)
}

// linuxArgPath maps a path from a .bat: Windows paths to the mounted drive, relative ones
// against the script's current folder.
func linuxArgPath(p, cwd string) string {
	switch {
	case isWinPath(p):
		return hostPath(p)
	case strings.HasPrefix(p, "/"):
		return ciJoin("/", strings.ReplaceAll(p, `\`, "/"))
	}
	return ciJoin(cwd, strings.ReplaceAll(p, `\`, "/"))
}

// batTokens splits a cmd.exe line: spaces separate, double quotes group (and are
// removed), ^ escapes the next character outside quotes.
func batTokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, has := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			if inQ && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			inQ, has = !inQ, true
		case c == '^' && !inQ && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			has = true
		case (c == ' ' || c == '\t') && !inQ:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

func isBatScript(p string) bool {
	e := strings.ToLower(filepath.Ext(p))
	return e == ".bat" || e == ".cmd"
}

// writeBatScript converts a .bat start script and writes it to dst.
func writeBatScript(bat, dst string) error {
	txt, err := batToSh(bat)
	if err != nil {
		return err
	}
	return writeGenScript(dst, txt)
}

var errNoServer = errors.New("llama-server not found")
