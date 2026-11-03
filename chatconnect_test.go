package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddrInServerCommandLine(t *testing.T) {
	h, p := addrIn("/usr/local/bin/llama-server -m /mnt/d/m.gguf --alias q --host 0.0.0.0 --port 8081 --jinja")
	if h != "127.0.0.1" || p != 8081 {
		t.Fatalf("got %s:%d", h, p)
	}
	if h, p := addrIn("llama-server -m m.gguf --host 127.0.0.2"); h != "127.0.0.2" || p != 8080 {
		t.Fatalf("defaults: got %s:%d", h, p)
	}
}

// A model someone else started that is still loading: the chat waits, then uses it.
func TestChatWaitsForLoadingModel(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !ready.Load() {
				w.WriteHeader(503)
				return
			}
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/v1/models": // llama-server answers this while loading too
			fmt.Fprint(w, `{"data":[{"id":"qwen3-30b-a3b"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, ps, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(ps)
	old := servedPoll
	servedPoll = 20 * time.Millisecond
	t.Cleanup(func() { servedPoll = old })
	time.AfterFunc(150*time.Millisecond, func() { ready.Store(true) })

	var said []string
	h, p, ms := loadedAI(host, port, func(s string) { said = append(said, s) })
	if h != host || p != port || len(ms) != 1 || ms[0] != "qwen3-30b-a3b" {
		t.Fatalf("got %s:%d %v", h, p, ms)
	}
	if !ready.Load() || len(said) != 1 || !strings.Contains(said[0], "still loading") {
		t.Fatalf("didn't wait for the load: %v", said)
	}
}

func TestChatNothingLoaded(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if _, p, ms := loadedAI("127.0.0.1", port, func(string) {}); ms != nil || p != port {
		t.Fatalf("found %v on %d", ms, p)
	}
}

func TestChatDesktopIcon(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	writeDesktopShortcuts("/opt/cs/contextsaver")
	b, err := os.ReadFile(filepath.Join(h, "Desktop", "ContextSaver Chat.desktop"))
	if err != nil || !strings.Contains(string(b), "Exec=/opt/cs/contextsaver chat\n") || !strings.Contains(string(b), "Terminal=true") {
		t.Fatalf("chat icon: %v\n%s", err, b)
	}
}
