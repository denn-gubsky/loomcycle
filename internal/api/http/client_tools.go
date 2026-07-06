package http

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/clienttools"
)

// clientToolSubprotocol is the app-level WebSocket subprotocol the server
// negotiates for /v1/client-tools. The client MAY also send a "bearer.<token>"
// subprotocol carrying its bearer (browsers can't set an Authorization header);
// that entry is read by extractBearer and never echoed — only this one is.
const clientToolSubprotocol = "loomcycle.client-tools.v1"

// handleClientTools serves GET /v1/client-tools (RFC BC): the client upgrades to
// a WebSocket, `hello`s the tools it can run on the user's machine, and loomcycle
// files the connection under the bearer's (tenant, subject) so a matching agent
// tool call routes here (see clienttools.Registry.Invoke + the dispatch fallback).
//
// auth already ran (authMiddleware wraps this handler), so the principal is on
// ctx. The connection can ONLY ever serve runs of that same principal.
func (s *Server) handleClientTools(w http.ResponseWriter, r *http.Request) {
	if s.clientTools == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "client_tools_unavailable",
			"the client-tool host is not enabled on this server")
		return
	}
	// Principal → registry key. Open mode (no principal) collapses to the empty
	// (tenant, subject), which is the correct single-tenant behavior.
	p, _ := auth.PrincipalFromContext(r.Context())
	key := clienttools.PrincipalKey{Tenant: p.TenantID, Subject: p.Subject}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{clientToolSubprotocol},
	})
	if err != nil {
		return // Accept already wrote the handshake error
	}
	// CloseNow on every exit path; a graceful Close is best-effort below.
	defer c.CloseNow()
	c.SetReadLimit(s.cfg.Env.ClientToolMaxBytes)

	ctx := r.Context()

	// First frame must be the hello registration.
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	if clienttools.TypeOf(data) != clienttools.FrameHello {
		_ = c.Close(websocket.StatusPolicyViolation, "expected hello frame first")
		return
	}
	var hello clienttools.HelloFrame
	if err := json.Unmarshal(data, &hello); err != nil {
		_ = c.Close(websocket.StatusUnsupportedData, "malformed hello")
		return
	}

	// One mutex-guarded writer — the read-pump, the heartbeat, and every Invoke's
	// send closure all write through it (net/http-style: concurrent writes must
	// not interleave; mirrors internal/api/http/sse.go's write discipline).
	var wmu sync.Mutex
	send := func(sendCtx context.Context, v any) error {
		b, mErr := json.Marshal(v)
		if mErr != nil {
			return mErr
		}
		wmu.Lock()
		defer wmu.Unlock()
		return c.Write(sendCtx, websocket.MessageText, b)
	}

	conn, deregister, err := s.clientTools.Register(key, hello.Tools, send)
	if err != nil {
		_ = c.Close(websocket.StatusTryAgainLater, "too many client-tool connections")
		return
	}
	defer deregister() // fails any in-flight invokes so no run hangs

	accepted := make([]string, 0, len(hello.Tools))
	for _, t := range hello.Tools {
		accepted = append(accepted, t.Name)
	}
	if err := send(ctx, clienttools.HelloOKFrame{
		Type: clienttools.FrameHelloOK, ConnectionID: conn.ID(), Accepted: accepted,
	}); err != nil {
		return
	}

	// Heartbeat: ping the client on an interval; a failed ping (dead socket)
	// cancels the read-pump via the shared ctx. Mirrors the SSE keepalive.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go s.clientToolHeartbeat(hbCtx, c)

	// Read-pump: route inbound result frames to their waiting invoke; ignore
	// pong/unknown. Returns (and fires the deferred deregister) on any read
	// error — client disconnect, ctx cancel, or an over-limit frame.
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if clienttools.TypeOf(data) != clienttools.FrameResult {
			continue // pong / anything else — ignore
		}
		var res clienttools.ResultFrame
		if err := json.Unmarshal(data, &res); err != nil {
			continue
		}
		conn.DeliverResult(res)
	}
}

// clientToolHeartbeat pings the client every SSE-keepalive interval; a failed
// ping means the socket is dead — close it so the read-pump unblocks.
func (s *Server) clientToolHeartbeat(ctx context.Context, c *websocket.Conn) {
	interval := s.cfg.Env.SSEKeepaliveInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pingCtx)
			cancel()
			if err != nil {
				_ = c.Close(websocket.StatusPolicyViolation, "heartbeat timeout")
				return
			}
		}
	}
}
