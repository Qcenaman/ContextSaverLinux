package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func modelHome(t *testing.T) {
	t.Helper()
	oc, oh := cfg, appHome
	t.Cleanup(func() { cfg, appHome = oc, oh })
	cfg = Config{}
	appHome = t.TempDir()
}

func writeModels(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(modelsPath(), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModelFileSeededWithBuiltIns(t *testing.T) {
	modelHome(t)
	if exists(modelsPath()) {
		t.Fatal("models.json exists before first use")
	}
	got := models()
	if !exists(modelsPath()) {
		t.Fatal("models.json not created")
	}
	if len(got) != len(defaultModels) {
		t.Fatalf("got %d models, want %d", len(got), len(defaultModels))
	}
	for n := range defaultModels { // Windows .bat scripts run as converted bash scripts
		if want := filepath.Join(genScriptDir(), n+".sh"); got[n] != want {
			t.Fatalf("%s: got %q, want %q", n, got[n], want)
		}
	}
	if ns := modelNames(); ns[0] != "qwen" || ns[len(ns)-1] != "gemma" {
		t.Fatalf("file order not kept: %v", ns)
	}
	if lmsMapped("oss-free") != "gpt-oss-120b-derestricted" {
		t.Fatal("LM Studio key lost")
	}
}

func TestModelFileSeedKeepsOldConfigModels(t *testing.T) {
	modelHome(t)
	cfg.Models = map[string]string{"mine": filepath.Join(t.TempDir(), "start-mine.bat")}
	if ns := modelNames(); len(ns) != 1 || ns[0] != "mine" {
		t.Fatalf("got %v", ns)
	}
}

func TestModelFileGGUFEntry(t *testing.T) {
	modelHome(t)
	dir := t.TempDir()
	gguf := filepath.Join(dir, "Coder Q5.gguf")
	srv := filepath.Join(dir, "llama-server")
	os.WriteFile(gguf, nil, 0o644)
	os.WriteFile(srv, nil, 0o644)
	file := func(ctx int) string {
		return fmt.Sprintf(`{"llama_server": %q, "models": [
  {"name": "qwen-coder", "gguf": %q, "description": "coder", "ctx_size": %d, "ngl": 99,
   "flash_attn": true, "cache_type": "q8_0", "temp": 0.7, "top_p": 0.8, "top_k": 20,
   "repeat_penalty": 1.05, "extra_args": ["--jinja"]}
]}`, filepath.ToSlash(srv), filepath.ToSlash(gguf), ctx)
	}
	writeModels(t, file(32768))
	s, err := resolveModel("qwen-coder")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s)
	txt := string(b)
	for _, want := range []string{`-m '` + gguf + `'`, "--ctx-size 32768", "-ngl 99", "-fa on", "-ctk q8_0 -ctv q8_0",
		"--temp 0.7", "--top-p 0.8", "--top-k 20", "--repeat-penalty 1.05", "--jinja", "--port 8080"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("script lacks %q:\n%s", want, txt)
		}
	}
	if h, p := scriptAddr(s); h != "127.0.0.1" || p != 8080 {
		t.Fatalf("address %s:%d", h, p)
	}
	if a := scriptAlias(s); a != "qwen-coder" {
		t.Fatalf("alias %q", a)
	}
	// an edit applies on the next use, no restart
	writeModels(t, file(8192))
	s, _ = resolveModel("qwen-coder")
	if b, _ := os.ReadFile(s); !strings.Contains(string(b), "--ctx-size 8192") {
		t.Fatalf("edit not applied:\n%s", b)
	}
	os.Remove(gguf)
	if _, err := resolveModel("qwen-coder"); err == nil || !strings.Contains(err.Error(), "GGUF file isn't there") {
		t.Fatalf("missing gguf: %v", err)
	}
	// a removed gguf entry takes its start script with it
	writeModels(t, `{"models": [{"name": "other", "script": "other.bat"}]}`)
	models()
	if exists(s) {
		t.Fatal("start script of a removed model left behind")
	}
}

