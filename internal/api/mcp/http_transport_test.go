package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// httpMockConnector is a Connector implementation for the HTTP MCP
// transport tests. Only methods exercised by the tests return non-zero
// values; the rest satisfy the interface with zero-value returns. Adding
// a new Connector method forces a compile failure here — exactly the
// signal we want.
type httpMockConnector struct {
	cancelCalls atomic.Int32
}

func (m *httpMockConnector) SpawnRun(context.Context, connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
	return connector.SpawnRunResult{
		AgentID: "a_http", RunID: "r_http", SessionID: "s_http", Status: "completed",
	}, nil
}
func (m *httpMockConnector) CancelRun(_ context.Context, _, _ string) (connector.CancelRunResult, error) {
	m.cancelCalls.Add(1)
	return connector.CancelRunResult{Cancelled: true}, nil
}
func (m *httpMockConnector) GetRun(context.Context, string) (connector.Run, error) {
	return connector.Run{}, nil
}
func (m *httpMockConnector) ListRuns(context.Context, connector.ListRunsFilter) ([]connector.Run, error) {
	return nil, nil
}
func (m *httpMockConnector) RegisterAgent(context.Context, connector.RegisterAgentRequest) (connector.AgentDescriptor, error) {
	return connector.AgentDescriptor{}, nil
}
func (m *httpMockConnector) UnregisterAgent(context.Context, string) error { return nil }
func (m *httpMockConnector) ListAgents(context.Context, bool) ([]connector.AgentDescriptor, error) {
	return nil, nil
}
func (m *httpMockConnector) Memory(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *httpMockConnector) Channel(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *httpMockConnector) AgentDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *httpMockConnector) Evaluation(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *httpMockConnector) Context(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *httpMockConnector) PauseRuntime(context.Context, int) (connector.PauseResult, error) {
	return connector.PauseResult{}, nil
}
func (m *httpMockConnector) ResumeRuntime(context.Context) (connector.ResumeResult, error) {
	return connector.ResumeResult{}, nil
}
func (m *httpMockConnector) GetRuntimeState(context.Context) (connector.RuntimeState, error) {
	return connector.RuntimeState{}, nil
}
func (m *httpMockConnector) CreateSnapshot(context.Context, connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	return connector.SnapshotDescriptor{}, nil
}
func (m *httpMockConnector) ListSnapshots(context.Context) ([]connector.SnapshotDescriptor, error) {
	return nil, nil
}
func (m *httpMockConnector) ExportSnapshot(context.Context, string) (connector.ExportSnapshotResult, error) {
	return connector.ExportSnapshotResult{}, errors.New("not implemented")
}
func (m *httpMockConnector) RestoreSnapshot(context.Context, connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	return connector.RestoreSnapshotResult{}, errors.New("not implemented")
}
func (m *httpMockConnector) DeleteSnapshot(context.Context, string) error {
	return errors.New("not implemented")
}

func (m *httpMockConnector) InterruptionResolve(context.Context, connector.InterruptionResolveRequest) (connector.InterruptionResolveResult, error) {
	return connector.InterruptionResolveResult{}, errors.New("not implemented")
}

