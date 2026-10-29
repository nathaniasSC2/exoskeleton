package agent

import (
	"context"
	"strings"
	"testing"
)

func TestActivity(t *testing.T) {
	for used, want := range map[string]string{
		"":                            "still thinking it through",
		"web_search":                  "so far 1 search",
		"web_search,fetch,web_search": "so far 2 searches and 1 page read",
		"notes_read,shell,shell,x":    "so far 1 note read, 2 server checks, and 1 x call",
	} {
		var u []string
		if used != "" {
			u = strings.Split(used, ",")
		}
		if got := activity(u); got != want {
			t.Errorf("activity(%q) = %q, want %q", used, got, want)
		}
	}
}

func TestStopTurn(t *testing.T) {
	a := &Agent{}
	if a.stopTurn("alex") {
		t.Fatal("stopped a turn that isn't running")
	}
	ctx, done := a.trackTurn(context.Background(), "alex")
	if !isStop("Stop!") || isStop("stop the render") {
		t.Fatal("isStop")
	}
	if !a.stopTurn("alex") || ctx.Err() == nil {
		t.Fatal("stop didn't cancel the turn")
	}
	if !done() {
		t.Fatal("done should report the stop")
	}
	if a.stopTurn("alex") {
		t.Fatal("finished turn still stoppable")
	}
}
