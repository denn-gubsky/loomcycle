package jwtverify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(claims)
	obj, err := s.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func claims(now time.Time) map[string]any {
	return map[string]any{"iss": "https://idp.example", "aud": "client-1", "sub": "u1",
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix()}
}

// jwksServer serves the public halves of the keys it holds, which a test may
// change (a rotation).
type jwksServer struct {
	mu   sync.Mutex
	keys []jose.JSONWebKey
	hits atomic.Int64
	srv  *httptest.Server
}

func newJWKSServer(t *testing.T, keys ...jose.JSONWebKey) *jwksServer {
	j := &jwksServer{keys: keys}
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		j.hits.Add(1)
		j.mu.Lock()
		defer j.mu.Unlock()
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: j.keys})
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func (j *jwksServer) set(keys ...jose.JSONWebKey) {
	j.mu.Lock()
	j.keys = keys
	j.mu.Unlock()
}

func pub(kid string, k any) jose.JSONWebKey {
	switch v := k.(type) {
	case *rsa.PrivateKey:
		return jose.JSONWebKey{Key: &v.PublicKey, KeyID: kid, Use: "sig"}
	case *ecdsa.PrivateKey:
		return jose.JSONWebKey{Key: &v.PublicKey, KeyID: kid, Use: "sig"}
	}
	panic("key type")
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func verifier(src KeySource, c *clock) *Verifier {
	return &Verifier{Keys: src, Algorithms: []jose.SignatureAlgorithm{jose.RS256, jose.ES256},
		Issuer: "https://idp.example", Audience: "client-1", MaxAge: 5 * time.Minute, RequireIAT: true, Now: c.now}
}

func TestVerify_AcceptsRSAAndECTokensFromTheKeySet(t *testing.T) {
	rk := rsaKey(t)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	j := newJWKSServer(t, pub("r1", rk), pub("e1", ek))
	c := &clock{t: time.Now()}
	v := verifier(&RemoteJWKS{URL: j.srv.URL, Now: c.now}, c)
	for _, tok := range []string{sign(t, jose.RS256, rk, "r1", claims(c.t)), sign(t, jose.ES256, ek, "e1", claims(c.t))} {
		got, err := v.Verify(context.Background(), tok)
		if err != nil {
			t.Fatal(err)
		}
		if s, _ := got.String("sub"); s != "u1" {
			t.Fatalf("claims = %v", got)
		}
	}
}

func TestVerify_RefusesBadClaims(t *testing.T) {
	rk := rsaKey(t)
	src := StaticKeys{pub("r1", rk)}
	c := &clock{t: time.Now()}
	v := verifier(src, c)
	cases := map[string]func(m map[string]any){
		"wrong aud":        func(m map[string]any) { m["aud"] = "someone-else" },
		"wrong iss":        func(m map[string]any) { m["iss"] = "https://evil.example" },
		"expired":          func(m map[string]any) { m["exp"] = c.t.Add(-6 * time.Minute).Unix() },
		"no exp":           func(m map[string]any) { delete(m, "exp") },
		"iat in future":    func(m map[string]any) { m["iat"] = c.t.Add(6 * time.Minute).Unix() },
		"iat too old":      func(m map[string]any) { m["iat"] = c.t.Add(-11 * time.Minute).Unix() },
		"no iat":           func(m map[string]any) { delete(m, "iat") },
		"nbf in future":    func(m map[string]any) { m["nbf"] = c.t.Add(6 * time.Minute).Unix() },
		"exp not a number": func(m map[string]any) { m["exp"] = "tomorrow" },
	}
	for name, mut := range cases {
		m := claims(c.t)
		mut(m)
		if _, err := v.Verify(context.Background(), sign(t, jose.RS256, rk, "r1", m)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Within the skew is fine.
	m := claims(c.t)
	m["exp"] = c.t.Add(-4 * time.Minute).Unix()
	if _, err := v.Verify(context.Background(), sign(t, jose.RS256, rk, "r1", m)); err != nil {
		t.Errorf("exp inside the skew refused: %v", err)
	}
	// An audience list containing ours is fine.
	m = claims(c.t)
	m["aud"] = []string{"other", "client-1"}
	if _, err := v.Verify(context.Background(), sign(t, jose.RS256, rk, "r1", m)); err != nil {
		t.Errorf("aud list refused: %v", err)
	}
}

// alg:none and the RS256→HS256 confusion (an HMAC keyed by the provider's
// public key) must never verify, whatever the key set holds.
func TestVerify_RefusesNoneAndHMACWithThePublicKey(t *testing.T) {
	rk := rsaKey(t)
	c := &clock{t: time.Now()}
	v := verifier(StaticKeys{pub("r1", rk)}, c)
	body, _ := json.Marshal(claims(c.t))
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"r1"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(body) + "."
	if _, err := v.Verify(context.Background(), none); !errors.Is(err, ErrInvalid) {
		t.Fatalf("alg none: %v", err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&rk.PublicKey)
	hs := sign(t, jose.HS256, der, "r1", claims(c.t))
	if _, err := v.Verify(context.Background(), hs); !errors.Is(err, ErrInvalid) {
		t.Fatalf("HS256 keyed by the public key: %v", err)
	}
	// And a verifier configured to allow HS256 is refused outright.
	bad := verifier(StaticKeys{pub("r1", rk)}, c)
	bad.Algorithms = []jose.SignatureAlgorithm{jose.HS256}
	if _, err := bad.Verify(context.Background(), hs); err == nil {
		t.Fatal("a verifier allowing HS256 verified a token")
	}
}

func TestVerify_RefusesAForeignKeyAndAMissingKid(t *testing.T) {
	rk, other := rsaKey(t), rsaKey(t)
	c := &clock{t: time.Now()}
	v := verifier(StaticKeys{pub("r1", rk)}, c)
	if _, err := v.Verify(context.Background(), sign(t, jose.RS256, other, "r1", claims(c.t))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("signed by another key under our kid: %v", err)
	}
	// No kid: accepted only when one key could have signed it.
	if _, err := v.Verify(context.Background(), sign(t, jose.RS256, rk, "", claims(c.t))); err != nil {
		t.Fatalf("no kid, one key: %v", err)
	}
	two := verifier(StaticKeys{pub("r1", rk), pub("r2", other)}, c)
	if _, err := two.Verify(context.Background(), sign(t, jose.RS256, rk, "", claims(c.t))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no kid, two keys: %v", err)
	}
}

// RFC FA §8 JWKS rotation: a token under a new kid verifies after one refresh,
// and a stream of unknown kids is rate-limited to one fetch per MinRefresh.
func TestRemoteJWKS_RotationVerifiesAfterOneRefreshAndStormsAreRateLimited(t *testing.T) {
	old, rotated := rsaKey(t), rsaKey(t)
	j := newJWKSServer(t, pub("k1", old))
	c := &clock{t: time.Now()}
	src := &RemoteJWKS{URL: j.srv.URL, Now: c.now}
	v := verifier(src, c)
	ctx := context.Background()
	if _, err := v.Verify(ctx, sign(t, jose.RS256, old, "k1", claims(c.t))); err != nil {
		t.Fatal(err)
	}
	if src.Fetches() != 1 {
		t.Fatalf("fetches after first verify = %d", src.Fetches())
	}
	// The provider rotates. The first token under the new kid makes one
	// fetch and verifies.
	c.t = c.t.Add(2 * time.Minute)
	j.set(pub("k1", old), pub("k2", rotated))
	if _, err := v.Verify(ctx, sign(t, jose.RS256, rotated, "k2", claims(c.t))); err != nil {
		t.Fatalf("new kid after one refresh: %v", err)
	}
	if src.Fetches() != 2 {
		t.Fatalf("fetches after rotation = %d, want 2", src.Fetches())
	}
	// A storm of made-up kids inside MinRefresh: at most one more fetch.
	for i := 0; i < 50; i++ {
		tok := sign(t, jose.RS256, rotated, "bogus-"+string(rune('a'+i%26)), claims(c.t))
		if _, err := v.Verify(ctx, tok); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unknown kid verified: %v", err)
		}
	}
	if n := src.Fetches(); n > 3 {
		t.Fatalf("refresh storm made %d fetches", n)
	}
	if hits := j.hits.Load(); hits > 3 {
		t.Fatalf("provider saw %d key set requests", hits)
	}
	// Known keys keep verifying while refreshes are held back.
	if _, err := v.Verify(ctx, sign(t, jose.RS256, old, "k1", claims(c.t))); err != nil {
		t.Fatalf("known key during the hold: %v", err)
	}
}

func TestParseKeySet_DropsSymmetricAndEncryptionKeys(t *testing.T) {
	rk := rsaKey(t)
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: []byte("shared-secret-0123456789abcdef!!"), KeyID: "oct", Algorithm: "HS256"},
		{Key: &rk.PublicKey, KeyID: "enc", Use: "enc"},
		{Key: &rk.PublicKey, KeyID: "sig", Use: "sig"},
	}}
	body, _ := json.Marshal(set)
	keys, err := ParseKeySet(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].KeyID != "sig" {
		t.Fatalf("kept %d keys: %+v", len(keys), keys)
	}
}

func TestVerify_IssuerFunc(t *testing.T) {
	rk := rsaKey(t)
	c := &clock{t: time.Now()}
	v := verifier(StaticKeys{pub("r1", rk)}, c)
	v.Issuer = ""
	v.IssuerOK = func(iss string, cl Claims) bool {
		tid, _ := cl.String("tid")
		return iss == "https://login.example/"+tid+"/v2.0"
	}
	m := claims(c.t)
	m["tid"], m["iss"] = "t1", "https://login.example/t1/v2.0"
	if _, err := v.Verify(context.Background(), sign(t, jose.RS256, rk, "r1", m)); err != nil {
		t.Fatal(err)
	}
	m["iss"] = "https://login.example/t2/v2.0"
	if _, err := v.Verify(context.Background(), sign(t, jose.RS256, rk, "r1", m)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("templated issuer mismatch: %v", err)
	}
}
