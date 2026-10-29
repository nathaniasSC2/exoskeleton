package engine

import "testing"

func TestParseVariant(t *testing.T) {
	for _, s := range []string{"fast", "think", "think256", "think1024", "effort-low", "effort-high"} {
		if _, err := ParseVariant(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "slow", "think10", "think99999", "thinkx", "effort-max"} {
		if _, err := ParseVariant(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestProfileBody(t *testing.T) {
	qwen, oss := profiles[0], profiles[1]
	fast, _ := ParseVariant("fast")
	t1k, _ := ParseVariant("think1024")
	think, _ := ParseVariant("think")

	b := qwen.body(fast, 2048)
	if b["max_tokens"] != 2048 || b["temperature"] != 0.7 || b["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Errorf("qwen fast: %v", b)
	}
	if _, ok := b["thinking_budget_tokens"]; ok {
		t.Errorf("qwen fast must not send a thinking budget")
	}
	b = qwen.body(t1k, 2048)
	if b["max_tokens"] != 3072 || b["thinking_budget_tokens"] != 1024 || b["temperature"] != 1.0 {
		t.Errorf("qwen think1024: the answer must keep its 2048 on top of the budget: %v", b)
	}
	if b = qwen.body(think, 2048); b["max_tokens"] != 2048+8192 || b["thinking_budget_tokens"] != 8192 {
		t.Errorf("qwen think: %v", b)
	}
	if b = oss.body(fast, 2048); b["chat_template_kwargs"].(map[string]any)["reasoning_effort"] != "low" {
		t.Errorf("gpt-oss fast should be effort low: %v", b)
	}
	k2 := profiles[2]
	if b = k2.body(fast, 2048); b["chat_template_kwargs"].(map[string]any)["reasoning_effort"] != "high" || b["thinking_budget_tokens"] != 128 || b["max_tokens"] != 2560 {
		t.Errorf("k2 fast: always effort high with a small budget: %v", b)
	}
	if b = k2.body(t1k, 2048); b["thinking_budget_tokens"] != 1024 || b["max_tokens"] != 3072 {
		t.Errorf("k2 think1024: %v", b)
	}
	hi, _ := ParseVariant("effort-high")
	if b = oss.body(hi, 2048); b["chat_template_kwargs"].(map[string]any)["reasoning_effort"] != "high" || b["max_tokens"] != 2048+thinkCap {
		t.Errorf("gpt-oss high: %v", b)
	}
}
