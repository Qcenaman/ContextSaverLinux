package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------- web: search and read pages on this PC so page text never comes through the chat ----------

type WebRequest struct {
	Action   string  `json:"action"`
	Query    string  `json:"query,omitempty"`
	URLs     StrList `json:"urls,omitempty"`
	Question string  `json:"question,omitempty"`
	Pages    int     `json:"pages,omitempty"`
	Model    string  `json:"model,omitempty"`
	Force    bool    `json:"force,omitempty"`
	Fresh    bool    `json:"fresh,omitempty"`
}

type webLink struct {
	Text string `json:"t"`
	URL  string `json:"u"`
}

type webPage struct {
	URL     string    `json:"url"`
	Final   string    `json:"final"`
	Title   string    `json:"title"`
	Note    string    `json:"note,omitempty"`
	Links   []webLink `json:"links,omitempty"`
	Fetched time.Time `json:"fetched"`
	Cache   string    `json:"-"`
	Lines   []string  `json:"-"`
}

type webResult struct{ Title, URL, Snippet string }

const webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

var webClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 20 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	},
}

func webDir() string { return filepath.Join(appHome, "web") }

func normURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" || (pu.Scheme != "http" && pu.Scheme != "https") {
		return "", fmt.Errorf("not a web address: %s", u)
	}
	pu.Fragment = ""
	return pu.String(), nil
}

func webReq(method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	prepReq(req)
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return req, nil
}

// getPage fetches a page (or takes it from the 30-minute cache) and turns it into numbered text lines.
func getPage(raw string, fresh bool) (*webPage, error) {
	u, err := normURL(raw)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(u))
	key := hex.EncodeToString(sum[:8])
	os.MkdirAll(webDir(), 0o755)
	metaP, textP := filepath.Join(webDir(), key+".json"), filepath.Join(webDir(), key+".txt")
	if !fresh {
		if b, err := os.ReadFile(metaP); err == nil {
			var p webPage
			if json.Unmarshal(b, &p) == nil && time.Since(p.Fetched) < 30*time.Minute {
				if t, err := os.ReadFile(textP); err == nil {
					p.Cache, p.Lines = textP, strings.Split(string(t), "\n")
					return &p, nil
				}
			}
		}
	}
	req, err := webReq("GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := webClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't open %s: %v", u, cleanNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s answered %s", u, resp.Status)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	limit := int64(8 << 20)
	if strings.Contains(ct, "pdf") || strings.HasSuffix(strings.ToLower(strings.SplitN(u, "?", 2)[0]), ".pdf") {
		limit = 64 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("couldn't read %s: %v", u, cleanNetErr(err))
	}
	final := resp.Request.URL.String()
	p := &webPage{URL: u, Final: final, Fetched: time.Now()}
	head := strings.ToLower(string(b[:min(len(b), 600)]))
	switch {
	case strings.Contains(ct, "pdf") || strings.HasPrefix(head, "%pdf"):
		d, err := parsePDF(b)
		if err != nil {
			return nil, fmt.Errorf("couldn't read the PDF at %s: %v", u, err)
		}
		t := readPDFText(d, nil, 16<<20)
		for i, pg := range t.pages {
			if len(t.pages) > 1 {
				p.Lines = append(p.Lines, fmt.Sprintf("[page %d]", i+1))
			}
			p.Lines = append(p.Lines, strings.Split(pg, "\n")...)
		}
		p.Title = t.title
		if n := len(t.scanned()); n > 0 {
			p.Note = fmt.Sprintf("%d scanned page(s) have no text; download the PDF and read it to OCR them", n)
		}
	case strings.Contains(ct, "html") || strings.Contains(ct, "xhtml") ||
		(ct == "" || strings.Contains(ct, "text/plain")) && (strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")):
		base, _ := url.Parse(final)
		s := toUTF8(b)
		p.Title, p.Lines, p.Links = htmlToText(s, base)
		if n := textLen(p.Lines); n < 400 && strings.Contains(strings.ToLower(s), "<script") {
			p.Note = "very little text without JavaScript; this page may need a browser"
		}
	case strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml") || strings.Contains(ct, "javascript") || ct == "":
		if !utf8.Valid(b) && bytesLookBinary(b) {
			return nil, fmt.Errorf("%s isn't a page or text (%s)", u, ct)
		}
		p.Lines = cleanLines(strings.Split(strings.ReplaceAll(toUTF8(b), "\r\n", "\n"), "\n"), true)
	default:
		return nil, fmt.Errorf("%s isn't a page or text (%s); use run_job download for files", u, ct)
	}
	if p.Title == "" {
		p.Title = urlName(final)
	}
	if int64(len(b)) == limit {
		p.Note = strings.TrimPrefix(p.Note+fmt.Sprintf("; only the first %d MB were read", limit>>20), "; ")
	}
	os.WriteFile(textP, []byte(strings.Join(p.Lines, "\n")), 0o644)
	mb, _ := json.Marshal(p)
	os.WriteFile(metaP, mb, 0o644)
	p.Cache = textP
	return p, nil
}

func cleanNetErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && strings.Contains(s, "Get \"") {
		s = s[i+2:]
	}
	return s
}

