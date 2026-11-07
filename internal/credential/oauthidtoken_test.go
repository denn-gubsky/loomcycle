package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
)

// idTokenHarness is a harness whose fake server advertises no userinfo
// endpoint and adds an id_token made as mode says (edited by claims) to
// every grant, stamped from the harness clock.
func idTokenHarness(t *testing.T, mode string, claims func(c map[string]any)) *oauthHarness {
	t.Helper()
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) {
		f.IDToken = mode
		f.IDTokenClaims = claims
		f.Now = h.clock.now
		f.GrantOpenID = true
		f.MetaOverride = func(m map[string]any) { delete(m, "userinfo_endpoint") }
	})
	return h
}

// connectedAs connects id and returns its status's connected_as, failing
// the test when the connect carries an error.
func (h *oauthHarness) connectedAs(t *testing.T, id Identity) string {
	t.Helper()
	h.connect(t, id)
	m, err := h.e.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if m.OAuth.Status.LastError != "" {
		t.Errorf("last_error %q, want none", m.OAuth.Status.LastError)
	}
	return m.OAuth.Status.ConnectedAs
}

func TestOAuthConnected_ConnectedAsFromVerifiedIDToken(t *testing.T) {
	h := idTokenHarness(t, oauthtest.IDTokenSigned, nil)
	if got := h.connectedAs(t, oauthID("docs")); got != "idtoken@example.test" {
		t.Errorf("connected_as %q, want the id_token's email", got)
	}
	if h.f.Count("jwks") != 1 {
		t.Errorf("jwks fetched %d times, want 1", h.f.Count("jwks"))
	}
}

