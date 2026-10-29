// Package mcp is a minimal Model Context Protocol implementation over
// streamable HTTP: a server that exposes exo's tools to Claude Code, and a
// client for the notes server. JSON responses only, no SSE streams, no sessions on
// the server side; that's all either peer needs.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nathaniasSC2/exoskeleton/internal/tools"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- server ----

type Server struct {
	Token    string
	Registry *tools.Registry
	Lookup   func(key string) *tools.Ctx // "turn:12", "browse:12", "job:3"
}

// ServeHTTP handles POST /mcp/{turn}, /mcp/browse/{turn} and /mcp/job/{id}.
// The path ties tool calls (and the files they produce) to the reply or job
// they belong to, and decides which toolset is offered.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/mcp/")
	kind := "turn"
	if k, id, ok := strings.Cut(rest, "/"); ok {
		kind, rest = k, id
	}
	n, _ := strconv.ParseInt(rest, 10, 64)
	tc := s.Lookup(kind + ":" + strconv.FormatInt(n, 10))
	if tc == nil {
		http.Error(w, "unknown or finished turn", 404)
		return
	}
	var req rpcReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if len(req.ID) == 0 { // notification
		w.WriteHeader(202)
		return
	}
	result, rerr := s.dispatch(r.Context(), tc, &req)
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) dispatch(ctx context.Context, tc *tools.Ctx, req *rpcReq) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "exo", "version": "1"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var list []map[string]any
		for _, t := range s.Registry.Exposed(tc) {
			list = append(list, map[string]any{"name": t.Name, "description": t.Desc, "inputSchema": t.Schema()})
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{-32602, "bad params"}
		}
		res := s.Registry.Call(ctx, tc, p.Name, p.Arguments)
		content := []map[string]any{{"type": "text", "text": res.Text}}
		if res.Image != "" {
			if b, err := os.ReadFile(res.Image); err == nil && len(b) < 4<<20 {
				content = append(content, map[string]any{"type": "image", "mimeType": "image/jpeg", "data": base64.StdEncoding.EncodeToString(b)})
			}
		}
		return map[string]any{"content": content, "isError": res.IsError}, nil
	}
	return nil, &rpcErr{-32601, "method not found: " + req.Method}
}

// ---- client ----

// Client calls tools on a remote MCP server (the notes). It initializes lazily
// and re-initializes once if the server forgets the session.
type Client struct {
	URL     string
	mu      sync.Mutex
	session string
	id      int
	http    *http.Client
}

func NewClient(url string) *Client {
	return &Client{URL: url, http: &http.Client{Timeout: 45 * time.Second}}
}

func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if c.session == "" {
			if err := c.initLocked(ctx); err != nil {
				return "", err
			}
		}
		res, status, err := c.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
		if status == 404 || status == 400 {
			c.session = ""
			continue
		}
		if err != nil {
			return "", err
		}
		var r struct {
			Content []struct{ Type, Text string }
			IsError bool
		}
		json.Unmarshal(res, &r)
		var b strings.Builder
		for _, x := range r.Content {
			if x.Type == "text" {
				b.WriteString(x.Text)
			}
		}
		if r.IsError {
			return "", fmt.Errorf("%s", b.String())
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("MCP session keeps expiring")
}

func (c *Client) initLocked(ctx context.Context) error {
	c.session = "pending"
	_, _, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "exo", "version": "1"}})
	if err != nil {
		c.session = ""
		return err
	}
	if c.session == "pending" {
		c.session = "none"
	}
	c.notify(ctx, "notifications/initialized")
	return nil
}

func (c *Client) post(ctx context.Context, body any) (*http.Response, error) {
	j, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(j))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" && c.session != "pending" && c.session != "none" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	return c.http.Do(req)
}

func (c *Client) notify(ctx context.Context, method string) {
	if resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method}); err == nil {
		resp.Body.Close()
	}
}

func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, int, error) {
	c.id++
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, resp.StatusCode, fmt.Errorf("mcp %s: %d %s", method, resp.StatusCode, b)
	}
	var raw []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(io.LimitReader(resp.Body, 16<<20))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				raw = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				var probe struct{ ID json.RawMessage }
				if json.Unmarshal(raw, &probe) == nil && len(probe.ID) > 0 {
					break
				}
			}
		}
	} else {
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	}
	var env struct {
		Result json.RawMessage
		Error  *rpcErr
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("mcp %s: bad response", method)
	}
	if env.Error != nil {
		return nil, resp.StatusCode, fmt.Errorf("mcp %s: %s", method, env.Error.Message)
	}
	return env.Result, resp.StatusCode, nil
}
