package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Macros the user records or builds in the ContextSaver terminal, played back with the
// mouse and keyboard, and the user's own triggers (a hotkey, or a color showing up or
// changing in an area of the screen) that click a spot or play a macro.
// Everything lives in D:\Claude Apps\ContextSaver\Macros as editable JSON.

type MacroStep struct {
	Click     string   `json:"click,omitempty"` // left, right, middle
	Count     int      `json:"count,omitempty"` // 2 = double click
	Down      string   `json:"down,omitempty"`  // hold a mouse button (drags)
	Up        string   `json:"up,omitempty"`
	Move      bool     `json:"move,omitempty"`
	X         *int     `json:"x,omitempty"`
	Y         *int     `json:"y,omitempty"`
	Scroll    int      `json:"scroll,omitempty"` // notches: + up, - down
	Press     string   `json:"press,omitempty"`  // keys like ctrl+s, enter, f5
	KeyDown   string   `json:"key_down,omitempty"`
	KeyUp     string   `json:"key_up,omitempty"`
	Type      string   `json:"type,omitempty"`
	Wait      int      `json:"wait,omitempty"`       // milliseconds
	WaitColor string   `json:"wait_color,omitempty"` // wait until the pixel at x,y shows this color
	Tolerance int      `json:"tolerance,omitempty"`
	Timeout   int      `json:"timeout,omitempty"` // ms, for wait_color (default 10000)
	Mods      []string `json:"mods,omitempty"`    // keys held during a click
}

type Macro struct {
	Name    string      `json:"name"`
	Steps   []MacroStep `json:"steps"`
	Created time.Time   `json:"created"`
}

type Trigger struct {
	Name       string `json:"name"`
	On         bool   `json:"on"`
	Kind       string `json:"kind"` // color or hotkey
	X          int    `json:"x,omitempty"`
	Y          int    `json:"y,omitempty"`
	W          int    `json:"w,omitempty"`
	H          int    `json:"h,omitempty"`
	Color      string `json:"color,omitempty"`     // fire when the area shows this color; empty = when it changes
	Tolerance  int    `json:"tolerance,omitempty"` // 0-255 per channel
	Hotkey     string `json:"hotkey,omitempty"`
	Do         string `json:"do"` // click or macro
	ClickX     int    `json:"click_x,omitempty"`
	ClickY     int    `json:"click_y,omitempty"`
	Button     string `json:"button,omitempty"` // left, right, middle, double
	Macro      string `json:"macro,omitempty"`
	MouseBack  bool   `json:"mouse_back,omitempty"`
	CooldownMs int    `json:"cooldown_ms,omitempty"`
}

type rawInput struct {
	kind   string // mouse, wheel, move, key
	button string
	down   bool
	x, y   int
	wheel  int
	vk     uint16
	at     time.Duration
}

var errMacroStopped = errors.New("stopped (Ctrl+Alt+Esc)")

func ip(v int) *int { return &v }

func utf16Units(s string) []uint16 { return utf16.Encode([]rune(s)) }

// ---------- keys ----------

var keyNames = map[string]uint16{
	"backspace": 0x08, "tab": 0x09, "enter": 0x0D, "shift": 0x10, "ctrl": 0x11, "alt": 0x12, "pause": 0x13,
	"capslock": 0x14, "esc": 0x1B, "space": 0x20, "pageup": 0x21, "pagedown": 0x22, "end": 0x23, "home": 0x24,
	"left": 0x25, "up": 0x26, "right": 0x27, "down": 0x28, "printscreen": 0x2C, "insert": 0x2D, "delete": 0x2E,
	"win": 0x5B, "rwin": 0x5C, "menu": 0x5D, "multiply": 0x6A, "add": 0x6B, "subtract": 0x6D, "decimal": 0x6E,
	"divide": 0x6F, "numlock": 0x90, "scrolllock": 0x91, "lshift": 0xA0, "rshift": 0xA1, "lctrl": 0xA2,
	"rctrl": 0xA3, "lalt": 0xA4, "ralt": 0xA5, "mute": 0xAD, "volumedown": 0xAE, "volumeup": 0xAF,
	"nexttrack": 0xB0, "prevtrack": 0xB1, "stopmedia": 0xB2, "playpause": 0xB3,
	";": 0xBA, "=": 0xBB, ",": 0xBC, "-": 0xBD, ".": 0xBE, "/": 0xBF, "`": 0xC0, "[": 0xDB, "\\": 0xDC, "]": 0xDD, "'": 0xDE,
}

