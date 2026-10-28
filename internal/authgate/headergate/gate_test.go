package headergate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

var _ authgate.Gate = (*Gate)(nil)

func TestGate_CloudflareAssertionFromTheProxyBecomesAnAssertion(t *testing.T) {
	k := rsaKey(t)
	cf := cfGate(t, pinned("k1", k))
	g := NewCloudflareGate(cf, proxyNet)
	g.now = fixedNow
	r := proxyRequest("10.0.0.9")
	r.Header.Set(CloudflareHeader, signJWT(t, jose.RS256, k, "k1", cfClaims()))
	a, err := g.Verify(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject != "u-123" || a.Issuer != "https://"+cfTeam || a.Email != "alice@acme.com" ||
		!a.AuthTime.Equal(t0) || !a.ExpiresAt.Equal(t0.Add(time.Hour)) || a.Claims["aud"] == nil {
		t.Fatalf("assertion %+v", a)
	}
	if g.Kind() != Kind || g.Preset() != PresetCloudflareAccess {
		t.Fatal("kind/preset")
	}
	if err := g.Begin(context.Background(), nil, r, authgate.LoginState{}); !errors.Is(err, authgate.ErrNotInteractive) {
		t.Fatalf("Begin = %v", err)
	}
}

func TestGate_ValidAssertionFromOutsideTheCIDRsIsRefused(t *testing.T) {
	k := rsaKey(t)
	g := NewCloudflareGate(cfGate(t, pinned("k1", k)), proxyNet)
	r := proxyRequest("203.0.113.50")
	r.Header.Set(CloudflareHeader, signJWT(t, jose.RS256, k, "k1", cfClaims()))
	if _, err := g.Verify(context.Background(), r); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify from outside the CIDRs = %v, want ErrInvalidCredential", err)
	}
}

func TestGate_NoHeaderIsNoCredential(t *testing.T) {
	g := NewForwardAuthGate(hmacGate(), proxyNet)
	if _, err := g.Verify(context.Background(), proxyRequest("10.0.0.9")); !errors.Is(err, authgate.ErrNoCredential) {
		t.Fatalf("Verify = %v, want ErrNoCredential", err)
	}
}

func TestGate_ForwardAuthCarriesItsGroupsAndTenantHint(t *testing.T) {
	g := NewForwardAuthGate(hmacGate(), proxyNet)
	r := proxyRequest("10.0.0.9")
	r.Header.Set(IdentityHeader, testIdentity)
	r.Header.Set(IdentitySignatureHeader, SignForwardAuth(hmacSecret, testIdentity, t0, "nonce-0000000000001"))
	a, err := g.Verify(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject != "alice" || a.TenantHint != "acme" || len(a.Groups) != 2 || a.Issuer != "" {
		t.Fatalf("assertion %+v", a)
	}
	if hs := g.Headers(); len(hs) != 2 {
		t.Fatalf("headers %v", hs)
	}
}

func TestGate_ALBAliasHeaderIsIgnored(t *testing.T) {
	s := newALBServer(t)
	k := s.addKey(t, "kid-1")
	g := NewALBGate(albGate(s.keySet()), proxyNet)
	r := proxyRequest("10.0.0.9")
	r.Header["X_Amzn_Oidc_Data"] = []string{albToken(t, k, albHeaderClaims("kid-1"), albPayload(), true)}
	if _, err := g.Verify(context.Background(), r); !errors.Is(err, authgate.ErrNoCredential) {
		t.Fatalf("Verify with only an alias = %v, want ErrNoCredential", err)
	}
	r.Header.Set(ALBHeader, albToken(t, k, albHeaderClaims("kid-1"), albPayload(), true))
	if a, err := g.Verify(context.Background(), r); err != nil || a.Subject != "idp-user-9" {
		t.Fatalf("Verify = %+v, %v", a, err)
	}
}
