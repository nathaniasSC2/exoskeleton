// Package agent is the conversation loop: inbound message → one turn on the
// current brain → reply on the same channel. Context is rebuilt from the
// database every turn with hard size caps, so nothing grows without bound.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/channel"
	"github.com/nathaniasSC2/exoskeleton/internal/config"
	"github.com/nathaniasSC2/exoskeleton/internal/engine"
	"github.com/nathaniasSC2/exoskeleton/internal/jobs"
	"github.com/nathaniasSC2/exoskeleton/internal/prompts"
	"github.com/nathaniasSC2/exoskeleton/internal/sched"
	"github.com/nathaniasSC2/exoskeleton/internal/store"
	"github.com/nathaniasSC2/exoskeleton/internal/tools"
)

// Context budget. These caps are the whole point of exo: a gateway that keeps
// one rolling session resends its whole history (~128k tokens) on every call.
const (
	recentTurns   = 12
	recentChars   = 6000
	inputClip     = 500
	replyClip     = 700
	factHits      = 6
	keepAlwaysN   = 4              // newest turns kept regardless of age
	recentMaxAge  = 72 * time.Hour // older turns drop out unless in keepAlwaysN
	debounce      = 1500 * time.Millisecond
	pendingMaxAge = 15 * time.Minute
)

type Agent struct {
	Cfg    *config.Config
	Store  *store.Store
	Reg    *tools.Registry
	Claude *engine.Claude
	Local  *engine.Local
	Jobs   *jobs.Runner

	Senders map[string]channel.Sender
	Discord *channel.Discord
	Sched   *sched.Scheduler

	mu     sync.Mutex
	wake   map[string]chan struct{}
	active sync.Map // turn ID -> *tools.Ctx
	turns  sync.Map // person -> *runningTurn: the chat turn "stop" cancels
	fo     failover
	sh     shadowState
}

func (a *Agent) Init() error {
	a.wake = map[string]chan struct{}{}
	for _, p := range a.Cfg.People {
		a.wake[p.ID] = make(chan struct{}, 1)
	}
	if a.Store.Get("mode") == "" {
		a.Store.Set("mode", "claude")
	}
	a.sh.live = make(chan shadowJob, 32)
	a.sh.hist = make(chan shadowJob, 1024)
	return a.writeDefaultPrompts()
}

// Run starts one worker per person and the failover probe. Blocks until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range a.Cfg.People {
		wg.Add(1)
		go func(p *config.Person) { defer wg.Done(); a.worker(ctx, p) }(p)
		a.poke(p.ID) // pick up anything that arrived just before a restart
	}
	wg.Add(3)
	go func() { defer wg.Done(); a.shadowWorker(ctx) }()
	go func() { defer wg.Done(); a.probeLoop(ctx) }()
	go func() { defer wg.Done(); a.reminderLoop(ctx) }()
	wg.Wait()
}

func (a *Agent) poke(person string) {
	select {
	case a.wake[person] <- struct{}{}:
	default:
	}
}

// ToolCtx is how the local engine finds the turn a tool call belongs to.
func (a *Agent) ToolCtx(turn int64) *tools.Ctx { return a.Lookup(fmt.Sprintf("turn:%d", turn)) }

// Lookup resolves an MCP path key ("turn:12", "job:3") to the live tool
// context it belongs to; finished turns and jobs resolve to nil.
func (a *Agent) Lookup(key string) *tools.Ctx {
	if v, ok := a.active.Load(key); ok {
		return v.(*tools.Ctx)
	}
	return nil
}

// Register makes a tool context reachable over MCP until the returned func runs.
func (a *Agent) Register(key string, c *tools.Ctx) func() {
	c.Key = key
	a.active.Store(key, c)
	return func() { a.active.Delete(key) }
}

// ---- inbound ----

