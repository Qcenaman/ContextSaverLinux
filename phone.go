package main

// pc c="phone": the local model lists, launches, installs and uninstalls apps on the
// user's Android phone through the AdbSaver engine (port 8765, bearer token). Token and
// addresses come from TokenSaver's phone.json (TokenSaver pairs the phone). Route:
// LAN -> Tailscale -> plain adb when the engine is down.
// install: the APK is fetched and checked on the PC before the approval, so RemoteAI's
// card shows the real name and size; once approved it is served once on a random path
// (10 minutes) and the engine downloads it, verifying sha256 and size.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"archive/zip"
)

// phoneNudge follows any run of adb: models reach for adb, but the phone command works
// over Wi-Fi/Tailscale without it and handles the approval.
const phoneNudge = `Note: for the user's phone use pc{c:"phone"} instead of adb, e.g. {"c":"phone","a":{"action":"install","repo":"Qcenaman/AdbSaver"}} or {"action":"status"}; it reaches the phone over Wi-Fi/Tailscale without adb.`

var adbCmdRe = regexp.MustCompile(`(?i)(^|[\s"'\\/&|;(])adb(\.exe)?("|'|\s|$)`)

func adbHint(cmd string) bool { return adbCmdRe.MatchString(cmd) }

var phoneCommands = map[string]bool{"phone": true, "android": true, "install_app": true, "apk": true, "mobile": true}

const (
	phoneMaxAPK        = 500 << 20
	phoneServeTTL      = 10 * time.Minute
	phoneDefaultTSHost = "100.109.168.97" // the PC's Tailscale address if tailscale.exe can't say
)

type phoneConf struct {
	LanIP       string `json:"lan_ip"`
	TSIP        string `json:"tailscale_ip"`
	Port        int    `json:"port"`
	Token       string `json:"token"`
	AdbSerial   string `json:"adb_serial"`
	LastBattery string `json:"last_battery"`
}

// phoneConfPath: TokenSaver's phone.json (a var so tests can point elsewhere).
var phoneConfPath = func() string {
	if isWin {
		return `D:\Claude Apps\TokenSaver\data\phone.json`
	}
	return filepath.Join(appHome, "phone.json")
}

func loadPhoneConf() phoneConf {
	var c phoneConf
	if b, err := os.ReadFile(phoneConfPath()); err == nil {
		json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &c)
	}
	if c.Port == 0 {
		c.Port = 8765
	}
	return c
}

func phoneAPKDir() string { return filepath.Join(appHome, "phone", "apk") }

// ---------- route ----------

var (
	phoneRouteMu  sync.Mutex
	phoneRouteB   string
	phoneRouteVia string
	phoneRouteAt  time.Time
)

var phoneHTTP = &http.Client{Timeout: 2 * time.Minute}

