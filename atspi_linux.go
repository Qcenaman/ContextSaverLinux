package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Windows' UI Automation on Linux: the accessibility tree (AT-SPI), read by a small Python
// script, works under Xorg and Wayland. It lists windows, gives a window's text in the
// same "x y h kind name value" lines as the Windows script, and clicks or fills a control
// by its name.

const atspiScript = `import sys
try:
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi
except Exception as e:
    print('E accessibility needs python3-gi and gir1.2-atspi-2.0 (contextsaver setup)'); sys.exit(1)

KIND = {'push button': 'Button', 'toggle button': 'Button', 'menu item': 'MenuItem', 'check menu item': 'MenuItem',
    'radio menu item': 'MenuItem', 'page tab': 'TabItem', 'link': 'Hyperlink', 'check box': 'CheckBox',
    'radio button': 'RadioButton', 'text': 'Edit', 'entry': 'Edit', 'password text': 'Edit', 'spin button': 'Edit',
    'label': 'Text', 'static': 'Text', 'paragraph': 'Text', 'heading': 'Text', 'caption': 'Text', 'list item': 'ListItem',
    'table cell': 'DataItem', 'tree item': 'TreeItem', 'combo box': 'ComboBox', 'frame': 'Window', 'window': 'Window',
    'dialog': 'Window', 'panel': 'Pane', 'filler': 'Pane', 'section': 'Group', 'scroll pane': 'Pane', 'document web': 'Pane',
    'document frame': 'Pane', 'list': 'List', 'table': 'Table', 'tree': 'Tree', 'tool bar': 'ToolBar', 'menu bar': 'MenuBar',
    'page tab list': 'Tab', 'scroll bar': 'ScrollBar', 'image': 'Image', 'separator': 'Separator', 'menu': 'MenuItem',
    'slider': 'Slider', 'progress bar': 'ProgressBar', 'status bar': 'StatusBar'}
TOP = ('frame', 'window', 'dialog', 'alert', 'file chooser')

def clean(s):
    return (s or '').replace('\t', ' ').replace('\n', ' ').replace('\r', ' ')

def windows():
    desk = Atspi.get_desktop(0)
    for i in range(desk.get_child_count()):
        app = desk.get_child_at_index(i)
        if app is None:
            continue
        try:
            pid = app.get_process_id()
        except Exception:
            pid = 0
        for j in range(app.get_child_count()):
            w = app.get_child_at_index(j)
            if w is None or w.get_role_name() not in TOP:
                continue
            st = w.get_state_set()
            try:
                e = w.get_extents(Atspi.CoordType.SCREEN)
                box = (e.x, e.y, e.width, e.height)
            except Exception:
                box = (0, 0, 0, 0)
            yield pid, j, box, st.contains(Atspi.StateType.ICONIFIED), st.contains(Atspi.StateType.ACTIVE), app.get_name() or '', w.get_name() or '', w

def find(pid, title, idx):
    best = None
    for p, j, box, mini, act, app, t, w in windows():
        if p != pid:
            continue
        if t == title:
            return w
        if best is None or j == idx:
            best = w
    if best is None:
        print('E that window is gone'); sys.exit(1)
    return best

def text_of(a):
    try:
        t = a.get_text_iface()
        if t is not None:
            n = Atspi.Text.get_character_count(t)
            if n > 0:
                return Atspi.Text.get_text(t, 0, min(n, 4000))
    except Exception:
        pass
    try:
        v = a.get_value_iface()
        if v is not None:
            return str(Atspi.Value.get_current_value(v))
    except Exception:
        pass
    return ''

def walk(a, depth, out, limit):
    if a is None or len(out) >= limit or depth > 60:
        return
    try:
        st = a.get_state_set()
        if not st.contains(Atspi.StateType.SHOWING) and depth > 1:
            return
        role = a.get_role_name()
        try:
            e = a.get_extents(Atspi.CoordType.SCREEN)
            x, y, h = e.x, e.y, e.height
        except Exception:
            x = y = h = 0
        out.append((x, y, h, KIND.get(role, 'Custom'), a.get_name() or '', text_of(a), a, role))
        for i in range(min(a.get_child_count(), 2000)):
            walk(a.get_child_at_index(i), depth + 1, out, limit)
    except Exception:
        return

def main():
    op = sys.argv[1]
    if op == 'list':
        for p, j, (x, y, w, h), mini, act, app, t, _ in windows():
            print('\t'.join(map(str, (p, j, x, y, w, h, int(mini), int(act), clean(app), clean(t)))))
        return
    pid, title, idx = int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
    win = find(pid, title, idx)
    if op == 'focus':
        try:
            c = win.get_component_iface()
            if c is not None:
                Atspi.Component.grab_focus(c)
        except Exception:
            pass
        print('OK'); return
    items = []
    walk(win, 0, items, int(sys.argv[5]) if op == 'text' and len(sys.argv) > 5 else 4000)
    if op == 'text':
        for x, y, h, k, n, v, _, _ in items:
            print('\t'.join(map(str, (x, y, h, k, clean(n), clean(v)))))
        return
    want, value = sys.argv[5], sys.argv[6] if len(sys.argv) > 6 else ''
    lw = want.lower()
    best, score = None, 0
    for x, y, h, k, n, v, a, role in items:
        ln = (n or '').lower()
        if not ln:
            continue
        s = 3 if ln == lw else (2 if lw in ln else 0)
        if not s:
            continue
        if op == 'set' and k == 'Edit':
            s += 2
        if op == 'click' and k in ('Button', 'MenuItem', 'Hyperlink', 'TabItem', 'CheckBox', 'RadioButton', 'ListItem', 'TreeItem'):
            s += 2
        if s > score:
            best, score = a, s
    if best is None and op == 'set':  # a field labelled by a neighbouring label
        for i, it in enumerate(items):
            if lw in (it[4] or '').lower():
                for nxt in items[i + 1:i + 6]:
                    if nxt[3] == 'Edit':
                        best = nxt[6]; break
            if best is not None:
                break
    if best is None:
        print('E nothing called "%s" in that window' % want); sys.exit(1)
    try:
        e = best.get_extents(Atspi.CoordType.SCREEN)
        rect = 'RECT %d %d %d %d' % (e.x, e.y, e.width, e.height)
    except Exception:
        rect = 'E it has no position to click'
    if op == 'set':
        try:
            et = best.get_editable_text_iface()
            if et is not None:
                c = best.get_component_iface()
                if c is not None:
                    Atspi.Component.grab_focus(c)
                Atspi.EditableText.set_text_contents(et, value)
                print('OK set'); return
        except Exception:
            pass
        print(rect); return
    try:
        act = best.get_action_iface()
        if act is not None:
            n = Atspi.Action.get_n_actions(act)
            names = [Atspi.Action.get_action_name(act, i).lower() for i in range(n)]
            for want_a in ('click', 'press', 'activate', 'toggle', 'jump', 'open'):
                if want_a in names:
                    Atspi.Action.do_action(act, names.index(want_a))
                    print('OK invoked'); return
            if n > 0:
                Atspi.Action.do_action(act, 0)
                print('OK invoked'); return
    except Exception:
        pass
    print(rect)

main()
`

