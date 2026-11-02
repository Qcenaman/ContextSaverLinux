package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Some clients end a tool call after a fixed time (LM Studio's bridge: 60 s, and the
// whole chat request fails). A pc call that runs longer keeps running in the
// background; the model gets an id and collects the result with more{id}.

type slowResult struct {
	out  string
	imgs []toolImage
	err  error
}

var slowCalls sync.Map // id -> chan slowResult

// toolCallLimit: tool_call_limit_seconds, or 50 s for LM Studio; 0 = no limit.
func toolCallLimit() time.Duration {
	if cfg.ToolCallLimit > 0 {
		return time.Duration(cfg.ToolCallLimit) * time.Second
	}
	if strings.Contains(strings.ToLower(clientName()), "lmstudio") {
		return 50 * time.Second
	}
	return 0
}

// runLimited runs f within what is left of the call's time limit.
func runLimited(start time.Time, f func() (string, []toolImage, error)) (string, []toolImage, error) {
	limit := toolCallLimit()
	if limit <= 0 {
		return f()
	}
	left := limit - time.Since(start)
	if left < 5*time.Second {
		left = 5 * time.Second
	}
	ch := make(chan slowResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- slowResult{err: fmt.Errorf("internal error: %v", r)}
			}
		}()
		o, im, e := f()
		ch <- slowResult{o, im, e}
	}()
	select {
	case r := <-ch:
		return r.out, r.imgs, r.err
	case <-time.After(left):
		id := "w" + randHex(3)
		slowCalls.Store(id, ch)
		return stillRunning(id), nil, nil
	}
}

func stillRunning(id string) string {
	return fmt.Sprintf("still running (%s); it continues in the background. Call pc more{id:%q} to wait for the result; don't start it again.", id, id)
}

// moreSlow waits for a background call's result; ok=false if id isn't one.
func moreSlow(id string) (string, []toolImage, error, bool) {
	v, ok := slowCalls.Load(id)
	if !ok {
		return "", nil, nil, false
	}
	ch := v.(chan slowResult)
	wait := toolCallLimit()
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	select {
	case r := <-ch:
		slowCalls.Delete(id)
		return r.out, r.imgs, r.err, true
	case <-time.After(wait):
		return stillRunning(id), nil, nil, true
	}
}
