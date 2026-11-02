package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal: f is an interactive terminal (a person can answer), not a pipe, a file
// or /dev/null (which is a character device too, so file modes can't tell).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}

// termSize: rows and columns of terminal f (0, 0 when it isn't one).
func termSize(f *os.File) (rows, cols int) {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0
	}
	return int(ws.Row), int(ws.Col)
}
