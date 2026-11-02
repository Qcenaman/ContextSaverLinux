package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type runArgs struct {
	Command        string            `json:"command"`
	Shell          string            `json:"shell,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutMinutes *int              `json:"timeout_minutes,omitempty"`
	KeepRunning    bool              `json:"keep_running,omitempty"`
	WaitFor        string            `json:"wait_for,omitempty"`
	Tail           *int              `json:"tail,omitempty"`
	Elevated       bool              `json:"elevated,omitempty"`
	Confirmed      bool              `json:"confirmed,omitempty"`
}

func outputPath(id string) string { return filepath.Join(jobsDir(), id, "output.log") }

func runCommand(j *Job) error {
	a := j.Spec.Run
	if strings.TrimSpace(a.Command) == "" {
		return errors.New("empty command")
	}
	cwd := ""
	if a.Cwd != "" {
		var err error
		if cwd, err = expandPath(a.Cwd); err != nil {
			return err
		}
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			return fmt.Errorf("folder not found: %s", cwd)
		}
	}
	var re *regexp.Regexp
	if a.WaitFor != "" {
		var err error
		if re, err = regexp.Compile("(?i)" + a.WaitFor); err != nil {
			return fmt.Errorf("bad wait_for pattern: %v", err)
		}
	}
	logPath := outputPath(j.ID)
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	extra := []string{"PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8", "PIP_PROGRESS_BAR=off",
		"PIP_NO_INPUT=1", "UV_NO_PROGRESS=1", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0", "TERM=dumb"}
	for k, v := range a.Env {
		extra = append(extra, k+"="+v)
	}
	var (
		pid      int
		stop     func()
		exitCode func() (int, error)
		done     = make(chan error, 1)
	)
	if a.Elevated {
		ep, err := startElevated(elevRequest{Command: a.Command, Shell: a.Shell, Cwd: cwd, Env: extra, Confirmed: a.Confirmed}, f)
		if err != nil {
			return err
		}
		pid = ep.pid
		go func() { <-ep.done; done <- ep.err }()
		stop = func() {
			ep.stop() // closing the connection makes the helper end the process tree
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
		}
		exitCode = func() (int, error) {
			if ep.code == nil {
				if ep.err == nil {
					ep.err = errors.New("the elevated helper didn't report an exit code")
				}
				return 0, ep.err
			}
			return *ep.code, nil
		}
	} else {
		cmd, err := buildCommand(a.Shell, a.Command)
		if err != nil {
			return err
		}
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), extra...)
		cmd.Stdout, cmd.Stderr = f, f
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("couldn't start: %v", err)
		}
		pid = cmd.Process.Pid
		go func() { done <- cmd.Wait() }()
		stop = func() {
			killTree(pid)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
		}
		exitCode = func() (int, error) { return cmd.ProcessState.ExitCode(), nil }
	}
	j.mu.Lock()
	j.st.PID = pid
	j.mu.Unlock()
	if a.Elevated {
		j.setCurrent(fmt.Sprintf("pid %d (elevated)", pid))
	} else {
		j.setCurrent("pid " + fmt.Sprint(pid))
	}
	j.save()
	if re != nil {
		go watchOutput(j, logPath, re, done)
	}
	var timeout <-chan time.Time
	mins := 60
	if a.TimeoutMinutes != nil {
		mins = *a.TimeoutMinutes
	}
	if !a.KeepRunning && mins > 0 {
		timeout = time.After(time.Duration(mins) * time.Minute)
	}
	select {
	case <-done:
	case <-j.ctx.Done():
		stop()
		return errCanceled
	case <-timeout:
		stop()
		return fmt.Errorf("stopped after %d minutes (timeout_minutes)", mins)
	}
	code, err := exitCode()
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.st.ExitCode = &code
	j.mu.Unlock()
	if code != 0 {
		return fmt.Errorf("exited with code %d", code)
	}
	return nil
}

// watchOutput follows the log file and marks the job ready when a line matches.
func watchOutput(j *Job, p string, re *regexp.Regexp, done chan error) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var partial string
	for {
		line, err := r.ReadString('\n')
		partial += line
		if err == nil {
			if re.MatchString(stripCtl(partial)) {
				j.markReady()
				return
			}
			partial = ""
			continue
		}
		if err != io.EOF {
			return
		}
		if partial != "" && re.MatchString(stripCtl(partial)) {
			j.markReady()
			return
		}
		select {
		case <-j.done:
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (j *Job) markReady() {
	j.readyOnce.Do(func() {
		j.mu.Lock()
		j.st.Matched = true
		j.mu.Unlock()
		close(j.ready)
	})
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

func stripCtl(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

var (
	errLineRe  = regexp.MustCompile(`(?i)(\berror\b|\berrors?:|exception|\bfailed\b|\bfailure\b|\bfatal\b|no module named|import failed|cannot import|could not|couldn't|not recognized as|permission denied|access is denied|\bnot found\b|out of memory|no space left|unable to|denied|conflict|incompatible|\bcrash)`)
	errNoiseRe = regexp.MustCompile(`(?i)(\b0 errors?\b|no errors|without errors?|error_log|errors=|ignore[_-]?errors|on_error|errorhandler|--no-error)`)
	frameRe    = regexp.MustCompile(`^\s+File "`)
)

type outSummary struct {
	errors     []string
	errorCount int
	tail       []string
	lines      int
	partial    bool
}

func summarizeOutput(p string, tailN int) outSummary {
	var s outSummary
	f, err := os.Open(p)
	if err != nil {
		return s
	}
	defer f.Close()
	fi, _ := f.Stat()
	const window = 4 << 20
	if fi != nil && fi.Size() > window {
		f.Seek(-window, io.SeekEnd)
		s.partial = true
	}
	b, _ := io.ReadAll(f)
	text, err := decodeText(b)
	if err != nil {
		text = strings.ToValidUTF8(string(b), "?")
	}
	raw := strings.Split(text, "\n")
	if s.partial && len(raw) > 0 {
		raw = raw[1:]
	}
	var lines []string
	for _, l := range raw {
		if l = strings.TrimSpace(stripCtl(l)); l != "" {
			lines = append(lines, l)
		}
	}
	s.lines = len(lines)
	type ent struct {
		text string
		n    int
		pos  int
	}
	seen := map[string]*ent{}
	add := func(t string, pos int) {
		t = clip(t, 300)
		s.errorCount++
		if e, ok := seen[t]; ok {
			e.n++
			e.pos = pos
			return
		}
		seen[t] = &ent{t, 1, pos}
	}
	inTB, frame := false, ""
	for i, l := range raw {
		l = stripCtl(l)
		tl := strings.TrimSpace(l)
		if tl == "" {
			continue
		}
		if strings.HasPrefix(tl, "Traceback (most recent call last)") {
			inTB, frame = true, ""
			continue
		}
		if inTB {
			if frameRe.MatchString(l) {
				frame = tl
				continue
			}
			if l[0] == ' ' || l[0] == '\t' || strings.HasPrefix(tl, "^") || strings.HasPrefix(tl, "~") {
				continue
			}
			inTB = false
			if frame != "" {
				add(tl+"  ← "+clip(frame, 160), i)
			} else {
				add(tl, i)
			}
			continue
		}
		if errLineRe.MatchString(tl) && !errNoiseRe.MatchString(tl) {
			add(tl, i)
		}
	}
	ents := make([]*ent, 0, len(seen))
	for _, e := range seen {
		ents = append(ents, e)
	}
	// keep the most recent distinct errors, shown in order
	for i := 0; i < len(ents); i++ {
		for k := i + 1; k < len(ents); k++ {
			if ents[k].pos < ents[i].pos {
				ents[i], ents[k] = ents[k], ents[i]
			}
		}
	}
	if len(ents) > 12 {
		ents = ents[len(ents)-12:]
	}
	for _, e := range ents {
		t := e.text
		if e.n > 1 {
			t += fmt.Sprintf(" (×%d)", e.n)
		}
		s.errors = append(s.errors, t)
	}
	if tailN < 0 {
		tailN = 15
	}
	tailN = min(tailN, 200)
	start := max(0, len(lines)-tailN)
	for _, l := range lines[start:] {
		s.tail = append(s.tail, clip(l, 300))
	}
	return s
}

func formatRun(s *JobState) string {
	var b strings.Builder
	title := s.ID
	if s.Name != "" {
		title += ` "` + s.Name + `"`
	}
	end := s.Ended
	if end.IsZero() {
		end = time.Now()
	}
	switch {
	case s.Status == "running":
		fmt.Fprintf(&b, "Job %s: running for %s (pid %d)", title, hd(time.Since(s.Started)), s.PID)
		if s.Matched {
			b.WriteString(" · ready (wait_for matched), left running")
		}
	case s.ExitCode != nil:
		fmt.Fprintf(&b, "Job %s: exited with code %d after %s", title, *s.ExitCode, hd(end.Sub(s.Started)))
	default:
		fmt.Fprintf(&b, "Job %s: %s after %s", title, s.Status, hd(end.Sub(s.Started)))
		if s.Error != "" && !strings.HasPrefix(s.Error, "exited with code") {
			b.WriteString(" — " + s.Error)
		}
	}
	if s.FirstStep != "" {
		b.WriteString("\n" + s.FirstStep)
	}
	p := outputPath(s.ID)
	tailN := 15
	if s.Tail != nil {
		tailN = *s.Tail
	}
	sum := summarizeOutput(p, tailN)
	if len(sum.errors) > 0 {
		fmt.Fprintf(&b, "\nError lines (%d total, latest distinct):", sum.errorCount)
		for _, e := range sum.errors {
			b.WriteString("\n  ✗ " + e)
		}
	}
	if len(sum.tail) > 0 {
		fmt.Fprintf(&b, "\nLast %d of %s lines:", len(sum.tail), hc(int64(sum.lines)))
		for _, l := range sum.tail {
			b.WriteString("\n  " + l)
		}
	} else if s.Status != "running" {
		b.WriteString("\n(no output)")
	}
	b.WriteString("\nFull output: " + p)
	if s.Status == "running" {
		b.WriteString("\nCheck again with job status id=" + s.ID + " (wait_seconds to wait), stop it with job cancel.")
	}
	return b.String()
}

func toolRun(raw json.RawMessage) (string, error) {
	var a struct {
		runArgs
		Name   string `json:"name"`
		Notify bool   `json:"notify"`
		Wait   *int   `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("give command")
	}
	ra := a.runArgs
	j, err := newJob(JobSpec{Name: a.Name, Kind: "run", Run: &ra, Source: "chat"})
	if err != nil {
		return "", err
	}
	j.start()
	wait := waitSecs(a.Wait, 30, 50)
	if wait > 0 {
		select {
		case <-j.done:
		case <-j.ready:
		case <-time.After(time.Duration(wait) * time.Second):
		}
	}
	s := j.snapshot()
	return withNotify(formatState(&s), &s, a.Notify), nil
}
