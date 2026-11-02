package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func goProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dir := filepath.Join(t.TempDir(), "hello")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/hello\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc greet(s string) string { return \"Hi \" + s }\n\nfunc main() { fmt.Println(greet(\"x\")) }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet(\"a\") != \"Hi a\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n\nfunc TestOther(t *testing.T) {}\n"), 0o644)
	return dir
}

func codeHome(t *testing.T) {
	t.Helper()
	oh := appHome
	appHome = t.TempDir()
	t.Cleanup(func() { appHome = oh })
}

func TestCompileGoAllTargets(t *testing.T) {
	codeHome(t)
	dir := goProject(t)
	for target, want := range map[string]string{"linux": "hello", "windows": "hello.exe", "android": "hello"} {
		raw, _ := json.Marshal(map[string]any{"path": dir, "target": target})
		out, err := toolCompile(raw)
		if err != nil || !strings.HasPrefix(out, "✓ go for "+target) {
			t.Fatalf("%s: %v\n%s", target, err, out)
		}
		p := filepath.Join(dir, "dist", target, want)
		if !strings.Contains(out, p) || !exists(p) {
			t.Fatalf("%s: %s not made:\n%s", target, p, out)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "dist", "windows", "hello.exe"))
	if len(b) < 2 || string(b[:2]) != "MZ" {
		t.Fatal("windows build isn't a Windows program")
	}
	// a broken build answers with only the failing lines
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { undefinedThing() }\n"), 0o644)
	raw, _ := json.Marshal(map[string]any{"path": dir})
	out, _ := toolCompile(raw)
	if !strings.HasPrefix(out, "✗") || !strings.Contains(out, "undefinedThing") {
		t.Fatalf("failure output:\n%s", out)
	}
}

func TestCompileCFile(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not installed")
	}
	codeHome(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tool.c")
	os.WriteFile(src, []byte("#include <stdio.h>\nint main(void){puts(\"ok\");return 0;}\n"), 0o644)
	raw, _ := json.Marshal(map[string]any{"path": src})
	out, err := toolCompile(raw)
	if err != nil || !exists(filepath.Join(dir, "dist", "linux", "tool")) {
		t.Fatalf("%v\n%s", err, out)
	}
	if o, _ := exec.Command(filepath.Join(dir, "dist", "linux", "tool")).Output(); strings.TrimSpace(string(o)) != "ok" {
		t.Fatalf("built program printed %q", o)
	}
}

func TestTestByName(t *testing.T) {
	dir := goProject(t)
	out, err := toolTest(map[string]any{"path": dir, "name": "TestGreet"})
	if err != nil || !strings.HasPrefix(out, "✓ go test") {
		t.Fatalf("%v %s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) { t.Fatal(\"boom here\") }\n"), 0o644)
	out, _ = toolTest(map[string]any{"path": dir})
	if !strings.HasPrefix(out, "✗") || !strings.Contains(out, "boom here") {
		t.Fatalf("failing test:\n%s", out)
	}
}

func TestOutline(t *testing.T) {
	dir := goProject(t)
	out, err := toolOutline(map[string]any{"path": dir})
	if err != nil || !strings.Contains(out, "main.go") || !strings.Contains(out, "greet") || !strings.Contains(out, "main") {
		t.Fatalf("%v\n%s", err, out)
	}
	// the fallback without ctags finds the same
	syms := regexSymbols(filepath.Join(dir, "main.go"))
	names := ""
	for _, s := range syms {
		names += s.name + " "
	}
	if !strings.Contains(names, "greet") || !strings.Contains(names, "main") {
		t.Fatalf("fallback found %q", names)
	}
}

func TestExportFilesAndZip(t *testing.T) {
	codeHome(t)
	os.MkdirAll(jobsDir(), 0o755)
	dir := filepath.Join(t.TempDir(), "proj")
	out, _, err := toolExport(map[string]any{"path": dir, "files": map[string]any{"src/a.go": "package a\n", "README.txt": "hi"}})
	if err != nil || !exists(filepath.Join(dir, "src", "a.go")) || !exists(filepath.Join(dir, "README.txt")) {
		t.Fatalf("%v %s", err, out)
	}
	if _, _, err := toolExport(map[string]any{"path": dir, "files": map[string]any{"../escape.txt": "x"}}); err == nil {
		t.Fatal("a file outside path was allowed")
	}
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "big.js"), []byte("x"), 0o644)
	out, _, err = toolExport(map[string]any{"path": dir, "zip": true})
	if err != nil || !strings.Contains(out, "2 files") {
		t.Fatalf("%v %s", err, out)
	}
	zr, err := zip.OpenReader(dir + ".zip")
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.Contains(f.Name, "node_modules") {
			t.Fatal("zip has node_modules")
		}
	}
}

func TestDepsListGo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n\nrequire (\n\tgithub.com/a/b v1.2.3\n\tgolang.org/x/sys v0.1.0 // indirect\n)\n"), 0o644)
	out, err := toolDeps(map[string]any{"path": dir})
	if err != nil || out != "github.com/a/b v1.2.3" {
		t.Fatalf("%v %q", err, out)
	}
	if d := lineDiff("a\nb\n", "a\nc\n"); !strings.Contains(d, "- b") || !strings.Contains(d, "+ c") {
		t.Fatalf("diff %q", d)
	}
}

func TestScaffoldGoPassesItsTests(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	files, err := scaffoldFiles("go", "demo-app", "someone")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for n, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, n)), 0o755)
		os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
	}
	if !strings.Contains(files[".github/workflows/test.yml"], "go test ./...") {
		t.Fatal("no test workflow")
	}
	if out, err := toolTest(map[string]any{"path": dir}); err != nil || !strings.HasPrefix(out, "✓") {
		t.Fatalf("scaffolded tests: %v %s", err, out)
	}
	for _, k := range []string{"android", "web", "python", "rust", "c"} {
		fs, err := scaffoldFiles(k, "x", "me")
		if err != nil || fs[".github/workflows/test.yml"] == "" {
			t.Fatalf("%s: %v", k, err)
		}
	}
}
