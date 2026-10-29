package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/sched"
)

func TestAppRuns(t *testing.T) {
	cfg := &config.Config{Brand: config.Brand{Prefix: "ACME", Slug: "acme"}, Name: "Acme", DataDir: t.TempDir(), Loc: time.UTC,
		DashAddr: "127.0.0.1:0", MCPAddr: "127.0.0.1:0", MCPToken: "t", MaxJobs: 1, JobTimeout: time.Minute,
		People: []*config.Person{{ID: "alex", Name: "Alex", Admin: true}}}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	a.OnStart(func(context.Context) error { ran = true; return nil })
	a.AddBuiltin(sched.Entry{Name: "tidy", Cron: "0 4 * * *"}, func(context.Context) (string, error) { return "", nil })
	a.AddCheck(func(context.Context) string { return "disk almost full" })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("OnStart didn't run")
	}
	var names []string
	for _, e := range a.Sched.Entries() {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "tidy") || !strings.Contains(got, "maintenance") {
		t.Fatalf("schedule = %s", got)
	}
	out, _ := a.maintenance(context.Background())
	if !strings.Contains(out, "disk almost full") {
		t.Fatalf("maintenance = %q", out)
	}
	var b bytes.Buffer
	a.Budget(&b)
	if !strings.Contains(b.String(), "alex system prompt") {
		t.Fatalf("budget = %s", b.String())
	}
}
