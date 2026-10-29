package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/tools"
)

// Local runs the turn on the local MoE (llama.cpp's OpenAI-compatible server)
// with exo's own tool loop. Used when Claude is down and the admin said
// "go local" (or failover is set to auto), and by shadow runs.
type Local struct {
	URL, Model string
	Registry   *tools.Registry
	ToolCtx    func(turn int64) *tools.Ctx
	// Variant is how hard it thinks (profile.go): "fast", "think1024", ...
	// Measured 2026-10-11: the same tool call took 17 s thinking, 1.9 s not.
	Variant string
	// AnswerTokens is what each model call may spend on the answer itself,
	// on top of any thinking budget. 0 = 2048.
	AnswerTokens int
	// SwitchFile, if set, exists while the model is being swapped;
	// calls wait it out instead of failing.
	SwitchFile string

	pc profileCache
}

func (l *Local) variant() Variant {
	v, err := ParseVariant(l.Variant)
	if err != nil {
		return Variant{Name: "fast"}
	}
	return v
}

func (l *Local) Name() string { return "local" }

// Ready reports whether the server answers (the lever may be off).
func (l *Local) Ready(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", l.URL+"/models", nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

func (l *Local) Turn(ctx context.Context, req *Request) *Result {
	res := &Result{}
	trace := req.Trace
	if trace == nil {
		trace = func(string, string, string) {}
	}
	tc := l.ToolCtx(req.TurnID)
	if tc == nil {
		res.Err, res.Kind = fmt.Errorf("no tool context for turn"), Other
		return res
	}
	msgs := []map[string]any{{"role": "system", "content": req.System}}
	for _, h := range req.History {
		msgs = append(msgs, map[string]any{"role": h.Role, "content": h.Text})
	}
	answer := l.AnswerTokens
	if answer <= 0 {
		answer = 2048
	}
	prof, vision := l.profile(ctx)
	cur := l.variant()
	if v, err := ParseVariant(req.Effort); err == nil { // per-turn override (voice calls)
		cur = v
	}
	trace("init", "variant", prof.Name+" "+cur.Name)
	msgs = append(msgs, map[string]any{"role": "user", "content": userContent(req.User, vision)})
	var defs []map[string]any
	for _, t := range l.Registry.Exposed(tc) {
		defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Desc, "parameters": t.Schema()}})
	}
	trace("init", l.Model, fmt.Sprintf("tools: %d", len(defs)))

	badJSON, guarded, empties := 0, false, 0
	draft := ""              // the reply as it stood before the self-check
	rewrote := false         // the self-check's rewrite got one nudge for leaking its working
	pushed := false          // a give-up reply with no tool tried got one push to try
	seen := map[string]int{} // identical tool calls this turn
	// Soft deadline: past 60% of the turn's time, stop tools and long thinking
	// and answer with what it has, so a slow turn still ends in a reply.
	wrapAt, wrapping := time.Time{}, false
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		wrapAt = time.Now().Add(time.Until(deadline) * 6 / 10)
	}
	genRate := 15.0 // tokens/s; replaced by each call's measured speed
	steps := 8      // tool steps; the self-check adds one so a long chain still gets it
	for step := 0; step < steps; step++ {
		if !wrapping && !wrapAt.IsZero() && time.Now().After(wrapAt) {
			wrapping = true
			trace("retry", "deadline", "turn is running long: answering now without more tools")
			if cur.Think && cur.thinkTokens() > 256 {
				cur = Variant{Name: "think256", Think: true, Budget: 256}
			}
			if step > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": finalAnswer})
			}
		}
		// Time-aware thinking: one call may spend at most half the time left,
		// at the speed this model actually generates (K2 at ~12 t/s once
		// thought for 5+ minutes in a single call, past the soft deadline).
		call := cur
		if call.Think && hasDeadline {
			fits := int(time.Until(deadline).Seconds()/2*genRate) - answer
			if fits < call.thinkTokens() {
				call.Budget = max(128, fits)
				trace("budget", call.Name, fmt.Sprintf("thinking capped at %d tokens to fit the time left (%.0f t/s)", call.Budget, genRate))
			}
		}
		body := prof.body(call, answer)
		body["model"], body["messages"] = l.Model, msgs
		offered := len(defs) > 0 && step < 7 && badJSON < 2 && !looping(seen) && !wrapping // last step, two broken calls or a loop: answer, no more tools
		if offered {
			body["tools"] = defs
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content   string     `json:"content"`
					Reasoning string     `json:"reasoning_content"`
					ToolCalls []toolCall `json:"tool_calls"`
				} `json:"message"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
			} `json:"usage"`
			Timings struct {
				PromptN  int     `json:"prompt_n"`
				PromptPS float64 `json:"prompt_per_second"`
				GenN     int     `json:"predicted_n"`
				GenPS    float64 `json:"predicted_per_second"`
			} `json:"timings"`
		}
		if err := l.post(ctx, body, &out); err != nil {
			if strings.Contains(err.Error(), "Failed to parse tool call") && badJSON < 2 && ctx.Err() == nil {
				// The model wrote a tool call llama-server couldn't parse (seen
				// with long inline scripts). Nudge once; the second time, answer without tools.
				badJSON++
				trace("retry", "tool_json", err.Error())
				msgs = append(msgs, map[string]any{"role": "user", "content": badToolJSON})
				continue
			}
			res.Err = err
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Kind = Timeout
			} else {
				res.Kind = Overloaded
			}
			trace("error", string(res.Kind), err.Error())
			return res
		}
		res.Usage.In += out.Usage.Prompt
		res.Usage.Out += out.Usage.Completion
		if len(out.Choices) == 0 {
			res.Err, res.Kind = fmt.Errorf("local model returned no choices"), Other
			return res
		}
		m := out.Choices[0].Message
		tm := out.Timings // new prompt tokens (after cache reuse) and generation speed, for tuning
		if tm.GenPS > 1 && tm.GenN > 32 {
			genRate = tm.GenPS
		}
		trace("timing", out.Choices[0].Finish, fmt.Sprintf("prompt %d tok @ %.0f t/s · gen %d tok @ %.1f t/s · think %d chars",
			tm.PromptN, tm.PromptPS, tm.GenN, tm.GenPS, len(m.Reasoning)))
		leak := leakRe.FindStringIndex(m.Content)
		if len(m.ToolCalls) == 0 && !offered && leak != nil {
			// Tools were withdrawn on purpose; keep only the prose before the call.
			m.Content, leak = strings.TrimSpace(m.Content[:leak[0]]), nil
		}
		if len(m.ToolCalls) == 0 && leak != nil {
			// llama-server didn't recognize a Qwen-format tool call and passed it
			// through as text (seen with long arguments). Run it ourselves.
			if calls := parseLeakedCalls(m.Content, step); len(calls) > 0 {
				trace("retry", "leaked_tool_call", fmt.Sprintf("parsed %d tool call(s) from the reply text", len(calls)))
				m.ToolCalls = calls
				m.Content = strings.TrimSpace(m.Content[:leak[0]])
			} else if badJSON < 2 {
				badJSON++
				trace("retry", "leaked_tool_call", "unparseable tool call in the reply text")
				msgs = append(msgs, map[string]any{"role": "user", "content": badToolJSON})
				continue
			}
		}
		if len(m.ToolCalls) == 0 {
			res.Text = strings.TrimSpace(thinkRe.ReplaceAllString(m.Content, ""))
			if draft != "" && (res.Text == "" || checkOKRe.MatchString(res.Text)) {
				res.Text = draft // the check passed it, or came back empty: the draft stands
			} else if draft != "" && res.Text != draft {
				// The check rewrote the draft. The person never saw that draft, so
				// the rewrite must not talk about it (or about checking at all).
				res.Text = stripScratch(res.Text)
				if !rewrote && step < 7 && ctx.Err() == nil && (confessRe.MatchString(res.Text) || scratchRe.MatchString(res.Text)) {
					rewrote = true
					trace("guard", "leaked_check", res.Text)
					draft = res.Text // if it answers OK to the nudge, this stands
					msgs = append(msgs, map[string]any{"role": "assistant", "content": res.Text},
						map[string]any{"role": "user", "content": leakNudge})
					res.Text = ""
					continue
				}
			}
			if len(tc.Used()) == 0 && fakeUsedRe.MatchString(res.Text) {
				// It copied the "(used: X)" tag from history without using anything.
				trace("guard", "fake_tool_tag", res.Text)
				res.Text = strings.TrimSpace(fakeUsedRe.ReplaceAllString(res.Text, ""))
				if !guarded && step < 6 {
					guarded = true
					msgs = append(msgs, map[string]any{"role": "assistant", "content": res.Text},
						map[string]any{"role": "user", "content": claimCheck})
					res.Text = ""
					continue
				}
			}
			if res.Text == "" && out.Choices[0].Finish == "length" && cur.Think {
				// Thinking ran out the whole call: answer again without thinking.
				trace("retry", "length", "thinking used the whole token budget; answering without thinking")
				cur = Variant{Name: "fast"}
				continue
			}
			if res.Text == "" && empties < 2 && ctx.Err() == nil {
				// It ran its checks and then said nothing (seen after long tool
				// chains, often on the last step). Ask for the reply; the second
				// time without tools; past the last step the loop ends in the
				// forced final answer below.
				empties++
				trace("retry", "empty", "empty reply; asking for the answer")
				msgs = append(msgs, map[string]any{"role": "user", "content": emptyReply})
				if empties == 2 {
					wrapping = true
				}
				continue
			}
			if !pushed && !guarded && len(tc.Used()) == 0 && offered && step < 6 && ctx.Err() == nil && giveUpRe.MatchString(res.Text) {
				// Small models say they can't, or ask the person to fetch something,
				// without trying a tool (2026-10-14: "send me the photo" when web_search
				// and screenshot would have found it). One push to try first.
				pushed = true
				trace("guard", "gave_up", res.Text)
				msgs = append(msgs, map[string]any{"role": "assistant", "content": res.Text},
					map[string]any{"role": "user", "content": tryFirst})
				res.Text, draft = "", ""
				continue
			}
			if !guarded && !l.acted(tc) && step < 6 && claimRe.MatchString(res.Text) {
				// Small models report work as done or underway without checking.
				// One chance to check with a tool or say it honestly.
				guarded = true
				trace("guard", "unverified_claim", res.Text)
				msgs = append(msgs, map[string]any{"role": "assistant", "content": res.Text},
					map[string]any{"role": "user", "content": claimCheck})
				res.Text = ""
				continue
			}
			if draft == "" && res.Text != "" && cur.Think && cur.thinkTokens() >= 1024 && !wrapping && ctx.Err() == nil {
				// Thinking turns get a second look before sending (selfCheck);
				// tools stay offered so it can verify a premise or a name.
				draft, steps = res.Text, steps+1
				trace("guard", "self_check", "checking the draft before it goes out")
				msgs = append(msgs, map[string]any{"role": "assistant", "content": res.Text},
					map[string]any{"role": "user", "content": selfCheck})
				res.Text = ""
				continue
			}
			if res.Text == "" {
				res.Err, res.Kind = fmt.Errorf("empty reply"), Other // never a blank text
			}
			trace("text", out.Choices[0].Finish, res.Text)
			return res
		}
		calls := []map[string]any{}
		for _, c := range m.ToolCalls {
			calls = append(calls, map[string]any{"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Function.Name, "arguments": c.Function.Arguments}})
		}
		assistant := map[string]any{"role": "assistant", "content": m.Content, "tool_calls": calls}
		if m.Reasoning != "" {
			// Keep the thinking with its tool calls (Qwen3.6 and K2 templates
			// preserve it): the model reads the tool result knowing why it asked,
			// and the re-sent history matches what's already in the KV cache.
			assistant["reasoning_content"] = m.Reasoning
		}
		msgs = append(msgs, assistant)
		var shots []Block // images tools returned this step (screenshots), shown after the results
		for _, c := range m.ToolCalls {
			var args tools.Args
			json.Unmarshal([]byte(c.Function.Arguments), &args)
			if fixed, ok := repairIFM(args); ok {
				// llama.cpp's K2 parser left K2's <ifm|arg_key> markup inside the
				// toolbox "tool" value; decode it into a proper {tool, args} call.
				trace("retry", "ifm_args", "repaired a K2-format tool call")
				args = fixed
				raw, _ := json.Marshal(args)
				c.Function.Arguments = string(raw)
			}
			trace("tool_call", c.Function.Name, c.Function.Arguments)
			res.Tools = append(res.Tools, c.Function.Name)
			sig := c.Function.Name + " " + c.Function.Arguments
			if seen[sig]++; seen[sig] > 1 {
				// Qwen sometimes repeats a call it already made; don't run it again.
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": repeatCall})
				trace("tool_result", c.Function.Name, repeatCall)
				continue
			}
			r := l.Registry.Call(ctx, tc, c.Function.Name, args)
			trace("tool_result", c.Function.Name, r.Text)
			content := clip(r.Text, 6000)
			if r.Image != "" && vision {
				shots = append(shots, Block{Image: r.Image})
			} else if r.Image != "" {
				content += "\n[This tool returned an image, but the model loaded right now can't see images. Don't describe what it shows; say you couldn't view it.]"
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": content})
		}
		if len(shots) > 0 {
			// Chat templates only take images in user messages, so the step's
			// screenshots follow its tool results as one; without this the model
			// "describes" pages it never saw.
			trace("images", "tool", fmt.Sprintf("showing %d tool image(s) to the model", len(shots)))
			msgs = append(msgs, map[string]any{"role": "user", "content": userContent(append([]Block{{Text: "[The image(s) your tool calls just returned, in order. Describe only what you can actually see.]"}}, shots...), true)})
		}
	}
	// Out of steps: one last call without tools so the person still gets an answer.
	trace("retry", "steps", "out of tool steps; asking for a final answer")
	msgs = append(msgs, map[string]any{"role": "user", "content": finalAnswer})
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	body := prof.body(Variant{Name: "fast"}, answer)
	body["model"], body["messages"] = l.Model, msgs
	if err := l.post(ctx, body, &out); err == nil && len(out.Choices) > 0 {
		if t := strings.TrimSpace(leakedFnRe.ReplaceAllString(thinkRe.ReplaceAllString(out.Choices[0].Message.Content, ""), "")); t != "" {
			res.Text = strings.TrimSpace(strings.Split(t, "<tool_call>")[0])
			trace("text", "final", res.Text)
			return res
		}
	}
	res.Err, res.Kind = fmt.Errorf("local model kept calling tools"), Other
	return res
}

const finalAnswer = "(system) No more tools in this turn. Answer the person now with what you found; say plainly what you couldn't check."

// selfCheck is the second look a thinking turn gets before its reply goes out:
// the 2026-10-11 eval's local failures were nearly all invented specifics,
// agreeing with a wrong premise, or claiming unchecked things. "OK" keeps the
// draft as is, so a clean answer costs one short call.
const selfCheck = `(system check, not from the person; they never see this exchange or your draft) Before the draft goes out, check it against this turn's tool results and the conversation:
- Every specific (number, score, record, date, price, status, or the name of a person, place, item, ability, setting, file or service) must appear in a tool result from this turn or in what the person said. Drop or hedge anything that doesn't; on niche topics never fill gaps from memory.
- Earlier replies in this conversation were checked with tools when they were sent (the context lists which). Don't retract, doubt or apologise for them without a new reason from a tool result, and never call them guesses. But a repeat question about something that changes (schedules, scores, prices, news) needs a fresh lookup, not a copy of the old answer.
- If the person's message assumed how something works or what happened, confirm it before agreeing, and say so if it's wrong. You can still use tools.
- If they mention something you haven't seen (a photo, listing, product, page), search for it with tools before asking them for it; never describe it from memory.
- If the draft says you can't do something, or asks them to do it, and you haven't tried a tool for it this turn, try one now.
- Don't say something was done unless a tool call above did it.
If the draft passes, reply with just: OK. Otherwise reply with ONLY the final message to the person, written as if it were the first and only draft: no "wait", "let me fix", "I made that up" or any mention of a draft, a check or a correction.`

// giveUpRe matches a reply that declines, or hands the work back to the person.
var giveUpRe = regexp.MustCompile(`(?i)\b(i can'?t|i cannot|i'?m (not able|unable)|i (don'?t|do not) have (a way|access)|unable to|i (need|would need|'d need) you to|(send|share|give) me (a|the) (link|file|image|url|photo|pic|picture)|(send|share) (it|that|the (file|image|photo)) (with|to) me)\b`)

// tryFirst is the push when a reply gives up before any tool was tried.
const tryFirst = "(system check, not from the person) You haven't tried a single tool this turn, but your reply says you can't or asks them to do it. Try first with the toolbox: web_search, then fetch the best result; start_job for downloading files or making and editing images; shell to look at the server. Then answer with what you found, or say exactly what you tried if it didn't work."

// leakNudge is the one retry when a rewrite still talks about its own checking.
const leakNudge = "(system check, not from the person) That reply talks about a draft, a check or a correction, but the person never saw any draft. Write only the message to them: the answer itself, with nothing about checking or fixing. Reply with the message, not OK."

// confessRe matches a rewrite that confesses to or corrects a draft nobody saw.
var confessRe = regexp.MustCompile(`(?i)\b(i (just )?made (that|this|it|those|them) up|i didn't look anything up|i (shouldn't|should not) have (said|put|made)|(my|the) (first |earlier )?(draft|guess)|i need to correct myself)`)

// scratchRe matches thinking-aloud paragraphs ("Wait, ...", "Hmm, ...", "Let me ...").
var scratchRe = regexp.MustCompile(`(?im)^\s*(wait|hmm+|let me (fix|just|check|redo|rewrite)|actually,? (let me|i need)|ok(ay)?,? so)\b`)

// stripScratch drops thinking-aloud paragraphs from a rewrite, and when the
// rewrite then says the same thing twice (a first attempt and a "fixed" one),
// keeps the later one.
func stripScratch(s string) string {
	var keep []string
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(strings.TrimSpace(s), -1) {
		if p = strings.TrimSpace(p); p != "" && !scratchRe.MatchString(p) {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return strings.TrimSpace(s) // nothing but scratch: leave it for the nudge
	}
	for i := 0; i < len(keep); i++ {
		for j := len(keep) - 1; j > i; j-- {
			if len(keep[i]) >= 20 && len(keep[j]) >= 20 && keep[i][:20] == keep[j][:20] {
				keep = append(keep[:i], keep[j:]...)
				break
			}
		}
	}
	return strings.Join(keep, "\n\n")
}

var checkOKRe = regexp.MustCompile(`(?i)^\W*ok\W*$`)

// fakeUsedRe matches the "(used: tool)" tag the history puts on past replies, when the model writes it itself.
var fakeUsedRe = regexp.MustCompile(`(?i)\(\s*(\w+ )?used:[^)]*\)\s*`)

const badToolJSON = "(system) Your last tool call wasn't valid JSON, so it didn't run. Keep tool arguments short and valid; never put long scripts or files in a tool call. Try again, or answer without the tool."

type toolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

var (
	// leakRe spots a tool call written into the reply text instead of made:
	// Qwen's XML format, or garbled forms seen in shadow runs ("ctrl94{task=...").
	leakRe        = regexp.MustCompile(`(?i)<tool_call>|<function=|<ifm\|tool_call|\bctrl\d{2,3}\s*\{|\{\s*"?tool"?\s*[:=]\s*"`)
	ifmCallRe     = regexp.MustCompile(`(?s)<ifm\|tool_call>\s*([\w.-]+)(.*?)(?:</ifm\|tool_call>|$)`)
	ifmPairRe     = regexp.MustCompile(`(?s)<ifm\|arg_key>(.*?)</ifm\|arg_key>\s*<ifm\|arg_value>(.*?)(?:</ifm\|arg_value>|<ifm\||$)`)
	leakedFnRe    = regexp.MustCompile(`(?s)<function=([\w.-]+)>(.*?)</function>`)
	leakedParamRe = regexp.MustCompile(`(?s)<parameter=([\w.-]+)>(.*?)</parameter>`)
)

// parseLeakedCalls reads Qwen's XML tool-call format out of reply text:
// <tool_call><function=NAME><parameter=KEY>VALUE</parameter>...</function></tool_call>.
// Values that parse as JSON (objects, numbers) are kept as JSON.
func parseLeakedCalls(content string, step int) []toolCall {
	var calls []toolCall
	for i, cm := range ifmCallRe.FindAllStringSubmatch(content, -1) { // K2-Horizon's native format
		raw, _ := json.Marshal(ifmArgs(cm[2]))
		c := toolCall{ID: fmt.Sprintf("leaked-%d-k%d", step, i)}
		c.Function.Name, c.Function.Arguments = cm[1], string(raw)
		calls = append(calls, c)
	}
	if len(calls) > 0 {
		return calls
	}
	for i, fm := range leakedFnRe.FindAllStringSubmatch(content, -1) {
		args := map[string]any{}
		for _, pm := range leakedParamRe.FindAllStringSubmatch(fm[2], -1) {
			v := strings.TrimSpace(pm[2])
			var j any
			if json.Unmarshal([]byte(v), &j) == nil && (strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[")) {
				args[pm[1]] = j
			} else {
				args[pm[1]] = v
			}
		}
		raw, _ := json.Marshal(args)
		c := toolCall{ID: fmt.Sprintf("leaked-%d-%d", step, i)}
		c.Function.Name, c.Function.Arguments = fm[1], string(raw)
		calls = append(calls, c)
	}
	return calls
}

// ifmArgs decodes K2's <ifm|arg_key>k</ifm|arg_key><ifm|arg_value>v</ifm|arg_value> pairs.
func ifmArgs(s string) map[string]any {
	args := map[string]any{}
	for _, pm := range ifmPairRe.FindAllStringSubmatch(s, -1) {
		k, v := strings.TrimSpace(pm[1]), strings.TrimSpace(pm[2])
		var j any
		if (strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[")) && json.Unmarshal([]byte(v), &j) == nil {
			args[k] = j
		} else {
			args[k] = v
		}
	}
	return args
}

// repairIFM fixes a toolbox call whose "tool" value still holds K2 markup, e.g.
// {"tool": "shell\n<ifm|arg_key>command</ifm|arg_key>\n<ifm|arg_value>ls -l"}.
func repairIFM(a tools.Args) (tools.Args, bool) {
	name, _ := a["tool"].(string)
	i := strings.Index(name, "<ifm|")
	if i < 0 {
		return nil, false
	}
	inner := ifmArgs(name[i:])
	out := tools.Args{"tool": strings.TrimSpace(name[:i])}
	if nested, ok := inner["args"].(map[string]any); ok {
		out["args"] = nested
	} else {
		if prev, ok := a["args"].(map[string]any); ok {
			for k, v := range prev {
				inner[k] = v
			}
		}
		delete(inner, "args")
		out["args"] = map[string]any(inner)
	}
	return out, true
}

const repeatCall = "(You already made this exact call in this turn; its result is above. Don't repeat it: use that result and answer.)"

// looping reports whether the model has repeated the same tool call twice over.
func looping(seen map[string]int) bool {
	for _, n := range seen {
		if n >= 3 {
			return true
		}
	}
	return false
}

const emptyReply = "(system) Your last message to the person was empty. Reply to them now with what you found; call a tool first only if you really need one."

// acted reports whether this turn ran any tool that changes something.
func (l *Local) acted(tc *tools.Ctx) bool {
	for _, n := range tc.Used() {
		if l.Registry.Acts(n) {
			return true
		}
	}
	return false
}

const claimCheck = "(system check, not from the person) Your reply says something is made, done, running, ready, sent, started, scheduled or coming later, but no tool that makes or changes anything (start_job, reminder and the like) ran in this turn; searches only look. Do it now with the toolbox (start_job to make or edit an image, jobs to check one), or rewrite the reply so it only states what you actually found or did. Reply to the person, not to this note."

// claimRe spots replies that report work as done, underway or promised.
var claimRe = regexp.MustCompile(`(?i)` +
	`\b(it'?s|it is|that'?s|render(ing)? is|job is|still) (done|ready|finished|complete|rendering|running|cooking|working|chugging|charging|going|on (its|the) way|in progress|processing)\b` +
	`|\b(I|it|we)('ll| will) (send|ping|text|let you know|update|get back|have it|follow up|start|kick off|queue|restart|fix|run|set up|build|render|deploy)\b` +
	`|\b(I'?ve|I have|just|I) (started|sent|set up|scheduled|kicked off|queued|restarted|fixed|updated|deployed|created|saved|cancelled|canceled|made sure|changed|tweaked|adjusted|configured|switched|edited|wrote|told it|added|removed|turned (on|off))\b` +
	`|\b(I'?m|I am) (on it|starting|kicking off|rendering|building|working on|keeping an eye on|watching|monitoring)\b` +
	`|\b(started|kicked off|queued) (it|a job|the job|the render)\b` +
	`|\bjob #?\d+ (is |has )?(started|running|queued|underway|kicked off|done|finished)\b` +
	`|\b(starting|kicking off|firing up|spinning up) (it|the|a|your|that|him|her|them)\b` +
	// Made things it never made (2026-10-14: "here's how it looks: clean green/gold
	// text overlay..." for a wallpaper no job rendered).
	`|\bhere'?s (how it (looks|came out|turned out)|(your|the|my|a) (new |finished |final )?(wallpaper|background|lock ?screen|edit|design|render|mockup|crop|version|draft|remix|poster|graphic|collage))\b` +
	`|\b(I'?ve|I have|just|I) (made|cropped|designed|generated|drew|drawn|rendered|edited|mocked up|put together|whipped up|built|composed|resized|upscaled)\b` +
	`|\b(it|this|that|the (wallpaper|background|image|edit|design|render)) (looks|came out|turned out) (great|clean|good|sick|amazing|awesome)\b` +
	`|^\W*(done|all set)\s*[—–!.,-]|\b(reminder|alarm|timer|job)('s| is)? (set|scheduled|added|queued|booked)\b`)

// userContent is the per-turn message: plain text, or typed parts when images
// are attached (llama-server with a vision projector; it may encode on the CPU).
func userContent(blocks []Block, vision bool) any {
	var text strings.Builder
	var imgs []map[string]any
	for _, b := range blocks {
		if b.Image == "" {
			text.WriteString(b.Text)
			continue
		}
		if !vision {
			text.WriteString("\n[A photo was attached, but the model loaded right now can't see images. Say so if it matters.]\n")
			continue
		}
		raw, err := os.ReadFile(b.Image)
		if err != nil {
			text.WriteString("\n[An image was attached but couldn't be read.]\n")
			continue
		}
		mime := "image/jpeg"
		if strings.HasSuffix(strings.ToLower(b.Image), ".png") {
			mime = "image/png"
		}
		imgs = append(imgs, map[string]any{"type": "image_url",
			"image_url": map[string]any{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)}})
	}
	if len(imgs) == 0 {
		return text.String()
	}
	return append([]map[string]any{{"type": "text", "text": text.String()}}, imgs...)
}

// WaitSwitch blocks while a gear change reloads the model (up to 4 minutes).
func (l *Local) WaitSwitch(ctx context.Context) {
	if l.SwitchFile == "" {
		return
	}
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
		if _, err := os.Stat(l.SwitchFile); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (l *Local) post(ctx context.Context, body any, out any) error {
	l.WaitSwitch(ctx)
	j, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", l.URL+"/chat/completions", bytes.NewReader(j))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("local %d: %s", resp.StatusCode, clip(string(raw), 300))
	}
	return json.Unmarshal(raw, out)
}
