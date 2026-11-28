package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/engine"
)

// Budgets, in characters, for what the harness puts in front of the model.
// Retrieval was always capped; the fixed layer wasn't, and it grew: the base
// system prompt went from 6.6k to 11.9k in seven weeks and the heartbeat's
// brief to 11.3k (2026-11-27 audit). budget_test.go holds the code to these.
const (
	BudgetSystem      = 10000 // a person's base system prompt: soul, rules, admin, pinned facts, interests, open loops
	BudgetDigest      = 6000  // the heartbeat's input
	BudgetChatContext = 8000  // a chat turn's recent history plus its <context> block, before the message itself

	maxInterestsChars = 1600 // the interests list (set_interests refuses more)
	maxLoopsChars     = 1000 // the open loops (set_open_loops refuses more)
	maxPinnedChars    = 1400 // pinned facts shown in the system prompt (whole facts, in order)
)

// The system prompt's headings for its dynamic parts.
func factsHeading(name string) string { return "# About " + name + "\n" }
func interestsHeading(name string) string {
	return "# What " + name + " is into lately (kept current by your heartbeat)\n"
}

const loopsHeading = "# Open loops (pending, promised or unresolved; kept by your heartbeat)\n"

// PromptBudgets caps editable prompt files, in characters. The dashboard
// refuses a save over its cap, and nightly maintenance reports a file that
// was edited past it on disk. A deployment adds its own before Init.
var PromptBudgets = map[string]int{"heartbeat.md": 6000}

// promptBudget is the cap for a prompt file (0 = none).
func promptBudget(name string) int { return PromptBudgets[name] }

// OverBudget lists every budget the live setup is over: prompt files in the
// data dir past PromptBudgets, and each person's assembled system prompt
// past BudgetSystem. Nightly maintenance reports it; "" means all is well.
func (a *Agent) OverBudget() string {
	var out []string
	names := make([]string, 0, len(PromptBudgets))
	for n := range PromptBudgets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(a.Cfg.Path("prompts", n))
		if err != nil {
			continue
		}
		if got := len(strings.TrimSpace(string(b))); got > PromptBudgets[n] {
			out = append(out, fmt.Sprintf("prompts/%s is %d characters (budget %d)", n, got, PromptBudgets[n]))
		}
	}
	for _, p := range a.Cfg.People {
		if s, _ := a.systemPrompt(p); len(s) > BudgetSystem {
			out = append(out, fmt.Sprintf("%s's system prompt is %d characters (budget %d)", p.Name, len(s), BudgetSystem))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "Prompts over budget (trim them, or move detail into a note the prompt names): " + strings.Join(out, "; ")
}

// CheckBudgets is the budget test, for exo's tests and a deployment's: every
// prompt file the agent would use is inside PromptBudgets, and each person's
// system prompt fits BudgetSystem even with the interests list, open loops
// and pinned facts at their caps. It returns one line per problem.
func CheckBudgets(a *Agent, people ...*config.Person) []string {
	var out []string
	for _, n := range PromptNames() {
		if max := promptBudget(n); max > 0 {
			if got := len(a.prompt(n)); got > max {
				out = append(out, fmt.Sprintf("%s is %d characters; its budget is %d", n, got, max))
			}
		}
	}
	for _, p := range people {
		static := len(a.soul(p)) + len(a.prompt("rules.md")) + 2
		if p.Admin {
			static += len(a.prompt("admin.md")) + 2
		}
		headers := len(factsHeading(p.Name)) + len(interestsHeading(p.Name)) + len(loopsHeading) + 3*2
		worst := static + maxPinnedChars + maxInterestsChars + maxLoopsChars + headers
		if worst > BudgetSystem {
			out = append(out, fmt.Sprintf("%s's system prompt can reach %d characters (soul, rules and admin are %d; the caps add %d); its budget is %d",
				p.Name, worst, static, worst-static, BudgetSystem))
		}
	}
	return out
}

// PreviewContext renders what a turn on channel ch would get before the
// message: its recent history and its <context> block (without the message),
// for budget tests. Like a real turn it may move the history window's start.
func (a *Agent) PreviewContext(p *config.Person, ch, input string) (history, block string) {
	history = engine.HistoryText(a.history(p, 0, ch))
	block = a.contextBlock(p, ch, input, "claude", 0, false, capsFor(ch).Budget-len(history))
	return history, strings.TrimSuffix(block, input)
}

// capLines keeps whole lines of s, in order, while they fit in n characters.
func capLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	var keep []string
	used := 0
	for _, l := range strings.Split(s, "\n") {
		if used+len(l)+1 > n {
			break
		}
		keep = append(keep, l)
		used += len(l) + 1
	}
	return strings.Join(keep, "\n")
}
