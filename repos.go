package main

// GitHub repos as paths: owner/name[@ref][/sub/path] in find, read, ls, check, outline
// and git means that repo. ContextSaver keeps its own temporary copies in data/repos
// (never TokenSaver's, never the user's own clones): a partial clone (file contents only
// when needed, full history for tags and compare), one worktree <name>@<ref> per ref
// sharing its objects. A copy is never updated on its own: only when the user says yes
// (asked once per chat when a request uses a copy that exists, or git pull on it). Copies
// are deleted by ContextSaver itself after 24 hours unused. Results show them as owner/name/..., which can be
// passed straight back. The copies are read-only: commit, push and release are refused.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	repoMaxIdle   = 24 * time.Hour        // copies unused this long are deleted
	repoCloneWait = 50 * time.Second      // a first clone answers "still cloning" after this
	repoURLBase   = "https://github.com/" // tests point this at local repos
)

func reposDir() string { return filepath.Join(appHome, "repos") }

type repoSpec struct{ Owner, Name, Ref, Sub string }

var (
	repoOwnerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	repoRefRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
	unsafeRefRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// parseRepoPath reads owner/name[@ref][/sub/path], also as github.com/... or a GitHub
// URL (.../tree/<ref>/sub). Absolute, ~, %VAR% and Windows paths are never repos.
func parseRepoPath(p string) (repoSpec, bool) {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
	fromURL := false
	for _, pre := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
		if len(p) > len(pre) && strings.EqualFold(p[:len(pre)], pre) {
			p, fromURL = p[len(pre):], true
			break
		}
	}
	if p == "" || strings.ContainsAny(p[:1], `/\~.%$`) || strings.ContainsAny(p, `\:`) || isWinPath(p) {
		return repoSpec{}, false
	}
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		return repoSpec{}, false
	}
	s := repoSpec{Owner: parts[0], Name: parts[1]}
	if len(parts) == 3 {
		s.Sub = strings.Trim(parts[2], "/")
	}
	if i := strings.Index(s.Name, "@"); i >= 0 {
		s.Name, s.Ref = s.Name[:i], s.Name[i+1:]
		if !repoRefRe.MatchString(s.Ref) {
			return repoSpec{}, false
		}
	}
	s.Name = strings.TrimSuffix(s.Name, ".git")
	if !repoOwnerRe.MatchString(s.Owner) || !repoNameRe.MatchString(s.Name) || s.Name == "." || s.Name == ".." {
		return repoSpec{}, false
	}
	if fromURL && s.Ref == "" {
		if q := strings.SplitN(s.Sub, "/", 3); len(q) >= 2 && (q[0] == "tree" || q[0] == "blob") && repoRefRe.MatchString(q[1]) {
			s.Ref, s.Sub = q[1], ""
			if len(q) == 3 {
				s.Sub = q[2]
			}
		}
	}
	return s, true
}

func isRepoPath(p string) bool {
	_, ok := parseRepoPath(p)
	return ok
}

// expandRead is expandPath for the tools that only read: owner/name[@ref][/sub] becomes
// that GitHub repo's local copy (cloned or refreshed first).
func expandRead(p string) (string, error) {
	if s, ok := parseRepoPath(p); ok {
		return repoLocal(s)
	}
	q, err := expandPath(p)
	if err != nil && strings.Contains(err.Error(), "must be absolute") {
		err = errors.New(err.Error() + "; a GitHub repo: owner/name[@ref][/sub]")
	}
	return q, err
}

func repoLocal(s repoSpec) (string, error) {
	dir, err := ensureRepo(s.Owner, s.Name, s.Ref)
	if err != nil {
		return "", err
	}
	if s.Sub == "" {
		return dir, nil
	}
	p := filepath.Join(dir, filepath.FromSlash(s.Sub))
	if !hasPathPrefix(p, dir) {
		return "", fmt.Errorf("%s leaves the repo", s.Sub)
	}
	return p, nil
}

