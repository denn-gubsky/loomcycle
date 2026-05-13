package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// fakeServer wraps httptest with a minimal MCP-over-HTTP responder.
// Mode picks the response set, mirroring the stdio test fakes.
type fakeServer struct {
	mode      string
	sessionID string
	requests  atomic.Int64
}

func newFakeServer(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	f := &fakeServer{mode: mode, sessionID: "s_" + mode}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &probe)

		// On the first call (probe is initialize), set the session header.
		if probe.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", f.sessionID)
		}

		// Notifications: 202 + empty body.
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
					"protocolVersion": mcp.ProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake-http", "version": "0.1"},
				},
			})
		case "tools/list":
			writeJSON(w, map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "search", "description": "search", "inputSchema": map[string]any{"type": "object"}},
					},
				},
			})
		case "tools/call":
			if f.mode == "session-invalidated" {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"session expired"}`))
				return
			}
			if f.mode == "rpc-error" {
				writeJSON(w, map[string]any{
					"jsonrpc": "2.0",
					"id":      *probe.ID,
					"error":   map[string]any{"code": -32603, "message": "synthetic"},
				})
				return
			}
			if f.mode == "auth-required" {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(probe.Params, &p)
			writeJSON(w, map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "got: " + p.Name}},
				},
			})
		}
	}))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestHandshakeAndCall(t *testing.T) {
	srv := newFakeServer(t, "ok")
	defer srv.Close()

	c, err := New(Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	init, err := mcp.Initialize(ctx, c, "loomcycle-test", "1.0")
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if init.ServerInfo.Name != "fake-http" {
		t.Errorf("ServerInfo: %+v", init.ServerInfo)
	}

	tools, err := mcp.ListTools(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "search" {
		t.Errorf("tools = %+v", tools)
	}

	res, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := mcp.JoinTextContent(res); got != "got: search" {
		t.Errorf("text = %q", got)
	}
}

// Regression: session ID returned on initialize must be sent on every
// subsequent request (including the next initialize call would the server
// expect that — we test by counting that the same header is on tools/list).
func TestSessionIDIsPropagated(t *testing.T) {
	var mu sync.Mutex
	var sessionsSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)

		mu.Lock()
		sessionsSeen = append(sessionsSeen, r.Header.Get("Mcp-Session-Id"))
		mu.Unlock()

		if probe.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "s_canonical")
		}
		if probe.ID != nil {
			writeJSON(w, map[string]any{
				"jsonrpc": "2.0", "id": *probe.ID,
				"result": map[string]any{
					"protocolVersion": mcp.ProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "x", "version": "1"},
					"tools":           []any{},
				},
			})
		}
	}))
	defer srv.Close()

	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mcp.ListTools(ctx, c); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sessionsSeen) < 3 {
		t.Fatalf("requests = %d, want >=3 (init, initialized notif, tools/list)", len(sessionsSeen))
	}
	if sessionsSeen[0] != "" {
		t.Errorf("first request had Mcp-Session-Id = %q (server hadn't issued one yet)", sessionsSeen[0])
	}
	for i, sid := range sessionsSeen[1:] {
		if sid != "s_canonical" {
			t.Errorf("post-init request %d: session = %q, want s_canonical", i+1, sid)
		}
	}
}

func TestRPCErrorPropagates(t *testing.T) {
	srv := newFakeServer(t, "rpc-error")
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}
	_, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "synthetic") {
		t.Errorf("error doesn't mention server message: %v", err)
	}
}

func TestSessionInvalidatedMarksDead(t *testing.T) {
	srv := newFakeServer(t, "session-invalidated")
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}
	if !c.Healthy() {
		t.Fatal("client should be Healthy before the 404")
	}

	_, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected 404 error")
	}
	if c.Healthy() {
		t.Errorf("client should be !Healthy after session invalidation")
	}

	// Subsequent calls fail fast without hitting the network.
	_, err = mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected dead-client error on subsequent call")
	}
	if !strings.Contains(err.Error(), "invalidated") {
		t.Errorf("error doesn't mention invalidation: %v", err)
	}
}

func TestHeadersForwarded(t *testing.T) {
	srv := newFakeServer(t, "auth-required")
	defer srv.Close()
	c, _ := New(Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}
	res, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(mcp.JoinTextContent(res), "got: search") {
		t.Errorf("got %q", mcp.JoinTextContent(res))
	}
}

// TestSSEResponseDecoded asserts that the client correctly parses
// a single-frame Streamable HTTP response delivered as
// `text/event-stream`. The official @modelcontextprotocol/sdk's
// WebStandardStreamableHTTPServerTransport defaults to SSE replies
// for initialize and tools/list when the client advertises
// text/event-stream in Accept; loomcycle previously fed the SSE body
// straight to json.Unmarshal and crashed on the leading `event:` line.
//
// Regression for the 2026-05-07 jobs-search-agent /api/mcp decode
// failure:
//
//	mcp http: decode response: invalid character 'e' looking for
//	beginning of value (body: event: message\ndata: {...})
func TestSSEResponseDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &probe)
		// Notifications (no id) → 202 Accepted, no body. mcp.Initialize
		// sends `notifications/initialized` after the initialize result;
		// reject anything that surfaces an error.
		if probe.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Always respond to requests with SSE-framed JSON, mirroring the
		// SDK's behaviour when the client advertises text/event-stream.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"sse-fake","version":"1"}}}`, *probe.ID))
	}))
	defer srv.Close()

	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := mcp.Initialize(ctx, c, "t", "1")
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.ServerInfo.Name != "sse-fake" {
		t.Errorf("ServerInfo.Name = %q, want sse-fake", info.ServerInfo.Name)
	}
}

