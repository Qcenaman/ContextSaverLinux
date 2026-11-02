package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Windows on Linux: under Xorg, wmctrl/xdotool list, focus and capture them by window id;
// on Wayland the accessibility tree lists them (atspi_linux.go).

func xWindows() bool {
	if wayland() || os.Getenv("DISPLAY") == "" {
		return false
	}
	_, err := exec.LookPath("wmctrl")
	return err == nil
}

func listWindowsRaw() []winInfo {
	if !xWindows() {
		return atspiWindows()
	}
	out, err := exec.Command("wmctrl", "-lpG").Output()
	if err != nil {
		return atspiWindows()
	}
	var ws []winInfo
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 8 || f[1] == "-1" { // -1: sticky desktop/panel windows
			continue
		}
		id, _ := strconv.ParseUint(f[0], 0, 64)
		n := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
		title := ""
		if len(f) > 8 {
			title = strings.Join(f[8:], " ")
		}
		w := winInfo{hwnd: uintptr(id), pid: n(2), x: n(3), y: n(4), w: n(5), h: n(6), title: title}
		w.exe = procExe(w.pid)
		if st, err := exec.Command("xprop", "-id", f[0], "_NET_WM_STATE").Output(); err == nil && bytes.Contains(st, []byte("HIDDEN")) {
			w.minimized = true
		}
		ws = append(ws, w)
	}
	return ws
}

func foregroundHwnd() uintptr {
	if xWindows() {
		if out, err := exec.Command("xdotool", "getactivewindow").Output(); err == nil {
			id, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			return uintptr(id)
		}
		return 0
	}
	atspiWindows()
	return atspiActive
}

func focusWindow(w winInfo) error {
	if xWindows() {
		if err := exec.Command("wmctrl", "-ia", fmt.Sprintf("0x%x", w.hwnd)).Run(); err != nil {
			return fmt.Errorf("couldn't bring the window to the front: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
		return nil
	}
	atspiOutput("focus", w, 10*time.Second) // best effort: Wayland decides which window is in front
	return nil
}

// captureWindow: a PNG of the window (Xorg), or of its part of a full screenshot.
func captureWindow(w winInfo) ([]byte, error) {
	if xWindows() {
		if _, err := exec.LookPath("import"); err != nil {
			return nil, errors.New("screenshots need ImageMagick (" + installHint("imagemagick") + ")")
		}
		return exec.Command("import", "-window", fmt.Sprintf("0x%x", w.hwnd), "png:-").Output()
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("contextsaver-shot-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := exec.Command("gnome-screenshot", "-f", tmp).Run(); err != nil || !exists(tmp) {
		return nil, errNoInput
	}
	b, err := os.ReadFile(tmp)
	if err != nil || w.w <= 0 || w.h <= 0 {
		return b, err
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return b, nil
	}
	r := image.Rect(w.x, w.y, w.x+w.w, w.y+w.h).Intersect(img.Bounds())
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if r.Empty() || !ok {
		return b, nil
	}
	var buf bytes.Buffer
	png.Encode(&buf, sub.SubImage(r))
	return buf.Bytes(), nil
}

// copyWindowText: select all + copy in the window, clipboard restored (Xorg); on
// Wayland its accessible text.
func copyWindowText(w winInfo) (string, error) {
	if err := xInput(); err != nil {
		return uiaText(w)
	}
	old, hadOld := clipText()
	if err := focusWindow(w); err != nil {
		return "", err
	}
	setClipText("")
	pressCombo("ctrl+a")
	time.Sleep(60 * time.Millisecond)
	pressCombo("ctrl+c")
	var t string
	for i := 0; i < 15; i++ {
		time.Sleep(100 * time.Millisecond)
		if t, _ = clipText(); t != "" {
			break
		}
	}
	xdo("click", "1") // drop the selection
	if hadOld {
		setClipText(old)
	}
	if t == "" {
		return "", errors.New("nothing was copied from that window")
	}
	return t, nil
}

func pasteText(s string) error {
	if err := xInput(); err != nil {
		return err
	}
	if err := setClipText(s); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)
	return pressCombo("ctrl+v")
}

func clipText() (string, bool) {
	var out []byte
	var err error
	if wayland() {
		out, err = exec.Command("wl-paste", "--no-newline").Output()
	} else {
		out, err = exec.Command("xclip", "-selection", "clipboard", "-o").Output()
	}
	if err != nil {
		return "", false
	}
	return string(out), true
}

func setClipText(s string) error {
	var c *exec.Cmd
	if wayland() {
		c = exec.Command("wl-copy")
	} else {
		c = exec.Command("xclip", "-selection", "clipboard", "-i")
	}
	c.Stdin = strings.NewReader(s)
	if err := c.Start(); err != nil {
		return errors.New("the clipboard needs wl-clipboard (Wayland) or xclip (Xorg): " + installHint("wl-clipboard xclip"))
	}
	go c.Wait() // both keep serving the clipboard until something else takes it
	return nil
}
