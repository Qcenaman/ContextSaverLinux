package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Browser help for app development: local dev URLs get http://, and page errors are
// caught in the page (console.error/warn, uncaught errors, failed loads) for console.

const errCatcherJS = `(()=>{if(window.__csErr)return;const L=window.__csErr=[];const add=(k,m)=>{L.push(k+': '+String(m).slice(0,300));if(L.length>200)L.shift()};` +
	`const ce=console.error,cw=console.warn;console.error=function(...a){add('console.error',a.map(x=>x&&x.stack||x).join(' '));return ce.apply(this,a)};` +
	`console.warn=function(...a){add('console.warn',a.join(' '));return cw.apply(this,a)};` +
	`addEventListener('error',e=>{const t=e.target;if(t&&t!==window&&(t.src||t.href))add('failed to load',t.src||t.href);else add('error',(e.message||'')+(e.filename?' at '+e.filename+':'+e.lineno+':'+e.colno:''))},true);` +
	`addEventListener('unhandledrejection',e=>add('unhandled rejection',e.reason&&e.reason.stack||e.reason))})()`

// fixURL adds a scheme: http:// for this PC's dev servers, https:// otherwise.
func fixURL(u string) string {
	if strings.Contains(u, "://") || strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "file:") {
		return u
	}
	host := strings.ToLower(u)
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if p, err := url.Parse("http://" + host); err == nil {
		h := p.Hostname()
		if h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" || strings.HasPrefix(h, "192.168.") || strings.HasPrefix(h, "10.") {
			return "http://" + u
		}
	}
	return "https://" + u
}

// armErrors installs the catcher for the current page and for pages loaded while this
// connection is open.
func armErrors(w *wsConn) {
	w.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": errCatcherJS}, 5*time.Second)
	w.eval(errCatcherJS, 5*time.Second)
}

// pageErrors returns the errors caught since the last call and clears them.
func pageErrors(w *wsConn) (string, error) {
	s, err := w.eval("JSON.stringify((window.__csErr||[]).splice(0))", 5*time.Second)
	if err != nil {
		return "", err
	}
	var errs []string
	json.Unmarshal([]byte(s), &errs)
	if len(errs) == 0 {
		return "no new page errors", nil
	}
	if len(errs) > 30 {
		errs = append([]string{fmt.Sprintf("(%d older skipped)", len(errs)-30)}, errs[len(errs)-30:]...)
	}
	return fmt.Sprintf("%d page error(s):\n%s", len(errs), strings.Join(errs, "\n")), nil
}
