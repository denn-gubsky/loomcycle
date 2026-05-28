// Package mcptest provides a minimal httptest-based MCP Streamable-HTTP
// server suitable for end-to-end tests that exercise the MCP HTTP
// client without standing up a real upstream.
//
// The server speaks just enough of the wire protocol to satisfy the
// loomcycle MCP HTTP client's handshake (initialize → notifications/
// initialized → tools/list → tools/call). It exposes ONE tool —
// `check_user` — whose handler:
//
//  1. Reads the inbound Authorization header.
//  2. Reads the `user_id` argument from the tool call.
//  3. Compares: bearer == "Bearer " + user_id.
//  4. Returns {ok, observed_bearer, observed_user_id, expected_bearer}.
//  5. Increments per-user_id + per-bearer counters atomically so the
//     test can assert call totals and bearer correctness.
//
// Intended use case is the v0.12.7 compound test
// (internal/api/http/scheduler_bearer_compound_test.go) which spins
// up TWO instances — one per agent-allowed-tool — to prove that
// per-server bearer header substitution propagates independently
// under cascading scheduler load.
//
// NOT intended as a full MCP server implementation. Notification
// handling is minimal; capability negotiation is just enough to pass
// the client's handshake. Anything beyond `check_user` returns a
// JSON-RPC method-not-found.
package mcptest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// Server wraps the underlying httptest.Server and exposes thread-safe
// accessors for the call counters + the per-call log. The test driver
// reads these after the cascade to assert correctness.
type Server struct {
	*httptest.Server

	// toolName is the single tool exposed. Defaults to "check_user"
	// but the constructor accepts an override so the compound test can
	// stand up two distinct servers with two distinct tool names.
	toolName string

	// Total request count across the entire server lifetime. Includes
	// the handshake (initialize + tools/list) so callers should
	// subtract the handshake count when asserting tool-call totals.
	requests atomic.Int64

	// toolCalls counts JUST tools/call invocations on `toolName`.
	toolCalls atomic.Int64

	// matchedBearers counts tool calls whose bearer matched the
	// expected pattern "Bearer " + user_id.
	matchedBearers atomic.Int64

	// log captures every tool call's (bearer, user_id) pair so the
	// test can assert per-user isolation (each user_id should appear
	// exactly once when the compound test fires one schedule per user).
	mu  sync.Mutex
	log []CallRecord
}

// CallRecord is one entry in the server's per-call log.
type CallRecord struct {
	UserID         string
	ObservedBearer string
	Matched        bool
}

// Option configures a Server at construction.
type Option func(*Server)

// WithToolName overrides the default "check_user" tool name. Used by
// the compound test to expose tools with distinct mcp__ names so the
// agent's allowed_tools list can route to specific servers.
func WithToolName(name string) Option {
	return func(s *Server) { s.toolName = name }
}

// NewServer returns a ready-to-use Server. The test must Close() it
// in a Cleanup hook (or defer) to release the port.
func NewServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	s := &Server{toolName: "check_user"}
	for _, opt := range opts {
		opt(s)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// ToolCalls returns the total tools/call hit count for the exposed tool.
func (s *Server) ToolCalls() int64 { return s.toolCalls.Load() }

// MatchedBearers returns the count of tool calls whose Authorization
// header matched the expected "Bearer " + user_id pattern.
func (s *Server) MatchedBearers() int64 { return s.matchedBearers.Load() }

// MismatchedBearers is a convenience accessor: total calls minus
// matched. Useful in the compound test's assertion line.
func (s *Server) MismatchedBearers() int64 {
	return s.toolCalls.Load() - s.matchedBearers.Load()
}

// CallLog returns a snapshot of every recorded tool call. Safe to call
// after Close(); the underlying slice is copied to avoid aliasing
// concurrent appends.
func (s *Server) CallLog() []CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CallRecord, len(s.log))
	copy(out, s.log)
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &probe)

	if probe.Method == "initialize" {
		// Set a per-server session id so the client's session-id
		// tracking exercise gets a non-empty value.
		w.Header().Set("Mcp-Session-Id", "s_mcptest")
	}

	// Notifications carry no id; respond with 202 and no body.
	if probe.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch probe.Method {
	case "initialize":
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      *probe.ID,
			"result": map[string]any{
				"protocolVersion": loommcp.ProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mcptest", "version": "0.1"},
			},
		})
	case "tools/list":
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      *probe.ID,
			"result": map[string]any{
				"tools": []map[string]any{
					{
						"name":        s.toolName,
						"description": "Validates that the inbound bearer matches the supplied user_id.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"user_id": map[string]any{"type": "string"},
							},
							"required": []string{"user_id"},
						},
					},
				},
			},
		})
	case "tools/call":
		s.handleToolCall(w, r, *probe.ID, probe.Params)
	default:
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      *probe.ID,
			"error":   map[string]any{"code": -32601, "message": "method not found"},
		})
	}
}

// handleToolCall is the bearer-validation core of the test server.
// The contract: bearer must be "Bearer " + user_id. The compound test
// constructs schedule forks where each user_id is identical to its
// `user_token` credential, so the round-trip succeeds for every call.
// Any mismatch (e.g. credential races, header substitution bugs)
// surfaces as MismatchedBearers() > 0.
func (s *Server) handleToolCall(w http.ResponseWriter, r *http.Request, id int64, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Name != s.toolName {
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32602, "message": "unknown tool: " + p.Name},
		})
		return
	}
	s.toolCalls.Add(1)

	var args struct {
		UserID string `json:"user_id"`
	}
	_ = json.Unmarshal(p.Arguments, &args)

	observedBearer := r.Header.Get("Authorization")
	expectedBearer := "Bearer " + args.UserID
	matched := observedBearer == expectedBearer
	if matched {
		s.matchedBearers.Add(1)
	}

	s.mu.Lock()
	s.log = append(s.log, CallRecord{
		UserID:         args.UserID,
		ObservedBearer: observedBearer,
		Matched:        matched,
	})
	s.mu.Unlock()

	// The MCP tool-call response shape: a `content` array with at
	// least one text block. The compound test doesn't parse the body
	// (it asserts on the server-side counters) but we return useful
	// detail for debugging when an operator runs the test by hand.
	resultText, _ := json.Marshal(map[string]any{
		"ok":               matched,
		"observed_bearer":  observedBearer,
		"expected_bearer":  expectedBearer,
		"observed_user_id": args.UserID,
	})
	writeJSON(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(resultText)},
			},
		},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