// isRepoCopy reports whether p is inside one of ContextSaver's temporary repo copies.
func isRepoCopy(p string) bool {
	return appHome != "" && hasPathPrefix(p, reposDir()) && !samePath(p, reposDir())
}

// shortRepoPaths shows the copies as owner/name[@ref]/... instead of long local paths.
func shortRepoPaths(s string) string {
	if appHome == "" || s == "" {
		return s
	}
	r := reposDir() + string(filepath.Separator)
	if !strings.Contains(s, r) {
		return s
	}
	return strings.ReplaceAll(s, r, "")
}

func shortRepoErr(err error) error {
	if err == nil {
		return nil
	}
	if s := shortRepoPaths(err.Error()); s != err.Error() {
		return errors.New(s)
	}
	return err
}

// repoBase is data/repos/<owner>/<name>, reusing a copy whose name differs only in case
// (GitHub names are case-insensitive).
func repoBase(owner, name string) string {
	return ciChild(ciChild(reposDir(), owner), name)
}

func ciChild(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Lstat(p); err == nil {
		return p
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() && strings.EqualFold(e.Name(), name) {
				return filepath.Join(dir, e.Name())
			}
		}
	}
	return p
}

// repoMarker is a hidden file next to a copy: .<copy>.used, .fetched, .lock, .cloning.
// Kept outside the copy so find, ls and compare never see it.
func repoMarker(dir, kind string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+"."+kind)
}

func repoTouch(p string) {
	now := time.Now()
	if os.Chtimes(p, now, now) != nil {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
}

// repoAge is how long ago a marker was touched (very long when it doesn't exist).
func repoAge(p string) time.Duration {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Duration(1 << 62)
	}
	return time.Since(fi.ModTime())
}

func isGitWork(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// tryRepoLock takes .<name>.lock (holding the pid) so two calls, or two ContextSaver
// processes, never clone or change the same repo's copies at once.
func tryRepoLock(base string) (func(), bool) {
	lp := repoMarker(base, "lock")
	os.MkdirAll(filepath.Dir(lp), 0o755)
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprint(f, os.Getpid())
			f.Close()
			return func() { os.Remove(lp) }, true
		}
		if !repoLockStale(lp) {
			return nil, false
		}
		os.Remove(lp)
	}
	return nil, false
}

func repoLockStale(lp string) bool {
	fi, err := os.Stat(lp)
	if err != nil {
		return true
	}
	age := time.Since(fi.ModTime())
	if age > 6*time.Hour {
		return true
	}
	b, _ := os.ReadFile(lp)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		return age > time.Minute
	}
	return pid != os.Getpid() && !pidAlive(pid)
}

