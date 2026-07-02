package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// --- mock upstream /v1/_mcp -------------------------------------------

type upstreamRec struct {
	mu   sync.Mutex
	reqs []recordedReq
}
type recordedReq struct {
	method    string // JSON-RPC method
	auth      string // Authorization header
	sessionID string // Mcp-Session-Id header
}

func (u *upstreamRec) record(r recordedReq) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reqs = append(u.reqs, r)
}
func (u *upstreamRec) snapshot() []recordedReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedReq(nil), u.reqs...)
}

// mockUpstream serves a minimal /v1/_mcp: initialize mints a session and
// echoes it; a "stream" tool replies with SSE (a notification + a final
// response); "boom" replies 500; everything else is a single JSON result.
func mockUpstream(t *testing.T, rec *upstreamRec) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/_mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &p)
		rec.record(recordedReq{method: p.Method, auth: r.Header.Get("Authorization"), sessionID: r.Header.Get("Mcp-Session-Id")})

		if p.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
			return
		}
		if p.ID == nil { // notification — no response body
			w.WriteHeader(http.StatusOK)
			return
		}
		switch p.Params.Name {
		case "stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/loomcycle/run_event\",\"params\":{\"seq\":1}}\n\n")
			if fl != nil {
				fl.Flush()
			}
			io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":"+itoa(*p.ID)+",\"result\":{\"ok\":true}}\n\n")
		case "drop":
			// SSE that delivers a notification then ends WITHOUT a final
			// response frame (simulates an upstream crash mid-run).
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/loomcycle/run_event\",\"params\":{\"seq\":1}}\n\n")
				fl.Flush()
			}
			// return → stream closes with no id-bearing final frame
		case "boom":
			http.Error(w, "kaboom", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"result":{"echo":"` + p.Params.Name + `"}}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// --- proxy pipe harness ----------------------------------------------

type proxyHarness struct {
	t      *testing.T
	stdinW *io.PipeWriter
	cancel context.CancelFunc
	mu     sync.Mutex
	cond   *sync.Cond
	got    map[int64]loommcp.Response
	notes  []loommcp.Notification
}

func newProxyHarness(t *testing.T, pc *ProxyClient) *proxyHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &proxyHarness{t: t, stdinW: inW, cancel: cancel, got: map[int64]loommcp.Response{}}
	h.cond = sync.NewCond(&h.mu)
	go func() { _ = pc.Serve(ctx, inR, outW); _ = outW.Close() }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), maxHTTPRequestBodyBytes)
		for sc.Scan() {
			line := sc.Bytes()
			var probe struct {
				ID *int64 `json:"id"`
			}
			if json.Unmarshal(line, &probe) != nil {
				continue
			}
			h.mu.Lock()
			if probe.ID != nil {
				var r loommcp.Response
				if json.Unmarshal(line, &r) == nil {
					h.got[*probe.ID] = r
				}
			} else {
				var n loommcp.Notification
				if json.Unmarshal(line, &n) == nil {
					h.notes = append(h.notes, n)
				}
			}
			h.cond.Broadcast()
			h.mu.Unlock()
		}
	}()
	return h
}

func (h *proxyHarness) send(frame string) {
	if _, err := io.WriteString(h.stdinW, frame+"\n"); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}
func (h *proxyHarness) waitResp(id int64, timeout time.Duration) loommcp.Response {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() { h.mu.Lock(); h.cond.Broadcast(); h.mu.Unlock() })
	defer timer.Stop()
	h.mu.Lock()
	for {
		if r, ok := h.got[id]; ok {
			h.mu.Unlock()
			return r
		}
		if time.Now().After(deadline) {
			h.mu.Unlock()
			h.t.Fatalf("timeout waiting for response id=%d", id)
			return loommcp.Response{}
		}
		h.cond.Wait()
	}
}
func (h *proxyHarness) noteCount() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notes) }
func (h *proxyHarness) close()         { _ = h.stdinW.Close(); h.cancel() }

func initFrame() string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
}
func callFrame(id int64, name string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
}

// --- tests ------------------------------------------------------------

