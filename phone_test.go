package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPK writes a tiny file that passes the APK check (zip magic).
func fakeAPK(t *testing.T, dir, name string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, append([]byte("PK\x03\x04"), []byte(strings.Repeat("apk-bytes ", 300))...), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func phoneTestEnv(t *testing.T) string {
	dir := t.TempDir()
	appHome = filepath.Join(dir, "home")
	old := phoneConfPath
	phoneConfPath = func() string { return filepath.Join(dir, "phone.json") }
	oldAdb := adbRun
	adbRun = func(time.Duration, ...string) (string, error) { return "", nil } // no device
	t.Cleanup(func() { phoneConfPath, adbRun = old, oldAdb })
	prepMu.Lock()
	prepared = map[string]*preparedAPK{}
	prepMu.Unlock()
	return dir
}

func TestPhoneCommandAndAliases(t *testing.T) {
	found := false
	for _, c := range compactCommands {
		found = found || c == "phone"
	}
	if !found {
		t.Fatal("phone missing from compactCommands")
	}
	if !strings.Contains(string(compactTool), `"phone"`) {
		t.Fatal("phone missing from tools/list")
	}
	for _, c := range []string{"phone", "android", "install_app", "apk", "mobile"} {
		out, _, err := compactDispatch("help", map[string]any{"c": c})
		if err != nil || !strings.HasPrefix(out, "phone{action}") {
			t.Errorf("help for alias %s: %q %v", c, out, err)
		}
		if riskyAction(c, map[string]any{"action": "uninstall", "package": "com.x.y"}) == "" {
			t.Errorf("alias %s not routed to phone", c)
		}
	}
}

func TestPhoneRisky(t *testing.T) {
	dir := phoneTestEnv(t)
	apk := fakeAPK(t, dir, "app-debug.apk")
	if w := riskyAction("phone", map[string]any{"action": "install", "path": apk, "test": true}); !strings.HasPrefix(w, "Install app-debug.apk (") || !strings.Contains(w, "on the phone") {
		t.Fatalf("install: %q", w)
	}
	if w := riskyAction("apk", map[string]any{"path": apk}); !strings.HasPrefix(w, "Install app-debug.apk") {
		t.Fatalf("install by source alone: %q", w)
	}
	if w := riskyAction("phone", map[string]any{"action": "uninstall", "package": "com.example.app"}); w != "Uninstall com.example.app from the phone" {
		t.Fatalf("uninstall: %q", w)
	}
	for _, a := range []map[string]any{{}, {"action": "status"}, {"action": "apps"}, {"action": "launch", "package": "com.example.app"}} {
		if w := riskyAction("phone", a); w != "" {
			t.Errorf("%v flagged: %q", a, w)
		}
	}
	// a bad source fails before any approval is asked
	if err := phonePreflight("phone", map[string]any{"action": "install", "path": filepath.Join(dir, "missing.apk")}); err == nil {
		t.Fatal("missing apk passed preflight")
	}
	if err := phonePreflight("phone", map[string]any{"action": "install", "path": apk, "url": "https://x/y.apk"}); err == nil {
		t.Fatal("two sources accepted")
	}
	notAPK := filepath.Join(dir, "x.apk")
	os.WriteFile(notAPK, []byte("hello"), 0o644)
	if err := phonePreflight("phone", map[string]any{"path": notAPK}); err == nil || !strings.Contains(err.Error(), "not an APK") {
		t.Fatalf("non-zip accepted: %v", err)
	}
}

func TestRepoResolverPicksAPK(t *testing.T) {
	old := ghReleases
	t.Cleanup(func() { ghReleases = old })
	ghReleases = func(repo, tag string) ([]byte, error) {
		if tag != "" {
			return []byte(`{"tag_name":"v1.2","assets":[{"name":"app-debug.apk","browser_download_url":"https://dl/v12/app-debug.apk"}]}`), nil
		}
		return []byte(`[
			{"tag_name":"v3","draft":true,"assets":[{"name":"draft.apk","browser_download_url":"https://dl/draft.apk"}]},
			{"tag_name":"v2","assets":[{"name":"notes.txt","browser_download_url":"https://dl/notes.txt"}]},
			{"tag_name":"v1.5","assets":[
				{"name":"app-debug.apk","browser_download_url":"https://dl/app-debug.apk"},
				{"name":"app-release.apk","browser_download_url":"https://dl/app-release.apk"}]}]`), nil
	}
	name, dl, _, tag, err := resolveRepoAPK("Qcenaman/AdbSaver", "")
	if err != nil || name != "app-release.apk" || dl != "https://dl/app-release.apk" || tag != "v1.5" {
		t.Fatalf("newest: %s %s %s %v", name, dl, tag, err)
	}
	if name, _, _, tag, err = resolveRepoAPK("Qcenaman/AdbSaver", "v1.2"); err != nil || name != "app-debug.apk" || tag != "v1.2" {
		t.Fatalf("tag: %s %s %v", name, tag, err)
	}
	for _, tg := range []string{"latest", "Latest", "newest"} {
		if phoneTag(map[string]any{"tag": tg}) != "" {
			t.Errorf("tag %q not treated as newest", tg)
		}
	}
	if phoneTag(map[string]any{"tag": "build-30"}) != "build-30" {
		t.Fatal("real tag dropped")
	}
	if _, _, _, ok := pickAPKAsset(ghRelease{}); ok {
		t.Fatal("empty release gave an asset")
	}
	if _, v, err := phoneSource(map[string]any{"repo": "https://github.com/Qcenaman/AdbSaver"}); err != nil || v != "Qcenaman/AdbSaver" {
		t.Fatalf("repo url form: %q %v", v, err)
	}
}

func TestServeOnce(t *testing.T) {
	dir := t.TempDir()
	apk := fakeAPK(t, dir, "a.apk")
	srv, err := serveOnce(apk, "127.0.0.1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if r, err := http.Get("http://" + u.Host + "/apk/wrong.apk"); err != nil || r.StatusCode != 404 {
		t.Fatalf("wrong path: %v %v", r, err)
	}
	r, err := http.Get(srv.URL)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("first get: %v %v", r, err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	want, _ := os.ReadFile(apk)
	if string(b) != string(want) {
		t.Fatal("served bytes differ")
	}
	if r, err := http.Get(srv.URL); err != nil || r.StatusCode != 404 {
		t.Fatalf("second get: %v %v", r, err)
	}
}

func TestInstallViaFakeEngine(t *testing.T) {
	dir := phoneTestEnv(t)
	apk := fakeAPK(t, dir, "app-release.apk")
	wantSHA, _ := fileSHA256(apk)
	fi, _ := os.Stat(apk)
	var got map[string]any
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(401)
			return
		}
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		got = req.Params.Arguments
		text := "installed com.example.app 1.2.3"
		// download like the engine does and verify
		if resp, err := http.Get(str(got, "url")); err != nil || resp.StatusCode != 200 {
			text = "download failed"
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if int64(len(b)) != fi.Size() {
				text = "size mismatch"
			}
		}
		if req.Params.Name != "apps" {
			text = "wrong tool " + req.Params.Name
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
	}))
	defer engine.Close()
	u, _ := url.Parse(engine.URL)
	host, port, _ := strings.Cut(u.Host, ":")
	b, _ := json.Marshal(map[string]any{"lan_ip": host, "port": atoiT(port), "token": "tok123"})
	os.WriteFile(phoneConfPath(), b, 0o644)
	phoneRouteMu.Lock()
	phoneRouteB = ""
	phoneRouteMu.Unlock()

	out, err := toolPhoneCmd(map[string]any{"action": "install", "path": apk, "test": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "installed com.example.app 1.2.3 (") || !strings.HasSuffix(out, ") via lan") {
		t.Fatalf("result %q", out)
	}
	if got["action"] != "install" || got["sha256"] != wantSHA || got["size"] != float64(fi.Size()) || got["test"] != true || got["replace"] != true {
		t.Fatalf("engine got %v", got)
	}
	if !strings.HasPrefix(str(got, "url"), "http://") {
		t.Fatalf("url %v", got["url"])
	}
	// the one-time URL is gone after the install
	if r, err := http.Get(str(got, "url")); err == nil && r.StatusCode == 200 {
		t.Fatal("one-time URL still serves")
	}
	if _, err := os.Stat(apk); err != nil {
		t.Fatal("the user's own apk was removed")
	}
}

