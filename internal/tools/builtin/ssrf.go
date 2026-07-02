package builtin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// guardedDialContext is the shared SSRF-blocking dialer used by the HTTP/WebFetch
// tools AND the mem9 MemoryBackend client. It resolves the host and refuses to
// connect to any private / loopback / link-local address (incl. the cloud
// metadata endpoint 169.254.169.254) — filtering at DIAL time, so DNS-rebinding
// and redirect targets are re-checked on every new connection, not just at a
// one-shot validate. A socket-level Control hook re-asserts the block after the
// OS resolves the address.
//
// allowPrivate lifts the block entirely (tests / an operator opt-in). Otherwise
// privateHostAllowlist (suffix-matched via hostAllowed) exempts specific hosts
// the operator has vouched are safe to reach on a private network. Extracted
// from HTTP.dialContext so a new caller (mem9) can't copy the weak first-layer
// URL check without this load-bearing dial-time guard — the exact class of gap
// the v1.9.x security review found in the mem9 backend.
func guardedDialContext(allowPrivate bool, privateHostAllowlist []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
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
				if !isPrivateIP(ip.IP) {
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
				if parsed := net.ParseIP(ip); parsed != nil && isPrivateIP(parsed) {
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

// newSSRFGuardedClient returns an *http.Client whose transport dials through
// guardedDialContext (private IPs blocked, subject to privateHostAllowlist) and
// whose redirects are bounded — each redirect hop is a fresh connection that
// re-runs the dial guard, so a 302 to an internal/metadata IP is refused too.
// Used for the mem9 backend client, which was previously a plain http.Client
// with no SSRF guard (a model-authored base_url could reach 169.254.169.254 and
// exfiltrate the allowlisted X-API-Key).
func newSSRFGuardedClient(timeout time.Duration, privateHostAllowlist []string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: guardedDialContext(false, privateHostAllowlist)},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}
