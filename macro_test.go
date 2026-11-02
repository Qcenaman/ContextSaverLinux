package main

import (
	"strings"
	"testing"
	"time"
)

func TestCompactRecording(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	k := func(vk uint16, down bool, at int) rawInput {
		return rawInput{kind: "key", vk: vk, down: down, at: ms(at)}
	}
	m := func(down bool, x, y, at int) rawInput {
		return rawInput{kind: "mouse", button: "left", down: down, x: x, y: y, at: ms(at)}
	}
	evs := []rawInput{
		m(true, 100, 200, 0), m(false, 101, 200, 80), // click
		m(true, 100, 200, 300), m(false, 100, 200, 360), m(true, 100, 200, 450), m(false, 100, 200, 500), // double click
		k(0x10, true, 1000), k('H', true, 1010), k('H', false, 1050), k(0x10, false, 1060), k('I', true, 1100), k('I', false, 1150), // "Hi"
		k(0x11, true, 2000), k('S', true, 2010), k('S', false, 2050), k(0x11, false, 2060), // ctrl+s
		m(true, 10, 10, 3000), {kind: "move", x: 50, y: 50, at: ms(3100)}, m(false, 90, 90, 3200), // drag
		{kind: "wheel", wheel: -240, x: 5, y: 5, at: ms(4000)},
	}
	var got []string
	for _, s := range compactRecording(evs) {
		got = append(got, stepText(s))
	}
	g := strings.Join(got, " | ")
	for _, want := range []string{"left click at 100,200", "left double-click at 100,200", `type "Hi"`, "press ctrl+s",
		"left button down at 10,10", "move at 50,50", "left button up at 90,90", "scroll -2 at 5,5"} {
		if !strings.Contains(g, want) {
			t.Fatalf("missing %q in %s", want, g)
		}
	}
	if strings.Contains(g, "press shift") || strings.Contains(g, "press ctrl |") {
		t.Fatalf("modifiers leaked: %s", g)
	}
}

func TestParseCombo(t *testing.T) {
	ks, err := parseCombo("Ctrl+Alt+F4")
	if err != nil || len(ks) != 3 || ks[2] != 0x73 {
		t.Fatalf("got %v %v", ks, err)
	}
	if _, err := parseCombo("ctrl+nope"); err == nil {
		t.Fatal("want error")
	}
}
