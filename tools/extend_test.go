package tools

import (
	"context"
	"strings"
	"testing"
)

func TestAddReplacesAndBranches(t *testing.T) {
	r, alex, _ := testReg(t)
	AddBranch("look", "look-ups", "notes")
	r.Add(
		&Tool{Name: "fetch", Where: "both", Untrusted: true, Branch: "web", Short: "read a page through a real browser",
			Params: map[string]any{"url": str("URL")}, Required: []string{"url"},
			Run: func(context.Context, *Ctx, Args) Result { return Result{Text: "page text"} }},
		&Tool{Name: "scores", Where: "both", Branch: "look", Short: "live scores",
			Run: func(context.Context, *Ctx, Args) Result { return Result{Text: "3-1"} }},
	)
	if n := strings.Count(r.index(&Ctx{Person: alex, Engine: "claude"}), "fetch("); n != 1 {
		t.Fatalf("fetch listed %d times", n)
	}
	idx := r.index(&Ctx{Person: alex, Engine: "claude"})
	if !strings.Contains(idx, "Look-ups:\n  scores()") || strings.Index(idx, "Look-ups") > strings.Index(idx, "Server") {
		t.Fatalf("index:\n%s", idx)
	}
	c := &Ctx{Person: alex, Engine: "claude"}
	res := r.Call(context.Background(), c, "fetch", Args{"url": "https://example.com"})
	if !strings.Contains(res.Text, "<untrusted") || c.TaintedBy() != "fetch" {
		t.Fatalf("an Untrusted tool must fence and taint: %q", res.Text)
	}
}

func TestKitsSetsAndStubs(t *testing.T) {
	r, alex, _ := testReg(t)
	r.Add(
		&Tool{Name: "open_page", Set: "browser", Kits: []string{"general"}, Run: func(context.Context, *Ctx, Args) Result { return Result{} }},
		&Tool{Name: "click", Set: "desktop", Kits: []string{"desktop"}, Run: func(context.Context, *Ctx, Args) Result { return Result{} }},
		&Tool{Name: "switch", Where: "both", SideEffect: true,
			Params:     map[string]any{"action": str("on, off or info")},
			ReplayRuns: func(a Args) bool { return a.S("action") == "info" },
			Stub:       func(_ *Ctx, a Args) string { return "switched " + a.S("action") },
			Run:        func(_ context.Context, _ *Ctx, a Args) Result { return Result{Text: "real " + a.S("action")} }},
	)
	names := func(ts []*Tool) string {
		var s []string
		for _, t := range ts {
			s = append(s, t.Name)
		}
		return strings.Join(s, ",")
	}
	if got := names(r.Toolset(&Ctx{Person: alex, Set: "job"})); got != "send_file,open_page" {
		t.Errorf("general job: %s", got)
	}
	if got := names(r.Toolset(&Ctx{Person: alex, Set: "job", Kit: "desktop"})); got != "send_file,click" {
		t.Errorf("desktop job: %s", got)
	}
	if got := names(r.Exposed(&Ctx{Person: alex, Set: "browser"})); got != "open_page" {
		t.Errorf("browser agent: %s", got)
	}
	c := &Ctx{Person: alex, Engine: "claude", Replay: true}
	if res := r.Call(context.Background(), c, "switch", Args{"action": "off"}); res.Text != "switched off" {
		t.Errorf("stub: %q", res.Text)
	}
	if res := r.Call(context.Background(), c, "switch", Args{"action": "info"}); res.Text != "real info" {
		t.Errorf("read-only action in a replay: %q", res.Text)
	}
}

func TestCtxValueAndClose(t *testing.T) {
	c := &Ctx{}
	n := 0
	tab := func() any { n++; return n }
	if c.Value("tab", tab) != 1 || c.Value("tab", tab) != 1 {
		t.Fatal("Value should create once")
	}
	var order []string
	c.OnDone(func() { order = append(order, "first") })
	c.OnDone(func() { order = append(order, "second") })
	c.Close()
	c.Close()
	if strings.Join(order, ",") != "second,first" {
		t.Fatalf("cleanups ran %v", order)
	}
}
