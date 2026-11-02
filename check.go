package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// check: lint and type-check code with Ruff + mypy (Python) and ESLint + tsc (JS/TS),
// for code on the PC, on the phone (pulled over ADB) or in a git repo (shallow clone).

type checkArgs struct {
	Action string `json:"action"`
	Source string `json:"source"`
	Path   string `json:"path"`
	Ref    string `json:"ref"`
	Device string `json:"device"`
	Fix    bool   `json:"fix"`
	Only   string `json:"only"`
	Max    int    `json:"max"`
}

type ckIssue struct {
	Tool, File string
	Line, Col  int
	Warn       bool
	Code, Msg  string
}

const ckReadyMarker = "ready-v1"

var ckToolOrder = map[string]int{"ruff": 0, "mypy": 1, "eslint": 2, "tsc": 3, "vet": 4, "staticcheck": 5, "lint": 6}

var ckSkipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "env": true,
	"__pycache__": true, "dist": true, "build": true, ".next": true, ".mypy_cache": true, ".ruff_cache": true,
	"site-packages": true, ".tox": true, "coverage": true, ".idea": true, ".vscode": true}

func checkerDir() string { return filepath.Join(appHome, "checker") }

func ckVenvBin(name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(checkerDir(), "py", "Scripts", name+".exe")
	}
	return filepath.Join(checkerDir(), "py", "bin", name)
}

func ckNodeMod(parts ...string) string {
	return filepath.Join(append([]string{checkerDir(), "node", "node_modules"}, parts...)...)
}

func ckExists(p string) bool { _, err := os.Stat(p); return err == nil }

func ckRun(dir string, timeout time.Duration, name string, args ...string) (string, string, int, error) {
	return ckRunEnv(nil, dir, timeout, name, args...)
}

func ckRunEnv(env []string, dir string, timeout time.Duration, name string, args ...string) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	hideWindow(c)
	c.Dir = dir
	c.Env = append(append(os.Environ(), "NO_COLOR=1", "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"), env...)
	var so, se strings.Builder
	c.Stdout, c.Stderr = &so, &se
	err := c.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code, err = ee.ExitCode(), nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return ckAnsi.ReplaceAllString(so.String(), ""), ckAnsi.ReplaceAllString(se.String(), ""), code, err
}

var ckAnsi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func ckFirstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}

func ckStep(what, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	so, se, code, err := ckRun(dir, timeout, name, args...)
	if err != nil {
		return "", fmt.Errorf("%s: %v", what, err)
	}
	if code != 0 {
		return "", fmt.Errorf("%s failed (exit %d): %s", what, code, ckFirstLines(se+"\n"+so, 6))
	}
	return so, nil
}

const ckEslintConfig = `import js from "@eslint/js";
import tseslint from "typescript-eslint";
import globals from "globals";

export default tseslint.config(
  { ignores: ["**/node_modules/**", "**/dist/**", "**/build/**", "**/.next/**", "**/coverage/**", "**/*.min.js", "**/.venv/**", "**/venv/**"] },
  { files: ["**/*.{js,mjs,cjs,jsx,ts,tsx,mts,cts}"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    languageOptions: {
      ecmaVersion: "latest",
      sourceType: "module",
      parserOptions: { ecmaFeatures: { jsx: true } },
      globals: { ...globals.browser, ...globals.node },
    },
    rules: {
      eqeqeq: ["error", "smart"],
      "no-var": "error",
      "prefer-const": "warn",
      "@typescript-eslint/no-unused-vars": ["warn", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
      "@typescript-eslint/no-explicit-any": "warn",
    },
  },
  { files: ["**/*.{js,cjs,mjs,jsx}"], rules: { "@typescript-eslint/no-require-imports": "off" } },
  { files: ["**/*.cjs"], languageOptions: { sourceType: "commonjs" } },
);
`

