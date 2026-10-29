package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var httpc = &http.Client{Timeout: 30 * time.Second}

const ua = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

// ---- local-engine web tools (Claude Code brings its own) ----

var (
	ddgLink    = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe      = regexp.MustCompile(`(?s)<[^>]+>`)
	dropRe     = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|footer|header)[^>]*>.*?</(script|style|noscript|svg|nav|footer|header)>`)
	spaceRe    = regexp.MustCompile(`[ \t]+`)
	blankRe    = regexp.MustCompile(`\n\s*\n+`)
)

func (r *Registry) webSearch(ctx context.Context, _ *Ctx, a Args) Result {
	// DuckDuckGo's HTML endpoint. Claude Code turns use its built-in WebSearch instead.
	form := url.Values{"q": {a.S("query")}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ua)
	resp, err := httpc.Do(req)
	if err != nil {
		return Result{Text: "search failed: " + err.Error(), IsError: true}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	links := ddgLink.FindAllStringSubmatch(string(body), 8)
	snips := ddgSnippet.FindAllStringSubmatch(string(body), 8)
	var b strings.Builder
	for i, l := range links {
		href := l[1]
		if u, err := url.Parse(html.UnescapeString(href)); err == nil && u.Query().Get("uddg") != "" {
			href = u.Query().Get("uddg")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, text(l[2]), href)
		if i < len(snips) {
			fmt.Fprintf(&b, "   %s\n", clip(text(snips[i][1]), 240))
		}
	}
	if b.Len() == 0 {
		return Result{Text: "no results (search may be rate-limited)", IsError: true}
	}
	return Result{Text: b.String()}
}

func (r *Registry) fetch(ctx context.Context, c *Ctx, a Args) Result {
	u := a.S("url")
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", ua)
	if resp, err := httpc.Do(req); err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode == 200 {
			if t := pageText(string(body)); len(t) > 200 {
				return Result{Text: clip(t, 7000)}
			}
		}
	}
	return Result{Text: "fetch failed: no readable page at " + u, IsError: true}
}

func pageText(h string) string {
	h = dropRe.ReplaceAllString(h, " ")
	h = strings.NewReplacer("</p>", "\n", "<br>", "\n", "</li>", "\n", "</h1>", "\n", "</h2>", "\n", "</h3>", "\n", "</tr>", "\n").Replace(h)
	return strings.TrimSpace(blankRe.ReplaceAllString(spaceRe.ReplaceAllString(html.UnescapeString(tagRe.ReplaceAllString(h, " ")), " "), "\n\n"))
}

func text(h string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(html.UnescapeString(tagRe.ReplaceAllString(h, "")), " "))
}

// ---- notes output shaping ----

// compactNoteSearch keeps titles, permalinks and a short match per hit.
func compactNoteSearch(raw string) string {
	s := unwrapResult(raw)
	var v struct {
		Results []struct {
			Title, Permalink, Content, Matched_chunk string
		}
	}
	if json.Unmarshal([]byte(s), &v) == nil && len(v.Results) > 0 {
		var b strings.Builder
		for _, x := range v.Results {
			snip := x.Matched_chunk
			if snip == "" {
				snip = x.Content
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", x.Title, x.Permalink, clip(strings.Join(strings.Fields(snip), " "), 240))
		}
		return b.String()
	}
	return clip(s, 3000)
}

// unwrapResult strips the {"result": "..."} envelope Basic Memory uses.
func unwrapResult(s string) string {
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) == nil {
		if r, ok := v["result"]; ok {
			switch x := r.(type) {
			case string:
				return x
			default:
				j, _ := json.Marshal(x)
				return string(j)
			}
		}
	}
	return s
}
