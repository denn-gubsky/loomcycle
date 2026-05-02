package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Caller is the minimum surface a transport must expose to drive the MCP
// protocol. Both stdio.Client and http.Client satisfy this so the
// protocol-layer helpers below stay transport-agnostic.
type Caller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Notify sends a fire-and-forget notification. The ctx caps the time
	// the implementation may spend writing — needed for stdio, where a
	// stuck pipe could otherwise block past the caller's deadline.
	Notify(ctx context.Context, method string, params any) error
	// Healthy reports whether the underlying transport is still able to
	// serve calls. For stdio: true while the child is alive. For HTTP:
	// effectively always true (stateless), unless a session has been
	// invalidated and we know about it. The Pool uses this to decide
	// whether to evict + respawn before returning a cached entry.
	Healthy() bool
}

// ProtocolVersion is what we advertise to MCP servers. 2024-11-05 is the
// current widely-supported version; 2025-06-18 added structured content but
// is server-side-optional. Bumping requires adjusting any tools/list result
// shape we depend on.
const ProtocolVersion = "2024-11-05"

// ClientInfo is the "we are loomcycle" advert sent in initialize.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams is the body of the `initialize` request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// InitializeResult is the server's reply.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
}

// ServerInfo is the server's identification.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolsListResult is the body of `tools/list` responses.
type ToolsListResult struct {
	Tools []ToolDescriptor `json:"tools"`
}

// ToolDescriptor is one tool the server exposes.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CallToolParams is the body of `tools/call`.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// CallToolResult is the body of `tools/call` responses. MCP servers return
// `content` as a heterogeneous array of typed blocks; for now we only handle
// text blocks (the dominant case for jobs-search-agent's brave-search use).
// Future: image, resource, audio.
type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock is one piece of tool output. v0.3 supports type=="text"; other
// kinds round-trip as opaque raw JSON in the Raw field for callers that want
// to inspect them.
type ContentBlock struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Raw  json.RawMessage `json:"-"` // entire block JSON, set by UnmarshalJSON
}

// UnmarshalJSON preserves the raw block JSON in Raw so non-text blocks
// (image, resource, …) survive the round-trip even though we only typed-decode
// the text branch.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type wire struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	b.Type = w.Type
	b.Text = w.Text
	b.Raw = make(json.RawMessage, len(data))
	copy(b.Raw, data)
	return nil
}

// Initialize performs the MCP handshake: send `initialize`, receive the
// server's capabilities, send the `notifications/initialized` notification.
// After this call the connection is ready for `tools/list` and `tools/call`.
func Initialize(ctx context.Context, c Caller, clientName, clientVersion string) (*InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      ClientInfo{Name: clientName, Version: clientVersion},
	}
	raw, err := c.Call(ctx, "initialize", params)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("initialize result: %w", err)
	}
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		return nil, fmt.Errorf("initialized notify: %w", err)
	}
	return &res, nil
}

// ListTools fetches the server's tool descriptors via `tools/list`.
func ListTools(ctx context.Context, c Caller) ([]ToolDescriptor, error) {
	raw, err := c.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var res ToolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("tools/list result: %w", err)
	}
	return res.Tools, nil
}

// CallTool invokes a tool via `tools/call`. Arguments must be a JSON object
// matching the tool's input schema; encode it ahead of time and pass as
// json.RawMessage. Returns the typed CallToolResult; the server's `isError`
// flag is preserved.
func CallTool(ctx context.Context, c Caller, name string, args json.RawMessage) (*CallToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	raw, err := c.Call(ctx, "tools/call", CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("tools/call %s: %w", name, err)
	}
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("tools/call %s result: %w", name, err)
	}
	return &res, nil
}

// JoinTextContent concatenates all text blocks in a tool result, separating
// with newlines. Callers that need the raw structured content should walk
// res.Content directly.
func JoinTextContent(res *CallToolResult) string {
	if res == nil {
		return ""
	}
	var out string
	for i, b := range res.Content {
		if b.Type != "text" {
			continue
		}
		if i > 0 && out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out
}
