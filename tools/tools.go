// Package tools is the assistant's whole toolbox. The same registry backs the
// MCP server (when Claude Code is the brain) and the local model's tool loop.
// Keep it small: every tool here costs schema tokens on every turn.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/proc"
	"github.com/nathaniasSC2/exoskeleton/store"
)

type Ctx struct {
	Person        *config.Person
	Channel, Chat string
	TurnID        int64
	Key           string // how this context is registered: turn:412, job:51, heartbeat
	Engine        string // "claude" | "local"
	Set           string // "" (chat), "heartbeat", "job" (background job)
	Replay        bool   // side effects are skipped
	Attach        func(path string)
	Deny          map[string]bool // tools this context may not use
	Kit           string          // jobs: which kit (general, or one from kits.json)

	mu      sync.Mutex
	tainted string   // what untrusted content this turn has read ("" = none)
	used    []string // tools actually run (for the dashboard)
	stubs   int      // side effects stubbed in a replay (fake ids)
	values  map[string]any
	done    []func()
}

// Value returns per-context state a tool keeps between calls (a browser tab,
// a session), creating it with init on first use.
func (c *Ctx) Value(key string, init func() any) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.values[key]; ok {
		return v
	}
	if c.values == nil {
		c.values = map[string]any{}
	}
	v := init()
	c.values[key] = v
	return v
}

// OnDone registers cleanup for when the turn or job ends (Close).
func (c *Ctx) OnDone(f func()) { c.mu.Lock(); c.done = append(c.done, f); c.mu.Unlock() }

// Close runs the cleanups registered with OnDone, once.
func (c *Ctx) Close() {
	c.mu.Lock()
	fs := c.done
	c.done = nil
	c.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// Taint marks the turn as having read untrusted content (web pages, search
// results). Gated tools refuse to run in a tainted turn.
func (c *Ctx) Taint(by string) {
	c.mu.Lock()
	if c.tainted == "" {
		c.tainted = by
	}
	c.mu.Unlock()
}

func (c *Ctx) TaintedBy() string { c.mu.Lock(); defer c.mu.Unlock(); return c.tainted }

func (c *Ctx) markUsed(name string) { c.mu.Lock(); c.used = append(c.used, name); c.mu.Unlock() }

// Used lists the tools run in this context, in order.
func (c *Ctx) Used() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.used...)
}

type Result struct {
	Text    string
	Image   string // optional JPEG path to show the model
	IsError bool
}

type Tool struct {
	Name, Desc string
	Params     map[string]any // JSON-schema properties
	Required   []string
	Admin      bool     // admins only
	Where      string   // "both" | "claude" | "local" (chat tools only)
	Set        string   // "" = the chat toolset; "heartbeat" = heartbeat-only; any other name = an agent's own direct set
	Kits       []string // background jobs with one of these kits get it too ("general" = plain start_job jobs)
	SideEffect bool
	Untrusted  bool                        // returns content written by strangers: fenced, and it taints the turn
	Branch     string                      // toolbox branch: memory, time, web, notes, server, create, files, self
	Short      string                      // one-line purpose for the toolbox index
	Gated      bool                        // refuses in a turn that has read untrusted content
	Timeout    time.Duration               // default 2 min
	Check      func(c *Ctx, a Args) string // optional argument check ("" = fine); runs before Run and before replay stubs
	Run        func(ctx context.Context, c *Ctx, a Args) Result
	// Replays skip side effects. ReplayRuns lets the read-only uses of a side-effect
	// tool run anyway (a list action); Stub is the reply the real tool would give.
	ReplayRuns func(a Args) bool
	Stub       func(c *Ctx, a Args) string
}

func (t *Tool) Schema() map[string]any {
	s := map[string]any{"type": "object", "properties": t.Params}
	if len(t.Required) > 0 {
		s["required"] = t.Required
	}
	if t.Params == nil {
		s["properties"] = map[string]any{}
	}
	return s
}

type Args map[string]any

