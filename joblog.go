package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// jobLog returns the output lines a job wrote since the last log call (for dev servers
// started with keep_running), at most maxLines of them.
func jobLog(id string, maxLines int) (string, error) {
	p := outputPath(id)
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("job %s has no output", id)
	}
	defer f.Close()
	posFile := filepath.Join(jobsDir(), id, "logpos")
	var pos int64
	if b, err := os.ReadFile(posFile); err == nil {
		pos, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if fi, err := f.Stat(); err == nil && fi.Size() < pos {
		pos = 0 // log was rewritten
	}
	f.Seek(pos, io.SeekStart)
	b, _ := io.ReadAll(io.LimitReader(f, 4<<20))
	s := string(b)
	// keep a partial last line for the next call
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[:i+1]
	} else {
		s = ""
	}
	os.WriteFile(posFile, []byte(strconv.FormatInt(pos+int64(len(s)), 10)), 0o644)
	state := ""
	if st, err := loadState(id); err == nil {
		state = " (" + st.Status + ")"
	}
	lines := strings.Split(strings.TrimRight(stripCtl(s), "\r\n"), "\n")
	if s == "" {
		return "no new output" + state, nil
	}
	skipped := 0
	if len(lines) > maxLines {
		skipped = len(lines) - maxLines
		lines = lines[skipped:]
	}
	for i := range lines {
		lines[i] = clip(strings.TrimRight(lines[i], "\r"), 300)
	}
	head := fmt.Sprintf("%d new line(s)%s", len(lines)+skipped, state)
	if skipped > 0 {
		head += fmt.Sprintf(", first %d skipped", skipped)
	}
	return head + ":\n" + strings.Join(lines, "\n"), nil
}
