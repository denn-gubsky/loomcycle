package a2a

import (
	"context"
	"errors"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/store"
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

// authorizeTaskTenant enforces the host/path-authoritative tenant on the
// A2A read/cancel paths, where the caller-supplied A2A Task.id IS the
// loomcycle agent_id — a non-secret, addressable handle. Without it a peer
// authenticated on one tenant's routed host could resolve a run owned by
// another tenant (cross-tenant cancel is destructive; cross-tenant get
// leaks run status + the raw ErrorMsg). The create path already honours
// the routed tenant via principalFromContext; this closes the read/cancel
// side, which previously resolved purely by agent_id.
//
// It resolves the run behind agentID and the tenant of its owning session
// (the runs table has no tenant column — tenant lives on the session, see
// store.Session.TenantID) and compares that to the authoritative routed
// tenant stamped by the mounting layer (WithRoutedTenant).
//
// Returns:
//   - nil in single-tenant / none mode (RoutedTenantFrom ok==false): there
//     is no cross-tenant surface, so this is a no-op and behaviour is
//     unchanged — no store round-trips are made.
//   - nil when the run's session tenant equals the routed tenant.
//   - a2asdk.ErrTaskNotFound on a cross-tenant mismatch, OR when the run /
//     session is not found. ErrTaskNotFound (not a distinct "forbidden")
//     is deliberate: a distinct error is an oracle that confirms the task
//     exists under another tenant. Failing closed on a not-found run also
//     covers the brief window between SDK task Create and run-row
//     persistence — a just-created task is hidden from a cross-tenant
//     probe rather than leaked.
//   - the underlying store error for any non-not-found failure, so a real
//     storage fault is not masked as "task not found".
func authorizeTaskTenant(ctx context.Context, runs RunReader, agentID string) error {
	routed, ok := RoutedTenantFrom(ctx)
	if !ok {
		return nil
	}
	run, err := runs.GetRunByAgentID(ctx, agentID)
	if err != nil {
		return notFoundAsTaskNotFound(err)
	}
	sess, err := runs.GetSession(ctx, run.SessionID)
	if err != nil {
		return notFoundAsTaskNotFound(err)
	}
	if sess.TenantID != routed {
		return a2asdk.ErrTaskNotFound
	}
	return nil
}

// notFoundAsTaskNotFound maps a loomcycle store-not-found to the SDK's
// a2asdk.ErrTaskNotFound sentinel (so the A2A frontier returns a proper
// task-not-found), while passing any other error through unchanged.
func notFoundAsTaskNotFound(err error) error {
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return a2asdk.ErrTaskNotFound
	}
	return err
}
