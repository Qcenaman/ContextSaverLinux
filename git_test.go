package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBumpVersionAndFunctionsMD(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nconst appVersion = \"1.2.3\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "FUNCTIONS.md"), []byte("# X\n**Version 1.2.3**\nintro\n## What it can do\n- a\n\n## What's new\n### 1.2.3\n- old\n"), 0o644)

	file, old, err := bumpVersion(dir, "1.3.0", true)
	if err != nil || file != "main.go" || old != "1.2.3" {
		t.Fatalf("bump: %q %q %v", file, old, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(b), `appVersion = "1.3.0"`) {
		t.Fatalf("main.go not bumped: %s", b)
	}

	if _, err := updateFunctionsMD(dir, "1.3.0", []string{"- git tool", "build action"}, true); err != nil {
		t.Fatal(err)
	}
	f, _ := os.ReadFile(filepath.Join(dir, "FUNCTIONS.md"))
	s := string(f)
	if !strings.Contains(s, "**Version 1.3.0**") {
		t.Errorf("version line not updated: %s", s)
	}
	new130 := strings.Index(s, "### 1.3.0\n- git tool\n- build action\n")
	old123 := strings.Index(s, "### 1.2.3")
	if new130 < 0 || old123 < 0 || new130 > old123 {
		t.Errorf("new section not inserted above the old one: %s", s)
	}
	// Second call with the same version must not duplicate the block.
	updateFunctionsMD(dir, "1.3.0", []string{"again"}, true)
	f, _ = os.ReadFile(filepath.Join(dir, "FUNCTIONS.md"))
	if strings.Count(string(f), "### 1.3.0") != 1 {
		t.Errorf("duplicate version block: %s", f)
	}
}

func TestBuildPlanDetectsGoAndErrLines(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	steps, kinds := buildPlan(dir, "", true)
	if kinds != "go" || len(steps) != 2 || steps[0].label != "go build" || steps[1].label != "go test" {
		t.Fatalf("plan: %q %+v", kinds, steps)
	}
	if steps, _ := buildPlan(dir, "", false); len(steps) != 1 {
		t.Fatalf("no_test should drop go test: %+v", steps)
	}
	out := "ok  \tx\t0.1s\n# x\n.\\main.go:12:3: undefined: foo\n--- FAIL: TestA (0.00s)\nFAIL\tx\t0.2s\n"
	lines := buildErrLines(out, 40)
	if len(lines) != 3 || !strings.Contains(lines[0], "main.go:12:3") {
		t.Fatalf("err lines: %q", lines)
	}
}
