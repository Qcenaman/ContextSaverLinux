package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type JobSpec struct {
	Name   string      `json:"name,omitempty"`
	Kind   string      `json:"kind"`
	Steps  []Step      `json:"steps,omitempty"`
	DryRun bool        `json:"dry_run,omitempty"`
	UndoOf string      `json:"undo_of,omitempty"`
	AI     *AIRequest  `json:"ai,omitempty"`
	Web    *WebRequest `json:"web,omitempty"`
	Source string      `json:"source,omitempty"`
	Run    *runArgs    `json:"run,omitempty"`
	Rag    *ragRequest `json:"rag,omitempty"`
}

type StepResult struct {
	Title      string         `json:"title"`
	Counts     map[string]int `json:"counts,omitempty"`
	Order      []string       `json:"order,omitempty"`
	Bytes      int64          `json:"bytes,omitempty"`
	Errors     []string       `json:"errors,omitempty"`
	ErrorCount int            `json:"error_count,omitempty"`
	Samples    []string       `json:"samples,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
	mu         sync.Mutex
}

type stepResultJSON struct {
	Title      string         `json:"title"`
	Counts     map[string]int `json:"counts,omitempty"`
	Order      []string       `json:"order,omitempty"`
	Bytes      int64          `json:"bytes,omitempty"`
	Errors     []string       `json:"errors,omitempty"`
	ErrorCount int            `json:"error_count,omitempty"`
	Samples    []string       `json:"samples,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
}

func (r *StepResult) MarshalJSON() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := make(map[string]int, len(r.Counts))
	for k, v := range r.Counts {
		c[k] = v
	}
	return json.Marshal(stepResultJSON{r.Title, c, append([]string(nil), r.Order...), r.Bytes,
		append([]string(nil), r.Errors...), r.ErrorCount, append([]string(nil), r.Samples...), append([]string(nil), r.Notes...)})
}

func (r *StepResult) UnmarshalJSON(b []byte) error {
	var x stepResultJSON
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	r.Title, r.Counts, r.Order, r.Bytes, r.Errors, r.ErrorCount, r.Samples, r.Notes =
		x.Title, x.Counts, x.Order, x.Bytes, x.Errors, x.ErrorCount, x.Samples, x.Notes
	return nil
}

func (r *StepResult) add(k string, n int) {
	r.mu.Lock()
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	if _, ok := r.Counts[k]; !ok {
		r.Order = append(r.Order, k)
	}
	r.Counts[k] += n
	r.mu.Unlock()
}

func (r *StepResult) addBytes(n int64) { r.mu.Lock(); r.Bytes += n; r.mu.Unlock() }

func (r *StepResult) err(s string) {
	r.mu.Lock()
	r.ErrorCount++
	if len(r.Errors) < 50 {
		r.Errors = append(r.Errors, s)
	}
	r.mu.Unlock()
}

func (r *StepResult) sample(s string) {
	r.mu.Lock()
	if len(r.Samples) < 12 {
		r.Samples = append(r.Samples, s)
	}
	r.mu.Unlock()
}

func (r *StepResult) note(s string) {
	r.mu.Lock()
	if len(r.Notes) < 12 {
		r.Notes = append(r.Notes, s)
	}
	r.mu.Unlock()
}

func (r *StepResult) text(i, n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parts []string
	for _, k := range r.Order {
		parts = append(parts, fmt.Sprintf("%s %s", k, hc(int64(r.Counts[k]))))
	}
	if r.ErrorCount > 0 {
		parts = append(parts, "failed "+hc(int64(r.ErrorCount)))
	}
	line := r.Title + ": "
	if n > 1 {
		line = fmt.Sprintf("%d. %s", i, line)
	}
	if len(parts) == 0 {
		line += "nothing to do"
	} else {
		line += strings.Join(parts, ", ")
	}
	if r.Bytes > 0 {
		line += " · " + hb(r.Bytes)
	}
	var b strings.Builder
	b.WriteString(line)
	for k, s := range r.Samples {
		if k >= 8 {
			fmt.Fprintf(&b, "\n   · …+%d more", len(r.Samples)-k)
			break
		}
		b.WriteString("\n   · " + s)
	}
	for _, s := range r.Notes {
		b.WriteString("\n   note: " + s)
	}
	for k, e := range r.Errors {
		if k >= 5 {
			break
		}
		b.WriteString("\n   ✗ " + e)
	}
	if r.ErrorCount > 5 {
		fmt.Fprintf(&b, "\n   … +%s more errors in the log", hc(int64(r.ErrorCount-5)))
	}
	return b.String()
}

