package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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
	// Unreachable upstream → the proxy must reply with an error, not hang.
	pc := NewProxyClient(ProxyConfig{Upstream: "http://127.0.0.1:1", Logf: func(string, ...any) {}})
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