func (a Args) S(k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprint(int64(v))
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (a Args) I(k string) int64 {
	var n int64
	fmt.Sscan(a.S(k), &n)
	return n
}

// B is true for a JSON true or the strings "true"/"yes"/"1".
func (a Args) B(k string) bool {
	if v, ok := a[k].(bool); ok {
		return v
	}
	switch strings.ToLower(a.S(k)) {
	case "true", "yes", "1":
		return true
	}
	return false
}

// Jobs is implemented by the job runner (kept as an interface to avoid an import cycle).
type Jobs interface {
	Start(person, channel, chat, task, dir string) (int64, error)
	StartKit(person, channel, chat, task, dir, kit string) (int64, error)
	Kits() []Kit
	Describe(person string, id int64) string
	Cancel(person string, id int64) error
}

// Kit is a specialist: a job that loads only its own MCP server and brief
// (kits.json in the data dir).
type Kit struct {
	Name  string         `json:"-"`
	Short string         `json:"short"` // index line
	Desc  string         `json:"desc"`  // full description for help
	Dir   string         `json:"dir"`   // working directory
	Brief string         `json:"brief"` // appended to the job brief
	MCP   map[string]any `json:"mcp"`   // the kit's MCP server config, as in Claude Code's --mcp-config
}

type Registry struct {
	Cfg   *config.Config
	Store *store.Store
	Jobs  Jobs
	Notes NotesClient // nil = no notes server configured
	// Routines, SetInterests and SetLoops are implemented by the agent.
	Routines     RoutineManager
	SetInterests func(person, text string) error
	SetLoops     func(person, text string)
	// Optional: a better search for the local engine's web_search (a real
	// browser, say), and a reader for pages plain HTTP can't get (bot walls).
	Search func(ctx context.Context, query string) (string, error)
	Fetch  func(ctx context.Context, c *Ctx, url string) (string, error)
	tools  []*Tool
}

type RoutineManager interface {
	AddRoutine(c *Ctx, name, cron, task, notify string) (string, error)
	ListRoutines(person string) string
	ChangeRoutine(person string, id int64, action string) (string, error)
}

// NotesClient calls a tool on the notes MCP server.
type NotesClient interface {
	Call(ctx context.Context, tool string, args map[string]any) (string, error)
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func New(cfg *config.Config, st *store.Store, jobs Jobs, notes NotesClient) *Registry {
	r := &Registry{Cfg: cfg, Store: st, Jobs: jobs, Notes: notes}
	r.tools = []*Tool{
		{Name: "send_file", Where: "both", Admin: true, SideEffect: true,
			Desc:     "Send a file from the server (image, .ics, pdf, ...) with your reply.",
			Params:   map[string]any{"path": str("Absolute path on the server")},
			Required: []string{"path"}, Run: r.sendFile},
		{Name: "remember", Where: "both", SideEffect: true,
			Desc:     "Save a durable fact about this person (preferences, plans, people, recurring things). Not for trivia or one-off chatter.",
			Params:   map[string]any{"fact": str("One self-contained sentence")},
			Required: []string{"fact"}, Run: r.remember},
		{Name: "forget", Where: "both", SideEffect: true,
			Desc:     "Delete a saved fact by id (ids appear in recall results).",
			Params:   map[string]any{"id": num("Fact id")},
			Required: []string{"id"}, Run: r.forget},
		{Name: "reminder", Where: "both", SideEffect: true,
			Desc: "Set, list or cancel a reminder. At the given time the reminder is sent to this person in this chat.",
			Params: map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"add", "list", "cancel"}},
				"at":     str("For add: local time as YYYY-MM-DD HH:MM (24h)"),
				"text":   str("For add: what to remind them about, written as the message they'll get"),
				"id":     num("For cancel: reminder id"),
			},
			Required: []string{"action"}, Check: r.reminderCheck, Run: r.reminder},
		{Name: "routine", Where: "both", SideEffect: true,
			Desc: "Set up, list, pause, resume or remove a recurring task you run on a schedule (\"every morning check X\"). Each run you do the task and message this person in this chat; with notify=notable you stay quiet when there's nothing worth saying.",
			Params: map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"add", "list", "pause", "resume", "remove"}},
				"name":   str("For add: short name"),
				"cron":   str("For add: 5-field cron in local time, e.g. '30 7 * * 1-5'"),
				"task":   str("For add: what to do each run, self-contained"),
				"notify": map[string]any{"type": "string", "enum": []string{"notable", "always"}},
				"id":     num("For pause/resume/remove"),
			},
			Required: []string{"action"}, Run: r.routine},
		{Name: "set_interests", Set: "heartbeat",
			Desc:     "Replace the list of what this person is into or tracking right now (12 lines max, one per line: topic, why it matters, what's next). Shown to you in every conversation.",
			Params:   map[string]any{"text": str("The whole list")},
			Required: []string{"text"}, Run: func(_ context.Context, c *Ctx, a Args) Result {
				if r.SetInterests == nil {
					return Result{Text: "unavailable", IsError: true}
				}
				if err := r.SetInterests(c.Person.ID, a.S("text")); err != nil {
					return Result{Text: err.Error(), IsError: true}
				}
				return Result{Text: "interests saved"}
			}},
		{Name: "set_open_loops", Set: "heartbeat",
			Desc:     "Replace the list of open loops: things this person is waiting on, things you promised to check or do, and unresolved questions (8 lines max, each with a date or trigger). Shown to you in every conversation. Drop what's done.",
			Params:   map[string]any{"text": str("The whole list, or empty to clear")},
			Required: []string{"text"}, Run: func(_ context.Context, c *Ctx, a Args) Result {
				if r.SetLoops == nil {
					return Result{Text: "unavailable", IsError: true}
				}
				r.SetLoops(c.Person.ID, a.S("text"))
				return Result{Text: "open loops saved"}
			}},
		{Name: "recall", Where: "both",
			Desc: "Search saved facts and past conversations with this person beyond what's in context.",
			Params: map[string]any{"query": str("What you're looking for, in plain words"),
				"exact": map[string]any{"type": "boolean", "description": "true to match exact names, ids or commands by keyword instead"}},
			Required: []string{"query"}, Run: r.recall},
		{Name: "shell", Where: "both", Admin: true, SideEffect: true,
			Desc:     "Run a quick bash command on the server. For checks and lookups; real changes go to start_job. 60s limit, output truncated.",
			Params:   map[string]any{"command": str("bash command")},
			Required: []string{"command"}, Run: r.shell},
		{Name: "start_job", Where: "both", Admin: true, SideEffect: true,
			Desc: "Hand real work to a full Claude Code session in the background: building, fixing, deploying, editing files, anything that takes more than a couple of commands. The person gets a message when it finishes.",
			Params: map[string]any{
				"task": str("Complete instructions; the job does not see this conversation"),
				"dir":  str("Working directory (default: home)"),
			},
			Required: []string{"task"}, Run: r.startJob},
		{Name: "jobs", Where: "both", Admin: true,
			Desc:   "Status of background jobs: omit id for the recent list, give an id for detail.",
			Params: map[string]any{"id": num("Job id")}, Run: r.jobs},
		{Name: "cancel_job", Where: "both", Admin: true, SideEffect: true,
			Desc: "Stop a running background job.", Params: map[string]any{"id": num("Job id")},
			Required: []string{"id"}, Run: r.cancelJob},
		// Local-model stand-ins for what Claude Code has built in.
		{Name: "web_search", Where: "local",
			Desc: "Search the web.", Params: map[string]any{"query": str("Query")},
			Required: []string{"query"}, Run: r.webSearch},
		{Name: "fetch", Where: "local",
			Desc: "Read a web page as text.", Params: map[string]any{"url": str("URL")},
			Required: []string{"url"}, Run: r.fetch},
	}
	if notes != nil {
		r.tools = append(r.tools,
			&Tool{Name: "notes_search", Where: "both", Admin: true,
				Desc:     "Search the long-term notes (projects, setup, decisions). Use before answering about them.",
				Params:   map[string]any{"query": str("What you're looking for"), "exact": map[string]any{"type": "boolean", "description": "true for exact names, ids or commands"}},
				Required: []string{"query"}, Run: r.notesSearch},
			&Tool{Name: "notes_read", Where: "both", Admin: true,
				Desc:     "Read one note by title or permalink from notes_search.",
				Params:   map[string]any{"note": str("Title or permalink")},
				Required: []string{"note"}, Run: r.notesRead},
			&Tool{Name: "notes_append", Where: "both", Admin: true, SideEffect: true,
				Desc:     "Append a dated line to an existing note when the person tells you something changed or was decided. Use the exact permalink from notes_search. New notes or rewrites go to start_job.",
				Params:   map[string]any{"note": str("Exact permalink"), "text": str("What changed, one or two sentences")},
				Required: []string{"note", "text"}, Run: r.notesAppend})
	}
	r.tools = append(r.tools, r.KitTools()...)
	r.applyMeta()
	return r
}

