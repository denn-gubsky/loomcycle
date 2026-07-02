// Package netguard is the shared SSRF dial-time guard: it refuses to connect to
// private / loopback / link-local addresses (incl. the cloud metadata endpoint
// 169.254.169.254), filtering at DIAL time so DNS-rebinding and redirect targets
// are re-checked on every new connection — not at a one-shot validate. It is a
// leaf (only net / net/http) so every outbound-HTTP caller (the HTTP/WebFetch
// tools, the mem9 MemoryBackend client, the MCP-HTTP client) shares ONE
// implementation and a new caller can't copy the weak first-layer URL check
// without this load-bearing guard — the drift class the v1.9.x security review
// found in the mem9 backend.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// IsPrivateIP reports whether ip is loopback / link-local / multicast /
// unspecified / RFC1918-private. A nil ip is treated as private (fail-closed).
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate()
}

// hostAllowed suffix-matches host against an operator vouch list (case- and
// trailing-dot-insensitive): an entry "internal.example" matches
// "internal.example" and "mcp.internal.example". Empty host / empty entries
// never match. Mirrors the HTTP tool's host matcher so the private-host
// allowlist means the same thing everywhere.
func hostAllowed(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if entry == "" {
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
// opt-out); privateHostAllowlist (suffix-matched) exempts specific hosts an
// operator has vouched are safe on a private network.
func GuardedDialContext(allowPrivate bool, privateHostAllowlist []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		hostExempt := hostAllowed(host, privateHostAllowlist)

		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		candidates := ips
		if !allowPrivate && !hostExempt {
			candidates = candidates[:0]
			for _, ip := range ips {
				if !IsPrivateIP(ip.IP) {
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
				if parsed := net.ParseIP(ip); parsed != nil && IsPrivateIP(parsed) {
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
// always want the block (mem9, the MCP-HTTP client when the operator opts in).
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
