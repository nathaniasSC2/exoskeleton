// Package app wires the harness together: store, agent, tools, jobs, engines,
// channels, MCP server, scheduler and dashboard. cmd/exo is a thin main over
// it; a deployment with its own channels and tools builds an App, adds to it,
// and runs it:
//
//	a, err := app.New(cfg)
//	a.Reg.Add(myTools...)
//	a.Agent.AddChannel(agent.Channel{...})
//	a.AddBuiltin(sched.Entry{...}, fn)
//	err = a.Run(ctx)
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/agent"
	"github.com/nathaniasSC2/exoskeleton/channel"
	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/engine"
	"github.com/nathaniasSC2/exoskeleton/jobs"
	"github.com/nathaniasSC2/exoskeleton/mcp"
	"github.com/nathaniasSC2/exoskeleton/sched"
	"github.com/nathaniasSC2/exoskeleton/store"
	"github.com/nathaniasSC2/exoskeleton/tools"
	"github.com/nathaniasSC2/exoskeleton/web"
)

type App struct {
	Cfg   *config.Config
	Store *store.Store
	Agent *agent.Agent
	Reg   *tools.Registry
	Jobs  *jobs.Runner
	Sched *sched.Scheduler // built by Run
	Web   *web.Server      // routes, pages and nav can be added before Run
	MCP   *http.ServeMux   // the MCP listener's routes (/mcp/ is taken)

	builtins []builtin
	checks   []func(context.Context) string
	starts   []func(context.Context) error
	servers  []*http.Server
	stop     context.CancelFunc
}

type builtin struct {
	entry sched.Entry
	run   func(context.Context) (string, error)
}

// Open opens the brand's database in the data dir (<slug>.db).
func Open(cfg *config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	return store.Open(cfg.Path(cfg.Slug() + ".db"))
}

// New builds everything but starts nothing.
func New(cfg *config.Config) (*App, error) {
	st, err := Open(cfg)
	if err != nil {
		return nil, err
	}
	channel.Transcriber = strings.Fields(cfg.Transcribe)
	a := &agent.Agent{Cfg: cfg, Store: st, Senders: map[string]channel.Sender{"console": channel.Console{}}}
	runner := jobs.New(cfg, st)
	runner.Brief = a.JobBrief
	runner.Notify = a.NotifyFiles
	runner.Register = a.Register
	runner.Local = func() bool { return a.Mode() == "local" }
	var notes tools.NotesClient
	if cfg.NotesMCP != "" {
		notes = mcp.NewClient(cfg.NotesMCP)
	}
	reg := tools.New(cfg, st, runner, notes)
	a.Reg, a.Jobs = reg, runner
	a.Claude = &engine.Claude{Bin: cfg.ClaudeBin, Model: cfg.Model, Effort: cfg.Effort, Agents: cfg.Agents(),
		MCPBase: "http://" + cfg.MCPAddr, MCPToken: cfg.MCPToken, MCPName: cfg.Slug(), WorkDir: cfg.Path("work"),
		OnLimit: func(l engine.Limit) { j, _ := json.Marshal(l); st.Set("limit", string(j)) }}
	a.Local = &engine.Local{URL: cfg.LocalURL, Model: cfg.LocalModel, Registry: reg, ToolCtx: a.ToolCtx,
		Variant: cfg.LocalVariant, AnswerTokens: cfg.LocalMaxTokens}
	if err := a.Init(); err != nil {
		return nil, err
	}
	reg.Routines = a
	reg.SetInterests = a.SetInterests
	reg.SetLoops = a.SetLoops
	ap := &App{Cfg: cfg, Store: st, Agent: a, Reg: reg, Jobs: runner, MCP: http.NewServeMux(),
		Web: &web.Server{Agent: a, Store: st, Senders: a.Senders}}
	ap.MCP.Handle("/mcp/", &mcp.Server{Name: cfg.Slug(), Token: cfg.MCPToken, Registry: reg, Lookup: a.Lookup})
	return ap, nil
}

// Wrap puts a sender behind the dry-run switch.
func (ap *App) Wrap(s channel.Sender) channel.Sender {
	if ap.Cfg.DryRun {
		return &channel.DryRun{Inner: s}
	}
	return s
}

// AddBuiltin schedules a Go function next to maintenance and the heartbeat
// (its Cmd is set to its Name). Call before Run.
func (ap *App) AddBuiltin(e sched.Entry, run func(context.Context) (string, error)) {
	e.Cmd = e.Name
	ap.builtins = append(ap.builtins, builtin{e, run})
}

// AddCheck adds a nightly check to maintenance; a non-empty result is a
// problem to report.
func (ap *App) AddCheck(f func(context.Context) string) { ap.checks = append(ap.checks, f) }

// OnStart runs f when Run starts, after the built-in channels are up and
// before the listeners, the scheduler and the agent (start background
// loops, re-attach to work here). An error stops Run.
func (ap *App) OnStart(f func(context.Context) error) { ap.starts = append(ap.starts, f) }

