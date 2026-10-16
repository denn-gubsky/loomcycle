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
	operatorKeyGateOn bool,
) Authenticator {
	return func(ctx context.Context, h http.Header) (Peer, bool) {
		// Per-request open-mode check: no auth configured ⇒ anonymous, matching
		// HTTP. Evaluated every call so a runtime-minted admin token flips A2A
		// closed without a restart. Open mode is never restricted/isolated (fail-open).
		if authConfigured == nil || !authConfigured(ctx) {
			return Peer{Name: "anonymous"}, true
		}
		got := h.Get("Authorization")
		const pfx = "Bearer "
		if len(got) <= len(pfx) || !strings.EqualFold(got[:len(pfx)], pfx) {
			return Peer{}, false
		}
		if resolve == nil {
			return Peer{}, false
		}
		p, ok := resolve(ctx, got[len(pfx):])
		if !ok {
			return Peer{}, false
		}
		// A credential limited to certain agents is refused here outright.
		// This plane starts whichever agent a card exposes and carries no
		// principal to the run for an allowlist to be checked against, so the
		// only safe answer for a restricted token is no.
		if p.Restrictions.RunPlaneOnly() {
			return Peer{}, false
		}
		// The same holds for a retention mode: it is read off the run's
		// principal, which this plane does not put on the run, so a
		// no-trace token's task would be stored. A rate is held here (the
		// executor takes each fresh run from it, see Peer.Principal), and an
		// override lock holds by construction: a message carries no per-run
		// overrides.
		if p.Restrictions.Retention != "" {
			return Peer{}, false
		}
		// Every peer call is part of starting or following a run this plane
		// starts, so it needs the scope that starts runs.
		if !auth.HasScope(p.Scopes, auth.ScopeRunsCreate) {
			return Peer{}, false
		}
		// RFC AX: derive the operator-key restriction from the peer's OWN
		// resolved scopes — the SAME auth.OperatorKeyRestricted used on the
		// HTTP/gRPC planes, so there's one source of truth. A granular A2A peer
		// that lacks providers:operator-key is restricted; a legacy peer / one
		// holding the scope / the gate-off case all fail open (false).
		restricted := auth.OperatorKeyRestricted(p, ok, operatorKeyGateOn)
		// RFC BX P2b: derive the isolation bit from the peer's OWN resolved scopes
		// — the SAME auth.IsIsolated used on the HTTP plane. A substrate:user peer
		// is isolated; admin/tenant/legacy/open-mode all fail open (false).
		isolated := auth.IsIsolated(p, ok)
		// Attribution name only. Preserve the legacy peer's historical
		// "a2a-peer" name; attribute operator-token peers by their subject.
		peer := Peer{
			Name:        p.Subject,
			Restricted:  restricted,
			Isolated:    isolated,
			Tenant:      p.TenantID,
			TenantBound: !p.Legacy && !auth.HasScope(p.Scopes, auth.ScopeAdmin),
			Principal:   &p,
		}
		if p.Legacy || p.Subject == "" {
			peer.Name = "a2a-peer"
		}
		return peer, true
	}
}
