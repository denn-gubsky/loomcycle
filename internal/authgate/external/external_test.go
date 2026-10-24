package external

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const testAud = "https://lc.example"

// memReplay is a ReplayCache in a map.
type memReplay struct {
	mu   sync.Mutex
	seen map[string]bool
	err  error
	n    int
}

func (m *memReplay) UseOnce(_ context.Context, gate, jti string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	if m.err != nil {
		return false, m.err
	}
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	k := gate + "\x00" + jti
	if m.seen[k] {
		return false, nil
	}
	m.seen[k] = true
	return true, nil
}

type signer struct {
	alg jose.SignatureAlgorithm
	key any
	pub Key
}

func edSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(jose.JSONWebKey{Key: pub})
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsePublicJWK(raw)
	if err != nil {
		t.Fatal(err)
	}
	return signer{alg: jose.EdDSA, key: priv, pub: k}
}

func ecSigner(t *testing.T) signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(jose.JSONWebKey{Key: &priv.PublicKey, KeyID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsePublicJWK(raw)
	if err != nil {
		t.Fatal(err)
	}
	return signer{alg: jose.ES256, key: priv, pub: k}
}

func hmacSigner(t *testing.T) signer {
	t.Helper()
	secret := []byte(strings.Repeat("s", 40))
	k, err := HMACKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return signer{alg: jose.HS256, key: secret, pub: k}
}

func (s signer) sign(t *testing.T, std jwt.Claims, extra map[string]any) string {
	t.Helper()
	sg, err := jose.NewSigner(jose.SigningKey{Algorithm: s.alg, Key: s.key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := jwt.Signed(sg).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var jtiN int

func claimsFor(gate string, now time.Time) jwt.Claims {
	jtiN++
	return jwt.Claims{
		Issuer: gate, Audience: jwt.Audience{testAud}, Subject: "12345",
		ID:       fmt.Sprintf("jti-%d", jtiN),
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(2 * time.Minute)),
	}
}

func verifier(gate string, k Key, rc ReplayCache) *Verifier {
	return &Verifier{Gate: gate, Key: k, Audience: testAud, Replay: rc}
}

func TestVerify_AcceptsAnAssertionUnderEachKeyKind(t *testing.T) {
	for name, s := range map[string]signer{"ed25519": edSigner(t), "es256": ecSigner(t), "hmac": hmacSigner(t)} {
		t.Run(name, func(t *testing.T) {
			v := verifier("tg", s.pub, &memReplay{})
			tok := s.sign(t, claimsFor("tg", time.Now()), map[string]any{
				"lc_use": "assert", "groups": []string{"tg-chat-1"}, "email": "a@b.c", "amr": []string{"hwk"},
			})
			c, err := v.Verify(context.Background(), tok, UseAssert)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if c.Subject != "12345" || c.Gate != "tg" || len(c.Groups) != 1 || c.Email != "a@b.c" {
				t.Fatalf("claims = %+v", c)
			}
		})
	}
}

// RFC FA §8 "assertion forgery": an assertion signed by gate A is refused for
// gate B, whichever gate it names.
func TestVerify_RefusesAnAssertionSignedByAnotherGatesKey(t *testing.T) {
	a, b := edSigner(t), edSigner(t)
	vb := verifier("gate-b", b.pub, &memReplay{})
	for _, iss := range []string{"gate-a", "gate-b"} {
		tok := a.sign(t, claimsFor(iss, time.Now()), map[string]any{"lc_use": "assert"})
		if _, err := vb.Verify(context.Background(), tok, UseAssert); !errors.Is(err, ErrInvalid) {
			t.Fatalf("A's assertion naming %s verified for B: %v", iss, err)
		}
	}
	// And B's own key, naming A, is refused by B: iss must be the gate.
	tok := b.sign(t, claimsFor("gate-a", time.Now()), map[string]any{"lc_use": "assert"})
	if _, err := vb.Verify(context.Background(), tok, UseAssert); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an assertion naming another gate verified: %v", err)
	}
}

// RFC FA §8: a second use of a jti is refused.
func TestVerify_RefusesTheSecondUseOfAJTI(t *testing.T) {
	s := edSigner(t)
	v := verifier("tg", s.pub, &memReplay{})
	tok := s.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "assert"})
	if _, err := v.Verify(context.Background(), tok, UseAssert); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), tok, UseAssert); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second use: %v", err)
	}
}

