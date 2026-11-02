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

// Setup: what a fresh Ubuntu needs for every ContextSaver tool, as steps. "contextsaver
// setup" asks before each one in a terminal; sys{action:setup,what} lets the local model
// run them itself (root steps through the admin helper, no password). Output goes to a
// writer, never stdout while serving the tool.

type pkgGroup struct {
	ask  string
	pkgs []string
}

var setupGroups = map[string]pkgGroup{
	"tools": {"Tools ContextSaver uses (GitHub CLI, git, ctags, 7-Zip, PDF, OCR and picture tools, ffmpeg, clipboard and window tools)",
		[]string{"git", "gh", "universal-ctags", "curl", "unzip", "p7zip-full", "poppler-utils", "tesseract-ocr", "imagemagick", "ffmpeg",
			"wl-clipboard", "xclip", "xdotool", "wmctrl", "x11-utils", "python3-gi", "gir1.2-atspi-2.0", "gnome-screenshot", "libvulkan1", "mesa-vulkan-drivers"}},
	"compilers": {"Compilers for building apps (C/C++, CMake, Go, Windows .exe cross-compiler, Java for Android, Python, Node.js, Rust)",
		[]string{"build-essential", "cmake", "ninja-build", "golang-go", "mingw-w64", "openjdk-17-jdk-headless", "python3-venv",
			"python3-pip", "nodejs", "npm", "cargo"}},
	"office": {"LibreOffice, to convert Word/Excel/PowerPoint files (large)",
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
	return len(missingPkgs(setupGroups["tools"])) > 0 || !exists(linuxLlamaServer("")) || !ghSignedIn()
}

// asRoot runs a bash command as root: directly when already root, else through the admin
// helper, else sudo in a terminal.
func asRoot(out io.Writer, command string) error {
	switch {
	case os.Geteuid() == 0:
		c := exec.Command(bashBin(), "-c", command)
		c.Stdout, c.Stderr = out, out
		return c.Run()
	case helperInstalled():
		ep, err := startElevated(elevRequest{Command: command, Shell: "bash"}, out)
		if err != nil {
			return err
		}
		<-ep.done
		if ep.err != nil {
			return ep.err
		}
		if ep.code != nil && *ep.code != 0 {
			return fmt.Errorf("exit %d", *ep.code)
		}
		return nil
	case isTerminal(os.Stdin):
		c := exec.Command("sudo", bashBin(), "-c", command)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, out
		return c.Run()
	}
	return errors.New("Tell the user: " + helperHowTo())
}

type setupStep struct {
	name, ask string
	defYes    bool
	needed    func() bool
	run       func(out io.Writer) (string, error)
}

func aptStep(name string) setupStep {
	g := setupGroups[name]
	return setupStep{name, g.ask, true, func() bool { return len(missingPkgs(g)) > 0 }, func(out io.Writer) (string, error) {
		m := missingPkgs(g)
		if err := asRoot(out, "export DEBIAN_FRONTEND=noninteractive; apt-get update -q && apt-get install -y -q --no-install-recommends "+strings.Join(m, " ")); err != nil {
			return "", fmt.Errorf("apt: %v", err)
		}
		if left := missingPkgs(g); len(left) > 0 {
			return "", fmt.Errorf("still missing: %s", strings.Join(left, " "))
		}
		return "installed " + strings.Join(m, " "), nil
	}}
}

func setupSteps() []setupStep {
	return []setupStep{
		aptStep("tools"), aptStep("compilers"), aptStep("office"),
		{"chromium", "Chromium for the browser tool (pages that need JavaScript or your sign-in)", true,
			func() bool { return browserExe() == "" }, func(out io.Writer) (string, error) {
				if err := asRoot(out, "snap install chromium"); err != nil {
					return "", err
				}
				return "Chromium installed", nil
			}},
		{"llama", "llama.cpp's Linux build (Vulkan, runs on the AMD GPU) in ~/llama.cpp", true,
			func() bool { return !exists(linuxLlamaServer("")) }, installLlamaCpp},
		{"whisper", "whisper.cpp for audio/video transcripts (builds a few minutes)", true,
			func() bool { return !exists(filepath.Join(whisperDir(), "whisper-cli")) && lookOK("cmake") && lookOK("g++") }, buildWhisper},
		{"android", "the Android SDK in ~/Android/Sdk, for building APKs (about 500 MB)", false,
			func() bool { return androidSDK() == "" && lookOK("java") }, func(out io.Writer) (string, error) {
				return installAndroidSDK(out, "platform-tools", "platforms;android-35", "build-tools;35.0.0")
			}},
		{"ndk", "the Android NDK, for C/C++ and Rust on Android (about 1 GB)", false,
			func() bool { return androidSDK() != "" && androidNDK() == "" }, func(out io.Writer) (string, error) {
				return installAndroidSDK(out, "ndk;27.2.12479018")
			}},
		{"github", "a GitHub sign-in, so ContextSaver can push, release and check Actions", true,
			func() bool { return lookOK("gh") && !ghSignedIn() }, func(out io.Writer) (string, error) {
				if !isTerminal(os.Stdin) {
					return gitLogin() // the local model shows the user the code
				}
				c := exec.Command("gh", ghLoginArgs()...)
				c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, out
				if err := c.Run(); err != nil {
					return "", err
				}
				exec.Command("gh", "auth", "setup-git").Run()
				return "signed in to GitHub" + ensureGitIdentity(), nil
			}},
	}
}

func consoleSetup() {
	fmt.Printf("ContextSaver %s setup for %s\n\n", appVersion, osName())
	for _, st := range setupSteps() {
		if !st.needed() {
			continue
		}
		q := "[Y/n]"
		if !st.defYes {
			q = "[y/N]"
		}
		if !askYes("\nInstall "+st.ask+"? "+q+" ", st.defYes) {
			continue
		}
		if msg, err := st.run(os.Stdout); err != nil {
			fmt.Println(st.name, "not done:", err)
		} else {
			fmt.Println(msg)
		}
	}
	setupWindowsMounts()
	if !helperInstalled() && exists(installPath()) {
		if askYes("\nInstall the admin helper, so your local AI can run admin commands without your password each time (destructive ones still ask you)? [Y/n] ", true) {
			consoleInstallHelper()
		}
	}
	fmt.Println("\nSetup done.")
}

// toolSetup: sys{action:setup,what}: the steps named in what (all = every missing one
// but the NDK), done without questions; the full output goes to a log.
func toolSetup(a map[string]any) (string, error) {
	what := strings.ToLower(strings.Join(strings.FieldsFunc(str(a, "what", "name", "step"), func(r rune) bool { return r == ',' || r == ' ' }), " "))
	want := map[string]bool{}
	for _, w := range strings.Fields(what) {
		want[w] = true
	}
	all := len(want) == 0 || want["all"]
	logPath := filepath.Join(appHome, "cache", "setup.log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	var res []string
	known := map[string]bool{"all": true}
	for _, st := range setupSteps() {
		known[st.name] = true
		if !(want[st.name] || all && st.name != "ndk") {
			continue
		}
		if !st.needed() {
			if want[st.name] {
				res = append(res, st.name+": already there")
			}
			continue
		}
		fmt.Fprintf(lf, "==== %s %s\n", time.Now().Format("2006-01-02 15:04:05"), st.name)
		if msg, err := st.run(lf); err != nil {
			res = append(res, "✗ "+st.name+": "+err.Error())
		} else {
			res = append(res, "✓ "+st.name+": "+msg)
		}
	}
	for w := range want {
		if !known[w] {
			return "", fmt.Errorf("unknown setup step %q; steps: all tools compilers office chromium llama whisper android ndk github", w)
		}
	}
	if !helperInstalled() && os.Geteuid() != 0 {
		res = append(res, "⚠ "+helperHowTo())
	}
	if len(res) == 0 {
		return "everything is set up already", nil
	}
	return strings.Join(res, "\n") + "\nlog: " + logPath, nil
}

// installLlamaCpp downloads the newest llama.cpp Ubuntu Vulkan build into ~/llama.cpp.
func installLlamaCpp(out io.Writer) (string, error) {
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
	fmt.Fprintln(out, "Downloading llama.cpp", tag, "...")
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
func buildWhisper(out io.Writer) (string, error) {
	dir := whisperDir()
	src := filepath.Join(dir, "src")
	if !exists(src) {
		if err := runTo(out, dir, "git", "clone", "--depth", "1", "https://github.com/ggml-org/whisper.cpp", src); err != nil {
			return "", err
		}
	}
	args := []string{"-S", src, "-B", filepath.Join(src, "build"), "-DBUILD_SHARED_LIBS=OFF", "-DCMAKE_BUILD_TYPE=Release"}
	if _, err := exec.LookPath("glslc"); err == nil {
		args = append(args, "-DGGML_VULKAN=ON")
	}
	if err := runTo(out, src, "cmake", args...); err != nil {
		return "", err
	}
	if err := runTo(out, src, "cmake", "--build", filepath.Join(src, "build"), "-j", "--config", "Release", "--target", "whisper-cli"); err != nil {
		return "", err
	}
	cli := filepath.Join(dir, "whisper-cli")
	if err := copyPlain(filepath.Join(src, "build", "bin", "whisper-cli"), cli); err != nil {
		return "", err
	}
	os.Chmod(cli, 0o755)
	return "whisper-cli built", nil
}

func runTo(out io.Writer, dir, name string, args ...string) error {
	os.MkdirAll(dir, 0o755)
	c := exec.Command(name, args...)
	c.Dir, c.Stdout, c.Stderr = dir, out, out
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
func installAndroidSDK(out io.Writer, pkgs ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	h, _ := os.UserHomeDir()
	sdk := filepath.Join(h, "Android", "Sdk")
	tools := filepath.Join(sdk, "cmdline-tools", "latest")
	if !exists(filepath.Join(tools, "bin", "sdkmanager")) {
		const url = "https://dl.google.com/android/repository/commandlinetools-linux-13114758_latest.zip"
		zp := filepath.Join(os.TempDir(), "android-cmdline-tools.zip")
		fmt.Fprintln(out, "Downloading Android command-line tools...")
		if err := download(ctx, url, zp); err != nil {
			return "", err
		}
		defer os.Remove(zp)
		tmp := filepath.Join(sdk, "cmdline-tools", "tmp")
		os.RemoveAll(tmp)
		if err := runTo(out, sdk, "unzip", "-q", zp, "-d", tmp); err != nil {
			return "", err
		}
		os.RemoveAll(tools)
		if err := os.Rename(filepath.Join(tmp, "cmdline-tools"), tools); err != nil {
			return "", err
		}
		os.RemoveAll(tmp)
	}
	sm := filepath.Join(tools, "bin", "sdkmanager")
	yes := exec.Command(sm, "--sdk_root="+sdk, "--licenses")
	yes.Stdin = strings.NewReader(strings.Repeat("y\n", 20))
	yes.Run()
	if err := runTo(out, sdk, sm, append([]string{"--sdk_root=" + sdk}, pkgs...)...); err != nil {
		return "", err
	}
	return strings.Join(pkgs, ", ") + " in " + sdk, nil
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

// sudoRun: a root command in the terminal (setupWindowsMounts asks there first).
func sudoRun(args ...string) error {
	c := exec.Command("sudo", args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
