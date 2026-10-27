package headergate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/identity"
	"github.com/denn-gubsky/loomcycle/internal/authgate/jwtverify"
)

const (
	cfTeam = "acme.cloudflareaccess.com"
	cfAud  = "aud-tag-1"
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signJWT(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := s.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cfClaims() map[string]any {
	return map[string]any{
		"iss": "https://" + cfTeam, "aud": []string{cfAud}, "type": "app",
		"sub": "u-123", "email": "alice@acme.com", "exp": t0.Add(time.Hour).Unix(), "iat": t0.Unix(),
	}
}

func pinned(kid string, k *rsa.PrivateKey) jwtverify.KeySource {
	return jwtverify.StaticKeys{{Key: &k.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}}
}

func cfGate(t *testing.T, keys jwtverify.KeySource) *CloudflareAccess {
	t.Helper()
	g, err := NewCloudflareAccess(cfTeam, cfAud, keys, nil, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestCloudflare_ValidTokenNamesTheUser(t *testing.T) {
	k := rsaKey(t)
	id, err := cfGate(t, pinned("k1", k)).Verify(context.Background(), signJWT(t, jose.RS256, k, "k1", cfClaims()))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u-123" || id.Email != "alice@acme.com" || id.Issuer != "https://"+cfTeam || !id.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("identity %+v", id)
	}
}

func TestCloudflare_WrongClaimsAreRefused(t *testing.T) {
	k := rsaKey(t)
	cases := map[string]func(map[string]any){
		"another team's issuer":      func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" },
		"another application's aud":  func(c map[string]any) { c["aud"] = []string{"aud-tag-2"} },
		"expired":                    func(c map[string]any) { c["exp"] = t0.Add(-time.Hour).Unix() },
		"no exp":                     func(c map[string]any) { delete(c, "exp") },
		"issued in the future":       func(c map[string]any) { c["iat"] = t0.Add(time.Hour).Unix() },
		"no sub and no service name": func(c map[string]any) { delete(c, "sub") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := cfClaims()
			mutate(c)
			if _, err := cfGate(t, pinned("k1", k)).Verify(context.Background(), signJWT(t, jose.RS256, k, "k1", c)); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
			}
		})
	}
}

func TestCloudflare_WrongAlgorithmOrKeyIsRefused(t *testing.T) {
	k := rsaKey(t)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecKeys := jwtverify.StaticKeys{{Key: &ec.PublicKey, KeyID: "k1", Algorithm: string(jose.ES256), Use: "sig"}}
	cases := map[string]struct {
		keys  jwtverify.KeySource
		token func() string
	}{
		// ES256 is an asymmetric algorithm the shared verifier knows, but
		// Cloudflare signs RS256 only, and this gate allows nothing else.
		"ES256 under an ES256 key": {ecKeys, func() string { return signJWT(t, jose.ES256, ec, "k1", cfClaims()) }},
		"HS256 keyed by the public key bytes": {pinned("k1", k), func() string {
			return signJWT(t, jose.HS256, []byte(fmt.Sprint(k.PublicKey.N)), "k1", cfClaims())
		}},
		"signed by another RSA key": {pinned("k1", k), func() string { return signJWT(t, jose.RS256, rsaKey(t), "k1", cfClaims()) }},
		"unknown kid":               {pinned("k1", k), func() string { return signJWT(t, jose.RS256, k, "k2", cfClaims()) }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := cfGate(t, tc.keys).Verify(context.Background(), tc.token()); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
			}
		})
	}
}

func TestCloudflare_ServiceTokenGetsAServiceSubject(t *testing.T) {
	k := rsaKey(t)
	c := cfClaims()
	c["sub"], c["email"], c["common_name"] = "", "", "abc.access"
	id, err := cfGate(t, pinned("k1", k)).Verify(context.Background(), signJWT(t, jose.RS256, k, "k1", c))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "service:abc.access" || id.Email != "" {
		t.Fatalf("identity %+v", id)
	}
	// The identity map reads the sub claim before the subject: Access sends
	// "sub": "" for a service token, which must not hide the service subject.
	got, err := identity.Subject("cf", nil, authgate.Assertion{Subject: id.Subject, Claims: id.Claims})
	if err != nil || got != "cf:service:abc.access" {
		t.Fatalf("mapped subject = %q, %v; want cf:service:abc.access", got, err)
	}
}

// certsServer serves a team's /cdn-cgi/access/certs, counting fetches.
type certsServer struct {
	mu      sync.Mutex
	keys    []jose.JSONWebKey
	fetches int
	srv     *httptest.Server
}

func newCertsServer(t *testing.T) *certsServer {
	c := &certsServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.fetches++
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: c.keys})
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *certsServer) publish(kid string, k *rsa.PrivateKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"})
}

func (c *certsServer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetches
}

func TestCloudflare_RotatedKidVerifiesAfterOneRefreshAndAStormIsRateLimited(t *testing.T) {
	cs := newCertsServer(t)
	old := rsaKey(t)
	cs.publish("2026-q3", old)
	now := t0
	clock := func() time.Time { return now }
	jwks := &jwtverify.RemoteJWKS{URL: cs.srv.URL, Client: cs.srv.Client(), Now: clock}
	g, err := NewCloudflareAccess(cfTeam, cfAud, jwks, nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Verify(context.Background(), signJWT(t, jose.RS256, old, "2026-q3", cfClaims())); err != nil {
		t.Fatal(err)
	}
	// Six weeks on, Cloudflare rotates: a new kid appears beside the old.
	now = now.Add(2 * time.Minute)
	fresh := rsaKey(t)
	cs.publish("2026-q4", fresh)
	before := cs.count()
	if _, err := g.Verify(context.Background(), signJWT(t, jose.RS256, fresh, "2026-q4", cfClaims())); err != nil {
		t.Fatalf("token under the rotated kid: %v", err)
	}
	if cs.count() != before+1 {
		t.Fatalf("rotation took %d fetches, want 1", cs.count()-before)
	}
	// A burst of made-up kids inside a minute costs at most one fetch.
	before = cs.count()
	for i := 0; i < 50; i++ {
		tok := signJWT(t, jose.RS256, fresh, fmt.Sprintf("made-up-%d", i), cfClaims())
		if _, err := g.Verify(context.Background(), tok); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("made-up kid %d: %v", i, err)
		}
	}
	if n := cs.count() - before; n > 1 {
		t.Fatalf("50 unknown kids caused %d fetches, want at most 1", n)
	}
}

func TestNewCloudflareAccess_RefusesABadTeamOrNoAudience(t *testing.T) {
	if _, err := NewCloudflareAccess("https://"+cfTeam, cfAud, nil, nil, nil); err == nil {
		t.Fatal("accepted a URL as the team domain")
	}
	if _, err := NewCloudflareAccess(cfTeam, " ", nil, nil, nil); err == nil {
		t.Fatal("accepted an empty audience")
	}
}

func TestValidTeamDomain_RefusesAnythingButAHostName(t *testing.T) {
	for _, d := range []string{"https://acme.cloudflareaccess.com", "acme.cloudflareaccess.com/x", "acme.cloudflareaccess.com:443", "localhost", "evil.com@acme.cloudflareaccess.com", ""} {
		if ValidTeamDomain(d) {
			t.Errorf("ValidTeamDomain(%q) = true", d)
		}
	}
	if !ValidTeamDomain("acme.cloudflareaccess.com") || !ValidTeamDomain("login.acme.dev") {
		t.Fatal("a real team domain was refused")
	}
}