func TestAdbRunNudge(t *testing.T) {
	for _, c := range []string{"adb devices", "adb install -r x.apk", `"C:\tools\adb.exe" devices`, "cd D:\\x && adb shell pm list packages"} {
		if !adbHint(c) {
			t.Errorf("no nudge for %q", c)
		}
	}
	for _, c := range []string{"dir D:\\", "go test ./...", "type adbsaver.txt", "git -C D:\\AdbSaver status"} {
		if adbHint(c) {
			t.Errorf("nudge for %q", c)
		}
	}
	if !strings.Contains(compactDesc, "phone{action:install,app}") {
		t.Fatal("pc description doesn't point at phone")
	}
}

func TestPhoneLooseArgs(t *testing.T) {
	cases := map[string]string{"install_app": "install", "Install": "install", "update": "install", "uninstall_app": "uninstall", "remove": "uninstall",
		"open_app": "launch", "launch": "launch", "list_apps": "apps", "apps": "apps", "status": "status", "battery": "status"}
	for in, want := range cases {
		if got := phoneAction(map[string]any{"action": in}); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	if phoneAction(map[string]any{"app": "AdbSaver"}) != "install" || phoneAction(map[string]any{}) != "status" {
		t.Fatal("default action")
	}
	old := ghOwner
	t.Cleanup(func() { ghOwner = old; ownerOnce = sync.Once{}; ownerName = "" })
	ownerOnce, ownerName = sync.Once{}, ""
	ghOwner = func() string { return "Qcenaman" }
	for _, a := range []map[string]any{{"action": "install_app", "name": "AdbSaver"}, {"action": "install", "app": "AdbSaver"}, {"app": "Qcenaman/AdbSaver"}} {
		if k, v, err := phoneSource(a); err != nil || k != "repo" || v != "Qcenaman/AdbSaver" {
			t.Errorf("%v -> %s %s %v", a, k, v, err)
		}
	}
	if _, _, err := phoneSource(map[string]any{"action": "install"}); err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("no source: %v", err)
	}
}

func TestInstallErrorHints(t *testing.T) {
	if e := installErr(errorString("Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: ...]")); !strings.Contains(e.Error(), "uninstalled first") {
		t.Fatal(e)
	}
	if !phoneTransportErr(errorString("download failed: timeout")) || phoneTransportErr(errorString("sha256 mismatch: x")) {
		t.Fatal("transport errors misclassified")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func atoiT(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
