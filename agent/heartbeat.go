package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
)

// ---- when the heartbeat runs the model ----

// heartbeatWake says why this tick should run the model, or "" when nothing
// changed since last, the start of the last heartbeat that did. Before this,
// every tick ran the full review: eight a day at ~16k tokens even when idle,
// and the heartbeat became the biggest token spender (2026-11-27 audit; the
// weekly self-review had proposed skipping idle ticks twice).
func (a *Agent) heartbeatWake(p *config.Person, last, now time.Time) string {
	loc := a.Cfg.Loc
	n := now.In(loc)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	if last.Before(midnight) {
		return "the first heartbeat today"
	}
	for _, h := range a.Cfg.HeartbeatAlwaysAt {
		at := midnight.Add(time.Duration(h) * time.Hour)
		if !n.Before(at) && last.Before(at) {
			return fmt.Sprintf("the first heartbeat after %d:00", h)
		}
	}
	since := last.Unix()
	var id int64
	var kind string
	// Something the person said, or a text another system sent them, or a
	// logged server change. Notices (scheduled reports, reminders, its own
	// texts) don't count on their own: the next heartbeat still sees them.
	if a.Store.DB.QueryRow(`SELECT id,kind FROM turns WHERE person=? AND kind IN ('chat','external','action') AND status='ok'
		AND channel<>'console' AND started>? ORDER BY id LIMIT 1`, p.ID, since).Scan(&id, &kind) == nil {
		return map[string]string{"chat": "new conversation", "external": "a text from another system", "action": "a logged server change"}[kind]
	}
	// Console jobs are dashboard tests and a deployment's page-only loops.
	if a.Store.DB.QueryRow(`SELECT id FROM jobs WHERE person=? AND channel<>'console' AND status NOT IN ('queued','running')
		AND COALESCE(finished,created)>? ORDER BY id LIMIT 1`, p.ID, since).Scan(&id) == nil {
		return fmt.Sprintf("job #%d finished", id)
	}
	var name string
	if a.Store.DB.QueryRow(`SELECT name FROM runs WHERE exit<>0 AND started>? ORDER BY id LIMIT 1`, since).Scan(&name) == nil {
		return "the scheduled run " + name + " failed"
	}
	if a.Jobs != nil {
		if q := a.Jobs.Quiet(p.ID, 20*time.Minute); q != "" {
			return q
		}
	}
	if r := loopDue(a.Store.Get("loops:"+p.ID), last, now, loc); r != "" {
		return r
	}
	if a.HeartbeatWake != nil {
		if r := a.HeartbeatWake(last, now); r != "" {
			return r
		}
	}
	return ""
}

var (
	// Dates the way open loops get written: "Nov 28", "November 28th",
	// "2026-11-28", "11-28", "11/28".
	loopDateRe  = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b|\b(\d{4})-(\d{1,2})-(\d{1,2})\b|\b(\d{1,2})[/-](\d{1,2})\b`)
	loopTimeRe  = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b|\b([01]?\d|2[0-3]):([0-5]\d)\b`)
	loopDayRe   = regexp.MustCompile(`(?i)\b(mon|tue|tues|wed|thu|thur|thurs|fri|sat|sun)(?:day)?\b`)
	loopTodayRe = regexp.MustCompile(`(?i)\b(today|tonight|this (?:morning|afternoon|evening))\b`)
	monthIdx    = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12}
)

// loopDue finds an open loop that comes due between the last heartbeat that
// ran (last) and the next tick: a line that names today and a time from just
// after last to two hours from now (a heads-up beforehand, a follow-up after),
// or names today with no time and nothing has run since 9:00. Loops are free
// text kept by the heartbeat itself, so this reads them generously; a loop
// that's overdue is seen by the first heartbeat of each day anyway.
func loopDue(loops string, last, now time.Time, loc *time.Location) string {
	n := now.In(loc)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	nine := midnight.Add(9 * time.Hour)
	for _, line := range strings.Split(loops, "\n") {
		if strings.TrimSpace(line) == "" || !namesToday(line, n) {
			continue
		}
		times := loopTimes(line, midnight)
		if len(times) == 0 {
			if !n.Before(nine) && last.Before(nine) {
				return "an open loop is due today: " + oneLine(line, 100)
			}
			continue
		}
		for _, t := range times {
			if t.After(last) && !t.After(n.Add(2*time.Hour+5*time.Minute)) {
				return "an open loop is due around " + t.Format("3:04 PM") + ": " + oneLine(line, 100)
			}
		}
	}
	return ""
}

