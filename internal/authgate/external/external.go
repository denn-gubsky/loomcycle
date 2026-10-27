// Package external verifies what an external gate (RFC FA §3.5) sends
// loomcycle: a JWT signed under the one key the operator pinned for that gate.
//
// An external gate is a separate process — a bridge, a website, a broker in
// front of an identity provider loomcycle has never heard of. loomcycle never
// calls it and never runs it; the only thing it trusts is a signature under
// the gate's registered key. So everything that makes an assertion safe to act
// on is checked here, once, for every route that takes one:
//
//   - the signature, under the gate's own key and only the algorithm that key
//     is for (Ed25519 → EdDSA, P-256 → ES256, an HMAC secret → HS256), so one
//     gate's key never verifies another gate's assertion and no `alg` header
//     chooses the check;
//   - `iss` names the gate, `aud` is this deployment's issuer URL and nothing
//     else, `lc_use` is the purpose the route serves (a login assertion is not
//     a revoke request);
//   - `iat` and `exp` are present, the lifetime is at most five minutes, it has
//     not expired and was not issued in the future;
//   - `jti` is used once, in a cache the replicas share (the store), so an
//     assertion replayed to another replica is refused too.
//
// The replay check runs last, after the signature: only a gate's own key can
// put a jti in the cache.
package external

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	_ "crypto/sha256" // registers crypto.SHA256 for the thumbprint
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Use is what an assertion is for. Each route accepts exactly one, so a JWT
// minted for one purpose cannot be replayed to another within its lifetime.
type Use string

const (
	// UseLogin completes a browser login (GET/POST /v1/_auth/complete/{gate});
	// it must carry the login_state loomcycle handed the gate.
	UseLogin Use = "login"
	// UseAssert asks for an issued token for a non-browser client
	// (POST /v1/_auth/assert).
	UseAssert Use = "assert"
	// UseRevoke deprovisions one subject (POST /v1/_auth/revoke).
	UseRevoke Use = "revoke"
	// UseSync reconciles users and groups in bulk (POST /v1/_auth/sync).
	UseSync Use = "sync"
)

// Limits on what one assertion may carry. They bound the work an assertion
// can cause before anything else looks at it; the identity map applies its
// own rules on top.
const (
	// MaxLifetime is the longest exp − iat accepted (RFC FA D4).
	MaxLifetime = 5 * time.Minute
	// ClockSkew is how far a gate's clock may run ahead: iat, nbf and
	// auth_time may be this far in the future. exp gets no leeway — an
	// assertion is refused at now >= exp.
	ClockSkew = 30 * time.Second
	// ReplayMargin is how long a used jti is kept past its exp, so the
	// cache entry outlives every moment the assertion could still verify.
	ReplayMargin = time.Minute
	// MaxCompactBytes bounds the JWT itself; a sync of many users is the
	// largest legitimate one.
	MaxCompactBytes = 8 << 20
	maxIDLen        = 256
	maxJTILen       = 128
	maxGroups       = 256
	maxGroupLen     = 128
	maxSyncUsers    = 10000
	minHMACSecret   = 32
)

var (
	// ErrInvalid: the JWT is malformed, not signed by this gate's key, or its
	// claims do not hold. Callers answer it with one opaque message.
	ErrInvalid = errors.New("external gate: invalid assertion")
	// ErrReplayed: a valid assertion whose jti was already used.
	ErrReplayed = errors.New("external gate: assertion already used")
	// ErrUnavailable: the replay cache could not be consulted. The assertion
	// is refused (fail closed); the caller may retry with a new one.
	ErrUnavailable = errors.New("external gate: replay cache unavailable")
)

// Key is a gate's pinned verification key and the one algorithm it verifies.
type Key struct {
	alg    jose.SignatureAlgorithm
	verify any
	kid    string
	thumb  string
}

// Algorithm is the JWS algorithm this key verifies (EdDSA, ES256 or HS256).
func (k Key) Algorithm() string { return string(k.alg) }

// KeyID is the JWK's kid, when it had one.
func (k Key) KeyID() string { return k.kid }

// Fingerprint identifies the key without revealing it: the RFC 7638
// thumbprint of a public key, or a label for an HMAC secret (whose digest
// would be an offline-guessable verifier for the secret). For listings and
// for comparing with what a gate's discovery document says.
func (k Key) Fingerprint() string { return k.thumb }

