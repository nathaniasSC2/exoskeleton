package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/engine"
	"github.com/nathaniasSC2/exoskeleton/sched"
	"github.com/nathaniasSC2/exoskeleton/store"
	"github.com/nathaniasSC2/exoskeleton/tools"
)

// Everything here is the assistant acting without being messaged: routines
// it was asked to set up, the heartbeat, and self-checks. All of it runs as
// normal turns (so it's traced on the dashboard) but never in the chat context
// except for what it actually decides to say.

const maxRoutines = 20

// ---- routines (tools.RoutineManager) ----

func (a *Agent) AddRoutine(c *tools.Ctx, name, spec, task, notify string) (string, error) {
	name, task = strings.TrimSpace(name), strings.TrimSpace(task)
	if name == "" || task == "" {
		return "", fmt.Errorf("name and task are required")
	}
	if err := sched.ValidCron(spec); err != nil {
		return "", fmt.Errorf("bad cron %q: %v", spec, err)
	}
	if notify != "always" {
		notify = "notable"
	}
	if len(a.Store.Routines(c.Person.ID, false)) >= maxRoutines {
		return "", fmt.Errorf("already %d routines; remove one first", maxRoutines)
	}
	id, err := a.Store.AddRoutine(&store.Routine{Person: c.Person.ID, Channel: c.Channel, Chat: c.Chat,
		Name: name, Cron: spec, Task: task, Notify: notify})
	if err != nil {
		return "", err
	}
	a.SyncRoutines()
	next := ""
	if t := a.Sched.Next(routineEntryName(id, name)); !t.IsZero() {
		next = ", next run " + t.In(a.Cfg.Loc).Format("Mon Jan 2 3:04 PM")
	}
	return fmt.Sprintf("routine #%d %q set (%s%s)", id, name, spec, next), nil
}

func (a *Agent) ListRoutines(person string) string {
	rs := a.Store.Routines(person, false)
	if len(rs) == 0 {
		return "no routines"
	}
	var b strings.Builder
	for _, r := range rs {
		state := "on"
		if !r.Enabled {
			state = "paused"
		}
		fmt.Fprintf(&b, "#%d %s [%s, %s, notify %s]: %s\n", r.ID, r.Name, r.Cron, state, r.Notify, oneLine(r.Task, 120))
	}
	return b.String()
}

