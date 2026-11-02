package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ContextSaver terminal: what opens when you double-click ContextSaver. You make
// macros and triggers here, pick spots and colors on screen, run saved jobs, and call
// any ContextSaver tool yourself. Triggers run while the terminal is open.

const termHelp = `Commands (Ctrl+Alt+Esc stops a macro and pauses triggers at any time):
  macro record NAME          record your clicks and keys; F9 stops
  macro new NAME             build a macro step by step (point with the mouse, F8 marks)
  macro play NAME [times] [speed]
  macro list | macro show NAME | macro edit NAME | macro remove NAME
  pick                       mouse position and the color under it; F8 notes a spot
  trigger new                a hotkey or a color on screen that clicks or plays a macro
  trigger list | trigger on NAME | trigger off NAME | trigger remove NAME
  pause | resume             all triggers
  jobs | job NAME            your saved jobs
  tool NAME {json}           call a ContextSaver tool, e.g. tool scan {"path":"D:\\Downloads"}
  reconnect | disconnect     connect ContextSaver to Claude desktop, or not
  helper | helper remove     install/restart or remove the admin helper
  version | help | exit`

var termOut sync.Mutex

func say(format string, a ...any) {
	termOut.Lock()
	fmt.Printf(format+"\n", a...)
	termOut.Unlock()
}

func termAsk(q string) (string, bool) {
	termOut.Lock()
	fmt.Print(q)
	termOut.Unlock()
	s, err := stdin.ReadString('\n')
	if err != nil && s == "" {
		return "", false
	}
	return strings.TrimSpace(s), true
}

func ask(q string) string {
	s, _ := termAsk(q)
	return s
}

func terminal() {
	dpiAware()
	say("\nContextSaver %s terminal. Type help for commands.", appVersion)
	say("Admin helper: %s · saved jobs: %d · macros: %d", helperStatus(), len(listSaved()), len(listMacros()))
	on := 0
	for _, t := range loadTriggers() {
		if t.On {
			on++
		}
	}
	if on > 0 {
		say("%d trigger(s) on; they run while this window is open (minimize it if you like).", on)
	}
	w := startWatcher(func(s string) { say("\n[%s] %s", time.Now().Format("15:04:05"), s) })
	defer close(w.quit)
	for {
		line, ok := termAsk("> ")
		if !ok {
			return
		}
		if line == "" {
			continue
		}
		cmd, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToLower(cmd) {
		case "exit", "quit", "q":
			return
		case "help", "?":
			say("%s", termHelp)
		case "version":
			say("contextsaver %s", appVersion)
		case "macro", "macros":
			termMacro(rest)
		case "pick":
			termPick()
		case "trigger", "triggers":
			termTrigger(rest)
		case "pause":
			w.setPaused(true)
			say("Triggers paused.")
		case "resume":
			w.setPaused(false)
			say("Triggers running.")
		case "jobs":
			termJobs()
		case "job":
			termRunJob(rest)
		case "tool":
			termTool(rest)
		case "reconnect":
			consoleInstall()
		case "disconnect":
			consoleUninstall()
		case "helper":
			if strings.EqualFold(rest, "remove") {
				consoleUninstallHelper()
			} else {
				consoleInstallHelper()
			}
		default:
			if n, err := strconv.Atoi(cmd); err == nil {
				termRunJob(strconv.Itoa(n))
				continue
			}
			say("Unknown command %q; type help.", cmd)
		}
	}
}

// ---------- pointing ----------

func waitRelease(vk uint16) {
	for keyHeld(vk) {
		time.Sleep(20 * time.Millisecond)
	}
}

// markSpot shows the mouse position and color live until F8 (marks it) or Esc (cancels).
func markSpot(what string) (int, int, rgb, bool) {
	say("  Point the mouse at %s and press F8 (Esc cancels).", what)
	waitRelease(0x77)
	waitRelease(0x1B)
	for {
		x, y := cursorPos()
		c, _ := pixelAt(x, y)
		termOut.Lock()
		fmt.Printf("\r  %5d,%-5d %s   ", x, y, c)
		termOut.Unlock()
		switch {
		case keyHeld(0x77):
			waitRelease(0x77)
			say("")
			return x, y, c, true
		case keyHeld(0x1B):
			waitRelease(0x1B)
			say("\r  canceled              ")
			return 0, 0, rgb{}, false
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func termPick() {
	if !inputSupported {
		say("%v", errNoInput)
		return
	}
	say("Move the mouse; F8 notes the spot under it. Press Enter here when done.")
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			x, y := cursorPos()
			c, _ := pixelAt(x, y)
			if keyHeld(0x77) {
				waitRelease(0x77)
				say("\r  noted %d,%d %s          ", x, y, c)
			}
			termOut.Lock()
			fmt.Printf("\r  %5d,%-5d %s   ", x, y, c)
			termOut.Unlock()
			time.Sleep(40 * time.Millisecond)
		}
	}()
	stdin.ReadString('\n')
	close(done)
	time.Sleep(60 * time.Millisecond)
	say("")
}

// ---------- macros ----------

// nameAndNumbers splits "my macro 3 1.5" into the name and trailing numbers.
func nameAndNumbers(s string) (string, []float64) {
	f := strings.Fields(s)
	var nums []float64
	for len(f) > 1 {
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			break
		}
		nums = append([]float64{v}, nums...)
		f = f[:len(f)-1]
	}
	return strings.Trim(strings.Join(f, " "), `"`), nums
}

