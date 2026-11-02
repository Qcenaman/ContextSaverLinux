package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// openLinux starts what "open" names the way the desktop would: a URL, file or folder in
// its default app (xdg-open), a program on PATH or by full path, else an installed app
// by its menu name (gtk-launch).
func openLinux(t, args string) (string, error) {
	low := strings.ToLower(t)
	start := func(c *exec.Cmd) error {
		detach(c, false)
		c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
		if err := c.Start(); err != nil {
			return err
		}
		c.Process.Release()
		return nil
	}
	if strings.Contains(low, "://") || strings.HasPrefix(low, "mailto:") {
		return "opened " + t, start(exec.Command("xdg-open", t))
	}
	if p, err := expandPath(t); err == nil && exists(p) {
		fi, _ := os.Stat(p)
		if fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 && !strings.HasSuffix(low, ".desktop") {
			return "started " + p, start(exec.Command(p, strings.Fields(args)...))
		}
		if strings.HasSuffix(low, ".desktop") {
			return "started " + p, start(exec.Command("gio", "launch", p))
		}
		return "opened " + p, start(exec.Command("xdg-open", p))
	}
	if p, err := exec.LookPath(t); err == nil {
		return "started " + t, start(exec.Command(p, strings.Fields(args)...))
	}
	if id := desktopID(t); id != "" {
		return "started " + id, start(exec.Command("gtk-launch", id))
	}
	return "", fmt.Errorf("no app, file or URL called %s", t)
}

// desktopID finds an installed app's menu entry by file name or Name=.
func desktopID(name string) string {
	h, _ := os.UserHomeDir()
	want := strings.ToLower(strings.TrimSuffix(name, ".desktop"))
	var byName string
	for _, d := range []string{filepath.Join(h, ".local", "share", "applications"), "/usr/share/applications",
		"/var/lib/snapd/desktop/applications", "/var/lib/flatpak/exports/share/applications"} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			n := e.Name()
			if !strings.HasSuffix(n, ".desktop") {
				continue
			}
			id := strings.TrimSuffix(n, ".desktop")
			low := strings.ToLower(id)
			if low == want || strings.HasSuffix(low, "."+want) || strings.HasSuffix(low, "_"+want) {
				return id
			}
			if byName == "" && strings.EqualFold(desktopEntryValue(filepath.Join(d, n), "Name"), name) {
				byName = id
			}
		}
	}
	return byName
}

// openInEditor opens a text file in the desktop's editor ($VISUAL/$EDITOR in a terminal, else xdg-open).
func openInEditor(p string) error {
	for _, v := range []string{os.Getenv("VISUAL"), os.Getenv("EDITOR")} {
		if v != "" {
			c := exec.Command(v, p)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		}
	}
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return errors.New("no editor found")
	}
	return exec.Command("xdg-open", p).Start()
}
