package authz

import (
	"context"
	"time"
)

// The transport gates share one evaluation (Evaluate) and read two
// request-wide facts from ctx: the rollout mode and the transport. Both are
// stamped by the surface that received the request, never by its body.

type ctxKeyMode struct{}
type ctxKeyTransport struct{}

// WithMode stamps the rollout mode a request is evaluated under.
func WithMode(ctx context.Context, m Mode) context.Context {
	return context.WithValue(ctx, ctxKeyMode{}, m)
}

// ModeFrom returns the stamped mode; compat when none is stamped (a path
// nobody wired the resolver into decides as it did before RFC EY).
func ModeFrom(ctx context.Context) Mode {
	if m, ok := ctx.Value(ctxKeyMode{}).(Mode); ok && m != "" {
		return m
	}
	return ModeCompat
}

// WithTransport stamps which surface the request arrived on (one of
// Transports).
func WithTransport(ctx context.Context, t string) context.Context {
	return context.WithValue(ctx, ctxKeyTransport{}, t)
}

// TransportFrom returns the stamped transport, or "".
func TransportFrom(ctx context.Context) string {
	t, _ := ctx.Value(ctxKeyTransport{}).(string)
	return t
}

// GateResult is what a transport gate does with an evaluation.
type GateResult struct {
	// Allowed is the final answer under the mode.
	Allowed bool
	// Unavailable is set when enforce mode had no compiled set to decide
	// with (the resolver failed): the request is refused, and the refusal
	// is a 503-shaped "try again", not a 403.
	Unavailable bool
	// Diverged is set in shadow mode when the engine and the legacy gate
	// disagree; the caller audits it.
	Diverged bool
	// Evaluated is set when the engine actually decided (a set was
	// present); Decision is then meaningful.
	Evaluated bool
	Decision  Decision
}

// Evaluate is the one gate every transport calls after its legacy check:
// legacyAllowed is what the scope gate said. eff may be nil (no resolver,
// or the store could not be read).
func Evaluate(ctx context.Context, eff *Effective, action string, res Resource, legacyAllowed bool) GateResult {
	return evaluate(ctx, eff, action, res, legacyAllowed, (*Effective).Decide)
}

// EvaluateScope is Evaluate for a question about every instance of one scope
// (Effective.DecideScope); res.Name is the bare scope.
func EvaluateScope(ctx context.Context, eff *Effective, action string, res Resource, legacyAllowed bool) GateResult {
	return evaluate(ctx, eff, action, res, legacyAllowed, (*Effective).DecideScope)
}

func evaluate(ctx context.Context, eff *Effective, action string, res Resource, legacyAllowed bool, decide func(*Effective, Request) Decision) GateResult {
	mode := ModeFrom(ctx)
	if eff == nil {
		if mode == ModeEnforce {
			return GateResult{Unavailable: true}
		}
		return GateResult{Allowed: legacyAllowed}
	}
	if res.TenantID == "" {
		res.TenantID = eff.Subject.TenantID
	}
	d := decide(eff, Request{
		Subject:  eff.Subject,
		Action:   action,
		Resource: res,
		Context:  Context{Transport: TransportFrom(ctx), Now: time.Now()},
	})
	out := GateResult{Evaluated: true, Decision: d, Allowed: Resolve(mode, d, legacyAllowed)}
	if mode == ModeShadow {
		out.Diverged = Diverges(d, legacyAllowed)
	}
	return out
}

// EvaluateSurface is Evaluate for a transport surface.
func EvaluateSurface(ctx context.Context, eff *Effective, s Surface, legacyAllowed bool) GateResult {
	tenant := ""
	if eff != nil {
		tenant = eff.Subject.TenantID
	}
	return Evaluate(ctx, eff, s.Action, s.Resource(tenant), legacyAllowed)
}

// Grants reports whether a rule in eff explicitly ALLOWS action on res,
// whatever the rollout mode. It is the question for a verb that is only ever
// held by grant and never by a scope (RFC FS `review` on documents): in compat
// and shadow mode an unmatched question is decided by the scope gate, which
// would hand such a verb to every principal the route admits. No set grants
// nothing.
func Grants(ctx context.Context, eff *Effective, action string, res Resource) bool {
	if eff == nil {
		return false
	}
	if res.TenantID == "" {
		res.TenantID = eff.Subject.TenantID
	}
	d := eff.Decide(Request{
		Subject:  eff.Subject,
		Action:   action,
		Resource: res,
		Context:  Context{Transport: TransportFrom(ctx), Now: time.Now()},
	})
	return d.Outcome == Allowed
}

// GrantsSomewhere reports whether eff could allow action on some instance of
// typ: an unconditional allow names one, or a conditional rule might. A caller
// asking for a grant-only verb with none of either holds it nowhere, and is
// refused as a whole rather than answered item by item.
func GrantsSomewhere(eff *Effective, typ, action string) bool {
	if eff == nil {
		return false
	}
	return len(eff.AllowPatterns(typ, action)) > 0 || eff.HasConditionalRules(typ, action)
}