func phoneHealthy(base string) bool {
	cl := &http.Client{Timeout: 2500 * time.Millisecond}
	resp, err := cl.Get(base + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// phoneRoute: the engine's base URL and how it was reached (lan|tailscale), or "".
func phoneRoute(c phoneConf, force bool) (string, string) {
	phoneRouteMu.Lock()
	if !force && phoneRouteB != "" && time.Since(phoneRouteAt) < time.Minute {
		b, v := phoneRouteB, phoneRouteVia
		phoneRouteMu.Unlock()
		return b, v
	}
	phoneRouteMu.Unlock()
	for _, cand := range []struct{ ip, via string }{{c.LanIP, "lan"}, {c.TSIP, "tailscale"}} {
		if cand.ip == "" {
			continue
		}
		base := "http://" + net.JoinHostPort(cand.ip, strconv.Itoa(c.Port))
		if phoneHealthy(base) {
			phoneRouteMu.Lock()
			phoneRouteB, phoneRouteVia, phoneRouteAt = base, cand.via, time.Now()
			phoneRouteMu.Unlock()
			return base, cand.via
		}
	}
	return "", ""
}

// phoneEngine sends one MCP tools/call to the engine and returns its text.
func phoneEngine(c phoneConf, base, tool string, args map[string]any, timeout time.Duration) (string, error) {
	if c.Token == "" {
		return "", errors.New("phone token unknown: pair the phone in TokenSaver first (phone pair)")
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	cl := phoneHTTP
	if timeout > 0 {
		cl = &http.Client{Timeout: timeout}
	}
	resp, err := cl.Do(req)
	if err != nil {
		phoneRouteMu.Lock()
		phoneRouteB = ""
		phoneRouteMu.Unlock()
		return "", fmt.Errorf("phone unreachable: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 {
		return "", errors.New("phone rejected the token: pair the phone again in TokenSaver")
	}
	var r struct {
		Result struct {
			Content []struct{ Type, Text string } `json:"content"`
			IsError bool                          `json:"isError"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return "", fmt.Errorf("phone: bad reply (%d): %.200s", resp.StatusCode, rb)
	}
	if r.Error != nil {
		return "", errors.New("phone: " + r.Error.Message)
	}
	var sb strings.Builder
	for _, ct := range r.Result.Content {
		if ct.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(ct.Text)
		}
	}
	if r.Result.IsError {
		return "", errors.New(strings.TrimSpace(sb.String()))
	}
	return sb.String(), nil
}

// ---------- adb ----------

func adbExe() string {
	if p, err := lookPath("adb"); err == nil {
		return p
	}
	return "adb"
}

// adbRun runs adb (a var so tests never touch a real device).
var adbRun = func(timeout time.Duration, args ...string) (string, error) {
	return runQuiet(timeout, adbExe(), args...)
}

var adbDevRe = regexp.MustCompile(`(?m)^(\S+)\s+device\b`)

func phoneAdbSerial(c phoneConf) (string, error) {
	out, _ := adbRun(8*time.Second, "devices")
	var ipSerial, anyS string
	for _, m := range adbDevRe.FindAllStringSubmatch(out, -1) {
		if anyS == "" {
			anyS = m[1]
		}
		if strings.Contains(m[1], ":") && ipSerial == "" {
			ipSerial = m[1]
		}
	}
	if ipSerial != "" {
		return ipSerial, nil
	}
	if anyS != "" {
		return anyS, nil
	}
	if c.AdbSerial != "" {
		if o, _ := adbRun(10*time.Second, "connect", c.AdbSerial); strings.Contains(o, "connected") && !strings.Contains(o, "cannot") && !strings.Contains(o, "failed") {
			return c.AdbSerial, nil
		}
	}
	return "", errors.New("no phone reachable over adb (not on the LAN, or wireless debugging off)")
}

// phoneResolvePackage: a package name as given, or an installed app found by name.
func phoneResolvePackage(c phoneConf, a map[string]any) (string, error) {
	pkg, err := phonePackage(a)
	if err == nil {
		return pkg, nil
	}
	name := strings.ToLower(phoneAppName(a))
	if name == "" {
		return "", err
	}
	list, lerr := phoneApps(c, "")
	if lerr != nil {
		return "", lerr
	}
	var hits []string
	for _, l := range strings.Split(list, "\n") {
		if !strings.Contains(strings.ToLower(l), strings.ReplaceAll(name, " ", "")) {
			continue
		}
		for _, f := range strings.FieldsFunc(l, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '(' || r == ')' || r == ',' || r == '=' || r == ':'
		}) {
			if pkgRe.MatchString(f) {
				hits = append(hits, f)
				break
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("no installed app matches %q; apps{filter} lists them", name)
	}
	return "", fmt.Errorf("%q matches several apps, give package: %s", name, strings.Join(hits, ", "))
}

var pkgRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

func phonePackage(a map[string]any) (string, error) {
	p := strings.TrimSpace(str(a, "package", "pkg", "name", "app"))
	if p == "" {
		return "", errors.New("give package, e.g. {\"action\":\"launch\",\"package\":\"com.qcenaman.adbsaver\"}")
	}
	if !pkgRe.MatchString(p) {
		return "", fmt.Errorf("%q is not a package name (like com.example.app); apps{filter} lists them", p)
	}
	return p, nil
}

// ---------- command ----------

// phoneAction: the action a phone call will do (status when nothing says otherwise).
// Models guess action names (install_app, open_app, list_apps...), so match loosely.
func phoneAction(a map[string]any) string {
	act := strings.ToLower(strings.TrimSpace(str(a, "action")))
	switch {
	case act == "":
		if str(a, "path", "file", "apk", "url", "repo", "app", "name", "app_name") != "" {
			return "install"
		}
		return "status"
	case strings.Contains(act, "uninstall") || strings.Contains(act, "remove") || strings.Contains(act, "delete"):
		return "uninstall"
	case strings.Contains(act, "install") || act == "add" || strings.Contains(act, "update"):
		return "install"
	case strings.Contains(act, "launch") || strings.Contains(act, "open") || strings.Contains(act, "start") || act == "run":
		return "launch"
	case strings.Contains(act, "list") || strings.Contains(act, "apps") || strings.Contains(act, "packages") || act == "app":
		return "apps"
	case strings.Contains(act, "status") || strings.Contains(act, "info") || strings.Contains(act, "battery"):
		return "status"
	case strings.Contains(act, "log") || strings.Contains(act, "crash"):
		return "logcat"
	}
	return act
}

// phoneAppName: an app given by name (app/name), for install from the user's own repo.
func phoneAppName(a map[string]any) string {
	return strings.TrimSpace(str(a, "app", "name", "app_name", "package"))
}

// ghOwner: the signed-in GitHub user (a var so tests can fake it).
var ghOwner = func() string {
	if githubToken() == "" {
		return ""
	}
	b, code, err := ghAPI("GET", "/user", nil)
	if err != nil || code != 200 {
		return ""
	}
	var u struct{ Login string }
	json.Unmarshal(b, &u)
	return u.Login
}

var (
	ownerOnce sync.Once
	ownerName string
)

func myGitHub() string {
	ownerOnce.Do(func() { ownerName = ghOwner() })
	return ownerName
}

func toolPhoneCmd(a map[string]any) (string, error) {
	c := loadPhoneConf()
	switch phoneAction(a) {
	case "status":
		return phoneStatus(c), nil
	case "apps":
		return phoneApps(c, strings.ToLower(str(a, "filter", "query", "package", "name")))
	case "install":
		return phoneInstall(c, a)
	case "logcat":
		return toolLogcat(a)
	case "uninstall", "launch":
		pkg, err := phoneResolvePackage(c, a)
		if err != nil {
			return "", err
		}
		return phonePkgAction(c, phoneAction(a), pkg)
	}
	return "", fmt.Errorf("unknown phone action %q: status apps install uninstall launch", str(a, "action"))
}

var batteryRe = regexp.MustCompile(`level:\s*(\d+)`)

func phoneStatus(c phoneConf) string {
	base, via := phoneRoute(c, true)
	var route, engine, battery string
	if base != "" {
		route = via + " (" + strings.TrimPrefix(base, "http://") + ")"
		engine = "reachable"
		if out, err := phoneEngine(c, base, "run", map[string]any{"command": "dumpsys battery"}, 20*time.Second); err == nil {
			if m := batteryRe.FindStringSubmatch(out); m != nil {
				battery = m[1] + "%"
			}
		}
	} else {
		engine = "unreachable (lan=" + orDash(c.LanIP) + " tailscale=" + orDash(c.TSIP) + ")"
		if s, err := phoneAdbSerial(c); err == nil {
			route = "adb " + s
			if out, _ := adbRun(15*time.Second, "-s", s, "shell", "dumpsys battery"); out != "" {
				if m := batteryRe.FindStringSubmatch(out); m != nil {
					battery = m[1] + "%"
				}
			}
		} else {
			route = "none"
		}
	}
	if battery == "" {
		battery = orDash(c.LastBattery)
		if c.LastBattery != "" {
			battery += " (last report)"
		}
	}
	if c.Token == "" {
		engine += "; token missing: pair the phone in TokenSaver"
	}
	return "route: " + route + "\nengine: " + engine + "\nbattery: " + battery
}

func phoneApps(c phoneConf, filter string) (string, error) {
	var out, note string
	if base, _ := phoneRoute(c, false); base != "" {
		o, err := phoneEngine(c, base, "apps", map[string]any{"action": "list"}, 0)
		if err != nil {
			return "", err
		}
		out = o
	} else {
		s, err := phoneAdbSerial(c)
		if err != nil {
			return "", fmt.Errorf("phone engine unreachable and %v", err)
		}
		o, _ := adbRun(20*time.Second, "-s", s, "shell", "pm list packages -3")
		out, note = strings.ReplaceAll(o, "package:", ""), "(via adb: engine unreachable)\n"
	}
	if filter != "" {
		var keep []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(strings.ToLower(l), filter) {
				keep = append(keep, strings.TrimSpace(l))
			}
		}
		if len(keep) == 0 {
			return note + "no installed app matches " + filter, nil
		}
		out = strings.Join(keep, "\n")
	}
	return note + strings.TrimSpace(out), nil
}

func phonePkgAction(c phoneConf, act, pkg string) (string, error) {
	if base, _ := phoneRoute(c, false); base != "" {
		out, err := phoneEngine(c, base, "apps", map[string]any{"action": act, "package": pkg}, 0)
		if err == nil || !phoneTransportErr(err) {
			return strings.TrimSpace(out), err
		}
	}
	s, err := phoneAdbSerial(c)
	if err != nil {
		return "", fmt.Errorf("phone engine unreachable and %v", err)
	}
	if act == "uninstall" {
		out, _ := adbRun(2*time.Minute, "-s", s, "uninstall", pkg)
		if !strings.Contains(out, "Success") {
			return "", fmt.Errorf("uninstall %s failed: %s", pkg, lastLine(out))
		}
		return "uninstalled " + pkg + " via adb", nil
	}
	out, _ := adbRun(20*time.Second, "-s", s, "shell", "monkey -p "+pkg+" -c android.intent.category.LAUNCHER 1")
	if strings.Contains(out, "No activities found") || strings.Contains(out, "Error") {
		return "", fmt.Errorf("launch %s failed: %s", pkg, lastLine(out))
	}
	return "launched " + pkg + " via adb", nil
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

// phoneTransportErr: the engine couldn't be used (not reachable, or it couldn't fetch the
// file from the PC), so adb may still work; an install error from pm is not one.
func phoneTransportErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "phone unreachable") || strings.HasPrefix(s, "download failed") || strings.Contains(s, "bad reply")
}

// ---------- APK preparation (before the approval) ----------

type preparedAPK struct {
	file, name, from string
	size             int64
	sha              string
	temp             bool
	mod              time.Time
	at               time.Time
}

var (
	prepMu   sync.Mutex
	prepared = map[string]*preparedAPK{}
)

// phoneSource: exactly one of path, url, repo.
func phoneSource(a map[string]any) (kind, val string, err error) {
	n := 0
	for _, k := range []struct {
		kind string
		keys []string
	}{{"path", []string{"path", "file", "apk"}}, {"url", []string{"url"}}, {"repo", []string{"repo"}}} {
		if v := strings.TrimSpace(str(a, k.keys...)); v != "" {
			n++
			kind, val = k.kind, v
		}
	}
	if n == 0 {
		// just an app name: the user's own GitHub repo of that name
		if name := phoneAppName(a); name != "" && !strings.ContainsAny(name, `\:`) {
			if strings.Contains(name, "/") {
				kind, val, n = "repo", name, 1
			} else if owner := myGitHub(); owner != "" {
				kind, val, n = "repo", owner+"/"+strings.ReplaceAll(name, " ", ""), 1
			}
		}
	}
	if n != 1 {
		return "", "", errors.New(`install needs exactly one of app (name of one of the user's own GitHub apps, e.g. "AdbSaver"), repo ("owner/name"), url (a direct .apk link) or path (an .apk on the PC)`)
	}
	if kind == "repo" {
		val = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(val, "https://"), "github.com/"), ".git")
		val = strings.Trim(val, "/")
		if strings.Count(val, "/") != 1 {
			return "", "", fmt.Errorf("repo must be owner/name, got %q", val)
		}
	}
	return kind, val, nil
}

// preparePhoneAPK gets the APK onto the PC and checks it; cached so the approval and the
// install that follows use the very same file.
func preparePhoneAPK(a map[string]any) (*preparedAPK, error) {
	kind, val, err := phoneSource(a)
	if err != nil {
		return nil, err
	}
	tag := phoneTag(a)
	key := kind + "\x00" + val + "\x00" + tag
	prepMu.Lock()
	defer prepMu.Unlock()
	if p := prepared[key]; p != nil && time.Since(p.at) < 30*time.Minute {
		if fi, err := os.Stat(p.file); err == nil && fi.Size() == p.size && fi.ModTime().Equal(p.mod) {
			return p, nil
		}
		delete(prepared, key)
	}
	var p *preparedAPK
	switch kind {
	case "path":
		f, err := expandPath(val)
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(strings.ToLower(f), ".apk") {
			return nil, fmt.Errorf("%s is not an .apk", f)
		}
		p = &preparedAPK{file: f, name: filepath.Base(f), from: f}
	case "url":
		u, err := url.Parse(val)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("url must be an http(s) link to an .apk: %s", val)
		}
		if p, err = downloadAPK(val, "", ""); err != nil {
			return nil, err
		}
		p.from = val
	case "repo":
		name, dl, api, rtag, err := resolveRepoAPK(val, tag)
		if err != nil {
			return nil, err
		}
		if p, err = downloadAPK(dl, api, name); err != nil {
			return nil, err
		}
		p.from = val + " " + rtag
	}
	fi, err := os.Stat(p.file)
	if err != nil {
		return nil, err
	}
	if fi.Size() > phoneMaxAPK {
		return nil, fmt.Errorf("%s is %s, over the 500 MB cap", p.name, phoneSize(fi.Size()))
	}
	if err := checkAPK(p.file); err != nil {
		if p.temp {
			os.Remove(p.file)
		}
		return nil, err
	}
	if p.sha, err = fileSHA256(p.file); err != nil {
		return nil, err
	}
	p.size, p.mod, p.at = fi.Size(), fi.ModTime(), time.Now()
	prepared[key] = p
	return p, nil
}

// phoneTag: tag:"latest"/"newest" means the newest release, not a tag of that name.
func phoneTag(a map[string]any) string {
	t := strings.TrimSpace(str(a, "tag", "version"))
	switch strings.ToLower(t) {
	case "latest", "newest", "last", "current":
		return ""
	}
	return t
}

func forgetPrepared(p *preparedAPK) {
	prepMu.Lock()
	for k, v := range prepared {
		if v == p {
			delete(prepared, k)
		}
	}
	prepMu.Unlock()
	if p.temp {
		os.Remove(p.file)
	}
}

// checkAPK: a zip (starts with PK) that holds an AndroidManifest.xml.
func checkAPK(f string) error {
	fh, err := os.Open(f)
	if err != nil {
		return err
	}
	var head [4]byte
	_, err = io.ReadFull(fh, head[:])
	fh.Close()
	if err != nil || head[0] != 'P' || head[1] != 'K' {
		return fmt.Errorf("%s is not an APK (not a zip file)", filepath.Base(f))
	}
	return nil
}

var phoneDL = &http.Client{Timeout: 15 * time.Minute}

// downloadAPK fetches an APK into data\phone\apk. apiURL (a GitHub asset API link) is used
// with the stored GitHub sign-in, so private repos work too.
func downloadAPK(dl, apiURL, name string) (*preparedAPK, error) {
	os.MkdirAll(phoneAPKDir(), 0o755)
	var req *http.Request
	if tok := githubToken(); apiURL != "" && tok != "" {
		req, _ = http.NewRequest("GET", apiURL, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/octet-stream")
	} else {
		req, _ = http.NewRequest("GET", dl, nil)
	}
	resp, err := phoneDL.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, dl)
	}
	if resp.ContentLength > phoneMaxAPK {
		return nil, fmt.Errorf("the APK is %s, over the 500 MB cap", phoneSize(resp.ContentLength))
	}
	if name == "" {
		if _, ps, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && ps["filename"] != "" {
			name = ps["filename"]
		} else if u, err := url.Parse(dl); err == nil {
			name = path.Base(u.Path)
		}
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if !strings.HasSuffix(strings.ToLower(name), ".apk") {
		return nil, fmt.Errorf("%s is not an .apk link", dl)
	}
	f := filepath.Join(phoneAPKDir(), randHex(4)+"-"+name)
	out, err := os.Create(f)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, phoneMaxAPK+1))
	out.Close()
	if err != nil {
		os.Remove(f)
		return nil, fmt.Errorf("download failed: %v", err)
	}
	if n > phoneMaxAPK {
		os.Remove(f)
		return nil, errors.New("the APK is over the 500 MB cap")
	}
	return &preparedAPK{file: f, name: name, temp: true}, nil
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name string `json:"name"`
		DL   string `json:"browser_download_url"`
		API  string `json:"url"`
	} `json:"assets"`
}

