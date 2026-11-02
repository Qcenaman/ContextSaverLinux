package main

// check action:build — compile and run the project's tests, returning only
// the failures so lint, build and test are one call.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type buildStep struct {
	label string
	dir   string
	env   []string
	name  string
	args  []string
}

// buildPlan decides what to build and test from the files in root.
// only narrows to one ecosystem: go|js|python|android.
func buildPlan(root, only string, testsToo bool) ([]buildStep, string) {
	has := func(names ...string) bool {
		for _, n := range names {
			if exists(filepath.Join(root, filepath.FromSlash(n))) {
				return true
			}
		}
		return false
	}
	var steps []buildStep
	var kinds []string
	want := func(k string) bool { return only == "" || only == k }

	if want("android") && has("gradlew.bat", "gradlew") && has("app/build.gradle", "app/build.gradle.kts", "settings.gradle", "settings.gradle.kts") {
		kinds = append(kinds, "android")
		gw := filepath.Join(root, "gradlew")
		if isWin && exists(gw+".bat") {
			gw += ".bat"
		}
		os.Chmod(gw, 0o755)
		tasks := []string{"assembleDebug"}
		if testsToo {
			tasks = append(tasks, "testDebugUnitTest")
		}
		steps = append(steps, buildStep{"gradle " + strings.Join(tasks, " "), root, nil, gw, append(tasks, "--console=plain", "-q", "--offline")})
	} else if want("go") && has("go.mod") {
		kinds = append(kinds, "go")
		// A root with .go files is the program itself; ./... would also sweep
		// stray copies under data/ or backups/ that are not part of the build.
		pkg := "./..."
		if hasGlob(root, "*.go") {
			pkg = "."
		}
		steps = append(steps, buildStep{"go build", root, nil, "go", []string{"build", "-trimpath", "-o", os.DevNull, pkg}})
		if testsToo {
			steps = append(steps, buildStep{"go test", root, nil, "go", []string{"test", pkg}})
		}
	}
	if want("js") && has("package.json") {
		kinds = append(kinds, "js")
		scripts := pkgScripts(filepath.Join(root, "package.json"))
		npm, npx := "npm", "npx"
		if isWin {
			npm, npx = "npm.cmd", "npx.cmd"
		}
		if _, ok := scripts["build"]; ok {
			steps = append(steps, buildStep{"npm run build", root, []string{"CI=1"}, npm, []string{"run", "build", "--silent"}})
		} else if has("tsconfig.json") {
			steps = append(steps, buildStep{"tsc --noEmit", root, nil, npx, []string{"--no-install", "tsc", "--noEmit", "-p", "."}})
		}
		if _, ok := scripts["test"]; ok && testsToo {
			steps = append(steps, buildStep{"npm test", root, []string{"CI=1"}, npm, []string{"test", "--silent", "--", "--watchAll=false"}})
		}
	}
	if want("python") && (has("pyproject.toml", "setup.py", "pytest.ini", "tests", "test") || hasGlob(root, "test_*.py")) {
		kinds = append(kinds, "python")
		py := "python3"
		if exists(filepath.Join(root, ".venv", "bin", "python")) {
			py = filepath.Join(root, ".venv", "bin", "python")
		}
		steps = append(steps, buildStep{"python compile", root, nil, py, []string{"-m", "compileall", "-q", "."}})
		if testsToo {
			steps = append(steps, buildStep{"pytest", root, nil, py, []string{"-m", "pytest", "-q", "-x", "--no-header", "-p", "no:cacheprovider"}})
		}
	}
	return steps, strings.Join(kinds, ", ")
}

func hasGlob(root, pat string) bool {
	m, _ := filepath.Glob(filepath.Join(root, pat))
	return len(m) > 0
}

