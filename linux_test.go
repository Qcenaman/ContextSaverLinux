package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPidOnPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if got := pidOnPort(port); got != os.Getpid() {
		t.Fatalf("pidOnPort(%d) = %d, want %d", port, got, os.Getpid())
	}
	ln.Close()
	time.Sleep(100 * time.Millisecond)
	if got := pidOnPort(port); got != 0 {
		t.Fatalf("closed port still owned by %d", got)
	}
}

func TestTrashAndRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))
	p := filepath.Join(home, "a file.txt")
	os.WriteFile(p, []byte("x"), 0o644)
	at := time.Now()
	if !useRecycleBin(p) {
		t.Skip("no Trash on this filesystem")
	}
	if err := recycleBatch([]string{p}); err != nil || exists(p) {
		t.Fatalf("trash: %v, still there: %v", err, exists(p))
	}
	info, _ := filepath.Glob(filepath.Join(home, "share", "Trash", "info", "*.trashinfo"))
	if len(info) != 1 {
		t.Fatalf("trashinfo files: %v", info)
	}
	b, _ := os.ReadFile(info[0])
	if !strings.Contains(string(b), "Path="+filepath.Join(home, "a%20file.txt")) {
		t.Fatalf("trashinfo: %s", b)
	}
	if err := restoreRecycled(p, at); err != nil || !exists(p) {
		t.Fatalf("restore: %v", err)
	}
}

func TestLinuxDestructive(t *testing.T) {
	for _, c := range []string{"sudo dd if=/dev/zero of=/dev/sda", "mkfs.ext4 /dev/sdb1", "apt purge firefox", "systemctl stop ssh",
		"chmod -R 777 /srv", "find . -name x -delete", "echo x > /etc/fstab", "rm -rf build", "snap remove code"} {
		if destructiveMatch(c) == "" {
			t.Errorf("not caught: %s", c)
		}
	}
	for _, c := range []string{"apt install git", "systemctl status ssh", "ls -la /dev", "go build ./...", "chmod +x gradlew", "cat /etc/fstab"} {
		if m := destructiveMatch(c); m != "" {
			t.Errorf("%q flagged as %q", c, m)
		}
	}
}

func TestDesktopLauncher(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.desktop")
	if err := writeLauncher(p, "Use model - a", "c", "", []string{"/opt/my app/contextsaver", "model", `a"b$`}, true, 4); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `Exec=env CONTEXTSAVER_PAUSE=4 "/opt/my app/contextsaver" model "a\\"b\\$"`) {
		t.Fatalf("launcher:\n%s", b)
	}
	if fi, _ := os.Stat(p); fi.Mode()&0o111 == 0 {
		t.Fatal("launcher isn't executable")
	}
}

func TestHexAddr(t *testing.T) {
	if a, p := hexAddr("0100007F:1F90"); a != "127.0.0.1" || p != 8080 {
		t.Fatalf("%s:%d", a, p)
	}
	if a, p := hexAddr("00000000000000000000000000000000:0050"); a != "::" || p != 80 {
		t.Fatalf("%s:%d", a, p)
	}
}

func TestSetupStepsRouting(t *testing.T) {
	codeHome(t)
	if _, _, err := compactDispatch("sys", map[string]any{"action": "setup", "what": "bogus"}); err == nil || !strings.Contains(err.Error(), "android") {
		t.Fatalf("unknown step: %v", err)
	}
	names := map[string]bool{}
	for _, st := range setupSteps() {
		names[st.name] = true
	}
	for _, n := range []string{"tools", "compilers", "office", "chromium", "llama", "whisper", "android", "ndk", "github"} {
		if !names[n] {
			t.Errorf("no setup step %s", n)
		}
	}
}

func TestHelperMissingSaysHow(t *testing.T) {
	if helperInstalled() {
		t.Skip("helper installed here")
	}
	if !strings.Contains(helperHowTo(), "contextsaver install-helper") || !strings.Contains(startHelper(), "contextsaver install-helper") {
		t.Fatal("no how-to")
	}
	if _, err := startElevated(elevRequest{Command: "true"}, nil); err == nil || !strings.Contains(err.Error(), "Tell the user") || !strings.Contains(err.Error(), "install-helper") {
		t.Fatalf("elevated run without helper: %v", err)
	}
}