// ghReleases returns the releases JSON of repo (a list; with tag one release). A var so
// tests can fake it. Uses the stored GitHub sign-in, else the public API.
var ghReleases = func(repo, tag string) ([]byte, error) {
	p := "/repos/" + repo + "/releases?per_page=10"
	if tag != "" {
		p = "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	if githubToken() != "" {
		if b, code, err := ghAPI("GET", p, nil); err == nil && code == 200 {
			return b, nil
		}
	}
	req, _ := http.NewRequest("GET", "https://api.github.com"+p, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub releases of %s: HTTP %d", repo, resp.StatusCode)
	}
	return b, nil
}

// resolveRepoAPK: the newest release (or tag) of repo with an .apk asset.
func resolveRepoAPK(repo, tag string) (name, dl, api, rtag string, err error) {
	b, err := ghReleases(repo, tag)
	if err != nil {
		return "", "", "", "", err
	}
	var rels []ghRelease
	if tag != "" {
		var one ghRelease
		if err := json.Unmarshal(b, &one); err != nil {
			return "", "", "", "", fmt.Errorf("release %s of %s: bad reply", tag, repo)
		}
		rels = []ghRelease{one}
	} else if err := json.Unmarshal(b, &rels); err != nil {
		return "", "", "", "", fmt.Errorf("releases of %s: bad reply", repo)
	}
	for _, r := range rels {
		if r.Draft {
			continue
		}
		if n, d, ap, ok := pickAPKAsset(r); ok {
			return n, d, ap, r.TagName, nil
		}
		if tag != "" {
			break
		}
	}
	if tag != "" {
		return "", "", "", "", fmt.Errorf("release %s of %s has no .apk asset", tag, repo)
	}
	return "", "", "", "", fmt.Errorf("no recent release of %s has an .apk asset", repo)
}

