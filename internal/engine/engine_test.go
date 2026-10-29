package engine

import "testing"

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
