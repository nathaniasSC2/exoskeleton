package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/proc"
)

// Claude drives the real Claude Code CLI headless, logged in with the user's
// plan (CLAUDE_CODE_OAUTH_TOKEN in the environment). One short-lived process
// per turn: exo owns the context, so nothing accumulates between turns.
type Claude struct {
	EngineName         string // defaults to "claude"
	Bin, Model, Effort string
	MCPBase, MCPToken  string // exo's own MCP server
	Agents             string // --agents JSON (the "opus" sub-agent); used when the turn allows the Agent tool
	WorkDir            string
	OnLimit            func(Limit)
}

func (c *Claude) Name() string {
	if c.EngineName != "" {
		return c.EngineName
	}
	return "claude"
}

// args builds the CLI flags. mcpPath selects exo's toolset ("12" for a chat
// turn, "browse/12" for a browsing sub-agent); "" means no tools at all.
func (c *Claude) args(req *Request, model, mcpPath string) []string {
	a := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", model, "--system-prompt", req.System,
		"--tools", req.BuiltIns, // "" disables every built-in
		"--strict-mcp-config", "--setting-sources", "", "--no-session-persistence",
		"--disable-slash-commands", "--dangerously-skip-permissions"}
	if e := req.Effort; e != "" {
		a = append(a, "--effort", e)
	} else if c.Effort != "" {
		a = append(a, "--effort", c.Effort)
	}
	if c.Agents != "" && strings.Contains(","+req.BuiltIns+",", ",Agent,") {
		a = append(a, "--agents", c.Agents)
	}
	if mcpPath != "" {
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"exo": map[string]any{
			"type": "http", "url": c.MCPBase + "/mcp/" + mcpPath,
			"headers": map[string]string{"Authorization": "Bearer " + c.MCPToken}}}})
		a = append(a, "--mcp-config", string(cfg))
	}
	return a
}

func (c *Claude) Turn(ctx context.Context, req *Request) *Result {
	model := c.Model
	if req.Model != "" {
		model = req.Model
	}
	return c.run(ctx, req, model, fmt.Sprint(req.TurnID))
}

// SubAgent runs a one-off agent on one of exo's toolsets
// and returns its final message. Its tool traffic goes to req.Trace, never
// into the caller's context.
func (c *Claude) SubAgent(ctx context.Context, req *Request, mcpPath string) *Result {
	return c.run(ctx, req, c.Model, mcpPath)
}

// Probe checks whether the plan path works again, as cheaply as possible.
func (c *Claude) Probe(ctx context.Context, model string) *Result {
	return c.run(ctx, &Request{System: "Reply with the single word ok.", User: []Block{{Text: "ok?"}}}, model, "")
}

func (c *Claude) run(ctx context.Context, req *Request, model, mcpPath string) *Result {
	res := &Result{}
	trace := req.Trace
	if trace == nil {
		trace = func(string, string, string) {}
	}
	os.MkdirAll(c.WorkDir, 0o755)
	cmd := exec.CommandContext(ctx, c.Bin, c.args(req, model, mcpPath)...)
	cmd.Dir = c.WorkDir
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1")
	proc.Group(cmd) // Bash children die with the turn
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &capped{w: &stderr, n: 8 << 10}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		res.Err, res.Kind = err, Other
		return res
	}

	content := []map[string]any{}
	if h := HistoryText(req.History); h != "" {
		content = append(content, map[string]any{"type": "text", "text": h})
	}
	for _, b := range req.User {
		if b.Image != "" {
			if data, err := os.ReadFile(b.Image); err == nil {
				content = append(content, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(data)}})
			}
			continue
		}
		content = append(content, map[string]any{"type": "text", "text": b.Text})
	}
	msg, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	stdin.Write(append(msg, '\n'))
	stdin.Close()

	var final map[string]any
	rd := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev map[string]any
			if json.Unmarshal(line, &ev) == nil {
				c.event(ev, res, trace, &final)
			}
		}
		if err != nil {
			break
		}
	}
	werr := cmd.Wait()

	if final != nil {
		if u, _ := final["usage"].(map[string]any); u != nil {
			res.Usage.In = num(u["input_tokens"])
			res.Usage.Out = num(u["output_tokens"])
			res.Usage.CacheRead = num(u["cache_read_input_tokens"])
			res.Usage.CacheWrite = num(u["cache_creation_input_tokens"])
		}
		res.Usage.Cost, _ = final["total_cost_usd"].(float64)
		res.Text, _ = final["result"].(string)
		if isErr, _ := final["is_error"].(bool); isErr || final["subtype"] != "success" {
			res.Err = fmt.Errorf("claude: %v %s", final["subtype"], clip(res.Text, 300))
		}
	} else {
		res.Err = fmt.Errorf("claude exited without a result: %v %s", werr, clip(stderr.String(), 400))
	}
	if res.Err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			res.Kind = Timeout
		case res.Limit != nil && res.Limit.Status == "rejected":
			res.Kind = RateLimit
		default:
			res.Kind = classify(res.Err.Error() + " " + stderr.String())
		}
		trace("error", string(res.Kind), res.Err.Error()+"\n"+stderr.String())
		res.Text = ""
	}
	return res
}

