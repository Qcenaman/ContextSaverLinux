package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolateRag points the index at a temp folder: on Windows ragDir() is otherwise
// TokenSaver's real, shared index.
func isolateRag(t *testing.T, dir string) {
	old := cfg.RagDir
	cfg.RagDir = filepath.Join(dir, "home", "rag")
	t.Cleanup(func() { cfg.RagDir = old })
}

func TestRagConfigBackup(t *testing.T) {
	dir := t.TempDir()
	appHome = filepath.Join(dir, "home")
	isolateRag(t, dir)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := saveRagConfig(ragConfig{Roots: []string{a}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ragDir(), "config.json.bak")); err == nil {
		t.Fatal(".bak made with no previous config")
	}
	if err := saveRagConfig(ragConfig{Roots: []string{a, b}}); err != nil {
		t.Fatal(err)
	}
	if r := ragBackupRoots(); len(r) != 1 || r[0] != a {
		t.Fatalf("config.json.bak roots = %v, want [%s]", r, a)
	}
	// an unchanged config (timestamp aside) keeps the real previous one in .bak
	if err := saveRagConfig(ragConfig{Roots: []string{a, b}, Updated: "x"}); err != nil {
		t.Fatal(err)
	}
	if r := ragBackupRoots(); len(r) != 1 {
		t.Fatalf("no-op write replaced .bak: %v", r)
	}
	for i := 0; i < ragConfigBackupsKept+5; i++ {
		if err := saveRagConfig(ragConfig{Roots: []string{a, filepath.Join(dir, fmt.Sprint(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(ragDir(), "config-backups"))
	if len(ents) != ragConfigBackupsKept {
		t.Fatalf("%d dated backups, want %d", len(ents), ragConfigBackupsKept)
	}
}

func TestRagRootsGuards(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(filepath.VolumeName(tmp)+string(filepath.Separator), "not-temp-index")
	if err := ragRootsTempCheck([]string{tmp}, real); err == nil {
		t.Fatal("temp folder accepted as a root of the real index")
	}
	if err := ragRootsTempCheck([]string{real}, real); err != nil {
		t.Fatal(err)
	}
	if err := ragRootsTempCheck([]string{tmp}, filepath.Join(tmp, "rag")); err != nil {
		t.Fatal("a test's own index in the temp folder should allow temp roots:", err)
	}
	if !underTemp(`C:\Users\x\AppData\Local\Temp\go-build1\b`) {
		t.Fatal("AppData\\Local\\Temp not seen as temp")
	}
	old := []string{filepath.Join(real, "a"), filepath.Join(real, "b")}
	if err := ragRootsReplaceCheck(old, []string{filepath.Join(real, "c")}); err == nil {
		t.Fatal("replacing every root accepted")
	}
	if err := ragRootsReplaceCheck(old, []string{old[0], filepath.Join(real, "c")}); err != nil {
		t.Fatal(err)
	}
	if !ragMassRemoval(3, 4) || ragMassRemoval(2, 4) || ragMassRemoval(0, 0) {
		t.Fatal("mass removal threshold")
	}
}

func TestRagMassRemovalRefused(t *testing.T) {
	fakeEmbed(t)
	dir := t.TempDir()
	appHome = filepath.Join(dir, "home")
	isolateRag(t, dir)
	os.MkdirAll(filepath.Join(dir, "llama"), 0o755)
	os.WriteFile(filepath.Join(dir, "llama", "llama-server"), nil, 0o755)
	os.WriteFile(filepath.Join(dir, "llama", "llama-server.exe"), nil, 0o755)
	cfg.AIScript = filepath.Join(dir, "llama", "x.bat")
	cfg.LlamaServer = filepath.Join(dir, "llama", "llama-server")
	docs, other := filepath.Join(dir, "docs"), filepath.Join(dir, "other")
	os.MkdirAll(docs, 0o755)
	os.MkdirAll(other, 0o755)
	for i := 0; i < 4; i++ {
		os.WriteFile(filepath.Join(docs, fmt.Sprintf("d%d.txt", i)), []byte(fmt.Sprintf("document %d about mangoes", i)), 0o644)
	}
	os.WriteFile(filepath.Join(other, "o.txt"), []byte("the other folder"), 0o644)
	call := func(a map[string]any) (string, error) {
		b, _ := json.Marshal(a)
		out, err := toolRag(b)
		t.Log(out, err)
		return out, err
	}
	files := func() int {
		x, err := readRagIndex()
		if err != nil {
			return -1
		}
		return len(x.meta.Files)
	}
	waitFiles := func(n int) {
		for i := 0; i < 80 && files() != n; i++ {
			time.Sleep(250 * time.Millisecond)
		}
		if f := files(); f != n {
			t.Fatalf("index has %d files, want %d", f, n)
		}
	}
	if _, err := call(map[string]any{"action": "config", "roots": []string{docs}}); err != nil {
		t.Fatal(err)
	}
	call(map[string]any{"action": "update", "cpu": true, "wait_seconds": 30})
	waitFiles(4)
	time.Sleep(time.Second) // let the job finish

	if _, err := call(map[string]any{"action": "config", "roots": []string{other}}); err == nil {
		t.Fatal("config replacing every root was accepted without confirm")
	}
	if r := ragSavedRoots(); len(r) != 1 || r[0] != docs {
		t.Fatalf("refused config still changed the roots: %v", r)
	}
	if _, err := call(map[string]any{"action": "config", "roots": []string{other}, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	out, _ := call(map[string]any{"action": "update", "dry_run": true, "wait_seconds": 30})
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "confirm:true") {
		t.Fatal("dry run did not warn about the mass removal")
	}
	out, _ = call(map[string]any{"action": "update", "cpu": true, "wait_seconds": 30})
	if !strings.Contains(out, "REFUSED") || !strings.Contains(out, docs) {
		t.Fatal("update removing every file was not refused (or did not name the changed folder)")
	}
	if f := files(); f != 4 {
		t.Fatalf("refused update changed the index: %d files", f)
	}
	call(map[string]any{"action": "update", "cpu": true, "confirm": true, "wait_seconds": 30})
	waitFiles(1)
	time.Sleep(time.Second) // let the job finish
}
