package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestCloseIsIdempotent(t *testing.T) {
	c, _ := New(Config{URL: "http://example.invalid"})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}