func ckSetup(force bool) (string, error) {
	dir := checkerDir()
	marker := filepath.Join(dir, ckReadyMarker)
	if !force && ckExists(marker) && ckExists(ckVenvBin("ruff")) && ckExists(ckNodeMod("eslint", "bin", "eslint.js")) {
		return "", nil
	}
	nodeDir := filepath.Join(dir, "node")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		return "", err
	}
	venv := filepath.Join(dir, "py")
	py := ckVenvBin("python")
	if uv, err := exec.LookPath("uv"); err == nil {
		if _, err := ckStep("creating Python env", dir, 5*time.Minute, uv, "venv", "--allow-existing", venv); err != nil {
			return "", err
		}
		if _, err := ckStep("installing ruff and mypy", dir, 10*time.Minute, uv, "pip", "install", "--upgrade", "--python", py, "ruff", "mypy"); err != nil {
			return "", err
		}
	} else {
		sys := "python3"
		if runtime.GOOS == "windows" {
			sys = "python"
		}
		pyExe, err := exec.LookPath(sys)
		if err != nil {
			return "", errors.New("no Python found: install Python 3 or uv, then run check with action:setup")
		}
		if !ckExists(py) {
			if _, err := ckStep("creating Python env", dir, 5*time.Minute, pyExe, "-m", "venv", venv); err != nil {
				return "", err
			}
		}
		if _, err := ckStep("installing ruff and mypy", dir, 10*time.Minute, py, "-m", "pip", "install", "--upgrade", "--quiet", "ruff", "mypy"); err != nil {
			return "", err
		}
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return "", errors.New("no Node.js found: install Node.js, then run check with action:setup")
	}
	pkg := filepath.Join(nodeDir, "package.json")
	if !ckExists(pkg) {
		if err := os.WriteFile(pkg, []byte(`{"name":"tokensaver-checker","private":true}`+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	if _, err := ckStep("installing ESLint and TypeScript", nodeDir, 10*time.Minute, npm, "install", "--no-audit", "--no-fund", "--loglevel=error",
		"eslint@9", "@eslint/js@9", "typescript", "typescript-eslint", "globals", "@types/node"); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "eslint.config.mjs"), []byte(ckEslintConfig), 0o644); err != nil {
		return "", err
	}
	var vers []string
	if node, err := exec.LookPath("node"); err == nil {
		for _, t := range [][2]string{{"eslint", ckNodeMod("eslint", "bin", "eslint.js")}, {"tsc", ckNodeMod("typescript", "bin", "tsc")}} {
			if so, _, _, err := ckRun(dir, time.Minute, node, t[1], "--version"); err == nil {
				vers = append(vers, t[0]+" "+strings.TrimPrefix(strings.TrimSpace(so), "Version "))
			}
		}
	}
	for _, t := range [][]string{{ckVenvBin("ruff"), "--version"}, {py, "-m", "mypy", "--version"}} {
		if so, _, _, err := ckRun(dir, time.Minute, t[0], t[1:]...); err == nil {
			vers = append(vers, strings.TrimSpace(strings.SplitN(so, "(", 2)[0]))
		}
	}
	if err := os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return "", err
	}
	return "Checker ready: " + strings.Join(vers, " · ") + "\nInstalled in " + dir, nil
}

func ckSafe(s string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s, "_")
}

// ckFresh empties a work folder; only ever inside the checker's own work dir.
func ckFresh(dest string) error {
	work := filepath.Join(checkerDir(), "work")
	if rel, err := filepath.Rel(work, dest); err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return errors.New("refusing to clear " + dest)
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Dir(dest), 0o755)
}

func ckRepoURL(p string) (url, name string, err error) {
	p = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), "/"))
	if p == "" {
		return "", "", errors.New("path is required: owner/name or a git URL")
	}
	if strings.Contains(p, "://") || strings.HasPrefix(p, "git@") {
		url = p
	} else if regexp.MustCompile(`^[\w.-]+/[\w.-]+$`).MatchString(p) {
		url = "https://github.com/" + strings.TrimSuffix(p, ".git") + ".git"
	} else {
		return "", "", errors.New("repo path must be owner/name or a git URL")
	}
	parts := strings.FieldsFunc(strings.TrimSuffix(url, ".git"), func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) >= 2 {
		name = parts[len(parts)-2] + "_" + parts[len(parts)-1]
	} else {
		name = parts[len(parts)-1]
	}
	return url, ckSafe(name), nil
}

