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
	"strings"
	"time"
)

// 1.15.0: the rest of the screen and clipboard work, so Claude (or a local model)
// rarely needs full screenshots: click/scroll/drag by position, click on OCR text,
// hold keys, clipboard get/set, and small screenshots (or a local-AI answer about one).

// ocrPageOf runs Windows OCR on a PNG and keeps word boxes.
func ocrPageOf(img []byte) (ocrPage, error) {
	var pg ocrPage
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := runWinOCR(ctx, ocrReq{mode: "stdin", stdin: img, upscale: true, boxes: true}, func(p ocrPage) { pg = p }); err != nil {
		return pg, err
	}
	return pg, nil
}

// lineBox is the box around a line's words.
func lineBox(l ocrLine) (x, y, w, h int, ok bool) {
	if len(l.Words) == 0 {
		return 0, 0, 0, 0, false
	}
	x0, y0, x1, y1 := 1<<30, 1<<30, -1<<30, -1<<30
	for _, wd := range l.Words {
		x0, y0 = min(x0, wd.X), min(y0, wd.Y)
		x1, y1 = max(x1, wd.X+wd.W), max(y1, wd.Y+wd.H)
	}
	return x0, y0, x1 - x0, y1 - y0, true
}

// ocrWithPositions lays out OCR text with each line's centre, offset by dx,dy.
func ocrWithPositions(pg ocrPage, dx, dy int) string {
	var b strings.Builder
	for i, l := range pg.Lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.Text)
		if x, y, w, h, ok := lineBox(l); ok {
			fmt.Fprintf(&b, "  @%d,%d", dx+x+w/2, dy+y+h/2)
		}
	}
	return b.String()
}

// findOCRText returns the centre of the words matching q (window-relative), preferring exact matches.
func findOCRText(pg ocrPage, q string, nth int) (int, int, string, error) {
	q = strings.ToLower(strings.Join(strings.Fields(q), " "))
	if q == "" {
		return 0, 0, "", errors.New("click_text needs the text to click")
	}
	type hit struct {
		x, y  int
		text  string
		exact bool
	}
	var hits []hit
	for _, l := range pg.Lines {
		ws := l.Words
		for i := range ws {
			s := ""
			for j := i; j < len(ws); j++ {
				if j > i {
					s += " "
				}
				s += strings.ToLower(ws[j].Text)
				if strings.Contains(s, q) {
					x0, y0, x1, y1 := 1<<30, 1<<30, -1<<30, -1<<30
					for _, wd := range ws[i : j+1] {
						x0, y0 = min(x0, wd.X), min(y0, wd.Y)
						x1, y1 = max(x1, wd.X+wd.W), max(y1, wd.Y+wd.H)
					}
					hits = append(hits, hit{(x0 + x1) / 2, (y0 + y1) / 2, l.Text, strings.TrimSpace(s) == q})
					break
				}
				if len(s) > len(q)+40 {
					break
				}
			}
		}
	}
	if len(hits) == 0 {
		return 0, 0, "", fmt.Errorf("%q isn't on screen in that window (window ocr shows what is)", q)
	}
	var exact []hit
	for _, h := range hits {
		if h.exact {
			exact = append(exact, h)
		}
	}
	if len(exact) > 0 {
		hits = exact
	}
	if nth <= 0 {
		nth = 1
	}
	if nth > len(hits) {
		return 0, 0, "", fmt.Errorf("%q shows %d time(s), not %d", q, len(hits), nth)
	}
	h := hits[nth-1]
	note := ""
	if len(hits) > 1 {
		note = fmt.Sprintf(" (%d of %d matches)", nth, len(hits))
	}
	return h.x, h.y, clip(h.text, 60) + note, nil
}

// pointOf turns a step's [x,y] into screen coordinates (window-relative unless screen:true).
func pointOf(w winInfo, p []int, screen bool) (int, int, error) {
	if len(p) < 2 {
		return 0, 0, errors.New("give a position as [x, y]")
	}
	if screen {
		return p[0], p[1], nil
	}
	if w.minimized {
		return 0, 0, errors.New("that window is minimized")
	}
	return w.x + p[0], w.y + p[1], nil
}

// moveSmooth glides the pointer so apps see a real drag.
func moveSmooth(x0, y0, x1, y1 int) {
	const n = 16
	for i := 1; i <= n; i++ {
		setCursor(x0+(x1-x0)*i/n, y0+(y1-y0)*i/n)
		time.Sleep(12 * time.Millisecond)
	}
}

