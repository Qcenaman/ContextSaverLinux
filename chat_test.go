package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeModel streams like llama-server: first a tool call (split over chunks), then text.
func fakeModel(t *testing.T, call string, answer string) (*httptest.Server, *[]string) {
	var seen []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) { j, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", j) }
		if n == 0 && call != "" {
			half := len(call) / 2
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "function": map[string]any{"name": "pc", "arguments": call[:half]}}}}}}})
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": call[half:]}}}}}}})
		} else {
			for _, w := range strings.SplitAfter(answer, " ") {
				send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": w}}}})
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		n++
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func chatTestUI(base, input string) *chatUI {
	u := &chatUI{in: bufio.NewReader(strings.NewReader(input)), base: base, ctx: 8192, model: "fake"}
	u.newChat()
	return u
}

func TestChatRunsToolAndAnswers(t *testing.T) {
	codeHome(t)
	os.MkdirAll(jobsDir(), 0o755)
	f := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(f, []byte("the banana budget is 42 pesos\n"), 0o644)
	srv, seen := fakeModel(t, `{"c":"read","a":{"path":"`+f+`"}}`, "It is 42 pesos.")
	u := chatTestUI(srv.URL, "")
	u.log.Messages = append(u.log.Messages, chatMsg{Role: "user", Content: "what is the budget?"})
	if err := u.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := u.log.Messages
	if ms[len(ms)-1].Content != "It is 42 pesos." {
		t.Fatalf("answer %q", ms[len(ms)-1].Content)
	}
	var tool *chatMsg
	for i := range ms {
		if ms[i].Role == "tool" {
			tool = &ms[i]
		}
	}
	if tool == nil || tool.ToolCallID != "c1" || !strings.Contains(tool.Content, "42 pesos") {
		t.Fatalf("tool result: %+v", tool)
	}
	if !strings.Contains((*seen)[0], `"name":"pc"`) || len(*seen) != 2 {
		t.Fatalf("requests: %d, first lacks the pc tool", len(*seen))
	}
	u.save()
	if cs := listChats(); len(cs) != 1 || len(cs[0].Messages) != len(ms) {
		t.Fatal("chat not saved")
	}
}

func TestChatDeniedDangerousCommand(t *testing.T) {
	codeHome(t)
	victim := filepath.Join(t.TempDir(), "keep")
	os.MkdirAll(victim, 0o755)
	srv, _ := fakeModel(t, `{"c":"run","a":{"command":"rm -rf `+victim+`"}}`, "OK, I left it.")
	u := chatTestUI(srv.URL, "n\n")
	u.log.Messages = append(u.log.Messages, chatMsg{Role: "user", Content: "delete it"})
	if err := u.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(victim) {
		t.Fatal("a denied command ran")
	}
	found := false
	for _, m := range u.log.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "said no") {
			found = true
		}
	}
	if !found {
		t.Fatal("the model wasn't told the user said no")
	}
}

func TestParsePCArgsForms(t *testing.T) {
	for _, raw := range []string{`{"c":"read","a":{"path":"/x"}}`, `{"c":"read","a":"{\"path\":\"/x\"}"}`, `{"c":"read","path":"/x"}`} {
		c, a, err := parsePCArgs(json.RawMessage(raw))
		if err != nil || c != "read" || a["path"] != "/x" {
			t.Fatalf("%s: %v %v %v", raw, c, a, err)
		}
	}
}

func TestChatFitsContext(t *testing.T) {
	u := chatTestUI("", "")
	u.ctx = 1000 // ~3000 chars
	for i := 0; i < 20; i++ {
		u.log.Messages = append(u.log.Messages, chatMsg{Role: "user", Content: strings.Repeat("q", 200)}, chatMsg{Role: "assistant", Content: strings.Repeat("a", 200)})
	}
	u.fitContext()
	ms := u.log.Messages
	if ms[0].Role != "system" || ms[1].Role != "user" || len(ms) >= 41 {
		t.Fatalf("kept %d messages, second is %s", len(ms), ms[1].Role)
	}
}