func TestOAuthConnected_IDTokenUnverifiedEmailFallsToUsernameThenSub(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims func(c map[string]any)
		want   string
	}{
		{"email_verified false", func(c map[string]any) { c["email_verified"] = false }, "idtoken-user"},
		{"email_verified absent", func(c map[string]any) { delete(c, "email_verified") }, "idtoken@example.test"},
		{"no username either", func(c map[string]any) {
			c["email_verified"] = false
			delete(c, "preferred_username")
		}, "u-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := idTokenHarness(t, oauthtest.IDTokenSigned, tc.claims)
			if got := h.connectedAs(t, oauthID("docs")); got != tc.want {
				t.Errorf("connected_as %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOAuthConnected_UnverifiableIDTokenLeavesConnectedAsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		claims func(c map[string]any)
		kid    string
	}{
		{"wrong signature", oauthtest.IDTokenWrongKey, nil, ""},
		{"alg none", oauthtest.IDTokenAlgNone, nil, ""},
		{"hs256 keyed by the client_id", oauthtest.IDTokenHS256, nil, ""},
		{"wrong iss", oauthtest.IDTokenSigned, func(c map[string]any) { c["iss"] = "https://issuer.example.test" }, ""},
		{"iss with a trailing slash", oauthtest.IDTokenSigned, func(c map[string]any) { c["iss"] = c["iss"].(string) + "/" }, ""},
		{"wrong aud", oauthtest.IDTokenSigned, func(c map[string]any) { c["aud"] = "another-client" }, ""},
		{"aud array without the client", oauthtest.IDTokenSigned, func(c map[string]any) { c["aud"] = []string{"a", "b"} }, ""},
		{"foreign azp", oauthtest.IDTokenSigned, func(c map[string]any) {
			c["aud"] = []string{c["aud"].(string), "another-client"}
			c["azp"] = "another-client"
		}, ""},
		{"expired beyond the leeway", oauthtest.IDTokenSigned, func(c map[string]any) {
			c["exp"] = c["iat"].(int64) - 61
		}, ""},
		{"no exp", oauthtest.IDTokenSigned, func(c map[string]any) { delete(c, "exp") }, ""},
		{"issued in the future", oauthtest.IDTokenSigned, func(c map[string]any) {
			c["iat"] = c["iat"].(int64) + 3600
		}, ""},
		{"unknown kid", oauthtest.IDTokenSigned, nil, "rotated-away"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := idTokenHarness(t, tc.mode, tc.claims)
			h.f.Set(func(f *oauthtest.Server) { f.IDTokenKid = tc.kid })
			if got := h.connectedAs(t, oauthID("docs")); got != "" {
				t.Errorf("connected_as %q from an id_token that does not verify", got)
			}
		})
	}
}

func TestOAuthConnected_IDTokenWithinClockLeewayVerifies(t *testing.T) {
	h := idTokenHarness(t, oauthtest.IDTokenSigned, func(c map[string]any) {
		c["exp"] = c["iat"].(int64) - 30 // expired 30 s ago, inside the leeway
	})
	if got := h.connectedAs(t, oauthID("docs")); got != "idtoken@example.test" {
		t.Errorf("connected_as %q, want the id_token's email", got)
	}
}

func TestOAuthConnected_IDTokenAudienceArrayWithClientVerifies(t *testing.T) {
	h := idTokenHarness(t, oauthtest.IDTokenSigned, func(c map[string]any) {
		c["aud"] = []string{"another-client", c["aud"].(string)}
		c["azp"] = c["aud"].([]string)[1]
	})
	if got := h.connectedAs(t, oauthID("docs")); got != "idtoken@example.test" {
		t.Errorf("connected_as %q, want the id_token's email", got)
	}
}

// An unknown kid fetches the key set again (the issuer may have rotated),
// but only once per refresh interval: a stream of made-up kids is not a
// stream of requests to the issuer.
func TestOAuthConnected_UnknownKidRefetchesKeySetOnceThenGivesUp(t *testing.T) {
	h := idTokenHarness(t, oauthtest.IDTokenSigned, nil)
	if got := h.connectedAs(t, oauthID("first")); got != "idtoken@example.test" {
		t.Fatalf("connected_as %q", got)
	}
	h.clock.advance(2 * time.Minute)
	h.f.Set(func(f *oauthtest.Server) { f.IDTokenKid = "rotated-away" })
	if got := h.connectedAs(t, oauthID("second")); got != "" {
		t.Errorf("connected_as %q from an unknown kid", got)
	}
	if n := h.f.Count("jwks"); n != 2 {
		t.Errorf("jwks fetched %d times, want 2 (the first, then one re-fetch for the unknown kid)", n)
	}
	if got := h.connectedAs(t, oauthID("third")); got != "" {
		t.Errorf("connected_as %q from an unknown kid", got)
	}
	if n := h.f.Count("jwks"); n != 2 {
		t.Errorf("jwks fetched %d times, want still 2 (rate-limited)", n)
	}
}

func TestOAuthConnected_UserinfoTakesPrecedenceOverIDToken(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) {
		f.GrantOpenID = true
		f.IDToken = oauthtest.IDTokenSigned
		f.Now = h.clock.now
	})
	if got := h.connectedAs(t, oauthID("docs")); got != "someone@example.test" {
		t.Errorf("connected_as %q, want userinfo's", got)
	}
	if h.f.Count("jwks") != 0 {
		t.Error("the key set was fetched though userinfo answered")
	}
}

func TestOAuthDiscover_JWKSURIOnAnotherHostIsDropped(t *testing.T) {
	for _, jwks := range []string{"https://keys.example.test/jwks", "http://127.0.0.1:1/jwks"} {
		h := idTokenHarness(t, oauthtest.IDTokenSigned, nil)
		h.f.Set(func(f *oauthtest.Server) {
			f.MetaOverride = func(m map[string]any) {
				delete(m, "userinfo_endpoint")
				m["jwks_uri"] = jwks
			}
		})
		id := oauthID("docs")
		if got := h.connectedAs(t, id); got != "" {
			t.Errorf("%s: connected_as %q from a key set off the issuer's host", jwks, got)
		}
		if _, sec := h.secretOf(t, id); sec.Endpoints.JWKS != "" {
			t.Errorf("%s: sealed jwks %q, want it refused", jwks, sec.Endpoints.JWKS)
		}
	}
}

