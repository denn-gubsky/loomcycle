package providers

import "context"

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
// It lives on the leaf providers package so LLM drivers can consult it without
// importing internal/tools (the provider→loop→tools layering boundary); builtin
// tools (WebSearch) may import providers and use it too. The resolver returns a
// NAME→value lookup, never carries a secret in ctx — the value is fetched +
// decrypted on demand.
type CredentialResolver func(ctx context.Context, name string) (CredentialResolution, bool)

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
// empty override must never blank the host key.
func ResolveCredentialFull(ctx context.Context, name string) (CredentialResolution, bool) {
	r, ok := ctx.Value(credResolverKey{}).(CredentialResolver)
	if !ok || r == nil {
		return CredentialResolution{}, false
	}
	res, found := r(ctx, name)
	if !found || res.Value == "" {
		return CredentialResolution{}, false
	}
	return res, true
}

// ResolveCredential is the value-only convenience over ResolveCredentialFull:
// the single call site a driver / tool uses when it needs only the key to send
// (the override value), not the owning scope.
func ResolveCredential(ctx context.Context, name string) (string, bool) {
	res, ok := ResolveCredentialFull(ctx, name)
	if !ok {
		return "", false
	}
	return res.Value, true
}
