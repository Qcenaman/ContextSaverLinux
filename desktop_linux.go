package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Launchers: Windows' .cmd/.lnk shortcuts become .desktop files, marked trusted so the
// desktop and the Files app run them on double-click.

// desktopArg quotes one Exec argument (Desktop Entry spec: quote, then escape \ again).
func desktopArg(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n\"'\\><~|&;$*?#()`=%") {
		return a
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`, `%`, `%%`)
	return `"` + r.Replace(a) + `"`
}

// writeLauncher writes a .desktop launcher. pause keeps its terminal open that many
// seconds after the command ends (like "timeout /t" in the Windows .cmd files).
func writeLauncher(path, name, comment, icon string, args []string, terminal bool, pause int) error {
	ex := make([]string, 0, len(args)+2)
	if pause > 0 {
		ex = append(ex, "env", "CONTEXTSAVER_PAUSE="+strconv.Itoa(pause))
	}
	for _, a := range args {
		ex = append(ex, desktopArg(a))
	}
	if icon == "" {
		icon = "utilities-terminal"
	}
	t := "false"
	if terminal {
		t = "true"
	}
	txt := "[Desktop Entry]\nType=Application\nVersion=1.0\nName=" + name + "\nComment=" + comment +
		"\nExec=" + strings.Join(ex, " ") + "\nIcon=" + icon + "\nTerminal=" + t + "\nCategories=Utility;\n"
	if old, err := os.ReadFile(path); err != nil || string(old) != txt {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(txt), 0o755); err != nil {
			return err
		}
	}
	os.Chmod(path, 0o755)
	quietCmd("gio", "set", path, "metadata::trusted", "true").Run()
	return nil
}

// pauseIfAsked: launchers set CONTEXTSAVER_PAUSE so their terminal stays readable.
func pauseIfAsked() {
	if n, _ := strconv.Atoi(os.Getenv("CONTEXTSAVER_PAUSE")); n > 0 {
		time.Sleep(time.Duration(n) * time.Second)
	}
}

// desktopEntryValue reads Key= from a .desktop file ("" when absent).
func desktopEntryValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// appIcon finds the icon of an installed app by its .desktop file name prefix.
func appIcon(prefixes ...string) string {
	h, _ := os.UserHomeDir()
	for _, d := range []string{filepath.Join(h, ".local", "share", "applications"), "/usr/share/applications"} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			low := strings.ToLower(e.Name())
			for _, p := range prefixes {
				if strings.HasPrefix(low, p) && strings.HasSuffix(low, ".desktop") {
					if v := desktopEntryValue(filepath.Join(d, e.Name()), "Icon"); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}