// screenStep runs the 1.15.0 do-steps; handled is false for steps it doesn't know.
func screenStep(w winInfo, s uiStep) (r string, handled bool, err error) {
	switch {
	case s.ClickText != "":
		img, err := captureWindow(w)
		if err != nil {
			return "", true, err
		}
		pg, err := ocrPageOf(img)
		if err != nil {
			return "", true, err
		}
		x, y, what, err := findOCRText(pg, s.ClickText, s.Nth)
		if err != nil {
			return "", true, err
		}
		if err := focusWindow(w); err != nil {
			return "", true, err
		}
		if err := clickAt(w.x+x, w.y+y, s.Button, 1, nil); err != nil {
			return "", true, err
		}
		return fmt.Sprintf("clicked %q at %d,%d", what, x, y), true, nil
	case len(s.ClickAt) > 0:
		x, y, err := pointOf(w, s.ClickAt, s.Screen)
		if err != nil {
			return "", true, err
		}
		if err := focusWindow(w); err != nil {
			return "", true, err
		}
		if err := clickAt(x, y, s.Button, 1, nil); err != nil {
			return "", true, err
		}
		b := s.Button
		if b == "" {
			b = "left"
		}
		return fmt.Sprintf("%s click at %d,%d", b, s.ClickAt[0], s.ClickAt[1]), true, nil
	case s.Scroll != 0:
		var x, y int
		if len(s.At) > 0 {
			if x, y, err = pointOf(w, s.At, s.Screen); err != nil {
				return "", true, err
			}
		} else {
			if w.minimized {
				return "", true, errors.New("that window is minimized")
			}
			x, y = w.x+w.w/2, w.y+w.h/2
		}
		setCursor(x, y)
		time.Sleep(30 * time.Millisecond)
		n := max(min(s.Scroll, 50), -50)
		for i := 0; i < absInt(n); i++ {
			if n > 0 {
				mouseWheel(1)
			} else {
				mouseWheel(-1)
			}
			time.Sleep(15 * time.Millisecond)
		}
		dir := "up"
		if n < 0 {
			dir = "down"
		}
		return fmt.Sprintf("scrolled %s %d", dir, absInt(n)), true, nil
	case len(s.Drag) > 0:
		if len(s.Drag) < 4 {
			return "", true, errors.New("drag: [x1, y1, x2, y2]")
		}
		x0, y0, err := pointOf(w, s.Drag[:2], s.Screen)
		if err != nil {
			return "", true, err
		}
		x1, y1, _ := pointOf(w, s.Drag[2:4], s.Screen)
		if err := focusWindow(w); err != nil {
			return "", true, err
		}
		btn := s.Button
		if btn == "" || btn == "double" {
			btn = "left"
		}
		setCursor(x0, y0)
		time.Sleep(40 * time.Millisecond)
		if err := mouseButton(btn, true); err != nil {
			return "", true, err
		}
		time.Sleep(60 * time.Millisecond)
		moveSmooth(x0, y0, x1, y1)
		time.Sleep(60 * time.Millisecond)
		mouseButton(btn, false)
		return fmt.Sprintf("dragged %d,%d → %d,%d", s.Drag[0], s.Drag[1], s.Drag[2], s.Drag[3]), true, nil
	case len(s.Move) > 0:
		x, y, err := pointOf(w, s.Move, s.Screen)
		if err != nil {
			return "", true, err
		}
		cx, cy := cursorPos()
		moveSmooth(cx, cy, x, y)
		return fmt.Sprintf("pointer at %d,%d", s.Move[0], s.Move[1]), true, nil
	case s.Hold != "":
		keys, err := parseCombo(s.Hold)
		if err != nil {
			return "", true, err
		}
		if err := focusWindow(w); err != nil {
			return "", true, err
		}
		secs := s.Secs
		if secs <= 0 {
			secs = 1
		}
		secs = min(secs, 10)
		for _, k := range keys {
			sendKey(k, false)
		}
		end := time.Now().Add(time.Duration(secs * float64(time.Second)))
		for time.Now().Before(end) {
			time.Sleep(30 * time.Millisecond)
		}
		for i := len(keys) - 1; i >= 0; i-- {
			sendKey(keys[i], true)
		}
		return fmt.Sprintf("held %s %gs", s.Hold, secs), true, nil
	}
	return "", false, nil
}