// namesToday: an explicit date that is today, "today"/"tonight", or today's
// weekday on a line with no other date ("Fri" in "check Fri Dec 4" is not today).
func namesToday(line string, n time.Time) bool {
	dated := false
	for _, m := range loopDateRe.FindAllStringSubmatch(line, -1) {
		var mo, d int
		switch {
		case m[1] != "":
			mo, d = int(monthIdx[strings.ToLower(m[1])]), atoi(m[2])
		case m[3] != "":
			if atoi(m[3]) != n.Year() {
				dated = true
				continue
			}
			mo, d = atoi(m[4]), atoi(m[5])
		default:
			mo, d = atoi(m[6]), atoi(m[7])
		}
		if mo < 1 || mo > 12 || d < 1 || d > 31 {
			continue // "1-3" is a score, not a date
		}
		dated = true
		if time.Month(mo) == n.Month() && d == n.Day() {
			return true
		}
	}
	if loopTodayRe.MatchString(line) {
		return true
	}
	if dated {
		return false
	}
	day := strings.ToLower(n.Weekday().String()[:3])
	for _, m := range loopDayRe.FindAllStringSubmatch(line, -1) {
		if strings.ToLower(m[1])[:3] == day {
			return true
		}
	}
	return false
}

func loopTimes(line string, midnight time.Time) []time.Time {
	var out []time.Time
	for _, m := range loopTimeRe.FindAllStringSubmatch(line, -1) {
		h, min := 0, 0
		if m[3] != "" {
			h, min = atoi(m[1]), atoi(m[2])
			if h < 1 || h > 12 {
				continue
			}
			if strings.EqualFold(m[3], "pm") && h != 12 {
				h += 12
			} else if strings.EqualFold(m[3], "am") && h == 12 {
				h = 0
			}
		} else {
			h, min = atoi(m[4]), atoi(m[5])
		}
		out = append(out, midnight.Add(time.Duration(h)*time.Hour+time.Duration(min)*time.Minute))
	}
	return out
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// ---- what the heartbeat is given ----

// heartbeatDigest is the heartbeat's input: what happened since the last one,
// within BudgetDigest. Interests and open loops are in the system prompt (the
// same text in both places used to be sent twice every tick), and earlier
// days' check-ins come as kind and time only.
func (a *Agent) heartbeatDigest(p *config.Person, since, now time.Time, why string) string {
	loc := a.Cfg.Loc
	head := fmt.Sprintf("Heartbeat at %s. Last heartbeat: %s.", now.In(loc).Format("Mon Jan 2 3:04 PM"), since.In(loc).Format("Mon Jan 2 3:04 PM"))
	if why != "" {
		head += " This one runs because of " + why + "."
	}
	secs := map[string]*section{"head": {lines: []string{head}}}

	conv := &section{title: "Conversation since then (oldest first):", empty: "Conversation since then: none.", min: 1500}
	turns, _ := a.Store.TurnsSince(p.ID, since.Unix(), 120)
	lastMsg := a.Store.Get("heartbeat:msg")
	for _, t := range turns {
		when := time.Unix(t.Started, 0).In(loc).Format("Mon 3:04PM")
		switch t.Kind {
		case "chat":
			conv.lines = append(conv.lines, fmt.Sprintf("[%s %s] %s: %s\n  %s: %s", when, t.Channel, p.Name, oneLine(t.Input, 400), a.Cfg.Name, oneLine(t.Reply, 400)))
		case "external":
			conv.lines = append(conv.lines, fmt.Sprintf("[%s] (sent to %s by another system): %s", when, p.Name, oneLine(t.Reply, 300)))
		case "action":
			conv.lines = append(conv.lines, fmt.Sprintf("[%s] (server change log): %s", when, oneLine(t.Reply, 300)))
		default:
			if lastMsg != "" && strings.TrimSpace(t.Reply) == lastMsg {
				continue // its own text from the last heartbeat; that's below
			}
			conv.lines = append(conv.lines, fmt.Sprintf("[%s] %s (unprompted): %s", when, a.Cfg.Name, oneLine(t.Reply, 300)))
		}
	}
	secs["conv"] = conv

	jobsDone := &section{title: "Background jobs since then:"}
	if rows, err := a.Store.DB.Query(`SELECT id,status,task,result FROM jobs WHERE person=? AND channel<>'console' AND COALESCE(finished,created)>? ORDER BY id LIMIT 10`, p.ID, since.Unix()); err == nil {
		for rows.Next() {
			var id int64
			var st, task, res string
			rows.Scan(&id, &st, &task, &res)
			jobsDone.lines = append(jobsDone.lines, fmt.Sprintf("- #%d %s: %s → %s", id, st, oneLine(task, 120), oneLine(res, 200)))
		}
		rows.Close()
	}
	secs["jobs"] = jobsDone

	failed := &section{title: "Scheduled runs that failed since then:"}
	if rows, err := a.Store.DB.Query(`SELECT name,substr(output,1,200) FROM runs WHERE exit<>0 AND started>? ORDER BY id LIMIT 5`, since.Unix()); err == nil {
		for rows.Next() {
			var n, o string
			rows.Scan(&n, &o)
			failed.lines = append(failed.lines, "- "+n+": "+oneLine(o, 200))
		}
		rows.Close()
	}
	secs["failed"] = failed

	running := &section{title: "Background jobs still running:"}
	if a.Jobs != nil {
		running.lines = a.Jobs.Running(p.ID)
	}
	secs["running"] = running

	// Hourly runs must not repeat themselves: show everything already said unprompted today.
	y, mo, d := now.In(loc).Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, loc).Unix()
	said := &section{title: "Already sent him unprompted today (don't repeat):"}
	if rows, err := a.Store.DB.Query(`SELECT started,reply FROM turns WHERE person=? AND kind='notice' AND channel<>'console' AND started>? ORDER BY id LIMIT 20`, p.ID, midnight); err == nil {
		for rows.Next() {
			var ts int64
			var r string
			rows.Scan(&ts, &r)
			said.lines = append(said.lines, fmt.Sprintf("- %s: %s", time.Unix(ts, 0).In(loc).Format("3:04PM"), oneLine(r, 160)))
		}
		rows.Close()
	}
	secs["said"] = said

	// Its own texts today, apart from the automated reports above, so it can pace check-ins.
	mine := &section{title: "Your own texts to him today, from earlier heartbeats:", empty: "Your own texts to him today, from earlier heartbeats: none yet.", min: 400,
		lines: recentHeartbeatTexts(a.Store, p.ID, midnight, now.Unix()+1, loc, a.Cfg.QuietFrom, a.Cfg.QuietTo)}
	secs["mine"] = mine
	if kinds := recentHeartbeatKinds(a.Store, p.ID, midnight-3*86400, midnight, loc, a.Cfg.QuietFrom, a.Cfg.QuietTo, maxPastKinds); kinds != "" {
		secs["kinds"] = &section{lines: []string{"Your check-ins on the three days before today, kind and time only (vary them): " + kinds}}
	}

	var lists []string
	if strings.TrimSpace(a.Store.Get("interests:"+p.ID)) == "" {
		lists = append(lists, "Interests list: empty; build one from what you know about him.")
	}
	if strings.TrimSpace(a.Store.Get("loops:"+p.ID)) == "" {
		lists = append(lists, "Open loops: none recorded.")
	}
	if len(lists) < 2 {
		lists = append(lists, "The interests list and open loops are in your system prompt.")
	}
	secs["lists"] = &section{lines: []string{strings.Join(lists, " ")}}

	if log := strings.TrimSpace(a.Store.Get("heartbeat:log")); log != "" {
		secs["log"] = &section{title: "Your notes from the last heartbeat:", lines: []string{oneLine(log, 1200)}}
	}

	order := []string{"head", "conv", "jobs", "failed", "running", "said", "mine", "kinds", "lists", "log"}
	trim := []string{"said", "jobs", "running", "failed", "log", "conv", "mine"} // cut first to last
	return fitSections(secs, order, trim, BudgetDigest, "\n\n")
}

