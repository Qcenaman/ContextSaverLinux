package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// browser: a real Chrome (or Edge) with its own ContextSaver profile, driven over the
// DevTools protocol. Pages that need JavaScript or your sign-in come back as text.

const cdpPort = 9333

var browserMu sync.Mutex

func browserExe() string {
	pf, pf86, la := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")
	for _, p := range []string{
		filepath.Join(pf, `Google\Chrome\Application\chrome.exe`), filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(la, `Google\Chrome\Application\chrome.exe`), filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`), "/usr/bin/chromium", "/usr/bin/google-chrome", os.Getenv("CONTEXTSAVER_BROWSER"),
	} {
		if exists(p) {
			return p
		}
	}
	return ""
}

type cdpTarget struct {
	ID, Type, URL, Title string
	WS                   string `json:"webSocketDebuggerUrl"`
}

func cdpTargets() ([]cdpTarget, error) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json", cdpPort))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ts []cdpTarget
	return ts, json.NewDecoder(resp.Body).Decode(&ts)
}

// ensureBrowser starts the ContextSaver browser if it isn't running (headless unless show).
func ensureBrowser(show bool) error {
	if _, err := cdpTargets(); err == nil {
		return nil
	}
	exe := browserExe()
	if exe == "" {
		return errors.New("no Chrome or Edge found")
	}
	prof := filepath.Join(appHome, "browser")
	os.MkdirAll(prof, 0o755)
	args := []string{fmt.Sprintf("--remote-debugging-port=%d", cdpPort), "--user-data-dir=" + prof, "--no-first-run",
		"--no-default-browser-check", "--remote-allow-origins=*"}
	if !show {
		args = append(args, "--headless=new", "--window-size=1280,1600")
	}
	if !isWin {
		args = append(args, "--no-sandbox")
	}
	cmd := exec.Command(exe, append(args, "about:blank")...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	for i := 0; i < 60; i++ {
		time.Sleep(200 * time.Millisecond)
		if _, err := cdpTargets(); err == nil {
			return nil
		}
	}
	return errors.New("the browser didn't start")
}

// ---------- a minimal websocket client ----------

type wsConn struct {
	c  net.Conn
	br *bufio.Reader
	id int
}

func wsDial(u string) (*wsConn, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", pu.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		pu.RequestURI(), pu.Host, base64.StdEncoding.EncodeToString(key))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		c.Close()
		return nil, errors.New("browser refused the connection")
	}
	return &wsConn{c: c, br: br}, nil
}

func (w *wsConn) send(b []byte) error {
	hdr := []byte{0x81}
	n := len(b)
	switch {
	case n < 126:
		hdr = append(hdr, byte(0x80|n))
	case n < 65536:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	out := make([]byte, n)
	for i := range b {
		out[i] = b[i] ^ mask[i%4]
	}
	_, err := w.c.Write(append(hdr, out...))
	return err
}

func (w *wsConn) recv() ([]byte, error) {
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.br, h); err != nil {
			return nil, err
		}
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			b := make([]byte, 2)
			io.ReadFull(w.br, b)
			n = int64(binary.BigEndian.Uint16(b))
		case 127:
			b := make([]byte, 8)
			io.ReadFull(w.br, b)
			n = int64(binary.BigEndian.Uint64(b))
		}
		if n > 256<<20 {
			return nil, errors.New("message too big")
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.br, p); err != nil {
			return nil, err
		}
		switch h[0] & 0x0F {
		case 0x8:
			return nil, io.EOF
		case 0x9, 0xA:
			continue
		}
		msg = append(msg, p...)
		if h[0]&0x80 != 0 {
			return msg, nil
		}
	}
}

// call sends one DevTools command and waits for its answer.
func (w *wsConn) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	w.id++
	id := w.id
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := w.send(b); err != nil {
		return nil, err
	}
	w.c.SetReadDeadline(time.Now().Add(timeout))
	for {
		m, err := w.recv()
		if err != nil {
			return nil, err
		}
		var r struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(m, &r) == nil && r.ID == id {
			if r.Error != nil {
				return nil, errors.New(r.Error.Message)
			}
			return r.Result, nil
		}
	}
}

func (w *wsConn) eval(js string, timeout time.Duration) (string, error) {
	res, err := w.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "awaitPromise": true}, timeout)
	if err != nil {
		return "", err
	}
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		Exc *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	json.Unmarshal(res, &r)
	if r.Exc != nil {
		return "", errors.New("page script failed: " + r.Exc.Text)
	}
	switch v := r.Result.Value.(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	default:
		b, _ := json.Marshal(v)
		return string(b), nil
	}
}

func pageConn() (*wsConn, error) {
	ts, err := cdpTargets()
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		if t.Type == "page" && t.WS != "" {
			return wsDial(t.WS)
		}
	}
	return nil, errors.New("the browser has no open tab")
}

// ---------- page scripts ----------

const jsFindEl = `(function(want, kinds){
  want = want.trim().toLowerCase();
  const all = [...document.querySelectorAll(kinds)];
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(e).visibility !== 'hidden'; };
  const label = e => (e.innerText || e.value || e.getAttribute('aria-label') || e.getAttribute('title') || e.getAttribute('placeholder') || e.getAttribute('name') || e.id || '').trim().replace(/\s+/g, ' ');
  const lab = e => { if (e.labels && e.labels.length) return e.labels[0].innerText.trim().toLowerCase(); return ''; };
  let best = null, score = 0;
  for (const e of all) {
    if (!vis(e)) continue;
    const t = label(e).toLowerCase(), l = lab(e);
    let s = 0;
    if (t === want || l === want) s = 3; else if (t.includes(want) || (l && l.includes(want))) s = 2; else continue;
    if (s > score) { best = e; score = s; }
  }
  return best;
})`

func jsStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func toolBrowser(raw json.RawMessage) (string, error) {
	var a struct {
		Action   string  `json:"action"`
		URL      string  `json:"url"`
		Target   string  `json:"target"`
		Value    string  `json:"value"`
		Pattern  string  `json:"pattern"`
		Show     bool    `json:"show"`
		Wait     float64 `json:"wait"`
		MaxChars int     `json:"max_chars"`
		Context  int     `json:"context"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	browserMu.Lock()
	defer browserMu.Unlock()
	maxc := a.MaxChars
	if maxc <= 0 {
		maxc = 4000
	}
	maxc = min(maxc, 30000)
	act := strings.ToLower(a.Action)
	if act == "close" {
		if ts, err := cdpTargets(); err == nil && len(ts) > 0 {
			if w, err := pageConn(); err == nil {
				w.call("Browser.close", map[string]any{}, 5*time.Second)
				w.c.Close()
			}
		}
		return "Browser closed.", nil
	}
	if a.Show {
		if _, err := cdpTargets(); err == nil {
			if w, err := pageConn(); err == nil {
				w.call("Browser.close", map[string]any{}, 5*time.Second)
				w.c.Close()
				time.Sleep(time.Second)
			}
		}
	}
	if err := ensureBrowser(a.Show); err != nil {
		return "", err
	}
	w, err := pageConn()
	if err != nil {
		return "", err
	}
	defer w.c.Close()
	settle := func() {
		for i := 0; i < 50; i++ {
			if s, _ := w.eval("document.readyState", 3*time.Second); s == "complete" {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
	}
	armErrors(w)
	if a.URL != "" {
		a.URL = fixURL(a.URL)
		if _, err := w.call("Page.navigate", map[string]any{"url": a.URL}, 30*time.Second); err != nil {
			return "", err
		}
		time.Sleep(300 * time.Millisecond)
		settle()
	}
	head := func() string {
		t, _ := w.eval("document.title + ' — ' + location.href", 5*time.Second)
		return t
	}
	switch act {
	case "console", "errors":
		return pageErrors(w)
	case "open", "", "text":
		t, err := w.eval("document.body ? document.body.innerText : ''", 15*time.Second)
		if err != nil {
			return "", err
		}
		t = strings.TrimSpace(blankRe.ReplaceAllString(t, "\n\n"))
		body, note := capText(t, maxc, "browser page")
		if act == "open" && a.URL != "" && a.Show {
			note += "\n(window shown: sign in there if needed; it stays signed in for later)"
		}
		return head() + "\n" + body + note, nil
	case "find":
		if a.Pattern == "" {
			return "", errors.New("find needs pattern")
		}
		t, err := w.eval("document.body ? document.body.innerText : ''", 15*time.Second)
		if err != nil {
			return "", err
		}
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(a.Pattern))
		if err != nil {
			return "", err
		}
		lines := strings.Split(t, "\n")
		ctx := a.Context
		var b strings.Builder
		hits := 0
		for i, l := range lines {
			if !re.MatchString(l) {
				continue
			}
			hits++
			for k := max(0, i-ctx); k <= min(len(lines)-1, i+ctx); k++ {
				fmt.Fprintf(&b, "%d: %s\n", k+1, clip(strings.TrimSpace(lines[k]), 300))
			}
			if b.Len() > maxc {
				b.WriteString("[cut]\n")
				break
			}
		}
		if hits == 0 {
			return head() + "\nNo matching lines.", nil
		}
		return head() + "\n" + strings.TrimRight(b.String(), "\n"), nil
	case "links":
		js := `JSON.stringify([...document.links].map(a => [(a.innerText||a.getAttribute('aria-label')||'').trim().replace(/\s+/g,' ').slice(0,80), a.href]).filter(x => x[1].startsWith('http')))`
		s, err := w.eval(js, 10*time.Second)
		if err != nil {
			return "", err
		}
		var ls [][2]string
		json.Unmarshal([]byte(s), &ls)
		var b strings.Builder
		for _, l := range ls {
			line := l[0] + " → " + l[1]
			if a.Pattern != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(a.Pattern)) {
				continue
			}
			b.WriteString(line + "\n")
			if b.Len() > maxc {
				b.WriteString("[cut]\n")
				break
			}
		}
		return head() + "\n" + strings.TrimRight(b.String(), "\n"), nil
	case "click":
		if a.Target == "" {
			return "", errors.New("click needs target: the text of a button or link")
		}
		js := `(function(){ const e = ` + jsFindEl + `(` + jsStr(a.Target) + `, 'a,button,input[type=submit],input[type=button],[role=button],[role=link],[role=tab],[role=menuitem],label,summary,[onclick]'); if (!e) return 'NOTFOUND'; e.scrollIntoView({block:'center'}); e.click(); return (e.innerText||e.value||e.getAttribute('aria-label')||'').trim().slice(0,60); })()`
		s, err := w.eval(js, 10*time.Second)
		if err != nil {
			return "", err
		}
		if s == "NOTFOUND" {
			return "", fmt.Errorf("nothing clickable called %q on the page", a.Target)
		}
		time.Sleep(500 * time.Millisecond)
		settle()
		return fmt.Sprintf("clicked %q; now: %s", s, head()), nil
	case "fill":
		if a.Target == "" {
			return "", errors.New("fill needs target (the field's label, placeholder or name) and value")
		}
		js := `(function(){ const e = ` + jsFindEl + `(` + jsStr(a.Target) + `, 'input,textarea,select,[contenteditable=true]'); if (!e) return 'NOTFOUND'; e.focus();
  if (e.isContentEditable) { e.innerText = ` + jsStr(a.Value) + `; } else { const p = Object.getPrototypeOf(e); const d = Object.getOwnPropertyDescriptor(p, 'value'); d && d.set ? d.set.call(e, ` + jsStr(a.Value) + `) : (e.value = ` + jsStr(a.Value) + `); }
  e.dispatchEvent(new Event('input', {bubbles:true})); e.dispatchEvent(new Event('change', {bubbles:true})); return e.tagName.toLowerCase(); })()`
		s, err := w.eval(js, 10*time.Second)
		if err != nil {
			return "", err
		}
		if s == "NOTFOUND" {
			return "", fmt.Errorf("no field called %q on the page", a.Target)
		}
		return fmt.Sprintf("filled %s %q", s, a.Target), nil
	case "press":
		key := a.Value
		if key == "" {
			key = "Enter"
		}
		for _, typ := range []string{"keyDown", "keyUp"} {
			p := map[string]any{"type": typ, "key": key}
			if key == "Enter" {
				p["code"], p["windowsVirtualKeyCode"], p["text"] = "Enter", 13, "\r"
				if typ == "keyUp" {
					delete(p, "text")
				}
			}
			w.call("Input.dispatchKeyEvent", p, 5*time.Second)
		}
		time.Sleep(500 * time.Millisecond)
		settle()
		return "pressed " + key + "; now: " + head(), nil
	case "wait":
		if a.Pattern == "" {
			return "", errors.New("wait needs pattern: text to wait for")
		}
		secs := a.Wait
		if secs <= 0 {
			secs = 20
		}
		end := time.Now().Add(time.Duration(min(secs, 50) * float64(time.Second)))
		for time.Now().Before(end) {
			t, _ := w.eval("document.body ? document.body.innerText : ''", 5*time.Second)
			if strings.Contains(strings.ToLower(t), strings.ToLower(a.Pattern)) {
				return fmt.Sprintf("%q is on the page: %s", a.Pattern, head()), nil
			}
			time.Sleep(500 * time.Millisecond)
		}
		return "", fmt.Errorf("%q didn't appear within %gs", a.Pattern, secs)
	}
	return "", errors.New("action must be open, text, find, links, click, fill, press, wait or close")
}
