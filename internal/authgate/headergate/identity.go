package headergate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

// Identity is what one verified edge assertion says about the person (or
// service) behind a request. It is the gate's word, not yet a loomcycle
// identity: the identity map turns it into a tenant and a subject.
type Identity struct {
	// Issuer names who asserted it: the Cloudflare team URL, the IdP behind
	// the ALB, or "" for a forward-auth proxy (the gate itself).
	Issuer string
	// Subject is the assertion's stable key at that issuer.
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
	// Tenant is a tenant the assertion names, for the identity map's
	// allowed_tenants to judge. Never used as-is.
	Tenant string
	// ExpiresAt is when the assertion stops being valid.
	ExpiresAt time.Time
	// AMR is how the edge says it authenticated.
	AMR []string
	// Claims are the assertion's other claims, for a claim-driven identity
	// map (tenant from `hd`, groups from `roles`).
	Claims map[string]any
}

// clockSkew is how far iat/nbf may sit in the future and exp in the past:
// clocks between an edge and this server drift a little.
const clockSkew = time.Minute

// maxAssertionBytes bounds a header assertion before any parsing. The edges'
// JWTs are a few KB; a header beyond this is not one.
const maxAssertionBytes = 16 << 10

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{authgate.ErrInvalidCredential}, a...)...)
}

// numericDate reads a JWT NumericDate claim (seconds since the epoch, a JSON
// number, possibly fractional). ok=false when absent or not a number.
func numericDate(v any) (time.Time, bool) {
	switch n := v.(type) {
	case float64:
		return time.Unix(int64(n), 0), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(int64(f), 0), true
	}
	return time.Time{}, false
}

// checkTimes enforces exp (required), and nbf/iat when present, against now.
func checkTimes(claims map[string]any, now time.Time) (time.Time, error) {
	exp, ok := numericDate(claims["exp"])
	if !ok {
		return time.Time{}, invalid("assertion has no exp")
	}
	if !now.Before(exp.Add(clockSkew)) {
		return time.Time{}, invalid("assertion expired")
	}
	for _, k := range []string{"nbf", "iat"} {
		if v, present := claims[k]; present {
			t, ok := numericDate(v)
			if !ok {
				return time.Time{}, invalid("assertion %s is not a date", k)
			}
			if t.After(now.Add(clockSkew)) {
				return time.Time{}, invalid("assertion %s is in the future", k)
			}
		}
	}
	return exp, nil
}

// audContains reports whether a JWT aud (a string or an array of strings)
// names want.
func audContains(v any, want string) bool {
	switch a := v.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func stringClaim(claims map[string]any, k string) string {
	s, _ := claims[k].(string)
	return strings.TrimSpace(s)
}

func stringsClaim(claims map[string]any, k string) []string {
	switch v := claims[k].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) != "" {
			return []string{strings.TrimSpace(v)}
		}
	}
	return nil
}

func boolClaim(claims map[string]any, k string) bool {
	switch v := claims[k].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// decodeClaims parses a JWT payload as a JSON object, keeping numbers exact.
func decodeClaims(payload []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, invalid("assertion payload is not a JSON object")
	}
	return m, nil
}