func bytesLookBinary(b []byte) bool {
	n := min(len(b), 4000)
	z := 0
	for _, c := range b[:n] {
		if c == 0 {
			z++
		}
	}
	return z > 0
}

func toUTF8(b []byte) string {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	if utf8.Valid(b) {
		return string(b)
	}
	if s, err := decodeText(b); err == nil && utf8.ValidString(s) {
		return s
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

func textLen(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	return n
}

// ---------- HTML to readable text ----------

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	titleRe       = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
	ogTitleRe     = regexp.MustCompile(`(?is)<meta\b[^>]*property=["']og:title["'][^>]*content=["']([^"']*)["']`)
	junkRes       []*regexp.Regexp
	chromeRes     []*regexp.Regexp
	preRe         = regexp.MustCompile(`(?is)<pre\b.*?</pre\s*>`)
	wsRe          = regexp.MustCompile(`\s+`)
	headOpenRe    = regexp.MustCompile(`(?i)<h([1-6])\b(?:[^>"']|"[^"]*"|'[^']*')*>`)
	headCloseRe   = regexp.MustCompile(`(?i)</h[1-6]\s*>`)
	liRe          = regexp.MustCompile(`(?i)<li\b(?:[^>"']|"[^"]*"|'[^']*')*>`)
	brRe          = regexp.MustCompile(`(?i)<br\b(?:[^>"']|"[^"]*"|'[^']*')*>`)
	cellCloseRe   = regexp.MustCompile(`(?i)</t[dh]\s*>`)
	blockRe       = regexp.MustCompile(`(?i)</?(p|div|section|article|main|header|footer|nav|aside|ul|ol|table|thead|tbody|tr|blockquote|pre|dl|dt|dd|figure|figcaption|form|fieldset|details|summary|hr|li|h[1-6]|body|html|center|address)\b(?:[^>"']|"[^"]*"|'[^']*')*>`)
	imgAltRe      = regexp.MustCompile(`(?is)<img\b[^>]*\balt=["']([^"']{3,})["'][^>]*>`)
	anyTagRe      = regexp.MustCompile(`(?s)<(?:[^>"']|"[^"]*"|'[^']*')*>`)
	anchorRe      = regexp.MustCompile(`(?is)<a\b((?:[^>"']|"[^"]*"|'[^']*')*)>(.*?)</a\s*>`)
	hrefRe        = regexp.MustCompile(`(?is)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	spacesRe      = regexp.MustCompile(`[ \t\x{00a0}]+`)
)

func init() {
	for _, t := range []string{"script", "style", "noscript", "svg", "template", "iframe", "head", "select", "button", "canvas", "math"} {
		junkRes = append(junkRes, regexp.MustCompile(`(?is)<`+t+`\b.*?</`+t+`\s*>`))
	}
	for _, t := range []string{"nav", "footer", "aside", "header"} {
		chromeRes = append(chromeRes, regexp.MustCompile(`(?is)<`+t+`\b.*?</`+t+`\s*>`))
	}
}

// region returns the part between the first <tag ...> and the last </tag>, if there is one.
func region(s, tag string) string {
	ls := strings.ToLower(s)
	i := strings.Index(ls, "<"+tag)
	for i >= 0 && i+len(tag)+1 < len(ls) && isNameChar(ls[i+len(tag)+1]) {
		j := strings.Index(ls[i+1:], "<"+tag)
		if j < 0 {
			i = -1
			break
		}
		i += 1 + j
	}
	e := strings.LastIndex(ls, "</"+tag)
	if i < 0 || e <= i {
		return ""
	}
	return s[i:e]
}

func isNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func htmlToText(s string, base *url.URL) (string, []string, []webLink) {
	title := ""
	if m := titleRe.FindStringSubmatch(s); m != nil {
		title = strings.TrimSpace(wsRe.ReplaceAllString(html.UnescapeString(anyTagRe.ReplaceAllString(m[1], "")), " "))
	}
	if title == "" {
		if m := ogTitleRe.FindStringSubmatch(s); m != nil {
			title = strings.TrimSpace(html.UnescapeString(m[1]))
		}
	}
	s = htmlCommentRe.ReplaceAllString(s, "")
	for _, re := range junkRes {
		s = re.ReplaceAllString(s, " ")
	}
	// Prefer the main content when the page marks it and it holds real text.
	body := s
	for _, tag := range []string{"main", "article"} {
		if r := region(s, tag); r != "" {
			if t := anyTagRe.ReplaceAllString(r, " "); len(strings.Fields(t)) > 80 {
				body = r
				break
			}
		}
	}
	if body == s {
		for _, re := range chromeRes {
			stripped := re.ReplaceAllString(body, " ")
			if len(strings.Fields(anyTagRe.ReplaceAllString(stripped, " "))) > 40 {
				body = stripped
			}
		}
	}
	links := extractLinks(body, base)
	// Collapse whitespace outside <pre>; keep line breaks inside it.
	var sb strings.Builder
	last := 0
	for _, loc := range preRe.FindAllStringIndex(body, -1) {
		sb.WriteString(wsRe.ReplaceAllString(body[last:loc[0]], " "))
		pre := strings.ReplaceAll(body[loc[0]:loc[1]], "\r\n", "\n")
		sb.WriteString(strings.ReplaceAll(pre, "\n", "\x00\x01"))
		last = loc[1]
	}
	sb.WriteString(wsRe.ReplaceAllString(body[last:], " "))
	t := sb.String()
	t = headOpenRe.ReplaceAllStringFunc(t, func(m string) string {
		n := int(m[2] - '0')
		return "\n\n" + strings.Repeat("#", n) + " "
	})
	t = headCloseRe.ReplaceAllString(t, "\n")
	t = liRe.ReplaceAllString(t, "\n- ")
	t = brRe.ReplaceAllString(t, "\n")
	t = cellCloseRe.ReplaceAllString(t, " | ")
	t = imgAltRe.ReplaceAllString(t, " [image: $1] ")
	t = blockRe.ReplaceAllString(t, "\n")
	t = anyTagRe.ReplaceAllString(t, "")
	t = html.UnescapeString(t)
	t = strings.ReplaceAll(t, "\x00", "\n")
	return title, cleanLines(strings.Split(t, "\n"), false), links
}

// cleanLines trims lines, drops empty runs and repeated lines.
func cleanLines(in []string, keepIndent bool) []string {
	var out []string
	blank := false
	prev := ""
	for _, l := range in {
		if keepIndent {
			l = strings.TrimRight(l, " \t\r")
		} else if strings.HasPrefix(l, "\x01") {
			l = strings.TrimRight(strings.TrimPrefix(l, "\x01"), " \t\r")
		} else {
			l = strings.TrimSpace(spacesRe.ReplaceAllString(l, " "))
			l = strings.TrimSpace(strings.TrimSuffix(l, "|"))
			if l == "-" || l == "|" {
				l = ""
			}
		}
		if strings.TrimSpace(l) == "" {
			blank = true
			continue
		}
		l = strings.ReplaceAll(l, "\x01", "")
		if l == prev && !keepIndent {
			continue
		}
		if blank && len(out) > 0 && !keepIndent {
			out = append(out, "")
		}
		blank = false
		out = append(out, l)
		prev = l
	}
	return out
}

func extractLinks(s string, base *url.URL) []webLink {
	var out []webLink
	seen := map[string]bool{}
	for _, m := range anchorRe.FindAllStringSubmatch(s, -1) {
		h := hrefRe.FindStringSubmatch(m[1])
		if h == nil {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(h[1] + h[2] + h[3]))
		low := strings.ToLower(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "tel:") {
			continue
		}
		ru, err := url.Parse(href)
		if err != nil {
			continue
		}
		if base != nil {
			ru = base.ResolveReference(ru)
		}
		ru.Fragment = ""
		abs := ru.String()
		if seen[abs] {
			continue
		}
		seen[abs] = true
		text := strings.TrimSpace(wsRe.ReplaceAllString(html.UnescapeString(anyTagRe.ReplaceAllString(m[2], " ")), " "))
		out = append(out, webLink{Text: clip(text, 120), URL: abs})
		if len(out) >= 800 {
			break
		}
	}
	return out
}

// ---------- search ----------

var (
	ddgBlockRe  = regexp.MustCompile(`(?i)<div[^>]+class="[^"]*\bresult\b[^"]*"`)
	ddgTitleRe  = regexp.MustCompile(`(?is)<a\b([^>]*\bclass="[^"]*\bresult__a\b[^"]*"[^>]*)>(.*?)</a\s*>`)
	ddgSnipRe   = regexp.MustCompile(`(?is)<(?:a|div|td)\b[^>]*class="[^"]*\bresult__snippet\b[^"]*"[^>]*>(.*?)</(?:a|div|td)\s*>`)
	bingBlockRe = regexp.MustCompile(`(?i)<li\b[^>]*class="[^"]*\bb_algo\b`)
	bingTitleRe = regexp.MustCompile(`(?is)<h2\b[^>]*>\s*<a\b([^>]*)>(.*?)</a\s*>`)
	bingSnipRe  = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p\s*>`)
	bingCaptRe  = regexp.MustCompile(`(?is)class="[^"]*b_lineclamp[^"]*"[^>]*>(.*?)</`)
)

