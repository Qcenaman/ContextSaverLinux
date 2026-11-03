package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoTestEnv points ContextSaver's data folder and "GitHub" at temp folders; returns the
// folder holding the bare repos (<owner>/<name>.git).
func repoTestEnv(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	oh, ob, ow := appHome, repoURLBase, repoCloneWait
	appHome = t.TempDir()
	hub := t.TempDir()
	repoURLBase = "file://" + filepath.ToSlash(hub) + "/"
	repoCloneWait = time.Minute
	lastRepoClean.Store(time.Now().Unix()) // no background cleanup during tests
	t.Cleanup(func() { appHome, repoURLBase, repoCloneWait = oh, ob, ow })
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
	return hub
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newHubRepo makes <hub>/<owner>/<name>.git with a first commit of files and returns a
// work clone to push more commits from.
func newHubRepo(t *testing.T, hub, owner, name string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(hub, owner, name+".git")
	os.MkdirAll(bare, 0o755)
	gitT(t, bare, "init", "-q", "--bare", "-b", "main")
	gitT(t, bare, "config", "uploadpack.allowFilter", "true")
	work := filepath.Join(hub, "work-"+owner+"-"+name)
	gitT(t, hub, "init", "-q", "-b", "main", work)
	gitT(t, work, "remote", "add", "origin", bare)
	commitFiles(t, work, files, "first")
	return work
}

// commitFiles writes files, commits them and pushes the current branch.
func commitFiles(t *testing.T, work string, files map[string]string, msg string) {
	t.Helper()
	for p, c := range files {
		f := filepath.Join(work, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", msg)
	gitT(t, work, "push", "-q", "origin", "HEAD")
}

func readT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseRepoPath(t *testing.T) {
	for in, want := range map[string]repoSpec{
		"Qcenaman/ContextSaver":                   {"Qcenaman", "ContextSaver", "", ""},
		"acme/demo@v1.2/src/a.go":                 {"acme", "demo", "v1.2", "src/a.go"},
		"acme/demo.git":                           {"acme", "demo", "", ""},
		"https://github.com/acme/demo/tree/dev/x": {"acme", "demo", "dev", "x"},
	} {
		if got, ok := parseRepoPath(in); !ok || got != want {
			t.Errorf("%s: got %+v %v, want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"/home/x/y", "~/x/y", `D:\x\y`, "D:/x/y", "single", "./a/b", "%HOME%/x", "a b/c", "acme/demo@bad ref"} {
		if _, ok := parseRepoPath(in); ok {
			t.Errorf("%s should not be a repo", in)
		}
	}
}

// owner/name resolves to ContextSaver's repos folder; later uses reuse the copy as it is,
// and an update (the user's yes) fast-forwards it, or resets it when history was rewritten.
func TestRepoPathResolvesAndUpdates(t *testing.T) {
	hub := repoTestEnv(t)
	work := newHubRepo(t, hub, "acme", "demo", map[string]string{"a.txt": "one\n", "src/b.go": "package b\n\nfunc Hello() {}\n"})
	p, err := expandRead("acme/demo/src/b.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(appHome, "repos", "acme", "demo", "src", "b.go"); p != want {
		t.Fatalf("got %s, want %s", p, want)
	}
	if s := shortRepoPaths("found " + p); s != "found acme/demo/src/b.go" {
		t.Fatalf("short path: %s", s)
	}
	if out, err := toolOutline(map[string]any{"path": "acme/demo/src"}); err != nil || !strings.Contains(out, "Hello") {
		t.Fatalf("outline of a repo path: %v %s", err, out)
	}

	commitFiles(t, work, map[string]string{"a.txt": "two\n"}, "second")
	os.Chtimes(repoMarker(repoBase("acme", "demo"), "fetched"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	p, _ = expandRead("acme/demo/a.txt")
	if got := readT(t, p); got != "one\n" {
		t.Fatalf("a copy is never updated on its own, even an hour old; got %q", got)
	}
	if out, err := updateRepoCopy("acme", "demo"); err != nil || !strings.Contains(out, "updated acme/demo, now at") || !strings.Contains(out, "second") {
		t.Fatalf("update: %v %s", err, out)
	}
	if p, err = expandRead("acme/demo/a.txt"); err != nil || readT(t, p) != "two\n" {
		t.Fatalf("an update fast-forwards: %v", err)
	}
	if out, err := updateRepoCopy("acme", "demo"); err != nil || !strings.Contains(out, "already up to date") {
		t.Fatalf("second update: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("three\n"), 0o644)
	gitT(t, work, "commit", "-q", "-a", "--amend", "-m", "rewritten")
	gitT(t, work, "push", "-q", "--force", "origin", "HEAD")
	updateRepoCopy("acme", "demo")
	if p, err = expandRead("acme/demo/a.txt"); err != nil || readT(t, p) != "three\n" {
		t.Fatalf("rewritten history resets to origin: %v", err)
	}

	// git clone into a folder that already is a clone of it: updated, not refused
	dest := filepath.Join(t.TempDir(), "mine")
	gitT(t, hub, "clone", "-q", repoURL("acme", "demo"), dest)
	commitFiles(t, work, map[string]string{"c.txt": "new\n"}, "third")
	out, err := gitClone(gitArgs{URL: repoURL("acme", "demo"), To: dest})
	if err != nil || !strings.Contains(out, "already cloned") || !strings.Contains(out, "third") {
		t.Fatalf("clone into an existing clone: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "c.txt")); err != nil {
		t.Fatal("existing clone was not updated")
	}
	// without to: the temporary copy, with path, branch and last commit (not updated)
	out, err = gitClone(gitArgs{URL: "acme/demo"})
	if err != nil || !strings.Contains(shortRepoPaths(out), "path: acme/demo\n") || !strings.Contains(out, "branch: main") || !strings.Contains(out, "rewritten") {
		t.Fatalf("clone to the repos folder: %v\n%s", err, out)
	}
	// git pull on the copy is how the user has it updated
	out, _, err = callTool("git", json.RawMessage(`{"action":"pull","path":"acme/demo"}`))
	if err != nil || !strings.Contains(out, "updated acme/demo, now at") || !strings.Contains(out, "third") {
		t.Fatalf("pull on the copy: %v\n%s", err, out)
	}
}

// @ref gives a worktree per branch, tag or commit; compare works across refs.
func TestRepoRef(t *testing.T) {
	hub := repoTestEnv(t)
	work := newHubRepo(t, hub, "acme", "demo", map[string]string{"v.txt": "1\n"})
	gitT(t, work, "tag", "v1")
	gitT(t, work, "push", "-q", "origin", "v1")
	commitFiles(t, work, map[string]string{"v.txt": "2\n"}, "two")
	sha := gitT(t, work, "rev-parse", "HEAD")
	gitT(t, work, "checkout", "-q", "-b", "dev")
	commitFiles(t, work, map[string]string{"d.txt": "dev\n"}, "dev")

	p, err := expandRead("acme/demo@v1/v.txt")
	if err != nil || readT(t, p) != "1\n" {
		t.Fatalf("tag: %v", err)
	}
	if want := filepath.Join(reposDir(), "acme", "demo@v1", "v.txt"); p != want {
		t.Fatalf("got %s, want %s", p, want)
	}
	if p, err = expandRead("acme/demo/v.txt"); err != nil || readT(t, p) != "2\n" {
		t.Fatalf("default branch: %v", err)
	}
	if p, err = expandRead("acme/demo@" + sha[:10] + "/v.txt"); err != nil || readT(t, p) != "2\n" {
		t.Fatalf("commit: %v", err)
	}
	if p, err = expandRead("acme/demo@dev/d.txt"); err != nil || readT(t, p) != "dev\n" {
		t.Fatalf("branch: %v", err)
	}
	commitFiles(t, work, map[string]string{"d.txt": "dev2\n"}, "dev2")
	if p, err = expandRead("acme/demo@dev/d.txt"); err != nil || readT(t, p) != "dev\n" {
		t.Fatalf("a branch worktree stays as it is until an update: %v", err)
	}
	if _, err := updateRepoCopy("acme", "demo"); err != nil {
		t.Fatal(err)
	}
	if p, err = expandRead("acme/demo@dev/d.txt"); err != nil || readT(t, p) != "dev2\n" {
		t.Fatalf("after an update the branch worktree moves to its newest commit: %v", err)
	}
	if _, err := expandRead("acme/demo@nope/v.txt"); err == nil {
		t.Fatal("an unknown ref must fail")
	}

	out, _, err := callTool("git", json.RawMessage(`{"action":"compare","path":"acme/demo@v1","to":"acme/demo@dev"}`))
	if err != nil || !strings.Contains(out, "A acme/demo@v1 (1 files) vs B acme/demo@dev (2 files)") || !strings.Contains(out, "v.txt +1 -1") || !strings.Contains(out, "only in B: d.txt") {
		t.Fatalf("compare two refs: %v\n%s", err, out)
	}
}

// The copies are read-only, and results name them owner/name, never the local path.
func TestRepoCopyReadOnly(t *testing.T) {
	hub := repoTestEnv(t)
	newHubRepo(t, hub, "acme", "demo", map[string]string{"a.txt": "1\n"})
	for _, action := range []string{"commit", "push", "release"} {
		if _, _, err := callTool("git", json.RawMessage(`{"action":"`+action+`","path":"acme/demo","message":"x","version":"9.9.9"}`)); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s on a temporary copy: %v", action, err)
		}
	}
	out, _, err := callTool("git", json.RawMessage(`{"action":"status","path":"acme/demo"}`))
	if err != nil || !strings.HasPrefix(out, "acme/demo\n") || strings.Contains(out, reposDir()) {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if _, err := ckTargetPath("acme/demo", true); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("check fix on a temporary copy: %v", err)
	}
}

func ckTargetPath(p string, fix bool) (string, error) {
	root, _, _, _, err := ckTarget(&checkArgs{Path: p, Fix: fix})
	return root, err
}

// Copies unused for 24 h are deleted by ContextSaver itself: no approval, nothing
// outside data/repos, and never a copy (or a base whose worktree) is still in use.
func TestRepoCleanup(t *testing.T) {
	repoTestEnv(t)
	mk := func(rel string, age time.Duration) string {
		p := filepath.Join(reposDir(), filepath.FromSlash(rel))
		os.MkdirAll(filepath.Join(p, ".git"), 0o755)
		os.WriteFile(filepath.Join(p, "f.txt"), []byte("x"), 0o444)
		m := repoMarker(p, "used")
		repoTouch(m)
		old := time.Now().Add(-age)
		os.Chtimes(m, old, old)
		return p
	}
	oldBase, oldWT := mk("acme/old", 25*time.Hour), mk("acme/old@v1", 25*time.Hour)
	fresh := mk("acme/fresh", time.Hour)
	kept, keptWT := mk("acme/kept", 25*time.Hour), mk("acme/kept@dev", time.Minute)
	outside := filepath.Join(appHome, "mine")
	os.MkdirAll(outside, 0o755)
	ancient := time.Now().Add(-1000 * time.Hour)
	os.Chtimes(outside, ancient, ancient)

	removed := cleanRepos(24 * time.Hour)
	if strings.Join(removed, " ") != "acme/old@v1 acme/old" {
		t.Fatalf("removed %v", removed)
	}
	for _, p := range []string{oldBase, oldWT, repoMarker(oldBase, "used"), repoMarker(oldWT, "used")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", p)
		}
	}
	for _, p := range []string{fresh, kept, keptWT, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should be kept", p)
		}
	}
	if ents, _ := os.ReadDir(approvalsDir()); len(ents) > 0 {
		t.Fatalf("cleanup asked for approval: %d", len(ents))
	}
}
