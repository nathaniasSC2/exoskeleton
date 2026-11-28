package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
)

func testApp(t *testing.T) *App {
	t.Helper()
	data := t.TempDir()
	scratch := t.TempDir()
	cfg := &config.Config{Brand: config.Brand{Prefix: "ACME", Slug: "acme"}, Name: "Acme", DataDir: data, Loc: time.UTC,
		DashAddr: "127.0.0.1:0", MCPAddr: "127.0.0.1:0", MCPToken: "t", MaxJobs: 1, JobTimeout: time.Minute,
		JobScratch: []string{filepath.Join(scratch, "j{id}")},
		People:     []*config.Person{{ID: "alex", Name: "Alex", Admin: true}}}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Store.DB.Close() })
	return a
}

func write(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(strings.Repeat("{\"type\":\"assistant\"}\n", size/21+1)[:size]), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-age)
	os.Chtimes(path, ts, ts)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPrune(t *testing.T) {
	a := testApp(t)
	day := 24 * time.Hour
	job := func(id int64, status string, ended time.Duration) {
		a.Store.DB.Exec(`INSERT INTO jobs(id,person,channel,chat,task,workdir,status,created,finished) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, "alex", "discord", "c", "t", "/tmp", status, time.Now().Add(-ended-time.Hour).Unix(), time.Now().Add(-ended).Unix())
		for _, ext := range []string{"jsonl", "err", "prompt", "brief"} {
			size := 100
			if ext == "jsonl" {
				size = 50000
			}
			write(t, a.Cfg.Path("jobs", jobName(id, ext)), size, ended)
		}
	}
	job(1, "done", 40*day)   // past 30 days: every file goes
	job(2, "failed", 10*day) // past 7: transcript gzipped
	job(3, "done", 2*day)    // recent: left alone

	// Scratch: job 2's is old and its job ended; job 3's was touched today.
	j2 := strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "2")
	write(t, j2+"/frame.png", 1000, 3*day)
	os.Chtimes(j2, time.Now().Add(-3*day), time.Now().Add(-3*day))
	write(t, strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "3")+"/frame.png", 1000, time.Hour)

	// .bak copies: 5 of schedule.json, 4 of a prompt; 3 newest kept.
	for i := 0; i < 5; i++ {
		write(t, a.Cfg.Path("schedule.json.bak-"+string(rune('a'+i))), 10, time.Duration(i+1)*time.Hour)
	}
	for i := 0; i < 4; i++ {
		write(t, a.Cfg.Path("prompts", "rules.md.bak-"+string(rune('a'+i))), 10, time.Duration(i+1)*time.Hour)
	}
	// A deployment's own rule and kv prefix.
	for i := 0; i < 10; i++ {
		write(t, a.Cfg.Path("work", "weekly", "2026-0"+string(rune('0'+i))+".json"), 10, time.Duration(i+1)*day)
	}
	a.KeepNewest(a.Cfg.Path("work", "weekly", "*.json"), 8, "weekly exports")
	a.PruneKV("pushed:job:", 7)
	a.Store.Set("pushed:job:1", "x")
	a.Store.DB.Exec(`UPDATE kv SET ts=? WHERE k='pushed:job:1'`, time.Now().Add(-8*day).Unix())
	a.Store.Set("pushed:job:2", "x")
	a.Store.DB.Exec(`INSERT INTO shadow(turn_id,source,variant,ts) VALUES(1,'live','fast',?),(2,'live','fast',?)`, time.Now().Add(-40*day).Unix(), time.Now().Unix())
	write(t, a.Cfg.Path("inbox", "old.jpg"), 10, 20*day)
	write(t, a.Cfg.Path("inbox", "new.jpg"), 10, day)

	// Dry run: a report, and nothing changes.
	rs, problems := a.Prune(context.Background(), true)
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	report := FormatPrune(rs, problems, true)
	for _, want := range []string{"1.jsonl", "gzip", "2.jsonl", "/j2", "schedule.json.bak-d", "rules.md.bak-d", "2026-08.json", "shadow rows", "pushed:job:", "old.jpg"} {
		if !strings.Contains(report, want) {
			t.Errorf("dry-run report is missing %q:\n%s", want, report)
		}
	}
	for _, never := range []string{"3.jsonl", "/j3", "schedule.json.bak-c", "new.jpg", "acme.db"} {
		if strings.Contains(report, never) {
			t.Errorf("dry-run report includes %q:\n%s", never, report)
		}
	}
	if !exists(a.Cfg.Path("jobs", "1.jsonl")) || !exists(a.Cfg.Path("schedule.json.bak-e")) {
		t.Fatal("a dry run deleted something")
	}

	// The real run.
	if _, problems := a.Prune(context.Background(), false); len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	checks := map[string]bool{
		a.Cfg.Path("jobs", "1.jsonl"):                        false,
		a.Cfg.Path("jobs", "1.prompt"):                       false,
		a.Cfg.Path("jobs", "2.jsonl"):                        false,
		a.Cfg.Path("jobs", "2.jsonl.gz"):                     true,
		a.Cfg.Path("jobs", "2.prompt"):                       true,
		a.Cfg.Path("jobs", "3.jsonl"):                        true,
		a.Cfg.Path("schedule.json.bak-c"):                    true,
		a.Cfg.Path("schedule.json.bak-d"):                    false,
		a.Cfg.Path("prompts", "rules.md.bak-c"):              true,
		a.Cfg.Path("prompts", "rules.md.bak-d"):              false,
		a.Cfg.Path("work", "weekly", "2026-07.json"):         true,
		a.Cfg.Path("work", "weekly", "2026-08.json"):         false,
		a.Cfg.Path("inbox", "old.jpg"):                       false,
		a.Cfg.Path("inbox", "new.jpg"):                       true,
		strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "2"): false,
		strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "3"): true,
	}
	for p, want := range checks {
		if exists(p) != want {
			t.Errorf("%s exists = %v, want %v", p, !want, want)
		}
	}
	var n int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM shadow`).Scan(&n)
	if n != 1 || a.Store.Get("pushed:job:1") != "" || a.Store.Get("pushed:job:2") == "" {
		t.Fatalf("db prune: shadow rows %d, kv %q %q", n, a.Store.Get("pushed:job:1"), a.Store.Get("pushed:job:2"))
	}

	// A running job's files and scratch are never touched, however old they
	// look, and no scratch is swept while any job runs.
	job(4, "running", 40*day)
	write(t, strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "1")+"/x", 10, 5*day)
	a.Prune(context.Background(), false)
	if !exists(a.Cfg.Path("jobs", "4.jsonl")) || !exists(strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "1")) {
		t.Fatal("pruned around a running job")
	}
}

