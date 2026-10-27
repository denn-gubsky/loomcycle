package headergate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-jose/go-jose/v4"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/jwtverify"
	"github.com/denn-gubsky/loomcycle/internal/authgate/replay"
)

// A verifier that cannot check a credential (its key source is down, its
// replay record is full) must not say the credential is invalid: the server
// ends a header-gate session on an invalid assertion, so an outage would log
// every user out, and it logs only non-credential errors. These pin that a
// fault is an error that does NOT wrap ErrInvalidCredential.

func TestCloudflare_KeySetOutageIsAFaultNotABadCredential(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	k := rsaKey(t)
	g := cfGate(t, &jwtverify.RemoteJWKS{URL: down.URL, Now: fixedNow})
	_, err := g.Verify(context.Background(), signJWT(t, jose.RS256, k, "kid-1", cfClaims()))
	if err == nil || errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify with the JWKS down = %v, want a non-credential error", err)
	}
}

func TestALB_KeyEndpointOutageIsAFaultNotABadCredential(t *testing.T) {
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	keys := s.keySet()
	keys.keyURL = func(string, string) string { return "http://127.0.0.1:1/unreachable" }
	_, err := albGate(keys).Verify(context.Background(), albToken(t, k, albHeaderClaims("kid-1"), albPayload(), true))
	if err == nil || errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify with the key endpoint down = %v, want a non-credential error", err)
	}
}

func TestALB_KidTheEndpointDoesNotKnowIsABadCredential(t *testing.T) {
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	_, err := albGate(s.keySet()).Verify(context.Background(), albToken(t, k, albHeaderClaims("kid-2"), albPayload(), true))
	if !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify with an unknown kid = %v, want ErrInvalidCredential", err)
	}
}

func TestForwardAuth_FullReplayCacheIsAFaultNotABadCredential(t *testing.T) {
	g := hmacGate()
	g.Replay = replay.New(1)
	if _, err := g.Verify(context.Background(), testIdentity, SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001")); err != nil {
		t.Fatal(err)
	}
	_, err := g.Verify(context.Background(), testIdentity, SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000002"))
	if err == nil || errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify with the replay cache full = %v, want a non-credential error", err)
	}
}