// pickAPKAsset: the .apk asset, preferring one with "release" in its name.
func pickAPKAsset(r ghRelease) (name, dl, api string, ok bool) {
	first := -1
	for i, as := range r.Assets {
		low := strings.ToLower(as.Name)
		if !strings.HasSuffix(low, ".apk") {
			continue
		}
		if strings.Contains(low, "release") {
			return as.Name, as.DL, as.API, true
		}
		if first < 0 {
			first = i
		}
	}
	if first < 0 {
		return "", "", "", false
	}
	as := r.Assets[first]
	return as.Name, as.DL, as.API, true
}

func phoneSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// phonePreflight fetches and checks an install's APK before any approval is asked, so a
// bad source fails right away and the approval card can show the real file.
func phonePreflight(c string, a map[string]any) error {
	if !phoneCommands[c] || phoneAction(a) != "install" {
		return nil
	}
	_, err := preparePhoneAPK(a)
	return err
}

// phoneRisky: install and uninstall need the user's approval; status/apps/launch don't.
func phoneRisky(a map[string]any) string {
	switch phoneAction(a) {
	case "install":
		p, err := preparePhoneAPK(a)
		if err != nil {
			_, val, _ := phoneSource(a)
			return "Install an app from " + val + " on the phone"
		}
		desc := "Install " + p.name + " (" + phoneSize(p.size) + ", from " + p.from + ") on the phone"
		if b, _ := a["test"].(bool); b {
			desc += " as a test build"
		}
		return desc
	case "uninstall":
		return "Uninstall " + str(a, "package", "pkg", "name", "app") + " from the phone"
	}
	return ""
}

