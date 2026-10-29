// Package engine runs one conversational turn on a brain: Claude Code on
// a Claude subscription or API key (the default) or a local model through an
// OpenAI-compatible server such as llama.cpp (the fallback).
// Both get the same prompt and the same tools; only the plumbing differs.
package engine

import (
	"context"
	"strings"
)

type Block struct {
	Text  string
	Image string // JPEG path
}

// Msg is one item of the recent conversation. Engines that take chat history
// (the local model) send Role/Text as real messages, so an append-only window
// lets the server reuse everything before the newest exchange; the others
// flatten Line into the transcript text.
type Msg struct {
	Role string // "user" | "assistant"
	Text string // message content
	Line string // the same item as transcript line(s)
}

// HistoryText flattens a history into the transcript block the Claude path sees.
func HistoryText(h []Msg) string {
	if len(h) == 0 {
		return ""
	}
	lines := make([]string, len(h))
	for i, m := range h {
		lines[i] = m.Line
	}
	return "Recent conversation (oldest first):\n" + strings.Join(lines, "\n") + "\n\n"
}

type Request struct {
	TurnID   int64
	System   string  // stable part: persona, rules, person card (cache-friendly)
	History  []Msg   // recent conversation, oldest first (append-only window)
	User     []Block // per-turn context, the message, any images
	BuiltIns string  // Claude Code built-in tools for this person
	Model    string  // overrides the engine's model for this turn (voice calls)
	Effort   string  // overrides the engine's effort for this turn
	Trace    func(kind, name, body string)
}

type Usage struct {
	In, Out, CacheRead, CacheWrite int64
	Cost                           float64
}

type ErrKind string

const (
	OK         ErrKind = ""
	RateLimit  ErrKind = "rate_limit"
	Auth       ErrKind = "auth"
	Overloaded ErrKind = "overloaded"
	Timeout    ErrKind = "timeout"
	Other      ErrKind = "other"
)

// Limit is the plan usage Claude Code reports on every call.
type Limit struct {
	Status   string  `json:"status"` // allowed | allowed_warning | rejected
	Window   string  `json:"window"`
	ResetsAt int64   `json:"resets_at"`
	FiveHour float64 `json:"five_hour"`
	SevenDay float64 `json:"seven_day"`
}

type Result struct {
	Text  string
	Usage Usage
	Tools []string
	Err   error
	Kind  ErrKind
	Limit *Limit
}

type Engine interface {
	Name() string
	Turn(ctx context.Context, req *Request) *Result
}

func classify(msg string) ErrKind {
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "usage limit", "rate limit", "rate_limit", "limit reached", "429", "too many requests"):
		return RateLimit
	case containsAny(m, "/login", "oauth", "401", "403", "authentication", "invalid api key", "expired", "unauthorized"):
		return Auth
	case containsAny(m, "529", "overloaded", "500", "502", "503", "api_error", "econn", "network", "socket"):
		return Overloaded
	}
	return Other
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