var keyAliases = map[string]string{"control": "ctrl", "return": "enter", "escape": "esc", "del": "delete", "ins": "insert",
	"pgup": "pageup", "pgdn": "pagedown", "windows": "win", "lwin": "win", "cmd": "win", "apps": "menu"}

var vkNames = map[uint16]string{}

func init() {
	for c := 'a'; c <= 'z'; c++ {
		keyNames[string(c)] = uint16(c - 'a' + 'A')
	}
	for c := '0'; c <= '9'; c++ {
		keyNames[string(c)] = uint16(c)
	}
	for i := 1; i <= 24; i++ {
		keyNames["f"+strconv.Itoa(i)] = uint16(0x6F + i)
	}
	for i := 0; i <= 9; i++ {
		keyNames["num"+strconv.Itoa(i)] = uint16(0x60 + i)
	}
	for n, v := range keyNames {
		if old, ok := vkNames[v]; !ok || len(n) < len(old) {
			vkNames[v] = n
		}
	}
	for _, v := range []uint16{0xA0, 0xA1} {
		vkNames[v] = "shift"
	}
	for _, v := range []uint16{0xA2, 0xA3} {
		vkNames[v] = "ctrl"
	}
	for _, v := range []uint16{0xA4, 0xA5} {
		vkNames[v] = "alt"
	}
}

func keyVK(name string) (uint16, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if a, ok := keyAliases[n]; ok {
		n = a
	}
	if v, ok := keyNames[n]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("unknown key %q", name)
}

func isModifier(name string) bool {
	switch name {
	case "ctrl", "alt", "shift", "win":
		return true
	}
	return false
}

// parseCombo splits "ctrl+shift+s" into key codes (modifiers first).
func parseCombo(s string) ([]uint16, error) {
	s = strings.TrimSpace(s)
	var parts []string
	if strings.HasSuffix(s, "++") || s == "+" { // ctrl++ means ctrl and the + key
		return nil, errors.New("use add for the + key")
	}
	parts = strings.Split(s, "+")
	var out []uint16
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		v, err := keyVK(p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("no keys given")
	}
	return out, nil
}

func comboHeld(combo []uint16) bool {
	for _, v := range combo {
		if !keyHeld(v) {
			return false
		}
	}
	return len(combo) > 0
}

// stopKeys is the emergency stop: Ctrl+Alt+Esc.
func stopKeys() bool { return keyHeld(0x11) && keyHeld(0x12) && keyHeld(0x1B) }

// ---------- storage ----------

func macrosDir() string { return filepath.Join(appRoot(), "Macros") }

func macroPath(name string) string { return filepath.Join(macrosDir(), slug(name)+".json") }

func triggersPath() string { return filepath.Join(macrosDir(), "triggers.json") }

func loadMacro(name string) (*Macro, error) {
	b, err := os.ReadFile(macroPath(name))
	if err != nil {
		for _, m := range listMacros() {
			if strings.EqualFold(m.Name, name) {
				return m, nil
			}
		}
		return nil, fmt.Errorf("no macro called %q (macro list shows them)", name)
	}
	var m Macro
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s isn't valid: %v", macroPath(name), err)
	}
	return &m, nil
}

func saveMacro(m *Macro) error {
	if err := validName(m.Name); err != nil {
		return err
	}
	os.MkdirAll(macrosDir(), 0o755)
	return writeJSONAtomic(macroPath(m.Name), m)
}

func listMacros() []*Macro {
	ents, _ := os.ReadDir(macrosDir())
	var out []*Macro
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "triggers.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(macrosDir(), e.Name()))
		if err != nil {
			continue
		}
		var m Macro
		if json.Unmarshal(b, &m) == nil && m.Name != "" {
			out = append(out, &m)
		}
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out
}

var trigMu sync.Mutex

func loadTriggers() []Trigger {
	trigMu.Lock()
	defer trigMu.Unlock()
	var ts []Trigger
	if b, err := os.ReadFile(triggersPath()); err == nil {
		json.Unmarshal(b, &ts)
	}
	return ts
}

