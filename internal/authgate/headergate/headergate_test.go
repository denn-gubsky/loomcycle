package headergate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/replay"
)

var t0 = time.Unix(1_800_000_000, 0)

func fixedNow() time.Time { return t0 }

// ---- the request filter ----

func proxyRequest(peer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/_me", nil)
	r.RemoteAddr = peer + ":40000"
	return r
}

var proxyNet = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}

func TestSource_HeaderFromOutsideTheTrustedCIDRsIsRefusedAndRemoved(t *testing.T) {
	r := proxyRequest("192.0.2.7")
	r.Header.Set(CloudflareHeader, "eyJ.valid.looking")
	_, err := Source{Headers: []string{CloudflareHeader}, Trusted: proxyNet}.Take(r)
	if !errors.Is(err, ErrUntrustedPeer) || !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Take from an untrusted peer = %v, want ErrUntrustedPeer", err)
	}
	if r.Header.Get(CloudflareHeader) != "" {
		t.Fatal("the untrusted header is still on the request for something later to read")
	}
}

func TestSource_NoTrustedCIDRsTrustsNobody(t *testing.T) {
	r := proxyRequest("10.0.0.5")
	r.Header.Set(CloudflareHeader, "x")
	if _, err := (Source{Headers: []string{CloudflareHeader}}).Take(r); !errors.Is(err, ErrUntrustedPeer) {
		t.Fatalf("Take with no trusted CIDRs = %v, want ErrUntrustedPeer", err)
	}
}

func TestSource_AliasHeadersAreStrippedAndNeverRead(t *testing.T) {
	for _, alias := range []string{
		"X_Loomcycle_Identity", "X.Loomcycle.Identity", "x_loomcycle-identity",
		"X-LOOMCYCLE_IDENTITY", "XLoomcycleIdentity", "X~Loomcycle~Identity",
	} {
		t.Run(alias, func(t *testing.T) {
			r := proxyRequest("10.0.0.5")
			// Set by key, bypassing canonicalisation, as Go's server stores
			// a name it cannot canonicalise.
			r.Header[alias] = []string{`{"sub":"mallory"}`}
			r.Header[IdentitySignatureHeader+"_"] = []string{"t=1,n=x,v1=00"}
			_, err := Source{Headers: []string{IdentityHeader, IdentitySignatureHeader}, Trusted: proxyNet}.Take(r)
			if !errors.Is(err, authgate.ErrNoCredential) {
				t.Fatalf("Take with only alias headers = %v, want ErrNoCredential (aliases ignored)", err)
			}
			if _, still := r.Header[alias]; still {
				t.Fatalf("alias %q is still on the request", alias)
			}
		})
	}
}

func TestSource_AliasBesideTheRealHeaderIsStrippedAndTheRealOneRead(t *testing.T) {
	r := proxyRequest("10.0.0.5")
	r.Header.Set(IdentityHeader, `{"sub":"alice"}`)
	r.Header["X_Loomcycle_Identity"] = []string{`{"sub":"mallory"}`}
	got, err := Source{Headers: []string{IdentityHeader}, Trusted: proxyNet}.Take(r)
	if err != nil {
		t.Fatal(err)
	}
	if got[IdentityHeader] != `{"sub":"alice"}` {
		t.Fatalf("read %q, want the canonical header's value", got[IdentityHeader])
	}
	if len(r.Header) != 1 {
		t.Fatalf("headers left: %v", r.Header)
	}
}

func TestSource_RepeatedHeaderIsRefused(t *testing.T) {
	r := proxyRequest("10.0.0.5")
	r.Header.Add(CloudflareHeader, "a")
	r.Header.Add(CloudflareHeader, "b")
	if _, err := (Source{Headers: []string{CloudflareHeader}, Trusted: proxyNet}).Take(r); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Take with a repeated header = %v, want ErrInvalidCredential", err)
	}
}

func TestSource_V4InV6PeerMatchesAV4CIDR(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[::ffff:10.0.0.5]:40000"
	r.Header.Set(CloudflareHeader, "x")
	if _, err := (Source{Headers: []string{CloudflareHeader}, Trusted: proxyNet}).Take(r); err != nil {
		t.Fatalf("v4-in-v6 peer inside the CIDR: %v", err)
	}
}

// ---- AWS ALB ----

const testSigner = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/my-alb/50dc6c495c0c9188"

type albServer struct {
	srv     *httptest.Server
	keys    map[string]*ecdsa.PrivateKey
	fetches atomic.Int32
}

