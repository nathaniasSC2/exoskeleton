package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
	"github.com/nathaniasSC2/exoskeleton/tools"
)

// fakeLlama answers /chat/completions with scripted replies and keeps the requests.
type fakeLlama struct {
	mu      sync.Mutex
	bodies  []map[string]any
	replies []string      // raw JSON responses, in order
	delay   time.Duration // before the first reply only
}

func (f *fakeLlama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" { // /props: model family detection
		w.Write([]byte(`{"model_path":"/models/Qwen3.6-35B-A3B.gguf"}`))
		return
	}
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	first := len(f.bodies) == 0
	f.mu.Unlock()
	if first {
		time.Sleep(f.delay)
	}
	var b map[string]any
	json.Unmarshal(raw, &b)
	f.mu.Lock()
	f.bodies = append(f.bodies, b)
	i := len(f.bodies) - 1
	f.mu.Unlock()
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	if strings.HasPrefix(f.replies[i], "500:") {
		http.Error(w, f.replies[i][4:], 500)
		return
	}
	w.Write([]byte(f.replies[i]))
}

func text(s string) string {
	j, _ := json.Marshal(s)
	return `{"choices":[{"message":{"content":` + string(j) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
}

func newLocal(t *testing.T, f *fakeLlama) *Local {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB.Close() })
	alex := &config.Person{ID: "alex", Name: "Alex", Admin: true}
	cfg := &config.Config{DataDir: t.TempDir(), Loc: time.UTC, People: []*config.Person{alex}}
	reg := tools.New(cfg, st, nil, nil)
	tc := &tools.Ctx{Person: alex, Engine: "local", Replay: true}
	return &Local{URL: srv.URL, Model: "m", Registry: reg, ToolCtx: func(int64) *tools.Ctx { return tc }}
}

func TestLocalTurnSendsImages(t *testing.T) {
	f := &fakeLlama{replies: []string{text("A dog on a chair.")}}
	l := newLocal(t, f)
	img := filepath.Join(t.TempDir(), "lou.jpg")
	os.WriteFile(img, []byte{0xff, 0xd8, 0xff, 0xe0}, 0o644)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "what's this?"}, {Image: img}}})
	if res.Err != nil || res.Text != "A dog on a chair." {
		t.Fatalf("turn: %v %q", res.Err, res.Text)
	}
	msgs := f.bodies[0]["messages"].([]any)
	parts, ok := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image not sent: %#v", msgs[len(msgs)-1])
	}
	if f.bodies[0]["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Fatalf("thinking should default off")
	}
}

func TestLocalTurnClaimGuard(t *testing.T) {
	f := &fakeLlama{replies: []string{text("The render is done, I'll send it in a sec."), text("I can't check renders from here, sorry.")}}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "any updates?"}}})
	if res.Text != "I can't check renders from here, sorry." || len(f.bodies) != 2 {
		t.Fatalf("guard: %q after %d calls", res.Text, len(f.bodies))
	}
}

func TestLocalTurnBadToolJSON(t *testing.T) {
	f := &fakeLlama{replies: []string{"500:Failed to parse tool call arguments as JSON", text("Here's the answer.")}}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "do it"}}})
	if res.Err != nil || res.Text != "Here's the answer." {
		t.Fatalf("bad json retry: %v %q", res.Err, res.Text)
	}
}

func toolCallReply(args string) string {
	j, _ := json.Marshal(args)
	return `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"toolbox","arguments":` + string(j) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
}

func TestLocalTurnRepeatBreakerAndStubs(t *testing.T) {
	call := `{"tool":"start_job","args":{"task":"render a dog"}}`
	f := &fakeLlama{replies: []string{toolCallReply(call), toolCallReply(call), toolCallReply(call), text("Started it.")}}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "render a dog"}}})
	if res.Err != nil || res.Text != "Started it." {
		t.Fatalf("turn: %v %q", res.Err, res.Text)
	}
	if _, ok := f.bodies[3]["tools"]; ok {
		t.Errorf("tools still offered after three identical calls")
	}
	msgs := f.bodies[1]["messages"].([]any)
	stub := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if !strings.HasPrefix(stub, "job #901 started") {
		t.Errorf("replay stub = %q", stub)
	}
	msgs = f.bodies[2]["messages"].([]any)
	if c := msgs[len(msgs)-1].(map[string]any)["content"].(string); c != repeatCall {
		t.Errorf("repeat not caught: %q", c)
	}
}

func TestLocalTurnSoftDeadline(t *testing.T) {
	call := `{"tool":"jobs"}`
	f := &fakeLlama{replies: []string{toolCallReply(call), text("Here's what I have.")}, delay: 80 * time.Millisecond}
	l := newLocal(t, f)
	l.Variant = "think4096"
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	res := l.Turn(ctx, &Request{System: "s", User: []Block{{Text: "status?"}}})
	if res.Text != "Here's what I have." {
		t.Fatalf("%v %q", res.Err, res.Text)
	}
	if _, ok := f.bodies[0]["tools"]; !ok {
		t.Errorf("first step should still offer tools")
	}
	if len(f.bodies) != 2 {
		t.Fatalf("want 2 calls, got %d", len(f.bodies))
	}
	// the slow first step (80 of 120 ms) crossed the 60% mark: wrap up
	if _, ok := f.bodies[1]["tools"]; ok || f.bodies[1]["thinking_budget_tokens"].(float64) > 256 {
		t.Errorf("second step should wrap up (no tools, short thinking): tools=%v budget=%v", f.bodies[1]["tools"] != nil, f.bodies[1]["thinking_budget_tokens"])
	}
}

