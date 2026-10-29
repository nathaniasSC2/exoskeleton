// Package jobs runs real work as full Claude Code sessions in the background:
// project CLAUDE.md, the notes, every built-in tool. The chat only ever sees
// the final report, never the terminal output, so jobs can't bloat its context.
//
// Each job runs as its own transient systemd service (exo-job-<id>), not as
// a child of exo: restarting or redeploying exo doesn't touch running jobs,
// and exo re-attaches to them when it comes back (Resume).
package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/config"
	"github.com/nathaniasSC2/exoskeleton/internal/store"
	"github.com/nathaniasSC2/exoskeleton/internal/tools"
)

type Runner struct {
	Cfg    *config.Config
	Store  *store.Store
	Brief  func() string // job system-prompt addendum (prompts/job.md)
	Notify func(person, channel, chat, text string, files []string)
	// Register exposes the job's toolset (send_file) over exo's MCP server
	// until the returned func is called.
	Register func(key string, c *tools.Ctx) func()
	// Local reports whether the assistant is on the local model. Jobs started
	// then run Claude Code against it (llama-server's Anthropic-compatible API),
	// one at a time, since they share its context pool with chat.
	Local func() bool

	kits map[string]tools.Kit

	sem       chan struct{}
	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc // queued jobs waiting for a slot
	cancelled map[int64]bool               // running jobs that were stopped on purpose
}

const progressEvery = 30 * time.Minute // a running job's progress note

const localJobBrief = "You are running on a local model, not Claude: smaller and slower. Keep each step simple and concrete, check every result before moving on, and never put a long file or script inside one tool call (write it in pieces). If the task is beyond what you can do reliably, say so in your report instead of guessing."

func unitName(id int64) string { return fmt.Sprintf("exo-job-%d", id) }

func New(cfg *config.Config, st *store.Store) *Runner {
	r := &Runner{Cfg: cfg, Store: st, sem: make(chan struct{}, max(1, cfg.MaxJobs)),
		cancels: map[int64]context.CancelFunc{}, cancelled: map[int64]bool{}, kits: loadKits(cfg.Path("kits.json"))}
	st.DB.Exec(`ALTER TABLE jobs ADD COLUMN kit TEXT NOT NULL DEFAULT 'general'`) // no-ops once they exist
	st.DB.Exec(`ALTER TABLE jobs ADD COLUMN files TEXT NOT NULL DEFAULT ''`)
	st.DB.Exec(`ALTER TABLE jobs ADD COLUMN brain TEXT NOT NULL DEFAULT 'claude'`)
	return r
}

// Resume re-attaches to jobs that were running (or still queued) when exo
// last stopped. Call once at startup, after Register and Notify are set.
func (r *Runner) Resume() {
	type pending struct {
		id                                        int64
		person, channel, chat, task, dir, kit, st string
		started                                   int64
	}
	var ps []pending
	rows, err := r.Store.DB.Query(`SELECT id,person,channel,chat,task,workdir,kit,status,COALESCE(started,0) FROM jobs WHERE status IN ('queued','running')`)
	if err != nil {
		return
	}
	for rows.Next() {
		var x pending
		rows.Scan(&x.id, &x.person, &x.channel, &x.chat, &x.task, &x.dir, &x.kit, &x.st, &x.started)
		ps = append(ps, x)
	}
	rows.Close()
	for _, x := range ps {
		if x.st == "queued" {
			ctx, cancel := context.WithTimeout(context.Background(), r.Cfg.JobTimeout)
			r.mu.Lock()
			r.cancels[x.id] = cancel
			r.mu.Unlock()
			go r.run(ctx, x.id, x.person, x.channel, x.chat, x.task, x.dir, x.kit)
			continue
		}
		slog.Info("re-attaching to job", "id", x.id)
		select { // it already holds a slot in practice; don't block on it
		case r.sem <- struct{}{}:
		default:
		}
		go r.watch(x.id, x.person, x.channel, x.chat, x.kit, time.Unix(x.started, 0))
	}
}

func (r *Runner) Start(person, channel, chat, task, dir string) (int64, error) {
	return r.StartKit(person, channel, chat, task, dir, "general")
}

// loadKits reads kits.json: {"name": {short, desc, dir, brief, mcp}}.
func loadKits(path string) map[string]tools.Kit {
	kits := map[string]tools.Kit{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return kits
	}
	if err := json.Unmarshal(raw, &kits); err != nil {
		slog.Warn("kits.json", "err", err)
		return map[string]tools.Kit{}
	}
	for name, k := range kits {
		k.Name = name
		kits[name] = k
	}
	return kits
}