func TestPruneCap(t *testing.T) {
	a := testApp(t)
	a.Retention.JobCap = 120000
	for id := int64(1); id <= 4; id++ {
		a.Store.DB.Exec(`INSERT INTO jobs(id,person,channel,chat,task,workdir,status,created,finished) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, "alex", "discord", "c", "t", "/tmp", "done", time.Now().Add(-time.Duration(10-id)*time.Hour).Unix(), time.Now().Add(-time.Duration(10-id)*time.Hour).Unix())
		write(t, a.Cfg.Path("jobs", jobName(id, "jsonl")), 50000, time.Duration(10-id)*time.Hour)
	}
	a.Prune(context.Background(), false)
	if exists(a.Cfg.Path("jobs", "1.jsonl")) || exists(a.Cfg.Path("jobs", "2.jsonl")) || !exists(a.Cfg.Path("jobs", "3.jsonl")) || !exists(a.Cfg.Path("jobs", "4.jsonl")) {
		t.Fatal("over the cap, the oldest jobs' files should go first and only until it fits")
	}
}

func TestRemovable(t *testing.T) {
	a := testApp(t)
	for p, want := range map[string]bool{
		a.Cfg.Path("jobs", "1.jsonl"):                        true,
		a.Cfg.DataDir:                                        false,
		a.Cfg.Path("acme.db"):                                false,
		a.Cfg.Path("acme.db-wal"):                            false,
		"/etc/passwd":                                        false,
		filepath.Join(a.Cfg.DataDir, "..", "elsewhere"):      false,
		strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "7"): true,
		strings.ReplaceAll(a.Cfg.JobScratch[0], "{id}", "x"): false,
	} {
		if got := a.removable(p); got != want {
			t.Errorf("removable(%s) = %v, want %v", p, got, want)
		}
	}
}

func TestTempChanges(t *testing.T) {
	a := testApp(t)
	var ran []string
	a.undo = func(_ context.Context, id, cmd string) (string, error) { ran = append(ran, id); return "ok", nil }
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := a.AddTempChange(TempChange{ID: "max-jobs", What: "MAX_JOBS=4", Expires: future, Undo: "sed ...", Auto: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddTempChange(TempChange{ID: "push", What: "holiday push entry", Expires: future}); err != nil {
		t.Fatal(err)
	}
	if a.AddTempChange(TempChange{ID: "max-jobs", What: "again", Expires: future}) == nil {
		t.Fatal("duplicate id accepted")
	}
	if a.AddTempChange(TempChange{ID: "old", What: "x", Expires: "2020-01-01"}) == nil {
		t.Fatal("past expiry accepted")
	}
	if msgs := a.TempCheck(context.Background(), false); len(msgs) != 0 || len(ran) != 0 {
		t.Fatalf("nothing has expired yet: %v %v", msgs, ran)
	}
	// Expire both.
	cs, _ := a.TempChanges()
	for i := range cs {
		cs[i].Expires = time.Now().Add(-time.Minute).Format(time.RFC3339)
	}
	a.saveTemp(cs)
	if msgs := a.TempCheck(context.Background(), true); len(msgs) != 2 || len(ran) != 0 {
		t.Fatalf("dry run: %v %v", msgs, ran)
	}
	msgs := a.TempCheck(context.Background(), false)
	if len(ran) != 1 || ran[0] != "max-jobs" || len(msgs) != 2 {
		t.Fatalf("check: ran %v, msgs %v", ran, msgs)
	}
	cs, _ = a.TempChanges()
	if cs[0].Status != "reverted" || cs[1].Status != "asked" {
		t.Fatalf("statuses: %s %s", cs[0].Status, cs[1].Status)
	}
	// Asked once: the hourly check stays quiet; maintenance reminds.
	if msgs := a.TempCheck(context.Background(), false); len(msgs) != 0 {
		t.Fatalf("asked twice: %v", msgs)
	}
	if msgs := a.TempCheck(context.WithValue(context.Background(), maintKey{}, true), false); len(msgs) != 1 {
		t.Fatalf("maintenance reminder: %v", msgs)
	}
	if err := a.DoneTempChange("push", "removed by hand"); err != nil {
		t.Fatal(err)
	}
}

func TestExpireSchedule(t *testing.T) {
	a := testApp(t)
	later := time.Now().Add(48 * time.Hour).Format("2006-01-02")
	os.WriteFile(a.Cfg.Path("schedule.json"), []byte(`[
  {"name": "keep", "cron": "0 7 * * *", "cmd": "true"},
  {"name": "holiday-push", "cron": "0 * * * *", "cmd": "true", "until": "2026-11-26"},
  {"name": "later", "cron": "0 * * * *", "cmd": "true", "until": "`+later+`"}
]`), 0o644)
	msgs := a.expireSchedule(false)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "holiday-push") {
		t.Fatalf("msgs %v", msgs)
	}
	raw, _ := os.ReadFile(a.Cfg.Path("schedule.json"))
	if strings.Contains(string(raw), "holiday-push") || !strings.Contains(string(raw), "later") || !strings.Contains(string(raw), "\n  {\n    \"name\"") {
		t.Fatalf("schedule.json now:\n%s", raw)
	}
	if baks, _ := filepath.Glob(a.Cfg.Path("schedule.json.bak*")); len(baks) != 1 {
		t.Fatalf("backups: %v", baks)
	}
}

func jobName(id int64, ext string) string { return strings.TrimSpace(itoa(id)) + "." + ext }

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
