package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/config"
	"github.com/nathaniasSC2/exoskeleton/internal/engine"
	"github.com/nathaniasSC2/exoskeleton/internal/store"
	"github.com/nathaniasSC2/exoskeleton/internal/tools"
)

// Shadow mode: after a real Claude turn, the local model answers the same
// request in the background. Nothing is delivered and side-effect tools are
// skipped (read-only shell still runs), so the admin can compare the two on
// the dashboard's Shadow page and tune the local stack against a quality bar.
// EXO_SHADOW lists the variants: "fast" (no thinking), "think", "think<N>".
// Only admins' turns are shadowed: other people's conversations stay private.
//
// The "history" source replays past Claude turns exactly as Claude saw them
// (the stored system prompt and the traced user block), so there's a baseline
// without spending Claude calls. Tool results are today's, not the original's.

// shadowRev names the current prompt/engine revision. Bump it when the local
// path changes so the Shadow page compares rounds instead of mixing them.
// The log is the tuning history of the deployment this was extracted from
// (Qwen3.6-35B-A3B on llama.cpp); some rounds touched tools that aren't here.
//
//	r1  first run (2026-10-11 08:20)
//	r2  honesty/tool-use note for the local brain; shadow shell refusals match live
//	r3  images really reach the model; unverified-claim guard; broken tool JSON retried;
//	    jobs and specialists available on the local brain
//	r4  realistic stubs for skipped side effects, repeat-call breaker, wider claim
//	    guard, search-when-unsure and act-don't-describe in the local note
//	r5  tool calls leaked as Qwen XML text are parsed and run; "keeping an eye on" claims
//	r6  shell allows 2>/dev/null; no leaked-call revival once tools are withdrawn; a
//	    forced final answer instead of "kept calling tools"; no "(used: X)" in local history
//	r7  model profiles: card sampling per family and mode, thinking budgets (think<N>),
//	    answer allowance on top of thinking, gpt-oss effort, per-step timings
//	r8  web_search through a real browser's Bing (DuckDuckGo blocked it: every search was empty),
//	    quote-aware read-only shell with ; && || chains, wider leak/claim guards,
//	    don't-ask-just-do and notes-first in the local note
//	r9  web_search via Startpage first (Bing gave the bot off-topic results)
//	r10 search re-checks a slow page before falling back, fallback results flagged,
//	    "fetch the best page before stating specifics"; llama.cpp b11557
//	r11 the thinking stays with its tool calls across steps (preserved reasoning)
//	r12 an empty reply is never final (nudge, then answer without tools); the claim guard
//	    fires when no action tool ran, not only when no tool ran; check-before-agreeing
//	r13 the empty-reply retry also covers the last tool step (it fell through as a blank
//	    text after 7-step chains); a local turn never ends with an empty reply and no error
//	r14 time-aware thinking: each call's budget fits half the time left at the model's
//	    measured speed (slow models overran the 10-minute limit inside a single call)
//	    + K2 tool calls half-parsed by llama.cpp (<ifm|arg_key> markup) are repaired or parsed
//	r15 images tools return (screenshots) reach a vision model as a user message after the
//	    step's results; a text-only model is told it can't see them (it used to invent pages)
//	r16 self-check: a thinking turn's draft gets a second look (specifics must be in a tool
//	    result, premises confirmed, no unbacked "done") + bad tool args refused before
//	    replay stubs (enum values, reminder time/text)
//	r17 the self-check also covers long tool chains (it adds a step instead of being
//	    skipped past step 6) + reminder times tolerate seconds; replays skip "in the past"
//	r18 self-check v2 (found in the 2026-10-13 trial): told the person never sees the draft,
//	    earlier replies were tool-checked (a context line lists them, since local history
//	    carries no tags), unseen things get a question not a guess; a rewrite that leaks its
//	    working is stripped, and nudged once if it still confesses to a draft
//	r19 no-tool give-ups get one push to try (gave_up guard); the unseen-thing rule now
//	    says search first (r18's "ask" made it ask for a photo it could have found); the
//	    local note says what web_search/fetch/screenshot/start_job can do
//	r20 the unverified-claim guard also catches made things ("here's how it looks", "I
//	    cropped it", "Done — reminder set") when no acting tool ran, even after searches
//	    and screenshots
const shadowRev = "r20"

type shadowJob struct {
	turn    int64
	source  string // live | history
	variant string
	p       *config.Person
	ch      string
	chat    string
	req     *engine.Request
}

