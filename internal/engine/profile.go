package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Variant is how much the local model thinks on a turn:
//
//	fast            no thinking (gpt-oss can't turn it off: effort low)
//	think           thinking, capped only by thinkCap
//	think<N>        thinking capped at N tokens, e.g. think1024
//	effort-<level>  gpt-oss reasoning effort: low, medium, high
//
// The answer always gets its own AnswerTokens on top of the thinking budget,
// so thinking can never eat the reply (the failure that sank an earlier gateway's local
// path under its 31k-token prompt).
type Variant struct {
	Name   string
	Think  bool
	Budget int    // thinking-token cap; 0 = thinkCap
	Effort string // gpt-oss reasoning_effort
}

const thinkCap = 8192 // the most thinking any turn step may spend ("think"); quality over speed

func ParseVariant(s string) (Variant, error) {
	s = strings.TrimSpace(s)
	v := Variant{Name: s}
	switch {
	case s == "fast":
	case s == "think":
		v.Think = true
	case strings.HasPrefix(s, "think"):
		n, err := strconv.Atoi(strings.TrimPrefix(s, "think"))
		if err != nil || n < 32 || n > thinkCap {
			return v, fmt.Errorf("bad variant %q: think<N> needs 32 <= N <= %d", s, thinkCap)
		}
		v.Think, v.Budget = true, n
	case strings.HasPrefix(s, "effort-"):
		v.Effort = strings.TrimPrefix(s, "effort-")
		if v.Effort != "low" && v.Effort != "medium" && v.Effort != "high" {
			return v, fmt.Errorf("bad variant %q: effort is low, medium or high", s)
		}
		v.Think = true
	default:
		return v, fmt.Errorf("unknown variant %q", s)
	}
	return v, nil
}

// A Profile is a model family's recommended sampling (from the model cards)
// and how it is told to think.
type Profile struct {
	Name     string
	Thinking map[string]any // sampling when it thinks
	Direct   map[string]any // sampling when it doesn't
	// kwargs returns chat_template_kwargs, plus any top-level request fields.
	kwargs func(v Variant) (map[string]any, map[string]any)
}

var profiles = []Profile{
	{
		// Qwen3.6-35B-A3B model card (general use): thinking temp 1.0 / top_p
		// 0.95, non-thinking temp 0.7 / top_p 0.8; top_k 20, min_p 0 and
		// presence_penalty 1.5 for both (it curbs repetition in quantized models).
		Name:     "qwen",
		Thinking: map[string]any{"temperature": 1.0, "top_p": 0.95, "top_k": 20, "min_p": 0.0, "presence_penalty": 1.5},
		Direct:   map[string]any{"temperature": 0.7, "top_p": 0.8, "top_k": 20, "min_p": 0.0, "presence_penalty": 1.5},
		kwargs: func(v Variant) (map[string]any, map[string]any) {
			top := map[string]any{}
			if v.Think {
				top["thinking_budget_tokens"] = v.thinkTokens()
			}
			return map[string]any{"enable_thinking": v.Think}, top
		},
	},
	{
		// gpt-oss model card: temperature 1.0, top_p 1.0 (top_k 0 = off); it
		// always reasons, so "fast" means effort low (~24 thinking tokens on
		// short prompts, ~86 at medium).
		Name:     "gpt-oss",
		Thinking: map[string]any{"temperature": 1.0, "top_p": 1.0, "top_k": 0},
		Direct:   map[string]any{"temperature": 1.0, "top_p": 1.0, "top_k": 0},
		kwargs: func(v Variant) (map[string]any, map[string]any) {
			effort := v.Effort
			if effort == "" {
				effort = map[bool]string{false: "low", true: "medium"}[v.Think]
			}
			return map[string]any{"reasoning_effort": effort}, map[string]any{}
		},
	},
	{
		// K2-Horizon-MoVA model card: reasoning_effort always "high" (low and
		// medium are unsupported; llama.cpp #30246 also misroutes their output),
		// temperature 1.0, top_p 0.95. The thinking budget is the speed lever:
		// "fast" keeps 128 tokens of it.
		Name:     "k2",
		Thinking: map[string]any{"temperature": 1.0, "top_p": 0.95},
		Direct:   map[string]any{"temperature": 1.0, "top_p": 0.95},
		kwargs: func(v Variant) (map[string]any, map[string]any) {
			budget := 128
			if v.Think {
				budget = v.thinkTokens()
			}
			return map[string]any{"reasoning_effort": "high"}, map[string]any{"thinking_budget_tokens": budget}
		},
	},
}

func (v Variant) thinkTokens() int {
	if v.Budget > 0 {
		return v.Budget
	}
	return thinkCap
}

// maxTokens is the request's max_tokens: the answer allowance plus whatever
// thinking may spend.
func (v Variant) maxTokens(answer int) int {
	if !v.Think {
		return answer
	}
	return answer + v.thinkTokens()
}

// body builds the sampling and thinking part of a chat request.
func (p Profile) body(v Variant, answer int) map[string]any {
	b := map[string]any{"max_tokens": v.maxTokens(answer)}
	if p.Name != "qwen" && !v.Think { // these families always reason a little
		b["max_tokens"] = answer + 512
	}
	src := p.Direct
	if v.Think {
		src = p.Thinking
	}
	for k, x := range src {
		b[k] = x
	}
	kw, top := p.kwargs(v)
	b["chat_template_kwargs"] = kw
	for k, x := range top {
		b[k] = x
	}
	return b
}

// profileCache remembers which family llama-server has loaded, read from
// /props (a GET, which llm-proxy passes through without waking the model).
type profileCache struct {
	mu     sync.Mutex
	name   string
	vision bool
	at     time.Time
}

// profile returns the loaded model's family profile and whether it can see images.
func (l *Local) profile(ctx context.Context) (Profile, bool) {
	l.pc.mu.Lock()
	defer l.pc.mu.Unlock()
	if time.Since(l.pc.at) > time.Minute {
		l.pc.name, l.pc.vision = l.detect(ctx)
		l.pc.at = time.Now()
	}
	for _, p := range profiles {
		if p.Name == l.pc.name {
			return p, l.pc.vision
		}
	}
	return profiles[0], l.pc.vision
}

func (l *Local) detect(ctx context.Context) (family string, vision bool) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(l.URL, "/v1")+"/props", nil)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return "qwen", true
	}
	defer resp.Body.Close()
	var p struct {
		ModelPath  string `json:"model_path"`
		Modalities *struct {
			Vision bool `json:"vision"`
		} `json:"modalities"`
	}
	json.NewDecoder(resp.Body).Decode(&p)
	family, vision = "qwen", p.Modalities == nil || p.Modalities.Vision
	switch path := strings.ToLower(p.ModelPath); {
	case strings.Contains(path, "gpt-oss"):
		family = "gpt-oss"
	case strings.Contains(path, "k2-horizon"):
		family = "k2"
	}
	return family, vision
}