func (a *Agent) Handle(in channel.Inbound) {
	var p *config.Person
	switch in.Channel {
	case "discord":
		p = a.Cfg.ByDiscord(in.Sender)
	case "console":
		p = a.Cfg.Person(in.Sender)
	}
	if p == nil {
		slog.Info("ignored message from unknown sender", "channel", in.Channel) // not stored: not ours to keep
		return
	}
	if in.ReactTo != "" {
		if t := a.Store.TurnByOutGUID(in.ReactTo); t > 0 {
			a.Store.SetFeedback(t, in.Reaction, "")
			slog.Info("feedback", "turn", t, "score", in.Reaction)
		}
		return
	}
	imgs, _ := json.Marshal(in.Images)
	ev := &store.Event{Channel: in.Channel, Chat: in.Chat, Sender: in.Sender, Person: p.ID,
		GUID: in.GUID, Text: in.Text, Images: string(imgs), TS: in.TS.Unix()}
	ok, err := a.Store.AddEvent(ev)
	if err != nil {
		slog.Error("store event", "err", err)
		return
	}
	if !ok {
		return // duplicate delivery
	}
	if r, ok := a.Senders[in.Channel].(channel.Reader); ok {
		go r.MarkRead(context.Background(), in.Chat)
	}
	if isStop(in.Text) && a.stopTurn(p.ID) {
		a.Store.MarkEvents([]int64{ev.ID}, "done", 0) // answered by the stopped turn
		return
	}
	a.poke(p.ID)
}

func (a *Agent) worker(ctx context.Context, p *config.Person) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake[p.ID]:
		}
		// Let a burst of texts land so they're answered together, once.
		select {
		case <-ctx.Done():
			return
		case <-time.After(debounce):
		}
		evs, err := a.Store.PendingEvents(p.ID, pendingMaxAge)
		if err != nil || len(evs) == 0 {
			continue
		}
		a.process(ctx, p, evs)
		a.poke(p.ID) // anything that arrived during the turn
	}
}

func (a *Agent) process(ctx context.Context, p *config.Person, evs []*store.Event) {
	last := evs[len(evs)-1]
	var ids []int64
	var texts []string
	var images []string
	for _, e := range evs {
		ids = append(ids, e.ID)
		if e.Text != "" {
			texts = append(texts, e.Text)
		}
		var im []string
		json.Unmarshal([]byte(e.Images), &im)
		images = append(images, im...)
	}
	input := strings.Join(texts, "\n")
	if a.command(ctx, p, last, ids, input) {
		return
	}
	if a.mode() == "held" {
		a.Store.MarkEvents(ids, "held", 0) // Claude is down and the admin hasn't decided yet
		return
	}
	a.turn(ctx, p, last.Channel, last.Chat, input, images, ids)
}

// ---- the turn ----

type turnOut struct {
	reply string
	files []string
	err   *engine.Result
	req   *engine.Request // what the brain was given (shadow runs replay it on the local model)
}

func (a *Agent) turn(ctx context.Context, p *config.Person, ch, chat, input string, images []string, eventIDs []int64) {
	id := a.Store.StartTurn(p.ID, ch, chat, "chat", input)
	a.Store.MarkEvents(eventIDs, "done", id)
	tctx, untrack := a.trackTurn(ctx, p.ID)
	out := a.runTurn(tctx, p, id, ch, chat, input, images, false)
	if untrack() {
		a.deliver(ctx, p, ch, chat, id, stoppedReply, nil)
		return
	}
	if out.err != nil && out.err.Err != nil {
		if a.onFailure(ctx, p, out.err, eventIDs) {
			return // failover owns the reply now
		}
		out.reply = "Something broke on my end, sorry. (" + clipErr(out.err.Err) + ") Try again in a sec?"
	} else if a.mode() == "claude" {
		a.onSuccess()
		a.shadowLive(p, ch, chat, id, out)
	}
	a.deliver(ctx, p, ch, chat, id, out.reply, out.files)
}