func waitRepoLock(base string, d time.Duration) (func(), bool) {
	end := time.Now().Add(d)
	for {
		if unlock, ok := tryRepoLock(base); ok {
			return unlock, true
		}
		if time.Now().After(end) {
			return nil, false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

type cloneTask struct {
	done chan struct{}
	err  error
}

var (
	cloneMu sync.Mutex
	clones  = map[string]*cloneTask{}
)

// startClone clones owner/name into base in the background; a call made while it is
// still cloning gets the same task instead of starting another clone.
func startClone(owner, name, base string) *cloneTask {
	key := strings.ToLower(base)
	cloneMu.Lock()
	defer cloneMu.Unlock()
	if t, ok := clones[key]; ok {
		return t
	}
	t := &cloneTask{done: make(chan struct{})}
	clones[key] = t
	go func() {
		t.err = cloneRepo(owner, name, base)
		cloneMu.Lock()
		delete(clones, key)
		cloneMu.Unlock()
		close(t.done)
	}()
	return t
}

func repoURL(owner, name string) string { return repoURLBase + owner + "/" + name + ".git" }

// cloneRepo makes the partial clone (--filter=blob:none) next to base and moves it into
// place when complete, so a half-done clone is never used. Uses the GitHub sign-in.
func cloneRepo(owner, name, base string) error {
	unlock, ok := waitRepoLock(base, 6*time.Hour)
	if !ok {
		return fmt.Errorf("%s/%s is locked by another ContextSaver, call again", owner, name)
	}
	defer unlock()
	if isGitWork(base) {
		return nil // another process cloned it meanwhile
	}
	tmp := repoMarker(base, "cloning")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return err
	}
	args := append(append([]string{}, gitNetFlags...), "clone", "--filter=blob:none", "--quiet", repoURL(owner, name), tmp)
	out, code, err := gitExec(filepath.Dir(base), 30*time.Minute, args...)
	if err == nil && code != 0 {
		err = fmt.Errorf("could not clone %s/%s: %s", owner, name, cloneHint(out))
	}
	if err == nil {
		err = os.Rename(tmp, base)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}
	repoTouch(repoMarker(base, "fetched"))
	return nil
}

func cloneHint(out string) string {
	lo := strings.ToLower(out)
	switch {
	case strings.Contains(lo, "authentication") || strings.Contains(lo, "could not read username") || strings.Contains(lo, "403"):
		return "GitHub sign-in needed: git{action:login}, then call again"
	case strings.Contains(lo, "not found") || strings.Contains(lo, "does not exist"):
		return "no such GitHub repo (a folder on the PC needs an absolute path)"
	}
	return gitShort(out, 4)
}

// ensureRepo returns the local copy of owner/name (at ref when given): cloned the first
// time a request needs it, else used as it is (updates come only from updateRepoCopy);
// "still cloning" when a big first clone takes longer than repoCloneWait.
func ensureRepo(owner, name, ref string) (string, error) {
	if appHome == "" {
		return "", errors.New("ContextSaver's data folder is not set")
	}
	cleanReposSoon()
	base := repoBase(owner, name)
	if !isGitWork(base) {
		t := startClone(owner, name, base)
		select {
		case <-t.done:
			if t.err != nil {
				return "", t.err
			}
		case <-time.After(repoCloneWait):
			return "", fmt.Errorf("still cloning %s/%s (a big repo), call again in a minute", owner, name)
		}
	}
	repoTouch(repoMarker(base, "used"))
	if ref == "" {
		return base, nil
	}
	return ensureWorktree(base, ref)
}

// updateRepoCopy fetches owner/name's copy and fast-forwards it, or resets it to origin
// when history was rewritten; its branch worktrees follow on their next use. Runs only
// on the user's yes or git pull on the copy, never on its own.
func updateRepoCopy(owner, name string) (string, error) {
	base := repoBase(owner, name)
	if !isGitWork(base) {
		return "", fmt.Errorf("there is no copy of %s/%s yet", owner, name)
	}
	unlock, ok := waitRepoLock(base, repoCloneWait)
	if !ok {
		return "", fmt.Errorf("%s/%s is busy, call again", owner, name)
	}
	defer unlock()
	fm := repoMarker(base, "fetched")
	before := repoLastCommit(base)
	args := append(append([]string{}, gitNetFlags...), "fetch", "--prune", "--tags", "--force", "--quiet", "origin")
	if out, code, err := gitExec(base, 5*time.Minute, args...); err != nil || code != 0 {
		why := gitShort(out, 3)
		if err != nil {
			why = err.Error()
		}
		return "", fmt.Errorf("couldn't update %s/%s (%s); the copy from %s is used as it is", owner, name, why, agoText(repoAge(fm)))
	}
	if br, code, _ := gitExec(base, 20*time.Second, "symbolic-ref", "--short", "-q", "HEAD"); code == 0 && strings.TrimSpace(br) != "" {
		up := "origin/" + strings.TrimSpace(br)
		if _, code, _ := gitExec(base, 10*time.Minute, "merge", "--ff-only", "--quiet", up); code != 0 {
			gitExec(base, 10*time.Minute, "reset", "--hard", "--quiet", up)
		}
	}
	repoTouch(fm)
	if after := repoLastCommit(base); after != before {
		return fmt.Sprintf("updated %s/%s, now at %s", owner, name, after), nil
	}
	return fmt.Sprintf("%s/%s was already up to date", owner, name), nil
}

func repoLastCommit(dir string) string {
	out, _, _ := gitExec(dir, 20*time.Second, "log", "-1", "--date=short", "--format=%h %ad %s")
	return strings.TrimSpace(out)
}

func safeRef(ref string) string { return unsafeRefRe.ReplaceAllString(ref, "_") }

// ensureWorktree returns <name>@<ref>: a worktree of base at ref (branch, tag or commit)
// sharing base's objects; a branch moves to its newest commit after base was updated.
func ensureWorktree(base, ref string) (string, error) {
	wt := filepath.Join(filepath.Dir(base), filepath.Base(base)+"@"+safeRef(ref))
	fm := repoMarker(wt, "fetched")
	have := isGitWork(wt)
	if have && repoAge(fm) < repoAge(repoMarker(base, "fetched")) {
		repoTouch(repoMarker(wt, "used"))
		return wt, nil
	}
	unlock, ok := waitRepoLock(base, repoCloneWait)
	if !ok {
		if have {
			repoTouch(repoMarker(wt, "used"))
			return wt, nil
		}
		return "", fmt.Errorf("%s is busy (being updated), call again", shortRepoPaths(base))
	}
	defer unlock()
	commit, err := resolveRef(base, ref)
	switch {
	case err != nil && have:
		// offline: keep what is there
	case err != nil:
		return "", err
	case have:
		if head, _, _ := gitExec(wt, 20*time.Second, "rev-parse", "HEAD"); strings.TrimSpace(head) != commit {
			gitExec(wt, 10*time.Minute, "checkout", "--detach", "--force", "--quiet", commit)
		}
	default:
		if _, err := os.Lstat(wt); err == nil {
			removeRepoCopy(wt)
		}
		gitExec(base, 30*time.Second, "worktree", "prune")
		out, code, err := gitExec(base, 10*time.Minute, "worktree", "add", "--detach", "--force", "--quiet", wt, commit)
		if err == nil && code != 0 {
			err = fmt.Errorf("could not check out %s: %s", ref, gitShort(out, 3))
		}
		if err != nil {
			removeRepoCopy(wt)
			gitExec(base, 30*time.Second, "worktree", "prune")
			return "", err
		}
	}
	repoTouch(fm)
	repoTouch(repoMarker(wt, "used"))
	return wt, nil
}

// resolveRef finds ref as a branch on origin, a tag or a commit, fetching it when the
// copy doesn't have it yet.
func resolveRef(base, ref string) (string, error) {
	for _, r := range []string{"refs/remotes/origin/" + ref, "refs/tags/" + ref, ref} {
		if out, code, err := gitExec(base, 20*time.Second, "rev-parse", "--verify", "--quiet", r+"^{commit}"); err == nil && code == 0 && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out), nil
		}
	}
	args := append(append([]string{}, gitNetFlags...), "fetch", "--quiet", "origin", ref)
	if _, code, err := gitExec(base, 5*time.Minute, args...); err == nil && code == 0 {
		if out, code, _ := gitExec(base, 20*time.Second, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}"); code == 0 {
			return strings.TrimSpace(out), nil
		}
	}
	return "", fmt.Errorf("%s not found in %s (give a branch, tag or commit)", ref, shortRepoPaths(base))
}

