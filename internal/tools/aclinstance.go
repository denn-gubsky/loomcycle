package tools

import (
	"context"
	"log/slog"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ §1.5 seam 5 — the per-call INSTANCE question. The narrowings in
// aclnarrow.go answer what a run may reach by type when it starts; a tool
// whose target is only known from the call (the document a path names, the
// credential a name names) asks the instance question here, on every call,
// in a run and off one.

// CallerACL is the compiled set a per-call check asks: inside a run, the
// originating principal's (RunPolicy.ACL, inherited down the tree); off a
// run, the principal the transport stamped (an HTTP, gRPC or MCP substrate
// call). nil: no set — open mode, a trigger firing under a definition, or no
// resolver — and the caller behaves as it did before RFC EY.
func CallerACL(ctx context.Context) *authz.Effective {
	if eff := PolicyOf(ctx).ACL; eff != nil {
		return eff
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return p.Effective
	}
	return nil
}

// ACLAllowsInstance asks whether the set permits action on one instance.
// No set permits everything (the construction-time narrowing already refused
// an enforce-mode run with no set).
func ACLAllowsInstance(ctx context.Context, eff *authz.Effective, action string, res authz.Resource) bool {
	if eff == nil {
		return true
	}
	return gateAllows(ctx, eff, action, res, authz.Evaluate(ctx, eff, action, res, true))
}

// ACLFilterAllows is ACLAllowsInstance for filtering a listing: an instance
// the set refuses is left out of the answer (RFC EZ §1.6, a listing is a
// search), which is not a refusal worth a log line per row.
func ACLFilterAllows(ctx context.Context, eff *authz.Effective, action string, res authz.Resource) bool {
	return eff == nil || aclPermits(ctx, eff, action, res)
}

// ACLAllowsScope asks whether the set permits action on EVERY instance of
// one scope (res.Name is the bare scope): the question for an operation that
// reaches a scope's instances without naming them first.
func ACLAllowsScope(ctx context.Context, eff *authz.Effective, action string, res authz.Resource) bool {
	if eff == nil {
		return true
	}
	return gateAllows(ctx, eff, action, res, authz.EvaluateScope(ctx, eff, action, res, true))
}

// gateAllows folds a gate result into yes/no and logs a refusal or a shadow
// divergence — names and ids only.
func gateAllows(ctx context.Context, eff *authz.Effective, action string, res authz.Resource, r authz.GateResult) bool {
	ok := r.Allowed && !r.Unavailable
	if !ok || r.Diverged {
		event := "acl_denied"
		if ok {
			event = "acl_shadow_diverged"
		}
		slog.Warn("authz: "+event, "tenant", eff.Subject.TenantID, "subject", eff.Subject.ID,
			"action", action, "resource", res.Type+":"+res.Name, "outcome", r.Decision.Outcome.String(),
			"transport", authz.TransportFrom(ctx))
	}
	return ok
}