// runTurn does one turn on the current brain and records it. It does not deliver.
// turnOpts vary a turn for the non-chat callers.
type turnOpts struct {
	replay   bool            // historical message: no side effects, no history
	eval     bool            // fixed eval prompt: side effects stubbed, no history, but full live context (note hints, facts)
	set      string          // toolset ("" chat, "heartbeat")
	deny     map[string]bool // tools withheld
	noRecent bool            // skip the recent conversation (routines; the heartbeat brings its own digest)
	system   string          // extra system-prompt section (routine/heartbeat brief)
	model    string          // engine overrides for this turn
	effort   string
}

func (a *Agent) runTurn(ctx context.Context, p *config.Person, id int64, ch, chat, input string, images []string, replay bool) turnOut {
	return a.runTurnOpts(ctx, p, id, ch, chat, input, images, turnOpts{replay: replay})
}

func (a *Agent) runTurnOpts(ctx context.Context, p *config.Person, id int64, ch, chat, input string, images []string, o turnOpts) turnOut {
	replay := o.replay || o.eval
	eng := engine.Engine(a.Claude)
	local := a.mode() == "local"
	if local {
		eng = a.Local
	}
	var mu sync.Mutex
	var files []string
	tc := &tools.Ctx{Person: p, Channel: ch, Chat: chat, TurnID: id, Engine: eng.Name(), Replay: replay, Set: o.set, Deny: o.deny,
		Attach: func(path string) { mu.Lock(); files = append(files, path); mu.Unlock() }}
	defer a.Register(fmt.Sprintf("turn:%d", id), tc)()

	system, hash := a.systemPrompt(p)
	if o.system != "" {
		system += "\n\n" + o.system
	}
	user := a.contextBlock(p, ch, input, eng.Name(), id, o.replay || o.noRecent)
	blocks := []engine.Block{{Text: user}}
	for _, im := range images {
		blocks = append(blocks, engine.Block{Image: im})
	}
	seq := 0
	var history []engine.Msg
	if !replay && !o.noRecent {
		history = a.history(p, id)
	}
	req := &engine.Request{TurnID: id, System: system, History: history, User: blocks, BuiltIns: a.builtIns(local), Model: o.model, Effort: a.effortFor(eng, o),
		Trace: func(kind, name, body string) {
			if kind == "tool_call" && (name == "WebFetch" || name == "WebSearch") {
				tc.Taint(name) // Claude Code's built-in web tools read untrusted content too
			}
			seq++
			a.Store.Trace(id, seq, kind, name, body)
		}}
	req.Trace("prompt", hash[:8], engine.HistoryText(history)+user)

	stopTyping := a.typing(ctx, ch, chat, replay)
	start := time.Now()
	stopCheckIn := func() {}
	if !replay && o.set == "" && o.system == "" { // chat turns, not routines
		stopCheckIn = a.checkIn(p, ch, chat, tc, start)
	}
	timeout := a.Cfg.TurnTimeout
	if eng.Name() == "local" {
		timeout = a.Cfg.LocalTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	res := eng.Turn(tctx, req)
	if res.Err != nil && (res.Kind == engine.Overloaded || res.Kind == engine.Other) && eng.Name() == "claude" && ctx.Err() == nil {
		req.Trace("retry", string(res.Kind), res.Err.Error())
		time.Sleep(3 * time.Second)
		res = eng.Turn(tctx, req)
	}
	cancel()
	stopTyping()
	stopCheckIn()

	t := &store.Turn{ID: id, Engine: eng.Name(), Status: "ok", Reply: strings.TrimSpace(res.Text),
		Tools: strings.Join(toolsUsed(res.Tools, tc.Used()), ", "), LatencyMS: time.Since(start).Milliseconds(),
		InTok: res.Usage.In, OutTok: res.Usage.Out, CacheRead: res.Usage.CacheRead, CacheWrite: res.Usage.CacheWrite,
		Cost: res.Usage.Cost, PromptHash: hash[:12], Attachments: strings.Join(files, "\n")}
	if res.Err != nil {
		t.Status, t.Error = string(res.Kind), res.Err.Error()
		if t.Status == "" {
			t.Status = "error"
		}
	} else if t.Reply == "" && len(files) == 0 {
		t.Status, t.Error = "error", "empty reply"
		res.Err, res.Kind = fmt.Errorf("empty reply"), engine.Other
	}
	if replay {
		t.Status = "replay"
	}
	a.Store.FinishTurn(t)
	out := turnOut{reply: t.Reply, files: files, req: req}
	if res.Err != nil {
		out.err = res
	}
	return out
}

// effortFor is the per-turn effort override (Claude only; the local engine
// uses its configured variant).
func (a *Agent) effortFor(eng engine.Engine, o turnOpts) string {
	if eng.Name() == "local" {
		return ""
	}
	return o.effort
}

func (a *Agent) builtIns(local bool) string {
	if local {
		return ""
	}
	// Claude Code's own Bash is replaced by exo's leaner shell tool: each
	// built-in's schema is paid on every call. Agent hands quality-over-speed
	// work to the "deep" sub-agent (Config.Agents).
	if a.Cfg.DeepModel == "" {
		return "WebSearch,WebFetch"
	}
	return "WebSearch,WebFetch,Agent"
}

func (a *Agent) typing(ctx context.Context, ch, chat string, replay bool) func() {
	s := a.Senders[ch]
	if s == nil || replay {
		return func() {}
	}
	tctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(8 * time.Second) // Discord's indicator lasts ~10s
		defer t.Stop()
		s.Typing(tctx, chat, true)
		for {
			select {
			case <-tctx.Done():
				s.Typing(context.Background(), chat, false)
				return
			case <-t.C:
				if ch == "discord" {
					s.Typing(tctx, chat, true)
				}
			}
		}
	}()
	return cancel
}