func termMacro(rest string) {
	sub, arg, _ := strings.Cut(rest, " ")
	arg = strings.Trim(strings.TrimSpace(arg), `"`)
	switch strings.ToLower(sub) {
	case "", "list":
		ms := listMacros()
		if len(ms) == 0 {
			say("No macros yet. Make one with macro record NAME or macro new NAME.")
		}
		for _, m := range ms {
			say("  %s (%d steps): %s", m.Name, len(m.Steps), macroSummary(m))
		}
		say("Macros are in %s", macrosDir())
	case "record":
		if err := validName(arg); err != nil {
			say("%v", err)
			return
		}
		if !inputSupported {
			say("%v", errNoInput)
			return
		}
		say("Recording %q: do the clicks and keys now, then press F9 to stop.", arg)
		evs, err := recordInput(0x78)
		if err != nil {
			say("%v", err)
			return
		}
		m := &Macro{Name: arg, Steps: compactRecording(evs), Created: time.Now()}
		if len(m.Steps) == 0 {
			say("Nothing was recorded.")
			return
		}
		if err := saveMacro(m); err != nil {
			say("%v", err)
			return
		}
		say("Saved %q, %d steps: %s", m.Name, len(m.Steps), macroSummary(m))
		say("Play it with: macro play %s   (edit it with: macro edit %s)", m.Name, m.Name)
	case "new":
		termMacroNew(arg)
	case "play", "run":
		name, nums := nameAndNumbers(arg)
		m, err := loadMacro(name)
		if err != nil {
			say("%v", err)
			return
		}
		times, speed := 1, 1.0
		if len(nums) > 0 {
			times = max(int(nums[0]), 1)
		}
		if len(nums) > 1 && nums[1] > 0 {
			speed = nums[1]
		}
		say("Playing %q in 2 seconds (Ctrl+Alt+Esc stops)...", m.Name)
		if err := sleepStop(2*time.Second, stopKeys); err != nil {
			say("Stopped.")
			return
		}
		st := time.Now()
		if err := playMacro(m, times, speed, stopKeys); err != nil {
			say("%v", err)
			return
		}
		say("Done in %s.", hd(time.Since(st)))
	case "show":
		m, err := loadMacro(arg)
		if err != nil {
			say("%v", err)
			return
		}
		for i, s := range m.Steps {
			say("  %d. %s", i+1, stepText(s))
		}
	case "edit":
		m, err := loadMacro(arg)
		if err != nil {
			say("%v", err)
			return
		}
		if err := openInEditor(macroPath(m.Name)); err != nil {
			say("Open %s in any text editor.", macroPath(m.Name))
		}
	case "remove", "delete":
		m, err := loadMacro(arg)
		if err != nil {
			say("%v", err)
			return
		}
		for _, t := range loadTriggers() {
			if t.Do == "macro" && strings.EqualFold(t.Macro, m.Name) {
				say("Trigger %q plays this macro; change or remove it first.", t.Name)
				return
			}
		}
		if err := recycleBatch([]string{macroPath(m.Name)}); err != nil {
			say("%v", err)
			return
		}
		say("Moved %q to the Trash.", m.Name)
	default:
		say("macro record|new|play|list|show|edit|remove NAME")
	}
}

