package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// "contextsaver setup": what a fresh Ubuntu needs for every ContextSaver tool, one
// question per system change.

type pkgGroup struct {
	ask  string
	pkgs []string
}

var setupGroups = []pkgGroup{
	{"Tools ContextSaver uses (GitHub CLI, git, ctags, 7-Zip, PDF and OCR tools, ffmpeg, clipboard and window tools)",
		[]string{"git", "gh", "universal-ctags", "curl", "unzip", "p7zip-full", "poppler-utils", "tesseract-ocr", "ffmpeg",
			"wl-clipboard", "xclip", "xdotool", "python3-gi", "gir1.2-atspi-2.0", "gnome-screenshot", "libvulkan1", "mesa-vulkan-drivers"}},
	{"Compilers for building apps (C/C++, CMake, Go, Windows .exe cross-compiler, Java for Android, Python, Node.js, Rust)",
		[]string{"build-essential", "cmake", "ninja-build", "golang-go", "mingw-w64", "openjdk-17-jdk-headless", "python3-venv",
			"python3-pip", "nodejs", "npm", "cargo"}},
	{"LibreOffice, to convert Word/Excel/PowerPoint files (large)",
		[]string{"libreoffice-writer", "libreoffice-calc", "libreoffice-impress"}},
}

func pkgInstalled(p string) bool {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", p).Output()
	return err == nil && strings.Contains(string(out), "install ok installed")
}

func missingPkgs(g pkgGroup) []string {
	var m []string
	for _, p := range g.pkgs {
		if !pkgInstalled(p) {
			m = append(m, p)
		}
	}
	return m
}

func setupNeeded() bool {
	return len(missingPkgs(setupGroups[0])) > 0 || !exists(linuxLlamaServer("")) || !ghSignedIn()
}

func sudoRun(args ...string) error {
	c := exec.Command("sudo", args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func consoleSetup() {
	fmt.Printf("ContextSaver %s setup for %s\n\n", appVersion, osName())
	updated := false
	for _, g := range setupGroups {
		m := missingPkgs(g)
		if len(m) == 0 {
			continue
		}
		if !askYes(fmt.Sprintf("%s:\n  sudo apt install %s\nInstall? [Y/n] ", g.ask, strings.Join(m, " ")), true) {
			continue
		}
		if !updated {
			sudoRun("apt-get", "update", "-q")
			updated = true
		}
		if err := sudoRun(append([]string{"apt-get", "install", "-y", "-q"}, m...)...); err != nil {
			fmt.Println("apt failed:", err)
		}
	}
	if srv := linuxLlamaServer(""); !exists(srv) {
		if askYes("\nDownload llama.cpp's Linux build (Vulkan, runs on the AMD GPU) into ~/llama.cpp? [Y/n] ", true) {
			if out, err := installLlamaCpp(); err != nil {
				fmt.Println("llama.cpp not installed:", err)
			} else {
				fmt.Println(out)
			}
		}
	}
	if !exists(filepath.Join(whisperDir(), "whisper-cli")) && exists("/usr/bin/cmake") {
		if askYes("\nBuild whisper.cpp for audio/video transcripts (a few minutes)? [Y/n] ", true) {
			if err := buildWhisper(); err != nil {
				fmt.Println("whisper.cpp not built:", err)
			}
		}
	}
	if androidSDK() == "" && pkgInstalled("openjdk-17-jdk-headless") {
		if askYes("\nInstall the Android SDK to ~/Android/Sdk, for building APKs (about 1.5 GB)? [y/N] ", false) {
			if err := installAndroidSDK(askYes("Add the Android NDK too (C/C++ and Rust on Android, about 1 GB)? [y/N] ", false)); err != nil {
				fmt.Println("Android SDK not installed:", err)
			}
		}
	}
	setupWindowsMounts()
	if !helperInstalled() && exists(installPath()) {
		if askYes("\nInstall the admin helper, so your local AI can run admin commands without your password each time (destructive ones still ask you)? [Y/n] ", true) {
			consoleInstallHelper()
		}
	}
	if _, err := exec.LookPath("gh"); err == nil && !ghSignedIn() {
		if askYes("\nSign in to GitHub now (opens the browser), so ContextSaver can push, release and check Actions? [Y/n] ", true) {
			c := exec.Command("gh", ghLoginArgs()...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if c.Run() == nil {
				exec.Command("gh", "auth", "setup-git").Run()
			}
		}
	}
	fmt.Println("\nSetup done.")
}

// installLlamaCpp downloads the newest llama.cpp Ubuntu Vulkan build into ~/llama.cpp.
func installLlamaCpp() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	url, tag, err := githubAsset(ctx, "ggml-org/llama.cpp", func(n string) bool {
		return strings.Contains(n, "bin-ubuntu-vulkan-x64") && strings.HasSuffix(n, ".zip")
	})
	if err != nil {
		return "", err
	}
	h, _ := os.UserHomeDir()
	dir := filepath.Join(h, "llama.cpp")
	zp := filepath.Join(os.TempDir(), "llama.cpp-"+tag+".zip")
	fmt.Println("Downloading llama.cpp", tag, "...")
	if err := download(ctx, url, zp); err != nil {
		return "", err
	}
	defer os.Remove(zp)
	n, err := unzipFlat(zp, dir)
	if err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte(tag+"\n"), 0o644)
	return fmt.Sprintf("llama.cpp %s: %d files in %s", tag, n, dir), nil
}

// githubAsset: the download URL of the newest release asset matching pick.
func githubAsset(ctx context.Context, repo string, pick func(string) bool) (string, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", err
	}
	for _, a := range r.Assets {
		if pick(a.Name) {
			return a.URL, r.Tag, nil
		}
	}
	return "", "", fmt.Errorf("no matching download in %s %s", repo, r.Tag)
}