func (a *Agent) deliver(ctx context.Context, p *config.Person, ch, chat string, turn int64, text string, files []string) {
	s := a.Senders[ch]
	if s == nil {
		slog.Warn("no sender for channel", "channel", ch)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if strings.TrimSpace(text) != "" {
		guid, err := s.SendText(dctx, chat, text)
		if err != nil {
			slog.Error("send", "channel", ch, "err", err)
			a.Store.Trace(turn, 900, "error", "send", err.Error())
		}
		a.Store.AddOutGUID(turn, guid)
	}
	for _, f := range files {
		guid, err := s.SendFile(dctx, chat, f)
		if err != nil {
			slog.Error("send file", "channel", ch, "err", err)
			a.Store.Trace(turn, 901, "error", "send_file", err.Error())
			// Say so instead of dropping it (Discord refuses bot uploads over its size limit with a 413).
			size := ""
			if st, serr := os.Stat(f); serr == nil {
				size = fmt.Sprintf(", %.1f MB", float64(st.Size())/(1<<20))
			}
			why := "it didn't go through"
			if strings.Contains(err.Error(), "413") || strings.Contains(err.Error(), "too large") {
				why = "it's over the upload limit here"
			}
			s.SendText(dctx, chat, fmt.Sprintf("(Couldn't attach %s%s: %s. It's on the server at %s.)", filepath.Base(f), size, why, f))
			continue
		}
		a.Store.AddOutGUID(turn, guid)
	}
}

// Notify sends something the assistant says unprompted (job reports, scheduled
// messages, failover notices) and records it so it's in context next turn.
func (a *Agent) Notify(person, ch, chat, text string) {
	a.notify(person, ch, chat, "notice", text, nil)
}

// NotifyFiles is Notify with attachments (a job's renders, files it made).
func (a *Agent) NotifyFiles(person, ch, chat, text string, files []string) {
	a.notify(person, ch, chat, "notice", text, files)
}

// notify with kind "system" keeps harness chatter (/status, /help) out of the model's context.
func (a *Agent) notify(person, ch, chat, kind, text string, files []string) {
	p := a.Cfg.Person(person)
	if p == nil {
		return
	}
	id := a.Store.AddNotice(person, ch, chat, kind, text)
	if len(files) > 0 {
		a.Store.DB.Exec(`UPDATE turns SET attachments=? WHERE id=?`, strings.Join(files, "\n"), id)
	}
	if ch == "console" {
		return
	}
	a.deliver(context.Background(), p, ch, chat, id, text, files)
}

// DeliverTarget resolves "discord:<person>" for scheduled jobs.
func (a *Agent) DeliverTarget(target, text string) error {
	p, ch, chat, err := a.resolveTarget(target)
	if err != nil {
		return err
	}
	a.Notify(p.ID, ch, chat, text)
	return nil
}

func (a *Agent) resolveTarget(target string) (*config.Person, string, string, error) {
	ch, who, _ := strings.Cut(target, ":")
	p := a.Cfg.Person(who)
	if p == nil {
		return nil, "", "", fmt.Errorf("unknown person in %q", target)
	}
	chat := ""
	switch ch {
	case "discord":
		if a.Discord == nil || p.Discord == "" {
			return nil, "", "", fmt.Errorf("discord not available")
		}
		c, err := a.Discord.DM(p.Discord)
		if err != nil {
			return nil, "", "", err
		}
		chat = c
	}
	if chat == "" {
		return nil, "", "", fmt.Errorf("no chat for %q", target)
	}
	return p, ch, chat, nil
}

// reminderLoop delivers due reminders where they were set.
func (a *Agent) reminderLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, r := range a.Store.DueReminders() {
			a.Notify(r.Person, r.Channel, r.Chat, "⏰ "+r.Text)
		}
	}
}

