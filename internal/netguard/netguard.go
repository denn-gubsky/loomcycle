// Package netguard is the shared SSRF dial-time guard: it refuses to connect to
// private / loopback / link-local addresses (incl. the cloud metadata endpoint
// 169.254.169.254), filtering at DIAL time so DNS-rebinding and redirect targets
// are re-checked on every new connection — not at a one-shot validate. It is a
// leaf (only the standard library's net packages) so every outbound-HTTP caller
// (the HTTP/WebFetch tools, the MCP-HTTP client, hooks, A2A peers) shares ONE
// implementation and a new caller can't copy the weak first-layer URL check
// without this load-bearing guard — the drift class the v1.9.x security review
// found in a since-removed external memory-backend client.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// nonPublicV4 lists the IPv4 special-purpose ranges that are not globally
// reachable but that the stdlib classifiers (loopback / link-local / multicast /
// unspecified / RFC1918) miss. 100.64.0.0/10 is the one that matters most: it
// is carrier-grade NAT space, which is where Tailscale puts every tailnet peer
// and where Alibaba Cloud serves its metadata endpoint (100.100.100.200).
var nonPublicV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network": 0.x.y.z reaches the local host on Linux
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT shared address space
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, incl. broadcast 255.255.255.255
}

// nonPublicV6 lists IPv6 ranges refused outright rather than unwrapped. The
// local-use NAT64 prefix is operator-chosen in length (/48 to /96), so where the
// IPv4 sits inside it is not knowable here; it is never globally routed anyway.
var nonPublicV6 = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use IPv4/IPv6 translation
}

// embeddingV6 lists the IPv6 forms that carry an IPv4 address a gateway or the
// host stack forwards to, so the carried address is re-checked. Without this,
// 64:ff9b::a9fe:a9fe is the metadata service spelled as a "public" IPv6 address.
// v4-mapped (::ffff:a.b.c.d) never gets here: it is unmapped to IPv4 first.
var embeddingV6 = []struct {
	prefix netip.Prefix
	offset int // byte offset of the embedded IPv4 inside the 16 bytes
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},    // well-known NAT64 prefix
	{netip.MustParsePrefix("::ffff:0:0:0/96"), 12}, // IPv4-translated
	{netip.MustParsePrefix("::/96"), 12},           // IPv4-compatible (deprecated)
	{netip.MustParsePrefix("2002::/16"), 2},        // 6to4
}

// IsPrivateIP reports whether ip is an address an outbound call must not reach
// unless the operator vouched for it: loopback, link-local (incl. the cloud
// metadata service 169.254.169.254), multicast, unspecified, RFC1918 / ULA, the
// other IPv4 ranges that are not globally reachable (CGNAT, benchmarking, "this
// network", reserved), and any IPv6 form that embeds such an IPv4. A nil or
// malformed ip is treated as private (fail-closed).
func IsPrivateIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return isPrivateAddr(addr.Unmap())
}

func isPrivateAddr(a netip.Addr) bool {
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsPrivate() {
		return true
	}
	if a.Is4() {
		return inAnyPrefix(a, nonPublicV4)
	}
	if inAnyPrefix(a, nonPublicV6) {
		return true
	}
	b := a.As16()
	for _, e := range embeddingV6 {
		if e.prefix.Contains(a) {
			// The extracted address is IPv4, so this recursion is one level deep.
			return isPrivateAddr(netip.AddrFrom4([4]byte(b[e.offset : e.offset+4])))
		}
	}
	return false
}

func inAnyPrefix(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// isCIDREntry reports whether a private-host allowlist entry is a CIDR range
// rather than a host name. A host name never contains '/'.
func isCIDREntry(entry string) bool { return strings.Contains(entry, "/") }

// ValidatePrivateHostAllowlist checks the CIDR entries of a private-host
// allowlist, so a typo fails at config load instead of silently never matching
// (the dial guard skips a malformed range: fail-closed, but invisible). Host-name
// entries are not checked; any string is a valid suffix to match.
func ValidatePrivateHostAllowlist(entries []string) error {
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if !isCIDREntry(e) {
			continue
		}
		if _, err := netip.ParsePrefix(e); err != nil {
			return fmt.Errorf("entry %q is not a valid CIDR range (want e.g. 100.64.0.0/10 or 100.101.102.103/32): %v", e, err)
		}
	}
	return nil
}