func pkgScripts(p string) map[string]string {
	var pj struct {
		Scripts map[string]string `json:"scripts"`
	}
	b, err := os.ReadFile(p)
	if err == nil {
		json.Unmarshal(b, &pj)
	}
	if pj.Scripts == nil {
		pj.Scripts = map[string]string{}
	}
	return pj.Scripts
}

// Lines worth showing from a failed build/test run.
var buildErrRe = regexp.MustCompile(`(?i)(^|\s)(error|fail|failed|panic|exception|traceback|cannot|undefined|not found|expected|assert|\.go:\d+|\.ts\(\d+|\.py", line|\.kt:\d+|e: |--- FAIL|FAIL\s|\berror\[)`)

// srcPosRe: a source position (main_test.go:12:, app.kt:3:5) anywhere in a line.
var srcPosRe = regexp.MustCompile(`\w\.(?:go|ts|tsx|js|mjs|py|kt|kts|java|rs|c|cc|cpp|h|hpp|cs|dart|swift):\d+`)

func buildErrLines(out string, max int) []string {
	var res []string
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimRight(l, "\r")
		if strings.TrimSpace(t) == "" || !buildErrRe.MatchString(t) && !srcPosRe.MatchString(t) {
			continue
		}
		k := strings.TrimSpace(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		res = append(res, clip(k, 300))
		if len(res) >= max {
			break
		}
	}
	return res
}

func toolBuild(raw json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Only   string `json:"only"`
		Max    int    `json:"max"`
		NoTest bool   `json:"no_test"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", errors.New("build needs path (project folder)")
	}
	root := filepath.Clean(a.Path)
	if st, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("path not found: %s", root)
	} else if !st.IsDir() {
		root = filepath.Dir(root)
	}
	if a.Max <= 0 {
		a.Max = 40
	}
	steps, kinds := buildPlan(root, strings.ToLower(a.Only), !a.NoTest)
	if len(steps) == 0 {
		return "", fmt.Errorf("nothing to build in %s (no go.mod, package.json, gradlew, pyproject.toml or tests)", root)
	}
	logDir := filepath.Join(appHome, "cache", "build")
	os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, slug(filepath.Base(root))+".log")
	var full, b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", root, kinds)
	failed := false
	for _, s := range steps {
		start := time.Now()
		so, se, code, err := ckRunEnv(s.env, s.dir, 20*time.Minute, s.name, s.args...)
		out := so + "\n" + se
		fmt.Fprintf(&full, "==== %s (exit %d, %s)\n%s\n", s.label, code, time.Since(start).Round(time.Second), out)
		if err != nil {
			failed = true
			fmt.Fprintf(&b, "✗ %s: %v\n", s.label, err)
			for _, l := range buildErrLines(out, a.Max) {
				fmt.Fprintf(&b, "  %s\n", l)
			}
			break
		}
		if code != 0 {
			failed = true
			fmt.Fprintf(&b, "✗ %s: exit %d\n", s.label, code)
			lines := buildErrLines(out, a.Max)
			if len(lines) == 0 {
				lines = strings.Split(strings.TrimSpace(ckFirstLines(strings.TrimSpace(out), a.Max)), "\n")
			}
			for _, l := range lines {
				fmt.Fprintf(&b, "  %s\n", l)
			}
			break
		}
		note := ""
		if s.label == "go test" {
			if n := strings.Count("\n"+so, "\nok "); n > 0 {
				note = fmt.Sprintf(", %d package(s) ok", n)
			}
		} else if s.label == "pytest" {
			if m := regexp.MustCompile(`(\d+ passed[^\n]*)`).FindString(so); m != "" {
				note = ", " + m
			}
		}
		fmt.Fprintf(&b, "✓ %s (%s%s)\n", s.label, time.Since(start).Round(time.Second), note)
	}
	os.WriteFile(logPath, []byte(full.String()), 0o644)
	if failed {
		fmt.Fprintf(&b, "Full log: %s", logPath)
	} else {
		b.WriteString("all good")
	}
	return b.String(), nil
}
