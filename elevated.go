package main

import (
	"regexp"
	"strings"
)

// The elevated helper runs as the user's own account with the admin token
// (a logon scheduled task with "highest privileges"). ContextSaver hands it
// commands marked elevated over a local named pipe that only this user's
// ContextSaver can open. Every command is written to an admin-only audit log.

const (
	elevPipeName = `\\.\pipe\contextsaver-elevated`
	elevTaskName = `ContextSaver\Elevated Helper`
)

// elevRequest is the one message the client sends.
type elevRequest struct {
	Command   string   `json:"command"`
	Shell     string   `json:"shell,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Env       []string `json:"env,omitempty"`
	Confirmed bool     `json:"confirmed,omitempty"`
}

// elevMsg is what the helper sends back, one JSON object per line:
// first {pid} (or {error}), then {out} chunks, then {exit}.
type elevMsg struct {
	PID   int    `json:"pid,omitempty"`
	Out   []byte `json:"out,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
	Error string `json:"error,omitempty"`
}

// elevProc is a command running in the helper, seen from the client side.
type elevProc struct {
	pid  int
	done chan struct{}
	code *int
	err  error
	stop func()
}

// Commands that delete, wipe or change system state. The helper refuses these
// unless the request says the user confirmed them in chat.
var destructiveRe = func() *regexp.Regexp {
	b := `(?:^|[\s;&|(){}"'@])`
	e := `(?:\.exe)?(?:\s|$|[;&|)"'])`
	words := []string{
		`del`, `erase`, `rd`, `rmdir`, `rm`, `ri`, `format`, `diskpart`, `bcdedit`, `takeown`, `icacls`, `cacls`,
		`shutdown`, `bootrec`, `bcdboot`, `fsutil`, `mountvol`, `manage-bde`, `reagentc`,
		`Remove-Item`, `Clear-Content`, `Clear-Disk`, `Format-Volume`, `Remove-Partition`, `Initialize-Disk`,
		`Clear-RecycleBin`, `Remove-ItemProperty`, `Set-ItemProperty`, `New-ItemProperty`, `Clear-ItemProperty`,
		`Stop-Service`, `Remove-Service`, `Set-Service`, `New-Service`, `Set-Acl`,
		`Restart-Computer`, `Stop-Computer`, `Unregister-ScheduledTask`, `Disable-ScheduledTask`,
		`Uninstall-Package`, `Remove-AppxPackage`, `Set-MpPreference`, `Add-MpPreference`, `Remove-MpPreference`,
		`Disable-WindowsOptionalFeature`, `Set-ExecutionPolicy`, `Remove-LocalUser`, `Set-LocalUser`,
		`Remove-LocalGroupMember`, `Add-LocalGroupMember`, `Set-NetFirewallProfile`, `Remove-NetFirewallRule`,
		// Linux
		`shred`, `wipefs`, `dd`, `mkfs`, `mkswap`, `sgdisk`, `blkdiscard`, `partprobe`, `resize2fs`, `ntfsresize`,
		`e2fsck`, `fsck`, `cryptsetup`, `lvremove`, `vgremove`, `pvremove`, `lvreduce`, `swapoff`, `reboot`,
		`poweroff`, `halt`, `kexec`, `userdel`, `deluser`, `groupdel`, `delgroup`, `passwd`, `chpasswd`, `usermod`,
		`visudo`, `grub-install`, `update-grub`, `grub-mkconfig`, `efibootmgr`, `chattr`, `truncate`, `unlink`,
	}
	phrases := []string{
		`reg(?:\.exe)?\s+(?:delete|add|import|restore|load|unload|copy)`,
		`sc(?:\.exe)?\s+(?:\\\\\S+\s+)?(?:delete|stop|config|create|failure|sdset)`,
		`net(?:\.exe)?\s+(?:stop|user|localgroup|share\s+\S+\s+/delete)`,
		`schtasks(?:\.exe)?\s+(?:/delete|/change)`,
		`vssadmin(?:\.exe)?\s+(?:delete|resize)`,
		`wbadmin(?:\.exe)?\s+delete`,
		`wmic(?:\.exe)?\s+.*\b(?:delete|call\s+terminate|call\s+uninstall)`,
		`cipher(?:\.exe)?\s+/w`,
		`winget(?:\.exe)?\s+(?:uninstall|remove)`,
		`msiexec(?:\.exe)?\s+.*/x`,
		`netsh(?:\.exe)?\s+(?:advfirewall|firewall)\s+.*\b(?:set|delete|reset)`,
		`dism(?:\.exe)?\s+.*/(?:remove|disable)`,
		`move(?:\.exe)?\s+.*(?:\\windows\\|\\program files)`,
		// Linux
		`mkfs\.\w+`, `fsck\.\w+`,
		`systemctl\s+(?:--\S+\s+)*(?:stop|disable|mask|kill|reboot|poweroff|halt|isolate|rescue|emergency|set-default)`,
		`service\s+\S+\s+stop`,
		`(?:apt|apt-get|aptitude)\s+(?:-\S+\s+)*(?:remove|purge|autoremove|full-upgrade|dist-upgrade)`,
		`dpkg\s+.*(?:\s-r|\s-P|--remove|--purge)`,
		`(?:snap|flatpak)\s+(?:remove|uninstall)`,
		`ufw\s+(?:disable|delete|reset|deny|reject)`,
		`(?:iptables|ip6tables)\s+.*(?:\s-F|\s-X|\s-D|--flush|--delete)`,
		`nft\s+(?:flush|delete)`,
		`(?:chmod|chown|chgrp)\s+(?:\S+\s+)*(?:-R|-\w*R\w*|--recursive)`,
		`(?:fdisk|gdisk|cfdisk|sfdisk)\s+(?:-[^l\s]\S*\s+)*/dev/`,
		`parted\s+.*\b(?:mklabel|mktable|mkpart|rm|resizepart|resize)\b`,
		`find\s+.*(?:-delete|-exec\s+rm)`,
		`(?:>|tee\s+(?:-a\s+)?)\s*/dev/(?:sd|nvme|mmcblk|vd|hd)\w*`,
		`(?:>|tee\s+(?:-a\s+)?)\s*/etc/(?:fstab|passwd|shadow|sudoers|group|crypttab)`,
		`crontab\s+-r`,
		`mv\s+.*\s/(?:etc|usr|boot|bin|sbin|lib|lib64|var)(?:/\S*)?(?:\s|$)`,
	}
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`(?i)` + b + `(?:` + strings.Join(words, "|") + `|` + strings.Join(phrases, "|") + `)` + e)
}()

// destructiveMatch returns the part of command that looks destructive, or "".
func destructiveMatch(command string) string {
	m := destructiveRe.FindString(command)
	return strings.Trim(m, " \t\r\n;&|(){}\"'@")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ⏎ ")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	return strings.ReplaceAll(s, "\r", " ")
}
