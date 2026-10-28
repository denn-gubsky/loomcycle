package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func selfRevoke(s *Server, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/_auth/token/revoke", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleSelfRevokeToken)).ServeHTTP(rec, r)
	return rec
}

// `loomcycle logout`: a person's token retires itself and stops working at
// once, including when it is limited to certain agents.
func TestSelfRevokeToken_DelegatedTokenRetiresItself(t *testing.T) {
	for _, body := range []string{"", `{"agents":["chat"]}`} {
		s, st := tokenAuthServer(t, "legacy")
		ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
		seedUser(t, st, "acme", "alice", "isolated", "active")
		mine := mintFor(t, s, ops, "alice", body)
		other := mintFor(t, s, ops, "alice", "")
		if rec := selfRevoke(s, mine.Token); rec.Code != http.StatusOK {
			t.Fatalf("body %q: self-revoke = %d %s", body, rec.Code, rec.Body.String())
		}
		if _, ok := s.resolvePrincipal(context.Background(), mine.Token); ok {
			t.Fatalf("body %q: the retired token still authenticates", body)
		}
		// Only the presented credential: the user's other token is untouched.
		if _, ok := s.resolvePrincipal(context.Background(), other.Token); !ok {
			t.Fatalf("body %q: retiring one token retired another", body)
		}
		if rec := selfRevoke(s, mine.Token); rec.Code != http.StatusUnauthorized {
			t.Fatalf("body %q: a second revoke = %d, want 401", body, rec.Code)
		}
	}
}

// An operator token, a config principal or the legacy secret cannot retire
// itself through this route: a misdirected logout must not take an
// operator's credential down.
func TestSelfRevokeToken_RefusesOperatorAndUnmintedCredentials(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret-value")
	seedToken(t, st, "lct_operator_token_value_123", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	if rec := selfRevoke(s, "lct_operator_token_value_123"); rec.Code != http.StatusForbidden {
		t.Fatalf("operator token: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_operator_token_value_123"); !ok {
		t.Fatal("the operator token was retired")
	}
	if rec := selfRevoke(s, "legacy-secret-value"); rec.Code != http.StatusForbidden {
		t.Fatalf("legacy secret: %d %s", rec.Code, rec.Body.String())
	}
}

// A Web UI session never retires a bearer: its cookie is not the token.
func TestSelfRevokeToken_SessionCookieIsRefused(t *testing.T) {
	s, st, _ := sessionTestServer(t, "legacy")
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	tok := mintFor(t, s, ops, "alice", "")
	c := mustSession(t, s, tok.Token)
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleSelfRevokeToken)).ServeHTTP(rec, sessionReq(http.MethodPost, "/v1/_auth/token/revoke", c))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("session cookie: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.resolvePrincipal(context.Background(), tok.Token); !ok {
		t.Fatal("a cookie retired the bearer behind its session")
	}
}
