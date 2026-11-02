package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The window tool: read what's on screen as text instead of screenshots.
// list: open windows; text: a window's text from UI Automation (the accessibility
// tree, read-only); ocr: Windows OCR of a window or area, text only; copy: Ctrl+A,
// Ctrl+C in the window, clipboard put back afterwards.

type winInfo struct {
	hwnd       uintptr
	title, exe string
	pid        int
	minimized  bool
	x, y, w, h int
}

const uiaScript = `param([long]$Hwnd, [int]$Max = 3000)
$ErrorActionPreference = 'Stop'
$out = New-Object System.IO.StreamWriter([Console]::OpenStandardOutput(), (New-Object System.Text.UTF8Encoding $false))
try {
  Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes
  $A = [System.Windows.Automation.AutomationElement]
  $root = $A::FromHandle([IntPtr]$Hwnd)
  $cond = [System.Windows.Automation.Condition]::TrueCondition
  $all = $null
  for ($try = 0; $try -lt 3; $try++) {
    $all = $root.FindAll([System.Windows.Automation.TreeScope]::Descendants, $cond)
    if ($all.Count -gt 5) { break }
    Start-Sleep -Milliseconds 700
  }
  $vp = [System.Windows.Automation.ValuePattern]::Pattern
  $n = 0
  foreach ($e in $all) {
    if ($n -ge $Max) { break }
    try {
      $c = $e.Current
      if ($c.IsOffscreen) { continue }
      $t = $c.ControlType.ProgrammaticName -replace '^ControlType\.', ''
      $name = $c.Name
      $val = ''
      if ($t -eq 'Edit' -or $t -eq 'ComboBox' -or $t -eq 'Document') {
        $o = $null
        if ($e.TryGetCurrentPattern($vp, [ref]$o)) { $val = $o.Current.Value }
      }
      if (-not $name -and -not $val) { continue }
      $r = $c.BoundingRectangle
      if ($r.IsEmpty) { continue }
      $out.WriteLine([string]::Join([char]9, @([int]$r.Left, [int]$r.Top, [int]$r.Height, $t, ($name -replace '\s+', ' '), ($val -replace '\s+', ' '))))
      $n++
    } catch { }
  }
  $out.Flush()
} catch {
  $out.WriteLine('E ' + ($_.Exception.Message -replace '\s+', ' ')); $out.Flush(); exit 1
}
`

type uiaItem struct {
	x, y, h   int
	kind      string
	name, val string
}

// uiaText reads a window's accessibility tree and lays it out in rows, top to bottom.
func uiaText(w winInfo) (string, error) {
	script := filepath.Join(appHome, "scripts", "uiatext.ps1")
	if b, err := os.ReadFile(script); err != nil || string(b) != uiaScript {
		os.MkdirAll(filepath.Dir(script), 0o755)
		if err := os.WriteFile(script, []byte(uiaScript), 0o644); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := quietCmd(psExe(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, "-Hwnd", strconv.FormatInt(int64(w.hwnd), 10))
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	out, err := cmd.Output()
	close(done)
	var items []uiaItem
	seen := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "E ") {
			return "", errors.New("UI Automation: " + l[2:])
		}
		f := strings.Split(l, "\t")
		if len(f) < 6 {
			continue
		}
		var it uiaItem
		it.x, _ = strconv.Atoi(f[0])
		it.y, _ = strconv.Atoi(f[1])
		it.h, _ = strconv.Atoi(f[2])
		it.kind, it.name, it.val = f[3], strings.TrimSpace(f[4]), strings.TrimSpace(f[5])
		// containers repeat their children's text; keep leaf-like items
		switch it.kind {
		case "Window", "Pane", "Group", "Custom", "List", "Tree", "Table", "ToolBar", "MenuBar", "TitleBar", "Tab", "DataGrid", "ScrollBar", "Thumb", "Image", "Separator":
			if it.val == "" {
				continue
			}
		}
		key := it.name + "\x00" + it.val
		if seen[key] && it.val == "" {
			continue
		}
		seen[key] = true
		items = append(items, it)
	}
	if err != nil && len(items) == 0 {
		if ctx.Err() != nil {
			return "", errors.New("UI Automation took too long")
		}
		return "", fmt.Errorf("UI Automation failed: %v", err)
	}
	sort.SliceStable(items, func(a, b int) bool {
		if absInt(items[a].y-items[b].y) > max(4, min(items[a].h, items[b].h)/2) {
			return items[a].y < items[b].y
		}
		return items[a].x < items[b].x
	})
	var b strings.Builder
	lastY, lastH := -1<<30, 0
	for i, it := range items {
		s := it.name
		switch it.kind {
		case "Button", "SplitButton", "MenuItem", "TabItem", "Hyperlink":
			s = "[" + it.name + "]"
		case "CheckBox", "RadioButton":
			s = "(" + it.name + ")"
		}
		if it.val != "" && it.val != it.name {
			if it.name != "" {
				s = it.name + ": " + it.val
			} else {
				s = it.val
			}
		}
		if i > 0 {
			if absInt(it.y-lastY) > max(4, min(it.h, lastH)/2) {
				b.WriteByte('\n')
			} else {
				b.WriteString("  ")
			}
		}
		b.WriteString(s)
		lastY, lastH = it.y, it.h
	}
	return b.String(), nil
}