// ckTarget resolves what to check: root folder, optional single file, label and notes.
func ckTarget(a *checkArgs) (root, single, label string, notes []string, err error) {
	finish := func(p string) error {
		info, err := os.Stat(p)
		if err != nil {
			return errors.New(cleanErr(err) + ": " + p)
		}
		if info.IsDir() {
			root = p
		} else {
			root, single = filepath.Dir(p), p
		}
		return nil
	}
	switch strings.ToLower(a.Source) {
	case "", "pc":
		if a.Path == "" {
			return "", "", "", nil, errors.New("path is required: a file or folder on the PC")
		}
		p, err := expandRead(a.Path)
		if err == nil && a.Fix && isRepoCopy(p) {
			err = errors.New(p + " is a temporary read-only copy: check it without fix, or use source:\"repo\" to fix a clone of it")
		}
		if err != nil {
			return "", "", "", nil, err
		}
		if err := checkReadable(p); err != nil {
			return "", "", "", nil, err
		}
		return root, single, p, notes, finish(p)
	case "phone":
		if a.Path == "" {
			return "", "", "", nil, errors.New("path is required: a file or folder on the phone, e.g. /sdcard/Projects/app")
		}
		adb, err := exec.LookPath("adb")
		if err != nil {
			return "", "", "", nil, errors.New("adb not found on the PC (install Android platform-tools)")
		}
		var pre []string
		if a.Device != "" {
			ckRun(checkerDir(), 30*time.Second, adb, "connect", a.Device)
			pre = []string{"-s", a.Device}
		}
		so, _, _, err := ckRun(checkerDir(), 30*time.Second, adb, append(pre, "get-state")...)
		if (err != nil || strings.TrimSpace(so) != "device") && a.Device == "" {
			// find a paired phone with Wireless debugging on, over mDNS
			ms, _, _, _ := ckRun(checkerDir(), 30*time.Second, adb, "mdns", "services")
			if m := regexp.MustCompile(`_adb-tls-connect\._tcp\.?\s+(\d+\.\d+\.\d+\.\d+:\d+)`).FindStringSubmatch(ms); m != nil {
				ckRun(checkerDir(), 30*time.Second, adb, "connect", m[1])
				pre = []string{"-s", m[1]}
				so, _, _, err = ckRun(checkerDir(), 30*time.Second, adb, append(pre, "get-state")...)
			}
		}
		if err != nil || strings.TrimSpace(so) != "device" {
			return "", "", "", nil, errors.New("phone not reachable over ADB: turn on Wireless debugging and pass device:\"IP:port\" from Developer options › Wireless debugging (or plug in USB)")
		}
		dest := filepath.Join(checkerDir(), "work", "phone", ckSafe(strings.Trim(a.Path, "/")))
		if err := ckFresh(dest); err != nil {
			return "", "", "", nil, err
		}
		if _, err := ckStep("pulling from phone", checkerDir(), 10*time.Minute, adb, append(pre, "pull", a.Path, dest)...); err != nil {
			return "", "", "", nil, err
		}
		if a.Fix {
			notes = append(notes, "Fixes went into the PC copy at "+dest+", not the phone.")
		}
		return root, single, "phone:" + a.Path, notes, finish(dest)
	case "repo":
		url, name, err := ckRepoURL(a.Path)
		if err != nil {
			return "", "", "", nil, err
		}
		git, err := exec.LookPath("git")
		if err != nil {
			return "", "", "", nil, errors.New("git not found on the PC")
		}
		dest := filepath.Join(checkerDir(), "work", "repo", name)
		ref := a.Ref
		reused := false
		if ckExists(filepath.Join(dest, ".git")) {
			r := ref
			if r == "" {
				r = "HEAD"
			}
			if _, err := ckStep("fetching repo", dest, 10*time.Minute, git, "fetch", "--depth", "1", "--quiet", url, r); err == nil {
				if _, err := ckStep("updating repo", dest, 2*time.Minute, git, "reset", "--hard", "--quiet", "FETCH_HEAD"); err == nil {
					ckRun(dest, 2*time.Minute, git, "clean", "-fdxq")
					reused = true
				}
			}
		}
		if !reused {
			if err := ckFresh(dest); err != nil {
				return "", "", "", nil, err
			}
			args := []string{"clone", "--depth", "1", "--quiet"}
			if ref != "" {
				args = append(args, "--branch", ref)
			}
			if _, err := ckStep("cloning repo", checkerDir(), 10*time.Minute, git, append(args, url, dest)...); err != nil {
				return "", "", "", nil, err
			}
		}
		label = url
		if ref != "" {
			label += "@" + ref
		}
		if a.Fix {
			notes = append(notes, "Fixes went into the clone at "+dest+" (git diff there shows them); nothing was pushed.")
		}
		return root, single, label, notes, finish(dest)
	}
	return "", "", "", nil, errors.New("source must be pc, phone or repo")
}

func ckIgnored(rel string, ign []string) bool {
	for _, p := range ign {
		if rel == strings.TrimSuffix(p, "/") || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
			return true
		}
	}
	return false
}

