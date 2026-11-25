package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// The defaults of a page's socket bounds (P9 plan §3.3; workspace.expose).
const (
	// socketPiece is how much of a message the relay holds at once, each
	// way. With the default max_connections (32) a workspace's sockets
	// hold at most 32 × 2 × 16 KiB = 1 MiB in the relay on one replica,
	// whatever max_message_bytes is (D-P9-49).
	socketPiece               = 16 << 10
	defaultSocketMessageBytes = 16 << 20
	defaultSocketIdle         = 15 * time.Minute
	defaultSocketLifetime     = 4 * time.Hour
	defaultSocketRecheck      = 15 * time.Second
)

// isExposureSocketUpgrade reports whether r is a WebSocket upgrade to a
// workspace's exposed page: the one other route that takes the bearer as a
// subprotocol (RFC GW §5.3, as RFC BC's socket does).
func isExposureSocketUpgrade(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, exposurePrefix) && isWebSocketUpgrade(r)
}

// isWebSocketUpgrade follows RFC 6455's header rules: Upgrade is websocket,
// and Connection is a token list holding upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// socketBearerOffered reports whether the upgrade carries a bearer as a
// subprotocol: what the route accepts on a socket in place of the
// Authorization header.
func socketBearerOffered(r *http.Request) bool {
	for _, p := range offeredProtocols(r) {
		if strings.HasPrefix(p, "bearer.") && len(p) > len("bearer.") {
			return true
		}
	}
	return false
}

func offeredProtocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// applicationProtocols are the protocols offered for the application: never
// the marker, and never the bearer (RFC GW §5.3: the core removes it).
func applicationProtocols(r *http.Request) []string {
	var out []string
	for _, p := range offeredProtocols(r) {
		if p == exposureSocketMarker || strings.HasPrefix(p, "bearer.") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// serveSocket carries a WebSocket from the user's client to the exposed
// port, message by message (RFC GW §5.3; P9 plan §3.3). The caller has
// decided who (OpenExposure) and counted the socket in the workspace's
// open requests until it ends.
//
//   - The application is dialled first, through the tunnel and so the
//     worker's check that a server of the workspace listens on the port
//     itself (#2917). An application that is not there, or refuses the
//     upgrade, is a plain HTTP error; no socket is half open.
//   - It is offered only the client's protocols for it, never the marker or
//     the bearer, and it sees Host and Origin as the client sent them; no
//     Authorization or Cookie.
//   - Nothing of its 101 reaches the client but the protocol it chose; the
//     client is answered with that, or the marker.
//   - Each message is at most MaxMessageBytes; the workspace's byte rate
//     holds both ways; a socket quiet for SocketIdle, or open for
//     SocketLifetime, is closed; and every socketRecheck the exposure is
//     asked again, so unexpose, a release and the operator's switch close
//     an open socket too.
func (h *exposureHandler) serveSocket(w http.ResponseWriter, r *http.Request, e store.WorkspaceExposure, row store.WorkspaceRow,
	tenantID, userID, rest string) {
	lim := h.socketLimits()
	if h.reauth == nil || h.expiry == nil {
		// A socket that could not re-check its credential is not opened.
		http.Error(w, "the page's socket is not available", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	app, err := h.dialApplication(ctx, r, row, e.Port, rest, lim)
	if err != nil {
		var oe *dispatch.OpError
		if errors.As(err, &oe) && (oe.Class == workspace.ClassPortNotServed || oe.Class == workspace.ClassPortUnreachable) {
			http.Error(w, "no server of the workspace is listening on this page's port", http.StatusBadGateway)
			return
		}
		slog.DebugContext(r.Context(), "workspace exposure: the application refused the socket", "err", err)
		http.Error(w, "the workspace's application did not accept the socket", http.StatusBadGateway)
		return
	}
	defer app.CloseNow()

	answer := app.Subprotocol()
	if answer == "" {
		answer = exposureSocketMarker
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{answer},
		// NOTE: this is coder/websocket's Origin check, not TLS. The route
		// is authenticated by the bearer alone — the handler refused a
		// cookie before this — and a page of another site cannot read the
		// bearer, so it cannot open this socket (no cross-site WebSocket
		// hijacking). The Origin is the application's to judge: it is
		// passed on as sent (RFC GW §5.3), and loomboard's own origins (a
		// desktop app, an extension) could not be listed here.
		InsecureSkipVerify: true,
	})
	if err != nil {
		_ = app.Close(websocket.StatusGoingAway, "the client did not complete the handshake")
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(lim.MaxMessageBytes)
	app.SetReadLimit(lim.MaxMessageBytes)

	var (
		mu       sync.Mutex
		lastUsed = time.Now()
	)
	used := func() {
		mu.Lock()
		lastUsed = time.Now()
		mu.Unlock()
	}
	ended := make(chan relayEnd, 2)
	// Each message is streamed, piece by piece, never held whole: what one
	// socket holds is one piece a direction, whatever the message's size
	// (the security review of #2935). The read limit still bounds the
	// message, and the byte rate is taken per piece.
	relay := func(from, to *websocket.Conn) {
		for {
			typ, rd, err := from.Reader(ctx)
			if err != nil {
				ended <- relayEnd{from: from, err: err}
				return
			}
			used()
			wr, err := to.Writer(ctx, typ)
			if err != nil {
				ended <- relayEnd{from: to, err: err}
				return
			}
			src := &trackedReader{r: rd}
			for {
				h.gate.waitBytes(ctx, row.ID, socketPiece)
				n, err := io.CopyN(wr, src, socketPiece)
				if n > 0 {
					used()
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					// A failed read is the sender's (its close, its
					// message past the bound); any other, the receiver's.
					if src.err != nil {
						ended <- relayEnd{from: from, err: src.err}
					} else {
						ended <- relayEnd{from: to, err: err}
					}
					return
				}
			}
			if err := wr.Close(); err != nil {
				ended <- relayEnd{from: to, err: err}
				return
			}
		}
	}
	safego.Go(ctx, "workspace.exposure.socket-up", func() { relay(client, app) })
	safego.Go(ctx, "workspace.exposure.socket-down", func() { relay(app, client) })

	lifetime := time.NewTimer(lim.SocketLifetime)
	defer lifetime.Stop()
	// The credential's own end (a token's expiry or the end of its
	// retirement's grace), as the document feed does: the socket never
	// outlives it, whatever the recheck interval. Zero: no fixed end, which
	// the recheck finds.
	var credentialEnd <-chan time.Time
	if until := h.expiry(r); !until.IsZero() {
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		credentialEnd = t.C
	}
	tick := time.NewTicker(h.recheckEvery())
	defer tick.Stop()
	for {
		select {
		case end := <-ended:
			closeOther(client, app, end)
			return
		case <-lifetime.C:
			closeBoth(client, app, websocket.StatusGoingAway, "the page's socket reached its lifetime")
			return
		case <-credentialEnd:
			closeBoth(client, app, websocket.StatusPolicyViolation, "the credential expired")
			return
		case <-tick.C:
			mu.Lock()
			idle := time.Since(lastUsed)
			mu.Unlock()
			if idle >= lim.SocketIdle {
				closeBoth(client, app, websocket.StatusGoingAway, "the page's socket was idle")
				return
			}
			// The credential is resolved again, as every request is: a
			// token revoked, retired or expired, or a user disabled, ends
			// the socket. The bearer is read from the request again and
			// never kept or logged here; resolving it compares its hash.
			if p, err := h.reauth(r); err != nil || p.TenantID != tenantID || p.Subject != userID {
				closeBoth(client, app, websocket.StatusPolicyViolation, "the credential no longer holds")
				return
			}
			if _, _, err := h.x.OpenExposure(ctx, tenantID, userID, e.ID); err != nil {
				// Closed (unexposed, released, the switch off), or no longer
				// readable: either way the socket ends, closed.
				closeBoth(client, app, websocket.StatusPolicyViolation, "the page was closed")
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// relayEnd is how one direction of a socket ended: from is the side whose
// read or write failed.
type relayEnd struct {
	from *websocket.Conn
	err  error
}

// closeOther passes a side's close on to the other side with its code and
// reason, as they are; any other end closes the other side as gone.
func closeOther(client, app *websocket.Conn, end relayEnd) {
	other := app
	if end.from == app {
		other = client
	}
	var ce websocket.CloseError
	if errors.As(end.err, &ce) {
		_ = other.Close(ce.Code, ce.Reason)
		return
	}
	if websocket.CloseStatus(end.err) == websocket.StatusMessageTooBig {
		_ = other.Close(websocket.StatusMessageTooBig, "a message was over the page's bound")
		return
	}
	_ = other.Close(websocket.StatusGoingAway, "the other side went away")
}

func closeBoth(client, app *websocket.Conn, code websocket.StatusCode, why string) {
	_ = client.Close(code, why)
	_ = app.Close(code, why)
}

// dialApplication opens the application's socket over a tunnel: the Host,
// Origin and the client's protocols for it, nothing else of the request.
func (h *exposureHandler) dialApplication(ctx context.Context, r *http.Request, row store.WorkspaceRow, port int, rest string,
	lim exposeRouteLimits) (*websocket.Conn, error) {
	hdr := http.Header{}
	if o := r.Header.Get("Origin"); o != "" {
		hdr.Set("Origin", o)
	}
	target := "ws://workspace" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(dctx, target, &websocket.DialOptions{
		Host:         r.Host,
		HTTPHeader:   hdr,
		Subprotocols: applicationProtocols(r),
		HTTPClient: &http.Client{
			// A redirect of the handshake is not followed: the socket is to
			// this page's port or nowhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DisableKeepAlives:      true,
				DisableCompression:     true,
				MaxResponseHeaderBytes: exposureHeaderBytes,
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					// Not dctx: the tunnel outlives the handshake, for the
					// socket's whole life.
					t, err := h.x.DialExposure(ctx, row, port)
					if err != nil {
						return nil, err
					}
					return exposureConn{t}, nil
				},
			},
		},
	})
	if resp != nil && resp.Body != nil && c == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// socketLimits are the handler's bounds with defaults for any left unset.
func (h *exposureHandler) socketLimits() exposeRouteLimits {
	l := h.lim
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = defaultSocketMessageBytes
	}
	if l.SocketIdle <= 0 {
		l.SocketIdle = defaultSocketIdle
	}
	if l.SocketLifetime <= 0 {
		l.SocketLifetime = defaultSocketLifetime
	}
	return l
}

// recheckEvery is how often an open socket asks whether its exposure is
// still open, and is idle: never longer than the idle bound.
func (h *exposureHandler) recheckEvery() time.Duration {
	d := h.socketRecheck
	if d <= 0 {
		d = defaultSocketRecheck
	}
	if idle := h.socketLimits().SocketIdle; idle < d {
		d = idle / 2
	}
	if d <= 0 {
		d = time.Millisecond
	}
	return d
}

// waitBytes waits until the workspace's byte rate lets n bytes through, or
// ctx ends.
func (g *exposureGate) waitBytes(ctx context.Context, wsID string, n int) {
	if g.lim.BytesPerSecond <= 0 {
		return
	}
	for left := n; left > 0; {
		piece := min(left, rateWriterPiece)
		for {
			g.mu.Lock()
			now := time.Now()
			d := g.counters(wsID, now).bytes.takeOrWait(float64(piece), now)
			g.mu.Unlock()
			if d == 0 {
				break
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return
			}
		}
		left -= piece
	}
}

// trackedReader keeps the error its reader failed with (EOF aside), so the
// relay can tell the sender's failure from the receiver's.
type trackedReader struct {
	r   io.Reader
	err error
}

func (t *trackedReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF {
		t.err = err
	}
	return n, err
}
