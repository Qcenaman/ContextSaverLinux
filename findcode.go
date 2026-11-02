package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Code-friendly find: path globs (**/*.ts), build output skipped inside projects, and
// .gitignore honoured through git itself.

// globRe turns a path glob into a case-insensitive regexp over slash paths:
// ** = any folders, * = any name part, ? = one character.
func globRe(pat string) (*regexp.Regexp, error) {
	pat = strings.TrimPrefix(strings.ReplaceAll(pat, `\`, "/"), "./")
	var b strings.Builder
	b.WriteString("(?i)^")
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch {
		case strings.HasPrefix(pat[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pat[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func isPathGlob(p string) bool { return strings.ContainsAny(p, `/\`) || strings.Contains(p, "**") }

var buildOutDirs = map[string]bool{"dist": true, "build": true, "out": true, ".next": true, ".nuxt": true, "coverage": true, "target": true, "bin": true, "obj": true, ".gradle": true}

var projectMarkers = []string{"package.json", "go.mod", "Cargo.toml", "pyproject.toml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "pom.xml", "CMakeLists.txt"}

// isBuildOutDir: a build output folder next to a project file (a plain folder called
// "build" elsewhere is still searched).
func isBuildOutDir(name, full string) bool {
	if !buildOutDirs[strings.ToLower(name)] {
		return false
	}
	parent := filepath.Dir(full)
	for _, m := range projectMarkers {
		if exists(filepath.Join(parent, m)) {
			return true
		}
	}
	if ms, _ := filepath.Glob(filepath.Join(parent, "*.csproj")); len(ms) > 0 {
		return true
	}
	return false
}

// gitIgnored lists what .gitignore excludes under root (folders and files), keyed by
// pathKey. Empty when root isn't in a git repo or git isn't installed.
func gitIgnored(root string) map[string]bool {
	set := map[string]bool{}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return set
	}
	if _, code, err := gitExec(root, 10*time.Second, "rev-parse", "--is-inside-work-tree"); err != nil || code != 0 {
		return set
	}
	out, code, err := gitExec(root, 20*time.Second, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil || code != 0 {
		return set
	}
	for _, p := range strings.Split(out, "\x00") {
		p = strings.TrimSuffix(strings.TrimSpace(p), "/")
		if p != "" {
			set[pathKey(filepath.Join(root, filepath.FromSlash(p)))] = true
		}
	}
	return set
}

func pathKey(p string) string {
	p = filepath.Clean(p)
	if isWin {
		p = strings.ToLower(p)
	}
	return p
}