// ---------- one-time download URL ----------

type oneShot struct {
	URL  string
	srv  *http.Server
	once sync.Once
}

func (o *oneShot) Close() { o.once.Do(func() { o.srv.Close() }) }

// phoneServePort: 0 = a random port.
var phoneServePort = 0

// serveOnce serves file on host at a random path: one GET, then 404; closed after ttl.
func serveOnce(file, host string, ttl time.Duration) (*oneShot, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(phoneServePort)))
	if err != nil {
		return nil, fmt.Errorf("can't serve the APK on %s: %v", host, err)
	}
	p := "/apk/" + randHex(16) + ".apk"
	var used atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != p || !used.CompareAndSwap(false, true) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(file)
		if err != nil {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		}
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		io.Copy(w, f)
	})
	o := &oneShot{URL: "http://" + ln.Addr().String() + p, srv: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	go o.srv.Serve(ln)
	time.AfterFunc(ttl, o.Close)
	return o, nil
}

// ---------- install ----------

// localRouteTo: the PC's IPv4 on the interface that reaches target.
func localRouteTo(target string) string {
	conn, err := net.Dial("udp", net.JoinHostPort(target, "9"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	if ip := conn.LocalAddr().(*net.UDPAddr).IP.To4(); ip != nil {
		return ip.String()
	}
	return ""
}

func tailscalePCIP() string {
	exe := ""
	if p, err := lookPath("tailscale"); err == nil {
		exe = p
	} else if isWin && exists(`C:\Program Files\Tailscale\tailscale.exe`) {
		exe = `C:\Program Files\Tailscale\tailscale.exe`
	}
	if exe != "" {
		out, _ := runQuiet(8*time.Second, exe, "ip", "-4")
		for _, l := range strings.Fields(out) {
			if strings.HasPrefix(l, "100.") && net.ParseIP(l) != nil {
				return l
			}
		}
	}
	return phoneDefaultTSHost
}

// phonePCHost: the PC address the phone can reach over the route the engine answered on.
func phonePCHost(c phoneConf, via string) (string, error) {
	if via == "lan" {
		if ip := localRouteTo(c.LanIP); ip != "" {
			return ip, nil
		}
		return "", errors.New("no PC address on the phone's LAN")
	}
	return tailscalePCIP(), nil
}

func phoneInstall(c phoneConf, a map[string]any) (string, error) {
	p, err := preparePhoneAPK(a)
	if err != nil {
		return "", err
	}
	defer forgetPrepared(p)
	test, _ := a["test"].(bool)
	replace := true
	if b, ok := a["replace"].(bool); ok {
		replace = b
	}
	var engineErr error
	if base, via := phoneRoute(c, true); base != "" {
		out, err := phoneEngineInstall(c, base, via, p, test, replace)
		if err == nil {
			return installedLine(out, p, via), nil
		}
		if !phoneTransportErr(err) {
			return "", installErr(err)
		}
		engineErr = err
	}
	serial, aerr := phoneAdbSerial(c)
	if aerr != nil {
		if engineErr != nil {
			return "", fmt.Errorf("%v; adb fallback: %v", engineErr, aerr)
		}
		return "", fmt.Errorf("phone engine unreachable and %v", aerr)
	}
	args := []string{"-s", serial, "install"}
	if replace {
		args = append(args, "-r")
	}
	if test {
		args = append(args, "-t")
	}
	out, _ := adbRun(10*time.Minute, append(args, p.file)...)
	if !strings.Contains(out, "Success") {
		return "", installErr(errors.New(lastLine(out)))
	}
	return installedLine("", p, "adb"), nil
}

func phoneEngineInstall(c phoneConf, base, via string, p *preparedAPK, test, replace bool) (string, error) {
	host, err := phonePCHost(c, via)
	if err != nil {
		return "", err
	}
	srv, err := serveOnce(p.file, host, phoneServeTTL)
	if err != nil {
		return "", fmt.Errorf("download failed: %v", err)
	}
	defer srv.Close()
	return phoneEngine(c, base, "apps", map[string]any{"action": "install", "url": srv.URL, "sha256": p.sha, "size": p.size, "test": test, "replace": replace}, phoneServeTTL+time.Minute)
}

// installedLine: "installed <package> <versionName> (<size>) via lan|tailscale|adb".
func installedLine(engineOut string, p *preparedAPK, via string) string {
	l := strings.TrimSpace(firstLine(strings.TrimSpace(engineOut)))
	if !strings.HasPrefix(l, "installed ") {
		if pkg, ver := apkInfo(p.file); pkg != "" {
			l = strings.TrimSpace("installed " + pkg + " " + ver)
		} else {
			l = "installed " + p.name
		}
	}
	return l + " (" + phoneSize(p.size) + ") via " + via
}

func installErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "INSTALL_FAILED_UPDATE_INCOMPATIBLE"):
		msg += "\nThe installed app is signed with a different key: it has to be uninstalled first (that deletes its data). Ask the user; don't uninstall on your own."
	case strings.Contains(msg, "INSTALL_FAILED_TEST_ONLY"):
		msg += "\nThis is a test build: install again with test:true."
	case strings.Contains(msg, "INSTALL_FAILED_VERSION_DOWNGRADE"):
		msg += "\nThe phone has a newer version installed."
	case strings.Contains(strings.ToLower(msg), "url") && (strings.Contains(msg, "unknown") || strings.Contains(msg, "required")):
		msg += "\nThe AdbSaver on the phone may be too old for url installs: update AdbSaver."
	}
	return errors.New(msg)
}

