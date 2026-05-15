// Package runner drives loomcycle's HTTP MCP transport from the bench
// harness. One Client holds one Mcp-Session-Id for the lifetime of a
// sweep; concurrent goroutines share it (the http_sessions store reaps
// after 30 min inactivity, more than enough for any single case).
//
// Wire shape (per RFC v0.8.15.3):
//   - POST /v1/_mcp with method=initialize → response header
//     Mcp-Session-Id carries the assigned session id.
//   - Subsequent POSTs send the header back. spawn_run with
//     runEvents=true returns text/event-stream; everything else
//     returns application/json.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client is a minimal HTTP MCP client geared toward the bench
// workflow: initialize once, register_agent + spawn_run + get_run
// many times. No support for arbitrary tool discovery (the bench
// knows the tool surface statically).
type Client struct {
	baseURL    string
	bearer     string
	httpc      *http.Client
	sessionID  string
	idCounter  atomic.Int64
	runEvents  bool
	serverInfo ServerInfo
}

// ServerInfo is the result of initialize, kept for logging/debug.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// NewClient constructs an HTTP MCP client. baseURL points at the
// loomcycle root (e.g. "http://127.0.0.1:8787"); bearer is the
// LOOMCYCLE_AUTH_TOKEN. httpc may be nil → uses a sane default with
// no overall timeout (per-request timeouts are passed via ctx).
func NewClient(baseURL, bearer string, httpc *http.Client) *Client {
	if httpc == nil {
		httpc = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		bearer:  bearer,
		httpc:   httpc,
	}
}

// SessionID is exposed for logging/debug.
func (c *Client) SessionID() string { return c.sessionID }

// Initialize opens a fresh MCP session and opts into runEvents so
// subsequent spawn_run calls return SSE streams with per-event
// notifications. Must be called before any tool call.
func (c *Client) Initialize(ctx context.Context) error {
	id := c.nextID()
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"loomcycle": map[string]any{"runEvents": true},
		},
		"clientInfo": map[string]any{
			"name":    "lc-bench",
			"version": "0.1.0",
		},
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params":  params,
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/_mcp", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("initialize: HTTP %d: %s", resp.StatusCode, string(b))
	}

	c.sessionID = resp.Header.Get("Mcp-Session-Id")
	if c.sessionID == "" {
		return fmt.Errorf("initialize: server did not return Mcp-Session-Id header")
	}

	var result struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Result  struct {
			ProtocolVersion string         `json:"protocolVersion"`
			ServerInfo      ServerInfo     `json:"serverInfo"`
			Capabilities    map[string]any `json:"capabilities"`
			Instructions    string         `json:"instructions,omitempty"`
		} `json:"result"`
		Error *jsonRPCError `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("initialize: decode response: %w", err)
	}
	if result.Error != nil {
		return fmt.Errorf("initialize: %s", result.Error)
	}
	c.serverInfo = result.Result.ServerInfo

	// runEvents is enabled IFF the server echoes it back in
	// capabilities.loomcycle.runEvents. Servers that ignore the hint
	// fall back to blocking JSON responses for spawn_run.
	if loom, ok := result.Result.Capabilities["loomcycle"].(map[string]any); ok {
		if v, ok := loom["runEvents"].(bool); ok && v {
			c.runEvents = true
		}
	}

	// Send the notifications/initialized frame per MCP spec. Server
	// returns 204 / empty result; we don't care about the body.
	if err := c.sendNotification(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialize: send initialized notif: %w", err)
	}

	return nil
}

// RegisterAgent registers a dynamic agent with the supplied
// system_prompt + allowed_tools + provider/model pin. Returns the
// echo of the registered name on success.
func (c *Client) RegisterAgent(ctx context.Context, args RegisterAgentArgs) error {
	in, _ := json.Marshal(args)
	var raw json.RawMessage
	if err := c.callTool(ctx, "register_agent", in, &raw); err != nil {
		return err
	}
	// Result body is operator-info ({"name": "...", "ttl_seconds": ...}).
	// The bench doesn't need to parse it; success = no error.
	_ = raw
	return nil
}

// RegisterAgentArgs is the payload shape for register_agent (mirrors
// the MCP tool's schema in internal/api/mcp/tools.go:84).
type RegisterAgentArgs struct {
	Name         string   `json:"name"`
	SystemPrompt string   `json:"system_prompt"`
	AllowedTools []string `json:"allowed_tools"`
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
	Tier         string   `json:"tier,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	MaxTokens    int      `json:"max_tokens,omitempty"`
	Description  string   `json:"description,omitempty"`
	TTLSeconds   int      `json:"ttl_seconds,omitempty"`
}

