package builtin

import (
	"context"
	"net"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// guardedDialContext delegates to the shared internal/netguard guard so the
// HTTP/WebFetch tools and the MCP-HTTP client all share ONE dial-time SSRF
// implementation — no weak-copy drift (the class the v1.9.x review found). A
// thin wrapper keeps the existing call site (HTTP.dialContext) unchanged.
//
// netguard.NewGuardedClient (the whole-client form) has no caller in this
// package since the external memory backend was removed; reach for it directly
// from netguard if another tool ever needs a guarded *http.Client.

func guardedDialContext(allowPrivate bool, privateHostAllowlist []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return netguard.GuardedDialContext(allowPrivate, privateHostAllowlist)
}
