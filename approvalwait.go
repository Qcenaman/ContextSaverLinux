package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// approval = "wait": a risky pc call registers its approval and blocks until the user
// decides in RemoteAI, then runs inside the same tool call. The model never has to
// retry, so a lost reply can't lose the call's arguments.

var approvalPoll = 500 * time.Millisecond

// approvalKind: "client", "wait" or "" (in-chat confirm) for the connected client.
func approvalKind() string {
	if !approvalMode() {
		return ""
	}
	if m := strings.ToLower(strings.TrimSpace(cfg.Approval)); m == "wait" || m == "dangerous" {
		return "wait" // "dangerous" waits too, but only for dangerous calls (danger.go)
	}
	return "client"
}

// approvalWaitLimit: how long a call may block (approval_wait_seconds, default the
// approval's validity) and whether hitting it falls back to the client flow
// (approval_wait_cap_seconds, for clients that time out a tool call sooner).
func approvalWaitLimit() (time.Duration, bool) {
	limit := approvalTTL
	if cfg.ApprovalWaitSeconds > 0 {
		limit = time.Duration(cfg.ApprovalWaitSeconds) * time.Second
	}
	capS := cfg.ApprovalWaitCap
	if capS == 0 && strings.Contains(strings.ToLower(clientName()), "lmstudio") {
		// LM Studio's MCP bridge times a tool call out after 60 s (no progress token,
		// and the whole chat request then fails), so stop waiting before that.
		capS = 50
	}
	if capS > 0 {
		if c := time.Duration(capS) * time.Second; c < limit {
			return c, true
		}
	}
	return limit, false
}

// waitApproval blocks on a new approval. ok=true means approved and claimed: run it.
// Otherwise msg is what the call returns.
func waitApproval(c string, a map[string]any, what string, progress func(string)) (ok bool, msg string) {
	id, err := newApprovalOpt(c, a, what, true)
	if err != nil {
		return false, "Couldn't record the approval request: " + err.Error()
	}
	spawnApprovals()
	limit, fallback := approvalWaitLimit()
	logWait(id, progress != nil)
	start := time.Now()
	lastNote := start
	for time.Since(start) < limit {
		ap, err := loadApproval(id)
		if err != nil {
			return false, "Approval " + id + " disappeared; nothing ran."
		}
		switch ap.Status {
		case "approved":
			p, _ := approvalPath(id)
			if os.Rename(p, p+".used") != nil {
				return false, "Approval " + id + " was already used: it ran once. Don't retry it."
			}
			return true, ""
		case "denied":
			return false, "The user denied " + id + ". Don't retry it."
		}
		if progress != nil && time.Since(lastNote) >= 15*time.Second {
			progress(fmt.Sprintf("waiting for the user to approve %s in RemoteAI", id))
			lastNote = time.Now()
		}
		time.Sleep(approvalPoll)
	}
	ap, err := loadApproval(id)
	if err != nil {
		return false, "Approval " + id + " expired; nothing ran."
	}
	ap.Waiting = false
	p, _ := approvalPath(id)
	if fallback && ap.Status == "pending" && time.Now().Before(ap.Expires) {
		writeJSONAtomic(p, ap)
		return false, "APPROVAL " + id + ": " + what + ". Waiting for the user to approve it in RemoteAI; tell them. Once approved, call again with the same args plus confirm:\"" + id + "\"."
	}
	ap.Status = "expired"
	writeJSONAtomic(p, ap)
	return false, "Approval " + id + " expired; nothing ran."
}

// logWait records whether the client sent a progress token (keeps long waits alive).
func logWait(id string, progress bool) {
	if appHome == "" {
		return
	}
	if f, err := os.OpenFile(filepath.Join(appHome, "mcp-clients.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s\t%s\twait %s progressToken=%v\n", time.Now().Format("2006-01-02 15:04:05"), clientName(), id, progress)
		f.Close()
	}
}
