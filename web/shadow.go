package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/engine"
	"github.com/nathaniasSC2/exoskeleton/store"
)

// The Shadow page puts Claude's real reply next to what the local model said
// for the same request (see agent/shadow.go), with per-variant/model stats and
// verdict buttons that build the quality bar.

type shadowTurn struct {
	Turn *store.Turn
	Rows []shadowRow
}

type shadowRow struct {
	*store.Shadow
	Steps []shadowStep
}

type shadowStep struct{ K, N, B string }

var verdicts = map[string]bool{"better": true, "same": true, "worse": true, "wrong": true, "": true}

func (s *Server) shadowTurns(r *http.Request, limit int) []shadowTurn {
	src := r.FormValue("source")
	rows, _ := s.Store.Shadows(src, limit)
	var out []shadowTurn
	idx := map[int64]int{}
	for _, x := range rows {
		if v := r.FormValue("variant"); v != "" && x.Variant != v {
			continue
		}
		if r.FormValue("unjudged") != "" && x.Verdict != "" {
			continue
		}
		i, ok := idx[x.TurnID]
		if !ok {
			t, err := s.Store.Turn(x.TurnID)
			if err != nil || s.private(t.Person) {
				continue
			}
			out = append(out, shadowTurn{Turn: t})
			i = len(out) - 1
			idx[x.TurnID] = i
		}
		var items []struct{ K, N, B string }
		json.Unmarshal([]byte(x.Trace), &items)
		row := shadowRow{Shadow: x}
		for _, it := range items {
			if it.K == "tool_call" || it.K == "tool_result" || it.K == "retry" || it.K == "error" {
				row.Steps = append(row.Steps, shadowStep(it))
			}
		}
		out[i].Rows = append(out[i].Rows, row)
	}
	return out
}

func (s *Server) shadow(w http.ResponseWriter, r *http.Request) {
	queued, running, done, variants := s.Agent.ShadowStatus()
	s.Render(w, "shadow", map[string]any{
		"Stats": s.Store.ShadowStats(), "Turns": s.shadowTurns(r, 240),
		"Queued": queued, "Running": running, "Done": done, "Variants": strings.Join(variants, ", "),
		"Source": r.FormValue("source"), "Variant": r.FormValue("variant"), "Unjudged": r.FormValue("unjudged") != "",
	})
}

func (s *Server) shadowVerdict(w http.ResponseWriter, r *http.Request) {
	v := r.FormValue("v")
	if !verdicts[v] {
		http.Error(w, "bad verdict", 400)
		return
	}
	s.Store.SetShadowVerdict(id(r), v, strings.TrimSpace(r.FormValue("note")))
	back := r.Header.Get("Referer")
	if back == "" {
		back = "/shadow"
	}
	http.Redirect(w, r, back+fmt.Sprintf("#s%d", id(r)), http.StatusSeeOther)
}

// shadowHistory queues past Claude turns for replay on the local model.
func (s *Server) shadowHistory(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.FormValue("n"))
	n = min(max(n, 1), 500)
	var variants []string
	for _, v := range strings.Split(r.FormValue("variants"), ",") {
		if p, err := engine.ParseVariant(v); err == nil {
			variants = append(variants, p.Name)
		}
	}
	sample, _ := strconv.Atoi(r.FormValue("sample"))
	before, _ := strconv.ParseInt(r.FormValue("before"), 10, 64)
	var ids []int64
	for _, f := range strings.Split(r.FormValue("ids"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	q, err := s.Agent.ShadowHistory(n, sample, before, ids, variants)
	if err != nil && q == 0 {
		http.Error(w, err.Error(), 400)
		return
	}
	if r.FormValue("api") != "" {
		fmt.Fprintf(w, "queued %d\n", q)
		return
	}
	http.Redirect(w, r, "/shadow?source=history", http.StatusSeeOther)
}

// evalPost runs fixed test prompts on the current brain (Claude while the plan
// lasts) in the background; their turn ids are kept in kv eval:ids for shadow
// replays (POST /shadow/history ids=...).
func (s *Server) evalPost(w http.ResponseWriter, r *http.Request) {
	var prompts []string
	if err := json.Unmarshal([]byte(r.FormValue("prompts")), &prompts); err != nil || len(prompts) == 0 || len(prompts) > 40 {
		http.Error(w, "prompts must be a JSON list of 1-40 strings", 400)
		return
	}
	go func() {
		var ids []string
		for i, p := range prompts {
			s.Store.Set("eval:status", fmt.Sprintf("running %d/%d", i+1, len(prompts)))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			t, err := s.Agent.Eval(ctx, s.Agent.Cfg.People[0].ID, p)
			cancel()
			if err == nil && t != nil {
				ids = append(ids, strconv.FormatInt(t.ID, 10))
				s.Store.Set("eval:ids", strings.Join(ids, ","))
			}
		}
		s.Store.Set("eval:status", fmt.Sprintf("done %d", len(ids)))
	}()
	fmt.Fprintf(w, "started %d prompts\n", len(prompts))
}

func (s *Server) evalGet(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "%s\n%s\n", s.Store.Get("eval:status"), s.Store.Get("eval:ids"))
}

// apiShadow is the same comparison as JSON (for offline analysis).
func (s *Server) apiShadow(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Turn                                int64
		Channel, Input, Claude, ClaudeTools string
		ClaudeMS, ClaudeIn                  int64
		Shadows                             []*store.Shadow
	}
	var out []row
	for _, t := range s.shadowTurns(r, 2000) {
		x := row{Turn: t.Turn.ID, Channel: t.Turn.Channel, Input: t.Turn.Input, Claude: t.Turn.Reply, ClaudeTools: t.Turn.Tools,
			ClaudeMS: t.Turn.LatencyMS, ClaudeIn: t.Turn.InTok + t.Turn.CacheRead + t.Turn.CacheWrite}
		for _, r := range t.Rows {
			x.Shadows = append(x.Shadows, r.Shadow)
		}
		out = append(out, x)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"stats": s.Store.ShadowStats(), "turns": out})
}