const maxPastKinds = 600

// section is one block of the digest. Lines are cut from the front (the
// oldest) when the digest is over budget, down to min characters (min 0: the
// whole section may go). With no lines it shows empty, or nothing.
type section struct {
	title string
	lines []string
	empty string
	min   int
}

func (s *section) text() string {
	if len(s.lines) == 0 {
		return s.empty
	}
	t := strings.Join(s.lines, "\n")
	if s.title != "" {
		t = s.title + "\n" + t
	}
	return t
}

// fitSections joins the sections in order (sep between them, a newline at the
// end), cutting lines from the trim sections (first listed, first cut) until
// the whole fits budget. Nothing to show is "".
func fitSections(secs map[string]*section, order, trim []string, budget int, sep string) string {
	join := func() string {
		var parts []string
		for _, k := range order {
			if s := secs[k]; s != nil {
				if t := s.text(); t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return strings.Join(parts, sep) + "\n"
	}
	out := join()
	for _, k := range trim {
		s := secs[k]
		for s != nil && len(out) > budget && len(s.lines) > 0 {
			over := len(out) - budget
			if s.min > 0 && len(s.text())-len(s.lines[0])-1 < s.min {
				// Dropping the oldest line would take it under its minimum:
				// shorten that line instead, if enough of it would be left.
				if keep := len(s.lines[0]) - over; keep >= 40 || len(s.lines) == 1 {
					s.lines[0] = clipTo(s.lines[0], max(keep, 0))
					out = join()
					break
				}
			}
			s.lines = s.lines[1:]
			out = join()
		}
	}
	if len(out) > budget { // can't happen with sane sections; never send more than the budget
		out = clipTo(out, budget)
	}
	return out
}

// clipTo cuts s to at most n bytes, ending with "…" when cut, never inside a rune.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return ""
	}
	cut := n - len("…")
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// recentHeartbeatKinds lists the heartbeat's texts between from and to as
// "<time> <kind>", oldest first, within max characters (the oldest go first).
// The kind is the reply's KIND line ("move: chin tucks"); replies from before
// it existed show the start of the message.
func recentHeartbeatKinds(st *store.Store, person string, from, to int64, loc *time.Location, quietFrom, quietTo, max int) string {
	rows, err := st.DB.Query(`SELECT started,COALESCE(reply,'') FROM turns WHERE person=? AND kind='heartbeat' AND started>=? AND started<? ORDER BY id`, person, from, to)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var ts int64
		var r string
		if rows.Scan(&ts, &r) != nil {
			continue
		}
		at := time.Unix(ts, 0).In(loc)
		msg, kind, _ := parseHeartbeat(r)
		if msg == "" || inQuiet(at.Hour(), quietFrom, quietTo) {
			continue
		}
		if kind == "" || strings.EqualFold(kind, "none") {
			kind = "text: " + oneLine(msg, 50)
		}
		items = append(items, at.Format("Mon 3PM")+" "+oneLine(kind, 70))
	}
	for len(items) > 0 && len(strings.Join(items, "; ")) > max {
		items = items[1:]
	}
	return strings.Join(items, "; ")
}

