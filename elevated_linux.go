package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The admin helper on Linux: a root service that systemd starts when ContextSaver
// connects to its socket (socket activation) and that exits when idle, so admin commands
// need no password each time. Only the installing user can open the socket, and the
// helper also checks that the caller is that user's installed ContextSaver. The helper
// runs a root-owned copy of the program, so nothing the user's account can change runs
// as root. Every command goes to the admin log.

const (
	helperSock    = "/run/contextsaver-helper.sock"
	helperUnit    = "contextsaver-helper"
	helperUnitDir = "/etc/systemd/system"
	helperLibDir  = "/usr/local/lib/contextsaver"
)

func helperBin() string { return filepath.Join(helperLibDir, "contextsaver") }

func socketUnitPath() string  { return filepath.Join(helperUnitDir, helperUnit+".socket") }
func serviceUnitPath() string { return filepath.Join(helperUnitDir, helperUnit+".service") }

func auditPath() string { return filepath.Join(appRoot(), "admin-log", "elevated.log") }

var auditMu sync.Mutex

func audit(format string, a ...any) {
	auditMu.Lock()
	defer auditMu.Unlock()
	os.MkdirAll(filepath.Dir(auditPath()), 0o755)
	f, err := os.OpenFile(auditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func helperInstalled() bool { return exists(socketUnitPath()) }

// helperHowTo: the warning and the one way to fix it, the same wherever the helper is missing.
func helperHowTo() string {
	return "The admin helper isn't installed, so commands that need root (installing programs, system changes) won't work. " +
		"To install it once: open a Terminal (Ctrl+Alt+T) and type: contextsaver install-helper — it asks for your password one time."
}

// ---------- helper (root) ----------

var helperActive atomic.Int32

func serveElevatedHelper() int {
	if os.Geteuid() != 0 || os.Getenv("LISTEN_FDS") == "" {
		fmt.Println("The admin helper is started by systemd. Install it with: contextsaver install-helper")
		return 1
	}
	if pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID")); pid != os.Getpid() {
		fmt.Println("the systemd socket isn't for this process")
		return 1
	}
	allowed, err := strconv.Atoi(os.Getenv("CONTEXTSAVER_HELPER_UID"))
	if err != nil || allowed <= 0 {
		fmt.Println("CONTEXTSAVER_HELPER_UID isn't set; reinstall with: contextsaver install-helper")
		return 1
	}
	ln, err := net.FileListener(os.NewFile(3, "contextsaver-helper.sock"))
	if err != nil {
		fmt.Println("no listening socket from systemd:", err)
		return 1
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go func() { // idle: exit; systemd starts the helper again on the next connection
		for {
			time.Sleep(15 * time.Second)
			if helperActive.Load() == 0 && time.Since(time.Unix(0, last.Load())) > 60*time.Second {
				os.Exit(0)
			}
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			audit("ERROR accept: %v", err)
			return 1
		}
		last.Store(time.Now().UnixNano())
		go func() {
			handleElevated(c.(*net.UnixConn), allowed)
			last.Store(time.Now().UnixNano())
		}()
	}
}

// peerCred: the pid and uid on the other end of a Unix socket.
func peerCred(c *net.UnixConn) (pid, uid int, err error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	rc.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cerr != nil {
		return 0, 0, cerr
	}
	return int(cred.Pid), int(cred.Uid), nil
}

func sendMsg(c net.Conn, mu *sync.Mutex, m elevMsg) error {
	b, _ := json.Marshal(m)
	mu.Lock()
	defer mu.Unlock()
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.Write(append(b, '\n'))
	return err
}

func handleElevated(c *net.UnixConn, allowedUID int) {
	helperActive.Add(1)
	defer helperActive.Add(-1)
	defer c.Close()
	defer func() {
		if r := recover(); r != nil {
			audit("ERROR internal: %v", r)
		}
	}()
	var mu sync.Mutex
	send := func(m elevMsg) error { return sendMsg(c, &mu, m) }
	cpid, cuid, err := peerCred(c)
	client, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", cpid))
	if err != nil || cuid != allowedUID || !helperClientOK(client) {
		audit("REJECTED connection from pid %d uid %d (%s): only %s may use the helper", cpid, cuid, client, installPath())
		send(elevMsg{Error: "refused: only the installed ContextSaver of the user who installed the helper may use it"})
		return
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	var req elevRequest
	if err := json.Unmarshal(line, &req); err != nil || strings.TrimSpace(req.Command) == "" {
		send(elevMsg{Error: "bad request"})
		return
	}
	shell := req.Shell
	if shell == "" {
		shell = "bash"
	}
	if m := destructiveMatch(req.Command); m != "" && !req.Confirmed {
		audit("REFUSED (not confirmed, matched %q)  shell=%s  cwd=%s  cmd=%s", m, shell, req.Cwd, oneLine(req.Command))
		send(elevMsg{Error: fmt.Sprintf("refused: %q can delete or change system settings. Ask the user in chat, and only after they say yes run it again with confirmed: true.", m)})
		return
	}
	if req.Cwd != "" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			send(elevMsg{Error: "folder not found: " + req.Cwd})
			return
		}
	}
	cmd, err := buildCommand(shell, req.Command)
	if err != nil {
		send(elevMsg{Error: err.Error()})
		return
	}
	cmd.Dir = req.Cwd
	cmd.Env = append(os.Environ(), req.Env...)
	pr, pw, err := os.Pipe()
	if err != nil {
		send(elevMsg{Error: err.Error()})
		return
	}
	defer pr.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		audit("FAILED to start  shell=%s  cwd=%s  cmd=%s  — %v", shell, req.Cwd, oneLine(req.Command), err)
		send(elevMsg{Error: "couldn't start: " + err.Error()})
		return
	}
	pw.Close()
	pid := cmd.Process.Pid
	started := time.Now()
	note := ""
	if req.Confirmed {
		note = "  (confirmed by user)"
	}
	audit("START pid=%d  shell=%s  cwd=%s  cmd=%s%s", pid, shell, req.Cwd, oneLine(req.Command), note)
	send(elevMsg{PID: pid})

	var canceled atomic.Bool
	go func() { // the client never writes again: EOF means ContextSaver canceled, timed out or quit
		io.Copy(io.Discard, c)
		if canceled.CompareAndSwap(false, true) {
			killTree(pid)
		}
	}()
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 && send(elevMsg{Out: append([]byte(nil), buf[:n]...)}) != nil {
				io.Copy(io.Discard, pr)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	cmd.Wait()
	select {
	case <-outDone:
	case <-time.After(3 * time.Second): // something it started still holds the output open
	}
	code := cmd.ProcessState.ExitCode()
	end := ""
	if !canceled.CompareAndSwap(false, true) {
		end = "  (stopped by ContextSaver)"
	}
	audit("END   pid=%d  exit=%d  after %s%s", pid, code, hd(time.Since(started)), end)
	send(elevMsg{Exit: &code})
}

// helperClientOK: the installed program, or an older one still running after an update
// (install keeps it as contextsaver.old, later moved into old/).
func helperClientOK(client string) bool {
	client = strings.TrimSuffix(client, " (deleted)")
	if client == "" {
		return false
	}
	if samePath(client, installPath()) {
		return true
	}
	return strings.HasPrefix(filepath.Base(client), "contextsaver.old") && hasPathPrefix(client, appRoot())
}

// ---------- client ----------

func startElevated(req elevRequest, out io.Writer) (*elevProc, error) {
	conn, err := net.DialTimeout("unix", helperSock, 10*time.Second)
	if err != nil {
		if !helperInstalled() {
			return nil, errors.New("Tell the user: " + helperHowTo())
		}
		return nil, fmt.Errorf("couldn't reach the admin helper: %v (sudo systemctl restart %s.socket)", err, helperUnit)
	}
	c := conn.(*net.UnixConn)
	if _, uid, err := peerCred(c); err != nil || uid != 0 {
		c.Close()
		return nil, errors.New("refused: the admin helper socket isn't served by root")
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, fmt.Errorf("couldn't talk to the admin helper: %v", err)
	}
	r := bufio.NewReaderSize(c, 64<<10)
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("no answer from the admin helper: %v", err)
	}
	c.SetReadDeadline(time.Time{})
	var m elevMsg
	json.Unmarshal(line, &m)
	if m.Error != "" || m.PID == 0 {
		c.Close()
		if m.Error == "" {
			m.Error = "bad answer from the admin helper"
		}
		return nil, errors.New(m.Error)
	}
	var once sync.Once
	var stopped atomic.Bool
	ep := &elevProc{pid: m.PID, done: make(chan struct{})}
	ep.stop = func() { once.Do(func() { stopped.Store(true); c.Close() }) }
	go func() {
		defer close(ep.done)
		defer c.Close()
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				if stopped.Load() {
					ep.err = errCanceled
				} else {
					ep.err = fmt.Errorf("lost the connection to the admin helper: %v", err)
				}
				return
			}
			var m elevMsg
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			if len(m.Out) > 0 {
				out.Write(m.Out)
			}
			if m.Error != "" {
				ep.err = errors.New(m.Error)
				return
			}
			if m.Exit != nil {
				ep.code = m.Exit
				return
			}
		}
	}()
	return ep, nil
}

