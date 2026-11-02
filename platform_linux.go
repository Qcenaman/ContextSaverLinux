package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------- Trash (freedesktop.org), Linux's Recycle Bin ----------

func homeTrashDir() string {
	d := os.Getenv("XDG_DATA_HOME")
	if d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".local", "share")
	}
	return filepath.Join(d, "Trash")
}

func devID(p string) (uint64, bool) {
	var st syscall.Stat_t
	if syscall.Lstat(p, &st) != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// mountRoot climbs from p to the top folder on the same filesystem.
func mountRoot(p string) string {
	d, ok := devID(p)
	if !ok {
		return ""
	}
	for {
		up := filepath.Dir(p)
		if up == p {
			return p
		}
		if u, ok := devID(up); !ok || u != d {
			return p
		}
		p = up
	}
}

// trashDirFor: the home Trash when p is on the same disk, else <disk>/.Trash-<uid>
// (how the Files app trashes on other disks, e.g. the Windows partition). "" = no Trash.
func trashDirFor(p string) string {
	d, ok := devID(p)
	if !ok {
		return ""
	}
	home := homeTrashDir()
	if os.MkdirAll(filepath.Join(home, "files"), 0o700) == nil && os.MkdirAll(filepath.Join(home, "info"), 0o700) == nil {
		if hd, ok := devID(home); ok && hd == d {
			return home
		}
	}
	top := mountRoot(p)
	if top == "" || top == "/" {
		return ""
	}
	t := filepath.Join(top, ".Trash-"+strconv.Itoa(os.Getuid()))
	if os.MkdirAll(filepath.Join(t, "files"), 0o700) != nil || os.MkdirAll(filepath.Join(t, "info"), 0o700) != nil {
		return ""
	}
	return t
}

func useRecycleBin(p string) bool { return trashDirFor(p) != "" }

func trashOne(p string) error {
	p, _ = filepath.Abs(p)
	t := trashDirFor(p)
	if t == "" {
		return errors.New("no Trash on this disk")
	}
	rec := p // Path= is absolute in the home Trash, relative to the disk elsewhere
	if t != homeTrashDir() {
		if r, err := filepath.Rel(filepath.Dir(t), p); err == nil {
			rec = r
		}
	}
	parts := strings.Split(rec, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	base, ext := filepath.Base(p), filepath.Ext(p)
	for n := 1; n < 10000; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s.%d%s", strings.TrimSuffix(base, ext), n, ext)
		}
		info := filepath.Join(t, "info", name+".trashinfo")
		f, err := os.OpenFile(info, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "[Trash Info]\nPath=%s\nDeletionDate=%s\n", strings.Join(parts, "/"), time.Now().Format("2006-01-02T15:04:05"))
		f.Close()
		if err := os.Rename(p, filepath.Join(t, "files", name)); err != nil {
			os.Remove(info)
			return err
		}
		return nil
	}
	return errors.New("too many items with this name in the Trash")
}

