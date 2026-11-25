package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// workspaceExposures is what the exposure route needs of the workspace
// bookkeeping (internal/workspace/lifecycle.Manager).
type workspaceExposures interface {
	OpenExposure(ctx context.Context, tenantID, userID, id string) (store.WorkspaceExposure, store.WorkspaceRow, error)
	DialExposure(ctx context.Context, r store.WorkspaceRow, port int) (io.ReadWriteCloser, error)
}

// exposeRouteLimits are workspace.expose's per-workspace bounds the route
// keeps, per replica.
type exposeRouteLimits struct {
	MaxConnections    int
	RequestsPerSecond int
	BytesPerSecond    int64
}

const (
	exposurePrefix       = "/v1/workspaces/exposures/"
	exposureRoutePattern = "/v1/workspaces/exposures/{id}/{path...}"
	// exposurePolicy is set on every answer of the route, error answers
	// too, in place of the application's own (RFC GW §5.3): the page runs
	// scripts and forms in an opaque origin, may fetch only its own origin,
	// and cannot navigate the top window or open popups (the sandbox grants
	// neither allow-top-navigation nor allow-popups).
	//
	// Dropping sandbox, or adding allow-same-origin, reopens CodeQL
	// go/reflected-xss alerts 422 and 423, which were dismissed on this
	// basis: a page served from the API's origin runs in an opaque origin.
	// TestExposure_EveryAnswerIsSandboxedWithoutSameOrigin pins it.
	exposurePolicy = "sandbox allow-scripts allow-forms; connect-src 'self'"
	// exposureHeaderBytes bounds an application's answer's headers.
	exposureHeaderBytes = 64 << 10
)

// exposureHandler serves /v1/workspaces/exposures/{id}/… (RFC GW §5.3, P9
// plan §3.3): a workspace's exposed port, to the workspace's own user only.
type exposureHandler struct {
	x    workspaceExposures
	gate *exposureGate
}

func newExposureHandler(x workspaceExposures, lim exposeRouteLimits) *exposureHandler {
	return &exposureHandler{x: x, gate: newExposureGate(lim)}
}

// exposureNotFound is the one answer for every caller the route will not
// serve: an unknown id, another user's or tenant's page, a closed one.
func exposureNotFound(w http.ResponseWriter) {
	http.Error(w, "no such page", http.StatusNotFound)
}

