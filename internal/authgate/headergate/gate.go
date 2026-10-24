package headergate

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

// Kind is the HeaderGate's kind in `auth.gates.<name>.kind`.
const Kind = "header"

// The presets.
const (
	PresetCloudflareAccess = "cloudflare-access"
	PresetAWSALB           = "aws-alb"
	PresetForwardAuthHMAC  = "forward-auth-hmac"
)

// Gate is a HeaderGate: one preset's verifier behind the request filter. It
// is not interactive — the edge already authenticated the person, and every
// request it forwards carries the assertion — so Begin has nothing to start
// and Complete is Verify.
type Gate struct {
	preset string
	source Source
	verify func(ctx context.Context, h map[string]string) (Identity, error)
	now    func() time.Time
}

// NewCloudflareGate fronts a Cloudflare Access verifier.
func NewCloudflareGate(cf *CloudflareAccess, trusted []netip.Prefix) *Gate {
	return &Gate{
		preset: PresetCloudflareAccess,
		source: Source{Headers: []string{CloudflareHeader}, Trusted: trusted},
		verify: func(ctx context.Context, h map[string]string) (Identity, error) {
			return cf.Verify(ctx, h[CloudflareHeader])
		},
	}
}

// NewALBGate fronts an ALB verifier.
func NewALBGate(a *ALB, trusted []netip.Prefix) *Gate {
	return &Gate{
		preset: PresetAWSALB,
		source: Source{Headers: []string{ALBHeader}, Trusted: trusted},
		verify: func(ctx context.Context, h map[string]string) (Identity, error) {
			return a.Verify(ctx, h[ALBHeader])
		},
	}
}

// NewForwardAuthGate fronts a forward-auth HMAC verifier.
func NewForwardAuthGate(f *ForwardAuthHMAC, trusted []netip.Prefix) *Gate {
	return &Gate{
		preset: PresetForwardAuthHMAC,
		source: Source{Headers: []string{IdentityHeader, IdentitySignatureHeader}, Trusted: trusted},
		verify: func(ctx context.Context, h map[string]string) (Identity, error) {
			return f.Verify(ctx, h[IdentityHeader], h[IdentitySignatureHeader])
		},
	}
}

// Kind implements authgate.Gate.
func (g *Gate) Kind() string { return Kind }

// Preset names the gate's preset.
func (g *Gate) Preset() string { return g.preset }

// Headers are the request headers the gate reads (for the server's filter,
// which strips their aliases from every request).
func (g *Gate) Headers() []string { return append([]string(nil), g.source.Headers...) }

// Begin implements authgate.Gate: there is no login to start.
func (g *Gate) Begin(context.Context, http.ResponseWriter, *http.Request, authgate.LoginState) error {
	return authgate.ErrNotInteractive
}

// Complete implements authgate.Gate: the login request carries the assertion.
func (g *Gate) Complete(ctx context.Context, r *http.Request, _ authgate.LoginState) (authgate.Assertion, error) {
	return g.Verify(ctx, r)
}

// Verify implements authgate.Gate. A forward-auth header is single-use (its
// nonce is recorded), so a request must be verified once and the result
// shared, not verified again by each reader.
func (g *Gate) Verify(ctx context.Context, r *http.Request) (authgate.Assertion, error) {
	h, err := g.source.Take(r)
	if err != nil {
		return authgate.Assertion{}, err
	}
	id, err := g.verify(ctx, h)
	if err != nil {
		return authgate.Assertion{}, err
	}
	now := time.Now()
	if g.now != nil {
		now = g.now()
	}
	authTime := now
	if iat, ok := numericDate(id.Claims["iat"]); ok && !iat.After(now) {
		authTime = iat
	}
	return authgate.Assertion{
		Subject:       id.Subject,
		TenantHint:    id.Tenant,
		AMR:           id.AMR,
		AuthTime:      authTime,
		Issuer:        id.Issuer,
		Email:         id.Email,
		EmailVerified: id.EmailVerified,
		DisplayName:   id.Name,
		// The forward-auth identity names its groups as a field of its own;
		// the JWT presets carry them in Claims, for the identity map's
		// groups.claim to read.
		Groups:    forwardAuthGroups(g.preset, id),
		Claims:    id.Claims,
		ExpiresAt: id.ExpiresAt,
	}, nil
}

func forwardAuthGroups(preset string, id Identity) []string {
	if preset == PresetForwardAuthHMAC {
		return id.Groups
	}
	return nil
}