// ckGitIgnored lists gitignored paths under root (relative, folders end in /) when root is in a git work tree.
func ckGitIgnored(root string) []string {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	so, _, code, err := ckRun(root, time.Minute, git, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil || code != 0 {
		return nil
	}
	var out []string
	for _, l := range strings.Split(so, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func ckCollect(root, single string, ign []string) (py, js, ts, gof, kt int) {
	add := func(p string) {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".py", ".pyi":
			py++
		case ".js", ".jsx", ".mjs", ".cjs":
			if !strings.HasSuffix(strings.ToLower(p), ".min.js") {
				js++
			}
		case ".ts", ".tsx", ".mts", ".cts":
			ts++
		case ".kt", ".kts", ".java":
			kt++
		case ".go":
			gof++
		}
	}
	if single != "" {
		add(single)
		return
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel := ckRel(root, p)
		if d.IsDir() {
			if p != root && (ckSkipDirs[d.Name()] || ckIgnored(rel+"/", ign)) {
				return filepath.SkipDir
			}
			return nil
		}
		if ckIgnored(rel, ign) {
			return nil
		}
		add(p)
		if py+js+ts+gof+kt > 50000 {
			return filepath.SkipAll
		}
		return nil
	})
	return
}

func ckRel(root, p string) string {
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(p)
	}
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return p
}

func ckTarg(root, single string) string {
	if single != "" {
		return filepath.Base(single)
	}
	return "."
}

func ckFileHas(p, s string) bool {
	b, err := os.ReadFile(p)
	return err == nil && strings.Contains(string(b), s)
}

var ckRuffLine = regexp.MustCompile(`^(.+?):(\d+):(\d+): ([A-Z]+[0-9]+) (?:\[\*\] )?(.*)$`)
var ckRuffFixed = regexp.MustCompile(`\((\d+) fixed`)

func ckRuff(root, single string, fix bool) ([]ckIssue, string, string) {
	args := []string{"check", "--output-format", "concise", "--no-cache", "--exit-zero"}
	if !ckExists(filepath.Join(root, "ruff.toml")) && !ckExists(filepath.Join(root, ".ruff.toml")) && !ckFileHas(filepath.Join(root, "pyproject.toml"), "[tool.ruff") {
		args = append(args, "--select", "E4,E7,E9,F,B")
	}
	if fix {
		args = append(args, "--fix")
	}
	so, se, code, err := ckRun(root, 5*time.Minute, ckVenvBin("ruff"), append(args, ckTarg(root, single))...)
	if err != nil {
		return nil, "ruff: " + err.Error(), ""
	}
	var out []ckIssue
	for _, l := range strings.Split(so, "\n") {
		if m := ckRuffLine.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			out = append(out, ckIssue{Tool: "ruff", File: ckRel(root, m[1]), Line: ln, Col: col, Code: m[4], Msg: m[5]})
		}
	}
	if code != 0 && len(out) == 0 {
		return nil, "ruff failed: " + ckFirstLines(se+"\n"+so, 4), ""
	}
	note := ""
	if m := ckRuffFixed.FindStringSubmatch(so + se); m != nil {
		note = "ruff fixed " + m[1] + " issue(s)."
	}
	return out, "", note
}

var ckMypyLine = regexp.MustCompile(`^(.+?):(\d+):(?:(\d+):)? (error|warning|note): (.*?)(?:  \[([\w-]+)\])?$`)

