package main

import (
	"os"
	"path/filepath"
	"testing"
)

func dangerMode(t *testing.T, mode string) {
	old := cfg.Approval
	t.Cleanup(func() { cfg.Approval = old })
	cfg.Approval = mode
}

func TestDangerousCommands(t *testing.T) {
	safe := []string{
		`"D:\Claude Apps\AdbSaver\gradlew.bat" -p "D:\Local Projects\DumbApp" assembleDebug`,
		`npm install`, `npm run dev`, `npm uninstall left-pad`, `go test ./...`, `go build -o app.exe .`,
		`python -m pytest -q`, `pip install requests`, `git add -A`, `git commit -m "fix: remove old button"`,
		`git push`, `git push -u origin main`, `git push origin v1.2.0`, `git pull --ff-only`, `git status`,
		`git log --format=%h`, `git restore --staged x`, `adb install app.apk`, `adb devices`,
		`mkdir build`, `copy a.txt b.txt`, `Get-ChildItem -Recurse`,
	}
	for _, c := range safe {
		if m := dangerousCommand(c); m != "" {
			t.Errorf("safe command flagged %q: matched %q", c, m)
		}
	}
	bad := []string{
		`del /q build\x.txt`, `rd /s /q D:\tmp`, `rmdir old`, `rm -rf node_modules`, `Remove-Item -Recurse x`,
		`powershell Remove-AppxPackage foo`, `Get-AppxPackage *x* | Remove-AppxPackage`, `winget uninstall Foo`,
		`msiexec /x {GUID}`, `reg delete HKCU\Software\X /f`, `sc stop spooler`, `vssadmin delete shadows /all`,
		`dism /online /remove-package /packagename:x`, `format D:`, `netsh advfirewall set allprofiles state off`,
		`git push --force`, `git push -f origin main`, `git push --force-with-lease`, `git push origin +main`,
		`git push origin --delete feature`, `git push origin :feature`, `git reset --hard HEAD~1`,
		`git branch -D old`, `git clean -fdx`, `git filter-repo --path x`, `gh repo delete me/x --yes`,
		`gh release delete v1.0`, `adb uninstall com.x.y`, `adb shell pm uninstall com.x.y`, `type nul > notes.txt`,
		`Clear-ItemProperty -Path HKCU:\X -Name y`, `Stop-Service wuauserv`,
	}
	for _, c := range bad {
		if dangerousCommand(c) == "" {
			t.Errorf("dangerous command not flagged: %q", c)
		}
	}
}

func TestApprovalActionDangerousMode(t *testing.T) {
	dangerMode(t, "dangerous")
	dir := t.TempDir()
	existing := filepath.Join(dir, "main.go")
	os.WriteFile(existing, []byte("package main\n"), 0o644)

	noAsk := []struct {
		c string
		a map[string]any
	}{
		{"run", map[string]any{"command": `"D:\Claude Apps\AdbSaver\gradlew.bat" -p "D:\Local Projects\DumbApp" assembleDebug`}},
		{"run", map[string]any{"command": "npm install", "elevated": true}},
		{"run", map[string]any{"command": `git commit -am "x" && git push`}},
		{"git", map[string]any{"action": "push", "path": dir}},
		{"write", map[string]any{"path": existing, "content": "package main\nfunc main(){}\n"}},
		{"edit", map[string]any{"path": existing}},
		{"check", map[string]any{"action": "build", "path": dir}},
		{"browser", map[string]any{"action": "click", "text": "Save"}},
		{"run_job", map[string]any{"steps": []any{map[string]any{"op": "copy", "from": []any{existing}, "to": dir + "\\b"}}}},
	}
	for _, x := range noAsk {
		if w := approvalAction(x.c, x.a); w != "" {
			t.Errorf("%s %v asked for approval: %q", x.c, x.a, w)
		}
	}
	ask := []struct {
		c string
		a map[string]any
	}{
		{"run", map[string]any{"command": `del "D:\Local Projects\DumbApp\x.txt"`}},
		{"run", map[string]any{"command": "git push --force"}},
		{"run", map[string]any{"command": "Remove-AppxPackage Foo", "elevated": true}},
		{"write", map[string]any{"path": existing, "content": ""}},
		{"run_job", map[string]any{"steps": []any{map[string]any{"op": "delete", "from": []any{existing}}}}},
		{"phone", map[string]any{"action": "uninstall", "package": "com.example.app"}},
		{"saved", map[string]any{"action": "remove", "name": "x"}},
	}
	for _, x := range ask {
		if w := approvalAction(x.c, x.a); w == "" {
			t.Errorf("%s %v ran without approval", x.c, x.a)
		}
	}
}

func TestApprovalActionWaitModeUnchanged(t *testing.T) {
	dangerMode(t, "wait")
	a := map[string]any{"command": "npm install"}
	if approvalAction("run", a) != riskyAction("run", a) || approvalAction("run", a) == "" {
		t.Fatal(`"wait" must still ask for every risky call`)
	}
	if approvalKind() != "wait" && approvalMode() {
		t.Fatal("wait kind changed")
	}
}

func TestDangerousModeWaitsInRemoteAI(t *testing.T) {
	dangerMode(t, "dangerous")
	old := clientName()
	t.Cleanup(func() { mcpClient.Store(old) })
	mcpClient.Store("lmstudio")
	if !approvalMode() || approvalKind() != "wait" {
		t.Fatalf("dangerous mode must use the RemoteAI wait flow, got mode=%v kind=%q", approvalMode(), approvalKind())
	}
}