type JobState struct {
	ID         string        `json:"id"`
	Name       string        `json:"name,omitempty"`
	Kind       string        `json:"kind"`
	Source     string        `json:"source,omitempty"`
	Status     string        `json:"status"`
	DryRun     bool          `json:"dry_run,omitempty"`
	Started    time.Time     `json:"started"`
	Ended      time.Time     `json:"ended,omitempty"`
	Updated    time.Time     `json:"updated"`
	FilesTotal int64         `json:"files_total"`
	FilesDone  int64         `json:"files_done"`
	BytesTotal int64         `json:"bytes_total"`
	BytesDone  int64         `json:"bytes_done"`
	Current    string        `json:"current,omitempty"`
	Steps      []*StepResult `json:"steps,omitempty"`
	Error      string        `json:"error,omitempty"`
	Output     string        `json:"output,omitempty"`
	FirstStep  string        `json:"first_step,omitempty"`
	PID        int           `json:"pid,omitempty"`
	ExitCode   *int          `json:"exit_code,omitempty"`
	Tail       *int          `json:"tail,omitempty"`
	Matched    bool          `json:"matched,omitempty"`
}

type jrec struct {
	Op     string `json:"op"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Path   string `json:"path,omitempty"`
	Method string `json:"method,omitempty"`
	Held   string `json:"held,omitempty"`
	Size   int64  `json:"size,omitempty"`
	MTime  int64  `json:"mtime,omitempty"`
	At     int64  `json:"at,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

var running sync.Map

type Job struct {
	ID     string
	Dir    string
	Spec   JobSpec
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	st     JobState

	filesTotal, filesDone, bytesTotal, bytesDone atomic.Int64
	cur                                          atomic.Value

	wmu          sync.Mutex
	jw, lw       *bufio.Writer
	jfile, lfile *os.File
	madeDirs     sync.Map
	done         chan struct{}
	ready        chan struct{}
	readyOnce    sync.Once
}

func newJob(spec JobSpec) (*Job, error) {
	now := time.Now()
	id := now.Format("20060102-150405") + fmt.Sprintf("%03d", now.Nanosecond()/1e6) + "-" + randHex(2)
	dir := filepath.Join(jobsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: id, Dir: dir, Spec: spec, ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
	j.st = JobState{ID: id, Name: spec.Name, Kind: spec.Kind, Source: spec.Source, Status: "running", DryRun: spec.DryRun, Started: time.Now()}
	if len(spec.Steps) > 0 {
		j.st.FirstStep = stepTitle(&spec.Steps[0])
	}
	if spec.Run != nil {
		j.st.FirstStep = "$ " + clip(spec.Run.Command, 200)
		j.st.Tail = spec.Run.Tail
	}
	writeJSONAtomic(filepath.Join(dir, "spec.json"), spec)
	if !spec.DryRun && spec.Kind != "run" {
		var err error
		if j.jfile, err = os.OpenFile(filepath.Join(dir, "undo.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
			return nil, err
		}
		j.jw = bufio.NewWriter(j.jfile)
		if j.lfile, err = os.OpenFile(filepath.Join(dir, "log.tsv"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
			return nil, err
		}
		j.lw = bufio.NewWriter(j.lfile)
	}
	return j, nil
}

func (j *Job) start() {
	running.Store(j.ID, j)
	j.save()
	go j.saver()
	go j.run()
}

func (j *Job) saver() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-j.done:
			return
		case <-t.C:
			if exists(filepath.Join(j.Dir, "cancel")) {
				j.cancel()
			}
			j.flush()
			j.save()
		}
	}
}

func (j *Job) run() {
	var fatal error
	defer func() {
		if r := recover(); r != nil {
			fatal = fmt.Errorf("internal error: %v", r)
		}
		if errors.Is(fatal, errCanceled) {
			fatal = nil
		}
		j.mu.Lock()
		switch {
		case fatal != nil:
			j.st.Status, j.st.Error = "failed", fatal.Error()
		case j.ctx.Err() != nil:
			j.st.Status = "canceled"
		default:
			j.st.Status = "done"
		}
		j.st.Ended = time.Now()
		j.mu.Unlock()
		j.closeFiles()
		j.save()
		running.Delete(j.ID)
		close(j.done)
		j.cancel()
	}()
	switch j.Spec.Kind {
	case "files":
		for i := range j.Spec.Steps {
			if j.ctx.Err() != nil {
				break
			}
			st := &j.Spec.Steps[i]
			res := j.newStep(stepTitle(st))
			if err := runStep(j, st, res); err != nil && !errors.Is(err, errCanceled) {
				fatal = fmt.Errorf("step %d (%s): %v", i+1, st.Op, err)
				return
			}
		}
	case "undo":
		fatal = runUndo(j)
	case "ai":
		fatal = runAI(j)
	case "web":
		fatal = runWebAsk(j)
	case "run":
		fatal = runCommand(j)
	case "rag":
		fatal = runRagUpdate(j)
	default:
		fatal = fmt.Errorf("unknown job kind %q", j.Spec.Kind)
	}
}

func (j *Job) newStep(title string) *StepResult {
	r := &StepResult{Title: title}
	j.mu.Lock()
	j.st.Steps = append(j.st.Steps, r)
	j.mu.Unlock()
	return r
}

func (j *Job) setOutput(s string) { j.mu.Lock(); j.st.Output = s; j.mu.Unlock() }

func (j *Job) snapshot() JobState {
	j.mu.Lock()
	s := j.st
	s.Steps = append([]*StepResult(nil), j.st.Steps...)
	j.mu.Unlock()
	s.FilesTotal, s.FilesDone = j.filesTotal.Load(), j.filesDone.Load()
	s.BytesTotal, s.BytesDone = j.bytesTotal.Load(), j.bytesDone.Load()
	if v, ok := j.cur.Load().(string); ok && s.Status == "running" {
		s.Current = v
	}
	s.Updated = time.Now()
	return s
}

func (j *Job) save() {
	s := j.snapshot()
	writeJSONAtomic(filepath.Join(j.Dir, "state.json"), &s)
}

func (j *Job) journal(r jrec) {
	if r.At == 0 {
		r.At = time.Now().UnixNano()
	}
	b, _ := json.Marshal(r)
	j.wmu.Lock()
	if j.jw != nil {
		j.jw.Write(b)
		j.jw.WriteByte('\n')
	}
	j.wmu.Unlock()
}

func (j *Job) log(action, src, dst, note string) {
	j.wmu.Lock()
	if j.lw != nil {
		fmt.Fprintf(j.lw, "%s\t%s\t%s\t%s\t%s\n", time.Now().Format("15:04:05"), action, src, dst, note)
	}
	j.wmu.Unlock()
}

func (j *Job) flush() {
	j.wmu.Lock()
	if j.jw != nil {
		j.jw.Flush()
	}
	if j.lw != nil {
		j.lw.Flush()
	}
	j.wmu.Unlock()
}

func (j *Job) closeFiles() {
	j.flush()
	j.wmu.Lock()
	if j.jfile != nil {
		j.jfile.Close()
	}
	if j.lfile != nil {
		j.lfile.Close()
	}
	j.jw, j.lw, j.jfile, j.lfile = nil, nil, nil, nil
	j.wmu.Unlock()
}

func (j *Job) addBytes(n int64)           { j.bytesDone.Add(n) }
func (j *Job) addTotals(f int64, b int64) { j.filesTotal.Add(f); j.bytesTotal.Add(b) }
func (j *Job) fileDone()                  { j.filesDone.Add(1) }
func (j *Job) setCurrent(s string)        { j.cur.Store(s) }

func stepTitle(st *Step) string {
	from := ""
	if len(st.From) > 0 {
		from = st.From[0]
		if len(st.From) > 1 {
			from += fmt.Sprintf(" +%d more", len(st.From)-1)
		}
	}
	switch st.Op {
	case "copy", "move", "sync":
		return fmt.Sprintf("%s %s → %s", st.Op, from, st.To)
	case "mkdir", "write":
		if st.To != "" {
			return st.Op + " " + st.To
		}
	}
	return st.Op + " " + from
}

// ---------- lookup ----------

func loadState(id string) (*JobState, error) {
	if v, ok := running.Load(id); ok {
		s := v.(*Job).snapshot()
		return &s, nil
	}
	b, err := os.ReadFile(filepath.Join(jobsDir(), id, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("no job %s", id)
	}
	var s JobState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Status == "running" && time.Since(s.Updated) > 20*time.Second {
		if s.Kind == "run" && s.PID > 0 && pidAlive(s.PID) {
			s.Status = "running"
			s.Current = "still running after Claude restarted (pid " + fmt.Sprint(s.PID) + ")"
		} else {
			s.Status = "interrupted"
		}
	}
	return &s, nil
}

func recentJobIDs(n int) []string {
	ents, _ := os.ReadDir(jobsDir())
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if len(ids) > n {
		ids = ids[:n]
	}
	return ids
}

func resolveJobID(id string) (string, error) {
	id = strings.Trim(strings.TrimSpace(id), `:."'`)
	if id == "" || id == "latest" || id == "last" {
		ids := recentJobIDs(1)
		if len(ids) == 0 {
			return "", errors.New("no jobs yet")
		}
		return ids[0], nil
	}
	if exists(filepath.Join(jobsDir(), id)) {
		return id, nil
	}
	for _, x := range recentJobIDs(500) {
		if strings.HasSuffix(x, id) || strings.HasPrefix(x, id) {
			return x, nil
		}
	}
	return "", fmt.Errorf("no job %s", id)
}

func cancelJob(id string) string {
	if v, ok := running.Load(id); ok {
		v.(*Job).cancel()
		return "Canceling job " + id + "."
	}
	s, err := loadState(id)
	if err != nil {
		return err.Error()
	}
	if s.Status != "running" {
		return "Job " + id + " isn't running (" + s.Status + ")."
	}
	if s.Kind == "run" && s.PID > 0 && time.Since(s.Updated) > 20*time.Second {
		killTree(s.PID)
		s.Status, s.Ended = "canceled", time.Now()
		writeJSONAtomic(filepath.Join(jobsDir(), id, "state.json"), s)
		return "Stopped job " + id + " (pid " + fmt.Sprint(s.PID) + ")."
	}
	os.WriteFile(filepath.Join(jobsDir(), id, "cancel"), []byte("1"), 0o644)
	return "Asked job " + id + " to stop; it stops within a second or two."
}

func waitJob(j *Job, secs int) {
	if secs <= 0 {
		return
	}
	select {
	case <-j.done:
	case <-time.After(time.Duration(secs) * time.Second):
	}
}

func progressLine(s *JobState) string {
	var parts []string
	if s.BytesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d%%", min(s.BytesDone*100/max(s.BytesTotal, 1), 100)))
	} else if s.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d%%", min(s.FilesDone*100/max(s.FilesTotal, 1), 100)))
	}
	if s.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s/%s files", hc(s.FilesDone), hc(s.FilesTotal)))
	}
	if s.BytesTotal > 0 {
		el := time.Since(s.Started).Seconds()
		parts = append(parts, fmt.Sprintf("%s of %s", hb(s.BytesDone), hb(s.BytesTotal)))
		if el > 1 && s.BytesDone > 0 {
			speed := float64(s.BytesDone) / el
			parts = append(parts, hb(int64(speed))+"/s")
			if left := s.BytesTotal - s.BytesDone; left > 0 {
				parts = append(parts, "ETA "+hd(time.Duration(float64(left)/speed*float64(time.Second))))
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "working")
	}
	return strings.Join(parts, " · ")
}

