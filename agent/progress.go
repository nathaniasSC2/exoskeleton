package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/tools"
)

// A slow turn keeps the person posted, and they can stop it. Past Cfg.CheckIn
// a chat turn sends what it's been doing (people start wondering around 5
// minutes); "stop" while a turn runs cancels it cleanly. Long jobs post
// their own progress (jobs.go).

type runningTurn struct {
	cancel  context.CancelFunc
	stopped atomic.Bool
}

// trackTurn makes a person's chat turn stoppable. done untracks it and
// reports whether it was stopped.
func (a *Agent) trackTurn(ctx context.Context, person string) (tctx context.Context, done func() (stopped bool)) {
	tctx, cancel := context.WithCancel(ctx)
	rt := &runningTurn{cancel: cancel}
	a.turns.Store(person, rt)
	return tctx, func() bool {
		a.turns.CompareAndDelete(person, rt)
		cancel()
		return rt.stopped.Load()
	}
}

// stopTurn cancels the person's running chat turn, if there is one.
func (a *Agent) stopTurn(person string) bool {
	v, ok := a.turns.Load(person)
	if !ok {
		return false
	}
	rt := v.(*runningTurn)
	rt.stopped.Store(true)
	rt.cancel()
	return true
}

var stopWords = map[string]bool{"stop": true, "cancel": true, "stop that": true, "cancel that": true,
	"nvm": true, "never mind": true, "nevermind": true, "forget it": true}

func isStop(text string) bool {
	return stopWords[strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!"))]
}

const stoppedReply = "Stopped. Send it again, or a narrower version, whenever you want."

// checkIn texts a progress note if the turn is still running after Cfg.CheckIn.
func (a *Agent) checkIn(p *config.Person, ch, chat string, tc *tools.Ctx, start time.Time) (stop func()) {
	if a.Cfg.CheckIn <= 0 {
		return func() {}
	}
	t := time.AfterFunc(a.Cfg.CheckIn, func() {
		a.notify(p.ID, ch, chat, "system", fmt.Sprintf("Still on it (%d min in): %s. I'll text you when I have it; say \"stop\" to cancel.",
			int(time.Since(start).Minutes()+0.5), activity(tc.Used())), nil)
	})
	return func() { t.Stop() }
}

// ActivityNames words tool use for a progress note: tool -> {one, many}.
var ActivityNames = map[string][2]string{
	"web_search": {"search", "searches"}, "fetch": {"page read", "pages read"}, "WebSearch": {"search", "searches"}, "WebFetch": {"page read", "pages read"},
	"shell": {"server check", "server checks"}, "notes_read": {"note read", "notes read"}, "notes_search": {"note search", "note searches"},
	"recall": {"memory lookup", "memory lookups"}, "jobs": {"job check", "job checks"},
}

// activity sums up a turn's tool use so far in plain words.
func activity(used []string) string {
	var order [][2]string
	count := map[[2]string]int{}
	for _, u := range used {
		n, ok := ActivityNames[u]
		if !ok {
			n = [2]string{u + " call", u + " calls"}
		}
		if count[n] == 0 {
			order = append(order, n)
		}
		count[n]++
	}
	if len(order) == 0 {
		return "still thinking it through"
	}
	var parts []string
	for _, n := range order {
		label := n[0]
		if count[n] > 1 {
			label = n[1]
		}
		parts = append(parts, fmt.Sprintf("%d %s", count[n], label))
	}
	if len(parts) > 1 {
		parts[len(parts)-1] = "and " + parts[len(parts)-1]
	}
	sep := ", "
	if len(parts) == 2 {
		sep = " "
	}
	return "so far " + strings.Join(parts, sep)
}