type shadowState struct {
	live, hist chan shadowJob
	running    atomic.Int64 // turn being shadowed (0 = idle)
	done       atomic.Int64
}

func (a *Agent) shadowVariants() []string { return parseVariants(a.Cfg.Shadow) }

func parseVariants(list string) []string {
	var out []string
	for _, v := range strings.Split(list, ",") {
		if p, err := engine.ParseVariant(v); err == nil {
			out = append(out, p.Name)
		} else if strings.TrimSpace(v) != "" {
			slog.Warn("shadow: ignoring variant", "err", err)
		}
	}
	return out
}

// ShadowStatus is for the dashboard: queued jobs, the turn running now, and jobs done since start.
func (a *Agent) ShadowStatus() (queued int, running, done int64, variants []string) {
	return len(a.sh.live) + len(a.sh.hist), a.sh.running.Load(), a.sh.done.Load(), a.shadowVariants()
}

func (a *Agent) shadowLive(p *config.Person, ch, chat string, turn int64, out turnOut) {
	if out.req == nil || !p.Admin || ch == "console" {
		return
	}
	for _, v := range a.shadowVariants() {
		select {
		case a.sh.live <- shadowJob{turn: turn, source: "live", variant: v, p: p, ch: ch, chat: chat, req: out.req}:
		default:
			slog.Warn("shadow queue full; dropped", "turn", turn, "variant", v)
		}
	}
}

// ShadowHistory queues the admin's last n Claude chat turns for replay on the
// local model, for each given variant (default: the configured ones).
// With sample > 0 it takes that many turns spread evenly across the n, so a
// smaller run still covers every kind of conversation.
// before > 0 limits it to turns up to that id, so later runs replay the same set.
// ids, when given, replays exactly those turns (the eval set) instead.
func (a *Agent) ShadowHistory(n, sample int, before int64, ids []int64, variants []string) (int, error) {
	p := a.admin()
	if p == nil {
		return 0, fmt.Errorf("no admin")
	}
	if len(variants) == 0 {
		variants = a.shadowVariants()
	}
	if len(variants) == 0 {
		variants = []string{"fast"}
	}
	const cols = `SELECT t.id, t.channel, t.chat, t.prompt_hash,
		(SELECT body FROM trace WHERE turn_id=t.id AND kind='prompt' LIMIT 1),
		COALESCE((SELECT images FROM events WHERE turn_id=t.id AND images NOT IN ('','[]','null') LIMIT 1), '[]')
		FROM turns t `
	source := "history"
	var rows *sql.Rows
	var err error
	if len(ids) > 0 {
		source = "eval"
		list := make([]string, len(ids))
		for i, id := range ids {
			list[i] = strconv.FormatInt(id, 10)
		}
		rows, err = a.Store.DB.Query(cols+`WHERE t.person=? AND t.prompt_hash<>'' AND t.id IN (`+strings.Join(list, ",")+`) ORDER BY t.id DESC`, p.ID)
	} else {
		rows, err = a.Store.DB.Query(cols+`WHERE t.person=? AND t.kind='chat' AND t.engine='claude' AND t.status='ok'
		AND t.channel<>'console' AND t.prompt_hash<>'' AND (?=0 OR t.id<=?)
		ORDER BY t.id DESC LIMIT ?`, p.ID, before, before, n)
	}
	if err != nil {
		return 0, err
	}
	type row struct {
		id                 int64
		ch, chat, hash     string
		prompt, imagesJSON *string
	}
	var rs []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.ch, &r.chat, &r.hash, &r.prompt, &r.imagesJSON) == nil {
			rs = append(rs, r)
		}
	}
	rows.Close()
	if sample > 0 && sample < len(rs) {
		picked := make([]row, sample)
		for i := range picked {
			picked[i] = rs[i*len(rs)/sample]
		}
		rs = picked
	}
	queued, gear := 0, a.Local.Model
	for i := len(rs) - 1; i >= 0; i-- { // oldest first, like the conversation went
		r := rs[i]
		sys := a.Store.System(r.hash)
		if sys == "" || r.prompt == nil || *r.prompt == "" {
			continue
		}
		blocks := []engine.Block{{Text: *r.prompt}}
		var imgs []string
		if r.imagesJSON != nil {
			json.Unmarshal([]byte(*r.imagesJSON), &imgs)
		}
		for _, im := range imgs {
			if _, err := os.Stat(im); err == nil {
				blocks = append(blocks, engine.Block{Image: im})
			}
		}
		req := &engine.Request{TurnID: r.id, System: sys, User: blocks}
		for _, v := range variants {
			if a.Store.ShadowDone(r.id, v, shadowRev, gear) {
				continue
			}
			select {
			case a.sh.hist <- shadowJob{turn: r.id, source: source, variant: v, p: p, ch: r.ch, chat: r.chat, req: req}:
				queued++
			default:
				return queued, fmt.Errorf("history queue full after %d", queued)
			}
		}
	}
	return queued, nil
}