func termMacroNew(name string) {
	if err := validName(name); err != nil {
		say("%v", err)
		return
	}
	if !inputSupported {
		say("%v", errNoInput)
		return
	}
	say(`Add steps, one per line; an empty line saves the macro:
  c, r, m, d   left, right, middle or double click at a spot (point the mouse, F8)
  k KEYS       press keys, e.g. k ctrl+s or k enter
  t TEXT       type text
  w SECONDS    wait, e.g. w 1.5
  s N          scroll N notches (+ up, - down) at a spot (point, F8)
  u            wait until a spot shows its current color (point, F8)
  x            remove the last step`)
	m := &Macro{Name: name, Created: time.Now()}
	for {
		line, ok := termAsk("  step> ")
		if !ok || line == "" {
			break
		}
		op, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		var st MacroStep
		switch strings.ToLower(op) {
		case "c", "r", "m", "d":
			x, y, _, ok := markSpot("the spot to click")
			if !ok {
				continue
			}
			st = MacroStep{Click: map[string]string{"c": "left", "r": "right", "m": "middle", "d": "left"}[op], X: ip(x), Y: ip(y)}
			if op == "d" {
				st.Count = 2
			}
		case "k":
			if _, err := parseCombo(arg); err != nil {
				say("  %v", err)
				continue
			}
			st = MacroStep{Press: strings.ToLower(arg)}
		case "t":
			if arg == "" {
				continue
			}
			st = MacroStep{Type: arg}
		case "w":
			v, err := strconv.ParseFloat(arg, 64)
			if err != nil || v <= 0 {
				say("  w needs seconds, e.g. w 1.5")
				continue
			}
			st = MacroStep{Wait: int(v * 1000)}
		case "s":
			n, err := strconv.Atoi(arg)
			if err != nil || n == 0 {
				say("  s needs notches, e.g. s -3")
				continue
			}
			x, y, _, ok := markSpot("where to scroll")
			if !ok {
				continue
			}
			st = MacroStep{Scroll: n, X: ip(x), Y: ip(y)}
		case "u":
			x, y, c, ok := markSpot("the spot to wait for")
			if !ok {
				continue
			}
			st = MacroStep{WaitColor: c.String(), X: ip(x), Y: ip(y), Tolerance: 25, Timeout: 15000}
		case "x":
			if len(m.Steps) > 0 {
				m.Steps = m.Steps[:len(m.Steps)-1]
				say("  removed the last step")
			}
			continue
		default:
			say("  unknown step; use c r m d k t w s u x")
			continue
		}
		m.Steps = append(m.Steps, st)
		say("  %d. %s", len(m.Steps), stepText(st))
	}
	if len(m.Steps) == 0 {
		say("No steps; nothing saved.")
		return
	}
	if err := saveMacro(m); err != nil {
		say("%v", err)
		return
	}
	say("Saved %q (%d steps). Play it with: macro play %s", m.Name, len(m.Steps), m.Name)
}

// ---------- triggers ----------

func findTrigger(ts []Trigger, name string) int {
	for i, t := range ts {
		if strings.EqualFold(t.Name, name) {
			return i
		}
	}
	return -1
}

func termTrigger(rest string) {
	sub, arg, _ := strings.Cut(rest, " ")
	arg = strings.Trim(strings.TrimSpace(arg), `"`)
	ts := loadTriggers()
	switch strings.ToLower(sub) {
	case "", "list":
		if len(ts) == 0 {
			say("No triggers yet. Make one with trigger new.")
		}
		for _, t := range ts {
			say("  %s", triggerText(t))
		}
	case "new", "add":
		termTriggerNew()
	case "on", "off":
		i := findTrigger(ts, arg)
		if i < 0 {
			say("No trigger called %q.", arg)
			return
		}
		ts[i].On = strings.EqualFold(sub, "on")
		if err := saveTriggers(ts); err != nil {
			say("%v", err)
			return
		}
		say("  %s", triggerText(ts[i]))
	case "remove", "delete":
		i := findTrigger(ts, arg)
		if i < 0 {
			say("No trigger called %q.", arg)
			return
		}
		ts = append(ts[:i], ts[i+1:]...)
		if err := saveTriggers(ts); err != nil {
			say("%v", err)
			return
		}
		say("Removed %q.", arg)
	default:
		say("trigger new|list|on|off|remove NAME")
	}
}