func (c *Claude) event(ev map[string]any, res *Result, trace func(kind, name, body string), final *map[string]any) {
	switch ev["type"] {
	case "system":
		if ev["subtype"] == "init" {
			var names []string
			if ts, _ := ev["tools"].([]any); ts != nil {
				for _, t := range ts {
					names = append(names, fmt.Sprint(t))
				}
			}
			trace("init", fmt.Sprint(ev["model"]), "tools: "+strings.Join(names, ", "))
		}
	case "assistant":
		msg, _ := ev["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			blk, _ := b.(map[string]any)
			switch blk["type"] {
			case "text":
				trace("text", "", fmt.Sprint(blk["text"]))
			case "tool_use":
				name := strings.TrimPrefix(fmt.Sprint(blk["name"]), "mcp__exo__")
				in, _ := json.Marshal(blk["input"])
				res.Tools = append(res.Tools, name)
				trace("tool_call", name, string(in))
			case "thinking":
				if t := strings.TrimSpace(fmt.Sprint(blk["thinking"])); t != "" && t != "<nil>" {
					trace("thinking", "", t)
				}
			}
		}
	case "user":
		msg, _ := ev["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			blk, _ := b.(map[string]any)
			if blk["type"] == "tool_result" {
				trace("tool_result", "", toolResultText(blk["content"]))
			}
		}
	case "rate_limit_event":
		info, _ := ev["rate_limit_info"].(map[string]any)
		l := Limit{Status: fmt.Sprint(info["status"]), Window: fmt.Sprint(info["rateLimitType"]), ResetsAt: num(info["resetsAt"])}
		if w, _ := info["unifiedWindows"].(map[string]any); w != nil {
			if f, _ := w["five_hour"].(map[string]any); f != nil {
				l.FiveHour, _ = f["utilization"].(float64)
			}
			if s, _ := w["seven_day"].(map[string]any); s != nil {
				l.SevenDay, _ = s["utilization"].(float64)
			}
		}
		res.Limit = &l
		if c.OnLimit != nil {
			c.OnLimit(l)
		}
	case "result":
		*final = ev
	}
}

func toolResultText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, p := range x {
			if m, _ := p.(map[string]any); m != nil {
				if m["type"] == "text" {
					b.WriteString(fmt.Sprint(m["text"]))
				} else {
					fmt.Fprintf(&b, "[%v]", m["type"])
				}
			}
		}
		return b.String()
	}
	j, _ := json.Marshal(v)
	return string(j)
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// capped keeps only the first n bytes written (stderr can be chatty).
type capped struct {
	w io.Writer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n > 0 {
		k := min(len(p), c.n)
		c.w.Write(p[:k])
		c.n -= k
	}
	return len(p), nil
}