// repoInfo: path, branch and last commit of a clone (git clone's answer).
func repoInfo(dir string) string {
	br, _, _ := gitExec(dir, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if br = strings.TrimSpace(br); br == "HEAD" {
		br = "(detached)"
	}
	last, _, _ := gitExec(dir, 20*time.Second, "log", "-1", "--date=short", "--format=%h %ad %s")
	return fmt.Sprintf("path: %s\nbranch: %s\nlast commit: %s", dir, br, strings.TrimSpace(last))
}

// normRemote reduces a git URL to host/owner/name for comparing two of them.
func normRemote(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "file://", "git@"} {
		u = strings.TrimPrefix(u, p)
	}
	if i := strings.Index(u, "@"); i >= 0 && i < strings.Index(u+"/", "/") {
		u = u[i+1:]
	}
	u = strings.Replace(u, ":", "/", 1)
	return strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
}

var lastRepoClean atomic.Int64

// cleanReposSoon runs cleanRepos in the background, at most once an hour.
func cleanReposSoon() {
	now := time.Now().Unix()
	last := lastRepoClean.Load()
	if now-last < 3600 || !lastRepoClean.CompareAndSwap(last, now) {
		return
	}
	go cleanRepos(repoMaxIdle)
}

// cleanRepos deletes the copies not used for maxIdle. This is ContextSaver's own
// housekeeping of its temporary folder, not a user delete: it never asks for approval and
// never touches anything outside data/repos. Returns what it removed (owner/name[@ref]).
func cleanRepos(maxIdle time.Duration) []string {
	var removed []string
	owners, _ := os.ReadDir(reposDir())
	for _, o := range owners {
		if !o.IsDir() || strings.HasPrefix(o.Name(), ".") {
			continue
		}
		od := filepath.Join(reposDir(), o.Name())
		ents, _ := os.ReadDir(od)
		// worktrees (name@ref) before their base: a base goes only when all of its are gone
		sort.SliceStable(ents, func(i, j int) bool {
			return strings.Contains(ents[i].Name(), "@") && !strings.Contains(ents[j].Name(), "@")
		})
		for _, e := range ents {
			n := e.Name()
			if strings.HasPrefix(n, ".") || !e.IsDir() {
				continue
			}
			p := filepath.Join(od, n)
			if repoIdle(p) < maxIdle {
				continue
			}
			baseName, _, isWT := strings.Cut(n, "@")
			base := filepath.Join(od, baseName)
			if !isWT && hasWorktrees(od, n) {
				continue
			}
			unlock, ok := tryRepoLock(base)
			if !ok {
				continue // in use right now
			}
			if removeRepoCopy(p) {
				removed = append(removed, o.Name()+"/"+n)
				if isWT && isGitWork(base) {
					gitExec(base, 30*time.Second, "worktree", "prune")
				}
			}
			unlock()
		}
		// leftovers: an interrupted clone, markers of copies that are gone
		ents, _ = os.ReadDir(od)
		for _, e := range ents {
			n := e.Name()
			p := filepath.Join(od, n)
			switch {
			case strings.HasSuffix(n, ".cloning") && strings.HasPrefix(n, ".") && repoIdle(p) > 6*time.Hour:
				base := filepath.Join(od, strings.TrimSuffix(n[1:], ".cloning"))
				if unlock, ok := tryRepoLock(base); ok {
					os.RemoveAll(p)
					unlock()
				}
			case strings.HasPrefix(n, ".") && (strings.HasSuffix(n, ".used") || strings.HasSuffix(n, ".fetched")):
				c := strings.TrimSuffix(strings.TrimSuffix(n[1:], ".used"), ".fetched")
				if _, err := os.Stat(filepath.Join(od, c)); os.IsNotExist(err) {
					os.Remove(p)
				}
			}
		}
		os.Remove(od) // only when empty
	}
	return removed
}