func newALBServer(t *testing.T) *albServer {
	t.Helper()
	s := &albServer{keys: map[string]*ecdsa.PrivateKey{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fetches.Add(1)
		kid := strings.TrimPrefix(r.URL.Path, "/eu-west-1/")
		k, ok := s.keys[kid]
		if !ok {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		_ = pem.Encode(w, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *albServer) addKey(t *testing.T, kid string) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.keys[kid] = k
	return k
}

func (s *albServer) keySet() *ALBKeys {
	return &ALBKeys{Region: "eu-west-1", Now: fixedNow, keyURL: func(region, kid string) string {
		return s.srv.URL + "/" + region + "/" + kid
	}}
}

// albToken signs like an ALB: standard base64url WITH padding when pad.
func albToken(t *testing.T, key *ecdsa.PrivateKey, header, payload map[string]any, pad bool) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	if pad {
		enc = base64.URLEncoding.EncodeToString
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	input := enc(hb) + "." + enc(pb)
	d := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + enc(sig)
}

func albHeaderClaims(kid string) map[string]any {
	return map[string]any{
		"alg": "ES256", "kid": kid, "signer": testSigner, "iss": "https://idp.acme.com",
		"client": "client-1", "exp": t0.Add(2 * time.Minute).Unix(),
	}
}

func albPayload() map[string]any {
	return map[string]any{"sub": "idp-user-9", "email": "bob@acme.com", "email_verified": "true", "exp": t0.Add(2 * time.Minute).Unix()}
}

func albGate(keys *ALBKeys) *ALB {
	return &ALB{SignerARN: testSigner, Issuer: "https://idp.acme.com", ClientID: "client-1", Keys: keys, Now: fixedNow}
}

func TestALB_ValidPaddedTokenVerifies(t *testing.T) {
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	for _, pad := range []bool{true, false} {
		id, err := albGate(s.keySet()).Verify(context.Background(), albToken(t, k, albHeaderClaims("kid-1"), albPayload(), pad))
		if err != nil {
			t.Fatalf("pad=%v: %v", pad, err)
		}
		if id.Subject != "idp-user-9" || id.Email != "bob@acme.com" || !id.EmailVerified || id.Issuer != "https://idp.acme.com" {
			t.Fatalf("identity %+v", id)
		}
	}
}

func TestALB_TokenFromAnotherLoadBalancerIsRefusedWithoutAKeyFetch(t *testing.T) {
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	h := albHeaderClaims("kid-1")
	// Same region, same key endpoint, a valid signature — another account's
	// ALB. This is the ALBeast bypass.
	h["signer"] = "arn:aws:elasticloadbalancing:eu-west-1:999999999999:loadbalancer/app/evil/0123456789abcdef"
	_, err := albGate(s.keySet()).Verify(context.Background(), albToken(t, k, h, albPayload(), true))
	if !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
	}
	if s.fetches.Load() != 0 {
		t.Fatalf("a token for another ALB caused %d key fetches", s.fetches.Load())
	}
}

func TestALB_RefusesBadTokens(t *testing.T) {
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cases := map[string]func() string{
		"HS256": func() string {
			h := albHeaderClaims("kid-1")
			h["alg"] = "HS256"
			return albToken(t, k, h, albPayload(), true)
		},
		"alg none": func() string {
			h := albHeaderClaims("kid-1")
			h["alg"] = "none"
			return albToken(t, k, h, albPayload(), true)
		},
		"expired": func() string {
			h := albHeaderClaims("kid-1")
			h["exp"] = t0.Add(-5 * time.Minute).Unix()
			return albToken(t, k, h, albPayload(), true)
		},
		"payload expired": func() string {
			p := albPayload()
			p["exp"] = t0.Add(-5 * time.Minute).Unix()
			return albToken(t, k, albHeaderClaims("kid-1"), p, true)
		},
		"wrong issuer": func() string {
			h := albHeaderClaims("kid-1")
			h["iss"] = "https://evil.example"
			return albToken(t, k, h, albPayload(), true)
		},
		"wrong client": func() string {
			h := albHeaderClaims("kid-1")
			h["client"] = "client-2"
			return albToken(t, k, h, albPayload(), true)
		},
		"signed by another key": func() string { return albToken(t, other, albHeaderClaims("kid-1"), albPayload(), true) },
		"tampered payload": func() string {
			tok := albToken(t, k, albHeaderClaims("kid-1"), albPayload(), false)
			parts := strings.Split(tok, ".")
			p := albPayload()
			p["sub"] = "admin"
			pb, _ := json.Marshal(p)
			return parts[0] + "." + base64.RawURLEncoding.EncodeToString(pb) + "." + parts[2]
		},
		"path in kid": func() string { return albToken(t, k, albHeaderClaims("../kid-1"), albPayload(), true) },
		"no sub": func() string {
			p := albPayload()
			delete(p, "sub")
			return albToken(t, k, albHeaderClaims("kid-1"), p, true)
		},
		"two segments": func() string { return "a.b" },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := albGate(s.keySet()).Verify(context.Background(), mk()); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
			}
		})
	}
}

