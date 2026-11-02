package main

import (
	"encoding/json"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake embedding server: bag of words hashed into 1024 dims.
func fakeEmbed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:8091")
	if err != nil {
		t.Skip("port busy")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&req)
		type d struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var out []d
		for i, s := range req.Input {
			v := make([]float32, 1024)
			for _, w := range queryWords(strings.TrimPrefix(s, ragQueryInst)) {
				h := fnv.New32a()
				h.Write([]byte(w))
				v[h.Sum32()%ragDim]++
			}
			out = append(out, d{v, i})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": out})
	})
	go http.Serve(ln, mux)
	t.Cleanup(func() { ln.Close() })
}

func TestRag(t *testing.T) {
	fakeEmbed(t)
	dir := t.TempDir()
	appHome = filepath.Join(dir, "home")
	// never touch the real shared index (ragDir defaults to TokenSaver's on Windows)
	oldRag := cfg.RagDir
	cfg.RagDir = filepath.Join(dir, "rag")
	t.Cleanup(func() { cfg.RagDir = oldRag })
	os.MkdirAll(filepath.Join(dir, "llama"), 0o755)
	os.WriteFile(filepath.Join(dir, "llama", "llama-server"), nil, 0o755)
	os.WriteFile(filepath.Join(dir, "llama", "llama-server.exe"), nil, 0o755)
	cfg.AIScript = filepath.Join(dir, "llama", "x.bat")
	cfg.LlamaServer = filepath.Join(dir, "llama", "llama-server")
	docs := filepath.Join(dir, "docs")
	os.MkdirAll(filepath.Join(docs, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(docs, "a.md"), []byte("# Saved jobs\nSaved jobs can be re-run by double click.\n"+strings.Repeat("filler line about nothing\n", 120)+"The banana budget is 42 pesos."), 0o644)
	os.WriteFile(filepath.Join(docs, "b.txt"), []byte("Notes about ComfyUI workflows and custom nodes."), 0o644)
	os.WriteFile(filepath.Join(docs, "node_modules", "c.txt"), []byte("banana in node modules"), 0o644)
	call := func(a map[string]any) string {
		b, _ := json.Marshal(a)
		out, err := toolRag(b)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(out)
		return out
	}
	call(map[string]any{"action": "config", "roots": []string{docs}})
	call(map[string]any{"action": "update", "dry_run": true})
	out := call(map[string]any{"action": "update", "cpu": true, "wait_seconds": 30})
	if !strings.Contains(out, "done") {
		time.Sleep(3 * time.Second)
	}
	out = call(map[string]any{"action": "search", "query": "banana budget"})
	if !strings.Contains(out, "a.md") || strings.Contains(out, "node_modules") {
		t.Fatal("bad search")
	}
	call(map[string]any{"action": "remember", "text": "User prefers bananas over apples", "tags": []string{"food"}})
	out = call(map[string]any{"action": "search", "query": "which fruit bananas", "scope": "notes"})
	if !strings.Contains(out, "note") {
		t.Fatal("note not found")
	}
	os.WriteFile(filepath.Join(docs, "b.txt"), []byte("Changed: ComfyUI banana nodes."), 0o644)
	os.Chtimes(filepath.Join(docs, "b.txt"), time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	call(map[string]any{"action": "update", "cpu": true, "wait_seconds": 30})
	call(map[string]any{"action": "search", "query": "banana", "scope": "files"})
	call(map[string]any{"action": "status"})
	call(map[string]any{"action": "notes"})
}
