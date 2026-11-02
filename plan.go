package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// plan: a tiny task list for this MCP session, kept in data\plans so the user can see it.

type planStep struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

var (
	planMu    sync.Mutex
	planSteps []planStep
	planFile  = filepath.Join("plans", time.Now().Format("20060102-150405")+fmt.Sprintf("-%d.json", os.Getpid()))
)

func toolPlan(a map[string]any) string {
	planMu.Lock()
	defer planMu.Unlock()
	changed := false
	if v, ok := a["set"]; ok {
		planSteps = nil
		for _, s := range planList(v) {
			planSteps = append(planSteps, planStep{Text: s})
		}
		changed = true
	}
	if v, ok := a["add"]; ok {
		for _, s := range planList(v) {
			planSteps = append(planSteps, planStep{Text: s})
		}
		changed = true
	}
	if v, ok := a["done"]; ok {
		for _, n := range planNums(v) {
			if n >= 1 && n <= len(planSteps) {
				planSteps[n-1].Done = true
				changed = true
			}
		}
	}
	if changed {
		p := filepath.Join(appHome, planFile)
		os.MkdirAll(filepath.Dir(p), 0o755)
		b, _ := json.MarshalIndent(planSteps, "", "  ")
		os.WriteFile(p, b, 0o644)
	}
	return planShow()
}

func planShow() string {
	if len(planSteps) == 0 {
		return `no plan yet: plan{set:["step 1","step 2"]}`
	}
	done := 0
	var b strings.Builder
	for i, s := range planSteps {
		mark := "[ ]"
		if s.Done {
			mark = "[x]"
			done++
		}
		fmt.Fprintf(&b, "%d %s %s\n", i+1, mark, s.Text)
	}
	head := fmt.Sprintf("plan %d/%d done", done, len(planSteps))
	if done == len(planSteps) {
		head += " (all done)"
	}
	return head + "\n" + strings.TrimRight(b.String(), "\n")
}

// planList accepts ["a","b"], one string, or lines.
func planList(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s := strings.TrimSpace(fmt.Sprint(e)); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, l := range strings.Split(x, "\n") {
			l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*0123456789.)"))
			if l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// planNums accepts 2, "2", [1,2] or "1,2".
func planNums(v any) []int {
	var out []int
	add := func(s string) {
		var n int
		if _, err := fmt.Sscan(strings.TrimSpace(s), &n); err == nil {
			out = append(out, n)
		}
	}
	switch x := v.(type) {
	case float64:
		out = append(out, int(x))
	case string:
		for _, p := range strings.Split(x, ",") {
			add(p)
		}
	case []any:
		for _, e := range x {
			add(fmt.Sprint(e))
		}
	}
	return out
}
