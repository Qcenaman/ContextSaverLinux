package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func hideWindow(c *exec.Cmd) {}

// bashBin: the shell commands and start scripts run in (plain sh when bash is missing).
func bashBin() string {
	for _, p := range []string{"/bin/bash", "/usr/bin/bash"} {
		if exists(p) {
			return p
		}
	}
	return "/bin/sh"
}

// buildCommand runs command in bash. "cmd" (the Windows default) also means bash, so a
// model used to the Windows version keeps working; powershell/pwsh need PowerShell 7.
func buildCommand(shell, command string) (*exec.Cmd, error) {
	var c *exec.Cmd
	switch strings.ToLower(shell) {
	case "", "bash", "cmd":
		c = exec.Command(bashBin(), "-c", command)
	case "sh":
		c = exec.Command("/bin/sh", "-c", command)
	case "powershell", "pwsh":
		ps, err := exec.LookPath("pwsh")
		if err != nil {
			return nil, errors.New("PowerShell isn't installed; use bash (the default)")
		}
		c = exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", command)
	default:
		return nil, fmt.Errorf("unknown shell %q (bash, sh or pwsh)", shell)
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c, nil
}

// childrenOf reads /proc/<pid>/task/*/children.
func childrenOf(pid int) []int {
	var out []int
	ts, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	for _, t := range ts {
		b, _ := os.ReadFile(t)
		for _, f := range strings.Fields(string(b)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// killTree ends pid, everything it started (also children that left its group) and its group.
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	kids := childrenOf(pid)
	syscall.Kill(pid, syscall.SIGSTOP) // no new children while the tree is collected
	for _, k := range kids {
		killTree(k)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	syscall.Kill(pid, syscall.SIGKILL)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return !os.IsNotExist(err)
	}
	if i := strings.LastIndexByte(string(b), ')'); i > 0 {
		if f := strings.Fields(string(b)[i+1:]); len(f) > 0 && f[0] == "Z" {
			return false
		}
	}
	return true
}

// claudeRunning: the Claude desktop app is open (only the installer asks).
func claudeRunning() bool {
	ps, _ := procs()
	for _, p := range ps {
		if strings.EqualFold(p.name, "claude") || strings.EqualFold(p.name, "claude-desktop") {
			return true
		}
	}
	return false
}

func osName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				return strings.Trim(v, `"`)
			}
		}
	}
	return "Linux"
}

func platformInfo() []string {
	var u syscall.Utsname
	kernel := ""
	if syscall.Uname(&u) == nil {
		var b []byte
		for _, c := range u.Release {
			if c == 0 {
				break
			}
			b = append(b, byte(c))
		}
		kernel = ", kernel " + string(b)
	}
	session := os.Getenv("XDG_SESSION_TYPE")
	if session == "" {
		session = "no desktop"
	}
	lines := []string{fmt.Sprintf("OS: %s%s, %s session", osName(), kernel, session)}
	cpu := fmt.Sprintf("%d threads", runtime.NumCPU())
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				if _, v, ok := strings.Cut(l, ":"); ok {
					cpu = strings.TrimSpace(v) + ", " + cpu
				}
				break
			}
		}
	}
	lines = append(lines, "CPU: "+cpu)
	if total, avail := memInfo(); total > 0 {
		lines = append(lines, fmt.Sprintf("RAM: %s free of %s", hb(avail), hb(total)))
	}
	var disks []string
	for _, m := range diskMounts() {
		var st syscall.Statfs_t
		if syscall.Statfs(m.dir, &st) == nil && st.Blocks > 0 {
			label := m.fs
			if w := winDriveOf(m.dir); w != "" {
				label += ", Windows " + w
			}
			disks = append(disks, fmtDisk(m.dir, label, st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)))
		}
	}
	if len(disks) > 0 {
		lines = append(lines, "Disks: "+strings.Join(disks, "; "))
	}
	return lines
}

func memInfo() (total, avail int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return
}

type diskMount struct{ dev, dir, fs string }

// diskMounts: real disks (ext4, btrfs, ntfs, vfat...), not snaps, loops or pseudo filesystems.
func diskMounts() []diskMount {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return []diskMount{{"", "/", ""}}
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []diskMount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		x := strings.Fields(sc.Text())
		if len(x) < 3 || !strings.HasPrefix(x[0], "/dev/") || strings.HasPrefix(x[0], "/dev/loop") || x[2] == "squashfs" {
			continue
		}
		dir := unescapeMount(x[1])
		if seen[x[0]] || strings.HasPrefix(dir, "/snap/") || dir == "/boot/efi" {
			continue
		}
		seen[x[0]] = true
		out = append(out, diskMount{x[0], dir, x[2]})
	}
	if len(out) == 0 {
		out = []diskMount{{"", "/", ""}}
	}
	return out
}

// unescapeMount turns /proc/mounts' \040 (space) and friends back into characters.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// procs reads /proc: full program names (ps cuts them at 15 characters) and command lines.
func procs() ([]procInfo, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	page := int64(os.Getpagesize())
	var out []procInfo
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		name := ""
		if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
			name = filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
		} else if len(args) > 0 && args[0] != "" {
			name = filepath.Base(args[0])
		} else if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
			name = strings.TrimSpace(string(b))
		} else {
			continue
		}
		var rss int64
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid)); err == nil {
			if f := strings.Fields(string(b)); len(f) > 1 {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				rss = n * page
			}
		}
		out = append(out, procInfo{pid: pid, name: name, mem: rss, cmd: strings.Join(args, " ")})
	}
	return out, nil
}

func procNames() map[int]string {
	m := map[int]string{}
	ps, _ := procs()
	for _, p := range ps {
		m[p.pid] = p.name
	}
	return m
}

func listening() ([]portInfo, error) {
	ls := tcpListeners()
	if len(ls) == 0 {
		if _, err := os.Stat("/proc/net/tcp"); err != nil {
			return nil, errors.New("couldn't read /proc/net/tcp")
		}
		return nil, nil
	}
	want := map[uint64]bool{}
	for _, l := range ls {
		want[l.inode] = true
	}
	own := inodeOwners(want)
	out := make([]portInfo, 0, len(ls))
	for _, l := range ls {
		out = append(out, portInfo{port: l.port, addr: l.addr, pid: own[l.inode]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].port < out[j].port })
	return out, nil
}