// ---------- APK manifest (package, versionName) ----------

// apkInfo reads the package name and versionName from the APK's binary manifest.
func apkInfo(file string) (pkg, ver string) {
	defer func() { recover() }()
	zr, err := zip.OpenReader(file)
	if err != nil {
		return "", ""
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "AndroidManifest.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", ""
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 8<<20))
		rc.Close()
		return axmlManifest(b)
	}
	return "", ""
}

func axmlManifest(b []byte) (pkg, ver string) {
	le := binary.LittleEndian
	if len(b) < 8 || le.Uint16(b) != 0x0003 {
		return "", ""
	}
	var pool []string
	off := int(le.Uint16(b[2:]))
	for off+8 <= len(b) {
		typ, hsz, size := le.Uint16(b[off:]), int(le.Uint16(b[off+2:])), int(le.Uint32(b[off+4:]))
		if size < 8 || off+size > len(b) {
			break
		}
		ch := b[off : off+size]
		switch typ {
		case 0x0001:
			pool = axmlPool(ch)
		case 0x0102:
			name := axmlStr(pool, le.Uint32(ch[20:]))
			if name != "manifest" {
				break
			}
			astart, asz, acount := int(le.Uint16(ch[24:])), int(le.Uint16(ch[26:])), int(le.Uint16(ch[28:]))
			for i := 0; i < acount; i++ {
				at := ch[hsz+astart+i*asz:]
				an := axmlStr(pool, le.Uint32(at[4:]))
				raw := le.Uint32(at[8:])
				dt, data := at[15], le.Uint32(at[16:])
				val := ""
				if raw != 0xFFFFFFFF {
					val = axmlStr(pool, raw)
				} else if dt == 0x03 {
					val = axmlStr(pool, data)
				} else if dt >= 0x10 && dt <= 0x11 {
					val = strconv.FormatInt(int64(int32(data)), 10)
				}
				switch an {
				case "package":
					pkg = val
				case "versionName":
					ver = val
				}
			}
			return pkg, ver
		}
		off += size
	}
	return pkg, ver
}

