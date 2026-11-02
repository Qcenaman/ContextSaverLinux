package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		f := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGitCompare(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]string{
		"same.txt":       "x\ny\n",
		"changed.txt":    "one\ntwo\nthree\n",
		"onlyA.txt":      "a",
		"net_windows.go": "package x\nfunc f() {}\n",
		"build/out.bin":  "skipped",
	})
	writeTree(t, b, map[string]string{
		"same.txt":              "x\r\ny\r\n", // only line endings differ
		"changed.txt":           "one\n2\nthree\nfour\n",
		"onlyB.txt":             "b",
		"net_linux.go":          "package x\nfunc f() {}\nfunc g() {}\n",
		"node_modules/dep/i.js": "skipped",
		".git/config":           "skipped",
	})

	r, err := compareTrees(a, b, false, compareKeep(nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if r.Same != 1 || r.FilesA != 4 || r.FilesB != 4 ||
		strings.Join(r.OnlyA, " ") != "net_windows.go onlyA.txt" || strings.Join(r.OnlyB, " ") != "net_linux.go onlyB.txt" ||
		len(r.Changed) != 1 || r.Changed[0].name() != "changed.txt" || r.Changed[0].Add != 2 || r.Changed[0].Del != 1 {
		t.Fatalf("compare: %+v", r)
	}

	r, _ = compareTrees(a, b, true, compareKeep(nil, true))
	if strings.Join(r.OnlyA, " ") != "onlyA.txt" || strings.Join(r.OnlyB, " ") != "onlyB.txt" || len(r.Changed) != 2 ||
		r.Changed[1].name() != "net_windows.go <-> net_linux.go" || r.Changed[1].Add != 1 || r.Changed[1].Del != 0 {
		t.Fatalf("pair: %+v", r)
	}

	out, err := gitCompare(gitArgs{Path: a, To: b, Paths: []string{"changed.txt"}, Full: true}, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"same 0, changed 1, only in A 0, only in B 0", "-2: two\n", "+2: 2\n", "+4: four\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDiffLinesLarge(t *testing.T) {
	// a middle too big for the exact table still counts every changed line
	var a, b []string
	for i := 0; i < 2500; i++ {
		a = append(a, "a"+strings.Repeat("x", i%7)+string(rune('A'+i%26)))
		b = append(b, "b"+strings.Repeat("y", i%5)+string(rune('A'+i%26)))
	}
	add, del := 0, 0
	for _, o := range diffLines(a, b) {
		if o.kind == '+' {
			add++
		} else {
			del++
		}
	}
	if add != 2500 || del != 2500 {
		t.Fatalf("+%d -%d", add, del)
	}
}
