package main

// contextsaver chat: the local model with ContextSaver's tools, in a terminal. It talks
// to llama-server's OpenAI API directly and runs the pc tool inside this program, so the
// model's context holds only the one small tool. Risky calls ask y/n here.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type chatMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatLog struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Model    string    `json:"model"`
	Updated  time.Time `json:"updated"`
	Messages []chatMsg `json:"messages"`
}

type chatUI struct {
	in    *bufio.Reader
	color bool
	log   *chatLog
	base  string // http://host:port
	ctx   int    // the server's context size in tokens
	model string
	asked map[string]bool // repos whose copy this chat already settled (asked, cloned or updated)
}

func (u *chatUI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func chatDir() string { return filepath.Join(appHome, "chats") }

const chatMaxSteps = 40

func chatSystem() string {
	return "You are the user's assistant on his Linux PC, with the pc tool to act on it: read and change files, run commands, code, build, git, web. " +
		"Use it instead of guessing; check results before you answer. Answer briefly. Today is " + time.Now().Format("Mon 2 Jan 2006") + ".\n" +
		localInstructions + "\n" + aboutCard()
}

func consoleChat(args []string) int {
	u := &chatUI{in: bufio.NewReaderSize(os.Stdin, 1<<20), color: isTerminal(os.Stdout)}
	u.newChat()
	if err := u.connect(""); err != nil {
		fmt.Println(u.paint("31", err.Error()))
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	endBar := u.startBar()
	defer endBar()
	fmt.Println(u.paint("1", "ContextSaver Chat") + u.paint("2", fmt.Sprintf("  model %s, %dK context", filepath.Base(u.model), u.ctx/1024)))
	if !helperInstalled() {
		fmt.Println(u.paint("33", "⚠ "+helperHowTo()))
	}
	// Ctrl+C stops the answer being written; at the prompt it closes the chat.
	var mu sync.Mutex
	var stop context.CancelFunc
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		for s := range sig {
			if s != os.Interrupt { // window closed, or another model's icon took over
				u.save()
				endBar()
				os.Exit(0)
			}
			mu.Lock()
			cancel := stop
			mu.Unlock()
			if cancel == nil {
				u.save()
				endBar()
				fmt.Println()
				os.Exit(0)
			}
			cancel()
		}
	}()
	for {
		fmt.Print("\n" + u.paint("1;36", "you ›") + " ")
		line, err := u.readInput()
		if err != nil {
			fmt.Println()
			return 0
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if u.command(line) {
				return 0
			}
			continue
		}
		u.log.Messages = append(u.log.Messages, chatMsg{Role: "user", Content: line})
		if u.log.Title == "" {
			u.log.Title = clip(strings.Join(strings.Fields(line), " "), 60)
		}
		ctx, cancel := context.WithCancel(context.Background())
		mu.Lock()
		stop = cancel
		mu.Unlock()
		err = u.turn(ctx)
		mu.Lock()
		stop = nil
		mu.Unlock()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Println(u.paint("33", "\n[stopped]"))
			} else {
				fmt.Println(u.paint("31", "\n"+err.Error()))
			}
		}
		cancel()
		u.save()
	}
}

