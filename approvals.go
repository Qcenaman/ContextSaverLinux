package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Client approvals (config "approval": "client"). A risky call is recorded as pending
// and the model only gets its public id. The user approves or denies it in the client
// app (RemoteAI), which talks to this approval server, never to the model. The model's
// retry with confirm:<id> runs only once the user approved it.

type approval struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"`
	What    string    `json:"what"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Status  string    `json:"status"`  // pending | approved | denied | expired
	Waiting bool      `json:"waiting"` // a pc call is blocked on it (approval "wait")
}

const approvalTTL = 10 * time.Minute

// spawnApprovals starts the approval server if it isn't running (a hook for tests).
var spawnApprovals = spawnApprovalServer

// approvalMode: client approvals apply to API callers (LM Studio's bridge, unknown
// clients). Cline and AnythingLLM show the chat to the user, so they keep the in-chat
// confirm. approval_exempt_clients overrides the exempt list.
func approvalMode() bool {
	if m := strings.ToLower(strings.TrimSpace(cfg.Approval)); m != "client" && m != "wait" && m != "dangerous" {
		return false
	}
	name := strings.ToLower(clientName())
	if name == "" {
		return true
	}
	exempt := cfg.ApprovalExempt
	if len(exempt) == 0 {
		exempt = []string{"cline", "anythingllm", "anything-llm"}
	}
	for _, e := range exempt {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" && strings.Contains(name, e) {
			return false
		}
	}
	return true
}

// mcpClient is the clientInfo.name the MCP client sent on initialize.
var mcpClient atomic.Value

func clientName() string {
	s, _ := mcpClient.Load().(string)
	return s
}

// noteClient remembers the client and logs it once (data\mcp-clients.log), so the exempt
// list can be checked against real names.
func noteClient(name, version string) {
	mcpClient.Store(name)
	if appHome == "" {
		return
	}
	p := filepath.Join(appHome, "mcp-clients.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 64<<10 {
		os.Rename(p, p+".old")
	}
	if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s\t%s\t%s\tapprovals=%v\n", time.Now().Format("2006-01-02 15:04:05"), name, version, approvalMode())
		f.Close()
	}
}

func approvalPort() int {
	if cfg.ApprovalPort > 0 {
		return cfg.ApprovalPort
	}
	return 1235
}

func approvalsDir() string { return filepath.Join(appHome, "approvals") }

func approvalPath(id string) (string, error) {
	if len(id) != 8 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", errors.New("bad approval id")
	}
	return filepath.Join(approvalsDir(), id+".json"), nil
}

func loadApproval(id string) (*approval, error) {
	p, err := approvalPath(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var a approval
	return &a, json.Unmarshal(b, &a)
}

func newApproval(c string, a map[string]any, what string) (string, error) {
	return newApprovalOpt(c, a, what, false)
}

func newApprovalOpt(c string, a map[string]any, what string, waiting bool) (string, error) {
	os.MkdirAll(approvalsDir(), 0o755)
	pruneApprovals()
	id := randHex(4)
	now := time.Now()
	ap := approval{ID: id, Hash: argsHash(c, a), What: clip1(what, 300), Created: now, Expires: now.Add(approvalTTL), Status: "pending", Waiting: waiting}
	p, _ := approvalPath(id)
	return id, writeJSONAtomic(p, ap)
}

// useApproval spends an approved id for these exact args. note explains a pending or
// denied one; it is empty when the id doesn't belong to these args.
func useApproval(id, c string, a map[string]any) (ok bool, note string) {
	ap, err := loadApproval(id)
	if err != nil {
		if p, perr := approvalPath(id); perr == nil && exists(p+".used") {
			return false, "Approval " + id + " was already used: it ran once. Don't retry it."
		}
		return false, ""
	}
	if ap.Hash != argsHash(c, a) {
		return false, ""
	}
	switch {
	case ap.Status == "denied":
		return false, "The user denied " + id + ". Don't retry it."
	case time.Now().After(ap.Expires):
		return false, ""
	case ap.Status == "pending":
		return false, "Still waiting for the user to approve " + id + " in RemoteAI. Don't retry until they say so."
	case ap.Status == "approved":
		p, _ := approvalPath(id)
		// renaming claims it: only one call can spend an approval
		return os.Rename(p, p+".used") == nil, ""
	}
	return false, ""
}

// decideApproval records the user's answer for a pending approval.
func decideApproval(id string, approve bool) error {
	ap, err := loadApproval(id)
	if err != nil {
		return errors.New("no such approval")
	}
	if ap.Status != "pending" || time.Now().After(ap.Expires) {
		return errors.New("approval " + id + " is no longer pending")
	}
	ap.Status = "denied"
	if approve {
		ap.Status = "approved"
		ap.Expires = time.Now().Add(approvalTTL)
	}
	p, _ := approvalPath(id)
	return writeJSONAtomic(p, ap)
}

func pendingApprovals() []approval {
	ents, _ := os.ReadDir(approvalsDir())
	var out []approval
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ap, err := loadApproval(strings.TrimSuffix(e.Name(), ".json"))
		if err == nil && ap.Status == "pending" && time.Now().Before(ap.Expires) {
			ap.Hash = ""
			out = append(out, *ap)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// pruneApprovals removes approval files older than a day.
func pruneApprovals() {
	ents, _ := os.ReadDir(approvalsDir())
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(approvalsDir(), e.Name()))
		}
	}
}

// spendConfirm checks a confirm value: the client approval id in approval mode, the
// one-time token otherwise.
func spendConfirm(tok, c string, a map[string]any) (bool, string) {
	if !approvalMode() {
		return useConfirm(tok, c, a), ""
	}
	if tok == "" {
		return false, ""
	}
	return useApproval(tok, c, a)
}

// askApproval is what a risky call returns instead of running.
func askApproval(c string, a map[string]any, what, note string) string {
	if note != "" {
		return note
	}
	if approvalMode() {
		id, err := newApproval(c, a, what)
		if err != nil {
			return "Couldn't record the approval request: " + err.Error()
		}
		spawnApprovals()
		return "APPROVAL " + id + ": " + what + ". Waiting for the user to approve it in RemoteAI; tell them. Once approved, call again with the same args plus confirm:\"" + id + "\"."
	}
	return "CONFIRM " + newConfirm(c, a) + ": " + what + "; ask the user, then call again with the same args plus confirm:<token>"
}

// ---------- approval server ----------

// approvalHandler: GET /approvals lists pending ones; POST /approvals/{id}/approve|deny.
// Every request needs Authorization: Bearer <approval_token>.
func approvalHandler(token string) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /approvals", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"pending": pendingApprovals()})
	}))
	mux.HandleFunc("POST /approvals/{id}/{decision}", auth(func(w http.ResponseWriter, r *http.Request) {
		d := r.PathValue("decision")
		if d != "approve" && d != "deny" {
			http.Error(w, "decision must be approve or deny", http.StatusBadRequest)
			return
		}
		if err := decideApproval(r.PathValue("id"), d == "approve"); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		status := "approved"
		if d == "deny" {
			status = "denied"
		}
		json.NewEncoder(w).Encode(map[string]string{"id": r.PathValue("id"), "status": status})
	}))
	return mux
}

// ensureApprovalToken creates the approval server's token on first use and saves it.
func ensureApprovalToken() string {
	if cfg.ApprovalToken == "" {
		cfg.ApprovalToken = randHex(24)
		setConfigKey("approval_token", cfg.ApprovalToken)
	}
	return cfg.ApprovalToken
}

func setConfigKey(k string, v any) {
	p := filepath.Join(appHome, "config.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	m[k] = v
	b, _ := json.MarshalIndent(m, "", "  ")
	os.MkdirAll(appHome, 0o755)
	writeFileAtomic(p, b)
}

func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func spawnApprovalServer() {
	if !approvalMode() || portOpen(approvalPort()) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(exe, "approvals")
	detach(c, true)
	if c.Start() != nil {
		c = exec.Command(exe, "approvals")
		detach(c, false)
		c.Start()
	}
	if c.Process != nil {
		c.Process.Release()
	}
}

// consoleApprovals: contextsaver approvals [token | list | approve <id> | deny <id>]; no
// argument runs the approval server.
func consoleApprovals(args []string) int {
	if !approvalMode() {
		fmt.Println(`Client approvals are off. Set "approval": "client" in config.json.`)
		return 1
	}
	if len(args) == 0 {
		bind := cfg.ApprovalBind
		if bind == "" {
			bind = "127.0.0.1"
		}
		addr := fmt.Sprintf("%s:%d", bind, approvalPort())
		fmt.Println("approval server on", addr)
		if err := http.ListenAndServe(addr, approvalHandler(ensureApprovalToken())); err != nil {
			fmt.Println(err)
			return 1
		}
		return 0
	}
	switch args[0] {
	case "token":
		fmt.Println(ensureApprovalToken())
	case "list":
		for _, a := range pendingApprovals() {
			fmt.Println(a.ID, a.What)
		}
	case "approve", "deny":
		if len(args) < 2 {
			fmt.Println("usage: contextsaver approvals", args[0], "<id>")
			return 2
		}
		if err := decideApproval(args[1], args[0] == "approve"); err != nil {
			fmt.Println(err)
			return 1
		}
		fmt.Println(args[1], map[string]string{"approve": "approved", "deny": "denied"}[args[0]])
	default:
		fmt.Println("usage: contextsaver approvals [token | list | approve <id> | deny <id>]")
		return 2
	}
	return 0
}

// ---------- LM Studio API token ----------

// LM Studio needs an API token once its server requires authentication (it must, for
// mcp.json plugins over the API). ContextSaver's own calls to it carry lms_token.
type lmsAuthRT struct{ next http.RoundTripper }

func (t lmsAuthRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if cfg.LMSToken != "" && r.Header.Get("Authorization") == "" {
		if u, err := url.Parse(lmsBase); err == nil && strings.EqualFold(r.URL.Host, u.Host) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+cfg.LMSToken)
		}
	}
	return t.next.RoundTrip(r)
}

func init() { http.DefaultTransport = lmsAuthRT{http.DefaultTransport} }