func formatState(s *JobState) string {
	if s.Kind == "run" {
		return formatRun(s)
	}
	var b strings.Builder
	title := s.ID
	if s.Name != "" {
		title += ` "` + s.Name + `"`
	}
	dry := ""
	if s.DryRun {
		dry = " (dry run, nothing changed)"
	}
	if s.Status == "running" {
		fmt.Fprintf(&b, "Job %s: running%s · %s", title, dry, progressLine(s))
		if s.Current != "" {
			b.WriteString("\nNow: " + clip(s.Current, 160))
		}
	} else {
		end := s.Ended
		if end.IsZero() {
			end = s.Updated
		}
		fmt.Fprintf(&b, "Job %s: %s in %s%s", title, s.Status, hd(end.Sub(s.Started)), dry)
	}
	for i, r := range s.Steps {
		b.WriteString("\n" + r.text(i+1, len(s.Steps)))
	}
	if s.Error != "" {
		b.WriteString("\nError: " + s.Error)
	}
	if s.Output != "" {
		b.WriteString("\n" + s.Output)
	}
	if s.Status == "running" {
		b.WriteString("\nStill running; check again with job status id=" + s.ID)
	} else if !s.DryRun && s.Kind != "ai" && s.Kind != "web" && s.Kind != "rag" && exists(filepath.Join(jobsDir(), s.ID, "log.tsv")) {
		b.WriteString("\nLog: " + filepath.Join(jobsDir(), s.ID, "log.tsv"))
	}
	if s.Status == "interrupted" {
		b.WriteString("\nThe app stopped before the job finished. Running the same job again picks up where it left off (finished files are skipped).")
	}
	return b.String()
}

func listJobs(n int) string {
	ids := recentJobIDs(n)
	if len(ids) == 0 {
		return "No jobs yet."
	}
	var b strings.Builder
	for _, id := range ids {
		s, err := loadState(id)
		if err != nil {
			continue
		}
		what := s.Name
		if what == "" {
			what = s.FirstStep
		}
		if what == "" {
			what = s.Kind
		}
		dry := ""
		if s.DryRun {
			dry = " [dry run]"
		}
		src := ""
		if s.Source == "you" {
			src = " [run by user]"
		}
		fmt.Fprintf(&b, "%s  %-11s %s%s%s\n", id, s.Status, clip(what, 90), dry, src)
	}
	return strings.TrimRight(b.String(), "\n")
}

func waitAllJobs() {
	running.Range(func(_, v any) bool {
		if v.(*Job).Spec.Kind == "run" {
			return true
		}
		<-v.(*Job).done
		return true
	})
}
