package web

import (
	"html/template"
	"testing"
)

// Every page must parse and pass html/template's context checks; a failure
// here used to show up only as a blank dashboard page.
func TestTemplatesEscape(t *testing.T) {
	tpl := template.Must(template.New("").Funcs(template.FuncMap{
		"ts": func(int64) string { return "" }, "clip": func(s string, _ int) string { return s },
		"secs": func(int64) string { return "" }, "k": func(int64) string { return "" },
		"addTok": func(a, b, c int64) int64 { return 0 }, "pct": func(float64) string { return "" },
		"usd": func(float64) string { return "" }, "lines": func(string) []string { return nil },
		"until": func(int64) string { return "" }, "has": func([]string, string) bool { return false },
		"uptime": func(any) string { return "" }, "since": func(any) string { return "" }, "pctw": func(float64) int { return 0 },
	}).ParseFS(assets, "templates/*.html"))
	for _, page := range []string{"live", "turn", "jobs", "job", "schedule", "memory", "prompts", "console", "replay", "shadow"} {
		p := tpl.Lookup(page + ".html")
		if p == nil {
			t.Fatalf("missing %s", page)
		}
		// Escaping is analysed on first execution; executing against nil data
		// surfaces context errors before any field access fails.
		if err := p.Execute(discard{}, nil); err != nil && isEscapeErr(err) {
			t.Errorf("%s: %v", page, err)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func isEscapeErr(err error) bool {
	_, ok := err.(*template.Error)
	return ok
}