func saveTriggers(ts []Trigger) error {
	trigMu.Lock()
	defer trigMu.Unlock()
	os.MkdirAll(macrosDir(), 0o755)
	if ts == nil {
		ts = []Trigger{}
	}
	return writeJSONAtomic(triggersPath(), ts)
}

// ---------- describing ----------

func xyText(s MacroStep) string {
	if s.X == nil || s.Y == nil {
		return ""
	}
	return fmt.Sprintf(" at %d,%d", *s.X, *s.Y)
}

func stepText(s MacroStep) string {
	mods := ""
	if len(s.Mods) > 0 {
		mods = strings.Join(s.Mods, "+") + "+"
	}
	switch {
	case s.Click != "":
		kind := s.Click + " click"
		if s.Count == 2 {
			kind = s.Click + " double-click"
		}
		return mods + kind + xyText(s)
	case s.Down != "":
		return s.Down + " button down" + xyText(s)
	case s.Up != "":
		return s.Up + " button up" + xyText(s)
	case s.Move:
		return "move" + xyText(s)
	case s.Scroll != 0:
		return fmt.Sprintf("scroll %+d%s", s.Scroll, xyText(s))
	case s.Press != "":
		return "press " + s.Press
	case s.KeyDown != "":
		return "hold " + s.KeyDown
	case s.KeyUp != "":
		return "release " + s.KeyUp
	case s.Type != "":
		return fmt.Sprintf("type %q", clip(s.Type, 60))
	case s.WaitColor != "":
		return "wait for " + s.WaitColor + xyText(s)
	case s.Wait > 0:
		return fmt.Sprintf("wait %.2fs", float64(s.Wait)/1000)
	}
	return "?"
}

func macroSummary(m *Macro) string {
	var parts []string
	for _, s := range m.Steps {
		if s.Wait > 0 && s.Click == "" && s.Press == "" && s.Type == "" {
			continue
		}
		parts = append(parts, stepText(s))
	}
	return clip(strings.Join(parts, " · "), 200)
}

// ---------- turning a recording into steps ----------