func TestModelFileBadFile(t *testing.T) {
	modelHome(t)
	for _, c := range []struct{ file, want string }{
		{`{"models": [ {"name": "a", "script": "a.bat"}, ]}`, "line 1"},
		{`{"models": [{"name": "a", "script": "D:\llama.cpp\a.bat"}]}`, `\\`},
		{`{"models": []}`, "no models"},
		{`{"modles": []}`, `unknown setting "modles"`},
		{`{"models": {}}`, `"models" must be a list`},
		{``, "empty"},
	} {
		writeModels(t, c.file)
		if _, err := loadModels(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", c.file, err)
		}
		if _, err := resolveModel("a"); err == nil || !strings.Contains(err.Error(), "models file has a problem") {
			t.Fatalf("switch allowed with a broken file: %v", err)
		}
		if len(models()) == 0 {
			t.Fatal("no fallback list")
		}
		if _, err := lmsUse("a", false); err == nil || !strings.Contains(err.Error(), "models file has a problem") {
			t.Fatalf("LM Studio switch allowed with a broken file: %v", err)
		}
		if !strings.Contains(modelFileNotes(), "switching is refused") {
			t.Fatal("list doesn't show the file problem")
		}
	}
}

func TestModelFileBadEntries(t *testing.T) {
	modelHome(t)
	writeModels(t, `{"models": [
  {"name": "a", "script": "a.bat"},
  {"name": "b"},
  {"name": "A", "script": "a2.bat"},
  {"name": "c", "script": "c.bat", "ctx": 5},
  {"name": "d", "gguf": "d.bin"},
  {"name": "stop", "script": "s.bat"},
  {"name": "e", "script": "e.bat", "ctx_size": 4096},
  {"name": "ok", "script": "ok.bat", "description": "fine"}
]}`)
	set, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(set.order) != 1 || set.order[0] != "ok" {
		t.Fatalf("usable: %v", set.order)
	}
	all := strings.Join(set.problems, "\n")
	for _, want := range []string{`entries #1 and #3 are both named "a"`, `entry #2 (b): needs "gguf"`,
		`entry #4 (c): unknown setting "ctx"`, `entry #5 (d): "gguf" must be`, `entry #6 (stop): name "stop" is taken`,
		`entry #7 (e): llama-server settings only work with "gguf"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in:\n%s", want, all)
		}
	}
	if _, err := resolveModel("a"); err == nil || !strings.Contains(err.Error(), "both named") {
		t.Fatalf("duplicate usable: %v", err)
	}
	if _, err := resolveModel("zzz"); err == nil || !strings.Contains(err.Error(), "have: ok") {
		t.Fatalf("unknown model: %v", err)
	}
}

func TestModelShortcutsFollowFile(t *testing.T) {
	modelHome(t)
	root := t.TempDir()
	t.Setenv("CONTEXTSAVER_ROOT", root)
	t.Setenv("HOME", t.TempDir()) // desktop icons land in this test's own Desktop
	gone := filepath.Join(root, "Use model - gone.desktop")
	os.WriteFile(gone, []byte("x"), 0o644)
	writeModels(t, `{"models": [{"name": "one", "script": "one.bat"}, {"name": "two", "gguf": ""}]}`)
	out, err := writeModelShortcuts(`/opt/cs app/contextsaver`)
	if err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(root, "Use model - one.desktop")
	for _, p := range []string{one, filepath.Join(root, "Use model - two.desktop"), filepath.Join(root, "Stop model.desktop")} {
		if !exists(p) {
			t.Fatal("not made:", p)
		}
	}
	if exists(gone) || !strings.Contains(out, "removed: gone") {
		t.Fatalf("stale shortcut kept: %s", out)
	}
	if b, _ := os.ReadFile(one); !strings.Contains(string(b), `Exec=env CONTEXTSAVER_PAUSE=4 "/opt/cs app/contextsaver" model one`) {
		t.Fatalf("shortcut text: %s", b)
	}
	writeModels(t, `{bad`)
	if _, err := writeModelShortcuts("x"); err == nil || !exists(one) {
		t.Fatal("a broken file changed the shortcuts")
	}
}
