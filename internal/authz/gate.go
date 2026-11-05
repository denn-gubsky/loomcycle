package authz

import (
	"context"
	"net"
	"time"
)

// The transport gates share one evaluation (Evaluate) and read two
// request-wide facts from ctx: the rollout mode and the transport. Both are
// stamped by the surface that received the request, never by its body.

type ctxKeyMode struct{}
type ctxKeyTransport struct{}
type ctxKeySourceIP struct{}
type ctxKeyRunFacts struct{}

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

// WithSourceIP stamps the client address a request came from: the HTTP peer,
// or the client a trusted proxy names for it (auth.trusted_proxies), or the
// gRPC peer. Never the request's own word about itself.
func WithSourceIP(ctx context.Context, ip net.IP) context.Context {
	if ip == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeySourceIP{}, ip)
}

// SourceIPFrom returns the stamped client address, or nil.
func SourceIPFrom(ctx context.Context) net.IP {
	ip, _ := ctx.Value(ctxKeySourceIP{}).(net.IP)
	return ip
}

// RunFacts is what a condition may ask about the run a question is asked in
// (or, at a run intake, the run about to start): its agent, its spawn depth
// (0 for a top-level run), whether it is interactive, and the model and user
// tiers it routes by. The run entry points stamp it; a request outside any
// run carries none, and reads as a top-level, non-interactive caller with no
// agent or tier.
type RunFacts struct {
	Agent       string
	Depth       int
	Interactive bool
	Tier        string
	UserTier    string
}

// WithRunFacts stamps the run facts, replacing any a parent run stamped.
func WithRunFacts(ctx context.Context, f RunFacts) context.Context {
	return context.WithValue(ctx, ctxKeyRunFacts{}, f)
}

// RunFactsFrom returns the stamped run facts.
func RunFactsFrom(ctx context.Context) (RunFacts, bool) {
	f, ok := ctx.Value(ctxKeyRunFacts{}).(RunFacts)
	return f, ok
}

// ContextFrom is the condition context of a question asked under ctx: the
// transport, the client address and the run facts the surfaces stamped, and
// the clock now. Every gate decides with it, so a condition reads the same
// facts wherever it is asked.
func ContextFrom(ctx context.Context) Context {
	f, _ := RunFactsFrom(ctx)
	return Context{
		Transport:   TransportFrom(ctx),
		Now:         time.Now(),
		SourceIP:    SourceIPFrom(ctx),
		Agent:       f.Agent,
		Depth:       f.Depth,
		Interactive: f.Interactive,
		Tier:        f.Tier,
		UserTier:    f.UserTier,
	}
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
		Context:  ContextFrom(ctx),
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
		Context:  ContextFrom(ctx),
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