func ckMypy(root, single string, ign []string) ([]ckIssue, string) {
	args := []string{"-m", "mypy", "--ignore-missing-imports", "--no-error-summary", "--show-column-numbers", "--show-error-codes",
		"--no-color-output", "--hide-error-context", "--no-pretty", "--explicit-package-bases",
		"--cache-dir", filepath.Join(checkerDir(), "mypy_cache"),
		"--exclude", `(^|[/\\])(\.venv|venv|env|node_modules|build|dist|site-packages|\.git|\.tox)([/\\]|$)`,
	}
	for _, p := range ign {
		args = append(args, "--exclude", "^"+regexp.QuoteMeta(strings.TrimSuffix(p, "/"))+"(/|$)")
	}
	args = append(args, ckTarg(root, single))
	so, se, code, err := ckRun(root, 5*time.Minute, ckVenvBin("python"), args...)
	if err != nil {
		return nil, "mypy: " + err.Error()
	}
	var out []ckIssue
	for _, l := range strings.Split(so, "\n") {
		m := ckMypyLine.FindStringSubmatch(strings.TrimRight(l, "\r"))
		if m == nil || m[4] == "note" {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		out = append(out, ckIssue{Tool: "mypy", File: ckRel(root, m[1]), Line: ln, Col: col, Warn: m[4] == "warning", Code: m[6], Msg: m[5]})
	}
	if code > 1 || (code == 1 && len(out) == 0) {
		return out, "mypy didn't finish: " + ckFirstLines(so+"\n"+se, 4)
	}
	return out, ""
}

func ckEslint(root, single string, fix bool, ign []string) ([]ckIssue, string, string) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, "eslint: Node.js not found", ""
	}
	projCfg := false
	for _, n := range []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts", "eslint.config.mts", "eslint.config.cts"} {
		if ckExists(filepath.Join(root, n)) {
			projCfg = true
		}
	}
	projEslint := filepath.Join(root, "node_modules", "eslint", "bin", "eslint.js")
	var args []string
	used := "default rules"
	if projCfg && ckExists(projEslint) {
		args = []string{projEslint}
		used = "project config"
	} else {
		args = []string{ckNodeMod("eslint", "bin", "eslint.js"), "--config", filepath.Join(checkerDir(), "node", "eslint.config.mjs")}
	}
	args = append(args, "--format", "json", "--no-warn-ignored", "--no-error-on-unmatched-pattern")
	for _, p := range ign {
		if strings.HasSuffix(p, "/") {
			p += "**"
		}
		args = append(args, "--ignore-pattern", p)
	}
	if fix {
		args = append(args, "--fix")
	}
	so, se, code, err := ckRun(root, 5*time.Minute, node, append(args, ckTarg(root, single))...)
	if err != nil {
		return nil, "eslint: " + err.Error(), ""
	}
	var res []struct {
		FilePath string `json:"filePath"`
		Messages []struct {
			RuleID   *string `json:"ruleId"`
			Severity int     `json:"severity"`
			Message  string  `json:"message"`
			Line     int     `json:"line"`
			Column   int     `json:"column"`
		} `json:"messages"`
	}
	if i := strings.Index(so, "["); i >= 0 {
		so = so[i:]
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(so)), &res); err != nil {
		if code != 0 {
			return nil, "eslint failed: " + ckFirstLines(se+"\n"+so, 4), ""
		}
		return nil, "", ""
	}
	var out []ckIssue
	for _, f := range res {
		for _, m := range f.Messages {
			rule := ""
			if m.RuleID != nil {
				rule = *m.RuleID
			}
			out = append(out, ckIssue{Tool: "eslint", File: ckRel(root, f.FilePath), Line: m.Line, Col: m.Column, Warn: m.Severity < 2, Code: rule, Msg: m.Message})
		}
	}
	return out, "", "eslint used " + used + "."
}