func compactRecording(evs []rawInput) []MacroStep {
	var out []MacroStep
	var last time.Duration = -1
	held := map[string]bool{}
	used := map[string]bool{}
	addWait := func(at time.Duration) {
		if last >= 0 {
			if ms := int((at - last) / time.Millisecond); ms >= 40 {
				out = append(out, MacroStep{Wait: ms})
			}
		}
		last = at
	}
	heldMods := func() []string {
		var m []string
		for _, k := range []string{"ctrl", "alt", "shift", "win"} {
			if held[k] {
				m = append(m, k)
				used[k] = true
			}
		}
		return m
	}
	for i := 0; i < len(evs); i++ {
		e := evs[i]
		switch e.kind {
		case "mouse":
			if !e.down {
				continue
			}
			// a down and its up on the same spot is a click
			j := i + 1
			for j < len(evs) && evs[j].kind == "move" {
				j++
			}
			if j < len(evs) && evs[j].kind == "mouse" && !evs[j].down && evs[j].button == e.button &&
				absInt(evs[j].x-e.x) <= 4 && absInt(evs[j].y-e.y) <= 4 && evs[j].at-e.at < 800*time.Millisecond {
				if n := len(out); n > 0 && out[n-1].Click == e.button && out[n-1].Count < 2 && out[n-1].X != nil &&
					absInt(*out[n-1].X-e.x) <= 4 && absInt(*out[n-1].Y-e.y) <= 4 && e.at-last < 450*time.Millisecond {
					out[n-1].Count = 2
					last = evs[j].at
				} else {
					addWait(e.at)
					out = append(out, MacroStep{Click: e.button, X: ip(e.x), Y: ip(e.y), Mods: heldMods()})
				}
				i = j
				continue
			}
			addWait(e.at)
			out = append(out, MacroStep{Down: e.button, X: ip(e.x), Y: ip(e.y)})
			for i+1 < len(evs) && evs[i+1].kind == "move" {
				i++
				if evs[i].at-last >= 30*time.Millisecond {
					addWait(evs[i].at)
					out = append(out, MacroStep{Move: true, X: ip(evs[i].x), Y: ip(evs[i].y)})
				}
			}
			if i+1 < len(evs) && evs[i+1].kind == "mouse" && !evs[i+1].down {
				i++
				addWait(evs[i].at)
				out = append(out, MacroStep{Up: evs[i].button, X: ip(evs[i].x), Y: ip(evs[i].y)})
			}
		case "wheel":
			addWait(e.at)
			n := e.wheel / 120
			if n == 0 {
				n = 1
				if e.wheel < 0 {
					n = -1
				}
			}
			if k := len(out); k > 0 && out[k-1].Scroll != 0 && out[k-1].X != nil && *out[k-1].X == e.x && *out[k-1].Y == e.y && (out[k-1].Scroll > 0) == (n > 0) {
				out[k-1].Scroll += n
			} else {
				out = append(out, MacroStep{Scroll: n, X: ip(e.x), Y: ip(e.y)})
			}
		case "key":
			name, ok := vkNames[e.vk]
			if !ok {
				name = fmt.Sprintf("vk%d", e.vk)
			}
			if isModifier(name) {
				if e.down {
					if !held[name] {
						held[name], used[name] = true, false
					}
				} else if held[name] {
					held[name] = false
					if !used[name] {
						addWait(e.at)
						out = append(out, MacroStep{Press: name})
					}
				}
				continue
			}
			if !e.down {
				continue
			}
			// skip auto-repeat: the same key down again before it went up
			for i+1 < len(evs) && evs[i+1].kind == "key" && evs[i+1].vk == e.vk && evs[i+1].down {
				i++
			}
			mods := heldMods()
			combo := strings.Join(append(mods, name), "+")
			addWait(e.at)
			plain := len(name) == 1 && (len(mods) == 0 || len(mods) == 1 && mods[0] == "shift" && name[0] >= 'a' && name[0] <= 'z')
			if name == "space" && len(mods) == 0 {
				plain, name = true, " "
			}
			if plain {
				ch := name
				if len(mods) == 1 {
					ch = strings.ToUpper(name)
				}
				if k := len(out); k > 0 && out[k-1].Type != "" {
					out[k-1].Type += ch
				} else if k > 1 && out[k-1].Wait > 0 && out[k-1].Wait < 600 && out[k-2].Type != "" {
					out = out[:k-1]
					out[k-2].Type += ch
				} else {
					out = append(out, MacroStep{Type: ch})
				}
				continue
			}
			out = append(out, MacroStep{Press: combo})
		}
	}
	return out
}

// ---------- playing ----------

func sleepStop(d time.Duration, stop func() bool) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if stop() {
			return errMacroStopped
		}
		time.Sleep(min(25*time.Millisecond, time.Until(end)))
	}
	if stop() {
		return errMacroStopped
	}
	return nil
}

func pressCombo(combo string) error {
	keys, err := parseCombo(combo)
	if err != nil {
		return err
	}
	for _, k := range keys {
		sendKey(k, false)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		sendKey(keys[i], true)
	}
	return nil
}

func clickAt(x, y int, button string, count int, mods []string) error {
	if button == "double" {
		button, count = "left", 2
	}
	if button == "" {
		button = "left"
	}
	var modKeys []uint16
	for _, m := range mods {
		if v, err := keyVK(m); err == nil {
			modKeys = append(modKeys, v)
		}
	}
	setCursor(x, y)
	time.Sleep(15 * time.Millisecond)
	for _, k := range modKeys {
		sendKey(k, false)
	}
	for c := 0; c < max(count, 1); c++ {
		if err := mouseButton(button, true); err != nil {
			return err
		}
		mouseButton(button, false)
		if c+1 < count {
			time.Sleep(60 * time.Millisecond)
		}
	}
	for i := len(modKeys) - 1; i >= 0; i-- {
		sendKey(modKeys[i], true)
	}
	return nil
}

