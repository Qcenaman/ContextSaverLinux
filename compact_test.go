package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPCToolList(t *testing.T) {
	resp := handleRPC(rpcReq{Method: "tools/list", ID: json.RawMessage(`1`)})
	b, err := json.Marshal(resp["result"])
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties struct {
					C struct {
						Enum []string `json:"enum"`
					} `json:"c"`
					A struct {
						Description string `json:"description"`
					} `json:"a"`
				} `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "pc" {
		t.Fatalf("want exactly one tool named pc, got %d", len(r.Tools))
	}
	p := r.Tools[0].InputSchema.Properties
	if strings.Join(p.C.Enum, " ") != strings.Join(compactCommands, " ") {
		t.Fatalf("enum %v != compactCommands", p.C.Enum)
	}
	if !strings.Contains(p.A.Description, `"path"`) {
		t.Fatal("a has no example")
	}
	// ~4 bytes per token; keep tools/list under ~300 tokens (includes the line about the
	// user's own apps, which stops the model web-searching them)
	t.Logf("tools/list: %d bytes (~%d tokens)", len(b), len(b)/4)
	if len(b) > 1200 {
		t.Fatalf("tools/list is %d bytes (~%d tokens)", len(b), len(b)/4)
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range compactCommands {
		out, _, err := compactDispatch("help", map[string]any{"c": c})
		if err != nil || out == "" || strings.HasPrefix(out, "unknown command") || strings.HasPrefix(out, "no help") {
			t.Errorf("help for %q: %q %v", c, out, err)
		}
	}
}

func TestUnknownCListsEveryCommand(t *testing.T) {
	_, _, err := compactDispatch("nope", map[string]any{})
	if err == nil {
		t.Fatal("unknown c accepted")
	}
	msg := " " + err.Error() + " "
	for _, c := range compactCommands {
		if !strings.Contains(msg, " "+c+" ") {
			t.Errorf("unknown-c message misses %q: %s", c, msg)
		}
	}
}

func TestLMStudioNames(t *testing.T) {
	for _, n := range []string{"qwen", "gemma", "oss", "oss-free"} {
		if lmsNames[n] == "" {
			t.Errorf("no LM Studio key for %s", n)
		}
	}
	if !needsFullGPU("gemma", lmsNames["gemma"]) || needsFullGPU("oss", lmsNames["oss"]) {
		t.Fatal("VRAM check flags wrong models")
	}
}

func TestNeedsFullGPU(t *testing.T) {
	if !needsFullGPU("gemma", `D:\llama.cpp\start-gemma-4-31b.bat`) || !needsFullGPU("x", `D:\llama.cpp\start-qwen3.6-27b.bat`) {
		t.Fatal("qwen/gemma not flagged")
	}
	if needsFullGPU("oss", `D:\llama.cpp\start-gpt-oss-120b.bat`) {
		t.Fatal("gpt-oss flagged")
	}
}