func axmlStr(pool []string, i uint32) string {
	if int64(i) < int64(len(pool)) {
		return pool[i]
	}
	return ""
}

func axmlPool(ch []byte) []string {
	le := binary.LittleEndian
	count, flags, start := int(le.Uint32(ch[8:])), le.Uint32(ch[16:]), int(le.Uint32(ch[20:]))
	hsz := int(le.Uint16(ch[2:]))
	utf8 := flags&(1<<8) != 0
	out := make([]string, count)
	for i := 0; i < count; i++ {
		o := start + int(le.Uint32(ch[hsz+i*4:]))
		if utf8 {
			if ch[o]&0x80 != 0 {
				o++
			}
			o++
			n := int(ch[o])
			if ch[o]&0x80 != 0 {
				n = int(ch[o]&0x7f)<<8 | int(ch[o+1])
				o++
			}
			o++
			out[i] = string(ch[o : o+n])
		} else {
			n := int(le.Uint16(ch[o:]))
			if n&0x8000 != 0 {
				n = (n&0x7fff)<<16 | int(le.Uint16(ch[o+2:]))
				o += 2
			}
			o += 2
			u := make([]uint16, n)
			for j := range u {
				u[j] = le.Uint16(ch[o+j*2:])
			}
			out[i] = string(utf16.Decode(u))
		}
	}
	return out
}
