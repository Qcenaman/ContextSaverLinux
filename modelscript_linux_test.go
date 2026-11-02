package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Windows start script on the Windows partition runs as a Linux script: Windows paths
// mapped to the mount, relative ones against its cd, CUDA device names dropped.
func TestBatStartScriptConverted(t *testing.T) {
	modelHome(t)
	win := t.TempDir() // stands in for the mounted D: drive
	cfg.Drives = map[string]string{"D": win}
	srv := filepath.Join(t.TempDir(), "llama-server")
	os.WriteFile(srv, nil, 0o755)
	cfg.LlamaServer = srv
	os.MkdirAll(filepath.Join(win, "Models", "Qwen"), 0o755)
	os.MkdirAll(filepath.Join(win, "llama.cpp"), 0o755)
	os.WriteFile(filepath.Join(win, "Models", "Qwen", "q.gguf"), nil, 0o644)
	bat := "@echo off\r\nset LLAMA_ARG_THREADS=8\r\ncd /d D:\\llama.cpp\r\nllama-server.exe -m \"D:\\models\\qwen\\q.gguf\" ^\r\n  --alias qwen --device CUDA0 -c 32768 --port 8080 --host 127.0.0.1\r\nif errorlevel 1 pause\r\n"
	os.WriteFile(filepath.Join(win, "llama.cpp", "start-qwen.bat"), []byte(bat), 0o644)
	writeModels(t, `{"models": [{"name": "qwen", "script": "D:\\llama.cpp\\start-qwen.bat"}]}`)
	s, err := resolveModel("qwen")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s)
	txt := string(b)
	for _, want := range []string{"exec " + srv + " -m " + filepath.Join(win, "Models", "Qwen", "q.gguf"), "--alias qwen -c 32768 --port 8080",
		"cd " + filepath.Join(win, "llama.cpp"), "export LLAMA_ARG_THREADS=8"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("script lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "CUDA0") {
		t.Fatalf("CUDA device kept:\n%s", txt)
	}
	if h, p := scriptAddr(s); h != "127.0.0.1" || p != 8080 {
		t.Fatalf("address %s:%d", h, p)
	}
	if a := scriptAlias(s); a != "qwen" {
		t.Fatalf("alias %q", a)
	}
}

func TestWindowsPathsMapped(t *testing.T) {
	modelHome(t)
	win := t.TempDir()
	cfg.Drives = map[string]string{"D": win}
	os.MkdirAll(filepath.Join(win, "Projects", "App"), 0o755)
	got, err := expandPath(`D:\projects\app\main.go`)
	if err != nil || got != filepath.Join(win, "Projects", "App", "main.go") {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := expandPath(`Q:\nothing`); err == nil || !strings.Contains(err.Error(), "isn't mounted") {
		t.Fatalf("unmounted drive: %v", err)
	}
}

func TestBatTokens(t *testing.T) {
	got := batTokens(`"C:\a b\x.exe" -m "D:\m.gguf" --x ^"y^" a""b`)
	want := []string{`C:\a b\x.exe`, "-m", `D:\m.gguf`, "--x", `"y"`, "ab"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}
