package a2a

import (
	"context"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// FrontierAuthenticator builds the A2A frontier Authenticator so the A2A auth
// decision matches the HTTP/gRPC plane. It authenticates every bearer through
// the SAME operator-token substrate (resolve — which also handles the
// legacy-token fallback, so a legacy secret that HTTP has retired once an admin
// token exists is rejected here too), and it is DYNAMIC: the open-mode decision
// (authConfigured) is evaluated per request, so minting the first admin token
// at runtime closes A2A at the same moment it closes HTTP.
//
// Behaviour:
//   - auth NOT configured (true open dev mode) → ("anonymous", true), mirroring
//     the HTTP authMiddleware passthrough when no auth is set.
//   - auth configured + valid bearer → (name, true), attributed by the
//     principal's subject; legacy peers keep the historical "a2a-peer" name.
//     The name is run-attribution only — never an authz allowlist (CLAUDE.md §8).
//   - auth configured + missing/invalid bearer → ("", false); the
//     principalInterceptor then rejects with ErrUnauthenticated before any run
//     spawns.
//
// This fixes the gap where the authenticator was built ONLY from the legacy
// LOOMCYCLE_AUTH_TOKEN: an operator-token-only deployment (no legacy secret —
// the supported RFC AO / no-shell-mint posture) left Deps.Auth nil, so
// principalInterceptor.Before treated every A2A request as authenticated
// anonymous → unauthenticated run-spawn, cancel, and read while HTTP stayed
// gated.
func FrontierAuthenticator(
	authConfigured func(context.Context) bool,
	resolve func(context.Context, string) (auth.Principal, bool),
) Authenticator {
	return func(h http.Header) (string, bool) {
		ctx := context.Background()
		// Per-request open-mode check: no auth configured ⇒ anonymous, matching
		// HTTP. Evaluated every call so a runtime-minted admin token flips A2A
		// closed without a restart.
		if authConfigured == nil || !authConfigured(ctx) {
			return "anonymous", true
		}
		got := h.Get("Authorization")
		const pfx = "Bearer "
		if len(got) <= len(pfx) || !strings.EqualFold(got[:len(pfx)], pfx) {
			return "", false
		}
		if resolve == nil {
			return "", false
		}
		p, ok := resolve(ctx, got[len(pfx):])
		if !ok {
			return "", false
		}
		// Attribution name only. Preserve the legacy peer's historical
		// "a2a-peer" name; attribute operator-token peers by their subject.
		if p.Legacy || p.Subject == "" {
			return "a2a-peer", true
		}
		return p.Subject, true
	}
}
