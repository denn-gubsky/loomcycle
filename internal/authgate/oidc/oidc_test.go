package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/codeflow"
	"github.com/denn-gubsky/loomcycle/internal/authgate/oidc/oidctest"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

const redirectURI = "https://lc.example/v1/_auth/complete/idp"

func generic(p *oidctest.Provider) config.AuthOIDCGate {
	return config.AuthOIDCGate{Preset: config.OIDCPresetGeneric, ClientID: p.ClientID, ClientSecret: p.ClientSecret,
		Issuer: p.Issuer, DiscoveryURL: p.Server.URL + "/.well-known/openid-configuration",
		Scopes: []string{"openid", "email"}, RedirectURI: redirectURI}
}

func newProvider(t *testing.T) *oidctest.Provider {
	t.Helper()
	p := oidctest.New("client-1", "secret-1")
	t.Cleanup(p.Close)
	p.Claims = map[string]any{"sub": "u-1", "email": "a@acme.example", "email_verified": true, "name": "Alice"}
	return p
}

// login runs Begin, follows the provider's authorize redirect without
// following the callback, and returns the callback request and login state.
func login(t *testing.T, g *Gate, reauth bool) (*http.Request, authgate.LoginState) {
	t.Helper()
	l, err := codeflow.NewLogin()
	if err != nil {
		t.Fatal(err)
	}
	ls := codeflow.State(l.Secret, l.State, redirectURI, reauth, time.Now())
	rec := httptest.NewRecorder()
	if err := g.Begin(context.Background(), rec, httptest.NewRequest(http.MethodGet, "/v1/_auth/login/idp", nil), ls); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("begin = %d", rec.Code)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(cb.String(), redirectURI) {
		t.Fatalf("callback = %q (%d)", resp.Header.Get("Location"), resp.StatusCode)
	}
	return httptest.NewRequest(http.MethodGet, "/v1/_auth/complete/idp?"+cb.RawQuery, nil), ls
}

func TestComplete_AssertsTheVerifiedIdentity(t *testing.T) {
	p := newProvider(t)
	p.Claims["amr"] = []string{"pwd", "mfa", "bad value!"}
	g := New("idp", generic(p), p.Server.Client())
	r, ls := login(t, g, false)
	a, err := g.Complete(context.Background(), r, ls)
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject != "u-1" || a.Issuer != p.Issuer || a.Email != "a@acme.example" || !a.EmailVerified || a.DisplayName != "Alice" {
		t.Fatalf("assertion = %+v", a)
	}
	if strings.Join(a.AMR, ",") != "pwd,mfa" || a.AuthTime.IsZero() || a.Claims["sub"] != "u-1" {
		t.Fatalf("amr/auth_time/claims = %v %v %v", a.AMR, a.AuthTime, a.Claims)
	}
	q := p.LastAuthorize
	if q.Get("redirect_uri") != redirectURI || q.Get("code_challenge_method") != "S256" || q.Get("nonce") != ls.Nonce {
		t.Fatalf("authorize = %v", q)
	}
}

func TestComplete_NoAMRMeansOIDC(t *testing.T) {
	p := newProvider(t)
	g := New("idp", generic(p), p.Server.Client())
	r, ls := login(t, g, false)
	a, err := g.Complete(context.Background(), r, ls)
	if err != nil || len(a.AMR) != 1 || a.AMR[0] != "oidc" {
		t.Fatalf("amr = %v %v", a.AMR, err)
	}
}

// RFC FA §8 OIDC replay / code injection: a replayed code, a mismatched
// state and a mismatched nonce are refused.
func TestComplete_RefusesReplayedCodeAndMismatchedStateOrNonce(t *testing.T) {
	p := newProvider(t)
	g := New("idp", generic(p), p.Server.Client())
	r, ls := login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replayed code: %v", err)
	}
	r, ls = login(t, g, false)
	other := ls
	other.State = "another-state"
	if _, err := g.Complete(context.Background(), r, other); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("state mismatch: %v", err)
	}
	r, ls = login(t, g, false)
	ls.Nonce = "another-nonce"
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("nonce mismatch: %v", err)
	}
	// A verifier that does not match the challenge (a code stolen into
	// another browser's login) fails at the token endpoint.
	r, ls = login(t, g, false)
	ls.CodeVerifier = codeflow.Verifier([]byte("0123456789abcdef0123456789abcdef"), ls.State)
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("PKCE mismatch: %v", err)
	}
}