func TestALB_NewKidVerifiesAfterOneFetchAndIsCached(t *testing.T) {
	s := newALBServer(t)
	keys := s.keySet()
	k := s.addKey(t, "kid-rotated")
	for i := 0; i < 5; i++ {
		if _, err := albGate(keys).Verify(context.Background(), albToken(t, k, albHeaderClaims("kid-rotated"), albPayload(), true)); err != nil {
			t.Fatal(err)
		}
	}
	if s.fetches.Load() != 1 {
		t.Fatalf("%d fetches for one kid, want 1", s.fetches.Load())
	}
}

func TestALB_UnknownKidStormIsRateLimited(t *testing.T) {
	s := newALBServer(t)
	keys := s.keySet()
	k := s.addKey(t, "kid-1")
	for i := 0; i < 50; i++ {
		tok := albToken(t, k, albHeaderClaims(fmt.Sprintf("random-%d", i)), albPayload(), true)
		// Refused either way: as invalid once AWS said "no such key", or as
		// a fault (judged nothing) once the fetch budget is spent.
		if _, err := albGate(keys).Verify(context.Background(), tok); err == nil {
			t.Fatalf("unknown kid %d verified", i)
		}
	}
	if n := s.fetches.Load(); n > 3 {
		t.Fatalf("50 unknown kids caused %d fetches; the budget is a burst of 3", n)
	}
	// A kid the endpoint did not know is not asked about again.
	before := s.fetches.Load()
	tok := albToken(t, k, albHeaderClaims("random-0"), albPayload(), true)
	_, _ = albGate(keys).Verify(context.Background(), tok)
	if s.fetches.Load() != before {
		t.Fatal("a kid already answered 404 was fetched again")
	}
}

func TestALBKeys_FailedFetchDoesNotMarkTheKidUnknown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	keys := s.keySet()
	inner := keys.keyURL
	keys.keyURL = func(region, kid string) string {
		if fail.Load() {
			return "http://127.0.0.1:1/unreachable"
		}
		return inner(region, kid)
	}
	tok := albToken(t, k, albHeaderClaims("kid-1"), albPayload(), true)
	if _, err := albGate(keys).Verify(context.Background(), tok); err == nil {
		t.Fatal("verified with the key endpoint down")
	}
	fail.Store(false)
	if _, err := albGate(keys).Verify(context.Background(), tok); err != nil {
		t.Fatalf("after the endpoint recovered: %v", err)
	}
}

func TestALBRegionFromSigner(t *testing.T) {
	if got := ALBRegionFromSigner(testSigner); got != "eu-west-1" {
		t.Fatalf("region %q", got)
	}
	for _, bad := range []string{"", "arn:aws:elasticloadbalancing:eu-west-1:123:loadbalancer/app/x/1", "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/net/x/1"} {
		if ALBRegionFromSigner(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ---- forward-auth HMAC ----

var hmacSecret = []byte("0123456789abcdef0123456789abcdef")

func hmacGate() *ForwardAuthHMAC {
	return &ForwardAuthHMAC{
		Secret: func(context.Context) ([]byte, error) { return hmacSecret, nil },
		Replay: replay.New(0), Now: fixedNow,
	}
}

const testIdentity = `{"sub":"alice","email":"alice@acme.com","groups":["eng","ops"],"tenant":"acme"}`

func TestForwardAuth_ValidHeaderNamesTheUser(t *testing.T) {
	id, err := hmacGate().Verify(context.Background(), testIdentity, SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "alice" || id.Tenant != "acme" || len(id.Groups) != 2 {
		t.Fatalf("identity %+v", id)
	}
}

func TestForwardAuth_ReplayedNonceIsRefused(t *testing.T) {
	g := hmacGate()
	sig := SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001")
	if _, err := g.Verify(context.Background(), testIdentity, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Verify(context.Background(), testIdentity, sig); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replay = %v, want ErrInvalidCredential", err)
	}
}

func TestForwardAuth_StaleOrFutureTimestampIsRefused(t *testing.T) {
	for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
		sig := SignForwardAuth(hmacSecret, testIdentity, t0.Add(d), "nonce-0000000000001")
		if _, err := hmacGate().Verify(context.Background(), testIdentity, sig); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("timestamp %s off: %v, want ErrInvalidCredential", d, err)
		}
	}
}

func TestForwardAuth_TamperedOrWronglyKeyedHeaderIsRefusedAndNotRemembered(t *testing.T) {
	g := hmacGate()
	sig := SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001")
	tampered := strings.Replace(testIdentity, "alice", "admin", 1)
	if _, err := g.Verify(context.Background(), tampered, sig); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("tampered identity: %v", err)
	}
	wrongKey := SignForwardAuth([]byte("another-secret-another-secret!!!"), testIdentity, t0, "nonce-0000000000002")
	if _, err := g.Verify(context.Background(), testIdentity, wrongKey); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("wrong key: %v", err)
	}
	if g.Replay.Len() != 0 {
		t.Fatal("an unauthenticated header reached the replay cache")
	}
	// The real header with nonce 1 is still usable: the forgery did not burn it.
	if _, err := g.Verify(context.Background(), testIdentity, sig); err != nil {
		t.Fatalf("genuine header after a forgery: %v", err)
	}
}