// ---- prompt assembly ----

var channelNames = map[string]string{"discord": "Discord DM", "console": "dashboard test console"}

// localBrainNote tells the local model what it can't do (shadow runs add it too).
const localBrainNote = "Brain: the local model (Claude is unavailable). Background jobs run on the local model too (slower; give them small, concrete tasks); shell is read-only: say what you'd run for anything that changes the server, or hand it to a job. Before saying anything is running, done, sent, scheduled or being worked on, check it with a tool in this turn (jobs, shell, reminder); if you can't do something, say so plainly. Use the toolbox for live facts (the server, jobs, notes, older conversations) instead of memory, and web_search anything you'd otherwise be guessing; search results are only snippets, so fetch the best page before stating specifics. When asked for something done, do it with a tool (usually start_job) rather than describing a plan or asking whether you should. Never tell the person you can't do something you haven't tried with a tool this turn. Never narrate checking or fixing your own answer: they only see the final message.\n"

func (a *Agent) contextBlock(p *config.Person, ch, input, eng string, turnID int64, replay bool) string {
	now := time.Now().In(a.Cfg.Loc)
	var b strings.Builder
	b.WriteString("<context>\n")
	fmt.Fprintf(&b, "Now: %s\n", now.Format("Monday, January 2, 2006, 3:04 PM MST"))
	fmt.Fprintf(&b, "From: %s via %s\n", p.Name, channelName(ch))
	if eng == "local" {
		b.WriteString(localBrainNote)
		if !replay {
			b.WriteString(a.checkedNote(p, turnID))
		}
	}
	if !replay && len(keywords(input)) > 0 {
		// Older exchanges about the same things, beyond the recent window.
		if hits, err := a.Store.SearchTurns(p.ID, strings.Join(keywords(input), " "), 8); err == nil {
			var lines []string
			for _, t := range hits {
				if t.ID == turnID || time.Since(time.Unix(t.Started, 0)) < recentMaxAge || t.Kind == "replay" {
					continue
				}
				when := time.Unix(t.Started, 0).In(a.Cfg.Loc).Format("Mon Jan 2")
				if t.Input != "" {
					lines = append(lines, fmt.Sprintf("[%s] %s: %s → %s: %s", when, p.Name, oneLine(t.Input, 160), a.Cfg.Name, oneLine(t.Reply, 220)))
				} else {
					lines = append(lines, fmt.Sprintf("[%s] %s: %s", when, a.Cfg.Name, oneLine(t.Reply, 260)))
				}
				if len(lines) == 3 {
					break
				}
			}
			if len(lines) > 0 {
				b.WriteString("Earlier conversations that may be related:\n" + strings.Join(lines, "\n") + "\n")
			}
		}
	}
	if facts, _ := a.Store.SearchFacts(p.ID, input, factHits); len(facts) > 0 {
		b.WriteString("Saved facts that may be relevant:\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "- (%d) %s\n", f.ID, f.Text)
		}
	}
	if p.Admin && !replay {
		if hints := a.noteHints(input); hints != "" {
			b.WriteString("Notes matching what they mentioned (read with notes_read before relying on details):\n" + hints)
		}
	}
	if rs := a.Store.PendingReminders(p.ID, 5); len(rs) > 0 {
		var xs []string
		for _, r := range rs {
			xs = append(xs, fmt.Sprintf("#%d %s: %s", r.ID, time.Unix(r.At, 0).In(a.Cfg.Loc).Format("Mon Jan 2 3:04PM"), oneLine(r.Text, 80)))
		}
		b.WriteString("Upcoming reminders: " + strings.Join(xs, "; ") + "\n")
	}
	if p.Admin {
		rows, err := a.Store.DB.Query(`SELECT id,task,COALESCE(started,created) FROM jobs WHERE person=? AND status IN ('queued','running') ORDER BY id LIMIT 5`, p.ID)
		if err == nil {
			var js []string
			for rows.Next() {
				var id, st int64
				var task string
				rows.Scan(&id, &task, &st)
				js = append(js, fmt.Sprintf("#%d %q (%s)", id, oneLine(task, 80), time.Since(time.Unix(st, 0)).Round(time.Minute)))
			}
			rows.Close()
			if len(js) > 0 {
				b.WriteString("Background jobs running: " + strings.Join(js, "; ") + "\n")
			}
		}
	}
	b.WriteString("</context>\n\n")
	b.WriteString(input)
	return b.String()
}

