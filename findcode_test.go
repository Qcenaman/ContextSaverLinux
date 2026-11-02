package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGlobRe(t *testing.T) {
	re, _ := globRe("**/*.ts")
	for p, want := range map[string]bool{"a.ts": true, "src/a.ts": true, "src/x/b.TS": true, "a.tsx": false, "src/a.js": false} {
		if re.MatchString(p) != want {
			t.Errorf("**/*.ts vs %s", p)
		}
	}
	re, _ = globRe("src/*.go")
	if !re.MatchString("src/a.go") || re.MatchString("src/x/a.go") {
		t.Error("src/*.go")
	}
}

func TestFindCodeProject(t *testing.T) {
	dir := t.TempDir()
	w := func(p, s string) {
		p = filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	w("package.json", "{}")
	w("src/a.ts", "one\ntwo target\n")
	w("dist/a.js", "target")
	w("secret/s.ts", "target")
	w(".gitignore", "secret/\n")
	w("notes/build/n.ts", "target") // a plain folder called build, not next to a project file
	gitOK := false
	if _, code, err := gitExec(dir, 20*time.Second, "init", "-q"); err == nil && code == 0 {
		gitOK = true
	}
	call := func(a map[string]any) string {
		a["path"] = []string{dir}
		b, _ := json.Marshal(a)
		out, err := toolFind(b)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := call(map[string]any{"name": "**/*.ts"})
	if !strings.Contains(out, filepath.Join("src", "a.ts")) || !strings.Contains(out, filepath.Join("notes", "build", "n.ts")) {
		t.Fatalf("glob: %s", out)
	}
	out = call(map[string]any{"text": "target"})
	if strings.Contains(out, filepath.Join("dist", "a.js")) {
		t.Fatalf("build output searched: %s", out)
	}
	if gitOK && strings.Contains(out, filepath.Join("secret", "s.ts")) {
		t.Fatalf("gitignored searched: %s", out)
	}
	if !strings.Contains(out, "\n   2: two target") {
		t.Fatalf("no line number: %s", out)
	}
}
