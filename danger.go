package main

import (
	"regexp"
	"strings"
)

// approval "dangerous" (the default): ordinary work (builds, tests, git commit/push,
// editing files, installs, dev servers, browser and phone actions that remove nothing)
// runs without asking; only calls that delete, uninstall, rewrite history or change the
// system wait for Approve/Deny in RemoteAI. destructiveRe (the admin helper's list) is
// the single source of truth for commands, extended by dangerExtraRe below.

// Commands beyond destructiveRe that need approval: git history rewrites and deletions,
// GitHub deletions, app uninstalls (PC and phone) and emptying files.
var dangerExtraRe = func() *regexp.Regexp {
	b := `(?:^|[\s;&|(){}"'@])`
	phrases := []string{
		`git(?:\.exe)?\s+push\b[^;&|]*\s(?:-f|--force|--force-with-lease|--force-if-includes|--delete|-d|--mirror|--prune)(?:[\s=]|$)`,
		`git(?:\.exe)?\s+push\b[^;&|]*\s\+\S`,
		`git(?:\.exe)?\s+push\b[^;&|]*\s:\S`,
		`git(?:\.exe)?\s+reset\b[^;&|]*--hard`,
		`git(?:\.exe)?\s+clean\b[^;&|]*\s-\w*[fx]`,
		`git(?:\.exe)?\s+branch\b[^;&|]*\s(?:-d|-D|--delete)(?:\s|$)`,
		`git(?:\.exe)?\s+tag\b[^;&|]*\s(?:-d|--delete)(?:\s|$)`,
		`git(?:\.exe)?\s+(?:filter-branch|filter-repo)\b`,
		`git(?:\.exe)?\s+checkout\s+(?:--\s+)?\.(?:\s|$)`,
		`git(?:\.exe)?\s+stash\s+(?:drop|clear)`,
		`git(?:\.exe)?\s+update-ref\s+-d`,
		`gh(?:\.exe)?\s+(?:repo|release|issue|secret|variable|gist|run|cache|label|ruleset)\s+delete\b`,
		`gh(?:\.exe)?\s+api\b[^;&|]*(?:-X|--method)\s*DELETE`,
		`adb(?:\.exe)?\b[^;&|]*\s(?:uninstall|pm\s+(?:uninstall|clear|disable)|rm)\b`,
		`(?:choco|scoop|pip3?|npm|pnpm|yarn)(?:\.exe|\.cmd)?\s+(?:uninstall|remove)\b[^;&|]*\s(?:-g|--global)\b`,
		`(?:choco|scoop)(?:\.exe|\.cmd)?\s+uninstall\b`,
		`(?:apt|apt-get|snap|flatpak|pipx)\s+(?:-\S+\s+)*(?:remove|purge|uninstall)\b`,
		`git\s+push\b[^;&|]*\s--force\b`,
		`:\s*>\s*\S`,
		`Uninstall-\w+`,
		`dism(?:\.exe)?\b[^;&|]*/(?:remove|disable)`,
		`Remove-\w+`,
		`(?:type|copy)\s+nul\b`,
		`Set-Content\b[^;&|]*-Value\s*(?:''|""|\$null)(?:\s|$)`,
		`Disable-\w+`,
		`Set-NetFirewall\w*|New-NetFirewallRule|Disable-NetAdapter`,
		`(?:Set|Remove|New)-Partition|Resize-Partition|Dismount-\w+`,
	}
	return regexp.MustCompile(`(?i)` + b + `(?:` + strings.Join(phrases, "|") + `)`)
}()

// dangerousCommand returns the part of a shell command that needs approval, or "".
func dangerousCommand(command string) string {
	if m := destructiveMatch(command); m != "" {
		return m
	}
	m := dangerExtraRe.FindString(command)
	return strings.Trim(m, " \t\r\n;&|(){}\"'@")
}

// dangerousOnly: approval "dangerous" (also the default when the setting is empty).
func dangerousOnly() bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Approval), "dangerous")
}

// approvalAction: what needs the user's approval for this call, or "".
func approvalAction(c string, a map[string]any) string {
	what := riskyAction(c, a)
	if what == "" || !dangerousOnly() || isDangerous(c, a) {
		return what
	}
	return ""
}

// isDangerous: the call deletes, uninstalls, rewrites history or changes the system.
func isDangerous(c string, a map[string]any) bool {
	act := strings.ToLower(str(a, "action"))
	tool := c
	if n, ok := compactAlias[c]; ok {
		tool = n
	}
	switch c {
	case "write", "save", "create":
		p := str(a, "path", "to", "file")
		return p != "" && fileExists(p) && strings.TrimSpace(str(a, "content", "text")) == ""
	case "edit", "replace", "open", "launch", "start", "app":
		return false
	case "phone", "android", "install_app", "apk", "mobile":
		return phoneAction(a) == "uninstall"
	}
	switch tool {
	case "run":
		return dangerousCommand(str(a, "command", "cmd", "c", "script")) != ""
	case "run_job":
		steps, _ := a["steps"].([]any)
		for _, s := range steps {
			st, _ := s.(map[string]any)
			op := strings.ToLower(str(st, "op"))
			switch {
			case op == "delete",
				op == "sync" && st["delete_extra"] == true,
				str(st, "conflict") == "overwrite",
				op == "write" && fileExists(str(st, "to")) && strings.TrimSpace(str(st, "content")) == "":
				return true
			}
		}
		return false
	case "git":
		f, _ := a["force"].(bool)
		return f
	case "saved":
		return act == "remove"
	case "job":
		return act == "undo"
	case "rag":
		return true // index guard overrides and forgetting notes
	}
	return false
}