// RFC FA §8: wrong aud, wrong iss, expired, alg none and HS256 keyed by the
// RSA public key are refused.
func TestComplete_RefusesForgedIDTokens(t *testing.T) {
	cases := map[string]func(p *oidctest.Provider){
		"wrong aud": func(p *oidctest.Provider) { p.Mutate = func(c map[string]any) { c["aud"] = "other-client" } },
		"aud list without azp": func(p *oidctest.Provider) {
			p.Mutate = func(c map[string]any) { c["aud"] = []string{"client-1", "other"} }
		},
		"foreign azp": func(p *oidctest.Provider) { p.Mutate = func(c map[string]any) { c["azp"] = "other" } },
		"wrong iss":   func(p *oidctest.Provider) { p.Mutate = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"expired": func(p *oidctest.Provider) {
			p.Mutate = func(c map[string]any) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }
		},
		"old iat": func(p *oidctest.Provider) {
			p.Mutate = func(c map[string]any) { c["iat"] = time.Now().Add(-20 * time.Minute).Unix() }
		},
		"no sub": func(p *oidctest.Provider) { p.Mutate = func(c map[string]any) { delete(c, "sub") } },
		"alg none": func(p *oidctest.Provider) {
			p.IDToken = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1LTEifQ."
		},
		"HS256 with the RSA key": func(p *oidctest.Provider) {
			p.IDToken = hs256WithPublicKey(p)
		},
		"unknown signer": func(p *oidctest.Provider) { p.Signing = oidctest.NewRSAKey("k1") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t)
			p.Set(setup)
			g := New("idp", generic(p), p.Server.Client())
			r, ls := login(t, g, false)
			if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// RFC FA §8 JWKS rotation, end to end: the provider starts signing with a new
// key; the next login verifies after one key set refresh.
func TestComplete_KeyRotationVerifiesAfterOneRefresh(t *testing.T) {
	p := newProvider(t)
	clock := time.Now()
	g := New("idp", generic(p), p.Server.Client())
	g.SetClock(func() time.Time { return clock })
	r, ls := login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); err != nil {
		t.Fatal(err)
	}
	k2 := oidctest.NewECKey("k2")
	p.Set(func(p *oidctest.Provider) { p.Keys = append(p.Keys, k2); p.Signing = k2 })
	clock = clock.Add(2 * time.Minute)
	before := p.JWKSHits
	r, ls = login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if p.JWKSHits != before+1 {
		t.Fatalf("key set fetched %d times for the rotation", p.JWKSHits-before)
	}
}

func googleGate(p *oidctest.Provider) *Gate {
	p.Set(func(p *oidctest.Provider) { p.Issuer = config.GoogleIssuer })
	cfg := generic(p)
	cfg.Preset, cfg.Issuer, cfg.AllowedDomains = config.OIDCPresetGoogle, config.GoogleIssuer, []string{"acme.com"}
	return New("google", cfg, p.Server.Client())
}

// RFC FA §8: Google's hd AND email_verified are enforced; the e-mail's own
// domain is not evidence of Workspace membership.
func TestComplete_GoogleRequiresHDAndVerifiedEmail(t *testing.T) {
	cases := map[string]map[string]any{
		"no hd, workspace-looking e-mail": {"email": "a@acme.com", "email_verified": true},
		"other hd":                        {"hd": "evil.com", "email": "a@acme.com", "email_verified": true},
		"unverified e-mail":               {"hd": "acme.com", "email": "a@acme.com", "email_verified": false},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t)
			p.Claims = map[string]any{"sub": "g-1"}
			for k, v := range extra {
				p.Claims[k] = v
			}
			g := googleGate(p)
			r, ls := login(t, g, false)
			if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	p := newProvider(t)
	p.Claims = map[string]any{"sub": "g-1", "hd": "acme.com", "email": "a@acme.com", "email_verified": true}
	g := googleGate(p)
	r, ls := login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); err != nil {
		t.Fatalf("good google login: %v", err)
	}
	if p.LastAuthorize.Get("hd") != "acme.com" {
		t.Fatalf("hd hint missing: %v", p.LastAuthorize)
	}
}