// httpTestServer wires an HTTPHandler against the mock connector and
// returns an httptest.Server speaking the loomcycle MCP transport over
// real HTTP. Test bodies POST to ts.URL + "/" (the test server has no
// path routing — it sees every request).
func httpTestServer(t *testing.T, runner runner.Runner) (*httptest.Server, *httpMockConnector, *HTTPHandler) {
	t.Helper()
	mc := &httpMockConnector{}
	h := NewHTTPHandler(Config{
		Connector:     mc,
		Runner:        runner,
		Logf:          func(string, ...any) {},
		ServerName:    "loomcycle-test",
		ServerVersion: "v0.8.15.3-test",
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, mc, h
}

// postFrame sends one JSON-RPC frame to the server and returns the
// response. If sessionID is non-empty, it's added as the Mcp-Session-Id
// header. Returns (response, body bytes) — caller closes body.
func postFrame(t *testing.T, url, sessionID, payload string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, body
}

// --- HTTPSessionStore unit tests ---

func TestHTTPSessionStore_CreateAndGet(t *testing.T) {
	store := NewHTTPSessionStore(time.Minute)
	sess := NewSession()
	id := store.Create(sess)
	if id == "" {
		t.Fatal("Create returned empty ID")
	}
	if len(id) != 36 {
		t.Errorf("session ID length = %d, want 36 (UUIDv4 canonical form)", len(id))
	}
	got, ok := store.Get(id)
	if !ok {
		t.Fatal("Get returned ok=false for just-Created session")
	}
	if got != sess {
		t.Errorf("Get returned different *Session than Create stored")
	}
}

func TestHTTPSessionStore_GetUnknown(t *testing.T) {
	store := NewHTTPSessionStore(time.Minute)
	if _, ok := store.Get("00000000-0000-0000-0000-000000000000"); ok {
		t.Error("Get returned ok=true for unknown ID")
	}
}

func TestHTTPSessionStore_ExpiredOnGet(t *testing.T) {
	store := NewHTTPSessionStore(time.Millisecond)
	id := store.Create(NewSession())
	time.Sleep(5 * time.Millisecond)
	if _, ok := store.Get(id); ok {
		t.Error("Get returned ok=true for expired session (TTL elapsed)")
	}
	// Expired entry stays in the map until Sweep runs. Len reflects
	// the raw map size, not the "live" count.
	if n := store.Len(); n != 1 {
		t.Errorf("Len after expiry pre-sweep = %d, want 1", n)
	}
}

func TestHTTPSessionStore_GetExtendsTTL(t *testing.T) {
	store := NewHTTPSessionStore(100 * time.Millisecond)
	id := store.Create(NewSession())
	// Get just before expiry — should succeed AND reset the clock.
	time.Sleep(70 * time.Millisecond)
	if _, ok := store.Get(id); !ok {
		t.Fatal("first Get failed unexpectedly")
	}
	// Another 70ms — total elapsed is 140ms (> TTL) but only 70ms
	// since the last Get. Should still be live.
	time.Sleep(70 * time.Millisecond)
	if _, ok := store.Get(id); !ok {
		t.Error("Get failed after activity extended TTL — sliding window broken")
	}
}

func TestHTTPSessionStore_DeleteIsIdempotent(t *testing.T) {
	store := NewHTTPSessionStore(time.Minute)
	id := store.Create(NewSession())
	store.Delete(id)
	store.Delete(id)        // second delete should not panic
	store.Delete("missing") // unknown ID should not panic
	if _, ok := store.Get(id); ok {
		t.Error("Get returned ok=true after Delete")
	}
}

func TestHTTPSessionStore_Sweep(t *testing.T) {
	store := NewHTTPSessionStore(time.Millisecond)
	id1 := store.Create(NewSession())
	id2 := store.Create(NewSession())
	time.Sleep(5 * time.Millisecond)
	n := store.Sweep()
	if n != 2 {
		t.Errorf("Sweep deleted %d, want 2", n)
	}
	if store.Len() != 0 {
		t.Errorf("Len after Sweep = %d, want 0", store.Len())
	}
	// Subsequent Get on swept IDs returns false.
	if _, ok := store.Get(id1); ok {
		t.Error("Get id1 returned ok=true after Sweep")
	}
	if _, ok := store.Get(id2); ok {
		t.Error("Get id2 returned ok=true after Sweep")
	}
}

// --- HTTP transport integration tests ---

func TestHTTPTransport_InitializeAssignsSessionID(t *testing.T) {
	ts, _, h := httpTestServer(t, nil)
	resp, body := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("response missing Mcp-Session-Id header")
	}
	if len(sessionID) != 36 {
		t.Errorf("session ID length = %d, want 36", len(sessionID))
	}
	if h.Sessions().Len() != 1 {
		t.Errorf("sessions store len = %d, want 1", h.Sessions().Len())
	}
}

func TestHTTPTransport_SecondCallNeedsSessionHeader(t *testing.T) {
	ts, _, _ := httpTestServer(t, nil)
	// Skip initialize; go straight to tools/call without session.
	resp, body := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_runs","arguments":{"user_id":"u_a"}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400; body=%s", resp.StatusCode, body)
	}
}

func TestHTTPTransport_UnknownSessionReturns404WithJSONRPC(t *testing.T) {
	ts, _, _ := httpTestServer(t, nil)
	resp, body := postFrame(t, ts.URL, "11111111-2222-3333-4444-555555555555",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_runs","arguments":{"user_id":"u_a"}}}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
	var env struct {
		JSONRPC string `json:"jsonrpc"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response body not JSON-RPC: %v body=%s", err, body)
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", env.JSONRPC)
	}
	if env.Error == nil || env.Error.Code != -32001 {
		t.Errorf("expected error.code = -32001 (session not found), got %+v", env.Error)
	}
}

func TestHTTPTransport_FullSessionFlow(t *testing.T) {
	ts, mc, _ := httpTestServer(t, nil)
	// 1. initialize → session ID
	resp, body := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize status=%d body=%s", resp.StatusCode, body)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	// 2. tools/call cancel_run → dispatches through Connector
	resp, body = postFrame(t, ts.URL, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cancel_run","arguments":{"agent_id":"a_x"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("cancel_run status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	}
	if mc.cancelCalls.Load() != 1 {
		t.Errorf("Connector.CancelRun called %d times, want 1", mc.cancelCalls.Load())
	}
	// Echo session ID
	if resp.Header.Get("Mcp-Session-Id") != sessionID {
		t.Errorf("Mcp-Session-Id response header = %q, want %q",
			resp.Header.Get("Mcp-Session-Id"), sessionID)
	}
}

func TestHTTPTransport_DeleteTerminatesSession(t *testing.T) {
	ts, _, h := httpTestServer(t, nil)
	resp, _ := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if h.Sessions().Len() != 1 {
		t.Fatalf("session count after init = %d, want 1", h.Sessions().Len())
	}
	// DELETE the session
	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status=%d, want 204", delResp.StatusCode)
	}
	if h.Sessions().Len() != 0 {
		t.Errorf("session count after DELETE = %d, want 0", h.Sessions().Len())
	}
	// Subsequent calls with that session ID now 404.
	resp2, _ := postFrame(t, ts.URL, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_runs","arguments":{"user_id":"u"}}}`)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete status=%d, want 404", resp2.StatusCode)
	}
}