func recycleBatch(paths []string) error {
	var bad []string
	for _, p := range paths {
		if err := trashOne(p); err != nil {
			bad = append(bad, filepath.Base(p)+": "+err.Error())
		}
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

// restoreRecycled puts back the trashed copy of orig whose deletion time is closest to at.
func restoreRecycled(orig string, at time.Time) error {
	orig, _ = filepath.Abs(orig)
	if exists(orig) {
		return fmt.Errorf("%s exists again; restore it from the Trash by hand", orig)
	}
	cands := []string{homeTrashDir()}
	if top := mountRoot(filepath.Dir(orig)); top != "" && top != "/" {
		cands = append(cands, filepath.Join(top, ".Trash-"+strconv.Itoa(os.Getuid())))
	}
	var best, bestInfo string
	gap := time.Duration(1<<63 - 1)
	for _, t := range cands {
		infos, _ := filepath.Glob(filepath.Join(t, "info", "*.trashinfo"))
		for _, ip := range infos {
			p, when := parseTrashInfo(t, ip)
			if p != orig {
				continue
			}
			g := when.Sub(at)
			if g < 0 {
				g = -g
			}
			if g < gap {
				gap, bestInfo = g, ip
				best = filepath.Join(t, "files", strings.TrimSuffix(filepath.Base(ip), ".trashinfo"))
			}
		}
	}
	if best == "" {
		return errors.New("not in the Trash (emptied?)")
	}
	os.MkdirAll(filepath.Dir(orig), 0o755)
	if err := os.Rename(best, orig); err != nil {
		return err
	}
	os.Remove(bestInfo)
	return nil
}

func parseTrashInfo(trash, ip string) (string, time.Time) {
	b, err := os.ReadFile(ip)
	if err != nil {
		return "", time.Time{}
	}
	var p string
	var when time.Time
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Path="); ok {
			if u, err := url.PathUnescape(v); err == nil {
				v = u
			}
			if !filepath.IsAbs(v) {
				v = filepath.Join(filepath.Dir(trash), v)
			}
			p = v
		} else if v, ok := strings.CutPrefix(l, "DeletionDate="); ok {
			when, _ = time.ParseInLocation("2006-01-02T15:04:05", strings.TrimSpace(v), time.Local)
		}
	}
	return p, when
}

func holdRoot(p string) string { return filepath.Join(appHome, "trash") }
func stripVol(p string) string { return strings.TrimPrefix(p, "/") }

// desktopDir follows xdg-user-dirs, so a translated Desktop folder is found too.
func desktopDir() string {
	h, _ := os.UserHomeDir()
	if f, err := os.Open(filepath.Join(h, ".config", "user-dirs.dirs")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "XDG_DESKTOP_DIR="); ok {
				return strings.ReplaceAll(strings.Trim(v, `"`), "$HOME", h)
			}
		}
	}
	return filepath.Join(h, "Desktop")
}

func setFileTimes(p string, st fs.FileInfo)   { os.Chtimes(p, st.ModTime(), st.ModTime()) }
func setFileAttrs(p string, st fs.FileInfo)   { os.Chmod(p, st.Mode().Perm()) }
func creationTime(info fs.FileInfo) time.Time { return info.ModTime() }
func isCrossDevice(err error) bool            { return errors.Is(err, syscall.EXDEV) }

func sameVolume(a, b string) bool {
	for !exists(a) && filepath.Dir(a) != a {
		a = filepath.Dir(a)
	}
	for !exists(b) && filepath.Dir(b) != b {
		b = filepath.Dir(b)
	}
	x, ok1 := devID(a)
	y, ok2 := devID(b)
	return !ok1 || !ok2 || x == y
}

// ---------- model start scripts ----------

type aiProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *aiProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// startScript runs a start script in its own process group (in its own session, so
// the server outlives the app that asked for it, like a new console on Windows).
func startScript(script string) (*aiProc, error) {
	cmd := exec.Command(bashBin(), script)
	cmd.Dir = filepath.Dir(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if f, err := os.OpenFile(filepath.Join(appHome, "model-server.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &aiProc{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(p.done) }()
	return p, nil
}

// stopGroup ends a process group gently, then hard after 10 s.
func stopGroup(pid int, done <-chan struct{}) {
	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func stopLocalAI(p *aiProc, port int) {
	if p.cmd.Process != nil && !p.exited() {
		stopGroup(p.cmd.Process.Pid, p.done)
	}
	// a script that launched the server separately: stop what still listens on its port
	for i := 0; i < 5; i++ {
		pid := pidOnPort(port)
		if pid <= 0 {
			return
		}
		killTree(pid)
		time.Sleep(time.Second)
	}
}

// stopOwnLocalAI stops a model this app started: the script's process group, and the
// server on port only while it is still the one this app started (ownPID).
func stopOwnLocalAI(p *aiProc, port, ownPID int) {
	if p.cmd.Process != nil && !p.exited() {
		stopGroup(p.cmd.Process.Pid, p.done)
	}
	if ownPID <= 0 {
		return
	}
	for i := 0; i < 5; i++ {
		if pidOnPort(port) != ownPID {
			return
		}
		killTree(ownPID)
		time.Sleep(time.Second)
	}
}

func quietCmd(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

// ---------- ports ----------

// listenInodes: socket inodes listening on TCP port (IPv4 and IPv6).
func listenInodes(port int) map[uint64]bool {
	out := map[uint64]bool{}
	for _, l := range tcpListeners() {
		if l.port == port {
			out[l.inode] = true
		}
	}
	return out
}

type tcpListen struct {
	addr  string
	port  int
	inode uint64
}

// tcpListeners reads /proc/net/tcp and tcp6 for sockets in LISTEN state.
func tcpListeners() []tcpListen {
	var out []tcpListen
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			x := strings.Fields(line)
			if i == 0 || len(x) < 10 || x[3] != "0A" { // 0A = LISTEN
				continue
			}
			addr, port := hexAddr(x[1])
			ino, _ := strconv.ParseUint(x[9], 10, 64)
			out = append(out, tcpListen{addr, port, ino})
		}
	}
	return out
}

// hexAddr decodes /proc/net's "0100007F:1F90" into ("127.0.0.1", 8080).
func hexAddr(s string) (string, int) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0
	}
	port, _ := strconv.ParseUint(p, 16, 16)
	if len(h) == 8 {
		v, _ := strconv.ParseUint(h, 16, 32)
		return fmt.Sprintf("%d.%d.%d.%d", v&0xff, v>>8&0xff, v>>16&0xff, v>>24), int(port)
	}
	switch h {
	case strings.Repeat("0", 32):
		return "::", int(port)
	case "00000000000000000000000001000000":
		return "::1", int(port)
	}
	return "[" + strings.ToLower(h) + "]", int(port)
}

// inodeOwners maps the given socket inodes to the pid holding them. Without root only
// this user's processes are visible, which covers every server this app starts.
func inodeOwners(want map[uint64]bool) map[uint64]int {
	owners := map[uint64]int{}
	if len(want) == 0 {
		return owners
	}
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if err != nil || !strings.HasPrefix(l, "socket:[") {
				continue
			}
			ino, err := strconv.ParseUint(l[8:len(l)-1], 10, 64)
			if err != nil || !want[ino] {
				continue
			}
			if _, seen := owners[ino]; !seen {
				owners[ino] = pid
			}
			if len(owners) == len(want) {
				return owners
			}
		}
	}
	return owners
}

// pidOnPort: the process listening on TCP port, 0 if none (or not visible to this user).
func pidOnPort(port int) int {
	inodes := listenInodes(port)
	for _, pid := range inodeOwners(inodes) {
		if pid > 0 {
			return pid
		}
	}
	return 0
}

// migrateHiddenRoot moves a 0.1.0 install from ~/.contextsaver into the app folder and
// points the saved model script at its new place.
func migrateHiddenRoot() {
	h, _ := os.UserHomeDir()
	old := filepath.Join(h, ".contextsaver")
	if os.Getenv("CONTEXTSAVER_ROOT") != "" || !exists(old) || samePath(old, appRoot()) {
		return
	}
	if err := moveTree(old, appRoot()); err != nil {
		fmt.Printf("Couldn't move %s to %s: %v\n", old, appRoot(), err)
		return
	}
	fmt.Println("Moved", old, "→", appRoot())
	appHome = filepath.Join(appRoot(), "data")
	if b, err := os.ReadFile(filepath.Join(appHome, "config.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.AIScript != "" && hasPathPrefix(cfg.AIScript, old) {
		cfg.AIScript = filepath.Join(appRoot(), strings.TrimPrefix(cfg.AIScript, old+"/"))
		saveConfig()
	}
}