func inlineText(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(html.UnescapeString(anyTagRe.ReplaceAllString(s, "")), " "))
}

func splitBlocks(s string, re *regexp.Regexp) []string {
	locs := re.FindAllStringIndex(s, -1)
	var out []string
	for i, l := range locs {
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, s[l[0]:end])
	}
	return out
}

func webSearch(q string, n int) ([]webResult, string, error) {
	r1, e1 := searchDDG(q, n)
	if len(r1) > 0 {
		return r1, "DuckDuckGo", nil
	}
	r2, e2 := searchBing(q, n)
	if len(r2) > 0 {
		return r2, "Bing", nil
	}
	var why []string
	for _, e := range []error{e1, e2} {
		if e != nil {
			why = append(why, e.Error())
		}
	}
	if len(why) == 0 {
		return nil, "", nil
	}
	return nil, "", errors.New(strings.Join(why, "; "))
}

func searchDDG(q string, n int) ([]webResult, error) {
	form := url.Values{"q": {q}, "kl": {"wt-wt"}}
	req, err := webReq("POST", "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://html.duckduckgo.com/")
	resp, err := webClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DuckDuckGo: %v", cleanNetErr(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("DuckDuckGo answered %s", resp.Status)
	}
	var out []webResult
	for _, blk := range splitBlocks(string(b), ddgBlockRe) {
		if strings.Contains(blk, "result--ad") {
			continue
		}
		m := ddgTitleRe.FindStringSubmatch(blk)
		if m == nil {
			continue
		}
		h := hrefRe.FindStringSubmatch(m[1])
		if h == nil {
			continue
		}
		u := ddgTarget(html.UnescapeString(h[1] + h[2] + h[3]))
		if u == "" || strings.Contains(u, "duckduckgo.com/y.js") {
			continue
		}
		r := webResult{Title: inlineText(m[2]), URL: u}
		if sm := ddgSnipRe.FindStringSubmatch(blk); sm != nil {
			r.Snippet = inlineText(sm[1])
		}
		out = append(out, r)
		if len(out) >= n {
			break
		}
	}
	if len(out) == 0 {
		low := strings.ToLower(string(b))
		if strings.Contains(low, "anomaly") || strings.Contains(low, "captcha") {
			return nil, errors.New("DuckDuckGo asked for a captcha")
		}
	}
	return out, nil
}