// SpawnRun runs the named agent with one user turn and waits for
// terminal state. When the session opted into runEvents, the server
// streams events as SSE notifications; we accumulate them into the
// returned RunResult.Events slice. Otherwise the result is a single
// blocking JSON response and Events is empty.
func (c *Client) SpawnRun(ctx context.Context, args SpawnRunArgs) (RunResult, error) {
	in, _ := json.Marshal(args)
	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "spawn_run",
			"arguments": json.RawMessage(in),
		},
	}
	raw, _ := json.Marshal(frame)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/_mcp", bytes.NewReader(raw))
	if err != nil {
		return RunResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return RunResult{}, fmt.Errorf("spawn_run: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return RunResult{}, fmt.Errorf("spawn_run: HTTP %d: %s", resp.StatusCode, string(b))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return c.readSpawnRunSSE(resp.Body)
	}
	return c.readSpawnRunJSON(resp.Body)
}

// SpawnRunArgs mirrors the spawn_run MCP tool's arguments
// (internal/api/mcp/tools.go:25). The bench always sends one user
// segment with one trusted-text content block.
//
// AllowedTools narrows the agent's tool surface for THIS call.
// Empty/nil = use the agent's registered allowlist (the union we
// register on first agent-create); non-empty = intersect with that
// allowlist. The bench uses this to make cases that declare
// `allowed_tools: ["X"]` actually restrict the model to X at
// runtime, not just check at grading time. Pass an empty (non-nil)
// slice to deny all tools for a case.
type SpawnRunArgs struct {
	Agent        string          `json:"agent"`
	Segments     []PromptSegment `json:"segments"`
	TenantID     string          `json:"tenant_id,omitempty"`
	UserID       string          `json:"user_id,omitempty"`
	AgentID      string          `json:"agent_id,omitempty"`
	UserTier     string          `json:"user_tier,omitempty"`
	AllowedTools []string        `json:"allowed_tools,omitempty"`
}

// PromptSegment mirrors loop.PromptSegment on the wire.
type PromptSegment struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock mirrors loop.PromptContentBlock. The bench only emits
// {type: "trusted-text", text: "..."}.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UserTextSegment is the canonical bench prompt segment — one user
// turn with one trusted-text content block.
func UserTextSegment(text string) PromptSegment {
	return PromptSegment{
		Role: "user",
		Content: []ContentBlock{
			{Type: "trusted-text", Text: text},
		},
	}
}

// RunResult is what the bench needs from a spawn_run: the final text
// (graded structurally + semantically) and the captured event trace
// (graded functionally). Status mirrors connector.SpawnRunResult.Status.
type RunResult struct {
	AgentID    string          `json:"agent_id"`
	RunID      string          `json:"run_id"`
	SessionID  string          `json:"session_id"`
	Status     string          `json:"status"` // "completed" | "failed" | "cancelled"
	StopReason string          `json:"stop_reason"`
	FinalText  string          `json:"final_text"`
	Usage      *UsageInfo      `json:"usage,omitempty"`
	Events     []ProviderEvent `json:"events,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// UsageInfo mirrors providers.Usage on the wire (input/output token
// counts). Marshalled into the trace JSON for cost rollups.
type UsageInfo struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_input_tokens,omitempty"`
	Model               string `json:"model,omitempty"`
}

// ProviderEvent mirrors providers.Event on the wire. Only the fields
// the bench actually consumes are declared — the graders need Type,
// ToolUse, Text, Usage, and StopReason; the rest pass through as
// raw JSON in case future cases want to inspect them.
//
// ErrorMessage captures the providers.Event.Error string so traces
// preserve why a run got an EventError (provider content-filter,
// rate limit, mid-stream stall). Without this the bench grader sees
// only `{"type":"error"}` and the operator can't tell why empty
// final_text rows happened.
type ProviderEvent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ToolUse      *ToolUseEvent   `json:"tool_use,omitempty"`
	Usage        *UsageInfo      `json:"usage,omitempty"`
	StopReason   string          `json:"stop_reason,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	ErrorMessage string          `json:"error,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

// ToolUseEvent is the inner tool_use payload (matches providers.ToolUse).
type ToolUseEvent struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Close terminates the MCP session via DELETE /v1/_mcp. Best-effort.
func (c *Client) Close(ctx context.Context) error {
	if c.sessionID == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/_mcp", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// --- Internals ---

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

func (c *Client) nextID() int64 { return c.idCounter.Add(1) }

// callTool sends a tools/call JSON-RPC frame and decodes the result
// into out. Used for any tool whose response is application/json (not
// the spawn_run SSE path). out may be nil if the caller doesn't need
// the result body.
func (c *Client) callTool(ctx context.Context, name string, args json.RawMessage, out *json.RawMessage) error {
	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	raw, _ := json.Marshal(frame)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/_mcp", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: HTTP %d: %s", name, resp.StatusCode, string(b))
	}

	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *jsonRPCError   `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("%s: decode: %w", name, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %w", name, env.Error)
	}

	// tool/call result envelope: {"content":[{"type":"text","text":"..."}], "isError": bool}
	// For tools that return JSON in the text field (the loomcycle
	// convention via toolResultJSON), we surface the inner text raw.
	var toolResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &toolResult); err != nil {
		return fmt.Errorf("%s: decode tool result: %w", name, err)
	}
	if toolResult.IsError {
		msg := "tool returned isError=true"
		if len(toolResult.Content) > 0 {
			msg = toolResult.Content[0].Text
		}
		return fmt.Errorf("%s: %s", name, msg)
	}
	if out != nil && len(toolResult.Content) > 0 {
		*out = json.RawMessage(toolResult.Content[0].Text)
	}
	return nil
}

func (c *Client) sendNotification(ctx context.Context, method string, params any) error {
	frame := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		frame["params"] = params
	}
	raw, _ := json.Marshal(frame)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/_mcp", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// readSpawnRunJSON parses a blocking JSON spawn_run response.
func (c *Client) readSpawnRunJSON(body io.Reader) (RunResult, error) {
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *jsonRPCError   `json:"error,omitempty"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return RunResult{}, fmt.Errorf("decode spawn_run response: %w", err)
	}
	if env.Error != nil {
		return RunResult{}, env.Error
	}
	return parseSpawnRunResult(env.Result)
}

