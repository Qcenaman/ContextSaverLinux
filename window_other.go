//go:build !windows

package main

func listWindowsRaw() []winInfo                { return nil }
func foregroundHwnd() uintptr                  { return 0 }
func captureWindow(w winInfo) ([]byte, error)  { return nil, errNoInput }
func copyWindowText(w winInfo) (string, error) { return "", errNoInput }
func focusWindow(w winInfo) error              { return errNoInput }

func pasteText(s string) error { return errNoInput }

func clipText() (string, bool)   { return "", false }
func setClipText(s string) error { return errNoInput }
