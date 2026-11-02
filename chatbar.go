package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// The chat's command bar: the terminal's last line, kept out of the scrolling area so
// the commands stay visible while the conversation scrolls above it.

var barMu sync.Mutex

func (u *chatUI) barText(cols int) string {
	m := filepath.Base(u.model)
	if m == "" || m == "." {
		m = "no model"
	}
	t := fmt.Sprintf(" %s %dK │ /help = all commands │ Ctrl+C stops an answer ", m, u.ctx/1024)
	if r := []rune(t); len(r) > cols {
		t = string(r[:max(cols-1, 0)])
	}
	return t + strings.Repeat(" ", max(cols-len([]rune(t)), 0))
}

// drawBar (re)sets the scrolling area to all lines but the last and paints the bar there.
func (u *chatUI) drawBar() {
	if !u.color {
		return
	}
	rows, cols := termSize(os.Stdout)
	if rows < 4 || cols < 20 {
		return
	}
	barMu.Lock()
	defer barMu.Unlock()
	fmt.Printf("\x1b7\x1b[1;%dr\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", rows-1, rows, u.barText(cols))
}

// startBar clears the window, pins the bar and keeps it right when the window is resized.
func (u *chatUI) startBar() func() {
	if !u.color {
		return func() {}
	}
	fmt.Print("\x1b[2J\x1b[H")
	u.drawBar()
	fmt.Print("\x1b[H")
	w := make(chan os.Signal, 1)
	signal.Notify(w, syscall.SIGWINCH)
	go func() {
		for range w {
			u.drawBar()
		}
	}()
	return func() {
		signal.Stop(w)
		rows, _ := termSize(os.Stdout)
		fmt.Printf("\x1b7\x1b[r\x1b[%d;1H\x1b[2K\x1b8", rows)
	}
}
