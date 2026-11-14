package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// The text a voice call speaks while the turn runs: the main agent's text
// deltas, a paragraph break between text blocks, nothing from thinking, tool
// input or a sub-agent.
func TestTextStream(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"check."}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"toolbox"}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"tool\""}}}`,
		`{"type":"stream_event","parent_tool_use_id":"toolu_1","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"sub-agent words"}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"It's sunny."}}}`,
	}
	var got strings.Builder
	st := &textStream{on: func(s string) { got.WriteString(s) }}
	for _, l := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		st.event(ev)
	}
	if want := "Let me check.\n\nIt's sunny."; got.String() != want {
		t.Errorf("streamed %q, want %q", got.String(), want)
	}
	(&textStream{}).event(map[string]any{"type": "stream_event"}) // no callback: a no-op
}

func TestPartialMessagesFlag(t *testing.T) {
	c := &Claude{}
	has := func(req *Request) bool {
		return strings.Contains(strings.Join(c.args(req, "m", ""), " "), "--include-partial-messages")
	}
	if has(&Request{}) {
		t.Error("partial messages asked for without OnText")
	}
	if !has(&Request{OnText: func(string) {}}) {
		t.Error("OnText set but no --include-partial-messages")
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]ErrKind{
		"Claude AI usage limit reached|1791499200":       RateLimit,
		"API Error: 529 {\"type\":\"overloaded_error\"}": Overloaded,
		"Invalid API key · Please run /login":            Auth,
		"OAuth token has expired":                        Auth,
		"something odd happened":                         Other,
	}
	for msg, want := range cases {
		if got := classify(msg); got != want {
			t.Errorf("classify(%q) = %q, want %q", msg, got, want)
		}
	}
}
