package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// noteHints searches the notes for what the person just said and returns a
// few one-line pointers (title, permalink, description), so the model knows a
// note exists without paying for whole notes. It searches with the whole
// message in vector mode (EXO_NOTES_SEARCH), which finds the right note when
// the person's words don't match the note's. Measured on 26 real-style
// queries against a Basic Memory vault: keyword 0.40 MRR@5, vector + this
// filter 0.71. Basic Memory's "hybrid" let keyword scores dominate (0.39); a
// Qwen3 embedding model didn't beat the built-in one; a CPU reranker added
// +0.06 but took 45 s. Daily logs are history, so at most one shows up, and
// only when nothing better does.
func (a *Agent) noteHints(input string) string {
	if len(keywords(input)) == 0 || a.Reg == nil || a.Reg.Notes == nil {
		return "" // "ok thanks" needs no notes
	}
	q := strings.TrimSpace(input)
	if strings.HasPrefix(q, "[") { // a channel tag like "[voice]" is not part of what was said
		if _, rest, ok := strings.Cut(q, "]"); ok {
			q = strings.TrimSpace(rest)
		}
	}
	if len(q) > 400 {
		q = q[:400]
	}
	if v, ok := hintCache.get(q); ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	mode := a.Cfg.NotesSearch
	if mode == "" {
		mode = "vector"
	}
	raw, err := a.Reg.Notes.Call(ctx, "search_notes", map[string]any{"query": q, "page_size": 10, "search_type": mode, "output_format": "json"})
	if err != nil {
		return "" // notes down: answer without them rather than stall the turn
	}
	var res struct {
		Results []struct {
			Title, Permalink, Content string
			Score                     float64
			Metadata                  map[string]any
		}
	}
	s := raw
	var env map[string]any
	if json.Unmarshal([]byte(raw), &env) == nil {
		if r, ok := env["result"]; ok {
			b, _ := json.Marshal(r)
			s = string(b)
			if str, ok := r.(string); ok {
				s = str
			}
		}
	}
	json.Unmarshal([]byte(s), &res)
	var b strings.Builder
	n, logs := 0, 0
	seen := map[string]bool{}
	for _, r := range res.Results {
		if seen[r.Permalink] {
			continue // chunks of the same note
		}
		seen[r.Permalink] = true
		isLog := strings.Contains(r.Permalink, "/log/")
		if isLog && (logs > 0 || n >= 2) {
			continue
		}
		desc := ""
		if r.Metadata != nil {
			desc, _ = r.Metadata["description"].(string)
		}
		if desc == "" {
			desc = firstProse(r.Content)
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", r.Title, r.Permalink, oneLine(desc, 180))
		n++
		if isLog {
			logs++
		}
		if n == 3 {
			break
		}
	}
	hintCache.put(q, b.String())
	return b.String()
}

// firstProse skips frontmatter and headings and returns the first real text.
func firstProse(s string) string {
	if strings.HasPrefix(s, "---") {
		if i := strings.Index(s[3:], "\n---"); i >= 0 {
			s = s[i+7:]
		}
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, ">") && !strings.HasPrefix(line, "|") {
			return line
		}
	}
	return ""
}

// keywords keeps the words that could name a project, app or topic.
func keywords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
	}) {
		w = strings.Trim(w, "-")
		if len(w) < 4 || commonWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) == 6 {
			break
		}
	}
	return out
}

var commonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`about after again also always another anything around latest status back because been before being best better between both cant come could didnt does doesnt doing dont down each even ever every from going gonna good got gotta have having hello here hers hows into just keep last like little look looks made make many maybe more most much must need never next nice only other over really right same says should since some something still such sure take tell than thank thanks that thats them then there these they thing things think this those though through today tomorrow tonight wait want wanna well were what whats when where which while will with would know yeah yesterday your youre yours check checking running work working works quick quickly please help send show give find look start stop update updated new old week weekend morning night time times day days year years`) {
		commonWords[w] = true
	}
}

// hintCache avoids re-searching the same words within a few minutes.
var hintCache = &ttlCache{m: map[string]cacheEntry{}}

type cacheEntry struct {
	v  string
	at time.Time
}

type ttlCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

func (c *ttlCache) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Since(e.at) > 10*time.Minute {
		return "", false
	}
	return e.v, true
}

func (c *ttlCache) put(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 200 { // bounded: drop everything rather than track LRU
		c.m = map[string]cacheEntry{}
	}
	c.m[k] = cacheEntry{v, time.Now()}
}
