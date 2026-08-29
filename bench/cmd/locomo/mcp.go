package main

// mcp.go — the minimal MCP (JSON-RPC over Streamable HTTP) client the answer
// axis needs.
//
// WHY MCP and not the REST memory routes: the memory-LAYER ops (`add`,
// `recall`) and `spawn_run` exist only as in-band tools. The REST family under
// /v1/_memory covers the k/v plane, embeddings and search — not `add`. So the
// consolidation path is reachable off-run through the MCP transport or not at
// all.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MCPClient is one MCP session against a loomcycle instance.
type MCPClient struct {
	base   string
	bearer string
	httpc  *http.Client

	mu        sync.Mutex
	sessionID string
	nextID    int
	inited    bool
}

func NewMCPClient(base, bearer string, timeout time.Duration) *MCPClient {
	return &MCPClient{
		base:   strings.TrimRight(base, "/"),
		bearer: bearer,
		httpc:  &http.Client{Timeout: timeout},
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// toolResult is the MCP tools/call envelope. A tool that fails sets isError and
// puts the diagnosis in the text content rather than returning a JSON-RPC error.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (c *MCPClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	session := c.sessionID
	c.mu.Unlock()

	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/_mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Both media types per the Streamable HTTP spec — a strict server 406s
	// otherwise, and the response may come back either framed or plain.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp %s: %s: %s", method, resp.Status, truncate(strings.TrimSpace(string(body)), 300))
	}
	var out rpcResponse
	if err := json.Unmarshal(unframeSSE(body), &out); err != nil {
		return nil, fmt.Errorf("mcp %s: decode: %w", method, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp %s: %d %s", method, out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// unframeSSE pulls the JSON out of an SSE-framed response. The transport
// advertises both media types, so the server may answer either way.
func unframeSSE(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, "event:") && !strings.HasPrefix(s, "data:") {
		return b
	}
	for _, line := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			return []byte(strings.TrimSpace(rest))
		}
	}
	return b
}

// ensureInit performs the handshake once per client.
func (c *MCPClient) ensureInit(ctx context.Context) error {
	c.mu.Lock()
	done := c.inited
	c.mu.Unlock()
	if done {
		return nil
	}
	if _, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "locomo-bench", "version": "1"},
	}); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	c.mu.Lock()
	c.inited = true
	c.mu.Unlock()
	return nil
}

// CallTool invokes one MCP tool and returns its text content. A tool-level
// failure (isError) is returned as an error carrying the server's diagnosis.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := c.ensureInit(ctx); err != nil {
		return "", err
	}
	res, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var tr toolResult
	if err := json.Unmarshal(res, &tr); err != nil {
		return "", fmt.Errorf("mcp %s: decode tool result: %w", name, err)
	}
	var sb strings.Builder
	for _, c := range tr.Content {
		sb.WriteString(c.Text)
	}
	if tr.IsError {
		return sb.String(), fmt.Errorf("mcp tool %s failed: %s", name, truncate(sb.String(), 400))
	}
	return sb.String(), nil
}

// RunResult is the subset of a spawn_run ack this harness reads.
type RunResult struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason"`
	FinalText  string `json:"final_text"`
	Error      string `json:"error"`
	Usage      struct {
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
	} `json:"usage"`
}

// SpawnRun runs one agent to completion. userID selects the memory scope the
// run's `scope=user` ops resolve to — which is how a conversation's facts are
// addressed, since scope_id is server-derived and never caller-supplied.
func (c *MCPClient) SpawnRun(ctx context.Context, agent, userID, prompt string) (RunResult, error) {
	args := map[string]any{
		"agent": agent,
		// segments, not a bare prompt: spawn_run refuses a fresh run with no
		// user turn.
		"segments": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "trusted-text", "text": prompt}},
		}},
	}
	if userID != "" {
		args["user_id"] = userID
	}
	txt, err := c.CallTool(ctx, "spawn_run", args)
	if err != nil {
		return RunResult{}, err
	}
	var rr RunResult
	if err := json.Unmarshal([]byte(txt), &rr); err != nil {
		return RunResult{}, fmt.Errorf("spawn_run: decode ack: %w (got %s)", err, truncate(txt, 200))
	}
	if rr.Status != "completed" {
		return rr, fmt.Errorf("run %s: status=%s %s", rr.RunID, rr.Status, truncate(rr.Error, 200))
	}
	return rr, nil
}

// MemoryAdd enqueues one span of conversation onto the consolidation queue.
func (c *MCPClient) MemoryAdd(ctx context.Context, scope string, msgs []LayerMessage) (string, error) {
	items := make([]any, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, map[string]any{"role": m.Role, "content": m.Content})
	}
	txt, err := c.CallTool(ctx, "memory", map[string]any{
		"op": "add", "scope": scope, "messages": items,
	})
	if err != nil {
		return "", err
	}
	var ack struct {
		EventID string `json:"event_id"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal([]byte(txt), &ack)
	return ack.EventID, nil
}

// LayerMessage is one turn as the memory layer takes it.
type LayerMessage struct {
	Role    string
	Content string
}
