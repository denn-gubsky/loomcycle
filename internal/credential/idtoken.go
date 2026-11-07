package credential

// A display name from an OpenID Connect id_token (RFC GB follow-up): some
// authorization servers advertise no userinfo endpoint but add an id_token
// to the token response. The token names the account only once verified —
// signed by a key from the issuer's own jwks_uri (discovered, on the
// issuer's or the resource's host, and sealed with the other endpoints), for
// this client, by this issuer, unexpired. Anything less and it is ignored:
// connected_as is display metadata, so an id_token that does not verify is
// no error, just no name. The token itself is never stored, logged,
// returned or snapshotted.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/jwtverify"
)

const (
	// idTokenSkew is the clock skew tolerated on exp and iat: an id_token
	// fresh from the token endpoint has no reason to need the sign-in gates'
	// five minutes.
	idTokenSkew = 60 * time.Second
	// jwksMaxAge is how long a fetched key set is used. Short: it is held
	// only to name an account at connect and refresh, and a short life
	// follows a rotation without leaning on the unknown-kid re-fetch.
	jwksMaxAge = 5 * time.Minute
	// jwksMinRefresh is the least time between two fetches of one key set,
	// so an unknown kid re-fetches once, not on every token.
	jwksMinRefresh = time.Minute
	// maxJWKSEntries bounds the key-set cache; past it the least recently
	// used entry goes.
	maxJWKSEntries = 64
)

// jwksEntry is one issuer's cached key set.
type jwksEntry struct {
	keys *jwtverify.RemoteJWKS
	used time.Time
}

func (c *oauthClient) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// keySet returns the cached key set of issuer at jwksURL. It is keyed by
// both, so a credential's discovered metadata can never answer for another
// issuer's keys.
func (c *oauthClient) keySet(issuer, jwksURL string) *jwtverify.RemoteJWKS {
	c.jwksMu.Lock()
	defer c.jwksMu.Unlock()
	if c.jwks == nil {
		c.jwks = map[string]*jwksEntry{}
	}
	key := issuer + "\n" + jwksURL
	now := c.clock()
	if e := c.jwks[key]; e != nil {
		e.used = now
		return e.keys
	}
	if len(c.jwks) >= maxJWKSEntries {
		var oldest string
		for k, e := range c.jwks {
			if oldest == "" || e.used.Before(c.jwks[oldest].used) {
				oldest = k
			}
		}
		delete(c.jwks, oldest)
	}
	// The fetch goes through the OAuth client: guarded, no redirect; the
	// key set reader caps the body.
	e := &jwksEntry{used: now, keys: &jwtverify.RemoteJWKS{URL: jwksURL, Client: c.http,
		MaxAge: jwksMaxAge, MinRefresh: jwksMinRefresh, Now: c.clock}}
	c.jwks[key] = e
	return e.keys
}

// idTokenName returns a display name from idToken when it verifies against
// sec's sealed issuer, key set and client_id: email (unless email_verified
// is explicitly false), else preferred_username, else sub. "" otherwise;
// why is logged at Debug, never the token.
func (c *oauthClient) idTokenName(ctx context.Context, sec oauthSecret, idToken string) string {
	if idToken == "" {
		return ""
	}
	name, err := c.verifyIDToken(ctx, sec, idToken)
	if err != nil {
		slog.DebugContext(ctx, "oauth: id_token ignored; connected_as not taken from it", "issuer", sec.Endpoints.Issuer, "reason", err.Error())
		return ""
	}
	return name
}

func (c *oauthClient) verifyIDToken(ctx context.Context, sec oauthSecret, idToken string) (string, error) {
	if sec.Endpoints.JWKS == "" {
		return "", errors.New("the authorization server advertised no usable jwks_uri")
	}
	if sec.Endpoints.Issuer == "" || sec.ClientID == "" {
		return "", errors.New("no issuer or client_id to check it against")
	}
	v := &jwtverify.Verifier{
		Keys:       c.keySet(sec.Endpoints.Issuer, sec.Endpoints.JWKS),
		Algorithms: jwtverify.AsymmetricAlgorithms,
		Issuer:     sec.Endpoints.Issuer,
		Audience:   sec.ClientID,
		Skew:       idTokenSkew,
		Now:        c.clock,
	}
	claims, err := v.Verify(ctx, idToken)
	if err != nil {
		return "", err
	}
	// OIDC Core §3.1.3.7: an azp names the party the token was issued to;
	// one naming another client means this token was not issued to us,
	// whatever its aud lists.
	if azp, ok := claims.String("azp"); ok && !jwtverify.ConstantEqual(azp, sec.ClientID) {
		return "", errors.New("azp names another client")
	}
	var candidates []string
	if email, _ := claims.String("email"); email != "" && !emailUnverified(claims) {
		candidates = append(candidates, email)
	}
	for _, k := range []string{"preferred_username", "sub"} {
		if s, _ := claims.String(k); s != "" {
			candidates = append(candidates, s)
		}
	}
	for _, s := range candidates {
		if s = cleanConnectedAs(s); s != "" {
			return s, nil
		}
	}
	return "", errors.New("it names no email, preferred_username or sub")
}

// emailUnverified reports an email_verified claim that is explicitly false
// (some providers send it as the string "false"); absent is not.
func emailUnverified(c jwtverify.Claims) bool {
	switch v := c["email_verified"].(type) {
	case bool:
		return !v
	case string:
		return strings.EqualFold(v, "false")
	}
	return false
}