func TestHTTPTransport_MethodNotAllowed(t *testing.T) {
	ts, _, _ := httpTestServer(t, nil)
	resp, _ := http.Get(ts.URL)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status=%d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); !strings.Contains(got, "POST") {
		t.Errorf("Allow header = %q, want to contain POST", got)
	}
	_ = resp.Body.Close()
}

func TestHTTPTransport_BodyTooLarge(t *testing.T) {
	ts, _, _ := httpTestServer(t, nil)
	// 5MB body — exceeds the 4MB limit
	big := strings.Repeat("a", 5*1024*1024)
	payload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"` + big + `","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413", resp.StatusCode)
	}
}

// fakeStreamingRunner emits a scripted event sequence, used by the
// streaming SSE test to drive the spawn_run path on the HTTP transport.
type fakeStreamingRunner struct {
	events []providers.Event
}

func (f *fakeStreamingRunner) RunOnce(_ context.Context, _ runner.RunInput, cb runner.RunCallbacks) error {
	if cb.OnRegistered != nil {
		cb.OnRegistered("a_s", "r_s", "s_s", "")
	}
	for _, ev := range f.events {
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	}
	return nil
}

func TestHTTPTransport_SpawnRunStreaming_EmitsSSEFrames(t *testing.T) {
	fr := &fakeStreamingRunner{
		events: []providers.Event{
			{Type: providers.EventStarted},
			{Type: providers.EventText, Text: "hi"},
			{Type: providers.EventDone, StopReason: "end_turn"},
		},
	}
	ts, _, _ := httpTestServer(t, fr)

	// Initialize with runEvents opted in.
	resp, body := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"loomcycle":{"runEvents":true}},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize status=%d body=%s", resp.StatusCode, body)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")

	// spawn_run — should reply with text/event-stream.
	resp, body = postFrame(t, ts.URL, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa","segments":[]}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("spawn_run status=%d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// Body should contain SSE-framed JSON-RPC frames. Each frame:
	//   data: <json>\n\n
	// 3 events → 3 notifications → 3 SSE frames, plus 1 frame for the
	// final tools/call response = 4 total.
	frames := bytes.Count(body, []byte("\n\n"))
	if frames < 4 {
		t.Errorf("SSE frame count = %d, want >= 4 (3 notifications + 1 final response); body=%s", frames, body)
	}
	// Each frame starts with "data: ".
	if !bytes.Contains(body, []byte("data: ")) {
		t.Errorf("body missing 'data: ' prefix; body=%s", body)
	}
	// Notifications use the expected method name.
	if !bytes.Contains(body, []byte("notifications/loomcycle/run_event")) {
		t.Errorf("body missing run_event notification; body=%s", body)
	}
}

func TestHTTPTransport_SpawnRunWithoutOptIn_ReturnsJSONNotSSE(t *testing.T) {
	fr := &fakeStreamingRunner{
		events: []providers.Event{{Type: providers.EventDone, StopReason: "end_turn"}},
	}
	ts, _, _ := httpTestServer(t, fr)

	// Initialize WITHOUT runEvents.
	resp, _ := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	sessionID := resp.Header.Get("Mcp-Session-Id")

	// spawn_run — should reply with application/json (blocking path).
	resp, body := postFrame(t, ts.URL, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa","segments":[]}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (no SSE without runEvents opt-in)", ct)
	}
	// Body is a single JSON-RPC frame, no `data:` prefix.
	if bytes.Contains(body, []byte("data: ")) {
		t.Errorf("body has SSE framing despite no runEvents opt-in; body=%s", body)
	}
	var env loommcp.Response
	if err := json.Unmarshal(body, &env); err != nil {
		t.Errorf("response body not JSON-RPC: %v body=%s", err, body)
	}
}
