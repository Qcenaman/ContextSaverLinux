package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitApprovals(t *testing.T) string {
	clientApprovals(t)
	cfg.Approval = "wait"
	p, w, c := approvalPoll, cfg.ApprovalWaitSeconds, cfg.ApprovalWaitCap
	t.Cleanup(func() { approvalPoll, cfg.ApprovalWaitSeconds, cfg.ApprovalWaitCap = p, w, c })
	approvalPoll = 20 * time.Millisecond
	f := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(f, []byte("one\ntwo\n"), 0o644)
	return f
}

func pcEdit(t *testing.T, f string, extra map[string]any) chan string {
	a := map[string]any{"path": f, "find": "two", "replace": "TWO"}
	for k, v := range extra {
		a[k] = v
	}
	b, _ := json.Marshal(map[string]any{"c": "edit", "a": a})
	ch := make(chan string, 1)
	go func() {
		out, _, err := callCompact(b)
		if err != nil {
			out = "ERR " + err.Error()
		}
		ch <- out
	}()
	return ch
}

// pendingWaiting waits for the call to register its approval and returns it.
func pendingWaiting(t *testing.T) approval {
	for i := 0; i < 200; i++ {
		if ps := pendingApprovals(); len(ps) == 1 {
			return ps[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending approval")
	return approval{}
}

func TestWaitApprove(t *testing.T) {
	f := waitApprovals(t)
	ch := pcEdit(t, f, nil)
	ap := pendingWaiting(t)
	if !ap.Waiting {
		t.Fatal("blocked call not marked waiting")
	}
	if b, _ := os.ReadFile(f); strings.Contains(string(b), "TWO") {
		t.Fatal("ran before approval")
	}
	decideApproval(ap.ID, true)
	out := <-ch
	if !strings.Contains(out, "edited f.txt") {
		t.Fatalf("approved call output: %q", out)
	}
	if b, _ := os.ReadFile(f); !strings.Contains(string(b), "TWO") {
		t.Fatal("approved call didn't run")
	}
	// a replayed confirm after the resumed run doesn't run it again
	os.WriteFile(f, []byte("one\ntwo\n"), 0o644)
	if out := <-pcEdit(t, f, map[string]any{"confirm": ap.ID}); !strings.Contains(out, "already used") {
		t.Fatalf("replay: %q", out)
	}
	if b, _ := os.ReadFile(f); strings.Contains(string(b), "TWO") {
		t.Fatal("replay ran the action a second time")
	}
}

func TestWaitDeny(t *testing.T) {
	f := waitApprovals(t)
	ch := pcEdit(t, f, nil)
	ap := pendingWaiting(t)
	decideApproval(ap.ID, false)
	if out := <-ch; out != "The user denied "+ap.ID+". Don't retry it." {
		t.Fatalf("%q", out)
	}
	if b, _ := os.ReadFile(f); strings.Contains(string(b), "TWO") {
		t.Fatal("denied call ran")
	}
}

func TestWaitTimeout(t *testing.T) {
	f := waitApprovals(t)
	cfg.ApprovalWaitSeconds = 1
	out := <-pcEdit(t, f, nil)
	if !strings.HasPrefix(out, "Approval ") || !strings.HasSuffix(out, " expired; nothing ran.") {
		t.Fatalf("%q", out)
	}
	if len(pendingApprovals()) != 0 {
		t.Fatal("expired approval still pending")
	}
}

func TestWaitCapForLMStudio(t *testing.T) {
	old := clientName()
	t.Cleanup(func() { mcpClient.Store(old) })
	mcpClient.Store("lmstudio-mcp-server-session")
	if d, fb := approvalWaitLimit(); d != 50*time.Second || !fb {
		t.Fatalf("LM Studio wait %v fallback %v", d, fb)
	}
	mcpClient.Store("RemoteAI")
	if d, fb := approvalWaitLimit(); d != approvalTTL || fb {
		t.Fatalf("other client wait %v fallback %v", d, fb)
	}
}

func TestWaitCapFallsBackToClient(t *testing.T) {
	f := waitApprovals(t)
	cfg.ApprovalWaitCap = 1
	out := <-pcEdit(t, f, nil)
	if !strings.HasPrefix(out, "APPROVAL ") || !strings.Contains(out, "call again") {
		t.Fatalf("%q", out)
	}
	ap := pendingWaiting(t)
	if ap.Waiting {
		t.Fatal("still marked waiting after the cap")
	}
	decideApproval(ap.ID, true)
	if out := <-pcEdit(t, f, map[string]any{"confirm": ap.ID}); !strings.Contains(out, "edited f.txt") {
		t.Fatalf("client-flow retry: %q", out)
	}
}