// readSpawnRunSSE consumes the SSE stream, captures every
// notifications/loomcycle/run_event frame's Event into RunResult.Events,
// and parses the final tools/call response (which arrives as a regular
// SSE frame with a non-nil id matching the spawn_run request).
func (c *Client) readSpawnRunSSE(body io.Reader) (RunResult, error) {
	var result RunResult
	br := bufio.NewReader(body)
	var dataBuf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// Drain whatever's in the buffer; some servers don't
				// emit a trailing \n\n on the final frame.
				if dataBuf.Len() > 0 {
					if dispatchErr := dispatchSSEFrame(dataBuf.Bytes(), &result); dispatchErr != nil {
						return result, dispatchErr
					}
				}
				break
			}
			return result, fmt.Errorf("read SSE: %w", err)
		}
		// SSE frame terminator: empty line (just "\n") after one or
		// more "data: ..." lines.
		if line == "\n" || line == "\r\n" {
			if dataBuf.Len() > 0 {
				if err := dispatchSSEFrame(dataBuf.Bytes(), &result); err != nil {
					return result, err
				}
				dataBuf.Reset()
			}
			continue
		}
		const dataPrefix = "data: "
		if strings.HasPrefix(line, dataPrefix) {
			dataBuf.WriteString(strings.TrimRight(line[len(dataPrefix):], "\r\n"))
		}
		// Ignore comment lines (": ...") and event/id/retry fields —
		// not used by loomcycle's HTTP MCP today.
	}
	return result, nil
}

// dispatchSSEFrame parses one JSON-RPC frame (notification or final
// result) and updates result accordingly.
func dispatchSSEFrame(data []byte, result *RunResult) error {
	var probe struct {
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Params json.RawMessage `json:"params"`
		Error  *jsonRPCError   `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("decode SSE frame: %w (body=%q)", err, truncate(string(data), 256))
	}
	if probe.Error != nil {
		return probe.Error
	}
	if probe.Method == "notifications/loomcycle/run_event" {
		var payload struct {
			RunID   string        `json:"run_id"`
			AgentID string        `json:"agent_id"`
			Event   ProviderEvent `json:"event"`
		}
		if err := json.Unmarshal(probe.Params, &payload); err != nil {
			return fmt.Errorf("decode run_event: %w", err)
		}
		// Capture the raw event JSON for trace artifacts.
		var rawParams struct {
			Event json.RawMessage `json:"event"`
		}
		_ = json.Unmarshal(probe.Params, &rawParams)
		payload.Event.Raw = rawParams.Event
		if result.RunID == "" {
			result.RunID = payload.RunID
		}
		if result.AgentID == "" {
			result.AgentID = payload.AgentID
		}
		result.Events = append(result.Events, payload.Event)
		return nil
	}
	// Final tools/call response frame.
	if len(probe.Result) > 0 {
		final, err := parseSpawnRunResult(probe.Result)
		if err != nil {
			return err
		}
		// Preserve the events collected so far.
		final.Events = result.Events
		*result = final
		return nil
	}
	return nil
}

// parseSpawnRunResult extracts the SpawnRunResult shape from a
// tools/call response envelope.
func parseSpawnRunResult(rawResult json.RawMessage) (RunResult, error) {
	var toolResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rawResult, &toolResult); err != nil {
		return RunResult{}, fmt.Errorf("decode tools/call result: %w", err)
	}
	if len(toolResult.Content) == 0 {
		return RunResult{}, fmt.Errorf("tools/call result: no content")
	}
	body := toolResult.Content[0].Text
	var inner RunResult
	if err := json.Unmarshal([]byte(body), &inner); err != nil {
		// Some error paths emit human-readable text instead of the
		// SpawnRunResult JSON. Surface it as the Error field.
		inner.Status = "failed"
		inner.Error = body
		return inner, nil
	}
	if toolResult.IsError && inner.Error == "" {
		inner.Error = body
	}
	return inner, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