func TestOAuthDiscover_SealsJWKSURI(t *testing.T) {
	h := idTokenHarness(t, "", nil)
	id := oauthID("docs")
	h.connect(t, id)
	if _, sec := h.secretOf(t, id); sec.Endpoints.JWKS != h.f.Srv.URL+"/jwks" {
		t.Errorf("sealed jwks %q", sec.Endpoints.JWKS)
	}
}

// A grant connected with no name for its account takes one from the
// id_token of a later refresh; a name it has is not replaced.
func TestOAuthRefresh_FillsEmptyConnectedAsFromIDTokenOnly(t *testing.T) {
	h := idTokenHarness(t, "", nil)
	id := oauthID("docs")
	if got := h.connectedAs(t, id); got != "" {
		t.Fatalf("connected_as %q with no id_token", got)
	}
	h.f.Set(func(f *oauthtest.Server) { f.IDToken = oauthtest.IDTokenSigned })
	h.clock.advance(241 * time.Second)
	if _, err := h.sub(context.Background(), "", "", "docs"); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.e.Get(context.Background(), id); m.OAuth.Status.ConnectedAs != "idtoken@example.test" {
		t.Errorf("connected_as %q after a refresh with an id_token", m.OAuth.Status.ConnectedAs)
	}
	h.f.Set(func(f *oauthtest.Server) {
		f.IDTokenClaims = func(c map[string]any) { c["email"] = "changed@example.test" }
	})
	h.clock.advance(241 * time.Second)
	if _, err := h.sub(context.Background(), "", "", "docs"); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.e.Get(context.Background(), id); m.OAuth.Status.ConnectedAs != "idtoken@example.test" {
		t.Errorf("connected_as %q, want the first name kept", m.OAuth.Status.ConnectedAs)
	}
}

// The id_token is read for a display name and dropped: it is never stored,
// logged (at any level), audited, returned or quoted in an error.
func TestOAuthIDToken_NeverLeaks(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := idTokenHarness(t, oauthtest.IDTokenSigned, nil)
	good := oauthID("good")
	h.connect(t, good)
	h.f.Set(func(f *oauthtest.Server) {
		f.IDTokenClaims = func(c map[string]any) { c["iss"] = "https://issuer.example.test" }
	})
	bad := oauthID("bad")
	h.connect(t, bad)
	h.clock.advance(241 * time.Second)
	if _, err := h.sub(context.Background(), "", "", "bad"); err != nil {
		t.Fatal(err)
	}
	h.f.Set(func(f *oauthtest.Server) { f.IDToken = oauthtest.IDTokenAlgNone })
	h.clock.advance(241 * time.Second)
	if _, err := h.sub(context.Background(), "", "", "bad"); err != nil {
		t.Fatal(err)
	}

	var issued []string
	h.f.Set(func(f *oauthtest.Server) { issued = append(issued, f.IssuedIDTokens...) })
	if len(issued) < 3 {
		t.Fatalf("the fake issued %d id_tokens, want at least 3", len(issued))
	}
	var where []string
	where = append(where, logs.String())
	h.sink.mu.Lock()
	for _, ev := range h.sink.events {
		raw, _ := json.Marshal(ev)
		where = append(where, string(raw))
	}
	h.sink.mu.Unlock()
	for _, id := range []Identity{good, bad} {
		m, err := h.e.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(m)
		where = append(where, string(raw), m.OAuth.Status.LastError)
		row, _ := h.st.CredentialDefGet(context.Background(), id.TenantID, id.Scope, id.ScopeID, id.Name)
		where = append(where, string(row.Definition))
		_, sec := h.secretOf(t, id)
		raw, _ = json.Marshal(sec)
		where = append(where, string(raw))
	}
	for _, text := range where {
		for _, tok := range issued {
			// Each part of a JWS is checked: a logged payload or signature
			// is as much a leak as the whole token.
			for _, part := range strings.Split(tok, ".") {
				if len(part) > 16 && strings.Contains(text, part) {
					t.Fatalf("an id_token part (%.8s…) leaked into: %.200s", part, text)
				}
			}
		}
	}
	if !strings.Contains(logs.String(), "id_token") {
		t.Error("no debug line says why an id_token was ignored")
	}
}