func playMacro(m *Macro, times int, speed float64, stop func() bool) error {
	if !inputSupported {
		return errNoInput
	}
	if speed <= 0 {
		speed = 1
	}
	for rep := 0; rep < max(times, 1); rep++ {
		for _, s := range m.Steps {
			if stop() {
				return errMacroStopped
			}
			at := func() {
				if s.X != nil && s.Y != nil {
					setCursor(*s.X, *s.Y)
				}
			}
			var err error
			switch {
			case s.Click != "":
				if s.X == nil || s.Y == nil {
					x, y := cursorPos()
					err = clickAt(x, y, s.Click, s.Count, s.Mods)
				} else {
					err = clickAt(*s.X, *s.Y, s.Click, s.Count, s.Mods)
				}
			case s.Down != "":
				at()
				err = mouseButton(s.Down, true)
			case s.Up != "":
				at()
				err = mouseButton(s.Up, false)
			case s.Move:
				at()
			case s.Scroll != 0:
				at()
				mouseWheel(s.Scroll)
			case s.Press != "":
				err = pressCombo(s.Press)
			case s.KeyDown != "":
				var v uint16
				if v, err = keyVK(s.KeyDown); err == nil {
					sendKey(v, false)
				}
			case s.KeyUp != "":
				var v uint16
				if v, err = keyVK(s.KeyUp); err == nil {
					sendKey(v, true)
				}
			case s.Type != "":
				typeText(s.Type)
			case s.WaitColor != "" && s.X != nil && s.Y != nil:
				err = waitForColor(*s.X, *s.Y, s.WaitColor, s.Tolerance, s.Timeout, stop)
			case s.Wait > 0:
				err = sleepStop(time.Duration(float64(s.Wait)/speed)*time.Millisecond, stop)
			}
			if err != nil {
				return err
			}
			time.Sleep(time.Duration(25/speed) * time.Millisecond)
		}
	}
	return nil
}

// ---------- colors ----------

type rgb struct{ r, g, b int }

func parseColor(s string) (rgb, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return rgb{}, fmt.Errorf("color must look like #1E88E5")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return rgb{}, fmt.Errorf("color must look like #1E88E5")
	}
	return rgb{int(v >> 16 & 0xFF), int(v >> 8 & 0xFF), int(v & 0xFF)}, nil
}

func (c rgb) String() string { return fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b) }

func near(a, b rgb, tol int) bool {
	return absInt(a.r-b.r) <= tol && absInt(a.g-b.g) <= tol && absInt(a.b-b.b) <= tol
}

func pixelAt(x, y int) (rgb, error) {
	px, err := grabArea(x, y, 1, 1)
	if err != nil {
		return rgb{}, err
	}
	return rgb{int(px[2]), int(px[1]), int(px[0])}, nil
}

func areaHas(px []byte, c rgb, tol int) bool {
	for i := 0; i+3 < len(px); i += 4 {
		if near(rgb{int(px[i+2]), int(px[i+1]), int(px[i])}, c, tol) {
			return true
		}
	}
	return false
}

// areaChanged reports whether enough of the area differs from before (any pixel for small areas).
func areaChanged(px, before []byte, tol int) bool {
	if len(px) != len(before) {
		return true
	}
	n, changed := len(px)/4, 0
	for i := 0; i+3 < len(px); i += 4 {
		if absInt(int(px[i])-int(before[i])) > tol || absInt(int(px[i+1])-int(before[i+1])) > tol || absInt(int(px[i+2])-int(before[i+2])) > tol {
			changed++
		}
	}
	return changed > 0 && float64(changed) >= math.Max(1, float64(n)*0.01)
}