func (a *Agent) systemPrompt(p *config.Person) (string, string) {
	var parts []string
	parts = append(parts, a.soul(p), a.prompt("rules.md"))
	if p.Admin {
		parts = append(parts, a.prompt("admin.md"))
	}
	if facts, _ := a.Store.Facts(p.ID, true); len(facts) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "# About %s\n", p.Name)
		for _, f := range facts {
			b.WriteString("- " + f.Text + "\n")
		}
		parts = append(parts, b.String())
	}
	if in := strings.TrimSpace(a.Store.Get("interests:" + p.ID)); in != "" {
		parts = append(parts, fmt.Sprintf("# What %s is into lately (kept current by your heartbeat)\n%s", p.Name, in))
	}
	// Open loops change at most hourly (the heartbeat keeps them), so they sit
	// here with the other slow context, where prompt caches can reuse them.
	if loops := strings.TrimSpace(a.Store.Get("loops:" + p.ID)); loops != "" {
		parts = append(parts, "# Open loops (pending, promised or unresolved; kept by your heartbeat)\n"+loops)
	}
	s := strings.Join(parts, "\n\n")
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	a.Store.SaveSystem(h[:12], s)
	return s, h
}

// prompt reads an editable prompt (the data dir's copy, else the built-in
// default) with {{name}} replaced by the assistant's name.
func (a *Agent) prompt(name string) string {
	b, err := os.ReadFile(a.Cfg.Path("prompts", name))
	if err != nil {
		b, _ = prompts.Defaults.ReadFile(name)
	}
	return strings.ReplaceAll(strings.TrimSpace(string(b)), "{{name}}", a.Cfg.Name)
}

