package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatWake(t *testing.T) {
	a, p := testAgent(t)
	a.Cfg.HeartbeatAlwaysAt = []int{18}
	day := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC) // a Friday
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	if got := a.heartbeatWake(p, at(-4, 0), at(6, 0)); !strings.Contains(got, "first heartbeat today") {
		t.Errorf("first tick of the day: %q", got)
	}
	if got := a.heartbeatWake(p, at(8, 0), at(10, 0)); got != "" {
		t.Errorf("idle 10:00 tick should skip, got %q", got)
	}
	if got := a.heartbeatWake(p, at(16, 0), at(18, 0)); !strings.Contains(got, "18:00") {
		t.Errorf("18:00 tick: %q", got)
	}
	if got := a.heartbeatWake(p, at(18, 0), at(20, 0)); got != "" {
		t.Errorf("20:00 after an 18:00 run should skip, got %q", got)
	}

	// A scheduled report or reminder (a notice) doesn't wake it; a chat does.
	add := func(kind, ch string, ts time.Time) int64 {
		id := a.Store.StartTurn(p.ID, ch, "c", kind, "in")
		a.Store.DB.Exec(`UPDATE turns SET started=?, status='ok', reply='r' WHERE id=?`, ts.Unix(), id)
		return id
	}
	add("notice", "discord", at(10, 30))
	add("chat", "console", at(10, 40)) // a dashboard test
	if got := a.heartbeatWake(p, at(10, 0), at(12, 0)); got != "" {
		t.Errorf("notices and console turns shouldn't wake it, got %q", got)
	}
	add("chat", "discord", at(11, 0))
	if got := a.heartbeatWake(p, at(10, 0), at(12, 0)); got != "new conversation" {
		t.Errorf("chat: %q", got)
	}

	// A finished job wakes it unless it was a console (page-only) job.
	a.Store.DB.Exec(`INSERT INTO jobs(person,channel,chat,task,workdir,status,created,finished) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, "console", "core", "fix", "/tmp", "done", at(12, 0).Unix(), at(13, 0).Unix())
	if got := a.heartbeatWake(p, at(12, 0), at(14, 0)); got != "" {
		t.Errorf("console job shouldn't wake it, got %q", got)
	}
	a.Store.DB.Exec(`INSERT INTO jobs(person,channel,chat,task,workdir,status,created,finished) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, "discord", "c", "build", "/tmp", "done", at(12, 0).Unix(), at(13, 30).Unix())
	if got := a.heartbeatWake(p, at(12, 0), at(14, 0)); !strings.Contains(got, "finished") {
		t.Errorf("job: %q", got)
	}

	// A failed scheduled run.
	a.Store.DB.Exec(`INSERT INTO runs(name,started,exit) VALUES('backup',?,1)`, at(15, 0).Unix())
	if got := a.heartbeatWake(p, at(14, 0), at(16, 0)); !strings.Contains(got, "backup failed") {
		t.Errorf("failed run: %q", got)
	}

	// An open loop due in the next two hours.
	a.Store.Set("loops:"+p.ID, "Waiting on the Jets injury report, Nov 27 4:30pm")
	if got := a.heartbeatWake(p, at(16, 1), at(16, 2)); !strings.Contains(got, "4:30 PM") {
		t.Errorf("loop due: %q", got)
	}

	// The deployment's own reason.
	a.Store.Set("loops:"+p.ID, "")
	a.HeartbeatWake = func(last, now time.Time) string { return "ACME +7%" }
	if got := a.heartbeatWake(p, at(19, 0), at(20, 0)); got != "ACME +7%" {
		t.Errorf("hook: %q", got)
	}
}

func TestLoopDue(t *testing.T) {
	loc := time.UTC
	day := time.Date(2026, 11, 27, 0, 0, 0, 0, loc) // Friday
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	cases := []struct {
		name      string
		loops     string
		last, now time.Time
		due       bool
	}{
		{"time within the next two hours", "Check Friday's report at 2pm", at(10, 0), at(12, 0), true},
		{"time already seen by the last run", "Check the report today 9:30am", at(10, 0), at(12, 0), false},
		{"time passed since the last run", "Game tonight 11:15", at(10, 0), at(12, 0), true},
		{"later today, not yet", "Stream at 8pm today", at(10, 0), at(12, 0), false},
		{"no time, first run after 9", "Pending: suggestions sent 2026-11-27", at(8, 0), at(10, 0), true},
		{"no time, already ran after 9", "Pending: suggestions sent 2026-11-27", at(9, 30), at(12, 0), false},
		{"another day's date beats the weekday", "Ask him Fri Dec 4 10am about the trip", at(8, 0), at(10, 0), false},
		{"tomorrow", "Nov 28 3pm: dentist", at(12, 0), at(14, 0), false},
		{"a score isn't a date", "Jets 1-3, next game Sun", at(8, 0), at(10, 0), false},
		{"today's weekday with a time", "Fri 1pm: patch notes", at(10, 0), at(12, 0), true},
		{"ISO date with a 24h time", "2026-11-27 13:00 release", at(10, 0), at(12, 0), true},
		{"last year's date", "2025-11-27 13:00 release", at(10, 0), at(12, 0), false},
	}
	for _, c := range cases {
		got := loopDue(c.loops, c.last, c.now, loc)
		if (got != "") != c.due {
			t.Errorf("%s: loopDue(%q) = %q, want due=%v", c.name, c.loops, got, c.due)
		}
	}
}