func (h *exposureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setExposureHeaders(w.Header())
	// The bearer in the Authorization header, and nothing else: no cookie
	// (a browser sent to this address must run nothing), no token in the
	// address, no subprotocol (WebSocket is P9 PR 5).
	if _, viaCookie := uiAuthFrom(r.Context()); viaCookie || !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
		http.Error(w, "this route takes the bearer in the Authorization header", http.StatusUnauthorized)
		return
	}
	p, ok := auth.PrincipalFromContext(r.Context())
	// D22, D-P9-8: the workspace's own user, by a principal that names one.
	// Open mode authenticates nobody and the legacy token names no user:
	// neither reaches a page.
	if !ok || p.Legacy || p.TenantID == "" || p.Subject == "" {
		exposureNotFound(w)
		return
	}
	id := r.PathValue("id")
	e, row, err := h.x.OpenExposure(r.Context(), p.TenantID, p.Subject, id)
	switch {
	case errors.Is(err, lifecycle.ErrExposureNotFound):
		exposureNotFound(w)
		return
	case err != nil:
		slog.WarnContext(r.Context(), "workspace exposure: cannot read the exposure", "err", err)
		http.Error(w, "the page cannot be served now", http.StatusServiceUnavailable)
		return
	}
	if isUpgrade(r) {
		http.Error(w, "WebSocket is not served on this route yet", http.StatusNotImplemented)
		return
	}
	release, why := h.gate.admit(row.ID)
	if why != "" {
		http.Error(w, why, http.StatusTooManyRequests)
		return
	}
	defer release()
	prefix := exposurePrefix + id
	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if rest == "" || rest[0] != '/' {
		rest = "/" + rest
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: "workspace", Opaque: rest, RawQuery: pr.In.URL.RawQuery}
			// Host and Origin as the client sent them: an application's
			// origin check compares the two (spike D).
			pr.Out.Host = pr.In.Host
			for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
				pr.Out.Header.Del(name)
			}
		},
		Transport: &http.Transport{
			// One tunnel per request: a kept-alive connection would hold one
			// of the worker's op streams idle.
			DisableKeepAlives:      true,
			DisableCompression:     true,
			MaxResponseHeaderBytes: exposureHeaderBytes,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := h.x.DialExposure(ctx, row, e.Port)
				if err != nil {
					return nil, err
				}
				return exposureConn{c}, nil
			},
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header = exposureAnswerHeaders(resp.Header, prefix)
			// Trailers would carry headers past the allowlist;
			// exposureWriter drops any written after the body too.
			resp.Trailer = nil
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			setExposureHeaders(w.Header())
			var oe *dispatch.OpError
			if errors.As(err, &oe) && (oe.Class == workspace.ClassPortNotServed || oe.Class == workspace.ClassPortUnreachable) {
				http.Error(w, "no server of the workspace is listening on this page's port", http.StatusBadGateway)
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.DebugContext(r.Context(), "workspace exposure: the application did not answer", "err", err)
			http.Error(w, "the workspace's application did not answer", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(h.gate.limitBytes(newExposureWriter(w), row.ID), r)
}

// exposureWriter is the one way an answer reaches the client, whatever path
// of ReverseProxy writes it (the security reviews of #2931):
//
//   - The head is collected in a map of its own and sent only at the final
//     status: filtered again to the allowlist, Cache-Control forced, and the
//     sandbox policy put on the client's header map right before
//     WriteHeader, so nothing written earlier can strip it.
//   - A 1xx is dropped. ReverseProxy relays one through its own
//     ClientTrace (composed with any the request carries, so a trace of
//     ours cannot stop it), past ModifyResponse, then clears the header map.
//   - Trailers are dropped: ReverseProxy writes them into the header map
//     after the body, which is then a scratch map nobody sends.
type exposureWriter struct {
	w       http.ResponseWriter
	pending http.Header
	sent    bool
	scratch http.Header
}

func newExposureWriter(w http.ResponseWriter) *exposureWriter {
	return &exposureWriter{w: w, pending: http.Header{}}
}

func (e *exposureWriter) Header() http.Header {
	if e.sent {
		if e.scratch == nil {
			e.scratch = http.Header{}
		}
		return e.scratch
	}
	return e.pending
}

func (e *exposureWriter) WriteHeader(code int) {
	if e.sent || code < http.StatusOK {
		return
	}
	e.sent = true
	out := e.w.Header()
	for name, v := range exposureKept(e.pending) {
		out[name] = v
	}
	setExposureHeaders(out)
	e.w.WriteHeader(code)
}

func (e *exposureWriter) Write(b []byte) (int, error) {
	if !e.sent {
		e.WriteHeader(http.StatusOK)
	}
	return e.w.Write(b)
}

func (e *exposureWriter) Flush() {
	if !e.sent {
		e.WriteHeader(http.StatusOK)
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (e *exposureWriter) Unwrap() http.ResponseWriter { return e.w }

// exposureForwarded are the only headers of an application's answer that
// reach the client (the security review of #2931). The answer is served
// from the API's own origin, so a header that acts on the whole origin —
// Clear-Site-Data, Strict-Transport-Security, Access-Control-*, Link,
// Refresh, Service-Worker-Allowed, Origin-Agent-Cluster,
// Permissions-Policy, Alt-Svc, the reporting headers, Set-Cookie — would
// apply to the core, not the page. An allowlist, so a header the next
// browser release gives power to is dropped too. Each is here because a
// page needs it:
var exposureForwarded = []string{
	// What the body is, so the page renders and nosniff holds.
	"Content-Type",
	// The body's own framing and form; ReverseProxy sets Content-Length.
	"Content-Encoding", "Content-Length",
	// A download the page offers.
	"Content-Disposition",
	// Revalidation of what the page loads (the cache itself is forced
	// private below).
	"Etag", "Last-Modified",
	// Seeking in audio and video the page plays (206 answers).
	"Accept-Ranges", "Content-Range",
}

// exposureAnswerHeaders is an application's answer's headers as the client
// gets them: the allowlist, the Location rewritten under the prefix,
// Cache-Control forced private and never stored (a page is one user's), and
// the sandbox policy.
func exposureAnswerHeaders(in http.Header, prefix string) http.Header {
	out := http.Header{}
	for _, name := range exposureForwarded {
		if v := in.Values(name); len(v) > 0 {
			out[name] = append([]string(nil), v...)
		}
	}
	if loc := in.Get("Location"); loc != "" {
		out.Set("Location", underPrefix(loc, prefix))
	}
	out.Set("Cache-Control", "private, no-store")
	setExposureHeaders(out)
	return out
}

// exposureKept is the head the writer sends: the allowlist and Location
// as they are (ModifyResponse rewrote Location already; an error answer has
// none), and Cache-Control forced.
func exposureKept(in http.Header) http.Header {
	out := http.Header{}
	for _, name := range append(exposureForwarded, "Location") {
		if v := in.Values(name); len(v) > 0 {
			out[name] = append([]string(nil), v...)
		}
	}
	out.Set("Cache-Control", "private, no-store")
	return out
}

// setExposureHeaders puts the sandbox policy on an answer.
func setExposureHeaders(h http.Header) {
	h.Set("Content-Security-Policy", exposurePolicy)
	h.Set("X-Content-Type-Options", "nosniff")
}

// underPrefix rewrites a redirect the application sends to a path of its
// own back under the route's prefix. A redirect to another site, or to a
// scheme-relative address, is left as it is: it leaves the page. "/\\x" is
// scheme-relative too (browsers read a backslash as a slash), and so is a
// location holding a control character (browsers strip tabs and line
// breaks, so "/\t/x" is "//x"): neither is a path of the application's own
// (CodeQL go/bad-redirect-check).
func underPrefix(loc, prefix string) string {
	if ownPath(loc) {
		return prefix + loc
	}
	return loc
}

// ownPath reports whether loc is a path on the application's own origin:
// one slash, then anything but a second slash or a backslash, and no
// control character anywhere.
func ownPath(loc string) bool {
	if len(loc) == 0 || loc[0] != '/' {
		return false
	}
	if len(loc) > 1 && (loc[1] == '/' || loc[1] == '\\') {
		return false
	}
	for i := 0; i < len(loc); i++ {
		if c := loc[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// exposureConn is a tunnel as a net.Conn for http.Transport. It has no
// deadlines (the request's context bounds it) and no half-close: some
// servers read a half-closed connection as the client gone.
type exposureConn struct{ io.ReadWriteCloser }

func (exposureConn) LocalAddr() net.Addr              { return exposureAddr{} }
func (exposureConn) RemoteAddr() net.Addr             { return exposureAddr{} }
func (exposureConn) SetDeadline(time.Time) error      { return nil }
func (exposureConn) SetReadDeadline(time.Time) error  { return nil }
func (exposureConn) SetWriteDeadline(time.Time) error { return nil }

type exposureAddr struct{}

func (exposureAddr) Network() string { return "workspace" }
func (exposureAddr) String() string  { return "workspace" }

// exposureGate keeps workspace.expose's per-workspace bounds on this
// replica: requests open at once, requests started per second, and answer
// bytes per second.
type exposureGate struct {
	lim exposeRouteLimits
	mu  sync.Mutex
	by  map[string]*exposureCounters
}

type exposureCounters struct {
	open     int
	requests bucket
	bytes    bucket
	seen     time.Time
}

func newExposureGate(lim exposeRouteLimits) *exposureGate {
	return &exposureGate{lim: lim, by: map[string]*exposureCounters{}}
}

// exposureGateForget is how long a workspace's counters are kept unused
// once the map is large.
const exposureGateForget = 10 * time.Minute

func (g *exposureGate) counters(wsID string, now time.Time) *exposureCounters {
	c := g.by[wsID]
	if c == nil {
		if len(g.by) > 4096 {
			for k, o := range g.by {
				if o.open == 0 && now.Sub(o.seen) > exposureGateForget {
					delete(g.by, k)
				}
			}
		}
		c = &exposureCounters{
			requests: newBucket(float64(g.lim.RequestsPerSecond), float64(2*g.lim.RequestsPerSecond), now),
			bytes:    newBucket(float64(g.lim.BytesPerSecond), float64(2*g.lim.BytesPerSecond), now),
		}
		g.by[wsID] = c
	}
	c.seen = now
	return c
}

// admit counts a request of workspace wsID in, or says why not.
func (g *exposureGate) admit(wsID string) (release func(), why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	c := g.counters(wsID, now)
	if g.lim.MaxConnections > 0 && c.open >= g.lim.MaxConnections {
		return nil, "the workspace's pages have the most requests open they may; try again"
	}
	if g.lim.RequestsPerSecond > 0 && !c.requests.take(1, now) {
		return nil, "the workspace's pages are asked too often; try again shortly"
	}
	c.open++
	return func() {
		g.mu.Lock()
		c.open--
		g.mu.Unlock()
	}, ""
}

// limitBytes wraps w so an answer's body is sent no faster than the
// workspace's byte rate.
func (g *exposureGate) limitBytes(w http.ResponseWriter, wsID string) http.ResponseWriter {
	if g.lim.BytesPerSecond <= 0 {
		return w
	}
	return &rateWriter{ResponseWriter: w, wait: func(n int) time.Duration {
		g.mu.Lock()
		defer g.mu.Unlock()
		now := time.Now()
		return g.counters(wsID, now).bytes.takeOrWait(float64(n), now)
	}}
}

// rateWriter waits for the byte budget before each piece of a write.
type rateWriter struct {
	http.ResponseWriter
	// wait takes n bytes of budget and returns 0, or returns how long
	// until it could.
	wait func(n int) time.Duration
}

// rateWriterPiece is how much of a write waits for budget at once.
const rateWriterPiece = 16 << 10

func (r *rateWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		n := min(len(b), rateWriterPiece)
		for d := r.wait(n); d > 0; d = r.wait(n) {
			time.Sleep(d)
		}
		m, err := r.ResponseWriter.Write(b[:n])
		written += m
		if err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}

func (r *rateWriter) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *rateWriter) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// bucket is a token bucket: rate tokens a second, up to burst.
type bucket struct {
	rate, burst, tokens float64
	at                  time.Time
}

func newBucket(rate, burst float64, now time.Time) bucket {
	return bucket{rate: rate, burst: burst, tokens: burst, at: now}
}

func (b *bucket) refill(now time.Time) {
	b.tokens += now.Sub(b.at).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.at = now
}

func (b *bucket) take(n float64, now time.Time) bool {
	b.refill(now)
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// takeOrWait takes n tokens and returns 0, or returns how long until n
// would be there. A request for more than the burst takes the whole burst,
// so a rate below a piece a second is kept only roughly, never stalled.
func (b *bucket) takeOrWait(n float64, now time.Time) time.Duration {
	b.refill(now)
	if n > b.burst {
		n = b.burst
	}
	if b.tokens >= n {
		b.tokens -= n
		return 0
	}
	return time.Duration((n - b.tokens) / b.rate * float64(time.Second))
}
