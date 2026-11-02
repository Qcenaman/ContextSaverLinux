package main

// new{kind,name}: a project the way the user makes them — the app, a few tests, a GitHub
// Actions workflow that runs them on every push, git, and (when signed in to GitHub) a
// private repo with the first push. No README or docs.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var projNameRe = regexp.MustCompile(`[^A-Za-z0-9-]+`)

func toolNew(a map[string]any) (string, error) {
	kind := strings.ToLower(str(a, "kind", "type", "lang"))
	name := strings.Trim(projNameRe.ReplaceAllString(str(a, "name"), "-"), "-")
	if name == "" {
		return "", errors.New("new needs name")
	}
	parent := str(a, "path", "in", "folder")
	if parent == "" {
		h, _ := os.UserHomeDir()
		parent = filepath.Join(h, "Projects")
	}
	parent, err := expandPath(parent)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, name)
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		return "", fmt.Errorf("%s already exists and isn't empty", dir)
	}
	if err := checkAllowed(dir); err != nil {
		return "", err
	}
	user := ghUser()
	owner := user
	if owner == "" {
		owner = "me"
	}
	files, err := scaffoldFiles(kind, name, owner)
	if err != nil {
		return "", err
	}
	if kind == "android" {
		if err := fetchGradleWrapper(dir); err != nil {
			return "", fmt.Errorf("couldn't get the Gradle wrapper: %v", err)
		}
		if sdk := androidSDK(); sdk != "" {
			files["local.properties"] = "sdk.dir=" + sdk + "\n"
		}
	}
	names := make([]string, 0, len(files))
	for n, c := range files {
		p := filepath.Join(dir, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(c), mode); err != nil {
			return "", err
		}
		names = append(names, n)
	}
	sort.Strings(names)
	out := fmt.Sprintf("%s project %s: %s\n", kind, dir, strings.Join(names, ", "))
	gitExec(dir, 20*time.Second, "init", "-q", "-b", "main")
	gitExec(dir, 20*time.Second, "add", "-A")
	if _, code, _ := gitExec(dir, 30*time.Second, "commit", "-q", "-m", "0.1.0: new "+kind+" project"); code != 0 {
		return out + "git commit failed (git user.name/user.email set?)", nil
	}
	if a["repo"] == false {
		return out + "committed; no GitHub repo (repo:false)", nil
	}
	if user == "" {
		return out + "committed; not signed in to GitHub, so no repo yet (git{action:login}, then new again with the same name, or gh repo create)", nil
	}
	vis := "--private"
	if a["public"] == true {
		vis = "--public"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "gh", "repo", "create", name, vis, "--source", dir, "--push")
	c.Dir = dir
	if o, err := c.CombinedOutput(); err != nil {
		return out + "committed; gh repo create failed: " + clip(strings.TrimSpace(string(o)), 300), nil
	}
	return out + fmt.Sprintf("pushed to https://github.com/%s/%s (%s); the workflow runs the tests on every push", user, name, strings.TrimPrefix(vis, "--")), nil
}

func fetchGradleWrapper(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	base := "https://raw.githubusercontent.com/gradle/gradle/v" + gradleVersion + "/"
	for _, f := range []string{"gradlew", "gradle/wrapper/gradle-wrapper.jar"} {
		dst := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := download(ctx, base+f, dst); err != nil {
			return err
		}
	}
	return os.Chmod(filepath.Join(dir, "gradlew"), 0o755)
}

const gradleVersion = "8.10.2"

func ghWorkflow(steps string) string {
	return "name: test\non: [push, pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n" + steps
}