func TestLocalTurnKeepsReasoningWithToolCalls(t *testing.T) {
	withThink := `{"choices":[{"message":{"content":"","reasoning_content":"need the score","tool_calls":[{"id":"c1","type":"function","function":{"name":"toolbox","arguments":"{\"tool\":\"jobs\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	f := &fakeLlama{replies: []string{withThink, text("Done checking.")}}
	l := newLocal(t, f)
	l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "status?"}}})
	msgs := f.bodies[1]["messages"].([]any)
	var found bool
	for _, m := range msgs {
		if mm := m.(map[string]any); mm["role"] == "assistant" && mm["reasoning_content"] == "need the score" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning not carried with the tool call: %v", msgs)
	}
}

func TestLocalTurnEmptyReplyIsRetried(t *testing.T) {
	f := &fakeLlama{replies: []string{text(""), text(""), text("Here's what I found.")}}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "check the browser"}}})
	if res.Text != "Here's what I found." || len(f.bodies) != 3 {
		t.Fatalf("%q after %d calls", res.Text, len(f.bodies))
	}
	if _, ok := f.bodies[2]["tools"]; ok {
		t.Errorf("the second retry should withdraw tools")
	}
}

func TestLocalTurnEmptyOnLastStepGetsFinalAnswer(t *testing.T) {
	// seven tool steps (each a different call), then an empty answer on the last step
	var replies []string
	for i := 0; i < 7; i++ {
		replies = append(replies, toolCallReply(fmt.Sprintf(`{"tool":"recall","args":{"query":"q%d"}}`, i)))
	}
	replies = append(replies, text(""), text("Final: here's what I found."))
	f := &fakeLlama{replies: replies}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "dig in"}}})
	if res.Err != nil || res.Text != "Final: here's what I found." {
		t.Fatalf("%v %q after %d calls", res.Err, res.Text, len(f.bodies))
	}
}

func TestLocalTurnThinkingFitsTheTimeLeft(t *testing.T) {
	f := &fakeLlama{replies: []string{text("ok")}}
	l := newLocal(t, f)
	l.Variant = "think"
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second) // 200 s at 15 t/s = 3000 tokens
	defer cancel()
	l.Turn(ctx, &Request{System: "s", User: []Block{{Text: "hi"}}})
	got := f.bodies[0]["thinking_budget_tokens"].(float64)
	if got >= 8192 || got < 128 || got > 3000 {
		t.Fatalf("thinking budget %v should be cut to what fits in half the time left", got)
	}
}

func TestLocalTurnClaimAfterReadOnlyChecks(t *testing.T) {
	look := `{"tool":"jobs"}`
	f := &fakeLlama{replies: []string{toolCallReply(look), text("Job #3 started! Lou is powering up."), text("I can start that render if you want.")}}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "use Lou"}}})
	if res.Text != "I can start that render if you want." {
		t.Fatalf("a claim after only read-only tools must be challenged: %q", res.Text)
	}
}

func TestLocalTurnSelfCheck(t *testing.T) {
	for _, tt := range []struct{ check, want string }{
		{"OK", "Lions won 17-12."}, // passed: the draft stands
		{"", "Lions won 17-12."},   // empty check: the draft stands
		{"Lions won 17-12; I couldn't confirm their record.", "Lions won 17-12; I couldn't confirm their record."},
	} {
		f := &fakeLlama{replies: []string{text("Lions won 17-12."), text(tt.check)}}
		l := newLocal(t, f)
		l.Variant = "think"
		res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "lions?"}}})
		if res.Err != nil || res.Text != tt.want {
			t.Fatalf("check %q: %v %q", tt.check, res.Err, res.Text)
		}
		msgs := f.bodies[1]["messages"].([]any)
		if c, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string); c != selfCheck {
			t.Fatalf("second call isn't the self-check: %q", c)
		}
	}
	// fast turns (voice) skip it
	f := &fakeLlama{replies: []string{text("hey!")}}
	l := newLocal(t, f)
	if res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "yo"}}}); res.Text != "hey!" || len(f.bodies) != 1 {
		t.Fatalf("fast turn: %q after %d calls", res.Text, len(f.bodies))
	}
}

func TestLocalTurnBadReminderArgsRefusedInReplay(t *testing.T) {
	call := `{"tool":"reminder","args":{"action":"remind","at":"7pm"}}`
	f := &fakeLlama{replies: []string{toolCallReply(call), text("I couldn't set it.")}}
	l := newLocal(t, f)
	l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "remind me at 7"}}})
	msgs := f.bodies[1]["messages"].([]any)
	if c := msgs[len(msgs)-1].(map[string]any)["content"].(string); !strings.HasPrefix(c, `action must be one of add, list, cancel (got "remind")`) {
		t.Fatalf("bad enum not refused: %q", c)
	}
}

func TestLocalTurnGiveUpGuard(t *testing.T) {
	f := &fakeLlama{replies: []string{text("I can't see that photo. Send me the link and I'll make it."), text("Found a few shots of the TD; here's the best one.")}}
	l := newLocal(t, f)
	res := l.Turn(context.Background(), &Request{System: "s", User: []Block{{Text: "make a wallpaper from Garrett Wilson's TD"}}})
	if res.Text != "Found a few shots of the TD; here's the best one." || len(f.bodies) != 2 {
		t.Fatalf("give-up guard: %q after %d calls", res.Text, len(f.bodies))
	}
	msgs := f.bodies[1]["messages"].([]any)
	if c, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string); c != tryFirst {
		t.Fatalf("no push to try a tool: %q", c)
	}
}
