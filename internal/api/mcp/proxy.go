// proxy.go — RFC R thin-client MCP transport.
//
// `loomcycle mcp --upstream <url>` runs in CLIENT mode: a stdio ↔
// /v1/_mcp proxy that forwards every JSON-RPC frame to the ONE
// authoritative runtime and holds NO runtime of its own (no providers,
// scheduler, sweepers, Store, or bus). This enforces the single-runtime
// invariant — a second loomcycle process is a control client, never a
// second runtime — which dissolves the cross-process interruption-wake
// bug (F15), the listener contention (F9), and the rogue-runtime wedge
// (F16): every meta-tool call, including interruption_resolve, lands on
// the runtime that actually owns the run.
//
// The upstream's HTTP MCP transport (http_transport.go) already
// implements the full surface, including SSE streaming for
// spawn_run+runEvents, so the client only does framing, one HTTP/SSE
// connection per request, auth-header injection, and Mcp-Session-Id
// continuity.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// ProxyConfig configures the thin-client proxy.
type ProxyConfig struct {
	// Upstream is the authoritative runtime's base URL, e.g.
	// http://127.0.0.1:8788. The proxy POSTs to <Upstream>/v1/_mcp.
	Upstream string
	// Token is the bearer for the runtime's /v1/* auth. Empty in
	// open mode (no auth middleware) — then no Authorization header.
	Token string
	// Logf is the log sink (stderr; never stdout — stdout is the wire).
	Logf func(format string, v ...any)
	// Client overrides the HTTP client. nil → a default with no overall
	// timeout (a streaming spawn_run holds the response open for the
	// whole run; an overall timeout would truncate it).
	Client *http.Client
}

// ProxyClient is the thin-client MCP transport. Construct via
// NewProxyClient; drive via Serve.
type ProxyClient struct {
	cfg    ProxyConfig
	mcpURL string
	client *http.Client

	// writeMu serialises stdout writes so concurrent forwards (and the
	// SSE frames of a streaming spawn_run) can't interleave bytes.
	writeMu sync.Mutex
	// wg tracks in-flight forward goroutines for clean shutdown.
	wg sync.WaitGroup

	// session holds the Mcp-Session-Id the upstream assigns at
	// initialize; echoed on every subsequent request.
	sessMu  sync.RWMutex
	session string

	// initFrame caches the client's initialize request so the proxy can
	// transparently RE-HANDSHAKE when the upstream invalidates the session
	// (its 30-min inactivity TTL, or a restart): replaying it mints a fresh
	// Mcp-Session-Id without the client (e.g. Claude Code) noticing — no
	// /exit-relaunch. reinitMu single-flights concurrent re-handshakes.
	initMu    sync.RWMutex
	initFrame []byte
	reinitMu  sync.Mutex
}

// mcpSessionExpiredCode is the JSON-RPC error code the upstream HTTP transport
// returns (with HTTP 404) for an unknown/expired Mcp-Session-Id — the signal to
// re-handshake. Mirrors internal/api/mcp/http_transport.go.
const mcpSessionExpiredCode = -32001

// initializedNotification is the parameterless post-initialize notification,
// replayed after a re-handshake to mark the fresh session initialized.
var initializedNotification = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

// NewProxyClient builds a thin-client proxy targeting Upstream.
func NewProxyClient(cfg ProxyConfig) *ProxyClient {
	if cfg.Logf == nil {
		cfg.Logf = defaultLogf
	}
	client := cfg.Client
	if client == nil {
		// No overall timeout: streaming spawn_run holds the response
		// open for the run's duration. Per-request cancellation rides
		// the request context instead.
		client = &http.Client{}
	}
	return &ProxyClient{
		cfg:    cfg,
		mcpURL: strings.TrimRight(cfg.Upstream, "/") + "/v1/_mcp",
		client: client,
	}
}

