package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// window do: click buttons by name, fill fields, press keys, several steps per call.

const uiaDoScript = `param([long]$Hwnd, [string]$Op, [string]$Name, [string]$Value = '')
$ErrorActionPreference = 'Stop'
$out = New-Object System.IO.StreamWriter([Console]::OpenStandardOutput(), (New-Object System.Text.UTF8Encoding $false))
function Say($s) { $out.WriteLine($s); $out.Flush() }
try {
  Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes
  $A = [System.Windows.Automation.AutomationElement]
  $root = $A::FromHandle([IntPtr]$Hwnd)
  $all = $null
  for ($i = 0; $i -lt 3; $i++) { $all = $root.FindAll([System.Windows.Automation.TreeScope]::Descendants, [System.Windows.Automation.Condition]::TrueCondition); if ($all.Count -gt 5) { break }; Start-Sleep -Milliseconds 600 }
  $want = $Name.Trim().ToLower()
  $best = $null; $score = 0
  foreach ($e in $all) {
    try {
      $c = $e.Current
      if ($c.IsOffscreen -or -not $c.IsEnabled) { continue }
      $n = ($c.Name -replace '\s+', ' ').Trim().ToLower()
      $id = ([string]$c.AutomationId).ToLower()
      $t = $c.ControlType.ProgrammaticName
      $s = 0
      if ($n -eq $want -or $id -eq $want) { $s = 3 } elseif ($n.Contains($want)) { $s = 2 } else { continue }
      $isEdit = $t -match 'Edit|ComboBox|Document'
      if ($Op -eq 'set' -and $isEdit) { $s += 2 }
      if ($Op -eq 'click' -and $t -match 'Button|MenuItem|Hyperlink|TabItem|CheckBox|RadioButton|ListItem|TreeItem') { $s += 2 }
      if ($s -gt $score) { $best = $e; $score = $s }
    } catch { }
  }
  if ($null -eq $best) { Say ('E nothing called "' + $Name + '" in that window'); exit 1 }
  $r = $best.Current.BoundingRectangle
  $p = $null
  if ($Op -eq 'set') {
    if ($best.TryGetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern, [ref]$p)) { $best.SetFocus(); $p.SetValue($Value); Say 'OK set'; exit 0 }
    Say ('RECT {0} {1} {2} {3}' -f [int]$r.Left, [int]$r.Top, [int]$r.Width, [int]$r.Height); exit 0
  }
  if ($best.TryGetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern, [ref]$p)) { $p.Invoke(); Say 'OK invoked'; exit 0 }
  if ($best.TryGetCurrentPattern([System.Windows.Automation.TogglePattern]::Pattern, [ref]$p)) { $p.Toggle(); Say 'OK toggled'; exit 0 }
  if ($best.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$p)) { $p.Select(); Say 'OK selected'; exit 0 }
  Say ('RECT {0} {1} {2} {3}' -f [int]$r.Left, [int]$r.Top, [int]$r.Width, [int]$r.Height)
} catch { Say ('E ' + ($_.Exception.Message -replace '\s+', ' ')); exit 1 }
`

type uiStep struct {
	Click     string  `json:"click"`
	Type      string  `json:"type"`
	Into      string  `json:"into"`
	Press     string  `json:"press"`
	Wait      float64 `json:"wait"`
	ClickText string  `json:"click_text"`
	Nth       int     `json:"nth"`
	ClickAt   []int   `json:"click_at"`
	Button    string  `json:"button"`
	Scroll    int     `json:"scroll"`
	At        []int   `json:"at"`
	Drag      []int   `json:"drag"`
	Move      []int   `json:"move"`
	Hold      string  `json:"hold"`
	Secs      float64 `json:"secs"`
	Screen    bool    `json:"screen"`
}

func uiaDo(w winInfo, op, name, value string) (string, error) {
	script := filepath.Join(appHome, "scripts", "uiado.ps1")
	if b, err := os.ReadFile(script); err != nil || string(b) != uiaDoScript {
		os.MkdirAll(filepath.Dir(script), 0o755)
		if err := os.WriteFile(script, []byte(uiaDoScript), 0o644); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := quietCmd(psExe(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Hwnd", strconv.FormatInt(int64(w.hwnd), 10), "-Op", op, "-Name", name, "-Value", value)
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()
	out, _ := cmd.Output()
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	switch {
	case strings.HasPrefix(line, "OK"):
		return strings.TrimPrefix(line, "OK "), nil
	case strings.HasPrefix(line, "RECT "):
		f := strings.Fields(line[5:])
		if len(f) == 4 {
			x, _ := strconv.Atoi(f[0])
			y, _ := strconv.Atoi(f[1])
			ww, _ := strconv.Atoi(f[2])
			hh, _ := strconv.Atoi(f[3])
			if err := focusWindow(w); err != nil {
				return "", err
			}
			if err := clickAt(x+ww/2, y+hh/2, "left", 1, nil); err != nil {
				return "", err
			}
			if op == "set" {
				time.Sleep(80 * time.Millisecond)
				pressCombo("ctrl+a")
				return "typed", pasteText(value)
			}
			return "clicked", nil
		}
	case strings.HasPrefix(line, "E "):
		return "", errors.New(line[2:])
	}
	return "", errors.New("UI Automation gave no answer")
}

// windowDo runs steps in a window and reports each in a few words.
func windowDo(w winInfo, raw json.RawMessage) (string, error) {
	if strings.EqualFold(strings.TrimSuffix(w.exe, ".exe"), "claude") {
		return "", errors.New("ContextSaver doesn't click or type in Claude's own windows")
	}
	var steps []uiStep
	if err := json.Unmarshal(raw, &steps); err != nil || len(steps) == 0 {
		return "", errors.New(`steps: e.g. [{"click":"Save"},{"type":"hello","into":"File name"},{"press":"enter"},{"wait":1}]`)
	}
	var log []string
	for i, s := range steps {
		var r string
		var err error
		if sr, ok, serr := screenStep(w, s); ok {
			r, err = sr, serr
		} else {
			switch {
			case s.Click != "":
				r, err = uiaDo(w, "click", s.Click, "")
				r = s.Click + ": " + r
			case s.Type != "" && s.Into != "":
				r, err = uiaDo(w, "set", s.Into, s.Type)
				r = s.Into + ": " + r
			case s.Type != "":
				if err = focusWindow(w); err == nil {
					err = pasteText(s.Type)
					r = "typed"
				}
			case s.Press != "":
				if err = focusWindow(w); err == nil {
					err = pressCombo(s.Press)
					r = "pressed " + s.Press
				}
			case s.Wait > 0:
				time.Sleep(time.Duration(min(s.Wait, 30) * float64(time.Second)))
				r = fmt.Sprintf("waited %gs", s.Wait)
			default:
				err = errors.New("empty step")
			}
		}
		if err != nil {
			log = append(log, fmt.Sprintf("%d. failed: %v", i+1, err))
			return strings.Join(log, "\n"), nil
		}
		log = append(log, fmt.Sprintf("%d. %s", i+1, r))
		time.Sleep(120 * time.Millisecond)
	}
	return strings.Join(log, "\n"), nil
}
