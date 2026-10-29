// Package web is the dashboard. Server-rendered pages, every query LIMITed,
// no websockets, no in-memory history: the browser polls one small JSON
// endpoint and the server holds nothing per client.
package web

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nathaniasSC2/exoskeleton/agent"
	"github.com/nathaniasSC2/exoskeleton/channel"
	"github.com/nathaniasSC2/exoskeleton/engine"
	"github.com/nathaniasSC2/exoskeleton/jobs"
	"github.com/nathaniasSC2/exoskeleton/sched"
	"github.com/nathaniasSC2/exoskeleton/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	Agent   *agent.Agent
	Store   *store.Store
	Sched   *sched.Scheduler
	Senders map[string]channel.Sender
	Started time.Time

	// Additions from a deployment built on exo (all optional).
	Nav   []Link                   // more header links
	Pages fs.FS                    // more templates (*.html at its root), parsed with exo's so they can use head and foot
	Funcs template.FuncMap         // template functions those pages need
	Mount func(mux *http.ServeMux) // more routes

	tpl       *template.Template
	replaying atomic.Bool
}

// Link is a header link. Pages lists the page names that highlight it;
// First puts it right after Live instead of at the end.
type Link struct {
	Href, Label, Title string
	Pages              []string
	First              bool
}

func (s *Server) Handler() http.Handler {
	loc := s.Agent.Cfg.Loc
	funcs := template.FuncMap{
		"ts": func(t int64) string {
			if t == 0 {
				return "—"
			}
			tt := time.Unix(t, 0).In(loc)
			if time.Since(tt) < 20*time.Hour {
				return tt.Format("3:04:05 PM")
			}
			return tt.Format("Jan 2 3:04 PM")
		},
		"clip": func(s string, n int) string {
			s = strings.Join(strings.Fields(s), " ")
			if len(s) > n {
				return s[:n] + "…"
			}
			return s
		},
		"secs": func(ms int64) string {
			if ms == 0 {
				return "—"
			}
			return fmt.Sprintf("%.1fs", float64(ms)/1000)
		},
		"k": func(n int64) string {
			if n >= 1000 {
				return fmt.Sprintf("%.1fk", float64(n)/1000)
			}
			return strconv.FormatInt(n, 10)
		},
		"addTok": func(a, b, c int64) int64 { return a + b + c },
		"uptime": func(d time.Duration) string { return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24) },
		"since":  sinceStr,
		"pctw":   func(f float64) int { return int(f*100 + 0.5) },
		"pct":    func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"usd":    func(f float64) string { return fmt.Sprintf("$%.3f", f) },
		"lines":  func(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") },
		"until": func(t int64) string {
			if t == 0 {
				return ""
			}
			return time.Unix(t, 0).In(loc).Format("Mon 3:04 PM")
		},
		"has": func(xs []string, x string) bool { return slices.Contains(xs, x) },
	}
	for k, f := range s.Funcs {
		funcs[k] = f
	}
	s.tpl = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html"))
	if s.Pages != nil {
		s.tpl = template.Must(s.tpl.ParseFS(s.Pages, "*.html"))
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", s.live)
	mux.HandleFunc("GET /api/turns", s.apiTurns)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /turn/{id}", s.turn)
	mux.HandleFunc("POST /turn/{id}/feedback", s.feedback)
	mux.HandleFunc("GET /jobs", s.jobs)
	mux.HandleFunc("GET /job/{id}", s.job)
	mux.HandleFunc("POST /job/{id}/cancel", s.cancelJob)
	mux.HandleFunc("GET /schedule", s.schedule)
	mux.HandleFunc("POST /schedule/{name}/run", s.runNow)
	mux.HandleFunc("GET /memory", s.memory)
	mux.HandleFunc("POST /memory", s.memoryPost)
	mux.HandleFunc("GET /prompts", s.prompts)
	mux.HandleFunc("POST /prompts", s.promptsPost)
	mux.HandleFunc("GET /console", s.console)
	mux.HandleFunc("POST /console", s.consolePost)
	mux.HandleFunc("GET /replay", s.replay)
	mux.HandleFunc("POST /replay", s.replayPost)
	mux.HandleFunc("GET /shadow", s.shadow)
	mux.HandleFunc("POST /shadow/history", s.shadowHistory)
	mux.HandleFunc("POST /shadow/eval", s.evalPost)
	mux.HandleFunc("GET /shadow/eval", s.evalGet)
	mux.HandleFunc("POST /shadow/{id}/verdict", s.shadowVerdict)
	mux.HandleFunc("GET /api/shadow", s.apiShadow)
	mux.HandleFunc("POST /mode", s.modePost)
	if s.Mount != nil {
		s.Mount(mux)
	}
	return logRequests(mux)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("dashboard panic", "path", r.URL.Path, "err", p)
				http.Error(w, "internal error", 500)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// Render executes a page template with the header's data (Status, Page, Nav).
func (s *Server) Render(w http.ResponseWriter, name string, data map[string]any) {
	data["Status"] = s.status()
	data["Nav"] = s.Nav
	if _, ok := data["Page"]; !ok {
		data["Page"] = name
	}
	var buf bytes.Buffer // render fully first so a template bug is a visible 500, not a blank page
	if err := s.tpl.ExecuteTemplate(&buf, name+".html", data); err != nil {
		slog.Error("template", "page", name, "err", err)
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func id(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return n
}

// ---- status header ----

type Status struct {
	Name, Mode, Down string
	Limit            engine.Limit
	Channels         []ChanStatus
	DryRun, Schedule bool
	Failover         string
	TurnsToday       int
	Errors           int
	P50, P90         int64
	AvgIn            int64
	CostToday        float64
	JobsRunning      int
	Uptime           string
}

type ChanStatus struct {
	Name string
	OK   bool
}

// StatusNow is the header's data: brain, channels, usage, today's numbers.
func (s *Server) StatusNow() Status { return s.status() }

func (s *Server) status() Status {
	cfg := s.Agent.Cfg
	st := Status{Name: cfg.Name, Mode: s.Agent.Mode(), Down: s.Store.Get("claude_down"), DryRun: cfg.DryRun,
		Schedule: cfg.ScheduleEnabled, Failover: cfg.Failover, Uptime: time.Since(s.Started).Round(time.Minute).String()}
	json.Unmarshal([]byte(s.Store.Get("limit")), &st.Limit)
	var names []string
	for n := range s.Senders {
		if n != "console" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		st.Channels = append(st.Channels, ChanStatus{n, s.Senders[n].Healthy()})
	}
	y, m, d := time.Now().In(cfg.Loc).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cfg.Loc)
	rows, err := s.Store.DB.Query(`SELECT COALESCE(latency_ms,0), COALESCE(in_tok,0)+COALESCE(cache_read,0)+COALESCE(cache_write,0), COALESCE(cost,0), status
		FROM turns WHERE kind='chat' AND started>=? LIMIT 2000`, midnight.Unix())
	if err == nil {
		var lat []int64
		var in int64
		for rows.Next() {
			var l, t int64
			var c float64
			var stt string
			rows.Scan(&l, &t, &c, &stt)
			st.TurnsToday++
			if stt != "ok" && stt != "running" {
				st.Errors++
			}
			lat = append(lat, l)
			in += t
			st.CostToday += c
		}
		rows.Close()
		if len(lat) > 0 {
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			st.P50, st.P90 = lat[len(lat)/2], lat[len(lat)*9/10]
			st.AvgIn = in / int64(len(lat))
		}
	}
	s.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&st.JobsRunning)
	return st
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.status())
}

// ---- live ----

func (s *Server) filter(r *http.Request) store.TurnFilter {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	return store.TurnFilter{Person: q.Get("person"), Kind: q.Get("kind"), Feedback: q.Get("feedback") == "1",
		Errors: q.Get("errors") == "1", BeforeID: before, AfterID: after, Limit: 50}
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	f := s.filter(r)
	turns, _ := s.Store.Turns(f)
	turns = s.redact(turns)
	var older int64
	if len(turns) == 50 {
		older = turns[len(turns)-1].ID
	}
	s.Render(w, "live", map[string]any{"Turns": turns, "F": f, "Older": older, "Q": r.URL.RawQuery, "People": s.Agent.Cfg.People})
}

// apiTurns returns rows newer than ?after= as rendered HTML fragments.
func (s *Server) apiTurns(w http.ResponseWriter, r *http.Request) {
	f := s.filter(r)
	if f.AfterID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	turns, _ := s.Store.Turns(f)
	turns = s.redact(turns)
	var out []map[string]any
	for _, t := range turns {
		var b strings.Builder
		s.tpl.ExecuteTemplate(&b, "row", t)
		out = append(out, map[string]any{"id": t.ID, "html": b.String()})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) turn(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Turn(id(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.private(t.Person) {
		s.Render(w, "turn", map[string]any{"T": s.redact([]*store.Turn{t})[0], "Private": true})
		return
	}
	trace, _ := s.Store.TraceRows(t.ID)
	s.Render(w, "turn", map[string]any{"T": t, "Trace": trace, "System": s.Store.System(t.PromptHash)})
}

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	score, _ := strconv.Atoi(r.FormValue("score"))
	s.Store.SetFeedback(id(r), score, strings.TrimSpace(r.FormValue("note")))
	http.Redirect(w, r, fmt.Sprintf("/turn/%d", id(r)), http.StatusSeeOther)
}

// ---- jobs ----

type JobRow struct {
	ID                            int64
	Person, Task, Workdir, Status string
	Result, LogPath               string
	Created, Started, Finished    int64
	Cost                          float64
}

func (s *Server) jobRows(where string, args ...any) []JobRow {
	rows, err := s.Store.DB.Query(`SELECT id,person,task,workdir,status,result,log_path,created,COALESCE(started,0),COALESCE(finished,0),COALESCE(cost,0)
		FROM jobs `+where+` ORDER BY id DESC LIMIT 100`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var j JobRow
		rows.Scan(&j.ID, &j.Person, &j.Task, &j.Workdir, &j.Status, &j.Result, &j.LogPath, &j.Created, &j.Started, &j.Finished, &j.Cost)
		out = append(out, j)
	}
	return out
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	s.Render(w, "jobs", map[string]any{"Jobs": s.jobRows("")})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	js := s.jobRows("WHERE id=?", id(r))
	if len(js) == 0 {
		http.NotFound(w, r)
		return
	}
	s.Render(w, "job", map[string]any{"J": js[0], "Log": jobs.Tail(js[0].LogPath, 60)})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	js := s.jobRows("WHERE id=?", id(r))
	if len(js) == 1 {
		s.Agent.Jobs.Cancel(js[0].Person, js[0].ID)
	}
	http.Redirect(w, r, fmt.Sprintf("/job/%d", id(r)), http.StatusSeeOther)
}

// ---- schedule ----

type RunRow struct {
	Name, Output    string
	Started, Ended  int64
	Exit, Delivered int
}

func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	type E struct {
		sched.Entry
		Next int64
	}
	var es []E
	for _, e := range s.Sched.Entries() {
		n := s.Sched.Next(e.Name)
		es = append(es, E{e, n.Unix()})
	}
	var runs []RunRow
	rows, err := s.Store.DB.Query(`SELECT name,output,started,COALESCE(finished,0),COALESCE(exit,0),delivered FROM runs ORDER BY id DESC LIMIT 60`)
	if err == nil {
		for rows.Next() {
			var x RunRow
			rows.Scan(&x.Name, &x.Output, &x.Started, &x.Ended, &x.Exit, &x.Delivered)
			runs = append(runs, x)
		}
		rows.Close()
	}
	s.Render(w, "schedule", map[string]any{"Entries": es, "Runs": runs})
}

func (s *Server) runNow(w http.ResponseWriter, r *http.Request) {
	s.Sched.RunNow(r.PathValue("name"))
	http.Redirect(w, r, "/schedule", http.StatusSeeOther)
}

// ---- memory ----

func (s *Server) person(r *http.Request) string {
	p := r.FormValue("person")
	if s.Agent.Cfg.Person(p) == nil {
		p = s.defaultPerson()
	}
	return p
}

// defaultPerson is the first admin (or the first person) for dashboard pages.
func (s *Server) defaultPerson() string {
	for _, p := range s.Agent.Cfg.People {
		if p.Admin {
			return p.ID
		}
	}
	if len(s.Agent.Cfg.People) > 0 {
		return s.Agent.Cfg.People[0].ID
	}
	return ""
}

func (s *Server) memory(w http.ResponseWriter, r *http.Request) {
	p := s.person(r)
	facts, _ := s.Store.Facts(p, false)
	if s.private(p) {
		s.Render(w, "memory", map[string]any{"Person": p, "People": s.Agent.Cfg.People, "PrivateCount": len(facts)})
		return
	}
	s.Render(w, "memory", map[string]any{"Person": p, "People": s.Agent.Cfg.People, "Facts": facts})
}

func (s *Server) memoryPost(w http.ResponseWriter, r *http.Request) {
	p := s.person(r)
	fid, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	switch r.FormValue("op") {
	case "add":
		s.Store.AddFact(p, r.FormValue("text"), r.FormValue("pinned") == "1", "dashboard")
	case "delete":
		s.Store.DeleteFact(p, fid)
	case "pin":
		s.Store.PinFact(fid, true)
	case "unpin":
		s.Store.PinFact(fid, false)
	}
	http.Redirect(w, r, "/memory?person="+p, http.StatusSeeOther)
}

// ---- prompts ----

func (s *Server) prompts(w http.ResponseWriter, r *http.Request) {
	names := agent.PromptNames()
	name := r.FormValue("name")
	if name == "" {
		name = names[0]
	}
	content := s.Agent.ReadPrompt(name)
	if vid, _ := strconv.ParseInt(r.FormValue("version"), 10, 64); vid > 0 {
		if v, err := s.Store.PromptVersion(vid); err == nil && v.Name == name {
			content = v.Content
		}
	}
	versions, _ := s.Store.PromptVersions(name, 30)
	s.Render(w, "prompts", map[string]any{"Names": names, "Name": name, "Content": content, "Versions": versions,
		"Tokens": len(content) / 4, "Saved": r.FormValue("saved") == "1"})
}

func (s *Server) promptsPost(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if err := s.Agent.SavePrompt(name, r.FormValue("content")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/prompts?saved=1&name="+name, http.StatusSeeOther)
}

// ---- console ----

func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	turns, _ := s.Store.Turns(store.TurnFilter{Limit: 20})
	turns = s.redact(turns)
	var mine []*store.Turn
	for _, t := range turns {
		if t.Channel == "console" && t.Kind != "replay" {
			mine = append(mine, t)
		}
	}
	s.Render(w, "console", map[string]any{"People": s.Agent.Cfg.People, "Turns": mine})
}

func (s *Server) consolePost(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(25 << 20)
	text, images := s.Upload(r, "console")
	if text != "" || len(images) > 0 {
		s.Agent.Console(s.person(r), text, images...)
	}
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func (s *Server) modePost(w http.ResponseWriter, r *http.Request) {
	switch r.FormValue("mode") {
	case "local":
		go s.Agent.GoLocal(context.Background(), "dashboard")
	case "claude":
		go s.Agent.GoClaude(context.Background(), "")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- replay ----

type ReplayRow struct {
	Idx                           int
	Input, Baseline, Reply, Tools string
	Engine, Error                 string
	LatencyMS, InTok              int64
}

type ReplayRun struct {
	Run   string
	N     int
	Avg   int64
	AvgIn int64
	When  int64
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	var runs []ReplayRun
	rows, err := s.Store.DB.Query(`SELECT run, COUNT(*), CAST(AVG(latency_ms) AS INT), CAST(AVG(in_tok) AS INT), MAX(ts) FROM replay GROUP BY run ORDER BY MAX(ts) DESC LIMIT 20`)
	if err == nil {
		for rows.Next() {
			var x ReplayRun
			rows.Scan(&x.Run, &x.N, &x.Avg, &x.AvgIn, &x.When)
			runs = append(runs, x)
		}
		rows.Close()
	}
	run := r.FormValue("run")
	var res []ReplayRow
	if run != "" {
		rows, err := s.Store.DB.Query(`SELECT idx,input,baseline,reply,tools,engine,error,COALESCE(latency_ms,0),COALESCE(in_tok,0) FROM replay WHERE run=? ORDER BY idx LIMIT 200`, run)
		if err == nil {
			for rows.Next() {
				var x ReplayRow
				rows.Scan(&x.Idx, &x.Input, &x.Baseline, &x.Reply, &x.Tools, &x.Engine, &x.Error, &x.LatencyMS, &x.InTok)
				res = append(res, x)
			}
			rows.Close()
		}
	}
	_, haveSet := os.Stat(s.Agent.Cfg.Path("replay", "set.jsonl"))
	s.Render(w, "replay", map[string]any{"Runs": runs, "Run": run, "Results": res, "Busy": s.replaying.Load(), "HaveSet": haveSet == nil})
}

type sample struct {
	Text     string `json:"text"`
	Baseline string `json:"baseline"`
	Person   string `json:"person"`
}

func (s *Server) replayPost(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.FormValue("n"))
	n = min(max(n, 1), 60)
	var samples []sample
	switch r.FormValue("source") {
	case "feedback":
		turns, _ := s.Store.Turns(store.TurnFilter{Feedback: true, Limit: 200})
		for _, t := range turns {
			if t.Feedback < 0 && t.Input != "" {
				samples = append(samples, sample{t.Input, t.Reply, t.Person})
			}
		}
	default:
		f, err := os.Open(s.Agent.Cfg.Path("replay", "set.jsonl"))
		if err != nil {
			http.Error(w, "no replay set: <data>/replay/set.jsonl ({text, baseline, person} per line) is missing; use the feedback source", 400)
			return
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var x sample
			if json.Unmarshal(sc.Bytes(), &x) == nil && x.Text != "" {
				samples = append(samples, x)
			}
		}
		f.Close()
		rand.Shuffle(len(samples), func(i, j int) { samples[i], samples[j] = samples[j], samples[i] })
	}
	if len(samples) > n {
		samples = samples[:n]
	}
	if len(samples) == 0 || !s.replaying.CompareAndSwap(false, true) {
		http.Redirect(w, r, "/replay", http.StatusSeeOther)
		return
	}
	run := fmt.Sprintf("%s-%s", time.Now().In(s.Agent.Cfg.Loc).Format("0102-1504"), s.Agent.Mode())
	go func() {
		defer s.replaying.Store(false)
		for i, x := range samples {
			p := x.Person
			if p == "" {
				p = s.defaultPerson()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			t, err := s.Agent.Replay(ctx, p, x.Text)
			cancel()
			row := ReplayRow{Idx: i, Input: x.Text, Baseline: x.Baseline}
			if err != nil {
				row.Error = err.Error()
			} else {
				row.Reply, row.Tools, row.Engine, row.Error, row.LatencyMS = t.Reply, t.Tools, t.Engine, t.Error, t.LatencyMS
				row.InTok = t.InTok + t.CacheRead + t.CacheWrite
			}
			s.Store.DB.Exec(`INSERT INTO replay(run,idx,input,baseline,engine,reply,latency_ms,in_tok,tools,error,ts) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				run, row.Idx, row.Input, row.Baseline, row.Engine, row.Reply, row.LatencyMS, row.InTok, row.Tools, row.Error, time.Now().Unix())
		}
	}()
	http.Redirect(w, r, "/replay?run="+run, http.StatusSeeOther)
}

func sinceStr(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// private reports whether a person's conversations are hidden from the
// dashboard (everyone but admins). Their chats are theirs: the dashboard
// shows that they happened and whether anything broke, never what was said.
func (s *Server) private(person string) bool {
	p := s.Agent.Cfg.Person(person)
	return p != nil && !p.Admin
}

func (s *Server) redact(ts []*store.Turn) []*store.Turn {
	out := make([]*store.Turn, 0, len(ts))
	for _, t := range ts {
		if s.private(t.Person) {
			c := *t
			c.Input, c.Reply, c.Attachments, c.FeedbackNote = "", "🔒 private conversation", "", ""
			if c.Tools != "" {
				c.Tools = "(tools used)"
			}
			t = &c
		}
		out = append(out, t)
	}
	return out
}