// ---------- install / uninstall ----------

// elevateSelf runs this program's install-helper/uninstall-helper as root: sudo in a
// terminal, else the desktop's password dialog (pkexec).
func elevateSelf(arg string) error {
	exe := installPath()
	args := []string{exe, arg, "--root", appRoot()}
	var c *exec.Cmd
	if isTerminal(os.Stdin) {
		c = exec.Command("sudo", args...)
	} else {
		c = exec.Command("pkexec", args...)
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// rootArg: the --root the unprivileged call passed (root's own home is not the user's).
func rootArg() {
	for i := 2; i+1 < len(os.Args); i++ {
		if os.Args[i] == "--root" {
			os.Setenv("CONTEXTSAVER_ROOT", os.Args[i+1])
		}
	}
}

func invokingUser() (*user.User, error) {
	for _, v := range []string{"SUDO_UID", "PKEXEC_UID"} {
		if id := os.Getenv(v); id != "" && id != "0" {
			return user.LookupId(id)
		}
	}
	return nil, errors.New("run it as your own user (not as root): contextsaver install-helper")
}

func consoleInstallHelper() {
	if os.Geteuid() != 0 {
		if !exists(installPath()) {
			fmt.Println("Install ContextSaver first (run it without arguments), then install the helper.")
			return
		}
		fmt.Println("Ubuntu will ask once for your password to set up the admin helper...")
		if err := elevateSelf("install-helper"); err != nil {
			fmt.Println("Not installed — the password prompt was declined or failed:", err)
			return
		}
		if helperInstalled() {
			fmt.Println("Admin helper installed. It starts by itself when ContextSaver needs admin and stops when it's no longer in use.")
			fmt.Println("Every admin command is logged in", auditPath())
		}
		return
	}
	rootArg()
	u, err := invokingUser()
	if err != nil {
		fmt.Println(err)
		return
	}
	if os.Getenv("CONTEXTSAVER_ROOT") == "" {
		os.Setenv("CONTEXTSAVER_ROOT", filepath.Join(u.HomeDir, "Claude Apps", "ContextSaver"))
	}
	src := installPath()
	if !exists(src) {
		fmt.Println("ContextSaver isn't installed at", src)
		return
	}
	if err := os.MkdirAll(helperLibDir, 0o755); err != nil {
		fmt.Println("Couldn't create", helperLibDir+":", err)
		return
	}
	tmp := helperBin() + ".new"
	if err := copyPlain(src, tmp); err != nil {
		fmt.Println("Couldn't copy the program:", err)
		return
	}
	os.Chown(tmp, 0, 0)
	os.Chmod(tmp, 0o755)
	if err := os.Rename(tmp, helperBin()); err != nil {
		fmt.Println("Couldn't place the program:", err)
		return
	}
	// admin log: root writes, the user reads
	logDir := filepath.Dir(auditPath())
	os.MkdirAll(logDir, 0o755)
	os.Chown(logDir, 0, 0)
	os.Chmod(logDir, 0o755)
	gid := u.Gid
	socket := "[Unit]\nDescription=ContextSaver admin helper socket\n\n[Socket]\nListenStream=" + helperSock +
		"\nSocketUser=" + u.Uid + "\nSocketGroup=" + gid + "\nSocketMode=0600\nAccept=no\n\n[Install]\nWantedBy=sockets.target\n"
	service := "[Unit]\nDescription=ContextSaver admin helper (runs ContextSaver's admin commands; log: " + auditPath() + ")\nRequires=" + helperUnit + ".socket\n\n[Service]\nType=simple\nExecStart=" +
		helperBin() + " helper\nEnvironment=CONTEXTSAVER_HELPER_UID=" + u.Uid + "\nEnvironment=\"CONTEXTSAVER_ROOT=" + os.Getenv("CONTEXTSAVER_ROOT") + "\"\n"
	if err := os.WriteFile(socketUnitPath(), []byte(socket), 0o644); err != nil {
		fmt.Println("Couldn't write", socketUnitPath()+":", err)
		return
	}
	if err := os.WriteFile(serviceUnitPath(), []byte(service), 0o644); err != nil {
		fmt.Println("Couldn't write", serviceUnitPath()+":", err)
		return
	}
	quietCmd("systemctl", "stop", helperUnit+".service").Run()
	quietCmd("systemctl", "daemon-reload").Run()
	if out, err := quietCmd("systemctl", "enable", helperUnit+".socket").CombinedOutput(); err != nil {
		fmt.Println("Couldn't enable the helper:", strings.TrimSpace(string(out)))
		return
	}
	if out, err := quietCmd("systemctl", "restart", helperUnit+".socket").CombinedOutput(); err != nil {
		fmt.Println("Couldn't start the helper socket:", strings.TrimSpace(string(out)))
		return
	}
	audit("INSTALLED helper for %s (uid %s); starts only when ContextSaver needs it", u.Username, u.Uid)
}

func consoleUninstallHelper() {
	if os.Geteuid() != 0 {
		if !helperInstalled() {
			fmt.Println("The admin helper isn't installed.")
			return
		}
		fmt.Println("Ubuntu will ask once for your password to remove the admin helper...")
		if err := elevateSelf("uninstall-helper"); err != nil {
			fmt.Println("Not removed — the password prompt was declined or failed:", err)
			return
		}
		if helperInstalled() {
			fmt.Println("The helper is still installed; removing it failed.")
		} else {
			fmt.Println("Admin helper removed. The log is kept at", auditPath())
		}
		return
	}
	rootArg()
	quietCmd("systemctl", "disable", "--now", helperUnit+".socket").Run()
	quietCmd("systemctl", "stop", helperUnit+".service").Run()
	os.Remove(socketUnitPath())
	os.Remove(serviceUnitPath())
	os.RemoveAll(helperLibDir)
	quietCmd("systemctl", "daemon-reload").Run()
	audit("UNINSTALLED helper")
}

func fileHash(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return string(h.Sum(nil))
}

// refreshHelper brings an installed helper up to the newly installed program (the
// helper runs its own root-owned copy, so updating it needs the password once).
func refreshHelper() {
	if !helperInstalled() || os.Geteuid() == 0 {
		return
	}
	if a, b := fileHash(installPath()), fileHash(helperBin()); a != "" && a == b {
		return
	}
	if !isTerminal(os.Stdin) {
		fmt.Println("The admin helper runs an older version; update it with: contextsaver install-helper")
		return
	}
	fmt.Println("Updating the admin helper to this version (asks your password once)...")
	if err := elevateSelf("install-helper"); err != nil {
		fmt.Println("Admin helper not updated:", err)
	}
}

func helperRunning() bool {
	return quietCmd("systemctl", "is-active", "--quiet", helperUnit+".service").Run() == nil
}

func helperStatus() string {
	switch {
	case !helperInstalled():
		return "not installed"
	case helperRunning():
		return "running"
	}
	return "installed, starts on demand"
}

// startHelper is the first step of the "Local AI" icon: on Linux systemd starts the
// helper on demand, so this only reports whether it is set up.
func startHelper() string {
	if !helperInstalled() {
		return "⚠ " + helperHowTo()
	}
	if _, err := os.Stat(helperSock); err != nil {
		return "Admin helper socket is missing (sudo systemctl restart " + helperUnit + ".socket)"
	}
	return "Admin helper ready"
}

// stopHelper: the helper exits by itself a minute after its last command.
func stopHelper() string {
	if helperInstalled() && helperRunning() {
		return "Admin helper will stop by itself within a minute"
	}
	return ""
}

// moveTree merges src into dst (also across disks) and removes src once it's moved.
func moveTree(src, dst string) error {
	if !exists(dst) {
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
	}
	var dirs []string
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if fi.IsDir() {
			dirs = append(dirs, p)
			return os.MkdirAll(to, 0o755)
		}
		if exists(to) {
			return nil // kept where it was: dst already has one
		}
		if err := os.Rename(p, to); err == nil {
			return nil
		}
		if err := copyPlain(p, to); err != nil {
			return err
		}
		os.Chmod(to, fi.Mode().Perm())
		os.Chtimes(to, fi.ModTime(), fi.ModTime())
		return os.Remove(p)
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
	if err != nil {
		return err
	}
	if exists(src) {
		return fmt.Errorf("some files stayed in %s (already in %s)", src, dst)
	}
	return nil
}

var _ = bytes.Equal
