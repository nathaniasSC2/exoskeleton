// Package sched runs scheduled work: script jobs from schedule.json, the
// built-ins (maintenance, heartbeat) and the routines the assistant sets up.
// A script's stdout is the message; empty stdout means stay quiet. No model
// is involved, so these cost nothing and can't hallucinate.
package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/proc"
	"github.com/nathaniasSC2/exoskeleton/store"
)

type Entry struct {
	Name       string `json:"name"`
	Cron       string `json:"cron"`
	Cmd        string `json:"cmd"`
	Deliver    string `json:"deliver"` // "discord:<person>" | "" (log only)
	TimeoutMin int    `json:"timeout_min"`
	// Until makes an entry temporary: "YYYY-MM-DD" (through the end of that
	// local day) or an RFC 3339 time. Past it the entry stops running, and
	// nightly maintenance takes it out of schedule.json (app/temp.go).
	Until string `json:"until,omitempty"`
}

// UntilTime is when a temporary entry stops (ok=false: no until, or one that
// doesn't parse; UntilErr tells the two apart).
func (e Entry) UntilTime(loc *time.Location) (time.Time, bool) {
	t, err := ParseUntil(e.Until, loc)
	return t, err == nil && !t.IsZero()
}

// ParseUntil reads an until value: "" is the zero time, "YYYY-MM-DD" is the
// end of that local day, otherwise RFC 3339.
func ParseUntil(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return d.AddDate(0, 0, 1), nil
	}
	return time.Parse(time.RFC3339, s)
}

// Expired reports whether a temporary entry is past its until.
func (e Entry) Expired(now time.Time, loc *time.Location) bool {
	t, ok := e.UntilTime(loc)
	return ok && !now.Before(t)
}

type manualKey struct{}

// Manual reports whether a built-in was started by hand (Run now on the
// dashboard) rather than by its schedule, so it can skip its own idle checks.
func Manual(ctx context.Context) bool { v, _ := ctx.Value(manualKey{}).(bool); return v }

type Scheduler struct {
	Cfg     *config.Config
	Store   *store.Store
	Deliver func(target, text string) error
	// DeliverFiles sends text plus attachments. A cmd job attaches a file by
	// printing a line "ATTACH: /absolute/path"; see Attachments.
	DeliverFiles func(target, text string, files []string) error
	Builtin      map[string]func(context.Context) (string, error) // in-process jobs
	// Routine runs a routine by id ("routine:12" entries); routines deliver
	// their own messages.
	Routine func(ctx context.Context, id int64) (string, error)

	mu       sync.Mutex
	entries  []Entry
	routines []Entry
	rids     []cron.EntryID
	c        *cron.Cron
	running  map[string]bool
}

// SetRoutines replaces the routine entries (called whenever one is added,
// paused or removed). Static entries are untouched.
func (s *Scheduler) SetRoutines(rs []Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return fmt.Errorf("scheduler not started")
	}
	for _, id := range s.rids {
		s.c.Remove(id)
	}
	s.rids, s.routines = nil, nil
	var firstErr error
	for _, e := range rs {
		e := e
		id, err := s.c.AddJob(e.Cron, namedJob{e.Name, func() { s.run(e) }})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("routine %s: %w", e.Name, err)
			}
			continue
		}
		s.rids = append(s.rids, id)
		s.routines = append(s.routines, e)
	}
	return firstErr
}

// ValidCron reports whether a 5-field cron spec parses.
func ValidCron(spec string) error {
	_, err := cron.ParseStandard(spec)
	return err
}

func (s *Scheduler) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(append([]Entry(nil), s.entries...), s.routines...)
}

func (s *Scheduler) Next(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return time.Time{}
	}
	for _, e := range s.c.Entries() {
		if j, ok := e.Job.(namedJob); ok && j.name == name {
			return e.Next
		}
	}
	return time.Time{}
}

type namedJob struct {
	name string
	fn   func()
}

func (j namedJob) Run() { j.fn() }

// Start loads schedule.json. Script entries only run when EXO_SCHEDULE is on
// (so a new setup can be tested without side effects); builtins always run.
func (s *Scheduler) Start(builtins []Entry) error {
	s.running = map[string]bool{}
	var entries []Entry
	if s.Cfg.ScheduleEnabled {
		raw, err := os.ReadFile(s.Cfg.Path("schedule.json"))
		if err == nil {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return fmt.Errorf("schedule.json: %w", err)
			}
		}
	}
	entries = append(entries, builtins...)
	s.c = cron.New(cron.WithLocation(s.Cfg.Loc))
	for _, e := range entries {
		e := e
		if _, err := ParseUntil(e.Until, s.Cfg.Loc); err != nil {
			slog.Warn("schedule: unreadable until, the entry runs without one", "job", e.Name, "until", e.Until)
		}
		if _, err := s.c.AddJob(e.Cron, namedJob{e.Name, func() { s.run(e) }}); err != nil {
			return fmt.Errorf("schedule %s: %w", e.Name, err)
		}
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	s.c.Start()
	return nil
}

