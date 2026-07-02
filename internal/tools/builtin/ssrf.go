package builtin

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// guardedDialContext + newSSRFGuardedClient delegate to the shared
// internal/netguard guard so the HTTP/WebFetch tools, the mem9 backend client,
// and the MCP-HTTP client all share ONE dial-time SSRF implementation — no
// weak-copy drift (the class the v1.9.x review found). Thin wrappers keep the
// existing call sites (HTTP.dialContext, buildMem9) unchanged.

func guardedDialContext(allowPrivate bool, privateHostAllowlist []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return netguard.GuardedDialContext(allowPrivate, privateHostAllowlist)
}

func newSSRFGuardedClient(timeout time.Duration, privateHostAllowlist []string) *http.Client {
	return netguard.NewGuardedClient(timeout, privateHostAllowlist)
}
