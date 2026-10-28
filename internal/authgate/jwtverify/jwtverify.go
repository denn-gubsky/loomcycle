// Package jwtverify verifies signed JWTs for the auth gates (RFC FA): an OIDC
// ID token (P1), a trusted proxy's assertion (P3: Cloudflare Access, an ALB)
// and an external gate's assertion (P4). One verifier, so the checks that make
// a JWT trustworthy — an algorithm allowlist that can never include "none" or
// a shared-secret HMAC, a key chosen by kid from the issuer's own key set,
// issuer, audience and time checks with a bounded skew — are written once.
//
// Why not github.com/coreos/go-oidc: its remote key set refreshes on every
// unknown kid with no bound, so a stream of tokens naming made-up kids turns
// into a stream of requests to the provider; and the proxy assertions of P3
// (an ALB serves one PEM per kid, not a JWKS) need a key source go-oidc does
// not model. go-jose (which go-oidc is built on) does the JWS work here.
package jwtverify

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// DefaultSkew is the clock skew tolerated on exp, nbf and iat (RFC FA §8).
const DefaultSkew = 5 * time.Minute

// MaxTokenBytes bounds a token before it is parsed.
const MaxTokenBytes = 16 << 10

// AsymmetricAlgorithms are the signature algorithms a gate may allow. A
// shared-secret HMAC (HS*) is never one: a verifier keyed by a provider's
// PUBLIC key that also accepted HS256 would let anyone holding that public
// key forge a token (the classic RS256→HS256 confusion), and "none" is not a
// signature at all.
var AsymmetricAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// ErrInvalid is wrapped by every refusal. The detail is for logs; callers
// answer one opaque "invalid credentials" whatever it says.
var ErrInvalid = errors.New("jwt: invalid token")

// ErrUnknownKey: no key of the issuer's set matches the token's kid.
var ErrUnknownKey = errors.New("jwt: no key for the token's key id")

// ErrKeySetUnavailable: the key set could not be fetched, so the token was
// not judged at all. It is not wrapped in ErrInvalid: a caller that ends a
// session on an invalid token must not end it because the issuer's key
// endpoint is down.
var ErrKeySetUnavailable = errors.New("jwt: key set unavailable")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// KeySource returns the public key a token's kid names. kid may be "" for a
// token that names none; a source returns ErrUnknownKey when it cannot pick
// one key for it.
type KeySource interface {
	Key(ctx context.Context, kid string, alg jose.SignatureAlgorithm) (jose.JSONWebKey, error)
}

// Verifier checks a JWT's signature and its registered claims.
type Verifier struct {
	Keys KeySource
	// Algorithms are the accepted algorithms, a subset of
	// AsymmetricAlgorithms; empty means RS256 only.
	Algorithms []jose.SignatureAlgorithm
	// Issuer is the exact `iss` required, unless IssuerOK is set, which then
	// decides (a templated issuer: Entra's multi-tenant
	// https://login.microsoftonline.com/{tid}/v2.0).
	Issuer   string
	IssuerOK func(iss string, c Claims) bool
	// Audience must be one of the token's `aud` values.
	Audience string
	// Skew is the tolerated clock skew; zero means DefaultSkew.
	Skew time.Duration
	// MaxAge, when set, refuses a token whose `iat` is older than that (plus
	// skew). An ID token fresh from a token endpoint is seconds old; a proxy
	// assertion may legitimately be hours old and leaves this zero.
	MaxAge time.Duration
	// RequireIAT refuses a token without `iat`.
	RequireIAT bool
	Now        func() time.Time
}

// Claims is a verified token's payload. Numbers decode as json.Number.
type Claims map[string]any

