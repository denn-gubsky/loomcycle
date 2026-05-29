package a2a

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// principal is the authenticated identity the bridge threads into a
// loomcycle run: the tenant (A2A "agent owner") and the user name. It
// is derived ONLY from an already-authenticated source — the SDK's
// CallContext.User, populated by whatever auth middleware the A2A
// transport mounted (slice A2A-5). This helper does NOT authenticate;
// it does NOT replace loomcycle's bearer authMiddleware. It reads a
// decision already made upstream and shapes it for RunInput.
//
// Trust-boundary note: UserID/TenantID flow into run attribution and
// per-tenant fairness accounting, not into any authz allowlist. The
// allowlist floor stays operator-config-authoritative (CLAUDE.md §8),
// so an attacker who could forge a principal here still cannot widen
// host policy — they only mislabel run ownership.
type principal struct {
	// TenantID is the A2A tenant (agent owner). Empty when the request
	// carried no tenant.
	TenantID string
	// UserID is the authenticated user name, empty when the request
	// was unauthenticated (User.Authenticated == false) or no
	// CallContext was attached.
	UserID string
	// Authenticated mirrors the SDK User.Authenticated flag so callers
	// can default-deny (e.g. reject anonymous runs) at the route layer.
	Authenticated bool
}

// principalFromContext extracts the authenticated principal from the
// SDK CallContext on ctx, combined with the request tenant. The tenant
// is passed explicitly because the SDK carries it on the request
// (SendMessageRequest.Tenant / ExecutorContext.Tenant), not only on the
// CallContext — passing it in keeps this helper a pure projection with
// no hidden context lookups beyond the CallContext it documents.
//
// Returns a zero principal (Authenticated=false) when no CallContext is
// present — the unauthenticated default. Never panics on a nil User.
func principalFromContext(ctx context.Context, tenant string) principal {
	p := principal{TenantID: tenant}
	callCtx, ok := a2asrv.CallContextFrom(ctx)
	if !ok || callCtx.User == nil {
		return p
	}
	p.Authenticated = callCtx.User.Authenticated
	if callCtx.User.Authenticated {
		p.UserID = callCtx.User.Name
	}
	// CallContext.Tenant() is the authoritative tenant when the
	// transport set it; prefer it over the request-carried value only
	// when the request didn't supply one, so an explicit per-message
	// tenant still wins.
	if p.TenantID == "" {
		p.TenantID = callCtx.Tenant()
	}
	return p
}