// soul is the persona for a person: prompts/soul-<id>.md if there is one
// (a different voice or rules per person), else soul.md.
func (a *Agent) soul(p *config.Person) string {
	if b, err := os.ReadFile(a.Cfg.Path("prompts", "soul-"+p.ID+".md")); err == nil {
		return strings.ReplaceAll(strings.TrimSpace(string(b)), "{{name}}", a.Cfg.Name)
	}
	return a.prompt("soul.md")
}

func channelName(ch string) string {
	if n := channelNames[ch]; n != "" {
		return n
	}
	return ch
}

// JobBrief is appended to every background job's system prompt.
func (a *Agent) JobBrief() string { return a.prompt("job.md") }

// PromptNames lists the editable prompt files.
func PromptNames() []string {
	var out []string
	fs.WalkDir(prompts.Defaults, ".", func(p string, d fs.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".md") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func (a *Agent) ReadPrompt(name string) string { return a.prompt(name) }

func (a *Agent) SavePrompt(name, content string) error {
	if !validPrompt(name) {
		return fmt.Errorf("unknown prompt %q", name)
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if err := os.WriteFile(a.Cfg.Path("prompts", name), []byte(content), 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(content))
	a.Store.SavePromptVersion(name, content, hex.EncodeToString(sum[:])[:12])
	return nil
}

func validPrompt(name string) bool {
	for _, n := range PromptNames() {
		if n == name {
			return true
		}
	}
	return false
}

func (a *Agent) writeDefaultPrompts() error {
	dir := a.Cfg.Path("prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, n := range PromptNames() {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		b, _ := prompts.Defaults.ReadFile(n)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a.Store.SavePromptVersion(n, string(b), hex.EncodeToString(sum[:])[:12])
	}
	return nil
}

// ---- console + replay (dashboard) ----

// Console runs a message (and optional image) through the full pipeline
// without any real delivery.
func (a *Agent) Console(person, text string, images ...string) {
	a.Handle(channel.Inbound{Channel: "console", Chat: "console", Sender: person, Text: text, Images: images,
		GUID: fmt.Sprintf("console-%d", time.Now().UnixNano()), TS: time.Now()})
}

// Eval answers one fixed test prompt on the current brain as if the person had
// just sent it on Discord: full live context, no conversation history, side effects
// stubbed. The traced prompt is then replayed byte for byte on each local model
// (shadow history with ids), so every model sees exactly what Claude saw.
func (a *Agent) Eval(ctx context.Context, person, text string) (*store.Turn, error) {
	p := a.Cfg.Person(person)
	if p == nil {
		return nil, fmt.Errorf("unknown person")
	}
	id := a.Store.StartTurn(p.ID, "discord", "eval", "eval", text)
	a.runTurnOpts(ctx, p, id, "discord", "eval", text, nil, turnOpts{eval: true})
	return a.Store.Turn(id)
}

// Replay answers one historical message with no history and no side effects.
func (a *Agent) Replay(ctx context.Context, person, input string) (*store.Turn, error) {
	p := a.Cfg.Person(person)
	if p == nil {
		return nil, fmt.Errorf("unknown person")
	}
	id := a.Store.StartTurn(p.ID, "console", "replay", "replay", input)
	a.runTurn(ctx, p, id, "console", "replay", input, nil, true)
	return a.Store.Turn(id)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func total(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

func clipErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// SystemPromptFor exposes the assembled system prompt (for `exo budget`).
func (a *Agent) SystemPromptFor(p *config.Person) string { s, _ := a.systemPrompt(p); return s }

// toolsUsed lists what a turn really did: the engine's calls with the
// toolbox wrapper replaced by the tools it ran.
func toolsUsed(engineTools, inner []string) []string {
	var out []string
	for _, t := range engineTools {
		if t != "toolbox" {
			out = append(out, t)
		}
	}
	return append(out, inner...)
}