// Verify checks the token and returns its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	if v.Keys == nil {
		return nil, errors.New("jwt: verifier has no key source")
	}
	if len(token) > MaxTokenBytes {
		return nil, invalid("token longer than %d bytes", MaxTokenBytes)
	}
	algs, err := v.algorithms()
	if err != nil {
		return nil, err
	}
	jws, err := jose.ParseSignedCompact(token, algs)
	if err != nil {
		return nil, invalid("parse: %v", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, invalid("want one signature, got %d", len(jws.Signatures))
	}
	h := jws.Signatures[0].Protected
	if len(h.ExtraHeaders) > 0 {
		if _, crit := h.ExtraHeaders["crit"]; crit {
			// No extension is understood, so a critical one cannot be honoured.
			return nil, invalid("crit header present")
		}
	}
	alg := jose.SignatureAlgorithm(h.Algorithm)
	key, err := v.Keys.Key(ctx, h.KeyID, alg)
	if errors.Is(err, ErrKeySetUnavailable) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !key.IsPublic() || !key.Valid() {
		return nil, invalid("key %q is not a public key", key.KeyID)
	}
	if key.Algorithm != "" && key.Algorithm != string(alg) {
		return nil, invalid("key %q is for %s, token is %s", key.KeyID, key.Algorithm, alg)
	}
	payload, err := jws.Verify(key.Key)
	if err != nil {
		return nil, invalid("signature: %v", err)
	}
	var c Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil || c == nil {
		return nil, invalid("payload is not a JSON object")
	}
	if err := v.checkClaims(c); err != nil {
		return nil, err
	}
	return c, nil
}

func (v *Verifier) algorithms() ([]jose.SignatureAlgorithm, error) {
	if len(v.Algorithms) == 0 {
		return []jose.SignatureAlgorithm{jose.RS256}, nil
	}
	for _, a := range v.Algorithms {
		if !AllowedAlgorithm(a) {
			return nil, fmt.Errorf("jwt: algorithm %q may not be allowed", a)
		}
	}
	return v.Algorithms, nil
}

// AllowedAlgorithm reports whether a is an asymmetric signature algorithm.
func AllowedAlgorithm(a jose.SignatureAlgorithm) bool {
	for _, ok := range AsymmetricAlgorithms {
		if a == ok {
			return true
		}
	}
	return false
}

func (v *Verifier) checkClaims(c Claims) error {
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	skew := v.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	iss, _ := c.String("iss")
	switch {
	case v.IssuerOK != nil:
		if iss == "" || !v.IssuerOK(iss, c) {
			return invalid("issuer %q refused", iss)
		}
	case v.Issuer == "" || !ConstantEqual(iss, v.Issuer):
		return invalid("issuer %q refused", iss)
	}
	if v.Audience == "" || !c.HasAudience(v.Audience) {
		return invalid("audience refused")
	}
	exp, ok, err := c.Time("exp")
	if err != nil || !ok {
		return invalid("exp missing or malformed")
	}
	if !now.Before(exp.Add(skew)) {
		return invalid("expired")
	}
	if nbf, ok, err := c.Time("nbf"); err != nil {
		return invalid("nbf malformed")
	} else if ok && now.Add(skew).Before(nbf) {
		return invalid("not yet valid")
	}
	iat, ok, err := c.Time("iat")
	if err != nil {
		return invalid("iat malformed")
	}
	if !ok {
		if v.RequireIAT {
			return invalid("iat missing")
		}
		return nil
	}
	if now.Add(skew).Before(iat) {
		return invalid("issued in the future")
	}
	if v.MaxAge > 0 && now.Sub(iat) > v.MaxAge+skew {
		return invalid("issued too long ago")
	}
	return nil
}

// ConstantEqual compares two strings in constant time.
func ConstantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// String returns a string claim.
func (c Claims) String(name string) (string, bool) {
	s, ok := c[name].(string)
	return s, ok
}

// Bool returns a boolean claim. Some providers send "true" as a string
// (Cognito's email_verified), which is read as true too.
func (c Claims) Bool(name string) bool {
	switch v := c[name].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// Strings returns a claim that is a string or an array of strings. Non-string
// elements are skipped.
func (c Claims) Strings(name string) []string {
	switch v := c[name].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Time returns a NumericDate claim; ok is false when it is absent.
func (c Claims) Time(name string) (time.Time, bool, error) {
	raw, present := c[name]
	if !present {
		return time.Time{}, false, nil
	}
	n, ok := raw.(json.Number)
	if !ok {
		return time.Time{}, true, fmt.Errorf("%s is not a number", name)
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1<<40 {
		return time.Time{}, true, fmt.Errorf("%s is out of range", name)
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true, nil
}

// HasAudience reports whether aud is (or contains) want.
func (c Claims) HasAudience(want string) bool {
	for _, a := range c.Strings("aud") {
		if ConstantEqual(a, want) {
			return true
		}
	}
	return false
}