func TestVerify_RefusesALifetimeOverFiveMinutesAnExpiryAndAFutureIssue(t *testing.T) {
	s := edSigner(t)
	now := time.Now()
	cases := map[string]jwt.Claims{}
	long := claimsFor("tg", now)
	long.Expiry = jwt.NewNumericDate(now.Add(MaxLifetime + time.Second))
	cases["lifetime over 5 min"] = long
	exp := claimsFor("tg", now.Add(-4*time.Minute))
	exp.Expiry = jwt.NewNumericDate(now.Add(-time.Minute))
	cases["expired"] = exp
	fut := claimsFor("tg", now.Add(2*time.Minute))
	cases["issued in the future"] = fut
	noExp := claimsFor("tg", now)
	noExp.Expiry = nil
	cases["no exp"] = noExp
	noIat := claimsFor("tg", now)
	noIat.IssuedAt = nil
	cases["no iat"] = noIat
	noJTI := claimsFor("tg", now)
	noJTI.ID = ""
	cases["no jti"] = noJTI
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rc := &memReplay{}
			v := verifier("tg", s.pub, rc)
			if _, err := v.Verify(context.Background(), s.sign(t, c, map[string]any{"lc_use": "assert"}), UseAssert); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted: %v", err)
			}
			if rc.n != 0 {
				t.Fatal("a refused assertion reached the replay cache")
			}
		})
	}
}

// RFC FA §8: an assertion for another audience is refused, and so is one that
// names this deployment among others (it would be valid at both).
func TestVerify_RefusesAWrongOrSharedAudience(t *testing.T) {
	s := edSigner(t)
	v := verifier("tg", s.pub, &memReplay{})
	for name, aud := range map[string]jwt.Audience{
		"other":    {"https://other.example"},
		"shared":   {testAud, "https://other.example"},
		"trailing": {testAud + "/"},
		"none":     nil,
	} {
		c := claimsFor("tg", time.Now())
		c.Audience = aud
		if _, err := v.Verify(context.Background(), s.sign(t, c, map[string]any{"lc_use": "assert"}), UseAssert); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s audience accepted: %v", name, err)
		}
	}
}

// A JWT minted for one purpose is refused by every other route.
func TestVerify_RefusesAnAssertionMadeForAnotherUse(t *testing.T) {
	s := edSigner(t)
	v := verifier("tg", s.pub, &memReplay{})
	tok := s.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "assert"})
	for _, u := range []Use{UseLogin, UseRevoke, UseSync} {
		if _, err := v.Verify(context.Background(), tok, u); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an assert verified as %s: %v", u, err)
		}
	}
	none := s.sign(t, claimsFor("tg", time.Now()), map[string]any{})
	if _, err := v.Verify(context.Background(), none, UseAssert); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an assertion with no lc_use verified: %v", err)
	}
}

func TestVerify_ALoginNeedsItsStateAndNothingElseCarriesOne(t *testing.T) {
	s := edSigner(t)
	v := verifier("tg", s.pub, &memReplay{})
	if _, err := v.Verify(context.Background(), s.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "login"}), UseLogin); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a login with no login_state verified: %v", err)
	}
	c, err := v.Verify(context.Background(), s.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "login", "login_state": "st"}), UseLogin)
	if err != nil || c.LoginState != "st" {
		t.Fatalf("login: %+v %v", c, err)
	}
	if _, err := v.Verify(context.Background(), s.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "assert", "login_state": "st"}), UseAssert); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an assert carrying a login_state verified: %v", err)
	}
}

