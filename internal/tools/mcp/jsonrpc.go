// Package mcp speaks the Model Context Protocol — JSON-RPC 2.0 over either
// stdio (this package's stdio subpackage) or HTTP (planned). The root package
// holds the wire types and small helpers shared by both transports.
package mcp

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// Request is a JSON-RPC 2.0 request. MCP is strictly 2.0 — implementations
// must reject anything else.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (no ID, no expected response).
// MCP uses these for "initialized", server-side progress updates, etc.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Either Result or Error is set, not both.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil mcp error>"
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// IDGenerator hands out monotonically increasing IDs for outbound requests.
// Safe for concurrent use.
type IDGenerator struct{ n atomic.Int64 }

// Next returns a fresh request ID (positive, starting at 1).
func (g *IDGenerator) Next() int64 { return g.n.Add(1) }

// NewRequest builds a Request with the given method and params (params may
// be nil for parameterless methods).
func NewRequest(id int64, method string, params any) (Request, error) {
	req := Request{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return Request{}, fmt.Errorf("marshal params: %w", err)
		}
		req.Params = raw
	}
	return req, nil
}

// NewNotification builds a Notification with the given method and params.
func NewNotification(method string, params any) (Notification, error) {
	n := Notification{JSONRPC: "2.0", Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return Notification{}, fmt.Errorf("marshal params: %w", err)
		}
		n.Params = raw
	}
	return n, nil
}

// IncomingFrame represents one line read from an MCP transport. Exactly one
// of Response or Notification is non-nil. Returns ParseErr when the line is
// not a recognisable JSON-RPC frame; callers should log and skip.
type IncomingFrame struct {
	Response     *Response
	Notification *Notification
	ParseErr     error
}

// DecodeFrame inspects a single JSON-RPC frame and routes it to the right
// kind. JSON-RPC distinguishes by presence of "id": responses have one,
// notifications don't. We use a probe struct with explicit "id" presence.
func DecodeFrame(line []byte) IncomingFrame {
	var probe struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return IncomingFrame{ParseErr: fmt.Errorf("decode probe: %w", err)}
	}
	if probe.ID != nil {
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return IncomingFrame{ParseErr: fmt.Errorf("decode response: %w", err)}
		}
		return IncomingFrame{Response: &resp}
	}
	var n Notification
	if err := json.Unmarshal(line, &n); err != nil {
		return IncomingFrame{ParseErr: fmt.Errorf("decode notification: %w", err)}
	}
	return IncomingFrame{Notification: &n}
}