func (a *Agent) ChangeRoutine(person string, id int64, action string) (string, error) {
	ok := false
	switch action {
	case "pause":
		ok = a.Store.SetRoutineEnabled(person, id, false)
	case "resume":
		ok = a.Store.SetRoutineEnabled(person, id, true)
	case "remove":
		ok = a.Store.DeleteRoutine(person, id)
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
	if !ok {
		return "", fmt.Errorf("no routine #%d", id)
	}
	a.SyncRoutines()
	return fmt.Sprintf("routine #%d %s", id, map[string]string{"pause": "paused", "resume": "resumed", "remove": "removed"}[action]), nil
}

func routineEntryName(id int64, name string) string { return fmt.Sprintf("routine #%d %s", id, name) }

// SyncRoutines pushes the enabled routines into the scheduler.
func (a *Agent) SyncRoutines() {
	if a.Sched == nil {
		return
	}
	var es []sched.Entry
	for _, r := range a.Store.Routines("", true) {
		es = append(es, sched.Entry{Name: routineEntryName(r.ID, r.Name), Cron: r.Cron, Cmd: fmt.Sprintf("routine:%d", r.ID)})
	}
	a.Sched.SetRoutines(es)
}

const routineBrief = `# Scheduled routine

Nobody messaged you: this is a recurring task you were asked to run. Do the task now with your tools, checking real sources. Then write the message the person gets, in your normal texting voice: short, specific, no preamble about it being a routine.

If notify is "notable" and there's nothing new or worth their attention, reply with exactly SILENT and nothing else.`

// RunRoutine runs one routine now (the scheduler's hook).
func (a *Agent) RunRoutine(ctx context.Context, id int64) (string, error) {
	r, err := a.Store.Routine(id)
	if err != nil {
		return "", fmt.Errorf("no routine #%d", id)
	}
	if !r.Enabled {
		return "paused", nil
	}
	p := a.Cfg.Person(r.Person)
	if p == nil {
		return "", fmt.Errorf("unknown person %q", r.Person)
	}
	if a.mode() == "held" {
		a.Store.RoutineRan(id, "skipped: Claude unavailable")
		return "skipped: Claude unavailable", nil
	}
	input := fmt.Sprintf("Routine #%d %q (runs %s, notify %s). Task:\n%s", r.ID, r.Name, r.Cron, r.Notify, r.Task)
	tid := a.Store.StartTurn(p.ID, r.Channel, r.Chat, "routine", input)
	out := a.runTurnOpts(ctx, p, tid, r.Channel, r.Chat, input, nil, turnOpts{noRecent: true, system: routineBrief,
		deny: map[string]bool{"routine": true, "cancel_job": true}})
	if out.err != nil && out.err.Err != nil {
		a.Store.RoutineRan(id, "failed: "+out.err.Err.Error())
		return "", out.err.Err
	}
	reply := strings.TrimSpace(out.reply)
	if strings.EqualFold(strings.Trim(reply, ". "), "SILENT") || reply == "" {
		a.Store.RoutineRan(id, "silent")
		return "silent", nil
	}
	a.NotifyFiles(p.ID, r.Channel, r.Chat, reply, out.files)
	a.Store.RoutineRan(id, reply)
	return reply, nil
}

// ---- heartbeat ----

// Heartbeat reviews what's happened since the last one: learns (facts, notes,
// the interests list), follows up on what the admin cares about, takes only
// safe actions, and messages them at most once, only when it's worth it.
func (a *Agent) Heartbeat(ctx context.Context) (string, error) {
	p := a.admin()
	if p == nil {
		return "", fmt.Errorf("no admin person")
	}
	if a.mode() == "held" {
		return "skipped: Claude unavailable", nil
	}
	// Governor: a subscription is usually shared with the person's own Claude
	// Code work, so an hourly heartbeat backs off when usage runs hot.
	var l engine.Limit
	if a.mode() == "claude" && json.Unmarshal([]byte(a.Store.Get("limit")), &l) == nil && (l.FiveHour >= 0.8 || l.SevenDay >= 0.85) {
		return fmt.Sprintf("skipped: plan usage high (5h %.0f%%, 7d %.0f%%)", l.FiveHour*100, l.SevenDay*100), nil
	}
	now := time.Now()
	since := now.Add(-24 * time.Hour).Unix()
	if v := a.Store.Get("heartbeat:at"); v != "" {
		fmt.Sscan(v, &since)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Heartbeat at %s. Last heartbeat: %s.\n\n", now.In(a.Cfg.Loc).Format("Mon Jan 2 3:04 PM"),
		time.Unix(since, 0).In(a.Cfg.Loc).Format("Mon Jan 2 3:04 PM"))

	turns, _ := a.Store.TurnsSince(p.ID, since, 120)
	var lines []string
	for _, t := range turns {
		when := time.Unix(t.Started, 0).In(a.Cfg.Loc).Format("Mon 3:04PM")
		if t.Kind == "chat" {
			lines = append(lines, fmt.Sprintf("[%s %s] %s: %s\n  %s: %s", when, t.Channel, p.Name, oneLine(t.Input, 400), a.Cfg.Name, oneLine(t.Reply, 400)))
		} else if t.Kind == "action" {
			lines = append(lines, fmt.Sprintf("[%s] (server change log): %s", when, oneLine(t.Reply, 300)))
		} else {
			lines = append(lines, fmt.Sprintf("[%s] %s (unprompted): %s", when, a.Cfg.Name, oneLine(t.Reply, 300)))
		}
	}
	for total(lines) > 14000 && len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		b.WriteString("Conversation since then: none.\n\n")
	} else {
		b.WriteString("Conversation since then (oldest first):\n" + strings.Join(lines, "\n") + "\n\n")
	}
	if rows, err := a.Store.DB.Query(`SELECT id,status,task,result FROM jobs WHERE person=? AND COALESCE(finished,created)>? ORDER BY id LIMIT 10`, p.ID, since); err == nil {
		var js []string
		for rows.Next() {
			var id int64
			var st, task, res string
			rows.Scan(&id, &st, &task, &res)
			js = append(js, fmt.Sprintf("- #%d %s: %s → %s", id, st, oneLine(task, 120), oneLine(res, 200)))
		}
		rows.Close()
		if len(js) > 0 {
			b.WriteString("Background jobs since then:\n" + strings.Join(js, "\n") + "\n\n")
		}
	}
	if rows, err := a.Store.DB.Query(`SELECT name,substr(output,1,200) FROM runs WHERE exit<>0 AND started>? ORDER BY id LIMIT 5`, since); err == nil {
		var bad []string
		for rows.Next() {
			var n, o string
			rows.Scan(&n, &o)
			bad = append(bad, "- "+n+": "+oneLine(o, 200))
		}
		rows.Close()
		if len(bad) > 0 {
			b.WriteString("Scheduled runs that failed since then:\n" + strings.Join(bad, "\n") + "\n\n")
		}
	}
	if a.Jobs != nil {
		if run := a.Jobs.Running(p.ID); len(run) > 0 {
			b.WriteString("Background jobs still running:\n" + strings.Join(run, "\n") + "\n\n")
		}
	}
	// Hourly runs must not repeat themselves: show everything already said unprompted today.
	y, mo, d := now.In(a.Cfg.Loc).Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, a.Cfg.Loc).Unix()
	if rows, err := a.Store.DB.Query(`SELECT started,reply FROM turns WHERE person=? AND kind='notice' AND channel<>'console' AND started>? ORDER BY id LIMIT 20`, p.ID, midnight); err == nil {
		var said []string
		for rows.Next() {
			var ts int64
			var r string
			rows.Scan(&ts, &r)
			said = append(said, fmt.Sprintf("- %s: %s", time.Unix(ts, 0).In(a.Cfg.Loc).Format("3:04PM"), oneLine(r, 200)))
		}
		rows.Close()
		if len(said) > 0 {
			b.WriteString("Already sent him unprompted today (don't repeat):\n" + strings.Join(said, "\n") + "\n\n")
		}
	}
	if in := a.Store.Get("interests:" + p.ID); in != "" {
		b.WriteString("Current interests list:\n" + in + "\n\n")
	} else {
		b.WriteString("Current interests list: (empty; build one from what you know about him)\n\n")
	}
	if loops := a.Store.Get("loops:" + p.ID); loops != "" {
		b.WriteString("Open loops:\n" + loops + "\n\n")
	} else {
		b.WriteString("Open loops: (none recorded)\n\n")
	}
	if log := a.Store.Get("heartbeat:log"); log != "" {
		b.WriteString("Your notes from the last heartbeat:\n" + log + "\n")
	}
	input := b.String()

	ch, chat := a.lastChat(p)
	tid := a.Store.StartTurn(p.ID, ch, chat, "heartbeat", input)
	out := a.runTurnOpts(ctx, p, tid, ch, chat, input, nil, turnOpts{noRecent: true, set: "heartbeat",
		system: a.prompt("heartbeat.md"),
		deny:   map[string]bool{"start_job": true, "cancel_job": true, "send_file": true, "routine": true, "blender": true, "unreal": true, "graphics": true, "desktop": true}})
	if out.err != nil && out.err.Err != nil {
		return "", out.err.Err
	}
	msg, log := splitHeartbeat(out.reply)
	a.Store.Set("heartbeat:at", fmt.Sprint(now.Unix()))
	a.Store.Set("heartbeat:log", log)
	h := now.In(a.Cfg.Loc).Hour()
	if msg == "" || inQuiet(h, a.Cfg.QuietFrom, a.Cfg.QuietTo) { // quiet hours are a hard rule, not a suggestion
		return "silent\n" + log, nil
	}
	a.NotifyFiles(p.ID, ch, chat, msg, out.files)
	return "messaged: " + msg + "\n" + log, nil
}

