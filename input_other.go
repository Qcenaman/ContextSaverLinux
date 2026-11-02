//go:build !windows

package main

import "errors"

const inputSupported = false

var errNoInput = errors.New("macros and screen reading need Windows")

func dpiAware()                                     {}
func cursorPos() (int, int)                         { return 0, 0 }
func setCursor(x, y int)                            {}
func mouseButton(button string, down bool) error    { return errNoInput }
func mouseWheel(notches int)                        {}
func sendKey(vk uint16, up bool)                    {}
func typeText(s string)                             {}
func keyHeld(vk uint16) bool                        { return false }
func grabArea(x, y, w, h int) ([]byte, error)       { return nil, errNoInput }
func recordInput(stopVK uint16) ([]rawInput, error) { return nil, errNoInput }
