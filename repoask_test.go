package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func chatCall(c, args string) chatToolCall {
	var tc chatToolCall
	tc.ID, tc.Type = "c1", "function"
	tc.Function.Name = "pc"
	tc.Function.Arguments = `{"c":"` + c + `","a":` + args + `}`
	return tc
}

// A copy that exists is updated only on the user's yes, asked once per chat.
func TestChatAsksOnceBeforeUpdatingACopy(t *testing.T) {
	hub := repoTestEnv(t)
	os.MkdirAll(jobsDir(), 0o755)
	work := newHubRepo(t, hub, "acme", "demo", map[string]string{"a.txt": "first line\n"})
	if _, err := expandRead("acme/demo/a.txt"); err != nil { // a copy from an earlier chat
		t.Fatal(err)
	}
	commitFiles(t, work, map[string]string{"a.txt": "second line\n"}, "second")
	read := chatCall("read", `{"path":"acme/demo/a.txt"}`)
	ctx := context.Background()

	u := chatTestUI("", "n\ny\n")
	if out := u.runTool(ctx, read); !strings.Contains(out, "first line") || !strings.Contains(out, "chose not to update acme/demo") {
		t.Fatalf("no:\n%s", out)
	}
	if out := u.runTool(ctx, read); !strings.Contains(out, "first line") || strings.Contains(out, "note:") {
		t.Fatalf("asked twice in one chat:\n%s", out)
	}
	u.newChat()
	if out := u.runTool(ctx, read); !strings.Contains(out, "second line") || !strings.Contains(out, "updated acme/demo") {
		t.Fatalf("yes in a new chat:\n%s", out)
	}
}

// A repo without a copy is cloned without a question, and not asked about later in that chat.
func TestChatClonesWithoutAsking(t *testing.T) {
	hub := repoTestEnv(t)
	os.MkdirAll(jobsDir(), 0o755)
	newHubRepo(t, hub, "acme", "demo", map[string]string{"a.txt": "first line\n"})
	u := chatTestUI("", "y\n")
	ctx := context.Background()
	if out := u.runTool(ctx, chatCall("read", `{"path":"acme/demo/a.txt"}`)); !strings.Contains(out, "first line") || strings.Contains(out, "note:") {
		t.Fatalf("first use:\n%s", out)
	}
	u.runTool(ctx, chatCall("find", `{"path":"acme/demo","name":"*.txt"}`))
	if rest, _ := u.in.ReadString('\n'); rest != "y\n" {
		t.Fatal("the user was asked about a copy made in this chat")
	}
}

// Over MCP there is no terminal: the first call says how old the copy is, nothing updates.
func TestMCPNotesOldCopyOnce(t *testing.T) {
	hub := repoTestEnv(t)
	os.MkdirAll(jobsDir(), 0o755)
	work := newHubRepo(t, hub, "acme", "demo", map[string]string{"a.txt": "first line\n"})
	if _, err := expandRead("acme/demo/a.txt"); err != nil {
		t.Fatal(err)
	}
	commitFiles(t, work, map[string]string{"a.txt": "second line\n"}, "second")
	repoNotedMu.Lock()
	repoNoted = map[string]bool{}
	repoNotedMu.Unlock()
	raw := json.RawMessage(`{"c":"read","a":{"path":"acme/demo/a.txt"}}`)
	out, _, err := callCompact(raw)
	if err != nil || !strings.HasPrefix(out, "note: the copy of acme/demo was last updated") || !strings.Contains(out, `git{action:pull,path:"acme/demo"}`) || !strings.Contains(out, "first line") {
		t.Fatalf("first call: %v\n%s", err, out)
	}
	if out, _, _ = callCompact(raw); strings.Contains(out, "note:") || !strings.Contains(out, "first line") {
		t.Fatalf("second call:\n%s", out)
	}
}