// inQuiet reports whether hour h falls in [from, to), wrapping midnight.
func inQuiet(h, from, to int) bool {
	if from == to {
		return false
	}
	if from < to {
		return h >= from && h < to
	}
	return h >= from || h < to
}

// splitHeartbeat reads "MESSAGE: ... LOG: ..." (MESSAGE may be SILENT).
func splitHeartbeat(s string) (msg, log string) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "LOG:"); i >= 0 {
		log = strings.TrimSpace(s[i+4:])
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "MESSAGE:"))
	if strings.EqualFold(strings.Trim(s, ". "), "SILENT") {
		s = ""
	}
	return s, log
}

// SetLoops saves the open-loops list (what's pending, promised or unresolved).
func (a *Agent) SetLoops(person, text string) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > 8 {
		lines = lines[:8]
	}
	a.Store.Set("loops:"+person, strings.TrimSpace(strings.Join(lines, "\n")))
}

// SetInterests saves the interests list (shown in every chat's system prompt).
func (a *Agent) SetInterests(person, text string) error {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > 12 {
		lines = lines[:12]
	}
	text = strings.Join(lines, "\n")
	if len(text) > 2000 {
		text = text[:2000]
	}
	a.Store.Set("interests:"+person, text)
	return nil
}

// ---- self-checks (run by nightly maintenance) ----

// SelfCheck re-tests the full chat path when the Claude Code CLI changes
// version (its flags are exo's contract with it), and at least weekly.
// Returns a problem description, or "" when all is well.
func (a *Agent) SelfCheck(ctx context.Context) string {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	out, err := exec.CommandContext(vctx, a.Cfg.ClaudeBin, "--version").Output()
	cancel()
	if err != nil {
		return "Claude Code CLI isn't runnable: " + err.Error()
	}
	ver := strings.TrimSpace(string(out))
	var last int64
	fmt.Sscan(a.Store.Get("selftest:at"), &last)
	if ver == a.Store.Get("claude:version") && time.Since(time.Unix(last, 0)) < 7*24*time.Hour {
		return ""
	}
	if a.mode() != "claude" {
		return "" // can't test the Claude path during an outage; try tomorrow
	}
	p := a.admin()
	tid := a.Store.StartTurn(p.ID, "console", "selftest", "selftest", "self-test")
	res := a.runTurnOpts(ctx, p, tid, "console", "selftest", "Self-test: call the jobs tool once, then reply with exactly SELFTEST OK.", nil,
		turnOpts{noRecent: true})
	t, _ := a.Store.Turn(tid)
	if res.err != nil || !strings.Contains(res.reply, "SELFTEST OK") || t == nil || !strings.Contains(t.Tools, "jobs") {
		why := "no tool call"
		if res.err != nil && res.err.Err != nil {
			why = res.err.Err.Error()
		}
		return fmt.Sprintf("Self-test failed after Claude Code %s: %s. See dashboard turn #%d.", ver, oneLine(why, 200), tid)
	}
	a.Store.Set("claude:version", ver)
	a.Store.Set("selftest:at", fmt.Sprint(time.Now().Unix()))
	return ""
}