func atspiScriptPath() (string, error) {
	p := filepath.Join(appHome, "scripts", "atspi.py")
	if b, err := os.ReadFile(p); err == nil && string(b) == atspiScript {
		return p, nil
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	return p, os.WriteFile(p, []byte(atspiScript), 0o644)
}

// atspiCmd: the script for op on window w (list needs no window).
func atspiCmd(ctx context.Context, op string, w *winInfo, extra ...string) (*exec.Cmd, error) {
	p, err := atspiScriptPath()
	if err != nil {
		return nil, err
	}
	args := []string{p, op}
	if w != nil {
		args = append(args, strconv.Itoa(w.pid), w.title, strconv.Itoa(int(w.hwnd&0xffff)))
	}
	c := exec.CommandContext(ctx, "python3", append(args, extra...)...)
	c.Env = append(os.Environ(), "NO_AT_BRIDGE=0")
	return c, nil
}

// atspiWindows lists windows through accessibility (Wayland, or no wmctrl).
func atspiWindows() []winInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := atspiCmd(ctx, "list", nil)
	if err != nil {
		return nil
	}
	out, _ := c.Output()
	var ws []winInfo
	atspiActive = 0
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 10 {
			continue
		}
		n := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
		w := winInfo{hwnd: uintptr(n(0))<<16 | uintptr(n(1)&0xffff), pid: n(0), x: n(2), y: n(3), w: n(4), h: n(5),
			minimized: f[6] == "1", title: f[9]}
		w.exe = procExe(w.pid)
		if w.exe == "" {
			w.exe = f[8]
		}
		if f[7] == "1" {
			atspiActive = w.hwnd
		}
		ws = append(ws, w)
	}
	return ws
}

var atspiActive uintptr

func procExe(pid int) string {
	if e, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		return filepath.Base(strings.TrimSuffix(e, " (deleted)"))
	}
	return ""
}

// atspiOutput runs op on w and returns the script's output.
func atspiOutput(op string, w winInfo, timeout time.Duration, extra ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := atspiCmd(ctx, op, &w, extra...)
	if err != nil {
		return "", err
	}
	out, err := c.Output()
	if ctx.Err() != nil {
		return "", errors.New("accessibility took too long")
	}
	if s := strings.TrimSpace(string(out)); strings.HasPrefix(s, "E ") && !strings.Contains(s, "\n") {
		return "", errors.New(s[2:])
	}
	return string(out), err
}
