package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const isWin = runtime.GOOS == "windows"

const defaultAIScript = `D:\llama.cpp\start-qwen3.6-27b-nothink.bat`

type Config struct {
	Workers             int               `json:"workers,omitempty"`
	ExtraBlocked        []string          `json:"extra_blocked,omitempty"`
	AIScript            string            `json:"ai_script,omitempty"`
	AIEndpoint          string            `json:"ai_endpoint,omitempty"`
	AIModel             string            `json:"ai_model,omitempty"`
	NotifyRepo          string            `json:"notify_repo,omitempty"`
	Models              map[string]string `json:"models,omitempty"`
	MaxOut              int               `json:"max_out,omitempty"`
	Frontend            string            `json:"frontend,omitempty"`       // anythingllm | lmstudio; empty = AnythingLLM if installed
	Approval            string            `json:"approval,omitempty"`       // "dangerous" (default): only dangerous calls wait in RemoteAI; "wait"/"client": every risky call; "confirm": in-chat token
	ApprovalToken       string            `json:"approval_token,omitempty"` // the approval server's bearer token
	ApprovalPort        int               `json:"approval_port,omitempty"`  // default 1235
	LMSToken            string            `json:"lms_token,omitempty"`
	ToolCallLimit       int               `json:"tool_call_limit_seconds,omitempty"`   // longer pc calls continue in the background (default 50 for LM Studio)
	ApprovalWaitSeconds int               `json:"approval_wait_seconds,omitempty"`     // approval "wait": how long a call blocks (default 600)
	ApprovalWaitCap     int               `json:"approval_wait_cap_seconds,omitempty"` // below the client's tool-call timeout: then fall back to "call again"
	ApprovalExempt      []string          `json:"approval_exempt_clients,omitempty"`   // MCP clients that keep the in-chat confirm (default cline, anythingllm)
	ClineDir            string            `json:"cline_dir,omitempty"`                 // Cline's CLINE_DIR (default: env CLINE_DIR)
	LMSBind             string            `json:"lms_bind,omitempty"`                  // lms server start --bind (0.0.0.0 for RemoteAI over Tailscale)
	ApprovalBind        string            `json:"approval_bind,omitempty"`             // default 127.0.0.1; 0.0.0.0 for RemoteAI over Tailscale      // LM Studio API token for ContextSaver's own calls
	RagDir              string            `json:"rag_dir,omitempty"`
	KeepModelRunning    bool              `json:"keep_model_running,omitempty"` // pin: nothing auto-stops the model server
	ModelIdleMinutes    int               `json:"model_idle_minutes,omitempty"` // idle stop of servers ContextSaver started (default 15)
	Drives              map[string]string `json:"drives,omitempty"`             // Windows drive letter -> mount folder, e.g. {"D": "/mnt/d"}
	LlamaServer         string            `json:"llama_server,omitempty"`       // Linux llama-server (default: ~/llama.cpp, then PATH)
}

var (
	appHome string
	cfg     Config
)