func scaffoldFiles(kind, name, owner string) (map[string]string, error) {
	id := strings.ToLower(strings.ReplaceAll(name, "-", ""))
	pkg := "com." + strings.ToLower(projNameRe.ReplaceAllString(owner, "")) + "." + id
	switch kind {
	case "go":
		return map[string]string{
			"go.mod":     "module github.com/" + owner + "/" + name + "\n\ngo 1.22\n",
			".gitignore": "/" + name + "\n/dist/\n",
			"main.go": "package main\n\nimport \"fmt\"\n\nconst appVersion = \"0.1.0\"\n\nfunc greet(who string) string { return \"Hello, \" + who }\n\n" +
				"func main() {\n\tfmt.Println(greet(\"world\"), appVersion)\n}\n",
			"main_test.go":               "package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif got := greet(\"Ana\"); got != \"Hello, Ana\" {\n\t\tt.Fatalf(\"got %q\", got)\n\t}\n}\n",
			".github/workflows/test.yml": ghWorkflow("      - uses: actions/setup-go@v5\n        with:\n          go-version-file: go.mod\n      - run: go vet ./...\n      - run: go test ./...\n"),
		}, nil
	case "android", "kotlin", "apk":
		path := strings.ReplaceAll(pkg, ".", "/")
		return map[string]string{
			"settings.gradle.kts": "pluginManagement {\n    repositories { google(); mavenCentral(); gradlePluginPortal() }\n}\n" +
				"dependencyResolutionManagement {\n    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)\n    repositories { google(); mavenCentral() }\n}\n" +
				"rootProject.name = \"" + name + "\"\ninclude(\":app\")\n",
			"build.gradle.kts":  "plugins {\n    id(\"com.android.application\") version \"8.7.3\" apply false\n    id(\"org.jetbrains.kotlin.android\") version \"2.0.21\" apply false\n}\n",
			"gradle.properties": "org.gradle.jvmargs=-Xmx2g\nandroid.useAndroidX=true\nkotlin.code.style=official\n",
			"gradle/wrapper/gradle-wrapper.properties": "distributionBase=GRADLE_USER_HOME\ndistributionPath=wrapper/dists\n" +
				"distributionUrl=https\\://services.gradle.org/distributions/gradle-" + gradleVersion + "-bin.zip\nzipStoreBase=GRADLE_USER_HOME\nzipStorePath=wrapper/dists\n",
			".gitignore": ".gradle/\nbuild/\n/app/build/\nlocal.properties\n/dist/\n.idea/\n*.iml\n",
			"app/build.gradle.kts": "plugins {\n    id(\"com.android.application\")\n    id(\"org.jetbrains.kotlin.android\")\n}\n\nandroid {\n" +
				"    namespace = \"" + pkg + "\"\n    compileSdk = 35\n    defaultConfig {\n        applicationId = \"" + pkg + "\"\n" +
				"        minSdk = 26\n        targetSdk = 35\n        versionCode = 1\n        versionName = \"0.1.0\"\n    }\n" +
				"    compileOptions {\n        sourceCompatibility = JavaVersion.VERSION_17\n        targetCompatibility = JavaVersion.VERSION_17\n    }\n" +
				"    kotlinOptions { jvmTarget = \"17\" }\n}\n\ndependencies {\n    testImplementation(\"junit:junit:4.13.2\")\n}\n",
			"app/src/main/AndroidManifest.xml": "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<manifest xmlns:android=\"http://schemas.android.com/apk/res/android\">\n" +
				"    <application android:label=\"" + name + "\">\n        <activity android:name=\".MainActivity\" android:exported=\"true\">\n" +
				"            <intent-filter>\n                <action android:name=\"android.intent.action.MAIN\" />\n" +
				"                <category android:name=\"android.intent.category.LAUNCHER\" />\n            </intent-filter>\n        </activity>\n    </application>\n</manifest>\n",
			"app/src/main/java/" + path + "/MainActivity.kt": "package " + pkg + "\n\nimport android.app.Activity\nimport android.os.Bundle\nimport android.widget.TextView\n\n" +
				"fun greet(who: String) = \"Hello, $who\"\n\nclass MainActivity : Activity() {\n    override fun onCreate(savedInstanceState: Bundle?) {\n" +
				"        super.onCreate(savedInstanceState)\n        setContentView(TextView(this).apply { text = greet(\"world\") })\n    }\n}\n",
			"app/src/test/java/" + path + "/GreetTest.kt": "package " + pkg + "\n\nimport org.junit.Assert.assertEquals\nimport org.junit.Test\n\n" +
				"class GreetTest {\n    @Test fun greets() = assertEquals(\"Hello, Ana\", greet(\"Ana\"))\n}\n",
			".github/workflows/test.yml": ghWorkflow("      - uses: actions/setup-java@v4\n        with:\n          distribution: temurin\n          java-version: 17\n" +
				"      - run: chmod +x gradlew && ./gradlew testDebugUnitTest assembleDebug --console=plain\n"),
		}, nil
	case "web", "js", "node":
		return map[string]string{
			"package.json": "{\n  \"name\": \"" + strings.ToLower(name) + "\",\n  \"version\": \"0.1.0\",\n  \"type\": \"module\",\n  \"private\": true,\n" +
				"  \"scripts\": {\n    \"test\": \"node --test\",\n    \"build\": \"node build.mjs\"\n  }\n}\n",
			".gitignore": "node_modules/\n/dist/\n",
			"index.html": "<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>" + name +
				"</title></head>\n<body>\n<h1 id=\"out\"></h1>\n<script type=\"module\">import { greet } from './src/app.js'; document.getElementById('out').textContent = greet('world');</script>\n</body>\n</html>\n",
			"src/app.js":                 "export function greet(who) {\n  return `Hello, ${who}`;\n}\n",
			"test/app.test.js":           "import { test } from 'node:test';\nimport assert from 'node:assert/strict';\nimport { greet } from '../src/app.js';\n\ntest('greets', () => assert.equal(greet('Ana'), 'Hello, Ana'));\n",
			"build.mjs":                  "import { cpSync, mkdirSync } from 'node:fs';\nmkdirSync('dist', { recursive: true });\ncpSync('index.html', 'dist/index.html');\ncpSync('src', 'dist/src', { recursive: true });\n",
			".github/workflows/test.yml": ghWorkflow("      - uses: actions/setup-node@v4\n        with:\n          node-version: 20\n      - run: npm test\n"),
		}, nil
	case "python", "py":
		return map[string]string{
			"pyproject.toml":   "[project]\nname = \"" + strings.ToLower(name) + "\"\nversion = \"0.1.0\"\nrequires-python = \">=3.10\"\n",
			"requirements.txt": "",
			".gitignore":       "__pycache__/\n.venv/\n/dist/\n/build/\n",
			"main.py":          "def greet(who: str) -> str:\n    return f\"Hello, {who}\"\n\n\nif __name__ == \"__main__\":\n    print(greet(\"world\"))\n",
			"test_main.py":     "from main import greet\n\n\ndef test_greet():\n    assert greet(\"Ana\") == \"Hello, Ana\"\n",
			".github/workflows/test.yml": ghWorkflow("      - uses: actions/setup-python@v5\n        with:\n          python-version: '3.12'\n" +
				"      - run: pip install pytest -r requirements.txt\n      - run: pytest -q\n"),
		}, nil
	case "rust", "rs":
		return map[string]string{
			"Cargo.toml": "[package]\nname = \"" + strings.ToLower(name) + "\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			".gitignore": "/target/\n/dist/\n",
			"src/main.rs": "fn greet(who: &str) -> String {\n    format!(\"Hello, {who}\")\n}\n\nfn main() {\n    println!(\"{}\", greet(\"world\"));\n}\n\n" +
				"#[cfg(test)]\nmod tests {\n    #[test]\n    fn greets() {\n        assert_eq!(super::greet(\"Ana\"), \"Hello, Ana\");\n    }\n}\n",
			".github/workflows/test.yml": ghWorkflow("      - run: cargo test\n"),
		}, nil
	case "c", "cpp", "c++", "cmake":
		return map[string]string{
			"CMakeLists.txt": "cmake_minimum_required(VERSION 3.16)\nproject(" + id + " C)\nadd_library(greet src/greet.c)\nadd_executable(" + id + " src/main.c)\n" +
				"target_link_libraries(" + id + " greet)\nenable_testing()\nadd_executable(tests src/test.c)\ntarget_link_libraries(tests greet)\nadd_test(NAME greet COMMAND tests)\n",
			".gitignore":                 "/build/\n/dist/\n",
			"src/greet.h":                "#pragma once\nvoid greet(const char *who, char *out, int n);\n",
			"src/greet.c":                "#include <stdio.h>\n#include \"greet.h\"\n\nvoid greet(const char *who, char *out, int n) { snprintf(out, n, \"Hello, %s\", who); }\n",
			"src/main.c":                 "#include <stdio.h>\n#include \"greet.h\"\n\nint main(void) {\n    char s[64];\n    greet(\"world\", s, sizeof s);\n    puts(s);\n    return 0;\n}\n",
			"src/test.c":                 "#include <string.h>\n#include \"greet.h\"\n\nint main(void) {\n    char s[64];\n    greet(\"Ana\", s, sizeof s);\n    return strcmp(s, \"Hello, Ana\") != 0;\n}\n",
			".github/workflows/test.yml": ghWorkflow("      - run: cmake -B build && cmake --build build && ctest --test-dir build --output-on-failure\n"),
		}, nil
	}
	return nil, errors.New("kind must be go, android, web, python, rust or c")
}