func TestProxy_ForwardsWithAuthAndSession(t *testing.T) {
	rec := &upstreamRec{}
	up := mockUpstream(t, rec)
	pc := NewProxyClient(ProxyConfig{Upstream: up.URL, Token: "tok-123", Logf: func(string, ...any) {}})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second) // initialize response relayed
	h.send(callFrame(2, "memory"))
	got := h.waitResp(2, 2*time.Second)
	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}

	reqs := rec.snapshot()
	if len(reqs) < 2 {
		t.Fatalf("upstream saw %d requests, want >=2", len(reqs))
	}
	// Both requests carry the bearer; the post-initialize request carries
	// the session the upstream minted.
	for _, r := range reqs {
		if r.auth != "Bearer tok-123" {
			t.Errorf("request %q auth = %q, want Bearer tok-123", r.method, r.auth)
		}
	}
	var toolReq *recordedReq
	for i := range reqs {
		if reqs[i].method == "tools/call" {
			toolReq = &reqs[i]
		}
	}
	if toolReq == nil || toolReq.sessionID != "sess-1" {
		t.Fatalf("tools/call Mcp-Session-Id = %v, want sess-1", toolReq)
	}
}

func TestProxy_RelaysSSEStream(t *testing.T) {
	rec := &upstreamRec{}
	up := mockUpstream(t, rec)
	pc := NewProxyClient(ProxyConfig{Upstream: up.URL, Logf: func(string, ...any) {}})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	h.send(callFrame(7, "stream")) // upstream answers with SSE: 1 notification + final response
	got := h.waitResp(7, 2*time.Second)
	if got.Error != nil {
		t.Fatalf("stream final response error: %+v", got.Error)
	}
	// The run_event notification frame should have been relayed too.
	deadline := time.Now().Add(time.Second)
	for h.noteCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.noteCount() == 0 {
		t.Fatal("SSE notification frame was not relayed to stdout")
	}
}

func TestProxy_SSEDropBeforeFinalRepliesError(t *testing.T) {
	rec := &upstreamRec{}
	up := mockUpstream(t, rec)
	pc := NewProxyClient(ProxyConfig{Upstream: up.URL, Logf: func(string, ...any) {}})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	h.send(callFrame(11, "drop")) // SSE notification then stream ends, no final response
	got := h.waitResp(11, 2*time.Second)
	if got.Error == nil {
		t.Fatal("SSE stream that dropped before the final response should surface a JSON-RPC error")
	}
}

func TestProxy_UpstreamHTTPErrorBecomesJSONRPC(t *testing.T) {
	rec := &upstreamRec{}
	up := mockUpstream(t, rec)
	pc := NewProxyClient(ProxyConfig{Upstream: up.URL, Logf: func(string, ...any) {}})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	h.send(callFrame(9, "boom")) // upstream 500
	got := h.waitResp(9, 2*time.Second)
	if got.Error == nil {
		t.Fatal("upstream 500 should surface as a JSON-RPC error, got success")
	}
}

func TestProxy_DeadUpstreamRepliesError(t *testing.T) {
	// Unreachable upstream → the proxy exhausts its reconnect budget and then
	// replies with an error, not hangs. A fast reconnect schedule keeps the
	// test quick while still exercising the retry loop.
	pc := NewProxyClient(ProxyConfig{
		Upstream:        "http://127.0.0.1:1",
		Logf:            func(string, ...any) {},
		ReconnectDelays: []time.Duration{time.Millisecond, time.Millisecond},
	})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(callFrame(3, "memory"))
	got := h.waitResp(3, 3*time.Second)
	if got.Error == nil {
		t.Fatal("dead upstream should surface a JSON-RPC error")
	}
}