// Symmetric reports whether the key is a shared HMAC secret.
func (k Key) Symmetric() bool { return k.alg == jose.HS256 }

// ParsePublicJWK parses the operator's pinned key: an Ed25519 (kty OKP) or a
// P-256 (kty EC) PUBLIC JWK. A private key is refused — config holds only what
// verifies, and a private key there would let anyone who reads the config
// mint assertions.
func ParsePublicJWK(raw []byte) (Key, error) {
	if hasPrivatePart(raw) {
		return Key{}, errors.New("the JWK holds a private key (\"d\"); register only the public key")
	}
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(raw); err != nil {
		return Key{}, fmt.Errorf("not a JWK: %w", err)
	}
	if !jwk.Valid() || !jwk.IsPublic() {
		return Key{}, errors.New("the JWK is not a valid public key")
	}
	var alg jose.SignatureAlgorithm
	switch pk := jwk.Key.(type) {
	case ed25519.PublicKey:
		alg = jose.EdDSA
	case *ecdsa.PublicKey:
		if pk.Curve != elliptic.P256() {
			return Key{}, errors.New("an EC key must be on P-256 (ES256)")
		}
		alg = jose.ES256
	default:
		return Key{}, errors.New("the key must be Ed25519 (kty OKP) or P-256 (kty EC)")
	}
	if jwk.Algorithm != "" && jwk.Algorithm != string(alg) {
		return Key{}, fmt.Errorf("the JWK says alg %q but the key is for %s", jwk.Algorithm, alg)
	}
	if jwk.Use != "" && jwk.Use != "sig" {
		return Key{}, fmt.Errorf("the JWK's use is %q, not sig", jwk.Use)
	}
	tp, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return Key{}, err
	}
	return Key{alg: alg, verify: jwk.Key, kid: jwk.KeyID, thumb: "sha256:" + base64.RawURLEncoding.EncodeToString(tp)}, nil
}

