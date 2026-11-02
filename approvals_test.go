package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func clientApprovals(t *testing.T) {
	dir := t.TempDir()
	ah, ap, rd, sp := appHome, cfg.Approval, cfg.RagDir, spawnApprovals
	t.Cleanup(func() { appHome, cfg.Approval, cfg.RagDir, spawnApprovals = ah, ap, rd, sp })
	appHome = filepath.Join(dir, "home")
	cfg.RagDir = filepath.Join(dir, "rag")
	cfg.Approval = "client"
	spawnApprovals = func() {}
}

func pcCall(t *testing.T, a map[string]any) (string, error) {
	b, _ := json.Marshal(map[string]any{"c": "rag", "a": a})
	out, _, err := callCompact(b)
	return out, err
}

// The model only gets an id; it can't run the action until the user approves it.
func TestClientApprovalFlow(t *testing.T) {
	clientApprovals(t)
	old, nw := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	if _, err := pcCall(t, map[string]any{"action": "config", "roots": []string{old}}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"action": "config", "roots": []string{nw}, "confirm": true}
	out, _ := pcCall(t, args)
	if !strings.HasPrefix(out, "APPROVAL ") {
		t.Fatalf("no approval request: %q", out)
	}
	id := strings.TrimSuffix(strings.Fields(out)[1], ":")
	retry := map[string]any{"action": "config", "roots": []string{nw}, "confirm": id}
	// the model replaying the id before the user answers gets nothing done
	if out, _ := pcCall(t, retry); !strings.Contains(out, "Still waiting") {
		t.Fatalf("replay before approval: %q", out)
	}
	if got := ragSavedRoots(); !samePath(got[0], old) {
		t.Fatal("ran before approval")
	}
	if err := decideApproval(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := pcCall(t, retry); err != nil {
		t.Fatal(err)
	}
	if got := ragSavedRoots(); !samePath(got[0], nw) {
		t.Fatal("didn't run after approval")
	}
	// an approval is spent once
	withGuard := map[string]any{"action": "config", "roots": []string{nw}, "confirm": true}
	if ok, _ := useApproval(id, "rag", withGuard); ok {
		t.Fatal("approval reused")
	}
}

func TestClientApprovalDenied(t *testing.T) {
	clientApprovals(t)
	out, _ := pcCall(t, map[string]any{"action": "forget", "query": "x"})
	id := strings.TrimSuffix(strings.Fields(out)[1], ":")
	decideApproval(id, false)
	if out, _ := pcCall(t, map[string]any{"action": "forget", "query": "x", "confirm": id}); !strings.Contains(out, "denied") {
		t.Fatalf("denied approval: %q", out)
	}
}

func TestApprovalServer(t *testing.T) {
	clientApprovals(t)
	id, _ := newApproval("run", map[string]any{"command": "x"}, "run command: x")
	srv := httptest.NewServer(approvalHandler("secret"))
	defer srv.Close()
	do := func(method, path, tok string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := do("GET", "/approvals", ""); r.StatusCode != 401 {
		t.Fatal("no token accepted")
	}
	if r := do("POST", "/approvals/"+id+"/approve", "wrong"); r.StatusCode != 401 {
		t.Fatal("wrong token accepted")
	}
	r := do("GET", "/approvals", "secret")
	var l struct{ Pending []approval }
	json.NewDecoder(r.Body).Decode(&l)
	if len(l.Pending) != 1 || l.Pending[0].ID != id || l.Pending[0].Hash != "" {
		t.Fatalf("list: %+v", l)
	}
	if r := do("POST", "/approvals/"+id+"/approve", "secret"); r.StatusCode != 200 {
		t.Fatal("approve failed", r.StatusCode)
	} else {
		var s struct{ Status string }
		json.NewDecoder(r.Body).Decode(&s)
		if s.Status != "approved" {
			t.Fatalf("status %q", s.Status)
		}
	}
	id2, _ := newApproval("run", map[string]any{"command": "y"}, "run command: y")
	if r := do("POST", "/approvals/"+id2+"/deny", "secret"); r.StatusCode == 200 {
		var s struct{ Status string }
		json.NewDecoder(r.Body).Decode(&s)
		if s.Status != "denied" {
			t.Fatalf("deny status %q", s.Status)
		}
	}
	if r := do("POST", "/approvals/"+id+"/approve", "secret"); r.StatusCode != 404 {
		t.Fatal("approved twice")
	}
}

func TestLMSTokenSentOnlyToLMStudio(t *testing.T) {
	var got []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = append(got, r.Header.Get("Authorization")) })
	lms, other := httptest.NewServer(h), httptest.NewServer(h)
	defer lms.Close()
	defer other.Close()
	b, tok := lmsBase, cfg.LMSToken
	t.Cleanup(func() { lmsBase, cfg.LMSToken = b, tok })
	lmsBase, cfg.LMSToken = lms.URL, "lms-secret"
	hc := &http.Client{}
	hc.Get(lms.URL + "/api/v0/models")
	hc.Get(other.URL + "/x")
	if len(got) != 2 || got[0] != "Bearer lms-secret" || got[1] != "" {
		t.Fatalf("auth headers: %q", got)
	}
}