// TestProxy_ReinitializesOnSessionExpiry: when the upstream invalidates the
// session (its 30-min inactivity TTL, or a restart) and returns 404 / -32001,
// the proxy transparently re-runs the initialize handshake and retries the
// frame — so the client sees a normal result, no /exit-relaunch. Fails on the
// pre-fix proxy (which surfaced the 404 as a JSON-RPC error and never refreshed
// the dead session id).
func TestProxy_ReinitializesOnSessionExpiry(t *testing.T) {
	var mu sync.Mutex
	initCount := 0
	valid := "" // the session id the upstream currently accepts

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/_mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &p)

		if p.Method == "initialize" {
			mu.Lock()
			initCount++
			sid := "sess-" + itoa(int64(initCount))
			if initCount >= 2 {
				valid = sid // the re-handshake session is accepted; sess-1 stays "expired"
			}
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
			return
		}
		if p.ID == nil { // notification (notifications/initialized) — no body
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		ok := valid != "" && r.Header.Get("Mcp-Session-Id") == valid
		mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"error":{"code":-32001,"message":"session not found or expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"result":{"ok":true}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pc := NewProxyClient(ProxyConfig{Upstream: srv.URL, Logf: func(string, ...any) {}})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	// This call goes out on the (now-expired) sess-1 → 404/-32001 → the proxy
	// must re-handshake and retry, returning a clean result.
	h.send(callFrame(2, "memory"))
	got := h.waitResp(2, 3*time.Second)
	if got.Error != nil {
		t.Fatalf("expected transparent recovery, got JSON-RPC error: %+v", got.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if initCount < 2 {
		t.Errorf("expected a transparent re-initialize (initCount>=2), got %d", initCount)
	}
}

// flakyRT is a RoundTripper that returns a synthetic transport error on the
// Nth request(s) named in failOn, and delegates everything else to inner. It
// simulates an idle-reaped keep-alive socket or a connection dropped during an
// upstream restart — the request never reaches the server.
type flakyRT struct {
	mu     sync.Mutex
	n      int
	failOn map[int]bool
	inner  http.RoundTripper
}

func (f *flakyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.n++
	cur := f.n
	fail := f.failOn[cur]
	f.mu.Unlock()
	if fail {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	}
	return f.inner.RoundTrip(req)
}

// TestProxy_RecoversAfterTransportError: a tool call whose upstream connection
// drops (an idle-reaped keep-alive socket, or the restart window) is retried on
// a fresh connection rather than dead-ending — so the client gets a clean
// result, no reload/relaunch. Fails on the pre-fix proxy, which surfaced the
// transport error immediately with no retry.
func TestProxy_RecoversAfterTransportError(t *testing.T) {
	rec := &upstreamRec{}
	up := mockUpstream(t, rec)
	// req #1 = initialize (succeeds), req #2 = the first tools/call (drops),
	// retry #3 = tools/call (succeeds).
	flaky := &flakyRT{failOn: map[int]bool{2: true}, inner: http.DefaultTransport}
	pc := NewProxyClient(ProxyConfig{
		Upstream:        up.URL,
		Logf:            func(string, ...any) {},
		Client:          &http.Client{Transport: flaky},
		ReconnectDelays: []time.Duration{0, time.Millisecond},
	})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	h.send(callFrame(2, "memory"))
	got := h.waitResp(2, 3*time.Second)
	if got.Error != nil {
		t.Fatalf("expected transparent recovery after a transport error, got JSON-RPC error: %+v", got.Error)
	}
}

// TestProxy_RecoversAcrossServerRestart composes the two recovery mechanisms:
// the restart both DROPS the connection (transport error) AND invalidates the
// session (404/-32001 on the stale id). The proxy must reconnect (backoff
// retry) and then re-handshake, returning a clean result. This is the "doesn't
// recover when the server restarts" symptom end-to-end.
func TestProxy_RecoversAcrossServerRestart(t *testing.T) {
	var mu sync.Mutex
	initCount := 0
	valid := "" // the session id the upstream currently accepts

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/_mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &p)

		if p.Method == "initialize" {
			mu.Lock()
			initCount++
			sid := "sess-" + itoa(int64(initCount))
			if initCount >= 2 {
				valid = sid // the re-handshake session is accepted; sess-1 stays "expired"
			}
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
			return
		}
		if p.ID == nil { // notifications/initialized — no body
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		ok := valid != "" && r.Header.Get("Mcp-Session-Id") == valid
		mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"error":{"code":-32001,"message":"session not found or expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"result":{"ok":true}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Fail the first tools/call attempt (req #2) with a transport error to
	// model the connection dropping during the restart; the reconnect retry
	// then meets the invalidated session and re-handshakes.
	flaky := &flakyRT{failOn: map[int]bool{2: true}, inner: http.DefaultTransport}
	pc := NewProxyClient(ProxyConfig{
		Upstream:        srv.URL,
		Logf:            func(string, ...any) {},
		Client:          &http.Client{Transport: flaky},
		ReconnectDelays: []time.Duration{0, time.Millisecond},
	})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	h.send(callFrame(2, "memory"))
	got := h.waitResp(2, 3*time.Second)
	if got.Error != nil {
		t.Fatalf("expected recovery across a restart (reconnect + re-handshake), got: %+v", got.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if initCount < 2 {
		t.Errorf("expected a re-handshake after the restart (initCount>=2), got %d", initCount)
	}
}

// TestProxy_DefaultClientHasStallGuards pins the hardening: the default HTTP
// client bounds time-to-response-headers (so a stalled upstream can't hang a
// frame forever — nothing else does for a stdio server) and closes idle
// connections below the runtime server's 120s IdleTimeout (so the client
// reopens before the server severs an idle keep-alive socket).
func TestProxy_DefaultClientHasStallGuards(t *testing.T) {
	pc := NewProxyClient(ProxyConfig{Upstream: "http://127.0.0.1:8787", Logf: func(string, ...any) {}})
	tr, ok := pc.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client Transport = %T, want *http.Transport", pc.client.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want > 0 (a stalled upstream must not hang a frame forever)", tr.ResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout >= 120*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 0 < t < 120s (below the server's 120s IdleTimeout)", tr.IdleConnTimeout)
	}
	if pc.client.Timeout != 0 {
		t.Errorf("client.Timeout = %v, want 0 (a streaming spawn_run holds the body open for the whole run)", pc.client.Timeout)
	}
	if len(pc.reconnectDelays) == 0 {
		t.Error("reconnectDelays is empty, want a default schedule so transport errors are retried")
	}
	// The long-run client must NOT carry a header timeout — a slow spawn_run
	// whose headers arrive minutes late must not be mistaken for a stall.
	str, ok := pc.streamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("streamClient Transport = %T, want *http.Transport", pc.streamClient.Transport)
	}
	if str.ResponseHeaderTimeout != 0 {
		t.Errorf("streamClient ResponseHeaderTimeout = %v, want 0 (agent-run tools must never header-timeout → double-execute)", str.ResponseHeaderTimeout)
	}
}

// TestProxy_NoHeaderTimeoutOnSpawnRun guards the double-execution hazard: a
// spawn_run whose response headers arrive later than the (fast-frame) header
// timeout must NOT be retried — it runs on the no-header-timeout client. The
// upstream must therefore see exactly ONE spawn_run, and the result must come
// through. Without the long-run carve-out the header timeout would fire, the
// reconnect loop would re-POST, and the agent run would execute twice.
func TestProxy_NoHeaderTimeoutOnSpawnRun(t *testing.T) {
	var mu sync.Mutex
	spawnCount := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/_mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &p)
		if p.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		if p.Params.Name == "spawn_run" {
			mu.Lock()
			spawnCount++
			mu.Unlock()
			// Headers (and body) arrive only after a delay LONGER than the
			// fast-frame header timeout — a slow model / long run. A retry-happy
			// proxy would give up and re-POST before this returns.
			time.Sleep(250 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"result":{"ok":true}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*p.ID) + `,"result":{}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A very short header timeout for FAST frames; the spawn_run must bypass it.
	pc := NewProxyClient(ProxyConfig{
		Upstream:              srv.URL,
		Logf:                  func(string, ...any) {},
		ResponseHeaderTimeout: 50 * time.Millisecond,
		ReconnectDelays:       []time.Duration{0, time.Millisecond},
	})
	h := newProxyHarness(t, pc)
	defer h.close()

	h.send(initFrame())
	h.waitResp(1, 2*time.Second)
	h.send(callFrame(2, "spawn_run"))
	got := h.waitResp(2, 3*time.Second)
	if got.Error != nil {
		t.Fatalf("spawn_run should complete on the no-header-timeout client, got: %+v", got.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if spawnCount != 1 {
		t.Fatalf("upstream saw spawn_run %d times, want exactly 1 (a header-timeout retry double-executes the run)", spawnCount)
	}
}