// hasPrivatePart reports whether a JWK object carries a private member. go-jose
// would parse such a key as a private key and verify with its public half;
// refusing it outright is clearer.
func hasPrivatePart(raw []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	for _, k := range []string{"d", "p", "q", "dp", "dq", "qi", "k"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// HMACKey makes a key from a shared secret, for a gate that runs beside
// loomcycle and holds the same `$cred:` credential. At least 32 bytes: HS256
// is only as strong as its secret.
func HMACKey(secret []byte) (Key, error) {
	if len(secret) < minHMACSecret {
		return Key{}, fmt.Errorf("an HMAC secret must be at least %d bytes, got %d", minHMACSecret, len(secret))
	}
	return Key{alg: jose.HS256, verify: append([]byte(nil), secret...), thumb: "hmac-sha256"}, nil
}

// ReplayCache records jtis. UseOnce stores (gate, jti) until `until` and
// reports whether it was new; false means the jti was already used. now is
// the verifier's one clock reading, so an entry is judged expired by the
// same clock that accepted the assertion. It must be shared by every replica
// that accepts assertions for the gate, and the insert must be atomic per
// (gate, jti) — a unique key, not a read then a write.
type ReplayCache interface {
	UseOnce(ctx context.Context, gate, jti string, until, now time.Time) (bool, error)
}

// Verifier checks one gate's assertions.
type Verifier struct {
	// Gate is the gate's configured name; `iss` must equal it.
	Gate string
	Key  Key
	// Audience is this deployment's issuer URL (LOOMCYCLE_PUBLIC_URL without
	// a trailing slash); `aud` must be exactly it.
	Audience string
	Replay   ReplayCache
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Claims is a verified assertion's content. The identity fields describe the
// person at the gate; the identity map decides what they mean here.
type Claims struct {
	Gate     string
	Use      Use
	JTI      string
	IssuedAt time.Time
	Expiry   time.Time

	// Subject is the person's stable id at the gate (`sub`). For a revoke it
	// names the person to deprovision.
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	Groups        []string
	// Tenant is the gate's tenant hint, for the identity map to judge.
	Tenant string
	// AccessMode is what the gate asks for; honoured only on a trusted gate.
	AccessMode string
	// AMR is what the gate says about how the person authenticated. It is
	// parsed but never made a session's or token's amr: loomcycle did not see
	// that factor (internal/api/http assertionOf).
	AMR []string
	// AuthTime is when the person authenticated at the gate, never later
	// than IssuedAt; IssuedAt when the gate did not say.
	AuthTime time.Time
	// LoginState is the one-time state of a browser login (UseLogin only).
	LoginState string
	// Token is what a UseAssert caller asks the issued token to be held to.
	// It can only narrow what the gate's configuration allows.
	Token *TokenRequest
	// Users and Complete are a sync's payload. Complete says Users is every
	// user the gate knows, so one missing from it is gone.
	Users    []SyncUser
	Complete bool
}

// TokenRequest narrows an issued token (UseAssert).
type TokenRequest struct {
	Agents     []string `json:"agents,omitempty"`
	Overrides  []string `json:"overrides,omitempty"`
	RatePerMin int      `json:"rate_per_minute,omitempty"`
	RateBurst  int      `json:"rate_burst,omitempty"`
	TTLSeconds int      `json:"ttl_seconds,omitempty"`
}

// SyncUser is one user in a sync.
type SyncUser struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified,omitempty"`
	DisplayName   string   `json:"name,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	Tenant        string   `json:"tenant,omitempty"`
	AccessMode    string   `json:"access_mode,omitempty"`
}

// wireClaims is the JSON beside the registered claims.
type wireClaims struct {
	Use           Use           `json:"lc_use"`
	LoginState    string        `json:"login_state,omitempty"`
	Email         string        `json:"email,omitempty"`
	EmailVerified bool          `json:"email_verified,omitempty"`
	Name          string        `json:"name,omitempty"`
	Groups        []string      `json:"groups,omitempty"`
	Tenant        string        `json:"tenant,omitempty"`
	AccessMode    string        `json:"access_mode,omitempty"`
	AMR           []string      `json:"amr,omitempty"`
	AuthTime      int64         `json:"auth_time,omitempty"`
	Token         *TokenRequest `json:"token,omitempty"`
	Users         []SyncUser    `json:"users,omitempty"`
	Complete      bool          `json:"complete,omitempty"`
}

// Verify checks compact as an assertion of this gate for use. Every refusal
// is ErrInvalid, ErrReplayed or ErrUnavailable, wrapped with a reason fit for
// a log line (never for the client: tell it only that the assertion was
// refused).
func (v *Verifier) Verify(ctx context.Context, compact string, use Use) (Claims, error) {
	if v == nil || v.Key.verify == nil || v.Gate == "" || v.Audience == "" || v.Replay == nil {
		return Claims{}, fmt.Errorf("%w: verifier is not configured", ErrInvalid)
	}
	if compact == "" || len(compact) > MaxCompactBytes {
		return Claims{}, fmt.Errorf("%w: empty or oversized", ErrInvalid)
	}
	tok, err := jwt.ParseSigned(compact, []jose.SignatureAlgorithm{v.Key.alg})
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(tok.Headers) != 1 {
		return Claims{}, fmt.Errorf("%w: want exactly one signature", ErrInvalid)
	}
	if kid := tok.Headers[0].KeyID; kid != "" && v.Key.kid != "" && kid != v.Key.kid {
		return Claims{}, fmt.Errorf("%w: kid %q is not the gate's key", ErrInvalid, kid)
	}
	var std jwt.Claims
	var wc wireClaims
	if err := tok.Claims(v.Key.verify, &std, &wc); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	c, err := v.check(std, wc, use, now)
	if err != nil {
		return Claims{}, err
	}
	// Last, after the signature, aud and exp held: only an assertion this
	// gate signed reaches the cache, so nobody else can grow it or burn a
	// gate's jti.
	fresh, err := v.Replay.UseOnce(ctx, v.Gate, c.JTI, c.Expiry.Add(ReplayMargin), now)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !fresh {
		return Claims{}, fmt.Errorf("%w: jti %q", ErrReplayed, c.JTI)
	}
	return c, nil
}

func (v *Verifier) check(std jwt.Claims, wc wireClaims, use Use, now time.Time) (Claims, error) {
	bad := func(format string, args ...any) (Claims, error) {
		return Claims{}, fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
	}
	if std.Issuer != v.Gate {
		return bad("iss %q is not gate %q", std.Issuer, v.Gate)
	}
	if len(std.Audience) != 1 || std.Audience[0] != v.Audience {
		return bad("aud %v is not exactly %q", []string(std.Audience), v.Audience)
	}
	if wc.Use != use {
		return bad("lc_use %q, want %q", wc.Use, use)
	}
	if std.IssuedAt == nil || std.Expiry == nil {
		return bad("iat and exp are required")
	}
	iat, exp := std.IssuedAt.Time(), std.Expiry.Time()
	if !exp.After(iat) || exp.Sub(iat) > MaxLifetime {
		return bad("lifetime %s is not within (0, %s]", exp.Sub(iat), MaxLifetime)
	}
	if iat.After(now.Add(ClockSkew)) {
		return bad("issued in the future")
	}
	if !now.Before(exp) {
		return bad("expired")
	}
	if std.NotBefore != nil && std.NotBefore.Time().After(now.Add(ClockSkew)) {
		return bad("not valid yet")
	}
	if std.ID == "" || len(std.ID) > maxJTILen {
		return bad("jti is required (at most %d bytes)", maxJTILen)
	}
	c := Claims{
		Gate: v.Gate, Use: use, JTI: std.ID, IssuedAt: iat, Expiry: exp,
		Subject: std.Subject, Email: wc.Email, EmailVerified: wc.EmailVerified,
		DisplayName: wc.Name, Groups: wc.Groups, Tenant: wc.Tenant, AccessMode: wc.AccessMode,
		AMR: wc.AMR, AuthTime: iat, LoginState: wc.LoginState, Token: wc.Token,
	}
	if wc.AuthTime != 0 {
		at := time.Unix(wc.AuthTime, 0)
		if at.After(now.Add(ClockSkew)) {
			return bad("auth_time in the future")
		}
		// Never later than iat: a gate may say the person signed in earlier
		// than it signed (less fresh), never that they did so after it — the
		// assertion cannot vouch for a moment it did not see.
		if at.After(iat) {
			at = iat
		}
		c.AuthTime = at
	}
	switch use {
	case UseLogin, UseAssert, UseRevoke:
		if err := checkID("sub", c.Subject); err != nil {
			return bad("%v", err)
		}
	case UseSync:
		if c.Subject != "" {
			return bad("a sync names no sub")
		}
	}
	if use == UseLogin {
		if c.LoginState == "" || len(c.LoginState) > maxIDLen {
			return bad("login_state is required on a login")
		}
	} else if c.LoginState != "" {
		return bad("login_state belongs only to a login")
	}
	if c.Token != nil && use != UseAssert {
		return bad("token belongs only to an assert")
	}
	if err := checkGroups(c.Groups); err != nil {
		return bad("%v", err)
	}
	if use == UseSync {
		if len(wc.Users) > maxSyncUsers {
			return bad("at most %d users per sync", maxSyncUsers)
		}
		seen := make(map[string]bool, len(wc.Users))
		for i, u := range wc.Users {
			if err := checkID(fmt.Sprintf("users[%d].sub", i), u.Subject); err != nil {
				return bad("%v", err)
			}
			if seen[u.Subject] {
				return bad("users[%d]: %q appears twice", i, u.Subject)
			}
			seen[u.Subject] = true
			if err := checkGroups(u.Groups); err != nil {
				return bad("users[%d]: %v", i, err)
			}
		}
		c.Users, c.Complete = wc.Users, wc.Complete
	} else if len(wc.Users) > 0 || wc.Complete {
		return bad("users belong only to a sync")
	}
	return c, nil
}

func checkID(name, v string) error {
	if v == "" || len(v) > maxIDLen || strings.ContainsAny(v, "\x00\r\n") {
		return fmt.Errorf("%s is required (at most %d bytes, no control characters)", name, maxIDLen)
	}
	return nil
}

func checkGroups(gs []string) error {
	if len(gs) > maxGroups {
		return fmt.Errorf("at most %d groups", maxGroups)
	}
	for _, g := range gs {
		if g == "" || len(g) > maxGroupLen || strings.ContainsAny(g, "\x00\r\n") {
			return fmt.Errorf("group %q is empty, longer than %d bytes or has control characters", g, maxGroupLen)
		}
	}
	return nil
}

// Issuer normalises a public URL into the audience an assertion must name:
// LOOMCYCLE_PUBLIC_URL without trailing slashes.
func Issuer(publicURL string) string { return strings.TrimRight(strings.TrimSpace(publicURL), "/") }