// readInput reads one message; a line ending in \ continues on the next.
func (u *chatUI) readInput() (string, error) {
	var b strings.Builder
	for {
		l, err := u.in.ReadString('\n')
		if err != nil && l == "" {
			return "", err
		}
		l = strings.TrimRight(l, "\r\n")
		if strings.HasSuffix(l, `\`) {
			b.WriteString(strings.TrimSuffix(l, `\`) + "\n")
			continue
		}
		b.WriteString(l)
		return b.String(), nil
	}
}

func (u *chatUI) newChat() {
	u.log = &chatLog{ID: time.Now().Format("20060102-150405"), Messages: []chatMsg{{Role: "system", Content: chatSystem()}}}
	u.asked = map[string]bool{}
}

// connect makes sure a model answers. Without a name it uses whichever model is loaded,
// whoever started it, on any port (loadedAI); only when none is does it load the
// last-used one. With a name (/model x) it loads that model.
func (u *chatUI) connect(name string) error {
	host, port := scriptAddr(firstScript())
	var served []string
	if name == "" {
		host, port, served = loadedAI(host, port, func(s string) { fmt.Println(u.paint("2", s)) })
	}
	u.base = fmt.Sprintf("http://%s:%d", host, port)
	if len(served) > 0 {
		u.model = strings.Join(uniq(served), ",")
	} else {
		if name == "" {
			name = modelForScript(cfg.AIScript)
			if _, ok := models()[name]; !ok {
				if ns := modelNames(); len(ns) > 0 {
					name = ns[0]
				}
			}
		}
		fmt.Println(u.paint("2", "Loading "+name+"…"))
		out, err := useModel(name, 600, false)
		if err != nil {
			return err
		}
		fmt.Println(u.paint("2", out))
		host, port = scriptAddr(firstScript())
		u.base = fmt.Sprintf("http://%s:%d", host, port)
		if ms := servedModels(host, port); len(ms) > 0 {
			u.model = strings.Join(uniq(ms), ",")
		} else {
			u.model = name
		}
	}
	u.ctx = 32768
	if r, err := (&http.Client{Timeout: 3 * time.Second}).Get(u.base + "/props"); err == nil {
		var p struct {
			Def struct {
				NCtx int `json:"n_ctx"`
			} `json:"default_generation_settings"`
		}
		if json.NewDecoder(r.Body).Decode(&p) == nil && p.Def.NCtx > 0 {
			u.ctx = p.Def.NCtx
		}
		r.Body.Close()
	}
	u.log.Model = u.model
	u.drawBar()
	return nil
}

func (u *chatUI) command(line string) (quit bool) {
	f := strings.Fields(line)
	arg := strings.TrimSpace(strings.TrimPrefix(line, f[0]))
	switch strings.ToLower(f[0]) {
	case "/exit", "/quit", "/q":
		return true
	case "/new":
		u.save()
		u.newChat()
		fmt.Println(u.paint("2", "new chat"))
	case "/model", "/models":
		if arg == "" {
			fmt.Println(modelStatus())
			return false
		}
		if err := u.connect(strings.ToLower(arg)); err != nil {
			fmt.Println(u.paint("31", err.Error()))
		}
	case "/chats":
		for i, c := range listChats() {
			fmt.Printf("%2d  %s  %s\n", i+1, c.Updated.Format("Jan 2 15:04"), c.Title)
		}
		fmt.Println(u.paint("2", "/open N to continue one"))
	case "/open":
		cs := listChats()
		var n int
		fmt.Sscan(arg, &n)
		if n < 1 || n > len(cs) {
			fmt.Println("no chat", arg)
			return false
		}
		u.save()
		c := cs[n-1]
		if len(c.Messages) > 0 && c.Messages[0].Role == "system" {
			c.Messages[0].Content = chatSystem()
		}
		u.log = &c
		u.asked = map[string]bool{}
		for _, m := range c.Messages {
			switch {
			case m.Role == "user":
				fmt.Println(u.paint("1;36", "you ›"), m.Content)
			case m.Role == "assistant" && m.Content != "":
				fmt.Println(u.paint("1;32", "ai ›"), m.Content)
			}
		}
	default:
		fmt.Println("/new         start a new chat\n/models      list the models\n/model NAME  switch to another model\n/chats       the last chats\n/open N      continue chat N\n/exit        close (Ctrl+C at the prompt too)\nEnd a line with \\ to keep writing on the next line. Ctrl+C stops an answer.")
	}
	return false
}

// turn runs the model until it answers without calling a tool.
func (u *chatUI) turn(ctx context.Context) error {
	tools := chatTools()
	for step := 0; step < chatMaxSteps; step++ {
		u.fitContext()
		fmt.Print(u.paint("1;32", "ai ›") + " ")
		msg, err := u.complete(ctx, tools)
		if err != nil {
			return err
		}
		u.log.Messages = append(u.log.Messages, msg)
		if len(msg.ToolCalls) == 0 {
			fmt.Println()
			return nil
		}
		for _, tc := range msg.ToolCalls {
			out := u.runTool(ctx, tc)
			u.log.Messages = append(u.log.Messages, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: out})
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("stopped after %d tool steps", chatMaxSteps)
}

func chatTools() []map[string]any {
	var t struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	json.Unmarshal(compactTool, &t)
	return []map[string]any{{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}}}
}

// complete streams one answer, printing its text as it comes and gathering tool calls.
func (u *chatUI) complete(ctx context.Context, tools []map[string]any) (chatMsg, error) {
	body, _ := json.Marshal(map[string]any{"model": u.model, "messages": u.log.Messages, "tools": tools, "stream": true})
	req, _ := http.NewRequestWithContext(ctx, "POST", u.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return chatMsg{}, ctx.Err()
		}
		return chatMsg{}, fmt.Errorf("the model server didn't answer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return chatMsg{}, fmt.Errorf("model server: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	msg := chatMsg{Role: "assistant"}
	var text strings.Builder
	calls := map[int]*chatToolCall{}
	thinking := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		d, ok := strings.CutPrefix(l, "data:")
		if !ok {
			continue
		}
		d = strings.TrimSpace(d)
		if d == "[DONE]" {
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(d), &ev) != nil || len(ev.Choices) == 0 {
			continue
		}
		dl := ev.Choices[0].Delta
		if dl.Reasoning != "" && !thinking {
			thinking = true
			fmt.Print(u.paint("2", "(thinking) "))
		}
		if dl.Content != "" {
			text.WriteString(dl.Content)
			fmt.Print(dl.Content)
		}
		for _, t := range dl.ToolCalls {
			c := calls[t.Index]
			if c == nil {
				c = &chatToolCall{Type: "function"}
				calls[t.Index] = c
			}
			if t.ID != "" {
				c.ID = t.ID
			}
			c.Function.Name += t.Function.Name
			c.Function.Arguments += t.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return chatMsg{}, fmt.Errorf("lost the model's answer: %v", err)
	}
	if ctx.Err() != nil {
		return chatMsg{}, ctx.Err()
	}
	msg.Content = text.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		c := calls[i]
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d_%d", len(u.log.Messages), i)
		}
		msg.ToolCalls = append(msg.ToolCalls, *c)
	}
	return msg, nil
}

// runTool runs one pc call; a risky one asks here first.
func (u *chatUI) runTool(ctx context.Context, tc chatToolCall) string {
	if tc.Function.Name != "pc" {
		return "unknown tool " + tc.Function.Name + "; the only tool is pc"
	}
	c, a, err := parsePCArgs(json.RawMessage(tc.Function.Arguments))
	if err != nil {
		fmt.Println(u.paint("31", "\n  ✗ "+err.Error()))
		return err.Error()
	}
	b, _ := json.Marshal(a)
	fmt.Println(u.paint("2", "\n  · "+c+" "+clip(string(b), 160)))
	delete(a, "confirmed") // set only by the user's approval below
	if s, ok := a["confirm"].(string); ok && s != "" {
		delete(a, "confirm")
	}
	note := ""
	for _, s := range reposToAsk(c, a, u.asked) {
		note += u.askRepoUpdate(s)
	}
	approved := false
	if what := approvalAction(c, a); what != "" {
		fmt.Print(u.paint("1;33", "  allow: "+what+"? [y/N] "))
		ans, _ := u.in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Println(u.paint("2", "  denied"))
			return "The user said no to this. Don't retry it; ask what they want instead."
		}
		approved = true
	}
	if approved && (c == "run" || compactAlias[c] == "run") {
		a["confirmed"] = true
	}
	if err := phonePreflight(c, a); err != nil {
		return clip1(err.Error(), 600)
	}
	start := time.Now()
	out, _, err := runLimited(start, func() (string, []toolImage, error) { return compactDispatch(c, a) })
	if err != nil {
		msg := clip1(err.Error(), 600)
		fmt.Println(u.paint("31", "  ✗ "+firstLine(msg)))
		return note + "error: " + msg
	}
	out = capOut(out)
	fmt.Println(u.paint("2", "  ✓ "+clip(firstLine(out), 140)))
	return note + out
}

// askRepoUpdate: before a call uses a repo copy ContextSaver already has, the user says
// here whether to update it first (once per chat). Returns a note for the model.
func (u *chatUI) askRepoUpdate(s repoSpec) string {
	r := s.Owner + "/" + s.Name
	ago := repoUpdatedAgo(s)
	fmt.Print(u.paint("1;33", "  update your copy of "+r+" (last updated "+ago+") first? [y/N] "))
	ans, _ := u.in.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
		fmt.Println(u.paint("2", "  using it as it is"))
		return "note: the user chose not to update " + r + "; its copy is from " + ago + "\n"
	}
	fmt.Println(u.paint("2", "  updating "+r+"…"))
	out, err := updateRepoCopy(s.Owner, s.Name)
	if err != nil {
		fmt.Println(u.paint("31", "  ✗ "+err.Error()))
		return "note: " + err.Error() + "\n"
	}
	fmt.Println(u.paint("2", "  ✓ "+out))
	return "note: " + out + "\n"
}

// parsePCArgs reads {"c":..., "a":{...}} the way the pc tool does (a may be a JSON
// string, or the args may sit next to c).
func parsePCArgs(raw json.RawMessage) (string, map[string]any, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", nil, fmt.Errorf("bad tool arguments: %v", err)
	}
	c := strings.ToLower(strings.TrimSpace(jsonStr(top["c"])))
	if c == "" {
		c = strings.ToLower(strings.TrimSpace(jsonStr(top["cmd"])))
	}
	if c == "" {
		return "", nil, errors.New("give c (a command). " + firstLine(activeDesc))
	}
	a := map[string]any{}
	if r, ok := top["a"]; ok && len(r) > 0 && string(r) != "null" {
		if json.Unmarshal(r, &a) != nil {
			var s string
			if json.Unmarshal(r, &s) != nil || json.Unmarshal([]byte(s), &a) != nil {
				return "", nil, errors.New(`a must be an object, e.g. {"c":"read","a":{"path":"~/x.txt"}}`)
			}
		}
	} else {
		for k, v := range top {
			if k != "c" && k != "cmd" {
				var x any
				json.Unmarshal(v, &x)
				a[k] = x
			}
		}
	}
	return c, a, nil
}

// fitContext drops the oldest turns when the chat nears the model's context (about
// 3.5 characters a token), keeping the system prompt and whole turns.
func (u *chatUI) fitContext() {
	size := func() int {
		n := 0
		for _, m := range u.log.Messages {
			n += len(m.Content) + 60
			for _, t := range m.ToolCalls {
				n += len(t.Function.Arguments) + 40
			}
		}
		return n
	}
	limit := u.ctx * 3
	if size() < limit*85/100 {
		return
	}
	ms := u.log.Messages
	for size() > limit*60/100 && len(ms) > 2 {
		i := 1
		for i++; i < len(ms) && ms[i].Role != "user"; i++ {
		}
		if i >= len(ms) {
			break
		}
		ms = append(ms[:1], ms[i:]...)
		u.log.Messages = ms
	}
	fmt.Println(u.paint("2", "(older messages left out to fit the model's context)"))
}

func (u *chatUI) save() {
	if len(u.log.Messages) < 2 {
		return
	}
	u.log.Updated = time.Now()
	os.MkdirAll(chatDir(), 0o755)
	b, _ := json.MarshalIndent(u.log, "", " ")
	writeFileAtomic(filepath.Join(chatDir(), u.log.ID+".json"), b)
}

func listChats() []chatLog {
	ps, _ := filepath.Glob(filepath.Join(chatDir(), "*.json"))
	var cs []chatLog
	for _, p := range ps {
		var c chatLog
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &c) == nil {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Updated.After(cs[j].Updated) })
	if len(cs) > 15 {
		cs = cs[:15]
	}
	return cs
}
