package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/config"
	"github.com/nathaniasSC2/exoskeleton/internal/engine"
	"github.com/nathaniasSC2/exoskeleton/internal/store"
)

// history is the recent conversation as chat messages, oldest first.
//
// The window is append-only: it starts at a stored turn id and only grows,
// so this turn's history is last turn's history plus the newest exchange. A
// model server that caches prompts (llama.cpp, Claude) can then reuse
// everything up to the newest exchange instead of reprocessing it. When the
// window outgrows its caps (turns, characters, age) it jumps forward once,
// keeping the newest half, rather than sliding a little every turn.
func (a *Agent) history(p *config.Person, turnID int64) []engine.Msg {
	key := "histstart:" + p.ID
	start, _ := strconv.ParseInt(a.Store.Get(key), 10, 64)
	turns, err := a.Store.TurnsFrom(p.ID, start, 400)
	if err != nil {
		return nil
	}
	var ts []*store.Turn
	for _, t := range turns {
		if t.ID != turnID {
			ts = append(ts, t)
		}
	}
	if windowOver(ts) {
		ts = keepNewest(ts)
		if len(ts) > 0 {
			a.Store.Set(key, fmt.Sprint(ts[0].ID))
		}
	} else if len(ts) > 0 && fmt.Sprint(ts[0].ID) != a.Store.Get(key) {
		a.Store.Set(key, fmt.Sprint(ts[0].ID))
	}
	var out []engine.Msg
	for _, t := range ts {
		out = append(out, a.historyMsgs(p, t)...)
	}
	return out
}

func turnChars(t *store.Turn) int {
	return min(len(t.Input), inputClip) + min(len(t.Reply), replyClip) + len(t.Tools) + 40
}

func windowOver(ts []*store.Turn) bool {
	if len(ts) > recentTurns {
		return true
	}
	n := 0
	for _, t := range ts {
		n += turnChars(t)
	}
	if n > recentChars {
		return true
	}
	return len(ts) > keepAlwaysN && time.Since(time.Unix(ts[0].Started, 0)) > recentMaxAge
}

// keepNewest is the jump: the newest half of the caps, the last keepAlwaysN
// turns always included whatever their age.
func keepNewest(ts []*store.Turn) []*store.Turn {
	kept, chars, i := 0, 0, len(ts)
	for i > 0 {
		t := ts[i-1]
		c := turnChars(t)
		if kept >= keepAlwaysN && (kept >= recentTurns/2 || chars+c > recentChars/2 || time.Since(time.Unix(t.Started, 0)) > recentMaxAge) {
			break
		}
		kept++
		chars += c
		i--
	}
	return ts[i:]
}

// historyMsgs renders one turn. Text is what the local model gets as a chat
// message; Line keeps the transcript format the Claude path has always used.
func (a *Agent) historyMsgs(p *config.Person, t *store.Turn) []engine.Msg {
	when := time.Unix(t.Started, 0).In(a.Cfg.Loc).Format("Mon Jan 2 3:04PM")
	switch t.Kind {
	case "chat":
		in, reply := oneLine(t.Input, inputClip), oneLine(t.Reply, replyClip)
		user := engine.Msg{Role: "user", Text: fmt.Sprintf("[%s] %s", when, in), Line: fmt.Sprintf("[%s] %s: %s", when, p.Name, in)}
		me := engine.Msg{Role: "assistant", Text: reply, Line: fmt.Sprintf("[%s] %s: %s", when, a.Cfg.Name, reply)}
		if t.Tools != "" {
			me.Text = reply // no "(used: X)" tag here: the local model imitates it without calling anything
			me.Line = fmt.Sprintf("[%s] (%s used: %s)\n%s", when, a.Cfg.Name, t.Tools, me.Line)
		}
		return []engine.Msg{user, me}
	case "external":
		s := fmt.Sprintf("[%s] (texted to %s by another system, not you): %s", when, p.Name, oneLine(t.Reply, replyClip))
		return []engine.Msg{{Role: "user", Text: s, Line: s}}
	case "action":
		s := fmt.Sprintf("[%s] (server change log): %s", when, oneLine(t.Reply, replyClip))
		return []engine.Msg{{Role: "user", Text: s, Line: s}}
	default:
		s := fmt.Sprintf("[%s] %s (unprompted %s): %s", when, a.Cfg.Name, t.Kind, oneLine(t.Reply, replyClip))
		return []engine.Msg{{Role: "assistant", Text: s, Line: s}}
	}
}

// checkedNote lists the recent replies that were backed by tools, for the local
// model's context. The history it gets has no "(used: X)" tags (it copies them),
// so without this it can't tell an answer it looked up from one it made up, and
// its self-check disowns its own earlier research.
func (a *Agent) checkedNote(p *config.Person, turnID int64) string {
	start, _ := strconv.ParseInt(a.Store.Get("histstart:"+p.ID), 10, 64)
	turns, err := a.Store.TurnsFrom(p.ID, start, 400)
	if err != nil {
		return ""
	}
	var items []string
	for i := len(turns) - 1; i >= 0 && len(items) < 5; i-- {
		t := turns[i]
		if t.ID == turnID || t.Kind != "chat" || t.Tools == "" || t.Status != "ok" {
			continue
		}
		counts := map[string]int{}
		var order []string
		for _, name := range strings.Split(t.Tools, ",") {
			if name = strings.TrimSpace(name); name != "" {
				if counts[name] == 0 {
					order = append(order, name)
				}
				counts[name]++
			}
		}
		var used []string
		for _, name := range order {
			if counts[name] > 1 {
				name = fmt.Sprintf("%s x%d", name, counts[name])
			}
			used = append(used, name)
		}
		when := time.Unix(t.Started, 0).In(a.Cfg.Loc).Format("Mon 3:04PM")
		items = append(items, fmt.Sprintf("%s, about %q: %s", when, oneLine(t.Input, 50), strings.Join(used, ", ")))
	}
	if len(items) == 0 {
		return ""
	}
	// oldest first, so the list reads like the conversation
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return "Your earlier replies in this conversation that were backed by tool lookups at the time (don't doubt them without a new result, but look up anything that changes, like schedules, scores or prices, again when it's asked again): " + strings.Join(items, "; ") + "\n"
}
