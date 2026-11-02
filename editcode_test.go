package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeEdit(t *testing.T) {
	appHome = t.TempDir()
	p := filepath.Join(t.TempDir(), "e.ts")
	var ls []string
	for i := 1; i <= 12; i++ {
		ls = append(ls, "line "+string(rune('a'+i-1))+" value")
	}
	orig := strings.Join(ls, "\r\n") + "\r\n"
	os.WriteFile(p, []byte(orig), 0o644)
	edit := func(a map[string]any) (string, error) {
		a["path"] = p
		out, _, err := compactDispatch("edit", a)
		return out, err
	}
	// missing: closest lines with numbers
	if _, err := edit(map[string]any{"find": "lin e valu", "replace": "x"}); err == nil || !strings.Contains(err.Error(), "5: line e value") {
		t.Fatalf("miss: %v", err)
	}
	// several matches: refused with the count and lines
	if _, err := edit(map[string]any{"find": "value", "replace": "x"}); err == nil || !strings.Contains(err.Error(), "matches 12 times (lines 1, 2, 3") {
		t.Fatalf("multi: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != orig {
		t.Fatal("file changed by a refused edit")
	}
	// LF find on a CRLF file; short numbered result
	out, err := edit(map[string]any{"find": "line f value\nline g value", "replace": "line f NEW\nline g NEW"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"lines 6-7", "4: line d value", "6* line f NEW", "7* line g NEW", "9: line i value"} {
		if !strings.Contains(out, w) {
			t.Errorf("summary misses %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "line l value") {
		t.Errorf("summary shows the whole file:\n%s", out)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "line f NEW\r\nline g NEW\r\n") || strings.Count(string(b), "\r\n") != 12 {
		t.Fatalf("CRLF lost: %q", b)
	}
	// all:true replaces every match
	if _, err := edit(map[string]any{"find": "value", "replace": "v", "all": true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "value") {
		t.Fatal("all:true missed matches")
	}
}