var ckTscLine = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\): (error|warning) (TS\d+): (.*)$`)

func ckTsc(root, single string, tsFiles int, ign []string) ([]ckIssue, string, string) {
	hasTsconfig := ckExists(filepath.Join(root, "tsconfig.json"))
	if tsFiles == 0 && (single != "" || !hasTsconfig) {
		return nil, "", ""
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, "tsc: Node.js not found", ""
	}
	tsc := filepath.Join(root, "node_modules", "typescript", "bin", "tsc")
	if !ckExists(tsc) {
		tsc = ckNodeMod("typescript", "bin", "tsc")
	}
	noDeps := !ckExists(filepath.Join(root, "node_modules"))
	var args []string
	used := "default strict settings"
	if single == "" && hasTsconfig {
		args = []string{tsc, "--noEmit", "--pretty", "false", "-p", "tsconfig.json"}
		used = "project tsconfig"
	} else {
		r := filepath.ToSlash(root)
		cfg := map[string]any{"compilerOptions": map[string]any{
			"strict": true, "noEmit": true, "skipLibCheck": true, "target": "es2022", "module": "esnext",
			"moduleResolution": "bundler", "jsx": "react-jsx", "esModuleInterop": true, "resolveJsonModule": true,
			"allowImportingTsExtensions": true, "lib": []string{"es2023", "dom", "dom.iterable"},
			"typeRoots": []string{filepath.ToSlash(ckNodeMod("@types"))}, "types": []string{"node"},
		}}
		if single != "" {
			cfg["files"] = []string{filepath.ToSlash(single)}
		} else {
			cfg["include"] = []string{r + "/**/*.ts", r + "/**/*.tsx", r + "/**/*.mts", r + "/**/*.cts"}
			var ex []string
			for d := range ckSkipDirs {
				ex = append(ex, r+"/**/"+d)
			}
			for _, p := range ign {
				ex = append(ex, r+"/"+strings.TrimSuffix(p, "/"))
			}
			sort.Strings(ex)
			cfg["exclude"] = ex
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		p := filepath.Join(checkerDir(), "work", "tsconfig.check.json")
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, "tsc: " + err.Error(), ""
		}
		args = []string{tsc, "--pretty", "false", "-p", p}
	}
	so, se, code, err := ckRun(root, 5*time.Minute, node, args...)
	if err != nil {
		return nil, "tsc: " + err.Error(), ""
	}
	var out []ckIssue
	skipped := 0
	for _, l := range strings.Split(so, "\n") {
		m := ckTscLine.FindStringSubmatch(strings.TrimRight(l, "\r"))
		if m == nil {
			continue
		}
		if noDeps && (m[5] == "TS2307" || m[5] == "TS7016" || m[5] == "TS2875") {
			skipped++
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		out = append(out, ckIssue{Tool: "tsc", File: ckRel(root, m[1]), Line: ln, Col: col, Warn: m[4] == "warning", Code: m[5], Msg: m[6]})
	}
	if code != 0 && len(out) == 0 && skipped == 0 {
		return nil, "tsc failed: " + ckFirstLines(so+"\n"+se, 4), ""
	}
	note := "tsc used " + used + "."
	if skipped > 0 {
		note += fmt.Sprintf(" Skipped %d missing-package error(s) (no node_modules; run npm install for a full type check).", skipped)
	}
	return out, "", note
}

func toolCodeCheck(raw json.RawMessage) (string, error) {
	var a checkArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
	}
	if runtime.GOOS == "android" {
		return "", errors.New("check runs on the PC; call the PC's TokenSaver with source:\"phone\" to check code on the phone")
	}
	if a.Action == "setup" {
		return ckSetup(true)
	}
	if a.Action != "" && a.Action != "check" {
		return "", errors.New("action must be check or setup")
	}
	setupMsg, err := ckSetup(false)
	if err != nil {
		return "", err
	}
	if a.Max <= 0 {
		a.Max = 40
	}
	root, single, label, notes, err := ckTarget(&a)
	if err != nil {
		return "", err
	}
	ign := ckGitIgnored(root)
	py, js, ts, gof, kt := ckCollect(root, single, ign)
	want := func(k string) bool { return a.Only == "" || a.Only == k }
	doPy := py > 0 && want("python")
	doJs := js+ts > 0 && want("js")
	doGo := gof > 0 && single == "" && ckExists(filepath.Join(root, "go.mod")) && want("go")
	doAndroid := single == "" && ckIsAndroid(root) && want("android")
	if !doPy && !doJs && !doGo && !doAndroid {
		return fmt.Sprintf("Checked %s: no Python, JS/TS, Go module or Android project files found.", label), nil
	}
	var issues []ckIssue
	var fails []string
	add := func(is []ckIssue, fail, note string) {
		issues = append(issues, is...)
		if fail != "" {
			fails = append(fails, fail)
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	ran := []string{}
	if doPy {
		add(ckRuff(root, single, a.Fix))
		is, fail := ckMypy(root, single, ign)
		add(is, fail, "")
		ran = append(ran, "ruff", "mypy")
	}
	if doJs {
		add(ckEslint(root, single, a.Fix, ign))
		ran = append(ran, "eslint")
		if ts > 0 || (single == "" && ckExists(filepath.Join(root, "tsconfig.json"))) {
			add(ckTsc(root, single, ts, ign))
			ran = append(ran, "tsc")
		}
	}
	if doGo {
		add(ckGo(root))
		ran = append(ran, "vet", "staticcheck")
	}
	if doAndroid {
		add(ckAndroid(root))
		ran = append(ran, "lint")
	}
	sort.SliceStable(issues, func(i, j int) bool {
		x, y := issues[i], issues[j]
		if x.Warn != y.Warn {
			return !x.Warn
		}
		if x.File != y.File {
			return x.File < y.File
		}
		if x.Line != y.Line {
			return x.Line < y.Line
		}
		return ckToolOrder[x.Tool] < ckToolOrder[y.Tool]
	})
	errs, warns := map[string]int{}, map[string]int{}
	for _, is := range issues {
		if is.Warn {
			warns[is.Tool]++
		} else {
			errs[is.Tool]++
		}
	}
	var counts []string
	for _, t := range ran {
		s := fmt.Sprintf("%s %d", t, errs[t])
		if warns[t] > 0 {
			s += fmt.Sprintf(" (+%d warn)", warns[t])
		}
		counts = append(counts, s)
	}
	files := []string{}
	if py > 0 {
		files = append(files, fmt.Sprintf("%d Python", py))
	}
	if js+ts > 0 {
		files = append(files, fmt.Sprintf("%d JS/TS", js+ts))
	}
	if gof > 0 {
		files = append(files, fmt.Sprintf("%d Go", gof))
	}
	if kt > 0 {
		files = append(files, fmt.Sprintf("%d Kotlin/Java", kt))
	}
	var b strings.Builder
	if setupMsg != "" {
		b.WriteString(setupMsg + "\n")
	}
	fmt.Fprintf(&b, "Checked %s (%s files): %s\n", label, strings.Join(files, ", "), strings.Join(counts, " · "))
	for _, f := range fails {
		b.WriteString("⚠ " + f + "\n")
	}
	line := func(is ckIssue) string {
		sev := ""
		if is.Warn {
			sev = " (warn)"
		}
		msg := strings.Join(strings.Fields(is.Msg), " ")
		if len(msg) > 220 {
			msg = msg[:220] + "…"
		}
		return fmt.Sprintf("%s:%d:%d %s %s%s %s", is.File, is.Line, is.Col, is.Tool, is.Code, sev, msg)
	}
	var full strings.Builder
	fmt.Fprintf(&full, "Checked %s at %s\nroot: %s\n\n", label, time.Now().Format(time.RFC3339), root)
	for i, is := range issues {
		full.WriteString(line(is) + "\n")
		if i < a.Max {
			b.WriteString(line(is) + "\n")
		}
	}
	report := filepath.Join(checkerDir(), "last-report.txt")
	os.WriteFile(report, []byte(full.String()), 0o644)
	if len(issues) > a.Max {
		fmt.Fprintf(&b, "… %d more (full list: %s)\n", len(issues)-a.Max, report)
	}
	if len(issues) == 0 && len(fails) == 0 {
		b.WriteString("No problems found.\n")
	}
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

var ckGoLine = regexp.MustCompile(`^(?:vet: )?(.+?\.go):(\d+):(\d+): (.*?)(?: \(([A-Z]+\d+)\))?$`)

// ckGo runs go vet and staticcheck on the Go module at root.
func ckGo(root string) ([]ckIssue, string, string) {
	goExe, err := exec.LookPath("go")
	if err != nil {
		return nil, "go vet: Go not found", ""
	}
	var out []ckIssue
	var fails []string
	parse := func(tool, s string) int {
		n := 0
		for _, l := range strings.Split(s, "\n") {
			m := ckGoLine.FindStringSubmatch(strings.TrimSpace(l))
			if m == nil {
				continue
			}
			f := filepath.Clean(m[1])
			if !filepath.IsAbs(f) {
				f = filepath.Join(root, f)
			}
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			out = append(out, ckIssue{Tool: tool, File: ckRel(root, f), Line: ln, Col: col, Code: m[5], Msg: m[4]})
			n++
		}
		return n
	}
	so, se, code, err := ckRun(root, 10*time.Minute, goExe, "vet", "./...")
	if err != nil {
		fails = append(fails, "go vet: "+err.Error())
	} else if parse("vet", so+"\n"+se) == 0 && code != 0 {
		fails = append(fails, "go vet failed: "+ckFirstLines(se+"\n"+so, 4))
	}
	sc := filepath.Join(checkerDir(), "go", "bin", "staticcheck")
	if runtime.GOOS == "windows" {
		sc += ".exe"
	}
	if !ckExists(sc) {
		_, e, c, err := ckRunEnv([]string{"GOBIN=" + filepath.Dir(sc)}, root, 10*time.Minute, goExe, "install", "honnef.co/go/tools/cmd/staticcheck@latest")
		if err != nil || c != 0 {
			fails = append(fails, "staticcheck install failed: "+ckFirstLines(e, 4))
			return out, strings.Join(fails, "; "), ""
		}
	}
	so, se, code, err = ckRun(root, 10*time.Minute, sc, "-f", "text", "./...")
	if err != nil {
		fails = append(fails, "staticcheck: "+err.Error())
	} else if parse("staticcheck", so) == 0 && code != 0 {
		fails = append(fails, "staticcheck failed: "+ckFirstLines(se+"\n"+so, 4))
	}
	return out, strings.Join(fails, "; "), ""
}

// ckIsAndroid: a Gradle wrapper plus the Android Gradle plugin in the root or a module build file.
func ckIsAndroid(root string) bool {
	gw := "gradlew"
	if runtime.GOOS == "windows" {
		gw = "gradlew.bat"
	}
	if !ckExists(filepath.Join(root, gw)) {
		return false
	}
	files, _ := filepath.Glob(filepath.Join(root, "*", "build.gradle*"))
	files = append(files, filepath.Join(root, "build.gradle"), filepath.Join(root, "build.gradle.kts"), filepath.Join(root, "gradle", "libs.versions.toml"))
	for _, f := range files {
		if ckFileHas(f, "com.android") {
			return true
		}
	}
	return false
}

type ckLintXML struct {
	Issues []struct {
		ID        string `xml:"id,attr"`
		Severity  string `xml:"severity,attr"`
		Message   string `xml:"message,attr"`
		Locations []struct {
			File   string `xml:"file,attr"`
			Line   int    `xml:"line,attr"`
			Column int    `xml:"column,attr"`
		} `xml:"location"`
	} `xml:"issue"`
}

// ckAndroid runs Android lint through the project's Gradle wrapper and reads the XML reports it writes.
func ckAndroid(root string) ([]ckIssue, string, string) {
	// Java's Windows selector needs a plain temp path, or Gradle fails with "Unable to establish loopback connection".
	jtmp := filepath.Join(os.TempDir(), "jtmp")
	if runtime.GOOS == "windows" {
		jtmp = filepath.Join(os.Getenv("SystemDrive")+`\`, "jtmp")
	}
	os.MkdirAll(jtmp, 0o755)
	env := []string{"JAVA_TOOL_OPTIONS=-Djdk.net.unixdomain.tmpdir=" + jtmp + " -Djava.io.tmpdir=" + jtmp}
	gw := filepath.Join(root, "gradlew")
	if runtime.GOOS == "windows" {
		gw += ".bat"
	}
	os.Chmod(gw, 0o755)
	start := time.Now().Add(-2 * time.Second)
	so, se, code, err := ckRunEnv(env, root, 20*time.Minute, gw, "lint", "--console=plain", "-q", "--continue")
	if err != nil {
		return nil, "android lint: " + err.Error(), ""
	}
	var out []ckIssue
	seen := map[string]bool{}
	reports, libs := 0, 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".gradle", ".kotlin", "node_modules", "intermediates", "tmp":
				if p != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		n := d.Name()
		if !strings.HasPrefix(n, "lint-results") || !strings.HasSuffix(n, ".xml") {
			return nil
		}
		// an up-to-date lint task leaves its earlier report in place, which is still current
		if info, err := d.Info(); err != nil || (code != 0 && info.ModTime().Before(start)) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var r ckLintXML
		if xml.Unmarshal(b, &r) != nil {
			return nil
		}
		reports++
		for _, is := range r.Issues {
			if len(is.Locations) == 0 {
				continue
			}
			l := is.Locations[0]
			rel := ckRel(root, l.File)
			key := is.ID + "|" + rel + "|" + strconv.Itoa(l.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			if filepath.IsAbs(rel) {
				libs++
				continue
			}
			out = append(out, ckIssue{Tool: "lint", File: rel, Line: l.Line, Col: l.Column,
				Warn: is.Severity == "Warning" || is.Severity == "Information", Code: is.ID, Msg: is.Message})
		}
		return nil
	})
	if reports == 0 {
		msg := regexp.MustCompile(`(?m)^Picked up JAVA_TOOL_OPTIONS.*\r?\n?`).ReplaceAllString(so+"\n"+se, "")
		if i := strings.Index(msg, "What went wrong:"); i >= 0 {
			msg = msg[i+len("What went wrong:"):]
		}
		return nil, "android lint failed: " + ckFirstLines(msg, 4), ""
	}
	note := ""
	if libs > 0 {
		note = fmt.Sprintf("lint skipped %d warning(s) inside libraries.", libs)
	}
	return out, "", note
}
