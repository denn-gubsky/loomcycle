package headergate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CloudflareHeader carries the Access application token to the origin.
// Cloudflare's own guidance is to verify this header, not the
// CF_Authorization cookie, which an origin is not guaranteed to receive.
const CloudflareHeader = "Cf-Access-Jwt-Assertion"

// SignatureVerifier checks a compact JWS against an issuer's published keys
// and returns its payload. It owns the key handling — the JWKS fetch, the kid
// cache, a refresh on an unknown kid at a bounded rate — and the algorithm
// allowlist; the caller owns the claims. One implementation serves every gate
// that verifies a JWKS-signed token (the OIDC gate builds it).
type SignatureVerifier interface {
	VerifySignature(ctx context.Context, compact string) (payload []byte, err error)
}

// CloudflareAccess verifies Cloudflare Access application tokens.
type CloudflareAccess struct {
	// TeamDomain is the Access team domain, e.g. "acme.cloudflareaccess.com"
	// (a custom team domain works too). The token's iss must be
	// "https://"+TeamDomain.
	TeamDomain string
	// Audience is the Access application's AUD tag; the token's aud must
	// contain it, so a token for another application of the same team is
	// refused.
	Audience string
	// Keys verifies the signature under the team's JWKS (CertsURL), RS256.
	Keys SignatureVerifier
	Now  func() time.Time
}

var teamDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ValidTeamDomain reports whether d is a bare host name (no scheme, port or
// path): the certs URL and the expected issuer are both built from it.
func ValidTeamDomain(d string) bool { return teamDomainRE.MatchString(d) }

// CertsURL is the team's JWKS.
func CertsURL(teamDomain string) string {
	return "https://" + teamDomain + "/cdn-cgi/access/certs"
}

// Issuer is the iss a team's tokens carry.
func (c *CloudflareAccess) Issuer() string { return "https://" + c.TeamDomain }

// Verify checks one Cf-Access-Jwt-Assertion value.
func (c *CloudflareAccess) Verify(ctx context.Context, assertion string) (Identity, error) {
	assertion = strings.TrimSpace(assertion)
	if assertion == "" || len(assertion) > maxAssertionBytes {
		return Identity{}, invalid("cloudflare access: empty or oversized assertion")
	}
	if c.Keys == nil || c.Audience == "" || !ValidTeamDomain(c.TeamDomain) {
		return Identity{}, fmt.Errorf("cloudflare access: gate is not configured")
	}
	payload, err := c.Keys.VerifySignature(ctx, assertion)
	if err != nil {
		return Identity{}, invalid("cloudflare access: %v", err)
	}
	claims, err := decodeClaims(payload)
	if err != nil {
		return Identity{}, err
	}
	// The verifier may or may not have checked these; they are this gate's
	// binding to one team and one application, so it checks them itself.
	if stringClaim(claims, "iss") != c.Issuer() {
		return Identity{}, invalid("cloudflare access: wrong issuer")
	}
	if !audContains(claims["aud"], c.Audience) {
		return Identity{}, invalid("cloudflare access: wrong audience")
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	exp, err := checkTimes(claims, now)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{
		Issuer:  c.Issuer(),
		Subject: stringClaim(claims, "sub"),
		Email:   stringClaim(claims, "email"),
		// Access issues a token only after its identity provider (or its
		// one-time PIN to that address) authenticated the person.
		EmailVerified: stringClaim(claims, "email") != "",
		Name:          stringClaim(claims, "name"),
		Groups:        stringsClaim(claims, "groups"),
		ExpiresAt:     exp,
		AMR:           []string{"cf-access"},
		Claims:        claims,
	}
	// A service token (machine access) has no user: its sub is empty and its
	// common_name is the token's client id. It gets a subject of its own kind
	// so it can never collide with a person's sub.
	if id.Subject == "" {
		cn := stringClaim(claims, "common_name")
		if cn == "" {
			return Identity{}, invalid("cloudflare access: token names no subject")
		}
		id.Subject, id.Email, id.EmailVerified = "service:"+cn, "", false
		id.AMR = []string{"cf-access-service-token"}
	}
	return id, nil
}