// unzipFlat extracts every file of a zip into dir without its folders (llama.cpp's
// zip keeps the programs and their libraries under build/bin).
func unzipFlat(zp, dir string) (int, error) {
	zr, err := zip.OpenReader(zp)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		dst := filepath.Join(dir, filepath.Base(f.Name))
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			rc, _ := f.Open()
			target, _ := io.ReadAll(rc)
			rc.Close()
			os.Remove(dst)
			os.Symlink(filepath.Base(string(target)), dst)
			n++
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return n, err
		}
		mode := os.FileMode(0o644)
		if f.FileInfo().Mode()&0o111 != 0 || !strings.Contains(filepath.Base(f.Name), ".") {
			mode = 0o755
		}
		out, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return n, err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return n, err
		}
		if err := os.Rename(dst+".part", dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// buildWhisper builds whisper-cli from source (whisper.cpp ships no Linux programs),
// with Vulkan when the shader compiler is there, as one static program.
func buildWhisper() error {
	dir := whisperDir()
	src := filepath.Join(dir, "src")
	if !exists(src) {
		if err := runShown(dir, "git", "clone", "--depth", "1", "https://github.com/ggml-org/whisper.cpp", src); err != nil {
			return err
		}
	}
	args := []string{"-S", src, "-B", filepath.Join(src, "build"), "-DBUILD_SHARED_LIBS=OFF", "-DCMAKE_BUILD_TYPE=Release"}
	if _, err := exec.LookPath("glslc"); err == nil {
		args = append(args, "-DGGML_VULKAN=ON")
	}
	if err := runShown(src, "cmake", args...); err != nil {
		return err
	}
	if err := runShown(src, "cmake", "--build", filepath.Join(src, "build"), "-j", "--config", "Release", "--target", "whisper-cli"); err != nil {
		return err
	}
	return copyPlain(filepath.Join(src, "build", "bin", "whisper-cli"), filepath.Join(dir, "whisper-cli"))
}

func runShown(dir, name string, args ...string) error {
	os.MkdirAll(dir, 0o755)
	c := exec.Command(name, args...)
	c.Dir, c.Stdout, c.Stderr = dir, os.Stdout, os.Stderr
	return c.Run()
}

// androidSDK: ANDROID_HOME, else ~/Android/Sdk when it has the build tools ("" = none).
func androidSDK() string {
	h, _ := os.UserHomeDir()
	for _, d := range []string{os.Getenv("ANDROID_HOME"), os.Getenv("ANDROID_SDK_ROOT"), filepath.Join(h, "Android", "Sdk")} {
		if d != "" && exists(filepath.Join(d, "platform-tools")) {
			return d
		}
	}
	return ""
}

// installAndroidSDK installs Google's command-line tools, then platform-tools, a platform
// and build-tools (and the NDK) with sdkmanager, accepting its licenses.
func installAndroidSDK(ndk bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	h, _ := os.UserHomeDir()
	sdk := filepath.Join(h, "Android", "Sdk")
	tools := filepath.Join(sdk, "cmdline-tools", "latest")
	if !exists(filepath.Join(tools, "bin", "sdkmanager")) {
		const url = "https://dl.google.com/android/repository/commandlinetools-linux-13114758_latest.zip"
		zp := filepath.Join(os.TempDir(), "android-cmdline-tools.zip")
		fmt.Println("Downloading Android command-line tools...")
		if err := download(ctx, url, zp); err != nil {
			return err
		}
		defer os.Remove(zp)
		tmp := filepath.Join(sdk, "cmdline-tools", "tmp")
		os.RemoveAll(tmp)
		if err := runShown(sdk, "unzip", "-q", zp, "-d", tmp); err != nil {
			return err
		}
		os.RemoveAll(tools)
		if err := os.Rename(filepath.Join(tmp, "cmdline-tools"), tools); err != nil {
			return err
		}
		os.RemoveAll(tmp)
	}
	pkgs := []string{"platform-tools", "platforms;android-35", "build-tools;35.0.0"}
	if ndk {
		pkgs = append(pkgs, "ndk;27.2.12479018")
	}
	sm := filepath.Join(tools, "bin", "sdkmanager")
	yes := exec.Command(sm, "--sdk_root="+sdk, "--licenses")
	yes.Stdin = strings.NewReader(strings.Repeat("y\n", 20))
	yes.Run()
	if err := runShown(sdk, sm, append([]string{"--sdk_root=" + sdk}, pkgs...)...); err != nil {
		return err
	}
	fmt.Println("Android SDK in", sdk)
	return nil
}

// setupWindowsMounts offers to mount the Windows partitions at every start, so models
// and projects on D: are there without opening them in Files first.
func setupWindowsMounts() {
	out, err := runQuiet(10*time.Second, "lsblk", "-Pbno", "PATH,FSTYPE,SIZE,UUID,LABEL,MOUNTPOINT")
	if err != nil {
		return
	}
	fstab, _ := os.ReadFile("/etc/fstab")
	var add []string
	var names []string
	for _, l := range strings.Split(out, "\n") {
		f := lsblkPairs(l)
		var size int64
		fmt.Sscan(f["SIZE"], &size)
		if f["FSTYPE"] != "ntfs" || f["UUID"] == "" || size < 4<<30 || strings.Contains(string(fstab), f["UUID"]) {
			continue
		}
		name := strings.Map(func(r rune) rune {
			if r == ' ' || r == '/' {
				return '_'
			}
			return r
		}, f["LABEL"])
		if name == "" {
			name = "windows-" + strings.ToLower(f["UUID"][:4])
		}
		dir := "/mnt/" + name
		add = append(add, fmt.Sprintf("UUID=%s %s ntfs3 defaults,uid=%d,gid=%d,windows_names,nofail,x-gvfs-show 0 0", f["UUID"], dir, os.Getuid(), os.Getgid()))
		names = append(names, f["PATH"]+" → "+dir)
	}
	if len(add) == 0 {
		return
	}
	if !askYes("\nMount the Windows partitions at every start (adds them to /etc/fstab)?\n  "+strings.Join(names, "\n  ")+"\n[y/N] ", false) {
		return
	}
	for _, l := range add {
		dir := strings.Fields(l)[1]
		sudoRun("mkdir", "-p", dir)
	}
	c := exec.Command("sudo", "tee", "-a", "/etc/fstab")
	c.Stdin = strings.NewReader("# Windows partitions, added by ContextSaver setup\n" + strings.Join(add, "\n") + "\n")
	c.Stdout = io.Discard
	if err := c.Run(); err != nil {
		fmt.Println("Couldn't update /etc/fstab:", err)
		return
	}
	sudoRun("systemctl", "daemon-reload")
	if err := sudoRun("mount", "-a"); err != nil {
		fmt.Println("Mounting failed (with Windows' Fast Startup on, Windows leaves its disks locked; turn it off in Windows' power settings):", err)
	}
}

var errNotLinux = errors.New("Linux only")