func TestParseHeartbeat(t *testing.T) {
	cases := []struct{ in, msg, kind, log string }{
		{"MESSAGE: Chin tucks, 10 slow reps?\nKIND: move: chin tucks\nLOG: moved him", "Chin tucks, 10 slow reps?", "move: chin tucks", "moved him"},
		{"MESSAGE: SILENT\nKIND: none\nLOG: quiet", "", "none", "quiet"},
		{"MESSAGE: hi\nLOG: x", "hi", "", "x"},
		{"just text", "just text", "", ""},
		{"MESSAGE: a\nLOG: b\nKIND: reset: box breathing", "a", "reset: box breathing", "b"},
	}
	for _, c := range cases {
		m, k, l := parseHeartbeat(c.in)
		if m != c.msg || k != c.kind || l != c.log {
			t.Errorf("parseHeartbeat(%q) = %q, %q, %q", c.in, m, k, l)
		}
	}
}

func TestRecentHeartbeatKinds(t *testing.T) {
	a, p := testAgent(t)
	day := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 30; i++ {
		id := a.Store.StartTurn(p.ID, "discord", "c", "heartbeat", "in")
		reply := fmt.Sprintf("MESSAGE: a long check-in text number %d that goes on and on about stretches and breathing\nKIND: move: doorway chest stretch %d\nLOG: x", i, i)
		if i%3 == 0 {
			reply = "MESSAGE: an old-style text without a kind line, which shows its start\nLOG: x"
		}
		a.Store.DB.Exec(`UPDATE turns SET started=?, reply=? WHERE id=?`, day-3*86400+int64(i)*7200, reply, id)
	}
	got := recentHeartbeatKinds(a.Store, p.ID, day-3*86400, day, time.UTC, 22, 6, maxPastKinds)
	if len(got) > maxPastKinds || got == "" {
		t.Fatalf("kinds are %d chars (max %d): %q", len(got), maxPastKinds, got)
	}
	if strings.Contains(got, "stretches and breathing") || !strings.Contains(got, "move: doorway chest stretch") {
		t.Fatalf("should be kinds, not message text: %q", got)
	}
}

// TestDigestBudget fills everything the digest draws on far past normal and
// checks it still fits BudgetDigest, with the newest conversation kept.
func TestDigestBudget(t *testing.T) {
	a, p := testAgent(t)
	now := time.Now()
	since := now.Add(-6 * time.Hour)
	long := strings.Repeat("all work and no play makes a dull heartbeat ", 30)
	for i := 0; i < 80; i++ {
		id := a.Store.StartTurn(p.ID, "discord", "c", []string{"chat", "notice", "external"}[i%3], fmt.Sprintf("message %d %s", i, long))
		a.Store.DB.Exec(`UPDATE turns SET started=?, status='ok', reply=? WHERE id=?`, since.Unix()+int64(i)*60, fmt.Sprintf("reply %d %s", i, long), id)
	}
	newest := a.Store.StartTurn(p.ID, "discord", "c", "chat", "THE NEWEST MESSAGE")
	a.Store.DB.Exec(`UPDATE turns SET started=?, status='ok', reply='ok' WHERE id=?`, now.Unix()-30, newest)
	for i := 0; i < 15; i++ {
		a.Store.DB.Exec(`INSERT INTO jobs(person,channel,chat,task,workdir,status,created,finished,result) VALUES(?,?,?,?,?,?,?,?,?)`,
			p.ID, "discord", "c", long, "/tmp", "done", now.Unix()-600, now.Unix()-60, long)
		a.Store.DB.Exec(`INSERT INTO runs(name,started,exit,output) VALUES(?,?,1,?)`, fmt.Sprintf("job%d", i), now.Unix()-100, long)
		id := a.Store.StartTurn(p.ID, "discord", "c", "heartbeat", "in")
		a.Store.DB.Exec(`UPDATE turns SET started=?, reply=? WHERE id=?`, now.Unix()-int64(i)*3600-86400, "MESSAGE: "+long+"\nKIND: move: x\nLOG: y", id)
	}
	a.Store.Set("heartbeat:log", long+long)
	a.Store.Set("interests:"+p.ID, "Jets: 1-3")
	a.Store.Set("loops:"+p.ID, "report Friday")

	d := a.heartbeatDigest(p, since, now, "new conversation")
	if len(d) > BudgetDigest {
		t.Fatalf("digest is %d chars, budget %d", len(d), BudgetDigest)
	}
	if !strings.Contains(d, "THE NEWEST MESSAGE") {
		t.Fatalf("the newest conversation was cut:\n%s", d)
	}
	if strings.Contains(d, "Jets: 1-3") || strings.Contains(d, "report Friday") {
		t.Fatal("interests and loops belong in the system prompt, not the digest too")
	}
	if !strings.Contains(d, "Your own texts to him today") {
		t.Fatal("pacing section missing")
	}
}