// Kits lists the configured specialist kits, by name.
func (r *Runner) Kits() []tools.Kit {
	var out []tools.Kit
	for _, k := range r.kits {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StartKit runs a job with a specialist kit: only that kit's tools are loaded.
func (r *Runner) StartKit(person, channel, chat, task, dir, kit string) (int64, error) {
	if _, ok := r.kits[kit]; !ok && kit != "general" {
		return 0, fmt.Errorf("unknown kit %q", kit)
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return 0, fmt.Errorf("empty task")
	}
	res, err := r.Store.DB.Exec(`INSERT INTO jobs(person,channel,chat,task,workdir,created,kit) VALUES(?,?,?,?,?,?,?)`,
		person, channel, chat, task, dir, time.Now().Unix(), kit)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	ctx, cancel := context.WithTimeout(context.Background(), r.Cfg.JobTimeout)
	r.mu.Lock()
	r.cancels[id] = cancel
	r.mu.Unlock()
	go r.run(ctx, id, person, channel, chat, task, dir, kit)
	return id, nil
}

// run waits for a slot, writes the job's private files and launches its unit.
func (r *Runner) run(ctx context.Context, id int64, person, channel, chat, task, dir, kit string) {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
		r.finish(id, person, channel, chat, "cancelled", "cancelled before it started", 0, 0)
		return
	}
	r.mu.Lock()
	delete(r.cancels, id)
	r.mu.Unlock()
	fail := func(msg string) {
		<-r.sem
		r.finish(id, person, channel, chat, "failed", msg, 0, 0)
	}
	local := r.Local != nil && r.Local()
	for local { // one local job at a time
		var n int
		r.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='running' AND brain='local' AND id<>?`, id).Scan(&n)
		if n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			fail("cancelled while waiting for the local model")
			return
		case <-time.After(10 * time.Second):
		}
	}
	os.MkdirAll(r.Cfg.Path("jobs"), 0o755)
	base := r.Cfg.Path("jobs", fmt.Sprint(id))
	logPath := base + ".jsonl"
	mcpCfg, _ := json.Marshal(map[string]any{"mcpServers": r.mcpServers(id, kit)})
	files := map[string]string{
		base + ".mcp.json": string(mcpCfg), // carries exo's MCP token: 0600, never on a command line
		base + ".prompt":   fmt.Sprintf("Job #%d from %s, sent via %s.\n\n%s", id, person, r.Cfg.Name, task),
		base + ".brief":    strings.TrimSpace(r.Brief() + "\n\n" + r.kits[kit].Brief),
	}
	if local {
		files[base+".brief"] += "\n\n" + localJobBrief
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			fail("couldn't write the job's files: " + err.Error())
			return
		}
	}
	start := time.Now()
	brain := "claude"
	if local {
		brain = "local"
	}
	u, err := user.Current()
	if err != nil {
		fail("couldn't look up the current user: " + err.Error())
		return
	}
	r.Store.DB.Exec(`UPDATE jobs SET status='running', started=?, log_path=?, brain=? WHERE id=?`, start.Unix(), logPath, brain, id)
	home := u.HomeDir
	args := []string{"-n", "systemd-run", "--unit=" + unitName(id), "--collect", "--quiet",
		"--uid=" + u.Uid, "--gid=" + u.Gid, "--working-directory=" + dir}
	model := []string{"--model", r.Cfg.Model, "--effort", r.Cfg.Effort}
	if ag := r.Cfg.Agents(); ag != "" {
		model = append(model, "--agents", ag)
	}
	bin := r.Cfg.ClaudeBin
	if local {
		// No plan token: Claude Code talks to llama-server instead (measured
		// 2026-10-11: a du + write job finished correctly in 32 s).
		base := strings.TrimSuffix(r.Cfg.LocalURL, "/v1")
		m := r.Cfg.LocalModel
		args = append(args, "--setenv=ANTHROPIC_BASE_URL="+base, "--setenv=ANTHROPIC_AUTH_TOKEN=local",
			"--setenv=ANTHROPIC_MODEL="+m, "--setenv=ANTHROPIC_SMALL_FAST_MODEL="+m,
			"--setenv=ANTHROPIC_DEFAULT_HAIKU_MODEL="+m, "--setenv=ANTHROPIC_DEFAULT_SONNET_MODEL="+m,
			"--setenv=ANTHROPIC_DEFAULT_OPUS_MODEL="+m, "--setenv=CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
		model = []string{"--model", m}
	} else if r.Cfg.ClaudeEnv != "" {
		args = append(args, "-p", "EnvironmentFile=-"+r.Cfg.ClaudeEnv) // the Claude login, read by systemd, never on a command line
	}
	args = append(args,
		"-p", "StandardInput=file:"+base+".prompt",
		"-p", "StandardOutput=file:"+logPath,
		"-p", "StandardError=file:"+base+".err",
		"-p", fmt.Sprintf("RuntimeMaxSec=%d", int(r.Cfg.JobTimeout.Seconds())),
		"-p", "MemoryMax=8G",
		"--setenv=HOME="+home,
		"--setenv=PATH="+home+"/.local/bin:/usr/local/bin:/usr/bin:/bin",
		"--setenv=XDG_RUNTIME_DIR=/run/user/"+u.Uid,
		"--setenv=DISABLE_AUTOUPDATER=1",
		"--setenv=MCP_TOOL_TIMEOUT=3600000", // kit tools (renders) can be slow
		"--", bin, "-p", "--output-format", "stream-json", "--verbose")
	args = append(args, model...)
	args = append(args, "--dangerously-skip-permissions",
		"--mcp-config", base+".mcp.json", "--append-system-prompt-file", base+".brief")
	if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
		fail(fmt.Sprintf("couldn't start the job: %v %s", err, strings.TrimSpace(string(out))))
		return
	}
	r.watch(id, person, channel, chat, kit, start)
}

// watch follows a job's log until its unit ends, then reports. It's also how
// exo re-attaches after a restart: everything it needs is on disk.
func (r *Runner) watch(id int64, person, channel, chat, kit string, start time.Time) {
	defer func() { <-r.sem }()
	jc := &tools.Ctx{Person: r.Cfg.Person(person), Channel: channel, Chat: chat, Engine: "claude", Set: "job", Kit: kit,
		// Files are saved as they're sent, so they survive an exo restart too.
		Attach: func(p string) { r.Store.DB.Exec(`UPDATE jobs SET files = files || ? WHERE id=?`, p+"\n", id) }}
	if r.Register != nil && jc.Person != nil {
		defer r.Register(fmt.Sprintf("job:%d", id), jc)()
	}
	logPath := r.Cfg.Path("jobs", fmt.Sprintf("%d.jsonl", id))
	var result string
	var cost float64
	failed, gotResult := false, false
	est := usageEstimator{last: map[string]map[string]any{}, model: map[string]string{}}
	var off int64
	lastAct := "no output yet" // the session's latest step, for progress notes
	scan := func() {
		f, err := os.Open(logPath)
		if err != nil {
			return
		}
		defer f.Close()
		f.Seek(off, io.SeekStart)
		rd := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil { // a partial last line is read on the next pass
				return
			}
			off += int64(len(line))
			var ev map[string]any
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			est.add(ev)
			if msg, _ := ev["message"].(map[string]any); ev["type"] == "assistant" && msg != nil {
				content, _ := msg["content"].([]any)
				for _, c := range content {
					c, _ := c.(map[string]any)
					switch c["type"] {
					case "text":
						if t, _ := c["text"].(string); strings.TrimSpace(t) != "" {
							lastAct = "\"" + clip(strings.Join(strings.Fields(t), " "), 160) + "\""
						}
					case "tool_use":
						lastAct = fmt.Sprintf("running %v", c["name"])
					}
				}
			}
			if ev["type"] == "result" {
				result, _ = ev["result"].(string)
				cost, _ = ev["total_cost_usd"].(float64)
				isErr, _ := ev["is_error"].(bool)
				failed, gotResult = isErr || ev["subtype"] != "success", true
			}
		}
	}
	// A long job posts progress every half hour (and when exo re-attaches,
	// from the next mark on), with the way to stop it.
	nextNote := start.Add(progressEvery)
	for !nextNote.After(time.Now()) {
		nextNote = nextNote.Add(progressEvery)
	}
	for {
		scan()
		out, _ := exec.Command("systemctl", "is-active", unitName(id)).Output()
		if s := strings.TrimSpace(string(out)); s != "active" && s != "activating" && s != "deactivating" {
			break
		}
		if time.Now().After(nextNote) && r.Notify != nil {
			nextNote = nextNote.Add(progressEvery)
			r.Notify(person, channel, chat, fmt.Sprintf("Job #%d is still running (%s). Latest: %s. It stops on its own at %s; say \"stop job %d\" to end it now.",
				id, human(time.Since(start)), lastAct, human(r.Cfg.JobTimeout), id), nil)
		}
		time.Sleep(3 * time.Second)
	}
	time.Sleep(time.Second)
	scan()
	r.mu.Lock()
	cancelled := r.cancelled[id]
	delete(r.cancelled, id)
	r.mu.Unlock()
	status := "done"
	switch {
	case cancelled:
		status, result = "cancelled", strings.TrimSpace("cancelled. "+result)
	case !gotResult && time.Since(start) >= r.Cfg.JobTimeout-time.Minute:
		status, result = "failed", fmt.Sprintf("timed out after %s.", r.Cfg.JobTimeout)
	case !gotResult:
		errTail, _ := os.ReadFile(r.Cfg.Path("jobs", fmt.Sprintf("%d.err", id)))
		status, result = "failed", strings.TrimSpace("the session ended without a report. "+lastLines(string(errTail), 3))
	case failed:
		status = "failed"
	}
	if !gotResult && cost == 0 {
		cost = est.usd() // killed before Claude Code reported its own total
	}
	os.Remove(r.Cfg.Path("jobs", fmt.Sprintf("%d.mcp.json", id))) // the token copy isn't needed any more
	r.finish(id, person, channel, chat, status, strings.TrimSpace(result), cost, time.Since(start))
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " / ")
}

// mcpServers is what every job can use: the notes, exo's job tools
// (send_file), plus the kit's own server from kits.json.
func (r *Runner) mcpServers(id int64, kit string) map[string]any {
	servers := map[string]any{}
	if k, ok := r.kits[kit]; ok && k.MCP != nil { // a specialist kit brings only its own server
		servers[kit] = k.MCP
	}
	if r.Cfg.NotesMCP != "" {
		servers["notes"] = map[string]any{"type": "http", "url": r.Cfg.NotesMCP}
	}
	servers["exo"] = map[string]any{"type": "http", "url": fmt.Sprintf("http://%s/mcp/job/%d", r.Cfg.MCPAddr, id),
		"headers": map[string]string{"Authorization": "Bearer " + r.Cfg.MCPToken}}
	return servers
}

func (r *Runner) finish(id int64, person, channel, chat, status, result string, cost float64, took time.Duration) {
	// Local jobs cost nothing; Claude Code still prices them as if they were Claude.
	r.Store.DB.Exec(`UPDATE jobs SET status=?, result=?, cost=CASE WHEN brain='local' THEN 0 ELSE ? END, finished=? WHERE id=?`,
		status, result, cost, time.Now().Unix(), id)
	var fl string
	r.Store.DB.QueryRow(`SELECT files FROM jobs WHERE id=?`, id).Scan(&fl)
	var files []string
	for _, f := range strings.Split(fl, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	slog.Info("job finished", "id", id, "status", status, "took", took.Round(time.Second))
	msg := fmt.Sprintf("Job #%d %s (%s):\n\n%s", id, status, human(took), clip(result, 1800))
	if r.Notify != nil {
		r.Notify(person, channel, chat, msg, files)
	}
}

func (r *Runner) Cancel(person string, id int64) error {
	var owner, status string
	r.Store.DB.QueryRow(`SELECT person,status FROM jobs WHERE id=?`, id).Scan(&owner, &status)
	if owner != person {
		return fmt.Errorf("no job #%d", id)
	}
	r.mu.Lock()
	c, queued := r.cancels[id]
	if !queued && status == "running" {
		r.cancelled[id] = true
	}
	r.mu.Unlock()
	switch {
	case queued:
		c()
	case status == "running":
		if out, err := exec.Command("sudo", "-n", "systemctl", "stop", unitName(id)).CombinedOutput(); err != nil {
			return fmt.Errorf("couldn't stop job #%d: %v %s", id, err, out)
		}
	default:
		return fmt.Errorf("job #%d isn't running (%s)", id, status)
	}
	return nil
}

// Running describes a person's running jobs for the heartbeat: what each is
// doing, for how long, and when it last showed signs of life.
func (r *Runner) Running(person string) []string {
	rows, err := r.Store.DB.Query(`SELECT id,kit,task,COALESCE(started,created),log_path FROM jobs WHERE person=? AND status IN ('queued','running') ORDER BY id`, person)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, started int64
		var kit, task, logPath string
		rows.Scan(&id, &kit, &task, &started, &logPath)
		quiet := "no output yet"
		if st, err := os.Stat(logPath); err == nil {
			quiet = "last activity " + human(time.Since(st.ModTime())) + " ago"
		}
		out = append(out, fmt.Sprintf("#%d (%s kit, running %s, %s): %s\n  latest: %s", id, kit,
			human(time.Since(time.Unix(started, 0))), quiet, clip(strings.Join(strings.Fields(task), " "), 200),
			strings.ReplaceAll(Tail(logPath, 3), "\n", "\n  ")))
	}
	return out
}

func (r *Runner) Describe(person string, id int64) string {
	if id == 0 {
		rows, err := r.Store.DB.Query(`SELECT id,status,task,created,COALESCE(finished,0) FROM jobs WHERE person=? ORDER BY id DESC LIMIT 6`, person)
		if err != nil {
			return err.Error()
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var jid, created, fin int64
			var st, task string
			rows.Scan(&jid, &st, &task, &created, &fin)
			fmt.Fprintf(&b, "#%d %s, %s: %s\n", jid, st, ago(created), clip(task, 120))
		}
		if b.Len() == 0 {
			return "no jobs yet"
		}
		return b.String()
	}
	var st, task, result, logPath string
	var created, started, fin int64
	err := r.Store.DB.QueryRow(`SELECT status,task,result,log_path,created,COALESCE(started,0),COALESCE(finished,0) FROM jobs WHERE id=? AND person=?`,
		id, person).Scan(&st, &task, &result, &logPath, &created, &started, &fin)
	if err != nil {
		return fmt.Sprintf("no job #%d", id)
	}
	s := fmt.Sprintf("#%d %s (created %s)\ntask: %s\n", id, st, ago(created), clip(task, 400))
	if result != "" {
		return s + "result: " + clip(result, 1500)
	}
	if st == "running" {
		s += fmt.Sprintf("running for %s. Latest activity:\n%s", human(time.Since(time.Unix(started, 0))), Tail(logPath, 6))
	}
	return s
}

// Tail summarizes the last few events of a job log: assistant text and tool names.
func Tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, _ := f.Stat()
	off := max(0, st.Size()-256<<10)
	f.Seek(off, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	var lines []string
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev["type"] != "assistant" {
			continue
		}
		msg, _ := ev["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			blk, _ := b.(map[string]any)
			switch blk["type"] {
			case "text":
				lines = append(lines, "· "+clip(strings.Join(strings.Fields(fmt.Sprint(blk["text"])), " "), 200))
			case "tool_use":
				in, _ := json.Marshal(blk["input"])
				lines = append(lines, fmt.Sprintf("→ %v %s", blk["name"], clip(string(in), 140)))
			}
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// human formats a duration the way you'd text it: 40s, 12m, 1h5m.
func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func ago(ts int64) string {
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// usageEstimator prices the assistant messages in a job's stream, for jobs
// that die (timeout, kill) before Claude Code emits its result event. A
// message's usage repeats once per content block, so the last copy wins. The
// stream leaves out thinking tokens, so this runs ~25% under the real total.
type usageEstimator struct {
	last  map[string]map[string]any
	model map[string]string
}

// $ per million tokens: input, output, cache read, cache write (5 min), cache write (1 h).
var jobPrices = map[string][5]float64{
	"opus":   {4, 20, 0.20, 5, 8},
	"sonnet": {2, 10, 0.20, 2.5, 4},
	"haiku":  {0.10, 0.50, 0.01, 0.125, 0.20},
}

func (u *usageEstimator) add(ev map[string]any) {
	if ev["type"] != "assistant" {
		return
	}
	msg, _ := ev["message"].(map[string]any)
	if msg == nil {
		return
	}
	id, _ := msg["id"].(string)
	us, _ := msg["usage"].(map[string]any)
	if id == "" || us == nil {
		return
	}
	u.last[id] = us
	u.model[id], _ = msg["model"].(string)
}

func (u *usageEstimator) usd() float64 {
	var total float64
	for id, us := range u.last {
		price := jobPrices["opus"]
		for k, p := range jobPrices {
			if strings.Contains(u.model[id], k) {
				price = p
			}
		}
		n := func(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }
		cc, _ := us["cache_creation"].(map[string]any)
		h1 := n(cc, "ephemeral_1h_input_tokens")
		total += (n(us, "input_tokens")*price[0] + n(us, "output_tokens")*price[1] +
			n(us, "cache_read_input_tokens")*price[2] + (n(us, "cache_creation_input_tokens")-h1)*price[3] + h1*price[4]) / 1e6
	}
	return total
}