func ddgTarget(h string) string {
	if strings.HasPrefix(h, "//") {
		h = "https:" + h
	}
	pu, err := url.Parse(h)
	if err != nil {
		return ""
	}
	if strings.HasSuffix(pu.Hostname(), "duckduckgo.com") && strings.HasPrefix(pu.Path, "/l/") {
		return pu.Query().Get("uddg")
	}
	return h
}

func searchBing(q string, n int) ([]webResult, error) {
	req, err := webReq("GET", "https://www.bing.com/search?"+url.Values{"q": {q}, "setlang": {"en"}, "count": {fmt.Sprint(max(n, 10))}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := webClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Bing: %v", cleanNetErr(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Bing answered %s", resp.Status)
	}
	var out []webResult
	for _, blk := range splitBlocks(string(b), bingBlockRe) {
		m := bingTitleRe.FindStringSubmatch(blk)
		if m == nil {
			continue
		}
		h := hrefRe.FindStringSubmatch(m[1])
		if h == nil {
			continue
		}
		u := bingTarget(html.UnescapeString(h[1] + h[2] + h[3]))
		if u == "" {
			continue
		}
		r := webResult{Title: inlineText(m[2]), URL: u}
		if sm := bingCaptRe.FindStringSubmatch(blk); sm != nil {
			r.Snippet = inlineText(sm[1])
		} else if sm := bingSnipRe.FindStringSubmatch(blk); sm != nil {
			r.Snippet = inlineText(sm[1])
		}
		out = append(out, r)
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

func bingTarget(h string) string {
	pu, err := url.Parse(h)
	if err != nil {
		return ""
	}
	if strings.HasSuffix(pu.Hostname(), "bing.com") && strings.HasPrefix(pu.Path, "/ck/") {
		v := pu.Query().Get("u")
		if strings.HasPrefix(v, "a1") {
			v = v[2:]
			for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
				if d, err := enc.DecodeString(v); err == nil {
					return string(d)
				}
			}
		}
		return ""
	}
	if pu.Scheme != "http" && pu.Scheme != "https" {
		return ""
	}
	return h
}

// ---------- fetching several pages ----------

type pageOrErr struct {
	page *webPage
	err  error
}

func getPages(urls []string, fresh bool) []pageOrErr {
	out := make([]pageOrErr, len(urls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := getPage(u, fresh)
			out[i] = pageOrErr{p, err}
		}(i, u)
	}
	wg.Wait()
	return out
}

// pageURLs returns the URLs given, or the top results of a search for query.
func pageURLs(req *WebRequest, def int) ([]string, []webResult, string, error) {
	if len(req.URLs) > 0 {
		return req.URLs, nil, "", nil
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, nil, "", errors.New("give urls, or query to search first")
	}
	n := req.Pages
	if n <= 0 {
		n = def
	}
	n = min(n, 10)
	res, eng, err := webSearch(req.Query, n+3)
	if err != nil {
		return nil, nil, "", fmt.Errorf("search failed: %v", err)
	}
	var urls []string
	for _, r := range res {
		urls = append(urls, r.URL)
		if len(urls) >= n {
			break
		}
	}
	if len(urls) == 0 {
		return nil, res, eng, errors.New("the search found nothing for: " + req.Query)
	}
	return urls, res, eng, nil
}

// ---------- tool ----------

func toolWeb(raw json.RawMessage) (string, error) {
	var a struct {
		WebRequest
		URL      string `json:"url"`
		Pattern  string `json:"pattern"`
		Regex    bool   `json:"regex"`
		Context  *int   `json:"context"`
		FromLine int    `json:"from_line"`
		Lines    int    `json:"lines"`
		MaxChars int    `json:"max_chars"`
		Results  int    `json:"results"`
		Notify   bool   `json:"notify"`
		Wait     *int   `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	req := a.WebRequest
	if a.URL != "" {
		req.URLs = append(StrList{a.URL}, req.URLs...)
	}
	req.Action = strings.ToLower(req.Action)
	maxc := a.MaxChars
	if maxc <= 0 {
		maxc = 6000
	}
	maxc = min(maxc, 30000)
	switch req.Action {
	case "search":
		if strings.TrimSpace(req.Query) == "" {
			return "", errors.New("search needs query")
		}
		n := a.Results
		if n <= 0 {
			n = 8
		}
		res, eng, err := webSearch(req.Query, min(n, 25))
		if err != nil {
			return "", fmt.Errorf("search failed: %v", err)
		}
		if len(res) == 0 {
			return "No results for: " + req.Query, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d results (%s):", len(res), eng)
		for i, r := range res {
			fmt.Fprintf(&b, "\n[%d] %s\n    %s", i+1, clip(r.Title, 120), r.URL)
			if r.Snippet != "" {
				b.WriteString("\n    " + clip(r.Snippet, 220))
			}
		}
		return b.String(), nil
	case "read":
		if len(req.URLs) == 0 {
			return "", errors.New("read needs url")
		}
		p, err := getPage(req.URLs[0], req.Fresh)
		if err != nil {
			return "", err
		}
		return readPageText(p, a.FromLine, a.Lines, maxc), nil
	case "find":
		if strings.TrimSpace(a.Pattern) == "" {
			return "", errors.New("find needs pattern")
		}
		match, err := lineMatcher(a.Pattern, a.Regex)
		if err != nil {
			return "", err
		}
		urls, _, eng, err := pageURLs(&req, 5)
		if err != nil {
			return "", err
		}
		ctx := 1
		if a.Context != nil {
			ctx = min(max(*a.Context, 0), 10)
		}
		var b strings.Builder
		if eng != "" {
			fmt.Fprintf(&b, "Searched %s for %q; looked in the top %d pages.\n", eng, req.Query, len(urls))
		}
		total := 0
		for i, pe := range getPages(urls, req.Fresh) {
			if pe.err != nil {
				fmt.Fprintf(&b, "[%d] %s — %v\n", i+1, urls[i], pe.err)
				continue
			}
			hits, nh := findInLines(pe.page.Lines, match, ctx, 15)
			fmt.Fprintf(&b, "[%d] %s — %s (%d lines, %d hits)", i+1, clip(pe.page.Title, 90), pe.page.Final, len(pe.page.Lines), nh)
			if pe.page.Note != "" {
				b.WriteString(" note: " + pe.page.Note)
			}
			b.WriteString("\n")
			for _, h := range hits {
				b.WriteString(h + "\n")
			}
			total += nh
			if b.Len() > maxc {
				break
			}
		}
		out := b.String()
		if len(out) > maxc {
			out = clip(out, maxc) + "\n(cut at max_chars; narrow the pattern)"
		}
		if total == 0 {
			out += "No line matched " + strconvQuote(a.Pattern) + "."
		} else {
			out += "Read more around a hit with read url=<page> from_line=<line>."
		}
		return strings.TrimSpace(out), nil
	case "links":
		if len(req.URLs) == 0 {
			return "", errors.New("links needs url")
		}
		p, err := getPage(req.URLs[0], req.Fresh)
		if err != nil {
			return "", err
		}
		pat := strings.ToLower(a.Pattern)
		var b strings.Builder
		n := 0
		for _, l := range p.Links {
			if pat != "" && !strings.Contains(strings.ToLower(l.Text), pat) && !strings.Contains(strings.ToLower(l.URL), pat) {
				continue
			}
			n++
			if b.Len() < maxc {
				fmt.Fprintf(&b, "\n%s — %s", orDash(l.Text), l.URL)
			}
		}
		head := fmt.Sprintf("%s — %s: %d of %d links", clip(p.Title, 90), p.Final, n, len(p.Links))
		if pat != "" {
			head += fmt.Sprintf(" match %q", a.Pattern)
		}
		out := head + b.String()
		if b.Len() >= maxc {
			out += "\n(cut at max_chars; give a pattern to narrow it)"
		}
		return out, nil
	case "ask":
		if strings.TrimSpace(req.Question) == "" {
			return "", errors.New("ask needs question")
		}
		if len(req.URLs) == 0 && strings.TrimSpace(req.Query) == "" {
			return "", errors.New("ask needs urls, or query to search first")
		}
		if !req.Force {
			if w := gpuWarning(); w != "" {
				return strings.Replace(w, "local_ai", "web", -1), nil
			}
		}
		j, err := newJob(JobSpec{Name: "web ask", Kind: "web", Web: &req, Source: "chat"})
		if err != nil {
			return "", err
		}
		j.start()
		waitJob(j, waitSecs(a.Wait, 40, 50))
		s := j.snapshot()
		return withNotify(formatState(&s), &s, a.Notify), nil
	}
	return "", errors.New("action must be search, find, read, links or ask")
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func lineMatcher(pat string, isRegex bool) (func(string) []int, error) {
	if isRegex {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return nil, fmt.Errorf("bad regex: %v", err)
		}
		return func(l string) []int { return re.FindStringIndex(l) }, nil
	}
	lp := strings.ToLower(pat)
	return func(l string) []int {
		i := strings.Index(strings.ToLower(l), lp)
		if i < 0 {
			return nil
		}
		return []int{i, i + len(lp)}
	}, nil
}

// findInLines returns hits as "L12> text" and nearby non-blank lines as "L11  text"; gaps show as "…".
func findInLines(lines []string, match func(string) []int, ctx, maxHits int) ([]string, int) {
	var out []string
	hits, lastShown := 0, -1
	// reach moves n non-blank lines from i in direction d.
	reach := func(i, n, d int) int {
		k := i
		for n > 0 && k+d >= 0 && k+d < len(lines) {
			k += d
			if strings.TrimSpace(lines[k]) != "" {
				n--
			}
		}
		return k
	}
	for i := range lines {
		if i <= lastShown || match(lines[i]) == nil {
			continue
		}
		if hits >= maxHits {
			out = append(out, "  (more hits not shown)")
			break
		}
		from := max(reach(i, ctx, -1), lastShown+1)
		if lastShown >= 0 && from > lastShown+1 {
			out = append(out, "  …")
		}
		end := reach(i, ctx, 1)
		for k := from; k <= end; k++ {
			if m := match(lines[k]); m != nil {
				hits++
				out = append(out, fmt.Sprintf("  L%d> %s", k+1, clipAround(lines[k], m[0], 500)))
				if k > i {
					end = max(end, reach(k, ctx, 1))
				}
			} else if strings.TrimSpace(lines[k]) != "" {
				out = append(out, fmt.Sprintf("  L%d  %s", k+1, clip(lines[k], 300)))
			}
			lastShown = k
		}
	}
	return out, hits
}

func clipAround(s string, at, n int) string {
	if len(s) <= n {
		return s
	}
	start := max(at-n/3, 0)
	end := min(start+n, len(s))
	if end == len(s) {
		start = max(end-n, 0)
	}
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}
	out := s[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(s) {
		out += "…"
	}
	return out
}

func readPageText(p *webPage, from, n, maxc int) string {
	var b strings.Builder
	total := len(p.Lines)
	if from <= 0 {
		fmt.Fprintf(&b, "%s\n%s · %d lines, %s of text · cached text: %s", p.Title, p.Final, total, hb(int64(textLen(p.Lines))), p.Cache)
		if p.Note != "" {
			b.WriteString("\nnote: " + p.Note)
		}
		var outline []string
		seenH := map[string]bool{}
		for i, l := range p.Lines {
			if strings.HasPrefix(l, "#") && !seenH[l] {
				seenH[l] = true
				outline = append(outline, fmt.Sprintf("L%d %s", i+1, clip(l, 100)))
			}
		}
		if len(outline) > 0 {
			b.WriteString("\nOutline:")
			for i, o := range outline {
				if i >= 40 {
					fmt.Fprintf(&b, "\n+%d more headings", len(outline)-40)
					break
				}
				b.WriteString("\n" + o)
			}
		}
		from = 1
		if n <= 0 {
			n = 40
		}
		b.WriteString("\n---")
	}
	if n <= 0 {
		n = 80
	}
	end := min(from-1+n, total)
	shown := from - 1
	for i := from - 1; i < end; i++ {
		if p.Lines[i] == "" {
			shown = i + 1
			continue
		}
		line := fmt.Sprintf("\nL%d %s", i+1, clip(p.Lines[i], 2000))
		if b.Len()+len(line) > maxc {
			break
		}
		b.WriteString(line)
		shown = i + 1
	}
	if shown < total {
		fmt.Fprintf(&b, "\n(more: read from_line=%d; %d lines left)", shown+1, total-shown)
	} else if total == 0 {
		b.WriteString("\n(no text on this page)")
	}
	return b.String()
}

// ---------- ask job ----------

func runWebAsk(j *Job) error {
	req := j.Spec.Web
	if req == nil {
		return errors.New("no web request")
	}
	j.setCurrent("finding pages…")
	urls, _, eng, err := pageURLs(req, 4)
	if err != nil {
		return err
	}
	j.setCurrent(fmt.Sprintf("reading %d page(s)…", len(urls)))
	var docs []aiDoc
	var notes []string
	for _, pe := range getPages(urls, req.Fresh) {
		if pe.err != nil {
			notes = append(notes, pe.err.Error())
			continue
		}
		p := pe.page
		if p.Note != "" {
			notes = append(notes, p.Final+": "+p.Note)
		}
		text := strings.Join(p.Lines, "\n")
		docs = append(docs, aiDoc{label: clip(p.Title, 80), ref: p.Final, load: func() (string, error) { return text, nil }})
	}
	if len(docs) == 0 {
		return errors.New("no page could be read (" + strings.Join(notes, "; ") + ")")
	}
	aiMu.Lock()
	defer aiMu.Unlock()
	c, stop, err := openAI(j, req.Model)
	if err != nil {
		return err
	}
	defer stop()
	j.addTotals(int64(len(docs)), 0)
	out, err := aiAskDocs(j, c, docs, req.Question, "Pages", "pages", "page")
	if err != nil {
		return err
	}
	head := fmt.Sprintf("Local AI (%s) read %d page(s).", c.model, len(docs))
	if eng != "" {
		head += fmt.Sprintf(" Found with %s: %q.", eng, req.Query)
	}
	for _, n := range notes {
		head += "\nnote: " + clip(n, 200)
	}
	j.setOutput(head + "\n" + out)
	return nil
}