func TestForwardAuth_MalformedHeadersAreRefused(t *testing.T) {
	good := SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001")
	for name, sig := range map[string]string{
		"empty":          "",
		"no nonce":       strings.Replace(good, ",n=nonce-0000000000001", "", 1),
		"short nonce":    SignForwardAuth(hmacSecret, testIdentity, t0, "short"),
		"duplicate key":  good + ",t=1",
		"extra key":      good + ",x=1",
		"not hex":        "t=1800000000,n=nonce-0000000000001,v1=zz",
		"negative t":     "t=-1,n=nonce-0000000000001,v1=" + strings.Repeat("0", 64),
		"truncated hmac": good[:len(good)-2],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hmacGate().Verify(context.Background(), testIdentity, sig); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
			}
		})
	}
	noSub := `{"email":"x@y"}`
	if _, err := hmacGate().Verify(context.Background(), noSub, SignForwardAuth(hmacSecret, noSub, t0, "nonce-0000000000001")); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("identity without sub: %v", err)
	}
}

func TestForwardAuth_UnavailableSecretFailsClosed(t *testing.T) {
	g := hmacGate()
	g.Secret = func(context.Context) ([]byte, error) { return nil, errors.New("store down") }
	if _, err := g.Verify(context.Background(), testIdentity, SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001")); err == nil {
		t.Fatal("verified with no secret")
	}
}

func TestForwardAuth_HeaderAtTheWindowEdgeCannotBeReplayedLater(t *testing.T) {
	g := hmacGate()
	stamped := t0.Add(-DefaultHMACWindow + time.Second)
	sig := SignForwardAuth(hmacSecret, testIdentity, stamped, "nonce-0000000000edge")
	if _, err := g.Verify(context.Background(), testIdentity, sig); err != nil {
		t.Fatal(err)
	}
	last := stamped.Add(DefaultHMACWindow - time.Nanosecond)
	g.Now = func() time.Time { return last }
	if _, err := g.Verify(context.Background(), testIdentity, sig); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replay at the last acceptable moment: %v, want ErrInvalidCredential", err)
	}
	edge := SignForwardAuth(hmacSecret, testIdentity, t0.Add(-DefaultHMACWindow), "nonce-00000000000edge2")
	if _, err := hmacGate().Verify(context.Background(), testIdentity, edge); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("a header stamped exactly one window ago: %v, want ErrInvalidCredential", err)
	}
}

// sharedRecord stands in for the store's shared replay record.
type sharedRecord struct {
	mu   sync.Mutex
	seen map[string]time.Time
	err  error
}

func (s *sharedRecord) UseOnce(_ context.Context, scope, key string, expires, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	if until, ok := s.seen[scope+"/"+key]; ok && now.Before(until) {
		return false, nil
	}
	s.seen[scope+"/"+key] = expires
	return true, nil
}

func TestForwardAuth_HeaderReplayedToAnotherReplicaIsRefused(t *testing.T) {
	shared := &sharedRecord{}
	a, b := hmacGate(), hmacGate() // two replicas: their own caches, one record
	a.Shared, a.SharedScope = shared, "fwdhmac:g"
	b.Shared, b.SharedScope = shared, "fwdhmac:g"
	const n = "nonce-00000000shared"
	sig := SignForwardAuth(hmacSecret, testIdentity, t0, n)
	if _, err := a.Verify(context.Background(), testIdentity, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(context.Background(), testIdentity, sig); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replay to the second replica = %v, want ErrInvalidCredential", err)
	}
	for k := range shared.seen {
		if strings.Contains(k, n) {
			t.Fatal("the shared record holds the nonce itself, not its hash")
		}
	}
}

func TestForwardAuth_UnwritableSharedRecordRefusesTheHeader(t *testing.T) {
	g := hmacGate()
	g.Shared, g.SharedScope = &sharedRecord{err: errors.New("db down")}, "fwdhmac:g"
	_, err := g.Verify(context.Background(), testIdentity, SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000down"))
	if err == nil || errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify with the record down = %v, want a non-credential refusal", err)
	}
}
