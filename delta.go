package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// changesSince returns only the lines that are new or gone since the last time this
// key was read with changes:true, and remembers the text for next time.
func changesSince(key, text string) string {
	sum := sha1.Sum([]byte(strings.ToLower(key)))
	p := filepath.Join(appHome, "cache", "snapshots", hex.EncodeToString(sum[:10])+".txt")
	old, err := os.ReadFile(p)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(text), 0o644)
	if err != nil {
		return "[first read; next time with changes:true only differences come back]\n" + text
	}
	if string(old) == text {
		return "[no changes since last read]"
	}
	count := func(ls []string) map[string]int {
		m := map[string]int{}
		for _, l := range ls {
			m[l]++
		}
		return m
	}
	nl, ol := strings.Split(text, "\n"), strings.Split(string(old), "\n")
	oc, nc := count(ol), count(nl)
	var b strings.Builder
	added, gone := 0, 0
	for i, l := range nl {
		if oc[l] > 0 {
			oc[l]--
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		fmt.Fprintf(&b, "+%d: %s\n", i+1, l)
		added++
	}
	for _, l := range ol {
		if nc[l] > 0 {
			nc[l]--
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		fmt.Fprintf(&b, "-  %s\n", l)
		gone++
	}
	return fmt.Sprintf("[%d new or changed line(s), %d gone since last read]\n", added, gone) + strings.TrimRight(b.String(), "\n")
}
