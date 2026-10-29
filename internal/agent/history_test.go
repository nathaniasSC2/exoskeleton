package agent

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/config"
	"github.com/nathaniasSC2/exoskeleton/internal/engine"
	"github.com/nathaniasSC2/exoskeleton/internal/store"
)

func lines(h []engine.Msg) []string {
	out := make([]string, len(h))
	for i, m := range h {
		out[i] = m.Role + "|" + m.Text
	}
	return out
}

func isPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The window must be append-only between jumps, so a prompt cache can reuse
// everything before the newest exchange; jumps must be rare.
func TestHistoryIsAppendOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	p := &config.Person{ID: "alex", Name: "Alex", Admin: true}
	a := &Agent{Cfg: &config.Config{Loc: time.UTC, People: []*config.Person{p}}, Store: st}

	var prev []string
	jumps := 0
	for i := 0; i < 60; i++ {
		id := st.StartTurn("alex", "discord", "c", "chat", fmt.Sprintf("message %d", i))
		cur := lines(a.history(p, id)) // the turn being answered is not in its own history
		if prev != nil && !isPrefix(prev, cur) {
			jumps++
		}
		if len(cur) > 2*recentTurns {
			t.Fatalf("turn %d: window has %d messages", i, len(cur))
		}
		st.FinishTurn(&store.Turn{ID: id, Status: "ok", Reply: fmt.Sprintf("reply %d", i)})
		prev = cur
	}
	if jumps == 0 || jumps > 60/(recentTurns/2)+1 {
		t.Fatalf("%d jumps in 60 turns; want a handful (every ~%d turns)", jumps, recentTurns/2)
	}
}