// The algorithm is the key's, never the header's: an HS256 token keyed with
// the public key's bytes (the classic alg confusion) and an unsigned token
// are both refused by an Ed25519 gate.
func TestVerify_TheKeyDecidesTheAlgorithmNotTheHeader(t *testing.T) {
	ed := edSigner(t)
	v := verifier("tg", ed.pub, &memReplay{})
	pub := []byte(ed.pub.verify.(ed25519.PublicKey))
	confused := signer{alg: jose.HS256, key: append(pub, pub...)}
	tok := confused.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "assert"})
	if _, err := v.Verify(context.Background(), tok, UseAssert); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an HS256 token verified under an Ed25519 key: %v", err)
	}
	parts := strings.Split(ed.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "assert"}), ".")
	unsigned := "eyJhbGciOiJub25lIn0." + parts[1] + "."
	if _, err := v.Verify(context.Background(), unsigned, UseAssert); !errors.Is(err, ErrInvalid) {
		t.Fatalf("alg none verified: %v", err)
	}
}

func TestVerify_AReplayCacheFailureRefusesTheAssertion(t *testing.T) {
	s := edSigner(t)
	v := verifier("tg", s.pub, &memReplay{err: errors.New("db down")})
	if _, err := v.Verify(context.Background(), s.sign(t, claimsFor("tg", time.Now()), map[string]any{"lc_use": "assert"}), UseAssert); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("verify with no replay cache: %v", err)
	}
}

// Only the gate's key can put a jti in the cache: a forged assertion is
// refused before the cache is asked, so it cannot burn a real assertion's jti.
func TestVerify_ARefusedSignatureNeverReachesTheReplayCache(t *testing.T) {
	a, b := edSigner(t), edSigner(t)
	rc := &memReplay{}
	v := verifier("tg", b.pub, rc)
	c := claimsFor("tg", time.Now())
	if _, err := v.Verify(context.Background(), a.sign(t, c, map[string]any{"lc_use": "assert"}), UseAssert); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if rc.n != 0 {
		t.Fatal("a forged assertion reached the replay cache")
	}
	if _, err := v.Verify(context.Background(), b.sign(t, c, map[string]any{"lc_use": "assert"}), UseAssert); err != nil {
		t.Fatalf("the real assertion with the same jti: %v", err)
	}
}

func TestVerify_ASyncCarriesUsersAndNoSubject(t *testing.T) {
	s := edSigner(t)
	v := verifier("tg", s.pub, &memReplay{})
	c := claimsFor("tg", time.Now())
	c.Subject = ""
	got, err := v.Verify(context.Background(), s.sign(t, c, map[string]any{"lc_use": "sync", "complete": true,
		"users": []map[string]any{{"sub": "1", "groups": []string{"a"}}, {"sub": "2"}}}), UseSync)
	if err != nil || len(got.Users) != 2 || !got.Complete {
		t.Fatalf("sync: %+v %v", got, err)
	}
	c = claimsFor("tg", time.Now())
	c.Subject = ""
	if _, err := v.Verify(context.Background(), s.sign(t, c, map[string]any{"lc_use": "sync",
		"users": []map[string]any{{"sub": "1"}, {"sub": "1"}}}), UseSync); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a duplicate user verified: %v", err)
	}
}

func TestParsePublicJWK_RefusesAPrivateKeyAndOtherCurves(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw, _ := json.Marshal(jose.JSONWebKey{Key: priv})
	if _, err := ParsePublicJWK(raw); err == nil {
		t.Fatal("a private Ed25519 JWK was accepted")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	raw, _ = json.Marshal(jose.JSONWebKey{Key: &p384.PublicKey})
	if _, err := ParsePublicJWK(raw); err == nil {
		t.Fatal("a P-384 key was accepted")
	}
	if _, err := HMACKey([]byte("short")); err == nil {
		t.Fatal("a short HMAC secret was accepted")
	}
}
