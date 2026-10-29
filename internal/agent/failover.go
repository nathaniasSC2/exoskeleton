package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/config"
	"github.com/nathaniasSC2/exoskeleton/internal/engine"
	"github.com/nathaniasSC2/exoskeleton/internal/store"
)

// Failover state lives in kv so it survives restarts:
//   mode        = claude | local      which brain answers
//   claude_down = "" | reason         Claude failed; messages wait for a decision
//   local_mine  = 1                   exo started the local model (so exo may stop it)

type failover struct {
	mu        sync.Mutex
	fails     int
	askedAt   time.Time
	guestTold map[string]time.Time
	lastProbe time.Time
}

func (a *Agent) mode() string {
	if a.Store.Get("mode") == "local" {
		return "local"
	}
	if a.Store.Get("claude_down") != "" {
		return "held"
	}
	return "claude"
}

// Mode is the dashboard's view: claude, local, or held (down, awaiting a decision).
func (a *Agent) Mode() string { return a.mode() }

// onFailure decides whether a failed Claude turn means Claude is down. It
// returns true when failover has taken over the reply.
func (a *Agent) onFailure(ctx context.Context, p *config.Person, res *engine.Result, eventIDs []int64) bool {
	if a.mode() == "local" {
		return false
	}
	hard := res.Kind == engine.RateLimit || res.Kind == engine.Auth
	a.fo.mu.Lock()
	a.fo.fails++
	n := a.fo.fails
	a.fo.mu.Unlock()
	if !hard && n < 2 {
		return false // one bad turn is just an apology, not an outage
	}
	reason := describe(res)
	a.Store.Set("claude_down", reason)
	a.Store.MarkEvents(eventIDs, "held", 0)
	slog.Warn("claude down", "reason", reason)

	if !p.Admin {
		a.fo.mu.Lock()
		if a.fo.guestTold == nil {
			a.fo.guestTold = map[string]time.Time{}
		}
		told := time.Since(a.fo.guestTold[p.ID]) < 2*time.Hour
		a.fo.guestTold[p.ID] = time.Now()
		a.fo.mu.Unlock()
		if !told {
			ch, chat := a.lastChat(p)
			a.Notify(p.ID, ch, chat, "My brain's offline for a bit. I'll answer as soon as it's back.")
		}
	}
	switch a.Cfg.Failover {
	case "auto":
		go a.GoLocal(context.Background(), reason)
	case "ask":
		a.ask(reason)
	default:
		a.tellAdmin(fmt.Sprintf("Claude isn't answering (%s). I'll pick back up when it's back.", reason))
	}
	return true
}

func (a *Agent) onSuccess() {
	a.fo.mu.Lock()
	a.fo.fails = 0
	a.fo.mu.Unlock()
}

func (a *Agent) ask(reason string) {
	a.fo.mu.Lock()
	recent := time.Since(a.fo.askedAt) < 30*time.Minute
	if !recent {
		a.fo.askedAt = time.Now()
	}
	a.fo.mu.Unlock()
	if !recent {
		a.tellAdmin(fmt.Sprintf("Claude isn't answering (%s). Reply \"go local\" to switch me to the local model, or just wait and I'll answer when Claude's back.", reason))
	}
}

func (a *Agent) askPending() bool {
	a.fo.mu.Lock()
	defer a.fo.mu.Unlock()
	return a.Store.Get("claude_down") != "" && time.Since(a.fo.askedAt) < 6*time.Hour
}

func (a *Agent) admin() *config.Person {
	for _, p := range a.Cfg.People {
		if p.Admin {
			return p
		}
	}
	return nil
}

func (a *Agent) tellAdmin(text string) {
	if p := a.admin(); p != nil {
		ch, chat := a.lastChat(p)
		a.Notify(p.ID, ch, chat, text)
	}
}

// lastChat is where a person last talked to the assistant, falling back to their home channel.
func (a *Agent) lastChat(p *config.Person) (string, string) {
	var ch, chat string
	a.Store.DB.QueryRow(`SELECT channel,chat FROM events WHERE person=? AND channel<>'console' ORDER BY id DESC LIMIT 1`, p.ID).Scan(&ch, &chat)
	if ch == "" && p.HomeChat != "" {
		if _, ch2, chat2, err := a.resolveTarget(p.HomeChat + ":" + p.ID); err == nil {
			ch, chat = ch2, chat2
		}
	}
	return ch, chat
}

func describe(res *engine.Result) string {
	switch res.Kind {
	case engine.RateLimit:
		if res.Limit != nil && res.Limit.ResetsAt > 0 {
			return "usage limit, resets " + time.Unix(res.Limit.ResetsAt, 0).Local().Format("Mon 3:04 PM")
		}
		return "usage limit"
	case engine.Auth:
		return "login problem; the token may have expired"
	case engine.Timeout:
		return "it keeps timing out"
	case engine.Overloaded:
		return "the API is erroring or overloaded"
	}
	return "repeated errors"
}

