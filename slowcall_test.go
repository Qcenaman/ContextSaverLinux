package main

import (
	"strings"
	"testing"
	"time"
)

func TestSlowCall(t *testing.T) {
	old := cfg.ToolCallLimit
	t.Cleanup(func() { cfg.ToolCallLimit = old })
	cfg.ToolCallLimit = 1
	start := time.Now().Add(-5 * time.Second) // only the 5 s minimum is left... use a fast one first
	out, _, _ := runLimited(time.Now(), func() (string, []toolImage, error) { return "fast", nil, nil })
	if out != "fast" {
		t.Fatalf("%q", out)
	}
	done := make(chan struct{})
	out, _, _ = runLimited(start, func() (string, []toolImage, error) { <-done; return "slow result", nil, nil })
	if !strings.HasPrefix(out, "still running (w") {
		t.Fatalf("%q", out)
	}
	id := out[len("still running ("):strings.Index(out, ")")]
	close(done)
	res, _, _, ok := moreSlow(id)
	if !ok || res != "slow result" {
		t.Fatalf("more: %q %v", res, ok)
	}
	if _, _, _, ok := moreSlow(id); ok {
		t.Fatal("result handed out twice")
	}
}

func TestToolCallLimitLMStudio(t *testing.T) {
	old := clientName()
	t.Cleanup(func() { mcpClient.Store(old) })
	mcpClient.Store("lmstudio-mcp-server-session")
	if toolCallLimit() != 50*time.Second {
		t.Fatal("no limit for LM Studio")
	}
	mcpClient.Store("Cline")
	if toolCallLimit() != 0 {
		t.Fatal("limit for Cline")
	}
}