func waitForColor(x, y int, color string, tol, timeoutMs int, stop func() bool) error {
	c, err := parseColor(color)
	if err != nil {
		return err
	}
	if timeoutMs <= 0 {
		timeoutMs = 10000
	}
	end := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(end) {
		if got, err := pixelAt(x, y); err == nil && near(got, c, tol) {
			return nil
		}
		if err := sleepStop(80*time.Millisecond, stop); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s didn't show at %d,%d within %ds", color, x, y, timeoutMs/1000)
}

// ---------- triggers ----------

type trigState struct {
	until   time.Time
	was     bool
	started bool
	base    []byte
}

type watcher struct {
	mu     sync.Mutex
	paused bool
	logf   func(string)
	quit   chan struct{}
}

func (w *watcher) setPaused(p bool) {
	w.mu.Lock()
	w.paused = p
	w.mu.Unlock()
}

func (w *watcher) isPaused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func startWatcher(logf func(string)) *watcher {
	w := &watcher{logf: logf, quit: make(chan struct{})}
	go w.run()
	return w
}

func (w *watcher) run() {
	state := map[string]*trigState{}
	var ts []Trigger
	var mod time.Time
	stopWas := false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-w.quit:
			return
		case <-tick.C:
		}
		if fi, err := os.Stat(triggersPath()); err == nil && !fi.ModTime().Equal(mod) {
			mod, ts = fi.ModTime(), loadTriggers()
		}
		s := stopKeys()
		if s && !stopWas {
			w.setPaused(!w.isPaused())
			if w.isPaused() {
				w.logf("triggers paused (Ctrl+Alt+Esc again or resume to start them)")
			} else {
				w.logf("triggers running again")
			}
		}
		stopWas = s
		if w.isPaused() {
			continue
		}
		for _, t := range ts {
			if !t.On {
				continue
			}
			st := state[t.Name]
			if st == nil {
				st = &trigState{}
				state[t.Name] = st
			}
			fire := false
			switch t.Kind {
			case "hotkey":
				keys, err := parseCombo(t.Hotkey)
				if err != nil {
					continue
				}
				held := comboHeld(keys)
				fire = held && !st.was
				st.was = held
			case "color":
				px, err := grabArea(t.X, t.Y, max(t.W, 1), max(t.H, 1))
				if err != nil {
					continue
				}
				if t.Color != "" {
					c, err := parseColor(t.Color)
					if err != nil {
						continue
					}
					has := areaHas(px, c, t.Tolerance)
					fire = has && !st.was
					st.was = has
				} else {
					if st.base == nil {
						st.base = px
						continue
					}
					if areaChanged(px, st.base, t.Tolerance) {
						fire = true
						st.base = px
					}
				}
			}
			if !fire || time.Now().Before(st.until) {
				continue
			}
			st.until = time.Now().Add(time.Duration(max(t.CooldownMs, 300)) * time.Millisecond)
			w.logf(w.fire(t))
		}
	}
}

func (w *watcher) fire(t Trigger) string {
	stop := func() bool { return stopKeys() || w.isPaused() }
	switch t.Do {
	case "macro":
		m, err := loadMacro(t.Macro)
		if err != nil {
			return t.Name + ": " + err.Error()
		}
		if err := playMacro(m, 1, 1, stop); err != nil {
			return fmt.Sprintf("%s: macro %s: %v", t.Name, m.Name, err)
		}
		return fmt.Sprintf("%s → played %s", t.Name, m.Name)
	default:
		x0, y0 := cursorPos()
		if err := clickAt(t.ClickX, t.ClickY, t.Button, 1, nil); err != nil {
			return t.Name + ": " + err.Error()
		}
		if t.MouseBack {
			time.Sleep(30 * time.Millisecond)
			setCursor(x0, y0)
		}
		b := t.Button
		if b == "" {
			b = "left"
		}
		return fmt.Sprintf("%s → %s click at %d,%d", t.Name, b, t.ClickX, t.ClickY)
	}
}

func triggerText(t Trigger) string {
	on := "off"
	if t.On {
		on = "on"
	}
	when := ""
	switch t.Kind {
	case "hotkey":
		when = "when you press " + t.Hotkey
	default:
		area := fmt.Sprintf("%d,%d", t.X, t.Y)
		if t.W > 1 || t.H > 1 {
			area = fmt.Sprintf("the %dx%d area at %d,%d", t.W, t.H, t.X, t.Y)
		}
		if t.Color != "" {
			when = fmt.Sprintf("when %s shows %s (±%d)", area, t.Color, t.Tolerance)
		} else {
			when = fmt.Sprintf("when %s changes (±%d)", area, t.Tolerance)
		}
	}
	do := ""
	switch t.Do {
	case "macro":
		do = "play macro " + t.Macro
	default:
		b := t.Button
		if b == "" {
			b = "left"
		}
		do = fmt.Sprintf("%s click at %d,%d", b, t.ClickX, t.ClickY)
		if t.MouseBack {
			do += ", mouse back"
		}
	}
	return fmt.Sprintf("%s [%s]: %s, %s", t.Name, on, when, do)
}