// Serve runs the read-forward-write loop until stdin closes or ctx is
// done. initialize is forwarded INLINE so the Mcp-Session-Id is captured
// before any later frame needs it; every other frame is forwarded on its
// own goroutine so a long streaming spawn_run can't head-of-line-block
// the frames behind it (the RFC O property, preserved across the proxy).
func (p *ProxyClient) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), maxHTTPRequestBodyBytes)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) == 0 {
			continue
		}
		if ctx.Err() != nil {
			break
		}

		var probe struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &probe)

		if probe.Method == "initialize" {
			// Cache the handshake so we can replay it on a server-side session
			// expiry (re-handshake). Inline: capture the session before
			// forwarding anything that will need it (the upstream requires
			// Mcp-Session-Id on every non-initialize request).
			p.setInitFrame(line)
			p.forward(reqCtx, line, stdout, true)
			continue
		}

		p.wg.Add(1)
		go func(fr []byte) {
			defer p.wg.Done()
			p.forward(reqCtx, fr, stdout, true)
		}(line)
	}

	scanErr := scanner.Err()
	p.wg.Wait() // flush in-flight responses before returning
	if scanErr != nil {
		return fmt.Errorf("mcp proxy: read stdin: %w", scanErr)
	}
	return ctx.Err()
}

// forward POSTs one JSON-RPC frame to the upstream /v1/_mcp and relays
// the response (single JSON frame, or each SSE data frame) to stdout.
// allowReinit gates the transparent re-handshake on a session-expiry: it is
// true for client-originated frames and false for the single retry after a
// re-handshake (so a still-failing upstream can't loop).
func (p *ProxyClient) forward(ctx context.Context, frame []byte, stdout io.Writer, allowReinit bool) {
	sentSID := p.sessionID()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.mcpURL, bytes.NewReader(frame))
	if err != nil {
		p.replyError(stdout, frame, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Advertise both per the Streamable HTTP spec — strict servers 406
	// without text/event-stream when SSE is a possible response.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	if sentSID != "" {
		req.Header.Set("Mcp-Session-Id", sentSID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// Don't leave the MCP client hanging on a dead upstream.
		p.replyError(stdout, frame, "upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Capture the session id the upstream assigned (initialize) or
	// echoed. set-if-present is idempotent for subsequent requests.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.setSessionID(sid)
	}

	// An HTTP-level error (bad session, auth, body too large) arrives as
	// a non-2xx with a non-JSON-RPC body. Surface it as a JSON-RPC error
	// addressed to this frame so the client gets a usable response.
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		// Self-heal: the upstream invalidated our session (its 30-min
		// inactivity TTL, or a restart). Re-run the initialize handshake to
		// mint a fresh session, then retry this frame ONCE — so the client
		// never sees the expiry (no /exit-relaunch). Only on the real
		// session-expiry signal, never for initialize itself, at most one retry.
		if allowReinit && sessionExpired(resp.StatusCode, body) && frameMethod(frame) != "initialize" {
			if rerr := p.reinitialize(ctx, sentSID); rerr != nil {
				p.cfg.Logf("mcp proxy: re-initialize after session expiry failed: %v", rerr)
			} else {
				p.forward(ctx, frame, stdout, false)
				return
			}
		}
		p.replyError(stdout, frame, fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// If the stream ends without ever delivering a frame carrying the
		// request id (the final tools/call response), the upstream dropped
		// mid-run — surface an error so the client isn't left waiting on a
		// spawn_run that will never answer.
		if !p.relaySSE(resp.Body, stdout) {
			p.replyError(stdout, frame, "upstream SSE stream ended before the final response")
		}
		return
	}

	// Single application/json response. An empty body (a notification has
	// no response) writes nothing.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.replyError(stdout, frame, "read upstream response: "+err.Error())
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return
	}
	p.writeLine(stdout, bytes.TrimRight(body, "\n"))
}

// relaySSE reads the upstream text/event-stream and writes each JSON-RPC
// frame (one per `data:` line — loomcycle's sseWriter emits single-line
// data) to stdout as a newline-delimited frame, in arrival order. It
// returns true once it has relayed a frame carrying a request id (the
// final tools/call response), so the caller can tell a complete run from
// a stream that dropped after only run_event notifications (which have no
// id).
func (p *ProxyClient) relaySSE(body io.Reader, stdout io.Writer) bool {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxHTTPRequestBodyBytes)
	sawFinal := false
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // skip blank separators / other SSE fields
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 {
			continue
		}
		var probe struct {
			ID *int64 `json:"id"`
		}
		if json.Unmarshal(data, &probe) == nil && probe.ID != nil {
			sawFinal = true
		}
		p.writeLine(stdout, data)
	}
	return sawFinal
}