// TestExtractSSEData asserts the SSE parser handles the shapes the
// MCP SDK actually emits + the spec-permitted variations: single-line
// data, multi-line data joined with \n, leading/trailing whitespace,
// CRLF line endings, ignored event/id/retry fields, and missing data.
func TestExtractSSEData(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"single data line", "event: message\ndata: {\"a\":1}\n\n", `{"a":1}`, true},
		{"no space after colon", "data:{\"a\":1}\n\n", `{"a":1}`, true},
		{"multi data joined", "data: line1\ndata: line2\n\n", "line1\nline2", true},
		{"crlf endings", "event: message\r\ndata: {\"a\":1}\r\n\r\n", `{"a":1}`, true},
		{"id and retry ignored", "id: 7\nretry: 1000\ndata: {\"a\":1}\n\n", `{"a":1}`, true},
		{"no data line", "event: message\n\n", "", false},
		{"empty body", "", "", false},
		{"first frame only", "data: A\n\ndata: B\n\n", "A", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractSSEData([]byte(tc.in))
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v (got=%q)", ok, tc.ok, string(got))
			}
			if ok && string(got) != tc.want {
				t.Errorf("got %q, want %q", string(got), tc.want)
			}
		})
	}
}

// TestAcceptHeaderIncludesBothMediaTypes asserts that the client
// advertises both application/json and text/event-stream in Accept.
// MCP Streamable HTTP servers that conform strictly (e.g., the
// official @modelcontextprotocol/sdk's StreamableHTTP transport)
// reject anything else with HTTP 406 Not Acceptable. Regression for
// the 2026-05-07 jobs-search-agent /api/mcp 406 incident.
func TestAcceptHeaderIncludesBothMediaTypes(t *testing.T) {
	var sawAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAccept = r.Header.Get("Accept")
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &probe)
		if probe.Method == "initialize" {
			writeJSON(w, map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake", "version": "1"},
				},
			})
			return
		}
		if probe.ID != nil {
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{}})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}

	// Both media types MUST appear in the Accept header.
	if !strings.Contains(sawAccept, "application/json") {
		t.Errorf("Accept missing application/json: got %q", sawAccept)
	}
	if !strings.Contains(sawAccept, "text/event-stream") {
		t.Errorf("Accept missing text/event-stream: got %q", sawAccept)
	}
}

func TestHeadersMissingReturnsAuthError(t *testing.T) {
	srv := newFakeServer(t, "auth-required")
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL}) // no headers
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}
	_, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error doesn't mention 401: %v", err)
	}
}