// RFC FA §8: Entra's tid is checked against the allowlist, and with several
// tenants the issuer must be the token's own tenant's.
func TestComplete_EntraEnforcesTIDAndTemplatedIssuer(t *testing.T) {
	const t1, t2 = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	setup := func(t *testing.T, tid, iss string) (*oidctest.Provider, *Gate) {
		p := newProvider(t)
		p.Set(func(p *oidctest.Provider) {
			p.Issuer = config.EntraAuthority + "/{tenantid}/v2.0"
			p.Claims = map[string]any{"sub": "e-1", "tid": tid}
			p.Mutate = func(c map[string]any) { c["iss"] = iss }
		})
		cfg := generic(p)
		cfg.Preset, cfg.Issuer, cfg.IssuerTemplate, cfg.AllowedTIDs = config.OIDCPresetEntra, config.EntraAuthority+"/{tenantid}/v2.0", true, []string{t1}
		return p, New("entra", cfg, p.Server.Client())
	}
	_, g := setup(t, t1, config.EntraAuthority+"/"+t1+"/v2.0")
	r, ls := login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); err != nil {
		t.Fatalf("allowed tenant: %v", err)
	}
	_, g = setup(t, t2, config.EntraAuthority+"/"+t2+"/v2.0")
	r, ls = login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("unlisted tenant: %v", err)
	}
	_, g = setup(t, t1, config.EntraAuthority+"/"+t2+"/v2.0")
	r, ls = login(t, g, false)
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("issuer of another tenant: %v", err)
	}
}

// A reauth login needs the provider's auth_time from this login.
func TestComplete_ReauthNeedsAFreshAuthTime(t *testing.T) {
	p := newProvider(t)
	g := New("idp", generic(p), p.Server.Client())
	r, ls := login(t, g, true)
	if p.LastAuthorize.Get("prompt") != "login" || p.LastAuthorize.Get("max_age") != "0" {
		t.Fatalf("reauth params: %v", p.LastAuthorize)
	}
	if _, err := g.Complete(context.Background(), r, ls); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	p.Set(func(p *oidctest.Provider) {
		p.Mutate = func(c map[string]any) { c["auth_time"] = time.Now().Add(-2 * time.Hour).Unix() }
	})
	r, ls = login(t, g, true)
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("stale auth_time: %v", err)
	}
	p.Set(func(p *oidctest.Provider) { p.Mutate = func(c map[string]any) { delete(c, "auth_time") } })
	r, ls = login(t, g, true)
	if _, err := g.Complete(context.Background(), r, ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("no auth_time: %v", err)
	}
}

func TestDiscovery_RefusesAnotherIssuersDocument(t *testing.T) {
	p := newProvider(t)
	cfg := generic(p)
	cfg.Issuer = "https://someone-else.example"
	g := New("idp", cfg, p.Server.Client())
	l, _ := codeflow.NewLogin()
	err := g.Begin(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil),
		codeflow.State(l.Secret, l.State, redirectURI, false, time.Now()))
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("err = %v", err)
	}
}

func TestEndSessionURL(t *testing.T) {
	p := newProvider(t)
	cfg := generic(p)
	if New("idp", cfg, p.Server.Client()).EndSessionURL(context.Background(), "https://lc.example/ui/login") != "" {
		t.Fatal("end session without end_session: true")
	}
	cfg.EndSession = true
	u := New("idp", cfg, p.Server.Client()).EndSessionURL(context.Background(), "https://lc.example/ui/login")
	if !strings.HasPrefix(u, p.Server.URL+"/logout?") || !strings.Contains(u, "client_id=client-1") ||
		!strings.Contains(u, "post_logout_redirect_uri=") {
		t.Fatalf("url = %q", u)
	}
}