// writeLine emits one newline-delimited JSON-RPC frame to stdout under
// the write mutex (the stdio MCP framing the client expects).
func (p *ProxyClient) writeLine(stdout io.Writer, frame []byte) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := stdout.Write(frame); err != nil {
		p.cfg.Logf("mcp proxy: write stdout: %v", err)
		return
	}
	if _, err := stdout.Write([]byte("\n")); err != nil {
		p.cfg.Logf("mcp proxy: write newline: %v", err)
	}
}

// replyError writes a JSON-RPC error response addressed to the request's
// id, so a forward failure (dead upstream, HTTP error) doesn't leave the
// client waiting. Notifications (no id) get no response, by spec.
func (p *ProxyClient) replyError(stdout io.Writer, frame []byte, msg string) {
	id, ok := frameRequestID(frame)
	if !ok {
		p.cfg.Logf("mcp proxy: %s (notification dropped)", msg)
		return
	}
	resp := loommcp.Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &loommcp.Error{Code: -32603, Message: msg},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		p.cfg.Logf("mcp proxy: marshal error reply: %v", err)
		return
	}
	p.writeLine(stdout, raw)
}

func (p *ProxyClient) sessionID() string {
	p.sessMu.RLock()
	defer p.sessMu.RUnlock()
	return p.session
}

func (p *ProxyClient) setSessionID(id string) {
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	p.session = id
}

func (p *ProxyClient) setInitFrame(frame []byte) {
	cp := append([]byte(nil), frame...) // own the bytes; the scanner buffer is reused
	p.initMu.Lock()
	p.initFrame = cp
	p.initMu.Unlock()
}

func (p *ProxyClient) getInitFrame() []byte {
	p.initMu.RLock()
	defer p.initMu.RUnlock()
	return p.initFrame
}

// reinitialize re-runs the cached initialize handshake against the upstream to
// obtain a fresh Mcp-Session-Id after a server-side session expiry. staleSID is
// the session the failed request was sent with; if the live session already
// differs, another goroutine re-handshook concurrently and we reuse it
// (single-flight). The handshake POSTs are silent — their responses are NOT
// relayed to the client, which already completed its one initialize.
func (p *ProxyClient) reinitialize(ctx context.Context, staleSID string) error {
	p.reinitMu.Lock()
	defer p.reinitMu.Unlock()
	if cur := p.sessionID(); cur != "" && cur != staleSID {
		return nil // already refreshed by a concurrent forward
	}
	init := p.getInitFrame()
	if len(init) == 0 {
		return fmt.Errorf("no cached initialize to replay")
	}
	newSID, err := p.silentPost(ctx, init, "") // initialize ignores any session id; mints a fresh one
	if err != nil {
		return err
	}
	if newSID == "" {
		return fmt.Errorf("upstream returned no Mcp-Session-Id on re-initialize")
	}
	p.setSessionID(newSID)
	p.cfg.Logf("mcp proxy: upstream session expired — re-initialized (fresh session, client uninterrupted)")
	// Mark the new session initialized (a notification; no response). The
	// upstream doesn't currently gate tools/call on it, but this keeps the
	// handshake spec-correct + future-proof.
	if _, nerr := p.silentPost(ctx, initializedNotification, newSID); nerr != nil {
		p.cfg.Logf("mcp proxy: notifications/initialized after re-init: %v", nerr)
	}
	return nil
}

// silentPost POSTs a frame to the upstream WITHOUT relaying the response to
// stdout — used for the re-handshake (replaying initialize + initialized). It
// returns the Mcp-Session-Id the upstream set, if any.
func (p *ProxyClient) silentPost(ctx context.Context, frame []byte, sid string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.mcpURL, bytes.NewReader(frame))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	return resp.Header.Get("Mcp-Session-Id"), nil
}

// sessionExpired reports whether an upstream response is the "session not found
// or expired" signal: HTTP 404 carrying JSON-RPC error code -32001. Keying on
// the code (not any 404) avoids re-handshaking on unrelated errors.
func sessionExpired(status int, body []byte) bool {
	if status != http.StatusNotFound {
		return false
	}
	var e struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &e) == nil && e.Error != nil && e.Error.Code == mcpSessionExpiredCode
}

// frameMethod extracts the JSON-RPC method from a frame (best-effort).
func frameMethod(frame []byte) string {
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(frame, &probe)
	return probe.Method
}
