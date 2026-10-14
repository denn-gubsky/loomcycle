package providers

import (
	"context"
	"errors"
)

// ErrOperatorKeyForbidden is the RFC AX Layer-2 backstop refusal: a RESTRICTED
// run (OperatorKeyAllowed(ctx)==false) reached a driver's key-fallback with no
// own credential override for the requested key, so it must NOT touch the
// operator's host key. Returned by ResolveKeyOrOperator and propagated by each
// driver's Call as a fatal, NON-retryable error (classified permanent in
// errclass.go so tryProviderFallback treats it as terminal rather than
// cascading the same refusal across providers).
//
// This backstop is MANDATORY, not optional hardening: the restriction bit fails
// open (WithOperatorKeyAllowed defaults true when absent), and credential-aware
// routing (Layer 1) skips pinned agents, so only this driver-level refusal
// guarantees a restricted run never spends the operator's key.
var ErrOperatorKeyForbidden = errors.New("operator key forbidden for restricted run (RFC AX)")

// ErrCredentialUnusable is returned when the run HOLDS its own credential for a
// key but that credential may not be used (it has expired). It is not a miss:
// ResolveKeyOrOperator returns it instead of the operator's host key, whatever
// the run's operator-key permission, because a tenant's own key that lapsed
// must never be quietly replaced by the operator's. Classified permanent
// (errclass.go) so the provider cascade does not carry the call to another
// provider on the operator's key either. The resolver wraps the cause, so the
// message names the credential and why.
var ErrCredentialUnusable = errors.New("the run's own credential for this key cannot be used")

// credResolverKey is the ctx key for the per-run credential resolver.
type credResolverKey struct{}

// CredentialResolution is the result of resolving a credential override: the
// value to use plus WHICH scope owned the override, so a caller can attribute
// usage (RFC AV — operator vs tenant/user spend). Scope is "agent" | "user" |
// "tenant"; ScopeID is the owning subject/tenant id ("" for tenant scope).
type CredentialResolution struct {
	Value   string
	Scope   string
	ScopeID string
}

// CredentialResolver resolves a credential by its well-known env-var NAME (e.g.
// "ANTHROPIC_API_KEY", "BRAVE_API_KEY") for the run whose ctx this is, returning
// the resolution + true when the tenant/user has stored one that should OVERRIDE
// the operator's host key. It reads the run identity from ctx internally and
// applies scope precedence (agent > user > tenant). RFC AR: a tenant's own
// provider key is used for that tenant/user's requests; the operator key is the
// fallback. RFC AV: the returned Scope/ScopeID tag the usage record.
//
// A non-nil error means the run holds a credential of that name that must not
// be used, and must wrap ErrCredentialUnusable; it is never a miss. A lookup
// fault the resolver chooses to treat as a miss returns (_, false, nil).
//
// It lives on the leaf providers package so LLM drivers can consult it without
// importing internal/tools (the provider→loop→tools layering boundary); builtin
// tools (WebSearch) may import providers and use it too. The resolver returns a
// NAME→value lookup, never carries a secret in ctx — the value is fetched +
// decrypted on demand.
type CredentialResolver func(ctx context.Context, name string) (CredentialResolution, bool, error)

// WithCredentialResolver stamps r onto ctx. The run-launch sites set it from the
// credential engine; it flows via ctx to every provider Call and tool Execute.
func WithCredentialResolver(ctx context.Context, r CredentialResolver) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, credResolverKey{}, r)
}

// ResolveCredentialFull resolves the credential named name for the run's ctx,
// returning the full resolution (value + owning scope) + true on a hit. Safe
// when no resolver is stamped (returns zero, false → callers fall back to the
// operator's host key). A hit with an empty value is treated as a miss — an
// empty override must never blank the host key. The resolver's error (an
// override that exists but may not be used) is returned as is, never as a miss.
func ResolveCredentialFull(ctx context.Context, name string) (CredentialResolution, bool, error) {
	r, ok := ctx.Value(credResolverKey{}).(CredentialResolver)
	if !ok || r == nil {
		return CredentialResolution{}, false, nil
	}
	res, found, err := r(ctx, name)
	if err != nil {
		return CredentialResolution{}, false, err
	}
	if !found || res.Value == "" {
		return CredentialResolution{}, false, nil
	}
	return res, true, nil
}

// ResolveCredential is the value-only convenience over ResolveCredentialFull:
// the single call site a driver / tool uses when it needs only the key to send
// (the override value), not the owning scope.
func ResolveCredential(ctx context.Context, name string) (string, bool, error) {
	res, ok, err := ResolveCredentialFull(ctx, name)
	if err != nil || !ok {
		return "", false, err
	}
	return res.Value, true, nil
}

// operatorKeyAllowedKey is the ctx key for the RFC AX operator-key permission,
// mirrored here (drivers import providers, not auth/tools) as a ctx value so a
// driver's key-fallback path can consult it without an import cycle.
type operatorKeyAllowedKey struct{}

// WithOperatorKeyAllowed stamps whether the run on this ctx may fall back to the
// operator's host provider key (RFC AX). The run-launch sites set it from the
// negative permission bit (allowed = !OperatorKeyRestricted) right beside
// WithCredentialResolver, so it flows to every provider Call + tool Execute.
// Stage 1 only threads it; the driver backstop that reads it lands in stage 2.
func WithOperatorKeyAllowed(ctx context.Context, allowed bool) context.Context {
	return context.WithValue(ctx, operatorKeyAllowedKey{}, allowed)
}

// OperatorKeyAllowed reports whether the run may use the operator's host key. It
// DEFAULTS TO TRUE when absent (fail-open) — the same backward-safety posture as
// the negative bit itself: an un-stamped path (open mode, legacy, a missed
// stamp) keeps operator-key access.
func OperatorKeyAllowed(ctx context.Context) bool {
	allowed, ok := ctx.Value(operatorKeyAllowedKey{}).(bool)
	if !ok {
		return true
	}
	return allowed
}

// ResolveKeyOrOperator is the single choke point every LLM driver + WebSearch
// uses to pick the API key for a call under RFC AR (tenant override) + RFC AX
// (operator-key restriction). name is the well-known env-var name of the key
// (e.g. "ANTHROPIC_API_KEY"); operatorKey is the driver's host key. Precedence:
//
//   - a tenant/user override for name exists  → use it (source = its scope);
//   - an override exists but may not be used (expired) → its error, wrapping
//     ErrCredentialUnusable — never the operator key, restricted or not;
//   - no override + the run may use the operator key → the operator's host key
//     (source "operator");
//   - no override + the run is RESTRICTED → ErrOperatorKeyForbidden (never the
//     operator key).
//
// The returned source/scopeID ride the per-call Usage so the server can
// attribute spend (RFC AV). It reads the run identity + the operator-key
// permission from ctx, so it works uniformly across every transport.
func ResolveKeyOrOperator(ctx context.Context, name, operatorKey string) (key, source, scopeID string, err error) {
	r, ok, err := ResolveCredentialFull(ctx, name)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return r.Value, r.Scope, r.ScopeID, nil
	}
	if OperatorKeyAllowed(ctx) {
		return operatorKey, "operator", "", nil
	}
	return "", "", "", ErrOperatorKeyForbidden
}