// repoIdle: time since a copy was last used (its folder's age when it has no marker).
func repoIdle(p string) time.Duration {
	if d := repoAge(repoMarker(p, "used")); d != time.Duration(1<<62) {
		return d
	}
	return repoAge(p)
}

func hasWorktrees(od, name string) bool {
	ents, _ := os.ReadDir(od)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), name+"@") {
			return true
		}
	}
	return false
}

// removeRepoCopy deletes one copy, only ever a folder exactly at data/repos/<owner>/<copy>.
func removeRepoCopy(p string) bool {
	root := filepath.Clean(reposDir())
	p = filepath.Clean(p)
	if appHome == "" || !filepath.IsAbs(p) || filepath.Dir(filepath.Dir(p)) != root || !hasPathPrefix(p, root) {
		return false
	}
	if os.RemoveAll(p) != nil {
		return false
	}
	os.Remove(repoMarker(p, "used"))
	os.Remove(repoMarker(p, "fetched"))
	return true
}

// ---------- asking before an update ----------

// repoCommands are the commands that read ContextSaver's repo copies.
var repoCommands = map[string]bool{"find": true, "search": true, "ls": true, "scan": true, "dir": true, "list": true,
	"read": true, "cat": true, "git": true, "check": true, "lint": true, "outline": true, "symbols": true, "compare": true}

