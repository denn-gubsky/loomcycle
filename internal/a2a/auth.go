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

// routedTenantKeyType keys the tenant the A2A-5 mounting layer resolved
// from the request host/path. It is the TRUST-BOUNDARY tenant: when
// present it OVERRIDES any tenant the peer supplied in the message body
// or SDK CallContext, so a peer cannot mislabel its run's tenant by
// stuffing a body field. The mounting layer attaches it via
// WithRoutedTenant before the SDK handler runs.
type routedTenantKeyType struct{}

// WithRoutedTenant stamps the host/path-derived tenant onto ctx. The
// A2A server mux calls this whenever a tenancy routing mode is active so
// principalFromContext treats the routing decision as authoritative.
//
// The empty tenant is stamped too (not a no-op): an active routing mode
// that resolves NO tenant — a bare/non-tenant host, or a binding route
// reached without a tenant prefix — must still be authoritative, so the
// peer-supplied body tenant is never consulted in a multi-tenant
// deployment. Presence of the value (even "") is the "routing decided"
// signal; only "none"/single-tenant mode, which never calls this, leaves
// it absent and permits the body-tenant fallback.
func WithRoutedTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, routedTenantKeyType{}, tenant)
}

// RoutedTenantFrom returns the host/path-derived tenant (possibly "")
// when a routing mode stamped one, and ok=false when no routing decision
// was made (single-tenant / none mode). Exported so the A2A-5 server
// surface can read back the tenant it stamped (e.g. to anchor per-tenant
// AgentCard URLs).
func RoutedTenantFrom(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(routedTenantKeyType{}).(string)
	return t, ok
}

// principalFromContext extracts the authenticated principal from the
// SDK CallContext on ctx, combined with the request tenant. The tenant
// argument is the SDK-carried value (SendMessageRequest.Tenant /
// ExecutorContext.Tenant); it is used ONLY when no routed tenant is
// present on ctx. The routed tenant (host/path-derived, see
// WithRoutedTenant) is a trust boundary and always wins when set, so a
// body field cannot override the operator-configured routing.
//
// Returns a zero principal (Authenticated=false) when no CallContext is
// present — the unauthenticated default. Never panics on a nil User.
func principalFromContext(ctx context.Context, tenant string) principal {
	// A routing mode (host/path) is authoritative even when it resolves an
	// EMPTY tenant: in that mode the peer-supplied body tenant must never
	// govern attribution, otherwise a peer reaching a non-tenant host (host
	// mode) or an un-prefixed binding route (path mode) could mislabel its
	// run's tenant by stuffing a body field. The body/CallContext tenant is
	// consulted ONLY in single-tenant / none mode (no routing decision).
	routed, authoritative := RoutedTenantFrom(ctx)
	var p principal
	if authoritative {
		p.TenantID = routed
	} else {
		p.TenantID = tenant
	}
	callCtx, ok := a2asrv.CallContextFrom(ctx)
	if !ok || callCtx.User == nil {
		return p
	}
	p.Authenticated = callCtx.User.Authenticated
	if callCtx.User.Authenticated {
		p.UserID = callCtx.User.Name
	}
	// CallContext.Tenant() is the SDK-carried fallback — but only in
	// single-tenant mode. In a routing mode it stays suppressed (the
	// routed decision, empty or not, already won above).
	if !authoritative && p.TenantID == "" {
		p.TenantID = callCtx.Tenant()
	}
	return p
}
