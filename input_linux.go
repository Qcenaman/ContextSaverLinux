package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Mouse and keyboard on Linux. Under Xorg xdotool does what Windows' SendInput does.
// Wayland doesn't let one program click or type into another; there window text and
// clicking or filling named controls go through accessibility (atspi_linux.go) instead.

const inputSupported = true

var errNoInput = errors.New("clicking at a position, keys and screenshots of other apps need an Xorg session (log in with \"Ubuntu on Xorg\"); on Wayland use window text, do{click:name} and {type,into}")

func wayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}

// xInput: xdotool can drive the screen (an Xorg session with xdotool installed).
func xInput() error {
	if wayland() || os.Getenv("DISPLAY") == "" {
		return errNoInput
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		return errors.New("xdotool isn't installed (" + installHint("xdotool") + ")")
	}
	return nil
}

func xdo(args ...string) error {
	if err := xInput(); err != nil {
		return err
	}
	return exec.Command("xdotool", args...).Run()
}

func dpiAware() {}

func cursorPos() (int, int) {
	if xInput() != nil {
		return 0, 0
	}
	out, err := exec.Command("xdotool", "getmouselocation", "--shell").Output()
	if err != nil {
		return 0, 0
	}
	var x, y int
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "X="); ok {
			x, _ = strconv.Atoi(v)
		} else if v, ok := strings.CutPrefix(l, "Y="); ok {
			y, _ = strconv.Atoi(v)
		}
	}
	return x, y
}

func setCursor(x, y int) { xdo("mousemove", strconv.Itoa(x), strconv.Itoa(y)) }

var xButtons = map[string]string{"left": "1", "middle": "2", "right": "3"}

func mouseButton(button string, down bool) error {
	b, ok := xButtons[strings.ToLower(button)]
	if !ok {
		return fmt.Errorf("unknown mouse button %q", button)
	}
	act := "mouseup"
	if down {
		act = "mousedown"
	}
	return xdo(act, b)
}

func mouseWheel(notches int) {
	b := "4"
	if notches < 0 {
		b, notches = "5", -notches
	}
	if notches > 0 {
		xdo("click", "--repeat", strconv.Itoa(notches), b)
	}
}

// xKeysyms: ContextSaver's key names (macro.go) as X keysyms where they differ.
var xKeysyms = map[string]string{"backspace": "BackSpace", "tab": "Tab", "enter": "Return", "shift": "shift", "ctrl": "ctrl",
	"alt": "alt", "pause": "Pause", "capslock": "Caps_Lock", "esc": "Escape", "space": "space", "pageup": "Prior",
	"pagedown": "Next", "end": "End", "home": "Home", "left": "Left", "up": "Up", "right": "Right", "down": "Down",
	"printscreen": "Print", "insert": "Insert", "delete": "Delete", "win": "super", "rwin": "Super_R", "menu": "Menu",
	"multiply": "KP_Multiply", "add": "KP_Add", "subtract": "KP_Subtract", "decimal": "KP_Decimal", "divide": "KP_Divide",
	"numlock": "Num_Lock", "scrolllock": "Scroll_Lock", "mute": "XF86AudioMute", "volumedown": "XF86AudioLowerVolume",
	"volumeup": "XF86AudioRaiseVolume", "nexttrack": "XF86AudioNext", "prevtrack": "XF86AudioPrev", "stopmedia": "XF86AudioStop",
	"playpause": "XF86AudioPlay", ";": "semicolon", "=": "equal", ",": "comma", "-": "minus", ".": "period", "/": "slash",
	"`": "grave", "[": "bracketleft", "\\": "backslash", "]": "bracketright", "'": "apostrophe"}

func keysym(vk uint16) string {
	n := vkNames[vk]
	if s, ok := xKeysyms[n]; ok {
		return s
	}
	switch {
	case strings.HasPrefix(n, "num") && len(n) == 4:
		return "KP_" + n[3:]
	case strings.HasPrefix(n, "f") && len(n) > 1:
		return "F" + n[1:]
	}
	return n
}

func sendKey(vk uint16, up bool) {
	if s := keysym(vk); s != "" {
		act := "keydown"
		if up {
			act = "keyup"
		}
		xdo(act, s)
	}
}

func typeText(s string) { xdo("type", "--delay", "2", "--", s) }

func keyHeld(vk uint16) bool { return false }

// grabArea: a PNG of a screen rectangle.
func grabArea(x, y, w, h int) ([]byte, error) {
	if err := xInput(); err != nil {
		return nil, err
	}
	return exec.Command("import", "-window", "root", "-crop", fmt.Sprintf("%dx%d+%d+%d", w, h, x, y), "+repage", "png:-").Output()
}

func recordInput(stopVK uint16) ([]rawInput, error) {
	return nil, errors.New("recording macros needs Windows (Linux has no global input hooks); write the steps instead: macro new")
}
