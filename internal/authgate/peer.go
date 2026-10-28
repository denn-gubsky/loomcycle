package authgate

import (
	"net"
	"net/http"
	"net/netip"
)

// PeerAddr is the address of the host that opened the connection a request
// arrived on — the one address no request header can change. It is the base of
// every trusted-proxy decision: the client-address lookup for throttling and
// audit, and the header gates, which accept an identity header only from a
// pinned peer. false when RemoteAddr is not an IP address (a unix socket, a
// test that leaves it empty).
func PeerAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// InPrefixes reports whether a falls in any of prefixes.
func InPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