// appRoot is the one folder ContextSaver keeps everything in: the program, its data,
// the saved-job shortcuts and the admin log. Nothing goes on the Desktop or elsewhere.
func appRoot() string {
	if v := os.Getenv("CONTEXTSAVER_ROOT"); v != "" {
		return v
	}
	if isWin {
		return `D:\Claude Apps\ContextSaver`
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Claude Apps", "ContextSaver") // next to TokenSaver, like D:\Claude Apps on Windows
}

func initApp() {
	appHome = os.Getenv("CONTEXTSAVER_HOME")
	if appHome == "" {
		appHome = filepath.Join(appRoot(), "data")
	}
	for _, d := range []string{"jobs", "saved"} {
		os.MkdirAll(filepath.Join(appHome, d), 0o755)
	}
	if b, err := os.ReadFile(filepath.Join(appHome, "config.json")); err == nil {
		json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &cfg)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if strings.TrimSpace(cfg.Approval) == "" {
		cfg.Approval = "dangerous" // only deleting/uninstalling/system changes ask
	}
	if cfg.AIScript == "" && isWin {
		cfg.AIScript = defaultAIScript
	}
	if !isWin && isBatScript(cfg.AIScript) {
		cfg.AIScript = "" // a Windows script: the converted one comes from models.json
	}
}

func jobsDir() string  { return filepath.Join(appHome, "jobs") }
func savedDir() string { return filepath.Join(appHome, "saved") }

var envRe = regexp.MustCompile(`%([A-Za-z0-9_()]+)%`)

func expandPath(p string) (string, error) {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
	if p == "" {
		return "", errors.New("empty path")
	}
	p = envRe.ReplaceAllStringFunc(p, func(m string) string {
		if v := os.Getenv(m[1 : len(m)-1]); v != "" {
			return v
		}
		return m
	})
	if strings.HasPrefix(p, "~") {
		h, _ := os.UserHomeDir()
		p = h + p[1:]
	}
	if isWin {
		p = strings.ReplaceAll(p, "/", `\`)
	} else if isWinPath(p) {
		q, err := winToLinux(p)
		if err != nil {
			return "", err
		}
		p = q
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute (like /home/you/Photos or D:\\Photos): %s", p)
	}
	return filepath.Clean(p), nil
}

func normCase(p string) string {
	if isWin {
		return strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool {
	return normCase(filepath.Clean(a)) == normCase(filepath.Clean(b))
}

// hasPathPrefix reports whether p is prefix or inside prefix.
func hasPathPrefix(p, prefix string) bool {
	p, prefix = normCase(filepath.Clean(p)), normCase(filepath.Clean(prefix))
	if p == prefix {
		return true
	}
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(p, prefix)
}

func blockedRoots() []string {
	var r []string
	if isWin {
		for _, v := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData"} {
			if s := os.Getenv(v); s != "" {
				r = append(r, s)
			}
		}
	} else {
		r = append(r, "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/opt", "/proc", "/sbin", "/snap", "/sys", "/usr", "/var/lib")
	}
	if h, err := os.UserHomeDir(); err == nil {
		r = append(r, filepath.Join(h, ".ssh"), filepath.Join(h, ".gnupg"))
	}
	for _, e := range cfg.ExtraBlocked {
		if p, err := expandPath(e); err == nil {
			r = append(r, p)
		}
	}
	return r
}

var blockedTopNames = map[string]bool{
	"$recycle.bin": true, "system volume information": true, "recovery": true, "config.msi": true,
	"boot": true, "pagefile.sys": true, "hiberfil.sys": true, "swapfile.sys": true,
}

// checkAllowed refuses paths inside system folders.
func checkAllowed(p string) error {
	for _, b := range blockedRoots() {
		if hasPathPrefix(p, b) {
			return fmt.Errorf("%s is inside a protected system folder", p)
		}
	}
	if isWin {
		vol := filepath.VolumeName(p)
		rest := strings.TrimPrefix(p[len(vol):], `\`)
		first := strings.ToLower(strings.SplitN(rest, `\`, 2)[0])
		if blockedTopNames[first] {
			return fmt.Errorf("%s is inside a protected system folder", p)
		}
	} else if windowsProtected(p) {
		return fmt.Errorf("%s is inside Windows' own folders", p)
	}
	return nil
}

// readOnlyRoots: system folders that may be read (headers, configs, installed programs),
// never written; the Linux counterpart of Program Files.
var readOnlyRoots = []string{"/bin", "/boot", "/etc", "/lib", "/lib64", "/opt", "/sbin", "/snap", "/usr", "/var/lib"}

// checkReadable is checkAllowed, except Program Files (Linux: /usr, /etc, /opt...) may be
// read (never written).
func checkReadable(p string) error {
	if !isWin {
		for _, r := range readOnlyRoots {
			if hasPathPrefix(p, r) {
				return nil
			}
		}
	}
	if isWin {
		for _, v := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
			if s := os.Getenv(v); s != "" && hasPathPrefix(p, s) {
				for _, b := range []string{"SystemRoot", "ProgramData"} {
					if t := os.Getenv(b); t != "" && hasPathPrefix(p, t) {
						return checkAllowed(p)
					}
				}
				return nil
			}
		}
	}
	return checkAllowed(p)
}

// checkDeletable refuses whole drives, the user folder and the standard folders themselves.
func checkDeletable(p string) error {
	if err := checkAllowed(p); err != nil {
		return err
	}
	clean := filepath.Clean(p)
	vol := filepath.VolumeName(clean)
	if clean == vol+string(filepath.Separator) || clean == vol || clean == "/" {
		return fmt.Errorf("refusing to delete a whole drive: %s", p)
	}
	h, _ := os.UserHomeDir()
	prot := []string{h, filepath.Dir(h), desktopDir()}
	for _, n := range []string{"Desktop", "Documents", "Downloads", "Pictures", "Music", "Videos", "AppData", "OneDrive", "Favorites", "Contacts", "Links", "Saved Games", "Searches", "Public", "Templates", "snap", ".config", ".local", "Claude Apps"} {
		prot = append(prot, filepath.Join(h, n))
	}
	for _, x := range prot {
		if x != "" && samePath(x, clean) {
			return fmt.Errorf("refusing to delete %s itself (what's inside it can be deleted)", p)
		}
	}
	if hasPathPrefix(appHome, clean) {
		return errors.New("refusing to delete the app's own data folder")
	}
	return nil
}

var sizeUnits = []struct {
	suf string
	m   float64
}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}}

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	mult := 1.0
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suf))
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q (use e.g. 500KB, 10MB, 2GB)", s)
	}
	return int64(f * mult), nil
}

var relRe = regexp.MustCompile(`^(\d+)\s*(h|d|w|mo|m|y)$`)

func parseWhen(s string) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if m := relRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		now := time.Now()
		switch m[2] {
		case "h":
			return now.Add(-time.Duration(n) * time.Hour), nil
		case "d":
			return now.AddDate(0, 0, -n), nil
		case "w":
			return now.AddDate(0, 0, -7*n), nil
		case "m", "mo":
			return now.AddDate(0, -n, 0), nil
		case "y":
			return now.AddDate(-n, 0, 0), nil
		}
	}
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "2006-01", "2006"} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad date/age %q (use e.g. 30d, 6m, 1y or 2024-01-31)", s)
}

func hb(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func hc(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func hd(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// StrList accepts either a single string or a list of strings.
type StrList []string

func (s *StrList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one != "" {
			*s = StrList{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("expected a path or a list of paths")
	}
	*s = many
	return nil
}

func writeJSONAtomic(p string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	var rerr error
	for i := 0; i < 10; i++ {
		if rerr = os.Rename(tmp, p); rerr == nil {
			return nil
		}
		time.Sleep(30 * time.Millisecond)
	}
	return rerr
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func cleanErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}
