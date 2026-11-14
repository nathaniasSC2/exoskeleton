package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/engine"
	"github.com/nathaniasSC2/exoskeleton/store"
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
		cur := lines(a.history(p, id, "discord")) // the turn being answered is not in its own history
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

// A channel with its own caps (a voice call) keeps much more of each long
// message and lets the window grow further while the conversation is there;
// back on a text channel the window shrinks to the default size.
func TestChannelCaps(t *testing.T) {
	ChannelCaps["voice"] = Caps{Input: 3000, Reply: 2500, Chars: 24000, Turns: 16}
	defer delete(ChannelCaps, "voice")
	st, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	p := &config.Person{ID: "alex", Name: "Alex", Admin: true}
	a := &Agent{Cfg: &config.Config{Loc: time.UTC, People: []*config.Person{p}}, Store: st}

	long := strings.Repeat("word ", 400) // 2000 characters, a long spoken thought
	for i := 0; i < 5; i++ {
		id := st.StartTurn("alex", "voice", "call", "chat", fmt.Sprintf("monologue %d %s", i, long))
		st.FinishTurn(&store.Turn{ID: id, Status: "ok", Reply: fmt.Sprintf("answer %d %s", i, long)})
	}
	id := st.StartTurn("alex", "voice", "call", "chat", "and?")
	h := a.history(p, id, "voice")
	if len(h) != 10 {
		t.Fatalf("voice window has %d messages, want all 10", len(h))
	}
	if !strings.Contains(h[0].Text, "monologue 0") || len(h[0].Text) < 1900 || len(h[1].Text) < 1900 {
		t.Fatalf("voice turns clipped: %d / %d characters", len(h[0].Text), len(h[1].Text))
	}
	st.FinishTurn(&store.Turn{ID: id, Status: "ok", Reply: "that's all"})

	// Texting again: the default window, so it jumps, keeping the newest few
	// (still shown whole: they're voice turns).
	id = st.StartTurn("alex", "discord", "c", "chat", "hi")
	h = a.history(p, id, "discord")
	if len(h) != 2*keepAlwaysN {
		t.Fatalf("text window after a call has %d messages, want %d", len(h), 2*keepAlwaysN)
	}
	// A plain text turn is still clipped at the default.
	st.FinishTurn(&store.Turn{ID: id, Status: "ok", Reply: long})
	id = st.StartTurn("alex", "discord", "c", "chat", "ok")
	h = a.history(p, id, "discord")
	if last := h[len(h)-1].Text; len(last) > replyClip+10 {
		t.Fatalf("a text reply kept %d characters, want at most %d", len(last), replyClip)
	}
}