// reposToAsk: the GitHub repos a call uses whose copy exists and that this chat hasn't
// settled yet; settled (owner/name in lower case) records them as handled. A repo
// without a copy is settled right away (the call clones it fresh), and so is one the
// call itself updates (git pull).
func reposToAsk(c string, a map[string]any, settled map[string]bool) []repoSpec {
	if appHome == "" || !repoCommands[c] {
		return nil
	}
	act := strings.ToLower(str(a, "action"))
	if c == "git" && act == "clone" && str(a, "to") != "" {
		return nil // a clone into a folder of the user's own, not the copy
	}
	var out []repoSpec
	for _, p := range repoArgPaths(a) {
		s, ok := parseRepoPath(p)
		k := strings.ToLower(s.Owner + "/" + s.Name)
		if !ok || settled[k] {
			continue
		}
		settled[k] = true
		if !(c == "git" && act == "pull") && isGitWork(repoBase(s.Owner, s.Name)) {
			out = append(out, s)
		}
	}
	return out
}

// repoArgPaths: the path-like arguments of a call, under any of the names the commands
// accept for them.
func repoArgPaths(a map[string]any) []string {
	var ps []string
	for _, k := range []string{"path", "paths", "file", "files", "folder", "dir", "in", "to", "with", "against", "other", "a", "b", "from", "url", "root"} {
		switch v := a[k].(type) {
		case string:
			ps = append(ps, v)
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					ps = append(ps, s)
				}
			}
		case []string:
			ps = append(ps, v...)
		}
	}
	return ps
}

var (
	repoNotedMu sync.Mutex
	repoNoted   = map[string]bool{}
)

// repoUpdateNotes is for apps that call ContextSaver over MCP, where there is no terminal
// to ask in: the first call using a copy in a session says how old it is and how the
// user can have it updated. The copy itself is never updated on its own.
func repoUpdateNotes(c string, a map[string]any) string {
	repoNotedMu.Lock()
	ss := reposToAsk(c, a, repoNoted)
	repoNotedMu.Unlock()
	var b strings.Builder
	for _, s := range ss {
		r := s.Owner + "/" + s.Name
		fmt.Fprintf(&b, "note: the copy of %s was last updated %s; it isn't updated on its own. If the user wants it updated first: git{action:pull,path:%q}\n", r, repoUpdatedAgo(s), r)
	}
	return b.String()
}

func repoUpdatedAgo(s repoSpec) string {
	return agoText(repoAge(repoMarker(repoBase(s.Owner, s.Name), "fetched")))
}

// agoText: "just now", "12 min ago", "3 h ago", "2 days ago".
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < time.Duration(1<<62):
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return "at an unknown time"
}

// repoOfCopy names the GitHub repo that a folder in data/repos is a copy of.
func repoOfCopy(dir string) (owner, name string) {
	rel, err := filepath.Rel(reposDir(), dir)
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	name, _, _ = strings.Cut(parts[1], "@")
	return parts[0], name
}