// splitAllowlist separates an operator vouch list into host-name entries
// (suffix-matched against the dialed host) and CIDR entries (matched against
// each resolved address). A malformed CIDR is dropped: it can never match.
func splitAllowlist(entries []string) (hosts []string, cidrs []netip.Prefix) {
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if !isCIDREntry(e) {
			hosts = append(hosts, e)
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			cidrs = append(cidrs, p.Masked())
		}
	}
	return hosts, cidrs
}

// ipAllowed reports whether ip falls inside one of the operator's CIDR entries.
func ipAllowed(ip net.IP, cidrs []netip.Prefix) bool {
	if len(cidrs) == 0 {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	return ok && inAnyPrefix(addr.Unmap(), cidrs)
}

// IPLiteralAllowed reports whether host is an IP literal inside one of the CIDR
// entries of a private-host allowlist. Authoring-time checks use it so a def
// naming a tailnet IP is judged the way the dial guard will judge it.
func IPLiteralAllowed(host string, allowlist []string) bool {
	host = strings.Trim(host, "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	_, cidrs := splitAllowlist(allowlist)
	return ipAllowed(ip, cidrs)
}

// hostAllowed suffix-matches host against an operator vouch list (case- and
// trailing-dot-insensitive): an entry "internal.example" matches
// "internal.example" and "mcp.internal.example". Empty host / empty entries
// never match. Mirrors the HTTP tool's host matcher so the private-host
// allowlist means the same thing everywhere. CIDR entries are matched against
// resolved addresses (ipAllowed), never against the host name.
func hostAllowed(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if entry == "" || isCIDREntry(entry) {
			continue
		}
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// GuardedDialContext returns a DialContext that resolves the host and dials only
// public addresses, with a socket-level Control re-check after the OS resolves
// the address. allowPrivate lifts the block entirely (tests / an operator
// opt-out); privateHostAllowlist exempts what an operator has vouched is safe on
// a private network: a host-name entry (suffix-matched) exempts that host
// whatever it resolves to, and a CIDR entry ("100.64.0.0/10") exempts only the
// resolved addresses inside the range, so a vouched range cannot be used to
// reach a private address outside it.
func GuardedDialContext(allowPrivate bool, privateHostAllowlist []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	exemptHosts, exemptCIDRs := splitAllowlist(privateHostAllowlist)
	blocked := func(ip net.IP) bool { return IsPrivateIP(ip) && !ipAllowed(ip, exemptCIDRs) }
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		hostExempt := hostAllowed(host, exemptHosts)

		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		candidates := ips
		if !allowPrivate && !hostExempt {
			candidates = candidates[:0]
			for _, ip := range ips {
				if !blocked(ip.IP) {
					candidates = append(candidates, ip)
				}
			}
			if len(candidates) == 0 {
				return nil, fmt.Errorf("blocked: %s has no public addresses (got %d private)", host, len(ips))
			}
		}
		d := net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				if allowPrivate || hostExempt {
					return nil
				}
				ip, _, _ := net.SplitHostPort(address)
				if parsed := net.ParseIP(ip); parsed != nil && blocked(parsed) {
					return fmt.Errorf("blocked: socket-level address %s is private", ip)
				}
				return nil
			},
		}
		var lastErr error
		for _, ip := range candidates {
			conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, lastErr
	}
}

// NewGuardedClient returns an *http.Client that BLOCKS private-IP dials (subject
// to privateHostAllowlist) and bounds redirects — each hop re-dials through the
// guard, so a 302 to an internal/metadata IP is refused too. For callers that
// always want the block (the MCP-HTTP client when the operator opts in).
func NewGuardedClient(timeout time.Duration, privateHostAllowlist []string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: GuardedDialContext(false, privateHostAllowlist)},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}
