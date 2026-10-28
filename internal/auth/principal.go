package auth

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC L OSS multi-tenant authorization — the authoritative principal.
//
// The auth middleware resolves the inbound bearer to a Principal FROM
// THE TOKEN and stamps it into ctx. Downstream, the run-creation sites
// copy Principal.Subject → the run's user_id and Principal.TenantID →
// the run's tenant, so the keys that isolation already uses (fairness,
// memory tenancy, attribution, audit) become authority-derived rather
// than caller-asserted. See rfcs/oss-multi-tenant-authorization.md.

// Principal is the authenticated identity behind a bearer token.
type Principal struct {
	// TenantID is the authoritative data-isolation boundary. Always
	// non-empty for a resolved principal ("default" for the legacy
	// LOOMCYCLE_AUTH_TOKEN fallback).
	TenantID string
	// Subject is the authoritative per-actor id — the run's user_id and
	// the fairness key. Distinct subjects under one tenant get distinct
	// fairness caps + attribution while sharing the tenant's data.
	Subject string
	// Scopes is the granted capability set (closed catalog). substrate:admin
	// is a superuser scope (see HasScope).
	Scopes []string
	// TokenDefID is the operator_token_defs row id (empty for legacy).
	TokenDefID string
	// TokenSuffix is the 6-char grep handle for log correlation (never
	// the secret; empty for legacy).
	TokenSuffix string
	// Legacy is true when this principal came from the LOOMCYCLE_AUTH_TOKEN
	// shared-secret fallback rather than an OperatorTokenDef row.
	Legacy bool
	// Restrictions narrows the principal below its scopes. Zero for a token
	// minted without any, a legacy principal, and open mode.
	Restrictions Restrictions
	// Groups is the principal's transitively closed group membership in its
	// tenant (RFC EY), resolved server-side after the bearer; nil when no
	// ACL resolver is wired.
	Groups []string
	// Effective is the principal's compiled ACL set (RFC EY), resolved after
	// the bearer and cached per (tenant, subject, acl_version). nil when no
	// resolver is wired or the store could not be read: the gates then fall
	// back to the scope answer in compat and shadow mode and refuse in
	// enforce mode.
	Effective *authz.Effective
	// IssuedByGate names the auth gate that issued the token this principal
	// came from (RFC FA §7); "" for every other credential. Such a token
	// stands only while its user is active: the server re-reads the user's
	// status on every request it authenticates, so a disable or a gate's
	// revoke holds at once on every replica, whatever the sweeper did. The
	// tenant's gate policy (RFC FA P5) is checked against it the same way.
	IssuedByGate string
	// OAuthClient and OAuthFamily name the app sign-in (RFC FL) the token
	// came from: the OAuth client and the refresh-token family. "" for every
	// other credential. Such a token stands only while the client does.
	OAuthClient string
	OAuthFamily string
	// InheritsAuthority is set for a token minted with authority "inherit"
	// (RFC EY §8): Scopes is then not the token's stored set but the implied
	// set of its user's current access_mode, re-derived on every request it
	// authenticates. Restrictions still narrow it.
	InheritsAuthority bool
	// Login is how the person signed in, for a principal a Web UI session
	// authenticated (RFC FA §5); nil for a bearer. It is provenance for the
	// person's own agents to report, never consulted by a gate.
	Login *Login
}

// AuthorityMode names how the principal's scopes were decided, for reporting
// (/v1/_me, Context op=self): AuthorityInherit or AuthorityFrozen. Not secret.
func (p Principal) AuthorityMode() string {
	if p.InheritsAuthority {
		return AuthorityInherit
	}
	return AuthorityFrozen
}

// Login is the sign-in provenance of a Web UI session: the configured gate
// the person came through, the authentication methods that gate reported
// (RFC 8176 style) and when they last authenticated. It carries no session
// id, hash or credential reference.
type Login struct {
	Gate     string
	AMR      []string
	AuthTime time.Time
}

type ctxKeyPrincipal struct{}

// WithPrincipal stamps the resolved principal into ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal{}, p)
}

// PrincipalFromContext returns the stamped principal and whether one was
// present. Absent in open mode (no auth configured) and on un-authed
// internal paths — callers fall back to wire/explicit values.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal{}).(Principal)
	return p, ok
}

// ResolveWireIdentity makes a principal authoritative over caller-asserted wire
// tenant_id/user_id (RFC L Decision 5) and is the SINGLE source of truth for the
// override rule shared by every run-triggering surface (HTTP applyPrincipal, the
// MCP run-lifecycle handlers, …). It is pure / side-effect-free: callers that
// want triage logging on a disagreement compare the inputs to the outputs
// themselves.
//
// Rules:
//   - No principal (open / un-authed): wire values pass through unchanged.
//   - Legacy (LOOMCYCLE_AUTH_TOKEN, F18): single-operator, no-boundary mode —
//     HONOR the wire user_id (the placeholder subject only when the caller omits
//     it), but keep the legacy tenant (tenant routing is a real isolation axis
//     the wire must not steer).
//   - Real OperatorTokenDef principal: STRICT override to (p.TenantID,
//     p.Subject) — a caller must not be able to spoof another subject/tenant.
func ResolveWireIdentity(p Principal, ok bool, wireTenant, wireUser string) (tenant, subject string) {
	if !ok {
		return wireTenant, wireUser
	}
	if p.Legacy {
		subject = p.Subject
		if wireUser != "" {
			subject = wireUser
		}
		return p.TenantID, subject
	}
	return p.TenantID, p.Subject
}

// SubjectForFairness returns the authoritative fairness key: the
// principal's Subject when one is stamped, else the supplied fallback
// (the wire user_id, used in open mode / un-authed internal paths). This
// is what makes the per-subject fairness cap a real boundary — a caller
// can no longer forge a different user_id to dodge their cap, because on
// authed routes the Subject comes from the token, not the request body.
func SubjectForFairness(ctx context.Context, fallback string) string {
	if p, ok := PrincipalFromContext(ctx); ok && p.Subject != "" {
		return p.Subject
	}
	return fallback
}