// Add registers more tools. One with the name and Set of an existing tool
// replaces it (an agent's set may reuse a chat tool's name).
func (r *Registry) Add(ts ...*Tool) {
	for _, t := range ts {
		replaced := false
		for i, old := range r.tools {
			if old.Name == t.Name && old.Set == t.Set {
				r.tools[i], replaced = t, true
				break
			}
		}
		if !replaced {
			r.tools = append(r.tools, t)
		}
	}
	r.applyMeta()
}

// Tool returns a registered tool by name, or nil.
func (r *Registry) Tool(name string) *Tool {
	for _, t := range r.tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// For lists the chat tools for a person on an engine.
func (r *Registry) For(p *config.Person, engine string) []*Tool {
	var out []*Tool
	for _, t := range r.tools {
		if t.Set != "" || (t.Admin && !p.Admin) {
			continue
		}
		if t.Where != "both" && t.Where != engine {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Toolset lists what a tool context may use: the chat tools, plus the
// heartbeat's own, or only send_file for background jobs (they bring Claude
// Code's tools and their kit's MCP server).
func (r *Registry) Toolset(c *Ctx) []*Tool {
	var all []*Tool
	switch c.Set {
	case "":
		all = r.For(c.Person, c.Engine)
	case "heartbeat":
		all = r.For(c.Person, c.Engine)
		for _, t := range r.tools {
			if t.Set == "heartbeat" {
				all = append(all, t)
			}
		}
	case "job": // send_file, plus what the job's kit needs: less to misuse, less to inject into
		kit := c.Kit
		if kit == "" {
			kit = "general"
		}
		for _, t := range r.tools {
			if t.Name == "send_file" || slices.Contains(t.Kits, kit) {
				all = append(all, t)
			}
		}
	default: // an agent's own set (e.g. a browsing sub-agent)
		for _, t := range r.tools {
			if t.Set == c.Set {
				all = append(all, t)
			}
		}
	}
	if len(c.Deny) == 0 {
		return all
	}
	var out []*Tool
	for _, t := range all {
		if !c.Deny[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

func (r *Registry) find(c *Ctx, name string) *Tool {
	for _, t := range r.Toolset(c) {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// Acts reports whether a tool changes something (starts work, saves, sends),
// for the claim guard. shell counts as looking, not acting.
func (r *Registry) Acts(name string) bool {
	if name == "shell" {
		return false
	}
	for _, t := range r.tools {
		if t.Name == name {
			return t.SideEffect
		}
	}
	return false
}

// Call runs a tool and records it in the turn's trace.
func (r *Registry) Call(ctx context.Context, c *Ctx, name string, args Args) Result {
	if name == "toolbox" {
		return r.toolbox(ctx, c, args)
	}
	t := r.find(c, name)
	if t == nil {
		return Result{Text: "unknown tool " + name, IsError: true}
	}
	if by := c.TaintedBy(); by != "" && t.Gated {
		return Result{IsError: true, Text: fmt.Sprintf("Blocked by the injection guard: this turn already read untrusted web content (%s), so %s can't run now. Tell the person exactly what you want to do and why; once they confirm in their next message, do it then.", by, name)}
	}
	// Bad arguments are refused before anything runs, replays included, so a
	// malformed reminder never comes back as "reminder set".
	if bad := badArgs(t, c, args); bad != "" {
		return Result{IsError: true, Text: bad + ". Usage: " + usage(t)}
	}
	// Replays skip side effects but answer like the real tool (stub.go). Shadow
	// runs (replay on the local engine) let shell through: it runs read-only
	// commands and refuses the rest exactly as live local turns do.
	if c.Replay && t.SideEffect && !replayRuns(t, args) && !(name == "shell" && c.Engine == "local") {
		c.markUsed(name)
		if t.Stub != nil {
			return Result{Text: t.Stub(c, args)}
		}
		return Result{Text: c.replayStub(name, args)}
	}
	c.markUsed(name)
	start := time.Now()
	timeout := t.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := func() (res Result) {
		defer func() {
			if p := recover(); p != nil {
				res = Result{Text: fmt.Sprint("tool crashed: ", p), IsError: true}
			}
		}()
		return t.Run(ctx, c, args)
	}()
	// The engines trace calls and results; this just logs timing.
	slog.Info("tool", "turn", c.TurnID, "tool", name, "ms", time.Since(start).Milliseconds(), "err", res.IsError)
	if (untrusted[name] || t.Untrusted) && !res.IsError {
		c.Taint(name)
		res.Text = Fence(name, res.Text)
	}
	return res
}

// ---- implementations ----

func (r *Registry) sendFile(_ context.Context, c *Ctx, a Args) Result {
	p := filepath.Clean(a.S("path"))
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return Result{Text: "no such file: " + p, IsError: true}
	}
	if st.Size() > 50<<20 {
		return Result{Text: "file is over 50 MB", IsError: true}
	}
	if c.Attach != nil {
		c.Attach(p)
	}
	return Result{Text: "Will send " + filepath.Base(p) + " with your reply."}
}

func (r *Registry) remember(_ context.Context, c *Ctx, a Args) Result {
	id, err := r.Store.AddFact(c.Person.ID, a.S("fact"), false, fmt.Sprintf("turn %d", c.TurnID))
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	return Result{Text: fmt.Sprintf("saved (fact %d)", id)}
}

func (r *Registry) forget(_ context.Context, c *Ctx, a Args) Result {
	if r.Store.DeleteFact(c.Person.ID, a.I("id")) {
		return Result{Text: "deleted"}
	}
	return Result{Text: "no such fact", IsError: true}
}

func (r *Registry) recall(_ context.Context, c *Ctx, a Args) Result {
	q := a.S("query")
	var b strings.Builder
	facts, _ := r.Store.SearchFacts(c.Person.ID, q, 8)
	for _, f := range facts {
		fmt.Fprintf(&b, "fact %d: %s\n", f.ID, f.Text)
	}
	turns, _ := r.Store.SearchTurns(c.Person.ID, q, 8)
	sort.Slice(turns, func(i, j int) bool { return turns[i].ID < turns[j].ID })
	for _, t := range turns {
		when := time.Unix(t.Started, 0).In(r.Cfg.Loc).Format("Jan 2 2006")
		if t.Input != "" {
			fmt.Fprintf(&b, "[%s] %s: %s\n", when, c.Person.Name, clip(t.Input, 300))
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", when, r.Cfg.Name, clip(t.Reply, 400))
	}
	if b.Len() == 0 {
		return Result{Text: "nothing found"}
	}
	return Result{Text: b.String()}
}

// reminderAt reads a reminder time: "2006-01-02 15:04", seconds tolerated.
func (r *Registry) reminderAt(a Args) (time.Time, error) {
	at, err := time.ParseInLocation("2006-01-02 15:04", a.S("at"), r.Cfg.Loc)
	if err != nil {
		at, err = time.ParseInLocation("2006-01-02 15:04:05", a.S("at"), r.Cfg.Loc)
	}
	return at, err
}

func (r *Registry) reminderCheck(c *Ctx, a Args) string {
	switch a.S("action") {
	case "add":
		at, err := r.reminderAt(a)
		if err != nil {
			return "at must be YYYY-MM-DD HH:MM, 24h local time"
		}
		// A replay runs days after the conversation it repeats; its "tonight" isn't past.
		if !c.Replay && at.Before(time.Now().Add(-time.Minute)) {
			return "that time is in the past"
		}
		if a.S("text") == "" {
			return "text is required: the reminder message itself"
		}
	case "cancel":
		if a.I("id") == 0 {
			return "id is required to cancel"
		}
	}
	return ""
}

func (r *Registry) reminder(_ context.Context, c *Ctx, a Args) Result {
	switch a.S("action") {
	case "add":
		at, _ := r.reminderAt(a) // reminderCheck passed
		id, err := r.Store.AddReminder(&store.Reminder{Person: c.Person.ID, Channel: c.Channel, Chat: c.Chat, At: at.Unix(), Text: a.S("text")})
		if err != nil {
			return Result{Text: err.Error(), IsError: true}
		}
		return Result{Text: fmt.Sprintf("reminder #%d set for %s", id, at.Format("Mon Jan 2 3:04 PM"))}
	case "cancel":
		if r.Store.CancelReminder(c.Person.ID, a.I("id")) {
			return Result{Text: "cancelled"}
		}
		return Result{Text: "no pending reminder with that id", IsError: true}
	}
	var b strings.Builder
	for _, x := range r.Store.PendingReminders(c.Person.ID, 20) {
		fmt.Fprintf(&b, "#%d %s: %s\n", x.ID, time.Unix(x.At, 0).In(r.Cfg.Loc).Format("Mon Jan 2 3:04 PM"), x.Text)
	}
	if b.Len() == 0 {
		return Result{Text: "no pending reminders"}
	}
	return Result{Text: b.String()}
}

func (r *Registry) notesSearch(ctx context.Context, _ *Ctx, a Args) Result {
	mode := r.Cfg.NotesSearch
	if mode == "" {
		mode = "vector"
	}
	if a.B("exact") {
		mode = "text" // exact names, ids and commands match better by keyword
	}
	out, err := r.Notes.Call(ctx, "search_notes", map[string]any{"query": a.S("query"), "page_size": 6, "search_type": mode, "output_format": "json"})
	if err != nil {
		return Result{Text: "notes unavailable: " + err.Error(), IsError: true}
	}
	return Result{Text: compactNoteSearch(out)}
}

func (r *Registry) notesRead(ctx context.Context, _ *Ctx, a Args) Result {
	out, err := r.Notes.Call(ctx, "read_note", map[string]any{"identifier": a.S("note")})
	if err != nil {
		return Result{Text: "notes unavailable: " + err.Error(), IsError: true}
	}
	return Result{Text: clip(unwrapResult(out), 8000)}
}

// shell replaces Claude Code's built-in Bash, whose description alone is
// ~2.5k tokens on every call.
func (r *Registry) shell(ctx context.Context, c *Ctx, a Args) Result {
	if c.Engine == "local" && !ReadOnlyCommand(a.S("command")) {
		return Result{IsError: true, Text: "On the local model shell only runs read-only checks (status, logs, listings, disk/GPU/memory, curl GETs; pipes and ; && || are fine, redirection isn't). For a change or anything bigger, hand it to start_job."}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", a.S("command"))
	cmd.Dir = os.Getenv("HOME")
	proc.Group(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	s := string(out)
	if len(s) > 8000 {
		s = s[:3000] + fmt.Sprintf("\n…[%d bytes cut]…\n", len(s)-6000) + s[len(s)-3000:]
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{Text: s + "\n(timed out after 60s; use start_job for long work)", IsError: true}
		}
		return Result{Text: s + "\n(" + err.Error() + ")", IsError: true}
	}
	if strings.TrimSpace(s) == "" {
		s = "(no output)"
	}
	return Result{Text: s}
}

// notesAppend adds one dated line to an existing note. It refuses unknown
// notes so chat can't scatter stray files around the notes.
func (r *Registry) notesAppend(ctx context.Context, _ *Ctx, a Args) Result {
	note := strings.TrimPrefix(a.S("note"), "memory://")
	raw, err := r.Notes.Call(ctx, "read_note", map[string]any{"identifier": note, "output_format": "json"})
	if err != nil {
		return Result{Text: "notes unavailable: " + err.Error(), IsError: true}
	}
	var found struct {
		Permalink string `json:"permalink"`
	}
	json.Unmarshal([]byte(unwrapResult(raw)), &found)
	if found.Permalink == "" {
		return Result{Text: "no note with that exact permalink; find it with notes_search (new notes go to start_job)", IsError: true}
	}
	line := fmt.Sprintf("\n- (%s, via %s) %s\n", time.Now().In(r.Cfg.Loc).Format("2006-01-02"), r.Cfg.Name, a.S("text"))
	if _, err := r.Notes.Call(ctx, "edit_note", map[string]any{"identifier": found.Permalink, "operation": "append", "content": line}); err != nil {
		return Result{Text: "notes edit failed: " + err.Error(), IsError: true}
	}
	return Result{Text: "added to " + found.Permalink}
}

func (r *Registry) startJob(_ context.Context, c *Ctx, a Args) Result {
	dir := a.S("dir")
	if dir == "" {
		dir = os.Getenv("HOME")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Result{Text: "no such directory: " + dir, IsError: true}
	}
	id, err := r.Jobs.Start(c.Person.ID, c.Channel, c.Chat, a.S("task"), dir)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	return Result{Text: fmt.Sprintf("job #%d started in %s; its report goes to %s when it finishes (don't promise progress updates)", id, dir, c.Person.Name)}
}

func (r *Registry) routine(_ context.Context, c *Ctx, a Args) Result {
	if r.Routines == nil {
		return Result{Text: "routines unavailable", IsError: true}
	}
	var out string
	var err error
	switch act := a.S("action"); act {
	case "add":
		out, err = r.Routines.AddRoutine(c, a.S("name"), a.S("cron"), a.S("task"), a.S("notify"))
	case "list":
		out = r.Routines.ListRoutines(c.Person.ID)
	default:
		out, err = r.Routines.ChangeRoutine(c.Person.ID, a.I("id"), act)
	}
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	return Result{Text: out}
}

func (r *Registry) jobs(_ context.Context, c *Ctx, a Args) Result {
	return Result{Text: r.Jobs.Describe(c.Person.ID, a.I("id"))}
}

func (r *Registry) cancelJob(_ context.Context, c *Ctx, a Args) Result {
	if err := r.Jobs.Cancel(c.Person.ID, a.I("id")); err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	return Result{Text: "cancelled"}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
