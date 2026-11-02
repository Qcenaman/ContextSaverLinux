package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The rag guards' confirm:true goes through ContextSaver's own approval token, so a
// local model can't override them without the user.
func TestRagGuardNeedsUserToken(t *testing.T) {
	dir := t.TempDir()
	appHome = filepath.Join(dir, "home")
	oldRag := cfg.RagDir
	cfg.RagDir = filepath.Join(dir, "rag")
	t.Cleanup(func() { cfg.RagDir = oldRag })

	call := func(a map[string]any) (string, error) {
		b, _ := json.Marshal(map[string]any{"c": "rag", "a": a})
		out, _, err := callCompact(b)
		return out, err
	}
	old := filepath.Join(dir, "old")
	nw := filepath.Join(dir, "new")
	if _, err := call(map[string]any{"action": "config", "roots": []string{old}}); err != nil {
		t.Fatal(err)
	}
	// replacing every root: refused without confirm
	if _, err := call(map[string]any{"action": "config", "roots": []string{nw}}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("replace-all not refused: %v", err)
	}
	// confirm:true asks for the user's approval first
	out, err := call(map[string]any{"action": "config", "roots": []string{nw}, "confirm": true})
	if err != nil || !strings.HasPrefix(out, "CONFIRM ") {
		t.Fatalf("confirm:true ran without a token: %q %v", out, err)
	}
	tok := strings.TrimSuffix(strings.Fields(out)[1], ":")
	if got := ragSavedRoots(); len(got) != 1 || !samePath(got[0], old) {
		t.Fatalf("roots changed before approval: %v", got)
	}
	// the token alone carries the confirm:true it approved
	if _, err := call(map[string]any{"action": "config", "roots": []string{nw}, "confirm": tok}); err != nil {
		t.Fatal(err)
	}
	if got := ragSavedRoots(); len(got) != 1 || !samePath(got[0], nw) {
		t.Fatalf("roots not replaced after approval: %v", got)
	}
}