// findWindow picks the window that best matches a title, exe name, pid or handle.
func findWindow(q string) (winInfo, error) {
	ws := listWindowsRaw()
	q = strings.TrimSpace(q)
	if q == "" {
		fg := foregroundHwnd()
		for _, w := range ws {
			if w.hwnd == fg {
				return w, nil
			}
		}
		return winInfo{}, errors.New("give window: a title, program name (e.g. steamwebhelper) or pid")
	}
	lq := strings.ToLower(strings.TrimSuffix(strings.ToLower(q), ".exe"))
	n, numErr := strconv.ParseInt(q, 0, 64)
	var best *winInfo
	score := func(w winInfo) int {
		s := 0
		switch {
		case numErr == nil && (int64(w.pid) == n || int64(w.hwnd) == n):
			s = 100
		case strings.EqualFold(w.title, q):
			s = 90
		case strings.ToLower(strings.TrimSuffix(strings.ToLower(w.exe), ".exe")) == lq:
			s = 80
		case strings.Contains(strings.ToLower(w.title), lq):
			s = 70
		default:
			return 0
		}
		if !w.minimized {
			s += 5
		}
		return s
	}
	bs := 0
	for i := range ws {
		s := score(ws[i])
		if s > bs || s == bs && s > 0 && ws[i].w*ws[i].h > best.w*best.h {
			best, bs = &ws[i], s
		}
	}
	if best == nil {
		return winInfo{}, fmt.Errorf("no open window matches %q (window list shows them)", q)
	}
	return *best, nil
}

func capText(s string, maxc int, label string) (string, string) {
	if len(s) <= maxc {
		return s, ""
	}
	dir := filepath.Join(appHome, "cache", "windows")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, slug(label)+".txt")
	os.WriteFile(p, []byte(s), 0o644)
	cut := maxc
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut], fmt.Sprintf("\n[cut at %d of %d characters; all of it is in %s (read, grep or local_ai it)]", cut, len(s), p)
}

