package main

// deps{path,action:list|add|update|remove,name}: dependencies through each ecosystem's
// own tool; the answer is the manifest lines that changed (or the list).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func toolDeps(a map[string]any) (string, error) {
	root, err := projectRoot(str(a, "path"))
	if err != nil {
		return "", err
	}
	act := strings.ToLower(str(a, "action"))
	if act == "" {
		act = "list"
	}
	name := str(a, "name", "package", "dep")
	if name == "" && (act == "add" || act == "remove") {
		return "", errors.New(act + " needs name")
	}
	has := func(n string) bool { return exists(filepath.Join(root, n)) }
	var manifest string
	var steps [][]string
	switch {
	case has("go.mod"):
		manifest = "go.mod"
		switch act {
		case "add":
			if !strings.Contains(name, "@") {
				name += "@latest"
			}
			steps = [][]string{{"go", "get", name}, {"go", "mod", "tidy"}}
		case "update":
			t := "./..."
			if name != "" {
				t = name
			}
			steps = [][]string{{"go", "get", "-u", t}, {"go", "mod", "tidy"}}
		case "remove":
			steps = [][]string{{"go", "mod", "edit", "-droprequire", name}, {"go", "mod", "tidy"}}
		}
	case has("package.json"):
		manifest = "package.json"
		switch act {
		case "add":
			steps = [][]string{{"npm", "install", name}}
		case "update":
			steps = [][]string{append([]string{"npm", "update"}, strings.Fields(name)...)}
		case "remove":
			steps = [][]string{{"npm", "uninstall", name}}
		}
	case has("Cargo.toml"):
		manifest = "Cargo.toml"
		switch act {
		case "add":
			steps = [][]string{{"cargo", "add", name}}
		case "update":
			steps = [][]string{append([]string{"cargo", "update"}, strings.Fields(name)...)}
		case "remove":
			steps = [][]string{{"cargo", "remove", name}}
		}
	case has("app/build.gradle.kts") || has("app/build.gradle"):
		manifest = "app/build.gradle.kts"
		if !has(manifest) {
			manifest = "app/build.gradle"
		}
		if act != "list" {
			return gradleDep(root, manifest, act, name)
		}
	case has("requirements.txt") || has("pyproject.toml"):
		manifest = "requirements.txt"
		if act != "list" {
			return pythonDep(root, act, name)
		}
	default:
		return "", fmt.Errorf("no go.mod, package.json, Cargo.toml, build.gradle or requirements.txt in %s", root)
	}
	mp := filepath.Join(root, manifest)
	before, _ := os.ReadFile(mp)
	if act == "list" {
		return listDeps(manifest, string(before)), nil
	}
	if len(steps) == 0 {
		return "", errors.New("action must be list, add, update or remove")
	}
	for _, s := range steps {
		so, se, code, err := ckRunEnv(nil, root, 10*time.Minute, s[0], s[1:]...)
		if err != nil || code != 0 {
			lines := buildErrLines(so+"\n"+se, 15)
			if len(lines) == 0 {
				lines = strings.Split(ckFirstLines(strings.TrimSpace(so+"\n"+se), 15), "\n")
			}
			return "", fmt.Errorf("%s failed:\n%s", strings.Join(s, " "), strings.Join(lines, "\n"))
		}
	}
	after, _ := os.ReadFile(mp)
	return manifest + " " + lineDiff(string(before), string(after)), nil
}

// lineDiff: lines added (+) and removed (-), or "unchanged".
func lineDiff(a, b string) string {
	old, cur := map[string]int{}, map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		old[strings.TrimSpace(l)]++
	}
	for _, l := range strings.Split(b, "\n") {
		cur[strings.TrimSpace(l)]++
	}
	var out []string
	for _, l := range strings.Split(a, "\n") {
		if t := strings.TrimSpace(l); t != "" && cur[t] < old[t] {
			out = append(out, "- "+t)
			cur[t]++
		}
	}
	old2 := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		old2[strings.TrimSpace(l)]++
	}
	for _, l := range strings.Split(b, "\n") {
		if t := strings.TrimSpace(l); t != "" && old2[t] == 0 {
			out = append(out, "+ "+t)
		} else if t != "" {
			old2[t]--
		}
	}
	if len(out) == 0 {
		return "unchanged"
	}
	if len(out) > 30 {
		out = append(out[:30], fmt.Sprintf("…%d more", len(out)-30))
	}
	return "changed:\n" + strings.Join(out, "\n")
}

var (
	goReqRe     = regexp.MustCompile(`^\s*(?:require\s+)?([\w.\-]+/[\w.\-/]+)\s+(v[\w.\-+]+)(\s*//\s*indirect)?`)
	gradleDepRe = regexp.MustCompile(`^\s*(implementation|api|kapt|ksp|testImplementation|androidTestImplementation|debugImplementation)\s*\(?\s*["']([^"']+)["']`)
)

