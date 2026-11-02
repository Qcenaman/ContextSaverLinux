package main

// GitHub repos as paths: owner/name[@ref][/sub/path] in find, read, ls, check, outline
// and git means that repo. ContextSaver keeps its own temporary copies in data/repos
// (never TokenSaver's, never the user's own clones): a partial clone (file contents only
// when needed, full history for tags and compare), one worktree <name>@<ref> per ref
// sharing its objects, refreshed when older than 5 minutes and deleted by ContextSaver
// itself after 24 hours unused. Results show them as owner/name/..., which can be
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
	repoFresh     = 5 * time.Minute       // older copies fetch and fast-forward before use
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
// time, refreshed when older than repoFresh, "still cloning" when a big first clone
// takes longer than repoCloneWait.
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
	refreshBase(base)
	if ref == "" {
		return base, nil
	}
	return ensureWorktree(base, ref)
}

// refreshBase fetches and fast-forwards the main copy when older than repoFresh, or
// resets it to origin when history was rewritten. Offline, the copy is used as it is.
func refreshBase(base string) {
	fm := repoMarker(base, "fetched")
	if repoAge(fm) < repoFresh {
		return
	}
	unlock, ok := tryRepoLock(base)
	if !ok {
		return // another call is updating it right now
	}
	defer unlock()
	if repoAge(fm) < repoFresh {
		return
	}
	repoTouch(fm)
	args := append(append([]string{}, gitNetFlags...), "fetch", "--prune", "--tags", "--force", "--quiet", "origin")
	if _, code, err := gitExec(base, 5*time.Minute, args...); err != nil || code != 0 {
		return
	}
	br, code, _ := gitExec(base, 20*time.Second, "symbolic-ref", "--short", "-q", "HEAD")
	if code != 0 || strings.TrimSpace(br) == "" {
		return
	}
	up := "origin/" + strings.TrimSpace(br)
	if _, code, _ := gitExec(base, 10*time.Minute, "merge", "--ff-only", "--quiet", up); code != 0 {
		gitExec(base, 10*time.Minute, "reset", "--hard", "--quiet", up)
	}
}

func safeRef(ref string) string { return unsafeRefRe.ReplaceAllString(ref, "_") }

// ensureWorktree returns <name>@<ref>: a worktree of base at ref (branch, tag or commit)
// sharing base's objects; a branch moves to its newest commit once older than repoFresh.
func ensureWorktree(base, ref string) (string, error) {
	wt := filepath.Join(filepath.Dir(base), filepath.Base(base)+"@"+safeRef(ref))
	fm := repoMarker(wt, "fetched")
	have := isGitWork(wt)
	if have && repoAge(fm) < repoFresh {
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