func TestCtxCancelMidCall(t *testing.T) {
	// Slow server: sleeps 200ms before responding so a 30ms ctx fires first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		var probe struct {
			ID *int64 `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &probe)
		if probe.ID != nil {
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{}})
		}
	}))
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, "tools/call", map[string]any{})
	if err == nil {
		t.Fatal("expected ctx err")
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error doesn't surface context: %v", err)
	}
}

// Regression: a server that returns a response with an id that doesn't
// match the request id is a JSON-RPC 2.0 protocol violation. The
// single-POST streamable-HTTP shape makes correlation implicit, but if
// a buggy server returns a stale id we surface it as an error rather
// than silently feeding the wrong result back to the agent loop.
func TestRPCResponseIDMismatchSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Initialize: respond with the right id (so handshake succeeds).
		// tools/call: respond with the WRONG id to simulate a buggy server.
		respID := *probe.ID
		if probe.Method == "tools/call" {
			respID = 99999
		}
		if probe.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "s")
		}
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      respID,
			"result":  map[string]any{},
		})
	}))
	defer srv.Close()

	c, _ := New(Config{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Call(ctx, "tools/call", map[string]any{})
	if err == nil {
		t.Fatal("expected id-mismatch error")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error doesn't mention id: %v", err)
	}
}

// Regression: Notify must honour the caller's ctx. Previously the http
// transport hardcoded a 30s background context for notifications, so a
// run-level ctx-cancel during the initialized notification (sent inside
// Initialize) would not propagate.
func TestNotifyHonoursCallerCtx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID *int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.ID == nil {
			// Notification — sleep so a 30ms ctx fires before we 202.
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}))
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := c.Notify(ctx, "loomcycle/probe", nil)
	if err == nil {
		t.Fatal("expected ctx error from Notify")
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %v, want context-related", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, _ := New(Config{URL: "http://example.invalid"})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}

// recordingHeaderServer is a fake MCP HTTP server that records every
// inbound request's headers (keyed by request count) and replies with
// a minimal MCP-valid response. Used by the v0.8.x per-run bearer
// tests below. Always tools/call-shaped: the test calls mcp.CallTool
// directly without a full Initialize handshake (Initialize is
// covered elsewhere; we exercise the header substitution path only).
type recordingHeaderServer struct {
	mu       sync.Mutex
	requests []http.Header
}

func (r *recordingHeaderServer) handler(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var probe struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &probe)

	r.mu.Lock()
	r.requests = append(r.requests, req.Header.Clone())
	r.mu.Unlock()

	if probe.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      *probe.ID,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		},
	})
}

// TestMcpHttpClient_PerRunBearerSubstitution covers the happy path:
// header value `Bearer ${run.user_bearer}` + ctx with a non-empty
// UserBearer → outbound Authorization header has the substituted token.
func TestMcpHttpClient_PerRunBearerSubstitution(t *testing.T) {
	rec := &recordingHeaderServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	c, _ := New(Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.user_bearer}"},
	})

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID:    "a_test",
		UserBearer: "run-token-abc123def4",
	})
	if _, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(rec.requests))
	}
	if got := rec.requests[0].Get("Authorization"); got != "Bearer run-token-abc123def4" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer run-token-abc123def4")
	}
}

// TestMcpHttpClient_ConcurrentRunsSendDistinctBearers is the regression
// guard for the per-run state bleed risk: two goroutines share the same
// Client (single instance, single c.headers) but each carries its own
// ctx-borne bearer. The fake server must see one request with bearer
// AAA and one with bearer BBB, never crossed and never with a literal
// "${run.*}" placeholder.
func TestMcpHttpClient_ConcurrentRunsSendDistinctBearers(t *testing.T) {
	rec := &recordingHeaderServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	c, _ := New(Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.user_bearer}"},
	})

	const bearerA = "tokenAAA1234567890aa"
	const bearerB = "tokenBBB1234567890bb"
	var wg sync.WaitGroup
	wg.Add(2)
	call := func(b string) {
		defer wg.Done()
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
			AgentID:    "a_" + b[:4],
			UserBearer: b,
		})
		if _, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`)); err != nil {
			t.Errorf("CallTool(%s): %v", b, err)
		}
	}
	go call(bearerA)
	go call(bearerB)
	wg.Wait()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(rec.requests))
	}
	sawA, sawB := false, false
	for i, h := range rec.requests {
		auth := h.Get("Authorization")
		if strings.Contains(auth, "${run.") {
			t.Errorf("request #%d Authorization = %q contains unresolved placeholder", i, auth)
		}
		switch auth {
		case "Bearer " + bearerA:
			sawA = true
		case "Bearer " + bearerB:
			sawB = true
		default:
			t.Errorf("request #%d Authorization = %q matches neither bearer", i, auth)
		}
	}
	if !sawA || !sawB {
		t.Errorf("expected both bearers present, sawA=%v sawB=%v", sawA, sawB)
	}
}

// TestMcpHttpClient_MissingBearerDropsHeader covers the strict-phase
// failure mode: header value `Bearer ${run.user_bearer}` (no fallback)
// + ctx with empty UserBearer → header is DROPPED, NOT shipped as
// literal placeholder. A WARN log line is emitted with the agent_id
// and triage-safe bearer prefix.
func TestMcpHttpClient_MissingBearerDropsHeader(t *testing.T) {
	rec := &recordingHeaderServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	// Capture log output so we can assert the WARN line shape.
	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	c, _ := New(Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.user_bearer}"},
	})

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID: "a_test",
		// UserBearer left empty intentionally.
	})
	if _, err := mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(rec.requests))
	}
	if got := rec.requests[0].Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty (header dropped)", got)
	}

	logLine := logBuf.String()
	if !strings.Contains(logLine, "${run.user_bearer} unresolved") {
		t.Errorf("WARN log missing expected phrase, got: %q", logLine)
	}
	if !strings.Contains(logLine, "Authorization") {
		t.Errorf("WARN log missing header name (Authorization), got: %q", logLine)
	}
	if !strings.Contains(logLine, "a_test") {
		t.Errorf("WARN log missing agent_id, got: %q", logLine)
	}
	if !strings.Contains(logLine, "bearer=(empty)") {
		t.Errorf("WARN log should report bearer=(empty), got: %q", logLine)
	}
}