func toolWindow(raw json.RawMessage) (string, error) {
	var a struct {
		Action   string          `json:"action"`
		Window   string          `json:"window"`
		X        *int            `json:"x"`
		Y        *int            `json:"y"`
		W        int             `json:"w"`
		H        int             `json:"h"`
		MaxChars int             `json:"max_chars"`
		Summary  bool            `json:"summary"`
		Filter   string          `json:"filter"`
		Steps    json.RawMessage `json:"steps"`
		Then     string          `json:"then"`
		Pattern  string          `json:"pattern"`
		Changes  bool            `json:"changes"`
		Wait     float64         `json:"wait"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	if !inputSupported {
		return "", errNoInput
	}
	dpiAware()
	maxc := a.MaxChars
	if maxc <= 0 {
		maxc = 4000
	}
	maxc = min(maxc, 30000)
	act := strings.ToLower(a.Action)
	if act == "" || act == "list" {
		var b strings.Builder
		ws := listWindowsRaw()
		for _, w := range ws {
			line := fmt.Sprintf("%s · %s pid %d", clip(w.title, 70), w.exe, w.pid)
			if a.Filter != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(a.Filter)) {
				continue
			}
			if w.minimized {
				line += " · minimized"
			} else {
				line += fmt.Sprintf(" · %dx%d at %d,%d", w.w, w.h, w.x, w.y)
			}
			b.WriteString(line + "\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	if act == "wait" {
		secs := a.Wait
		if secs <= 0 {
			secs = 30
		}
		end := time.Now().Add(time.Duration(min(secs, 50) * float64(time.Second)))
		for {
			if w, err := findWindow(a.Window); err == nil {
				if a.Pattern == "" {
					return fmt.Sprintf("%s is open: %s", w.exe, w.title), nil
				}
				if strings.Contains(strings.ToLower(w.title), strings.ToLower(a.Pattern)) {
					return fmt.Sprintf("%q is in the title: %s", a.Pattern, w.title), nil
				}
				t, _ := uiaText(w)
				if strings.TrimSpace(t) == "" {
					t, _ = windowOCR(w)
				}
				if i := strings.Index(strings.ToLower(t), strings.ToLower(a.Pattern)); i >= 0 {
					ls := strings.LastIndex(t[:i], "\n") + 1
					le := strings.Index(t[i:], "\n")
					if le < 0 {
						le = len(t) - i
					}
					return fmt.Sprintf("%q showed in %s: %s", a.Pattern, w.exe, clip(t[ls:i+le], 200)), nil
				}
			}
			if time.Now().After(end) {
				return "", fmt.Errorf("not seen within %gs (call again to keep waiting)", min(secs, 50))
			}
			time.Sleep(1500 * time.Millisecond)
		}
	}
	var text, label string
	if act == "ocr" && a.X != nil && a.Y != nil && a.W > 0 && a.H > 0 {
		px, err := grabArea(*a.X, *a.Y, a.W, a.H)
		if err != nil {
			return "", err
		}
		label = fmt.Sprintf("area %d,%d %dx%d", *a.X, *a.Y, a.W, a.H)
		if text, err = ocrPNG(bgraPNG(px, a.W, a.H)); err != nil {
			return "", err
		}
	} else {
		w, err := findWindow(a.Window)
		if err != nil {
			return "", err
		}
		label = w.exe + " " + clip(w.title, 40)
		switch act {
		case "text":
			if text, err = uiaText(w); err != nil {
				return "", err
			}
			if strings.TrimSpace(text) == "" {
				if t2, err2 := windowOCR(w); err2 == nil && strings.TrimSpace(t2) != "" {
					text = "[no accessible text; OCR]\n" + t2
				}
			}
		case "ocr":
			if text, err = windowOCR(w); err != nil {
				return "", err
			}
		case "copy":
			if text, err = copyWindowText(w); err != nil {
				return "", err
			}
		case "do":
			res, err := windowDo(w, a.Steps)
			if err != nil {
				return "", err
			}
			if a.Then == "text" {
				time.Sleep(400 * time.Millisecond)
				if w2, err := findWindow(strconv.FormatInt(int64(w.hwnd), 10)); err == nil {
					w = w2
				}
				if t, err := uiaText(w); err == nil {
					t, note := capText(t, maxc, label)
					return res + "\nwindow now:\n" + t + note, nil
				}
			}
			return res, nil
		default:
			return "", errors.New("action must be list, text, ocr, copy or do")
		}
	}
	if strings.TrimSpace(text) == "" {
		return label + ": no text found", nil
	}
	if a.Summary && len(text) > 1500 {
		dir := filepath.Join(appHome, "cache", "windows")
		os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, slug(label)+".txt")
		if err := os.WriteFile(p, []byte(text), 0o644); err == nil {
			args, _ := json.Marshal(map[string]any{"action": "summarize", "paths": []string{p}, "detail": "normal"})
			if s, err := toolLocalAI(args); err == nil {
				return label + " (summary by local AI; full text in " + p + ")\n" + s, nil
			}
		}
	}
	if a.Changes {
		text = changesSince("window:"+act+":"+label, text)
	}
	body, note := capText(text, maxc, label)
	return label + "\n" + body + note, nil
}

func windowOCR(w winInfo) (string, error) {
	img, err := captureWindow(w)
	if err != nil {
		return "", err
	}
	return ocrPNG(img)
}

func ocrPNG(img []byte) (string, error) {
	var pg ocrPage
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := runWinOCR(ctx, ocrReq{mode: "stdin", stdin: img, upscale: true}, func(p ocrPage) { pg = p }); err != nil {
		return "", err
	}
	return pg.text(), nil
}

func bgraPNG(px []byte, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i+3 < len(px) && i+3 < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = px[i+2], px[i+1], px[i], 255
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