func termTriggerNew() {
	if !inputSupported {
		say("%v", errNoInput)
		return
	}
	ts := loadTriggers()
	name := ask("Name for the trigger: ")
	if err := validName(name); err != nil {
		say("%v", err)
		return
	}
	t := Trigger{Name: name, On: true, CooldownMs: 2000}
	if strings.TrimSpace(ask("What starts it? 1 = a color on screen, 2 = a hotkey [1]: ")) == "2" {
		t.Kind = "hotkey"
		t.Hotkey = strings.ToLower(ask("Hotkey, e.g. ctrl+alt+1: "))
		if _, err := parseCombo(t.Hotkey); err != nil {
			say("%v", err)
			return
		}
	} else {
		t.Kind = "color"
		x, y, c, ok := markSpot("the spot to watch")
		if !ok {
			return
		}
		t.X, t.Y, t.W, t.H = x, y, 1, 1
		if a := strings.ToLower(ask("Watch just that pixel (Enter), or an area around it, like 40x20: ")); a != "" {
			ws, hs, _ := strings.Cut(a, "x")
			w, e1 := strconv.Atoi(strings.TrimSpace(ws))
			h, e2 := strconv.Atoi(strings.TrimSpace(hs))
			if e1 != nil || e2 != nil || w < 1 || h < 1 || w*h > 1000*1000 {
				say("Area must look like 40x20.")
				return
			}
			t.X, t.Y, t.W, t.H = x-w/2, y-h/2, w, h
		}
		switch ask(fmt.Sprintf("Fire when: 1 = it shows %s (the color there now), 2 = it shows another color, 3 = it changes from how it looks now [1]: ", c)) {
		case "2":
			col, err := parseColor(ask("Color, like #1E88E5: "))
			if err != nil {
				say("%v", err)
				return
			}
			t.Color = col.String()
		case "3":
			t.Color = ""
		default:
			t.Color = c.String()
		}
		tol := 10
		if v, err := strconv.Atoi(ask("How exact? 0 = exact color, 100 = anything [10]: ")); err == nil && v >= 0 && v <= 100 {
			tol = v
		}
		t.Tolerance = tol * 255 / 100
	}
	if ask("Then: 1 = click a spot, 2 = play a macro [1]: ") == "2" {
		t.Do = "macro"
		t.Macro = ask("Macro name: ")
		if _, err := loadMacro(t.Macro); err != nil {
			say("%v", err)
			return
		}
	} else {
		t.Do = "click"
		if t.Kind == "color" && strings.ToLower(ask("Click the watched spot (Enter), or point somewhere else (p)? ")) != "p" {
			t.ClickX, t.ClickY = t.X+t.W/2, t.Y+t.H/2
		} else {
			x, y, _, ok := markSpot("where to click")
			if !ok {
				return
			}
			t.ClickX, t.ClickY = x, y
		}
		switch b := strings.ToLower(ask("left, right, middle or double click [left]: ")); b {
		case "right", "middle", "double":
			t.Button = b
		}
		t.MouseBack = askYes("Put the mouse back where it was after clicking? [Y/n] ", true)
	}
	if v, err := strconv.ParseFloat(ask("Seconds before it can fire again [2]: "), 64); err == nil && v >= 0.3 {
		t.CooldownMs = int(v * 1000)
	}
	if i := findTrigger(ts, t.Name); i >= 0 {
		ts[i] = t
	} else {
		ts = append(ts, t)
	}
	if err := saveTriggers(ts); err != nil {
		say("%v", err)
		return
	}
	say("Saved: %s", triggerText(t))
	say("It runs while this window is open (minimize it if you like). Ctrl+Alt+Esc pauses all triggers.")
}

// ---------- jobs and tools ----------

func termJobs() {
	list := listSaved()
	if len(list) == 0 {
		say("No saved jobs yet. Ask Claude to save one.")
		return
	}
	for i, s := range list {
		say("  %d. %s: %s", i+1, s.Name, clip(describeSteps(s.Steps), 90))
	}
	say("Run one with: job NAME (or just its number)")
}

func termRunJob(arg string) {
	list := listSaved()
	name := arg
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(list) {
		name = list[n-1].Name
	}
	sj, err := loadSaved(name)
	if err != nil {
		say("%v", err)
		return
	}
	if hasRisky(sj.Steps) {
		s := runConsoleJob(JobSpec{Name: sj.Name, Kind: "files", Steps: sj.Steps, DryRun: true, Source: "you"})
		say("%s", formatState(s))
		if s.Status != "done" || !askYes("Go ahead? [y/N] ", false) {
			return
		}
	}
	say("%s", formatState(runConsoleJob(JobSpec{Name: sj.Name, Kind: "files", Steps: sj.Steps, Source: "you"})))
}

func termTool(rest string) {
	name, args, _ := strings.Cut(rest, " ")
	args = strings.TrimSpace(args)
	if name == "" {
		say("tool NAME {json}; tools: scan find read run_job job saved local_ai run system web")
		return
	}
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		say("The arguments must be JSON, e.g. tool find {\"path\":[\"D:\\\\\"],\"name\":\"*.pdf\"}")
		return
	}
	out, imgs, err := callTool(name, json.RawMessage(args))
	if err != nil {
		say("%v", err)
		return
	}
	say("%s", out)
	for _, im := range imgs {
		say("(plus an image, %d KB)", len(im.data)>>10)
	}
}

// runMacroCLI plays a macro from the command line (contextsaver macro NAME [times]).
func runMacroCLI(args []string) int {
	dpiAware()
	if len(args) == 0 {
		fmt.Println(`usage: contextsaver macro "NAME" [times] [speed]`)
		return 2
	}
	name, nums := nameAndNumbers(strings.Join(args, " "))
	m, err := loadMacro(name)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	times, speed := 1, 1.0
	if len(nums) > 0 {
		times = max(int(nums[0]), 1)
	}
	if len(nums) > 1 && nums[1] > 0 {
		speed = nums[1]
	}
	if err := playMacro(m, times, speed, stopKeys); err != nil {
		fmt.Println(err)
		return 1
	}
	return 0
}
