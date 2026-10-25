// Package authgate holds the gates of RFC FA: components that verify an inbound
// credential and produce an identity assertion, and nothing more. A gate never
// decides authorization, never creates a session and never mints a token —
// turning an assertion into a session is the HTTP server's, once, for every
// gate, so a new gate is a new Gate implementation and no session code.
//
// P0 has one gate, BaseAuthGate, which accepts a loomcycle bearer. The later
// kinds (password, OIDC, OAuth, trusted-proxy header, AWS IAM, device login,
// external) implement the same interface.
package authgate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Gate verifies an inbound credential (RFC FA §3.4).
type Gate interface {
	// Kind is the gate's kind as written in `auth.gates.<name>.kind`.
	Kind() string
	// Begin starts an interactive login: a redirect to the provider, or a
	// rendered form. ErrNotInteractive for a gate with nothing to start.
	Begin(ctx context.Context, w http.ResponseWriter, r *http.Request, state LoginState) error
	// Complete finishes an interactive login (a callback, a form post) and
	// returns the assertion.
	Complete(ctx context.Context, r *http.Request, state LoginState) (Assertion, error)
	// Verify authenticates a non-interactive request. ErrNoCredential when the
	// request carries nothing this gate verifies.
	Verify(ctx context.Context, r *http.Request) (Assertion, error)
}

// LoginState is the per-login state an interactive gate carries from Begin to
// Complete. The token and password gates need none. A redirect gate (OIDC,
// OAuth, external) gets it from the server's login-state machinery
// (codeflow), which derives the verifier and nonce from a secret only the
// browser's login cookie holds, so a database read cannot finish a login.
type LoginState struct {
	// State is the OAuth `state` sent to the provider; Complete refuses a
	// callback whose state is not this one.
	State string
	// Nonce is the OIDC nonce the ID token must echo.
	Nonce string
	// CodeVerifier is the PKCE (S256) verifier.
	CodeVerifier string
	// RedirectURI is this gate's exact callback URL.
	RedirectURI string
	// Reauth asks the provider for a fresh authentication (prompt=login,
	// max_age=0): an account link must not ride an old provider session.
	Reauth bool
	// StartedAt is when the login began; with Reauth the provider's
	// auth_time must not be earlier.
	StartedAt time.Time
}

// Assertion is what a gate says about the person it verified (RFC FA §3.1).
type Assertion struct {
	// Subject is the identity's stable key at the gate (never a display name):
	// for an identity gate the provider's `sub`, which the identity map turns
	// into a loomcycle subject.
	Subject string
	// TenantHint is the tenant the gate saw, for the identity map to judge.
	TenantHint string
	// AMR lists the authentication methods used (RFC 8176 style).
	AMR []string
	// AuthTime is when the person authenticated.
	AuthTime time.Time
	// Issuer is who vouches for Subject (an OIDC `iss`); with the gate and
	// Subject it keys the person's user_identities row.
	Issuer string
	// Email, EmailVerified and DisplayName are attributes, never a key.
	Email         string
	EmailVerified bool
	DisplayName   string
	// Groups is the group list a gate knows natively (an external gate, a
	// forge's org teams). An OIDC gate leaves it empty: the identity map
	// reads groups from Claims.
	Groups []string
	// AccessMode is an access_mode the gate asserts; honoured only for a
	// trusted gate (D12).
	AccessMode string
	// Claims are the verified claims, for the identity map's tenant,
	// subject and groups claims.
	Claims map[string]any
	// ExpiresAt is when the assertion itself stops being valid.
	ExpiresAt time.Time
	// Credential is set only by a gate that verified a loomcycle credential.
	// The identity is then already authoritative (the bearer's own tenant and
	// subject), and a session made from it must stay bound to that credential:
	// it ends when the credential stops authenticating, and never carries more
	// than the credential did.
	Credential *Credential
}

// Credential is a verified loomcycle bearer and the principal it resolved to.
// Bearer is held only for the duration of the login; the session layer derives
// a binding from it and does not store it.
type Credential struct {
	Principal auth.Principal
	Bearer    string
}

var (
	// ErrNoCredential: the request carries nothing this gate verifies.
	ErrNoCredential = errors.New("authgate: no credential presented")
	// ErrInvalidCredential: the credential was presented and refused. Callers
	// answer it with one opaque message, whatever the reason.
	ErrInvalidCredential = errors.New("authgate: invalid credentials")
	// ErrNotInteractive: the gate has no interactive login to begin.
	ErrNotInteractive = errors.New("authgate: gate has no interactive login")
)

// Resolver turns a loomcycle bearer into its principal; false for a bearer
// that does not authenticate. The HTTP server's own resolution is passed in,
// so this gate and the API's bearer check cannot disagree.
type Resolver func(ctx context.Context, bearer string) (auth.Principal, bool)

// BaseAuthGate accepts a loomcycle bearer — minted, config-declared or the
// legacy shared secret — in an `Authorization: Bearer` header: the Web UI's
// paste box and the postMessage handoff both send it that way.
type BaseAuthGate struct {
	resolve Resolver
	now     func() time.Time
}

// NewBaseAuthGate builds the token gate over the server's bearer resolution.
func NewBaseAuthGate(resolve Resolver) *BaseAuthGate {
	return &BaseAuthGate{resolve: resolve, now: time.Now}
}

// KindToken is BaseAuthGate's kind.
const KindToken = "token"

// AMRBearer is the method a BaseAuthGate login records: possession of a
// bearer. RFC 8176 has no value for it.
const AMRBearer = "bearer"

func (g *BaseAuthGate) Kind() string { return KindToken }

// Begin: the paste box is the Web UI's own page; there is nothing to start.
func (g *BaseAuthGate) Begin(context.Context, http.ResponseWriter, *http.Request, LoginState) error {
	return ErrNotInteractive
}

// Complete is the paste box's post: the bearer rides the Authorization header,
// as on any API call.
func (g *BaseAuthGate) Complete(ctx context.Context, r *http.Request, _ LoginState) (Assertion, error) {
	return g.Verify(ctx, r)
}

// Verify resolves the request's bearer.
func (g *BaseAuthGate) Verify(ctx context.Context, r *http.Request) (Assertion, error) {
	bearer := bearerFromHeader(r.Header.Get("Authorization"))
	if bearer == "" {
		return Assertion{}, ErrNoCredential
	}
	p, ok := g.resolve(ctx, bearer)
	if !ok {
		return Assertion{}, ErrInvalidCredential
	}
	return Assertion{
		Subject:    p.Subject,
		TenantHint: p.TenantID,
		AMR:        []string{AMRBearer},
		AuthTime:   g.now(),
		Credential: &Credential{Principal: p, Bearer: bearer},
	}, nil
}

// bearerFromHeader extracts the token from an "Authorization: Bearer <token>"
// value (the scheme is case-insensitive, RFC 7235); "" for anything else.
func bearerFromHeader(h string) string {
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}
