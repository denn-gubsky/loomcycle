package headergate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/denn-gubsky/loomcycle/internal/authgate/jwtverify"
)

// CloudflareHeader carries the Access application token to the origin.
// Cloudflare's own guidance is to verify this header, not the
// CF_Authorization cookie, which an origin is not guaranteed to receive.
const CloudflareHeader = "Cf-Access-Jwt-Assertion"

// CloudflareAccess verifies Cloudflare Access application tokens through the
// shared JWT verifier: RS256 only, keys by kid from the team's certs endpoint
// (rotated every six weeks, the old key valid seven more days — a rotation
// verifies after one refresh, and made-up kids cannot become a request
// storm), iss = the team, aud = this application.
type CloudflareAccess struct {
	TeamDomain string
	Audience   string
	verifier   *jwtverify.Verifier
}

// NewCloudflareAccess builds the verifier for one Access application.
// teamDomain is e.g. "acme.cloudflareaccess.com" (a custom team domain works
// too); audience is the application's AUD tag, so a token for another
// application of the same team is refused. keys is nil for the team's live
// JWKS (CertsURL), fetched with client (nil: a 5-second-timeout client).
func NewCloudflareAccess(teamDomain, audience string, keys jwtverify.KeySource, client *http.Client, now func() time.Time) (*CloudflareAccess, error) {
	if !ValidTeamDomain(teamDomain) || strings.TrimSpace(audience) == "" {
		return nil, fmt.Errorf("cloudflare access: team_domain must be a host name and audience is required")
	}
	if keys == nil {
		if client == nil {
			client = &http.Client{
				Timeout:       5 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}
		}
		keys = &jwtverify.RemoteJWKS{URL: CertsURL(teamDomain), Client: client, Now: now}
	}
	return &CloudflareAccess{
		TeamDomain: teamDomain,
		Audience:   audience,
		verifier: &jwtverify.Verifier{
			Keys:       keys,
			Algorithms: []jose.SignatureAlgorithm{jose.RS256},
			Issuer:     "https://" + teamDomain,
			Audience:   audience,
			Now:        now,
		},
	}, nil
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
	claims, err := c.verifier.Verify(ctx, assertion)
	if errors.Is(err, jwtverify.ErrKeySetUnavailable) {
		return Identity{}, fmt.Errorf("cloudflare access: %w", err)
	}
	if err != nil {
		return Identity{}, invalid("cloudflare access: %v", err)
	}
	exp, _, _ := claims.Time("exp")
	email, _ := claims.String("email")
	sub, _ := claims.String("sub")
	name, _ := claims.String("name")
	id := Identity{
		Issuer:  c.Issuer(),
		Subject: strings.TrimSpace(sub),
		Email:   strings.TrimSpace(email),
		// Access issues a token only after its identity provider (or its
		// one-time PIN to that address) authenticated the person.
		EmailVerified: strings.TrimSpace(email) != "",
		Name:          strings.TrimSpace(name),
		Groups:        claims.Strings("groups"),
		ExpiresAt:     exp,
		AMR:           []string{"cf-access"},
		Claims:        claims,
	}
	// A service token (machine access) has no user: its sub is empty and its
	// common_name is the token's client id. It gets a subject of its own kind
	// so it can never collide with a person's sub.
	if id.Subject == "" {
		cn, _ := claims.String("common_name")
		if strings.TrimSpace(cn) == "" {
			return Identity{}, invalid("cloudflare access: token names no subject")
		}
		id.Subject, id.Email, id.EmailVerified = "service:"+strings.TrimSpace(cn), "", false
		id.AMR = []string{"cf-access-service-token"}
	}
	return id, nil
}
