package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// identitySessionServer is a session server with an identity gate named
// "google" in its configuration.
func identitySessionServer(t *testing.T, m config.AuthIdentityMap) (*Server, store.Store, *recordingSink) {
	t.Helper()
	s, st, sink := sessionTestServer(t, "legacy")
	g := identityGateCfg("google", m)
	s.cfg().ResolvedAuth.Gates = append(s.cfg().ResolvedAuth.Gates, g)
	return s, st, sink
}

// identityLoginSession provisions the assertion and establishes its session,
// returning the cookie.
func identityLoginSession(t *testing.T, s *Server, sub string, authTime time.Time) (*http.Cookie, identityLogin) {
	t.Helper()
	g, ok := s.identityGate("google")
	if !ok {
		t.Fatal("no identity gate")
	}
	out, err := s.provisionOrRefuse(context.Background(), g, assertion(sub, nil), "")
	if err != nil {
		t.Fatal(err)
	}
	r := loginReq("")
	rec := httptest.NewRecorder()
	if err := s.establishSession(rec, r, sessionIdentity{TenantID: out.TenantID, Subject: out.Subject, Gate: "google",
		AMR: []string{"oidc"}, AuthTime: authTime}); err != nil {
		t.Fatal(err)
	}
	c := cookieNamed(rec, sessionCookiePlain)
	if c == nil {
		t.Fatalf("no session cookie: %v", rec.Result().Cookies())
	}
	return c, out
}

func meOf(t *testing.T, s *Server, c *http.Cookie) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleWhoami)).ServeHTTP(rec, sessionReq(http.MethodGet, "/v1/_me", c))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestIdentitySession_IsTheUsersOwnReachAndCarriesAuthTime(t *testing.T) {
	s, _, _ := identitySessionServer(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto})
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	c, _ := identityLoginSession(t, s, "100", at)
	code, me := meOf(t, s, c)
	if code != http.StatusOK || me["subject"] != "google:100" || me["tenant_id"] != "acme" || me["is_admin"] != false {
		t.Fatalf("me = %d %v", code, me)
	}
	scopes, _ := me["scopes"].([]any)
	want, _ := auth.GrantableUserScopes("isolated")
	if len(scopes) != len(want) {
		t.Fatalf("scopes = %v, want the isolated grant %v", scopes, want)
	}
	a, _ := me["auth"].(map[string]any)
	if a["gate"] != "google" || a["auth_time"] != at.UTC().Format(time.RFC3339) {
		t.Fatalf("auth = %v", a)
	}
}

// Unlinking, disabling or deleting ends the session on its next request.
func TestIdentitySession_EndsWhenUnlinkedDisabledOrDeleted(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(st store.Store){
		"unlinked": func(st store.Store) { _, _ = st.UserIdentityDelete(ctx, "acme", "google:200", "google") },
		"disabled": func(st store.Store) {
			off := "disabled"
			_ = st.UserUpdate(ctx, "acme", "google:200", store.UserPatch{Status: &off})
		},
		"deleted": func(st store.Store) { _, _ = st.UserDelete(ctx, "acme", "google:200") },
		"relinked to another account": func(st store.Store) {
			_, _ = st.UserIdentityDelete(ctx, "acme", "google:200", "google")
			_ = st.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: "acme", Subject: "google:200", Gate: "google",
				Issuer: testIssuer, ExternalSubject: "someone-else"})
		},
		"recreated user": func(st store.Store) {
			_, _ = st.UserDelete(ctx, "acme", "google:200")
			time.Sleep(time.Millisecond)
			_ = st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:200", AccessMode: "tenant", Status: "active"})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, st, sink := identitySessionServer(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto})
			c, _ := identityLoginSession(t, s, "200", time.Now())
			if code, _ := meOf(t, s, c); code != http.StatusOK {
				t.Fatalf("before: %d", code)
			}
			mutate(st)
			if code, _ := meOf(t, s, c); code != http.StatusUnauthorized {
				t.Fatalf("after: %d, want 401", code)
			}
			if ev := lastEvent(t, sink, "auth_session_revoked"); ev.AuthReason != "credential_invalid" {
				t.Fatalf("reason = %q", ev.AuthReason)
			}
		})
	}
}

// RFC FA §3.7a: a pending user signs in, learns that it waits, and every other
// request — every ACL question — answers no.
func TestIdentitySession_PendingAnswersNoMatchAndShowsAwaitingApproval(t *testing.T) {
	s, st, _ := identitySessionServer(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto,
		NewUser: config.AuthNewUser{AccessMode: "isolated", Status: "pending"}})
	c, out := identityLoginSession(t, s, "300", time.Now())
	if !out.Pending {
		t.Fatal("login not pending")
	}
	code, me := meOf(t, s, c)
	if code != http.StatusOK {
		t.Fatalf("me = %d", code)
	}
	if a, _ := me["auth"].(map[string]any); a["status"] != "pending" {
		t.Fatalf("auth = %v", me["auth"])
	}
	if sc, _ := me["scopes"].([]any); len(sc) != 0 {
		t.Fatalf("pending scopes = %v", sc)
	}
	for _, path := range []string{"/v1/agents", "/v1/runs", "/v1/_users", "/v1/_auth/account", "/v1/memory"} {
		if code, reached := through(s, sessionReq(http.MethodGet, path, c)); reached || code != http.StatusForbidden {
			t.Errorf("GET %s = %d reached=%v", path, code, reached)
		}
	}
	if code, reached := through(s, sessionReq(http.MethodPost, "/v1/runs", c)); reached || code != http.StatusForbidden {
		t.Errorf("POST /v1/runs = %d reached=%v", code, reached)
	}
	// The principal itself carries no scope and an empty ACL set.
	id, _ := s.sessionIDFromCookie(sessionReq(http.MethodGet, "/v1/_me", c))
	p, ua, ok := s.sessionPrincipal(sessionReq(http.MethodGet, "/v1/_me", c), id)
	if !ok || !ua.pending || len(p.Scopes) != 0 || p.Effective == nil {
		t.Fatalf("principal = %+v pending=%v ok=%v", p, ua.pending, ok)
	}
	// Approval (by status here; the route comes with it) turns it into a user.
	on := "active"
	if err := st.UserUpdate(context.Background(), "acme", "google:300", store.UserPatch{Status: &on}); err != nil {
		t.Fatal(err)
	}
	if code, reached := through(s, sessionReq(http.MethodGet, "/v1/agents", c)); !reached {
		t.Fatalf("approved user still refused: %d", code)
	}
}

// A session made through a gate that later loses its identity map (removed
// from config) ends.
func TestIdentitySession_EndsWhenTheGateIsRemoved(t *testing.T) {
	s, _, _ := identitySessionServer(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto})
	c, _ := identityLoginSession(t, s, "400", time.Now())
	s.cfg().ResolvedAuth.Gates = nil
	if code, _ := meOf(t, s, c); code != http.StatusUnauthorized {
		t.Fatalf("after gate removal: %d", code)
	}
}
