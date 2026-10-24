package http

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// sourceIP is the address of the client a request came from, for throttling
// and audit. Behind a reverse proxy (a Cloudflare tunnel, tailscale serve)
// every request's peer is the proxy, so throttling by the peer would let one
// client's failures hold back everyone. A peer listed in auth.trusted_proxies
// may therefore say who its client is — in auth.client_ip_header when that is
// set, else as the right-most X-Forwarded-For entry that is not itself a
// trusted proxy. Any other peer is the client, whatever headers it sends: a
// header from an untrusted peer is the client's own word about itself.
func sourceIP(policy config.AuthPolicy, r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	trusted := func(a netip.Addr) bool {
		for _, p := range policy.TrustedProxies {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.String()
	}
	if h := policy.ClientIPHeader; h != "" {
		// One value, a bare address. Anything else (two copies of the
		// header, a list) is not what the proxy sends, so the peer stands.
		vals := r.Header.Values(h)
		if len(vals) == 1 {
			if a, err := netip.ParseAddr(strings.TrimSpace(vals[0])); err == nil {
				return a.Unmap().String()
			}
		}
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed hop: nothing to its left can be trusted either.
			break
		}
		if a = a.Unmap(); !trusted(a) {
			return a.String()
		}
	}
	return peer.String()
}

// ProxyThrottleWarning is the boot warning for a password gate that is
// evidently behind a TLS-terminating proxy (an https LOOMCYCLE_PUBLIC_URL,
// which the server itself does not serve) with no auth.trusted_proxies: every
// request then comes from the proxy's address, so one client's failed logins
// slow every client's. "" when there is nothing to say.
func ProxyThrottleWarning(cfg *config.Config) string {
	if len(cfg.ResolvedAuth.TrustedProxies) > 0 || !strings.HasPrefix(strings.ToLower(cfg.Env.PublicURL), "https://") {
		return ""
	}
	return "auth: a password gate is configured and LOOMCYCLE_PUBLIC_URL is https, so the server is probably " +
		"behind a TLS-terminating proxy, but auth.trusted_proxies is not set: sign-in throttling then sees every " +
		"client as the proxy's address. Set auth.trusted_proxies (and auth.client_ip_header, e.g. CF-Connecting-IP " +
		"behind a Cloudflare tunnel)"
}

// clientSource is sourceIP under the live config.
func (s *Server) clientSource(r *http.Request) string {
	return sourceIP(s.cfg().ResolvedAuth, r)
}