func (s *Scheduler) Stop() {
	if s.c != nil {
		<-s.c.Stop().Done()
	}
}

func (s *Scheduler) RunNow(name string) error {
	for _, e := range s.Entries() {
		if e.Name == name || e.Cmd == name {
			go s.runCtx(context.WithValue(context.Background(), manualKey{}, true), e)
			return nil
		}
	}
	return fmt.Errorf("no job %q", name)
}

func (s *Scheduler) run(e Entry) { s.runCtx(context.Background(), e) }

func (s *Scheduler) runCtx(parent context.Context, e Entry) {
	if !Manual(parent) && e.Expired(time.Now(), s.Cfg.Loc) {
		slog.Info("scheduled run skipped: past its until", "job", e.Name, "until", e.Until)
		return
	}
	s.mu.Lock()
	if s.running[e.Name] {
		s.mu.Unlock()
		return
	}
	s.running[e.Name] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.running, e.Name); s.mu.Unlock() }()

	timeout := time.Duration(max(e.TimeoutMin, 1)) * time.Minute
	if e.TimeoutMin == 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	res, _ := s.Store.DB.Exec(`INSERT INTO runs(name,started) VALUES(?,?)`, e.Name, time.Now().Unix())
	runID, _ := res.LastInsertId()

	var out string
	exit := 0
	if strings.HasPrefix(e.Cmd, "routine:") && s.Routine != nil {
		var rid int64
		fmt.Sscan(strings.TrimPrefix(e.Cmd, "routine:"), &rid)
		o, err := s.Routine(ctx, rid)
		out = o
		if err != nil {
			exit, out = 1, strings.TrimSpace(out+"\n"+err.Error())
		}
	} else if fn, ok := s.Builtin[e.Cmd]; ok {
		o, err := fn(ctx)
		out = o
		if err != nil {
			exit, out = 1, strings.TrimSpace(out+"\n"+err.Error())
		}
	} else {
		cmd := exec.CommandContext(ctx, "bash", "-c", e.Cmd)
		cmd.Dir = s.Cfg.DataDir
		proc.Group(cmd)
		cmd.WaitDelay = 5 * time.Second
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = os.Stderr // journald
		err := cmd.Run()
		if err != nil {
			exit = 1
			if ee, ok := err.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			}
		}
		out = strings.TrimSpace(buf.String())
		if len(out) > 8000 {
			out = out[:8000] + "…"
		}
		if exit != 0 && out == "" {
			out = fmt.Sprintf("%s failed (exit %d); see journalctl -u %s", e.Name, exit, s.Cfg.Slug())
		}
	}
	delivered := 0
	text, files := Attachments(out)
	if len(files) > 0 && s.DeliverFiles == nil {
		text, files = out, nil // nothing can attach: send the lines as they are
	}
	if (text != "" || len(files) > 0) && e.Deliver != "" && s.Deliver != nil {
		send := func() error { return s.Deliver(e.Deliver, text) }
		if len(files) > 0 {
			send = func() error { return s.DeliverFiles(e.Deliver, text, files) }
		}
		if err := send(); err != nil {
			slog.Warn("schedule delivery", "job", e.Name, "err", err)
		} else {
			delivered = 1
		}
	}
	s.Store.DB.Exec(`UPDATE runs SET finished=?, exit=?, output=?, delivered=? WHERE id=?`, time.Now().Unix(), exit, out, delivered, runID)
	slog.Info("scheduled run", "job", e.Name, "exit", exit, "chars", len(out), "delivered", delivered == 1)
}

// Attachments splits a cmd job's output into the message and its files. A line
// "ATTACH: /absolute/path" names a file to send with the message; it is taken
// out of the text. A path that is relative, missing or not a regular file stays
// in the text as it was, so a mistake is visible instead of silently dropped.
func Attachments(out string) (string, []string) {
	var keep, files []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "ATTACH: "); ok {
			p = strings.TrimSpace(p)
			if st, err := os.Stat(p); err == nil && filepath.IsAbs(p) && st.Mode().IsRegular() {
				files = append(files, p)
				continue
			}
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n")), files
}