// toolWindowFull adds the actions that return images or touch the clipboard, then hands the rest to toolWindow.
func toolWindowFull(raw json.RawMessage) (string, []toolImage, error) {
	var a struct {
		Action   string  `json:"action"`
		Window   string  `json:"window"`
		X        *int    `json:"x"`
		Y        *int    `json:"y"`
		W        int     `json:"w"`
		H        int     `json:"h"`
		MaxPx    int     `json:"max_px"`
		MaxChars int     `json:"max_chars"`
		Text     *string `json:"text"`
		Question string  `json:"question"`
		Force    bool    `json:"force"`
		Boxes    bool    `json:"boxes"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", nil, err
	}
	act := strings.ToLower(a.Action)
	switch {
	case act == "clipboard":
		if !inputSupported {
			return "", nil, errNoInput
		}
		if a.Text != nil {
			if err := setClipText(*a.Text); err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("clipboard set (%d characters); paste with do [{\"press\":\"ctrl+v\"}]", len([]rune(*a.Text))), nil, nil
		}
		t, ok := clipText()
		if !ok {
			return "clipboard holds no text", nil, nil
		}
		maxc := a.MaxChars
		if maxc <= 0 {
			maxc = 4000
		}
		body, note := capText(t, min(maxc, 30000), "clipboard")
		return body + note, nil, nil
	case act == "screenshot":
		s, img, err := screenshot(a.Window, a.X, a.Y, a.W, a.H, a.MaxPx, a.Question, a.Force)
		if img == nil {
			return s, nil, err
		}
		return s, []toolImage{{data: img, mime: "image/jpeg"}}, err
	case act == "ocr" && a.Boxes:
		if !inputSupported {
			return "", nil, errNoInput
		}
		dpiAware()
		var img []byte
		var dx, dy int
		label := ""
		if a.X != nil && a.Y != nil && a.W > 0 && a.H > 0 {
			px, err := grabArea(*a.X, *a.Y, a.W, a.H)
			if err != nil {
				return "", nil, err
			}
			img, dx, dy = bgraPNG(px, a.W, a.H), *a.X, *a.Y
			label = fmt.Sprintf("area %d,%d %dx%d (@ = screen x,y; use screen:true in do)", *a.X, *a.Y, a.W, a.H)
		} else {
			w, err := findWindow(a.Window)
			if err != nil {
				return "", nil, err
			}
			if img, err = captureWindow(w); err != nil {
				return "", nil, err
			}
			label = w.exe + " " + clip(w.title, 40) + " (@ = x,y in the window, for do click_at)"
		}
		pg, err := ocrPageOf(img)
		if err != nil {
			return "", nil, err
		}
		text := ocrWithPositions(pg, dx, dy)
		if strings.TrimSpace(text) == "" {
			return label + ": no text found", nil, nil
		}
		maxc := a.MaxChars
		if maxc <= 0 {
			maxc = 4000
		}
		body, note := capText(text, min(maxc, 30000), label)
		return label + "\n" + body + note, nil, nil
	}
	s, err := toolWindow(raw)
	return s, nil, err
}

// screenshot returns a small JPEG of a window or area, or (with question) the local AI's answer about it.
func screenshot(window string, x, y *int, w, h, maxPx int, question string, force bool) (string, []byte, error) {
	if !inputSupported {
		return "", nil, errNoInput
	}
	dpiAware()
	var pngData []byte
	var label string
	if x != nil && y != nil && w > 0 && h > 0 {
		px, err := grabArea(*x, *y, w, h)
		if err != nil {
			return "", nil, err
		}
		pngData = bgraPNG(px, w, h)
		label = fmt.Sprintf("area %d,%d %dx%d", *x, *y, w, h)
	} else {
		win, err := findWindow(window)
		if err != nil {
			return "", nil, err
		}
		if pngData, err = captureWindow(win); err != nil {
			return "", nil, err
		}
		label = win.exe + " " + clip(win.title, 40)
	}
	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return "", nil, err
	}
	rgba, ok := src.(*image.RGBA)
	if !ok {
		return "", nil, errors.New("unexpected image format")
	}
	if maxPx <= 0 {
		maxPx = 1000
	}
	maxPx = max(min(maxPx, 1600), 200)
	ow, oh := rgba.Bounds().Dx(), rgba.Bounds().Dy()
	small := shrink(rgba, maxPx)
	nw, nh := small.Bounds().Dx(), small.Bounds().Dy()
	var buf bytes.Buffer
	if err := jpegEncodeQ(&buf, small, 70); err != nil {
		return "", nil, err
	}
	scale := float64(ow) / float64(nw)
	info := fmt.Sprintf("%s: %dx%d shown at %dx%d", label, ow, oh, nw, nh)
	if scale > 1.01 {
		info += fmt.Sprintf("; multiply picture x,y by %.2f for click_at", scale)
	}
	if strings.TrimSpace(question) != "" {
		dir := filepath.Join(appHome, "cache", "windows")
		os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, slug("shot "+label)+".jpg")
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			return "", nil, err
		}
		args, _ := json.Marshal(map[string]any{"action": "ask", "paths": []string{p}, "question": question, "force": force})
		ans, err := toolLocalAI(args)
		if err != nil {
			return "", nil, err
		}
		return info + " (answered by local AI from " + p + ")\n" + ans, nil, nil
	}
	return info, buf.Bytes(), nil
}
