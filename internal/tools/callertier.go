package tools

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Tier is who is calling a tool, as the transport that admitted the call
// established it (RFC FY Part B). A gate that decides for the tenant — confirm
// or edit the ontology, author its review policy, rule on a proposal outside
// the caller's own scope, record a definition as the operator's — asks which
// tier, never whether "the operator marker" is set: a two-valued marker that
// every off-run call carried made a tenant member the operator of the tenant.
//
// EXPLICIT, and never derived from the identity the off-run path stamps: that
// path uses a synthetic agent id, and POST /v1/runs accepts a caller-supplied
// agent_id, so a run could claim it. A tier has no wire form.
type Tier uint8

const (
	// TierRun is a principal acting inside a run — a model, whatever token
	// started it — and anything no transport stamped. It is the zero value on
	// purpose: absent means "not the operator", so a path that forgot to stamp
	// fails closed, and the spawn boundary stamps it explicitly to take an
	// operator plane's tier off a child it starts.
	TierRun Tier = iota
	// TierIsolated is an isolated member (access_mode: isolated, substrate:user
	// topped), off-run.
	TierIsolated
	// TierMember is a non-isolated tenant member (RFC CB) off-run: a user
	// admitted to the tenant surfaces without being their operator.
	TierMember
	// TierOperatorPlane is the operator's own off-run call: substrate:admin, the
	// legacy shared token, open mode (no principal), and substrate:tenant within
	// its own tenant.
	TierOperatorPlane
)

func (t Tier) String() string {
	switch t {
	case TierRun:
		return "run"
	case TierIsolated:
		return "isolated"
	case TierMember:
		return "member"
	case TierOperatorPlane:
		return "operator_plane"
	}
	return "unknown"
}

type ctxKeyCallerTier struct{}

// WithCallerTier stamps who is calling. Each transport's off-run ctx builder
// stamps TierForPrincipal; the spawn boundary stamps TierRun.
func WithCallerTier(ctx context.Context, t Tier) context.Context {
	return context.WithValue(ctx, ctxKeyCallerTier{}, t)
}

// CallerTier returns the stamped tier, TierRun when none was stamped.
func CallerTier(ctx context.Context) Tier {
	t, _ := ctx.Value(ctxKeyCallerTier{}).(Tier)
	return t
}

// IsOperatorPlane reports the operator's own off-run call. The run id is asked
// too: the spawn boundary restamps TierRun, and a run id on ctx means a run
// reached the call whatever an ancestor stamped.
func IsOperatorPlane(ctx context.Context) bool {
	return CallerTier(ctx) == TierOperatorPlane && RunID(ctx) == ""
}

// IsOffRunPlane reports an off-run call through any transport's substrate
// plane, whoever the caller: the operator, a member or an isolated member — a
// person's call, as opposed to a run's.
func IsOffRunPlane(ctx context.Context) bool {
	return CallerTier(ctx) != TierRun && RunID(ctx) == ""
}

// TierForPrincipal is the tier of an off-run call by principal p (ok = one is
// present) acting in tenant actingTenant. A substrate:tenant principal is the
// operator plane only in its own tenant; acting anywhere else (which no
// transport lets it do) it is held to a member's tier rather than trusted.
func TierForPrincipal(p auth.Principal, ok bool, actingTenant string) Tier {
	switch {
	case !ok, p.Legacy, auth.HasScope(p.Scopes, auth.ScopeAdmin):
		return TierOperatorPlane
	case auth.IsIsolated(p, ok):
		return TierIsolated
	case auth.HasScope(p.Scopes, auth.ScopeTenant) && actingTenant == p.TenantID:
		return TierOperatorPlane
	}
	return TierMember
}
