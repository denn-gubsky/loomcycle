package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY §8: an inherit token proves identity only; its ceiling is the
// implied scope set of its user's CURRENT access_mode, materialised on every
// request. These tests drive resolvePrincipal with the token cache ON, so a
// pass proves the ceiling is re-read past a warm cache, not merely on a miss.

const inheritPlain = "lc_inherit_test_token_0123456789abcdef"

// seedAuthorityToken writes a member token for acme/alice with the given
// authority and restrictions, holding the scopes it was minted with.
func seedAuthorityToken(t *testing.T, st store.Store, authority string, mintedScopes []string, restrictionsJSON string) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_alice_tok", Name: "u-alice", TenantID: "acme", Subject: "alice",
		TokenHash: auth.HashToken(testTokenPepper, inheritPlain), AllowedScopes: mintedScopes,
		RestrictionsJSON: restrictionsJSON, Authority: authority,
	}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
}

func setAccessMode(t *testing.T, st store.Store, mode string) {
	t.Helper()
	if err := st.UserUpdate(context.Background(), "acme", "alice", store.UserPatch{AccessMode: &mode}); err != nil {
		t.Fatalf("set access_mode %s: %v", mode, err)
	}
}

func setStatus(t *testing.T, st store.Store, status string) {
	t.Helper()
	if err := st.UserUpdate(context.Background(), "acme", "alice", store.UserPatch{Status: &status}); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

func grantFor(t *testing.T, mode string) []string {
	t.Helper()
	g, err := auth.GrantableUserScopes(mode)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func sameScopes(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func resolveAlice(t *testing.T, s *Server) (auth.Principal, bool) {
	t.Helper()
	return s.resolvePrincipal(context.Background(), inheritPlain)
}

func TestInheritToken_PromotionWidensTheNextRequest(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.EnableTokenCache(time.Minute)
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedAuthorityToken(t, st, auth.AuthorityInherit, grantFor(t, "isolated"), "")

	p, ok := resolveAlice(t, s)
	if !ok || !auth.IsIsolated(p, ok) || !sameScopes(p.Scopes, grantFor(t, "isolated")) {
		t.Fatalf("before promotion: %v %v, want the isolated grant", p.Scopes, ok)
	}
	setAccessMode(t, st, "tenant")
	p, ok = resolveAlice(t, s)
	if !ok {
		t.Fatal("promoted user's inherit token stopped authenticating")
	}
	if !sameScopes(p.Scopes, grantFor(t, "tenant")) || auth.IsIsolated(p, ok) {
		t.Fatalf("after promotion: scopes %v, want the tenant grant %v (no re-mint, warm cache)", p.Scopes, grantFor(t, "tenant"))
	}
	if !p.InheritsAuthority {
		t.Error("principal does not report its inherited authority")
	}
}

func TestInheritToken_DemotionNarrowsTheNextRequest(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.EnableTokenCache(time.Minute)
	seedUser(t, st, "acme", "alice", "tenant", "active")
	seedAuthorityToken(t, st, auth.AuthorityInherit, grantFor(t, "tenant"), "")

	if p, ok := resolveAlice(t, s); !ok || !sameScopes(p.Scopes, grantFor(t, "tenant")) {
		t.Fatalf("before demotion: %v %v", p.Scopes, ok)
	}
	setAccessMode(t, st, "isolated")
	p, ok := resolveAlice(t, s)
	if !ok || !sameScopes(p.Scopes, grantFor(t, "isolated")) || !auth.IsIsolated(p, ok) {
		t.Fatalf("after demotion: scopes %v ok=%v, want the isolated grant — the mint-time tenant grant must not survive", p.Scopes, ok)
	}
}

func TestFrozenToken_KeepsItsMintedScopesAcrossAccessModeChanges(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.EnableTokenCache(time.Minute)
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedAuthorityToken(t, st, "", grantFor(t, "isolated"), "")

	setAccessMode(t, st, "tenant")
	p, ok := resolveAlice(t, s)
	if !ok || !sameScopes(p.Scopes, grantFor(t, "isolated")) || p.InheritsAuthority {
		t.Fatalf("frozen token after promotion: %v ok=%v inherit=%v, want its minted isolated grant", p.Scopes, ok, p.InheritsAuthority)
	}
}

func TestInheritToken_InactiveOrMissingUserRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, st store.Store)
	}{
		{"disabled", func(t *testing.T, st store.Store) { setStatus(t, st, "disabled") }},
		{"pending", func(t *testing.T, st store.Store) { setStatus(t, st, store.UserStatusPending) }},
		{"deleted", func(t *testing.T, st store.Store) {
			if _, err := st.UserDelete(context.Background(), "acme", "alice"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := tokenAuthServer(t, "")
			s.EnableTokenCache(time.Minute)
			seedUser(t, st, "acme", "alice", "tenant", "active")
			seedAuthorityToken(t, st, auth.AuthorityInherit, grantFor(t, "tenant"), "")
			if _, ok := resolveAlice(t, s); !ok {
				t.Fatal("active user's inherit token refused")
			}
			tc.mutate(t, st)
			if p, ok := resolveAlice(t, s); ok {
				t.Fatalf("%s user's inherit token still authenticates as %+v", tc.name, p)
			}
		})
	}
}

func TestInheritToken_RestrictionsStillNarrowAfterPromotion(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.EnableTokenCache(time.Minute)
	seedUser(t, st, "acme", "alice", "isolated", "active")
	rj, err := auth.MarshalRestrictions(auth.Restrictions{Agents: []string{"chat"}})
	if err != nil {
		t.Fatal(err)
	}
	seedAuthorityToken(t, st, auth.AuthorityInherit, grantFor(t, "isolated"), rj)
	setAccessMode(t, st, "tenant")
	p, ok := resolveAlice(t, s)
	if !ok || !sameScopes(p.Scopes, grantFor(t, "tenant")) {
		t.Fatalf("after promotion: %v %v", p.Scopes, ok)
	}
	if !p.Restrictions.RunPlaneOnly() || !slices.Equal(p.Restrictions.Agents, []string{"chat"}) {
		t.Fatalf("restrictions after promotion = %+v, want the token's agent allowlist kept", p.Restrictions)
	}
}

// The crossing a browser takes: a Web UI session made from an inherit token
// re-resolves the token on every request, so a promotion reaches the
// session's principal too, and /v1/_me says the token inherits.
func TestInheritToken_WebUISessionFollowsPromotionAndMeReportsIt(t *testing.T) {
	// A legacy secret switches authentication on; the session is the token's.
	s, st, _ := sessionTestServer(t, "legacy-secret")
	s.EnableTokenCache(time.Minute)
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedAuthorityToken(t, st, auth.AuthorityInherit, grantFor(t, "isolated"), "")
	c := mustSession(t, s, inheritPlain)

	principalOf := func() auth.Principal {
		t.Helper()
		var got auth.Principal
		var reached bool
		h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, reached = auth.PrincipalFromContext(r.Context())
		}))
		h.ServeHTTP(httptest.NewRecorder(), sessionReq(http.MethodGet, "/v1/_me", c))
		if !reached {
			t.Fatal("session request did not authenticate")
		}
		return got
	}
	if p := principalOf(); !sameScopes(p.Scopes, grantFor(t, "isolated")) {
		t.Fatalf("session before promotion: %v", p.Scopes)
	}
	setAccessMode(t, st, "tenant")
	if p := principalOf(); !sameScopes(p.Scopes, grantFor(t, "tenant")) {
		t.Fatalf("session after promotion: %v, want the tenant grant", p.Scopes)
	}

	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleWhoami)).ServeHTTP(rec, sessionReq(http.MethodGet, "/v1/_me", c))
	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /v1/_me (%d): %v", rec.Code, err)
	}
	if me["authority"] != auth.AuthorityInherit {
		t.Errorf("/v1/_me authority = %v, want inherit", me["authority"])
	}
}

func TestInheritToken_UnknownAuthorityRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	seedUser(t, st, "acme", "alice", "tenant", "active")
	seedAuthorityToken(t, st, "everything", grantFor(t, "tenant"), "")
	if p, ok := resolveAlice(t, s); ok {
		t.Fatalf("a token with an authority this build does not know authenticated as %+v", p)
	}
}
