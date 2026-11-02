//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func detach(c *exec.Cmd, _ bool) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
