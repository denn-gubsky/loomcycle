package a2a

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// maxPeerResponseBytes caps any single response body loomcycle reads from
// an A2A peer — the discovery card fetch and every SendMessage response.
// A2A AgentCards and task results are small JSON documents; a few MiB is
// generous. The cap is enforced by erroring past the limit rather than
// silently truncating (a truncated body would only fail JSON parsing later
// with a misleading error).
//
// Without it the SDK transports read peer responses with an unbounded
// io.ReadAll / json.Decode (a2aclient/agentcard resolver + jsonrpc/rest),
// so a hostile, compromised, or simply buggy peer could stream a multi-GB
// body and OOM the process within the request timeout (the timeout bounds
// wall-clock, not bytes). Every other outbound HTTP read in loomcycle is
// already bounded this way (providers/*, builtin/httptool, …); the
// A2A peer path was the lone exception.
const maxPeerResponseBytes = 4 << 20 // 4 MiB

// peerDialTimeout bounds the TCP connect to a peer address.
const peerDialTimeout = 10 * time.Second

// hardenedPeerClient builds the *http.Client loomcycle hands to the A2A SDK
// for talking to a peer: the card resolver (fetchPeerCard) and the
// JSON-RPC / REST transports (via WithJSONRPCTransport / WithRESTTransport).
// It layers three defences the SDK's default client lacks:
//
//   - body cap: response bodies are wrapped in a limit reader that ERRORS
//     past maxPeerResponseBytes, so the SDK's unbounded reads cannot OOM
//     the process.
//   - SSRF block: the dialer refuses private / loopback / link-local /
//     metadata-service addresses. A2AAgentDef agent_card_url / endpoint can
//     be model-authored (via a granted def-scope fork overlay), so a peer
//     URL must not be a lever to reach internal hosts. The block is
//     re-checked at the socket layer (Control) to defeat DNS rebinding.
//   - redirect bound: redirects are capped and every hop's destination is
//     re-validated, so a benign-looking public host cannot 302 into the
//     metadata service.
//
// timeout bounds the whole request (connect + headers + body).
func hardenedPeerClient(timeout time.Duration) *http.Client {
	base := &http.Transport{
		DialContext:           peerDialContext,
		TLSHandshakeTimeout:   peerDialTimeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &bodyLimitRoundTripper{base: base, limit: maxPeerResponseBytes},
		// Bound redirects and re-validate each hop. The default client
		// follows up to 10 redirects to attacker-chosen targets; the
		// per-hop dial still passes through peerDialContext, so a redirect
		// to a private IP is already refused at dial — this also caps the
		// hop count defensively.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("a2a: stopped after %d redirects", len(via))
			}
			return nil
		},
	}
}

// bodyLimitRoundTripper wraps a RoundTripper so every response body is
// bounded to limit bytes; a read past the cap returns errResponseTooLarge.
type bodyLimitRoundTripper struct {
	base  http.RoundTripper
	limit int64
}

func (rt *bodyLimitRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: rt.limit}
	return resp, nil
}

// errResponseTooLarge is returned by a peer-response read once the body
// exceeds maxPeerResponseBytes. Surfaced as a tool error, never silently.
var errResponseTooLarge = fmt.Errorf("a2a: peer response exceeds %d-byte cap", maxPeerResponseBytes)

// cappedBody errors (rather than silently truncating) once more than
// `remaining` bytes have been read, so an over-cap body fails loudly
// instead of being parsed as a truncated document.
type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		// Read at most remaining+1 so we can detect the overflow byte
		// without unbounded buffering.
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n, errResponseTooLarge
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// peerDialContext is the SSRF-blocking dialer: it resolves the host, dials
// only its public addresses, and re-checks the socket-level address to
// defeat DNS rebinding. Mirrors the canonical guard in
// internal/tools/builtin/httptool.go (isPrivateIP / dialContext) — kept as
// a small local copy rather than a shared dependency so the A2A trust path
// is self-contained; the predicates are stdlib net.IP classifiers, so
// there is no algorithm to drift.
func peerDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	public := ips[:0]
	for _, ip := range ips {
		if !isPrivatePeerIP(ip.IP) {
			public = append(public, ip)
		}
	}
	if len(public) == 0 {
		return nil, fmt.Errorf("a2a: refusing to dial %q — no public addresses (got %d private)", host, len(ips))
	}
	d := net.Dialer{
		Timeout: peerDialTimeout,
		Control: func(network, address string, c syscall.RawConn) error {
			ipStr, _, _ := net.SplitHostPort(address)
			if parsed := net.ParseIP(ipStr); parsed != nil && isPrivatePeerIP(parsed) {
				return fmt.Errorf("a2a: refusing to connect to private address %s", ipStr)
			}
			return nil
		},
	}
	var lastErr error
	for _, ip := range public {
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// isPrivatePeerIP reports whether ip is one loomcycle must not let a peer
// URL reach: loopback, link-local (169.254/16 + fe80::/10 — incl. the
// cloud metadata service at 169.254.169.254), multicast, unspecified, or
// RFC1918 / ULA private space. Same classifier set as httptool.isPrivateIP.
func isPrivatePeerIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate()
}
