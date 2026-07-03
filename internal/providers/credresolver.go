package providers

import "context"

// credResolverKey is the ctx key for the per-run credential resolver.
type credResolverKey struct{}

// CredentialResolver resolves a credential by its well-known env-var NAME (e.g.
// "ANTHROPIC_API_KEY", "BRAVE_API_KEY") for the run whose ctx this is, returning
// (value, true) when the tenant/user has stored one that should OVERRIDE the
// operator's host key. It reads the run identity from ctx internally and applies
// scope precedence (agent > user > tenant). RFC AR: a tenant's own provider key
// is used for that tenant/user's requests; the operator key is the fallback.
//
// It lives on the leaf providers package so LLM drivers can consult it without
// importing internal/tools (the provider→loop→tools layering boundary); builtin
// tools (WebSearch) may import providers and use it too. The resolver returns a
// NAME→value lookup, never carries a secret in ctx — the value is fetched +
// decrypted on demand.
type CredentialResolver func(ctx context.Context, name string) (string, bool)

// WithCredentialResolver stamps r onto ctx. The run-launch sites set it from the
// credential engine; it flows via ctx to every provider Call and tool Execute.
func WithCredentialResolver(ctx context.Context, r CredentialResolver) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, credResolverKey{}, r)
}

// ResolveCredential resolves the credential named name for the run's ctx,
// returning (value, true) on a hit. Safe when no resolver is stamped (returns
// "", false → callers fall back to the operator's host key). This is the single
// call site a driver / tool uses to honor a tenant-supplied key override.
func ResolveCredential(ctx context.Context, name string) (string, bool) {
	r, ok := ctx.Value(credResolverKey{}).(CredentialResolver)
	if !ok || r == nil {
		return "", false
	}
	v, found := r(ctx, name)
	if !found || v == "" {
		return "", false
	}
	return v, true
}