// localCmd runs EXO_LOCAL_START or EXO_LOCAL_STOP (whatever starts or stops
// the local model's server on this box).
func (a *Agent) localCmd(ctx context.Context, cmd string) error {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, "bash", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// GoLocal starts the local model if needed and moves every conversation to it.
func (a *Agent) GoLocal(ctx context.Context, reason string) error {
	if a.Store.Get("mode") == "local" {
		return nil
	}
	// Quiet by design: the only things worth messaging are the question and a failure.
	if !a.Local.Ready(ctx) {
		if a.Cfg.LocalStart == "" {
			a.tellAdmin("The local model isn't running, and EXO_LOCAL_START isn't set.")
			return fmt.Errorf("local model not ready")
		}
		if err := a.localCmd(ctx, a.Cfg.LocalStart); err != nil {
			a.tellAdmin("Couldn't start the local model: " + err.Error())
			return err
		}
		a.Store.Set("local_mine", "1")
		deadline := time.Now().Add(5 * time.Minute)
		for !a.Local.Ready(ctx) {
			if time.Now().After(deadline) {
				a.tellAdmin("The local model didn't come up within 5 minutes.")
				return fmt.Errorf("local model not ready")
			}
			time.Sleep(5 * time.Second)
		}
	}
	a.Store.Set("mode", "local")
	slog.Info("switched to local", "reason", reason)
	a.release()
	return nil
}

// GoClaude moves back to Claude and stops the local model if exo started it.
func (a *Agent) GoClaude(ctx context.Context, why string) {
	wasLocal := a.Store.Get("mode") == "local"
	a.Store.Set("mode", "claude")
	a.Store.Set("claude_down", "")
	a.onSuccess()
	if wasLocal && a.Store.Get("local_mine") == "1" && a.Cfg.LocalStop != "" {
		if err := a.localCmd(ctx, a.Cfg.LocalStop); err != nil {
			slog.Warn("stopping the local model", "err", err)
		}
		a.Store.Set("local_mine", "")
	}
	slog.Info("back on claude", "was_local", wasLocal, "why", why) // the dashboard shows it; no message
	a.release()
}

// release re-queues messages that waited out an outage so they get answered.
func (a *Agent) release() {
	a.Store.DB.Exec(`UPDATE events SET status='new', ts=? WHERE status='held' AND ts>?`, time.Now().Unix(), time.Now().Add(-6*time.Hour).Unix())
	for _, p := range a.Cfg.People {
		a.poke(p.ID)
	}
}

// probeLoop checks whether Claude works again while it's marked down, waiting
// out a known reset time rather than hammering the API.
func (a *Agent) probeLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.Store.Get("claude_down") == "" {
			continue
		}
		wait := 5 * time.Minute
		var l engine.Limit
		if json.Unmarshal([]byte(a.Store.Get("limit")), &l) == nil && l.Status == "rejected" && l.ResetsAt > 0 {
			if until := time.Until(time.Unix(l.ResetsAt, 0)); until > wait {
				wait = until
			}
		}
		if time.Since(a.fo.lastProbe) < wait {
			continue
		}
		a.fo.lastProbe = time.Now()
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		res := a.Claude.Probe(pctx, a.Cfg.ProbeModel)
		cancel()
		if res.Err == nil {
			slog.Info("claude probe ok")
			a.GoClaude(ctx, "probe")
		} else {
			slog.Info("claude still down", "kind", res.Kind)
		}
	}
}

// ---- commands ----

var stopJobRe = regexp.MustCompile(`^(?:stop|cancel|kill) job #?(\d+)$`)

// command handles the few things the harness answers itself, without a model.
func (a *Agent) command(ctx context.Context, p *config.Person, ev *store.Event, ids []int64, input string) bool {
	c := strings.ToLower(strings.Trim(strings.TrimSpace(input), ".!"))
	reply := func(text string) {
		a.Store.MarkEvents(ids, "done", 0)
		a.notify(p.ID, ev.Channel, ev.Chat, "system", text, nil)
	}
	switch {
	case c == "/new" || c == "new chat":
		var last int64
		a.Store.DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM turns WHERE person=?`, p.ID).Scan(&last)
		a.Store.Set("epoch:"+p.ID, fmt.Sprint(last)) // context starts after this turn
		reply("Fresh start. (Saved facts are kept.)")
	case c == "/status":
		reply(a.StatusLine(p))
	case c == "/help":
		reply("Commands: /status, /new (fresh conversation), /local and /claude (switch brains), /retry, \"stop\" (while I'm working on something), \"stop job N\".")
	case stopJobRe.MatchString(c):
		n, _ := strconv.ParseInt(stopJobRe.FindStringSubmatch(c)[1], 10, 64)
		if err := a.Jobs.Cancel(p.ID, n); err != nil {
			reply(fmt.Sprintf("Couldn't stop it: %v", err))
		} else {
			reply(fmt.Sprintf("Stopping job #%d; its report follows.", n))
		}
	case !p.Admin:
		return false
	case c == "/local" || c == "go local" || (a.askPending() && (c == "yes" || c == "do it" || c == "yes go local")):
		a.Store.MarkEvents(ids, "done", 0)
		go a.GoLocal(context.Background(), "asked")
	case c == "/claude" || c == "go claude" || c == "/retry":
		a.Store.MarkEvents(ids, "done", 0)
		go a.GoClaude(context.Background(), "")
	default:
		return false
	}
	return true
}

func (a *Agent) StatusLine(p *config.Person) string {
	var b strings.Builder
	switch a.mode() {
	case "local":
		b.WriteString("Brain: local model (fallback).")
	case "held":
		b.WriteString("Brain: Claude is down (" + a.Store.Get("claude_down") + "); waiting.")
	default:
		b.WriteString("Brain: Claude.")
	}
	var l engine.Limit
	if json.Unmarshal([]byte(a.Store.Get("limit")), &l) == nil && l.Status != "" {
		fmt.Fprintf(&b, " Usage: %.0f%% of 5h, %.0f%% of 7d.", l.FiveHour*100, l.SevenDay*100)
	}
	if p.Admin {
		var running int
		a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&running)
		fmt.Fprintf(&b, " Jobs running: %d.", running)
	}
	return b.String()
}
