// exo is a small-context agent harness for a home server.
//
//	exo serve    run everything (the systemd unit)
//	exo budget   measure the fixed token cost of each person's prompt and tools
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

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

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fatal(err)
	}
	st, err := store.Open(cfg.Path("exo.db"))
	if err != nil {
		fatal(err)
	}
	switch cmd {
	case "serve":
		err = serve(cfg, st)
	case "budget":
		err = budget(cfg, st)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fatal(err)
	}
}

func serve(cfg *config.Config, st *store.Store) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		MCPBase: "http://" + cfg.MCPAddr, MCPToken: cfg.MCPToken, WorkDir: cfg.Path("work"),
		OnLimit: func(l engine.Limit) { j, _ := json.Marshal(l); st.Set("limit", string(j)) }}
	a.Local = &engine.Local{URL: cfg.LocalURL, Model: cfg.LocalModel, Registry: reg, ToolCtx: a.ToolCtx,
		Variant: cfg.LocalVariant, AnswerTokens: cfg.LocalMaxTokens}
	if err := a.Init(); err != nil {
		return err
	}
	reg.Routines = a
	reg.SetInterests = a.SetInterests
	reg.SetLoops = a.SetLoops

	wrap := func(s channel.Sender) channel.Sender {
		if cfg.DryRun {
			return &channel.DryRun{Inner: s}
		}
		return s
	}
	var servers []*http.Server
	listen := func(name, addr string, h http.Handler) {
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func() {
			slog.Info("listening", "what", name, "addr", addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("listener", "what", name, "err", err)
				stop()
			}
		}()
	}

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
		a.Discord = dc
		a.Senders["discord"] = wrap(dc)
	}

	mcpSrv := &mcp.Server{Token: cfg.MCPToken, Registry: reg, Lookup: a.Lookup}
	mm := http.NewServeMux()
	mm.Handle("/mcp/", mcpSrv)
	listen("mcp", cfg.MCPAddr, mm)

	admin := ""
	for _, p := range cfg.People {
		if p.Admin && p.Discord != "" {
			admin = "discord:" + p.ID
			break
		}
	}
	sc := &sched.Scheduler{Cfg: cfg, Store: st, Deliver: a.DeliverTarget, Routine: a.RunRoutine,
		Builtin: map[string]func(context.Context) (string, error){
			"maintenance": maintenance(cfg, st, a),
			"heartbeat":   a.Heartbeat,
		}}
	a.Sched = sc
	heartbeat := cfg.Heartbeat
	if heartbeat == "" {
		heartbeat = "0 0 31 2 *" // never fires; the entry still shows on the dashboard for Run now
	}
	if err := sc.Start([]sched.Entry{
		{Name: "maintenance", Cron: "40 3 * * *", Cmd: "maintenance", Deliver: admin},
		{Name: "heartbeat", Cron: heartbeat, Cmd: "heartbeat"},
	}); err != nil {
		return err
	}
	defer sc.Stop()
	a.SyncRoutines()
	runner.Resume() // re-attach to jobs that kept running while exo was down

	dash := &web.Server{Agent: a, Store: st, Sched: sc, Senders: a.Senders, Started: time.Now()}
	listen("dashboard", cfg.DashAddr, dash.Handler())

	slog.Info("exo up", "dry_run", cfg.DryRun, "schedule", cfg.ScheduleEnabled, "failover", cfg.Failover, "mode", a.Mode())
	a.Run(ctx)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return nil
}

// maintenance runs nightly: prune traces, back the database up, clear
// old inbound/outbound media, and re-test the Claude path when the CLI
// changes. It reports only problems.
func maintenance(cfg *config.Config, st *store.Store, a *agent.Agent) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
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
		dir := cfg.Path("backups")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			problems = append(problems, "backup dir: "+err.Error())
		} else {
			dst := filepath.Join(dir, "exo-"+time.Now().Format("20060102")+".db")
			os.Remove(dst)
			if err := st.Backup(ctx, dst); err != nil {
				problems = append(problems, "backup: "+err.Error())
			}
			olds, _ := filepath.Glob(filepath.Join(dir, "exo-*.db"))
			sort.Strings(olds)
			for len(olds) > 14 {
				os.Remove(olds[0])
				olds = olds[1:]
			}
		}
		if p := a.SelfCheck(ctx); p != "" {
			problems = append(problems, p)
		}
		return strings.Join(problems, "\n"), nil
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "exo:", err)
	os.Exit(1)
}
