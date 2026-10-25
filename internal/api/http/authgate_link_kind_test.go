package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/oidc/oidctest"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// addOIDCGate puts an OIDC gate "idp" (tenant acme) in front of a fake
// provider on an existing server.
func addOIDCGate(t *testing.T, s *Server) *oidctest.Provider {
	t.Helper()
	p := oidctest.New("client-1", "secret-1")
	t.Cleanup(p.Close)
	p.Claims = map[string]any{"sub": "u-1", "email": "a@acme.example", "email_verified": true}
	g := identityGateCfg("idp", config.AuthIdentityMap{Tenant: "acme"})
	g.OIDC = &config.AuthOIDCGate{Preset: config.OIDCPresetGeneric, ClientID: "client-1", ClientSecret: "secret-1", Issuer: p.Issuer,
		DiscoveryURL: p.Server.URL + "/.well-known/openid-configuration", Scopes: []string{"openid"}, RedirectURI: oidcTestRedirect}
	s.cfg().ResolvedAuth.Gates = append(s.cfg().ResolvedAuth.Gates, g)
	s.cfg().Env.PublicURL = "https://lc.example"
	s.oidcHTTP = p.Server.Client()
	return p
}

// A token-gate session stands for a pasted bearer, which may be narrower than
// its user (restricted, fewer scopes). Linking an identity from it would give
// the user's FULL reach to every later sign-in through that identity: a
// restricted credential widening itself. Only a session whose credential is
// the user itself (password or identity gate) may link or unlink.
func TestLink_TokenGateSessionsCannotLink(t *testing.T) {
	for name, restrictions := range map[string]string{"restricted": `{"retention":"none"}`, "unrestricted": ""} {
		t.Run(name, func(t *testing.T) {
			s, st, _ := sessionTestServer(t, "legacy")
			addOIDCGate(t, s)
			s.cfg().ResolvedAuth.Gates = append(s.cfg().ResolvedAuth.Gates, config.AuthGate{Name: "paste", Kind: config.AuthGateKindToken, Label: "token"})
			ctx := context.Background()
			if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", AccessMode: "tenant", Status: "active"}); err != nil {
				t.Fatal(err)
			}
			const bearer = "lct_link_kind_test_bearer"
			scopes, _ := auth.GrantableUserScopes("tenant")
			if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: "def_alice", Name: "u-alice", TenantID: "acme",
				Subject: "alice", TokenHash: auth.HashToken(testTokenPepper, bearer), AllowedScopes: scopes, RestrictionsJSON: restrictions}); err != nil {
				t.Fatal(err)
			}
			c := mustSession(t, s, bearer)
			rec := httptest.NewRecorder()
			s.authMiddleware(http.HandlerFunc(s.handleLinkUserIdentity)).ServeHTTP(rec, linkReq(c, "alice", `{"gate":"idp"}`))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "link_needs_user_session") {
				t.Fatalf("token-gate session link = %d %s", rec.Code, rec.Body.String())
			}
			if cookieNamed(rec, loginCookiePlain) != nil {
				t.Fatal("a link login was started")
			}
		})
	}
}

// A password-gate session is the user's own credential, so it may link.
func TestLink_PasswordGateSessionMayLink(t *testing.T) {
	f := newGateFixture(t)
	p := addOIDCGate(t, f.srv)
	inv := f.inviteCode(t, bAcmeOp, "local", `{"email":"alice@example.com"}`)
	b := f.redeemed(t, "local", "alice@example.com", inv)
	_, subject := b.me()
	code, body := b.do("POST", "/v1/_users/"+subject+"/identities", `{"gate":"idp"}`)
	if code != http.StatusOK {
		t.Fatalf("password session link = %d %v", code, body)
	}
	if u, _ := body["redirect"].(string); !strings.HasPrefix(u, p.Server.URL+"/authorize?") {
		t.Fatalf("redirect = %v", body["redirect"])
	}
}

// The link is re-checked at the callback against the session that asked: a
// session revoked (or one of another kind) between start and callback links
// nothing.
func TestLink_SessionRevokedBetweenStartAndCompleteIsRefused(t *testing.T) {
	f, c, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
	rec, redirect := f.startLink(t, linkReq(c, subject, `{"gate":"idp"}`))
	if redirect == "" {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	id, _ := f.s.sessionIDFromCookie(sessionReq(http.MethodGet, "/", c))
	if _, err := f.st.UISessionRevoke(context.Background(), sessionHash(id), time.Now()); err != nil {
		t.Fatal(err)
	}
	done := f.complete(t, f.authorize(t, redirect), cookieNamed(rec, loginCookiePlain))
	if !strings.Contains(done.Header().Get("Location"), "link_error") {
		t.Fatalf("callback = %q", done.Header().Get("Location"))
	}
	if _, err := f.st.UserIdentityGet(context.Background(), "idp", f.p.Issuer, "u-1"); !isNotFound(err) {
		t.Fatal("linked through a revoked session")
	}
}