var hbLabel = regexp.MustCompile(`\b(MESSAGE|KIND|LOG):`)

// parseHeartbeat reads "MESSAGE: ... KIND: ... LOG: ..." (MESSAGE may be
// SILENT; KIND is the check-in kind, if any). The first of each label counts.
func parseHeartbeat(s string) (msg, kind, log string) {
	s = strings.TrimSpace(s)
	locs := hbLabel.FindAllStringSubmatchIndex(s, -1)
	seen := map[string]bool{}
	var keep [][]int
	for _, l := range locs {
		if label := s[l[2]:l[3]]; !seen[label] {
			seen[label] = true
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 || strings.TrimSpace(s[:keep[0][0]]) != "" {
		end := len(s)
		if len(keep) > 0 {
			end = keep[0][0]
		}
		msg = strings.TrimSpace(s[:end])
	}
	for i, l := range keep {
		end := len(s)
		if i+1 < len(keep) {
			end = keep[i+1][0]
		}
		val := strings.TrimSpace(s[l[1]:end])
		switch s[l[2]:l[3]] {
		case "MESSAGE":
			msg = val
		case "KIND":
			kind = val
		case "LOG":
			log = val
		}
	}
	if strings.EqualFold(strings.Trim(msg, ". "), "SILENT") {
		msg = ""
	}
	return msg, kind, log
}