func listDeps(manifest, text string) string {
	var out []string
	switch manifest {
	case "go.mod":
		for _, l := range strings.Split(text, "\n") {
			if m := goReqRe.FindStringSubmatch(l); m != nil && m[3] == "" {
				out = append(out, m[1]+" "+m[2])
			}
		}
	case "package.json", "Cargo.toml", "requirements.txt":
		in := manifest == "requirements.txt"
		for _, l := range strings.Split(text, "\n") {
			t := strings.TrimSpace(l)
			switch {
			case strings.Contains(t, `"dependencies"`) || strings.Contains(t, `"devDependencies"`) || strings.HasPrefix(t, "[dependencies") || strings.HasPrefix(t, "[dev-dependencies"):
				in = true
				out = append(out, strings.Trim(t, `{:"[]`)+":")
				continue
			case strings.HasPrefix(t, "}") || strings.HasPrefix(t, "["):
				if manifest != "requirements.txt" {
					in = false
				}
				continue
			}
			if in && t != "" && !strings.HasPrefix(t, "#") {
				out = append(out, "  "+strings.TrimSuffix(t, ","))
			}
		}
	default:
		for _, l := range strings.Split(text, "\n") {
			if m := gradleDepRe.FindStringSubmatch(l); m != nil {
				out = append(out, m[1]+" "+m[2])
			}
		}
	}
	if len(out) == 0 {
		return "no dependencies"
	}
	return strings.Join(out, "\n")
}

// gradleDep adds or removes an implementation("group:name:version") line; update is
// left to the user's choice of version (it says which lines to change).
func gradleDep(root, manifest, act, name string) (string, error) {
	p := filepath.Join(root, manifest)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := string(b)
	kts := strings.HasSuffix(manifest, ".kts")
	line := "    implementation(\"" + name + "\")"
	if !kts {
		line = "    implementation '" + name + "'"
	}
	switch act {
	case "add":
		if strings.Count(name, ":") < 2 {
			return "", errors.New(`gradle dependencies are group:name:version, e.g. androidx.core:core-ktx:1.13.1`)
		}
		i := strings.LastIndex(s, "dependencies {")
		if i < 0 {
			s += "\ndependencies {\n" + line + "\n}\n"
		} else {
			j := i + len("dependencies {")
			s = s[:j] + "\n" + line + s[j:]
		}
	case "remove":
		var keep []string
		found := false
		for _, l := range strings.Split(s, "\n") {
			if m := gradleDepRe.FindStringSubmatch(l); m != nil && strings.HasPrefix(m[2], name) {
				found = true
				continue
			}
			keep = append(keep, l)
		}
		if !found {
			return "", fmt.Errorf("%s isn't in %s", name, manifest)
		}
		s = strings.Join(keep, "\n")
	default:
		return listDeps(manifest, s) + "\n(change the version in " + manifest + " with edit, then check{action:build})", nil
	}
	if err := writeFileAtomic(p, []byte(s)); err != nil {
		return "", err
	}
	return manifest + " " + lineDiff(string(b), s), nil
}

// pythonDep installs into the project's .venv (made when missing; Ubuntu blocks system
// pip) and keeps requirements.txt in step.
func pythonDep(root, act, name string) (string, error) {
	venv := filepath.Join(root, ".venv")
	if !exists(filepath.Join(venv, "bin", "pip")) {
		if _, se, code, err := ckRunEnv(nil, root, 5*time.Minute, "python3", "-m", "venv", venv); err != nil || code != 0 {
			return "", fmt.Errorf("couldn't make .venv: %s (sudo apt install python3-venv)", strings.TrimSpace(se))
		}
	}
	pip := filepath.Join(venv, "bin", "pip")
	rp := filepath.Join(root, "requirements.txt")
	before, _ := os.ReadFile(rp)
	var args []string
	switch act {
	case "add":
		args = []string{"install", name}
	case "update":
		if name == "" {
			args = []string{"install", "-U", "-r", rp}
		} else {
			args = []string{"install", "-U", name}
		}
	case "remove":
		args = []string{"uninstall", "-y", name}
	default:
		return "", errors.New("action must be list, add, update or remove")
	}
	if so, se, code, err := ckRunEnv(nil, root, 10*time.Minute, pip, args...); err != nil || code != 0 {
		return "", fmt.Errorf("pip %s failed:\n%s", act, ckFirstLines(strings.TrimSpace(so+"\n"+se), 12))
	}
	base := strings.ToLower(regexp.MustCompile(`[<>=!~\[ ].*`).ReplaceAllString(name, ""))
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(string(before)), "\n") {
		if t := strings.TrimSpace(l); t != "" && strings.ToLower(regexp.MustCompile(`[<>=!~\[ ].*`).ReplaceAllString(t, "")) != base {
			keep = append(keep, t)
		}
	}
	if act == "add" || (act == "update" && name != "") {
		ver, _, _, _ := ckRunEnv(nil, root, time.Minute, pip, "show", base)
		v := regexp.MustCompile(`(?m)^Version:\s*(\S+)`).FindStringSubmatch(ver)
		if v != nil {
			keep = append(keep, base+"=="+v[1])
		} else {
			keep = append(keep, name)
		}
	}
	after := strings.Join(keep, "\n") + "\n"
	if err := os.WriteFile(rp, []byte(after), 0o644); err != nil {
		return "", err
	}
	return "requirements.txt " + lineDiff(string(before), after), nil
}
