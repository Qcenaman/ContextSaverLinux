package main

import "testing"

func TestFixURL(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:5173":        "http://localhost:5173",
		"127.0.0.1:8080/app":    "http://127.0.0.1:8080/app",
		"example.com":           "https://example.com",
		"http://localhost:3000": "http://localhost:3000",
		"https://x.dev/a":       "https://x.dev/a",
	} {
		if got := fixURL(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
