package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlan(t *testing.T) {
	appHome = t.TempDir()
	out := toolPlan(map[string]any{"set": []any{"write main.go", "add test", "push"}})
	if !strings.HasPrefix(out, "plan 0/3 done") || !strings.Contains(out, "2 [ ] add test") {
		t.Fatalf("%q", out)
	}
	out = toolPlan(map[string]any{"done": "1,2"})
	if !strings.Contains(out, "1 [x] write main.go") || !strings.Contains(out, "3 [ ] push") {
		t.Fatalf("%q", out)
	}
	if out = toolPlan(map[string]any{"done": float64(3)}); !strings.Contains(out, "(all done)") {
		t.Fatalf("%q", out)
	}
}

func TestClineProfile(t *testing.T) {
	t.Cleanup(func() { setProfile("") })
	setProfile("cline")
	var tool struct {
		Description string
		InputSchema struct {
			Properties struct {
				C struct{ Enum []string }
			}
		}
	}
	json.Unmarshal(compactTool, &tool)
	enum := " " + strings.Join(tool.InputSchema.Properties.C.Enum, " ") + " "
	for _, c := range []string{"git", "check", "rag", "office", "web", "ai", "model"} {
		if !strings.Contains(enum, " "+c+" ") {
			t.Errorf("cline profile lacks %s", c)
		}
	}
	for _, c := range []string{"read", "write", "edit", "run", "find", "browser"} {
		if strings.Contains(enum, " "+c+" ") || strings.Contains(tool.Description, c+"{") {
			t.Errorf("cline profile advertises %s, which Cline has", c)
		}
	}
	if len(compactTool) >= len(buildCompactToolFor(compactDesc, compactCommands)) {
		t.Error("cline profile is not smaller")
	}
	// commands outside the profile still work
	if out, _, err := compactDispatch("help", map[string]any{"c": "read"}); err != nil || !strings.HasPrefix(out, "read{") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestApprovalPerClient(t *testing.T) {
	old, oldC := cfg.Approval, clientName()
	t.Cleanup(func() { cfg.Approval = old; mcpClient.Store(oldC) })
	cfg.Approval = "client"
	for name, want := range map[string]bool{"": true, "LM Studio": true, "some-bridge": true, "Cline": false, "anythingllm": false, "AnythingLLM Desktop": false} {
		mcpClient.Store(name)
		if approvalMode() != want {
			t.Errorf("client %q: approvals %v, want %v", name, !want, want)
		}
	}
}
