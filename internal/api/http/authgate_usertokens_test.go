package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA §4: strict mode's per-user-token rule. The fixture is one tenant
// with a registered user "alice" and five tokens: alice's hand-minted frozen
// member token, alice's hand-minted inherit token, alice's gate-issued
// token, a tenant operator token for alice's subject, and a service token
// for a subject that is no user.
func gateOnlyServer(t *testing.T, gateOnly bool) (*Server, store.Store) {
	t.Helper()
	s, st := tokenAuthServer(t, "legacy-secret")
	ctx := context.Background()
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []struct {
		id, subject, bearer, gate, authority string
		scopes                               []string
	}{
		{"def_minted", "alice", "minted-bearer", "", "", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{"def_inherit", "alice", "inherit-bearer", "", auth.AuthorityInherit, []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{"def_gate", "alice", "gate-bearer", "paste", "", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{"def_op", "alice", "operator-bearer", "", "", []string{auth.ScopeTenant}},
		{"def_ci", "ci-bot", "service-bearer", "", "", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
	} {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: tok.id, Name: tok.id, TenantID: "acme",
			Subject: tok.subject, TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
			IssuedByGate: tok.gate, Authority: tok.authority}); err != nil {
			t.Fatal(err)
		}
	}
	pol := strictPolicy()
	pol.UserTokensGateOnly = gateOnly
	s.cfg().ResolvedAuth = pol
	s.cfg().Env.UIInsecureCookies = true
	return s, st
}

func TestGateOnly_RefusesAHandMintedUserTokenOnTheAPIAndAcceptsAGateIssuedOne(t *testing.T) {
	s, _ := gateOnlyServer(t, true)
	// Twice: the second pass answers from the token cache, which must not
	// carry a refused token past the check.
	s.tokenCache = newTokenCache(30 * time.Second)
	for pass := 0; pass < 2; pass++ {
		for bearer, want := range map[string]bool{
			"minted-bearer":   false,
			"inherit-bearer":  false,
			"gate-bearer":     true,
			"operator-bearer": true,
			"service-bearer":  true,
			"legacy-secret":   true,
		} {
			if reached, code := runMW(t, s, http.MethodGet, "/v1/runs", bearer); reached != want {
				t.Errorf("pass %d, %s: reached = %v (code %d), want %v", pass, bearer, reached, code, want)
			}
		}
	}
	// gRPC and A2A resolve through the exported resolver: the same answer.
	if _, ok := s.ResolvePrincipal(context.Background(), "minted-bearer"); ok {
		t.Error("ResolvePrincipal (gRPC, A2A) admitted a hand-minted user token under gate_only")
	}
	if p, ok := s.ResolvePrincipal(context.Background(), "gate-bearer"); !ok || p.IssuedByGate != "paste" {
		t.Errorf("ResolvePrincipal refused a gate-issued token: %+v %v", p, ok)
	}
}

// A service token stands under gate_only only while its subject is no user:
// a user created later for that subject makes it a per-user token.
func TestGateOnly_AServiceTokenWhoseSubjectBecomesAUserIsRefused(t *testing.T) {
	s, st := gateOnlyServer(t, true)
	if reached, _ := runMW(t, s, http.MethodGet, "/v1/runs", "service-bearer"); !reached {
		t.Fatal("a service token was refused")
	}
	if err := st.UserCreate(context.Background(), store.UserRow{TenantID: "acme", Subject: "ci-bot", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if reached, code := runMW(t, s, http.MethodGet, "/v1/runs", "service-bearer"); reached {
		t.Fatalf("the token of a now-registered user still passes (%d)", code)
	}
}

// Under gate_only the user-token API mints nothing: a person's token comes
// from a gate. Without it the same request mints.
func TestGateOnly_TheUserTokenAPIRefusesToMint(t *testing.T) {
	mint := func(s *Server) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/_users/alice/tokens", strings.NewReader(`{}`))
		r.SetPathValue("subject", "alice")
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}))
		rec := httptest.NewRecorder()
		s.handleMintUserToken(rec, r)
		return rec
	}
	s, _ := gateOnlyServer(t, true)
	if rec := mint(s); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "user_tokens_gate_only") {
		t.Fatalf("gate_only mint = %d %s, want 403 user_tokens_gate_only", rec.Code, rec.Body.String())
	}
	s, _ = gateOnlyServer(t, false)
	if rec := mint(s); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("strict without gate_only mint = %d %s, want success", rec.Code, rec.Body.String())
	}
}

// Without gate_only, strict mode still keeps the hand-minted token working on
// the API (a client's), but refuses it as a Web UI sign-in; a gate-issued
// token and an operator token still sign in.
func TestStrict_RefusesAHandMintedUserTokenOnTheUIOnly(t *testing.T) {
	s, _ := gateOnlyServer(t, false)
	if reached, code := runMW(t, s, http.MethodGet, "/v1/runs", "minted-bearer"); !reached {
		t.Fatalf("strict without gate_only refused the token on the API (%d)", code)
	}
	for _, b := range []string{"minted-bearer", "inherit-bearer"} {
		if rec := doLogin(t, s, loginReq(b)); rec.Code != http.StatusForbidden {
			t.Fatalf("UI sign-in with %s = %d, want 403: %s", b, rec.Code, rec.Body.String())
		}
	}
	for _, b := range []string{"gate-bearer", "operator-bearer"} {
		if rec := doLogin(t, s, loginReq(b)); rec.Code != http.StatusNoContent {
			t.Errorf("UI sign-in with %s = %d, want 204: %s", b, rec.Code, rec.Body.String())
		}
	}
}

// A session made while the rule did not apply ends once it does: the token
// binding is re-checked per request.
func TestStrict_EndsASessionBoundToAHandMintedUserToken(t *testing.T) {
	s, _ := gateOnlyServer(t, false)
	s.cfg().ResolvedAuth = config.AuthPolicy{Mode: config.AuthModeToken}
	c := mustSession(t, s, "minted-bearer")
	if code, reached := through(s, sessionReq(http.MethodGet, "/v1/runs", c)); !reached {
		t.Fatalf("token mode session = %d", code)
	}
	s.cfg().ResolvedAuth = strictPolicy()
	if code, reached := through(s, sessionReq(http.MethodGet, "/v1/runs", c)); reached || code != http.StatusUnauthorized {
		t.Fatalf("strict mode, session of a hand-minted user token = %d (reached %v), want 401", code, reached)
	}
}

func TestTokenMode_UserTokensAreUnaffected(t *testing.T) {
	s, _ := gateOnlyServer(t, false)
	s.cfg().ResolvedAuth = config.AuthPolicy{Mode: config.AuthModeToken}
	if reached, _ := runMW(t, s, http.MethodGet, "/v1/runs", "minted-bearer"); !reached {
		t.Fatal("token mode refused a user token")
	}
	if rec := doLogin(t, s, loginReq("minted-bearer")); rec.Code != http.StatusNoContent {
		t.Fatalf("token mode UI sign-in = %d", rec.Code)
	}
}