// Listen serves h on addr while the app runs; a listener that fails stops it.
func (ap *App) Listen(name, addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ap.servers = append(ap.servers, srv)
	go func() {
		slog.Info("listening", "what", name, "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listener", "what", name, "err", err)
			ap.stop()
		}
	}()
}

// Run starts everything and blocks until ctx ends.
func (ap *App) Run(ctx context.Context) error {
	ctx, ap.stop = context.WithCancel(ctx)
	defer ap.stop()
	cfg, a := ap.Cfg, ap.Agent

	if cfg.DiscordEnabled && cfg.DiscordToken != "" {
		dc, err := channel.NewDiscord(cfg.DiscordToken, cfg.Path("inbox"), func(id string) bool { return cfg.ByDiscord(id) != nil }, a.Handle)
		if err != nil {
			return err
		}
		// discordgo reconnects on its own only after a first successful Open, so
		// a network blip at startup would otherwise leave Discord down until a restart.
		go func() {
			for wait := 5 * time.Second; ; wait = min(2*wait, 5*time.Minute) {
				err := dc.Open()
				if err == nil {
					slog.Info("discord connected")
					return
				}
				slog.Error("discord", "err", err, "retry_in", wait)
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}()
		defer dc.Close()
		a.AddChannel(agent.Channel{Name: "discord", Sender: ap.Wrap(dc), TypingEvery: 8 * time.Second, // its indicator lasts ~10s
			Who: func(in channel.Inbound) *config.Person { return cfg.ByDiscord(in.Sender) },
			ChatFor: func(p *config.Person) (string, error) {
				if p.Discord == "" {
					return "", fmt.Errorf("%s has no Discord id", p.ID)
				}
				return dc.DM(p.Discord)
			}})
	}
	for _, f := range ap.starts {
		if err := f(ctx); err != nil {
			return err
		}
	}

	ap.Listen("mcp", cfg.MCPAddr, ap.MCP)

	admin := "" // maintenance problems go to the first admin on Discord
	for _, p := range cfg.People {
		if p.Admin && p.Discord != "" {
			admin = "discord:" + p.ID
			break
		}
	}
	sc := &sched.Scheduler{Cfg: cfg, Store: ap.Store, Deliver: a.DeliverTarget, Routine: a.RunRoutine,
		Builtin: map[string]func(context.Context) (string, error){
			"maintenance": ap.maintenance,
			"heartbeat":   a.Heartbeat,
		}}
	heartbeat := cfg.Heartbeat
	if heartbeat == "" {
		heartbeat = "0 0 31 2 *" // never fires; the entry still shows on the dashboard for Run now
	}
	entries := []sched.Entry{
		{Name: "maintenance", Cron: "40 3 * * *", Cmd: "maintenance", Deliver: admin},
		{Name: "heartbeat", Cron: heartbeat, Cmd: "heartbeat"},
	}
	for _, b := range ap.builtins {
		sc.Builtin[b.entry.Name] = b.run
		entries = append(entries, b.entry)
	}
	ap.Sched, a.Sched, ap.Web.Sched = sc, sc, sc
	if err := sc.Start(entries); err != nil {
		return err
	}
	defer sc.Stop()
	a.SyncRoutines()
	ap.Jobs.Resume() // re-attach to jobs that kept running while the harness was down

	ap.Web.Started = time.Now()
	ap.Listen("dashboard", cfg.DashAddr, ap.Web.Handler())

	slog.Info(cfg.Slug()+" up", "dry_run", cfg.DryRun, "schedule", cfg.ScheduleEnabled, "failover", cfg.Failover, "mode", a.Mode())
	a.Run(ctx)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range ap.servers {
		s.Shutdown(sctx)
	}
	return nil
}

// maintenance runs nightly: prune traces, back the database up, clear old
// inbound/outbound media, re-test the Claude path when the CLI changes, and
// run the added checks. It reports only problems.
func (ap *App) maintenance(ctx context.Context) (string, error) {
	cfg, st := ap.Cfg, ap.Store
	st.Prune(ctx, 30)
	var problems []string
	for _, dir := range []string{cfg.Path("inbox"), cfg.Path("outbox")} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 14*24*time.Hour {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if dir := cfg.BackupDir; dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			problems = append(problems, "backup dir: "+err.Error())
		} else {
			dst := filepath.Join(dir, cfg.Slug()+"-"+time.Now().Format("20060102")+".db")
			os.Remove(dst)
			if err := st.Backup(ctx, dst); err != nil {
				problems = append(problems, "backup: "+err.Error())
			}
			olds, _ := filepath.Glob(filepath.Join(dir, cfg.Slug()+"-*.db"))
			sort.Strings(olds)
			for len(olds) > 14 {
				os.Remove(olds[0])
				olds = olds[1:]
			}
		}
	}
	if p := ap.Agent.SelfCheck(ctx); p != "" {
		problems = append(problems, p)
	}
	for _, f := range ap.checks {
		if p := f(ctx); p != "" {
			problems = append(problems, p)
		}
	}
	return strings.Join(problems, "\n"), nil
}
