package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The laptop dual-boots: models, llama.cpp and projects stay on the Windows partitions.
// A Windows path (D:\llama.cpp\model.gguf) is found on the mounted Windows partition
// that has it. config.json "drives" ({"D": "/mnt/d"}) pins a letter to a folder.

var winPathRe = regexp.MustCompile(`^([A-Za-z]):(?:[\\/](.*))?$`)

func isWinPath(p string) bool { return winPathRe.MatchString(p) }

var winDrives = struct {
	sync.Mutex
	letter  map[string]string // "D" -> mount folder, once a path was found there
	mounted bool              // tried mounting the Windows partitions once
}{letter: map[string]string{}}

// windowsMounts: mounted partitions that can be Windows drives (NTFS, exFAT, FAT).
func windowsMounts() []string {
	var out []string
	for _, m := range diskMounts() {
		switch m.fs {
		case "ntfs", "ntfs3", "fuseblk", "exfat", "vfat":
			out = append(out, m.dir)
		}
	}
	return out
}

func isWindowsSystem(dir string) bool {
	_, err := os.Stat(ciJoin(dir, "Windows/System32"))
	return err == nil
}

// ciJoin joins rest to root matching each existing part case-insensitively (Windows
// names aren't case-sensitive, Linux mounts are); parts that don't exist yet are kept.
func ciJoin(root, rest string) string {
	cur := root
	parts := strings.Split(strings.ReplaceAll(rest, `\`, "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(cur, part)
		if _, err := os.Lstat(next); err != nil {
			found := false
			if ents, err := os.ReadDir(cur); err == nil {
				for _, e := range ents {
					if strings.EqualFold(e.Name(), part) {
						next, found = filepath.Join(cur, e.Name()), true
						break
					}
				}
			}
			if !found {
				return filepath.Join(append([]string{cur}, parts[i:]...)...)
			}
		}
		cur = next
	}
	return cur
}

// winToLinux maps a Windows path to where it is on this Linux system.
func winToLinux(p string) (string, error) {
	m := winPathRe.FindStringSubmatch(p)
	if m == nil {
		return p, nil
	}
	letter, rest := strings.ToUpper(m[1]), m[2]
	for k, v := range cfg.Drives {
		if strings.EqualFold(strings.TrimSuffix(k, ":"), letter) && v != "" {
			return ciJoin(v, rest), nil
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if dir := findDrive(letter, rest); dir != "" {
			return ciJoin(dir, rest), nil
		}
		if attempt == 0 && !mountWindowsPartitions() {
			break
		}
	}
	return "", fmt.Errorf(`%s is on the Windows %s: drive, which isn't mounted here. Open it once in Files (or set "drives" in %s, e.g. {"%s": "/mnt/%s"})`,
		p, letter, filepath.Join(appHome, "config.json"), letter, strings.ToLower(letter))
}

// findDrive picks the mount for letter: C: is the one with Windows on it; for the others
// the mount that has the path, else the one seen before, else the only candidate.
func findDrive(letter, rest string) string {
	var sys string
	var others []string
	for _, d := range windowsMounts() {
		if isWindowsSystem(d) {
			if sys == "" {
				sys = d
			}
			continue
		}
		others = append(others, d)
	}
	if letter == "C" {
		return sys
	}
	winDrives.Lock()
	defer winDrives.Unlock()
	if rest != "" {
		for _, d := range others {
			if _, err := os.Stat(ciJoin(d, rest)); err == nil {
				winDrives.letter[letter] = d
				return d
			}
		}
	}
	if d := winDrives.letter[letter]; d != "" && exists(d) {
		return d
	}
	if len(others) == 1 && letter == "D" {
		return others[0]
	}
	return ""
}

// winDriveOf names the Windows drive mounted at dir ("" when unknown).
func winDriveOf(dir string) string {
	for k, v := range cfg.Drives {
		if samePath(v, dir) {
			return strings.ToUpper(strings.TrimSuffix(k, ":")) + ":"
		}
	}
	winDrives.Lock()
	for k, v := range winDrives.letter {
		if samePath(v, dir) {
			winDrives.Unlock()
			return k + ":"
		}
	}
	winDrives.Unlock()
	switch m := diskMountFS(dir); m {
	case "ntfs", "ntfs3", "fuseblk":
		if isWindowsSystem(dir) {
			return "C:"
		}
	}
	return ""
}

func diskMountFS(dir string) string {
	for _, m := range diskMounts() {
		if m.dir == dir {
			return m.fs
		}
	}
	return ""
}

// mountWindowsPartitions mounts unmounted NTFS partitions the way the Files app does
// (udisks, under /media/<user>), once per run. True when one was mounted.
func mountWindowsPartitions() bool {
	winDrives.Lock()
	if winDrives.mounted {
		winDrives.Unlock()
		return false
	}
	winDrives.mounted = true
	winDrives.Unlock()
	out, err := runQuiet(10*time.Second, "lsblk", "-Pbno", "PATH,FSTYPE,SIZE,MOUNTPOINT")
	if err != nil {
		return false
	}
	did := false
	for _, l := range strings.Split(out, "\n") {
		f := lsblkPairs(l)
		size, _ := strconv.ParseInt(f["SIZE"], 10, 64)
		if f["FSTYPE"] != "ntfs" || f["MOUNTPOINT"] != "" || size < 4<<30 { // skip Windows' small recovery partitions
			continue
		}
		if _, err := runQuiet(30*time.Second, "udisksctl", "mount", "--no-user-interaction", "-b", f["PATH"]); err == nil {
			did = true
		}
	}
	return did
}

var lsblkRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

func lsblkPairs(line string) map[string]string {
	m := map[string]string{}
	for _, x := range lsblkRe.FindAllStringSubmatch(line, -1) {
		m[x[1]] = x[2]
	}
	return m
}

// windowsProtected: Windows' own folders on a mounted Windows partition (never written).
func windowsProtected(p string) bool {
	for _, d := range windowsMounts() {
		if !hasPathPrefix(p, d) || samePath(p, d) {
			continue
		}
		rel, _ := filepath.Rel(d, p)
		first := strings.ToLower(strings.SplitN(rel, "/", 2)[0])
		if blockedTopNames[first] {
			return true
		}
		if isWindowsSystem(d) {
			switch first {
			case "windows", "program files", "program files (x86)", "programdata":
				return true
			}
		}
	}
	return false
}