func (a *Agent) shadowWorker(ctx context.Context) {
	for {
		var j shadowJob
		select { // live turns first
		case j = <-a.sh.live:
		default:
			select {
			case j = <-a.sh.live:
			case j = <-a.sh.hist:
			case <-ctx.Done():
				return
			}
		}
		if a.mode() != "claude" && j.source == "live" {
			continue // the local model is the real brain right now; don't compete with it
		}
		a.sh.running.Store(j.turn)
		a.runShadow(ctx, j)
		a.sh.running.Store(0)
		a.sh.done.Add(1)
	}
}

type traceItem struct {
	Kind string `json:"k"`
	Name string `json:"n,omitempty"`
	Body string `json:"b,omitempty"`
}

func (a *Agent) runShadow(ctx context.Context, j shadowJob) {
	a.Local.WaitSwitch(ctx) // the model is being swapped
	if !a.Local.Ready(ctx) {
		a.Store.AddShadow(&store.Shadow{TurnID: j.turn, Source: j.source, Variant: j.variant, Rev: shadowRev, Error: "local model not reachable"})
		return
	}
	tc := &tools.Ctx{Person: j.p, Channel: j.ch, Chat: j.chat, TurnID: j.turn, Engine: "local", Replay: true}
	var items []traceItem
	req := *j.req
	req.User = append([]engine.Block(nil), j.req.User...)
	if len(req.User) > 0 && !strings.Contains(req.User[0].Text, localBrainNote) {
		req.User[0].Text = insertAfterLine(req.User[0].Text, "From: ", localBrainNote)
	}
	req.Model, req.Effort = "", ""
	req.Trace = func(kind, name, body string) {
		if len(body) > 2000 {
			body = body[:2000] + "…"
		}
		items = append(items, traceItem{kind, name, body})
	}
	eng := &engine.Local{URL: a.Local.URL, Model: a.Local.Model, Registry: a.Reg,
		ToolCtx: func(int64) *tools.Ctx { return tc }, Variant: j.variant, AnswerTokens: a.Cfg.LocalMaxTokens, SwitchFile: a.Local.SwitchFile}
	x := &store.Shadow{TurnID: j.turn, Source: j.source, Variant: j.variant, Rev: shadowRev}
	if n := len(req.User) - 1; n > 0 {
		req.Trace("init", "images", fmt.Sprint(n))
	}
	x.Gear, x.Cold = a.Local.Model, a.localSleeping(ctx)
	sctx, cancel := context.WithTimeout(ctx, a.Cfg.LocalTimeout)
	start := time.Now()
	res := eng.Turn(sctx, &req)
	cancel()
	x.LatencyMS = time.Since(start).Milliseconds()
	x.Reply, x.InTok, x.OutTok = strings.TrimSpace(res.Text), res.Usage.In, res.Usage.Out
	x.Tools = strings.Join(toolsUsed(res.Tools, tc.Used()), ", ")
	if res.Err != nil {
		x.Error = res.Err.Error()
	}
	tj, _ := json.Marshal(items)
	x.Trace = string(tj)
	a.Store.AddShadow(x)
	slog.Info("shadow", "turn", j.turn, "source", j.source, "variant", j.variant, "gear", x.Gear, "ms", x.LatencyMS, "err", x.Error != "")
}

func insertAfterLine(text, prefix, add string) string {
	i := strings.Index(text, "\n"+prefix)
	if i < 0 {
		return add + text
	}
	nl := strings.Index(text[i+1:], "\n")
	if nl < 0 {
		return text + "\n" + add
	}
	at := i + 1 + nl + 1
	return text[:at] + add + text[at:]
}

// localSleeping asks llama-server whether the model is unloaded (its sleep
// mode), so cold-start latency can be told apart.
func (a *Agent) localSleeping(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(a.Local.URL, "/v1")+"/props", nil)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var p struct {
		Sleeping bool `json:"is_sleeping"`
	}
	json.NewDecoder(resp.Body).Decode(&p)
	return p.Sleeping
}
