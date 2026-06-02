package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

const testTokenPepper = "pep-rfcl"

// tokenAuthServer builds a Server backed by in-memory SQLite + a legacy
// AuthToken + a token pepper — enough to exercise resolvePrincipal and
// the principal-stamping middleware.
func tokenAuthServer(t *testing.T, legacy string) (*Server, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hookReg := hooks.NewRegistry()
	cfg := &config.Config{}
	cfg.Env.AuthToken = legacy
	cfg.Env.OperatorTokenPepper = testTokenPepper
	s := &Server{
		cfg:            cfg,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
		store:          st,
	}
	return s, st
}

func seedToken(t *testing.T, st store.Store, plaintext, tenant, subject string, scopes []string, retiredAt time.Time) {
	t.Helper()
	_, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID:         "def_" + subject,
		Name:          subject,
		TenantID:      tenant,
		Subject:       subject,
		TokenHash:     auth.HashToken(testTokenPepper, plaintext),
		AllowedScopes: scopes,
		RetiredAt:     retiredAt,
	})
	if err != nil {
		t.Fatalf("seed token: %v", err)
	}
}

func TestSessionOwnershipOK_Matrix(t *testing.T) {
	sess := store.Session{TenantID: "acme", UserID: "alice"}
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"no principal (open mode)", context.Background(), true},
		{"legacy principal exempt", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "default", Subject: "default", Legacy: true}), true},
		{"owner matches", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice"}), true},
		{"wrong tenant", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "evil", Subject: "alice"}), false},
		{"wrong subject", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "mallory"}), false},
	}
	for _, c := range cases {
		if got := sessionOwnershipOK(c.ctx, sess); got != c.want {
			t.Errorf("%s: sessionOwnershipOK = %v, want %v", c.name, got, c.want)
		}
	}
}

// A continuation runs under the SESSION'S stored tenant+subject. Without an
// ownership check, principal-A could POST to principal-B's session id and
// execute under B's identity (cross-tenant memory, B's transcript replayed to
// A, fairness evasion). Session ids are not secrets. The guard returns an
// opaque 404 to a non-owner.
func TestHandleMessages_RejectsCrossPrincipalSession(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	sess, err := st.CreateSession(context.Background(), "acme", "agentx", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	body := `{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`

	mkReq := func(tenant, subject string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sess.ID+"/messages", strings.NewReader(body))
		r.SetPathValue("id", sess.ID)
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
			TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeRunsCreate},
		}))
		rr := httptest.NewRecorder()
		s.handleMessages(rr, r)
		return rr
	}

	// Different principal → opaque 404 (the security property).
	if rr := mkReq("evil", "mallory"); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-principal continuation: status=%d, want 404 (ownership not enforced?)", rr.Code)
	}
	// The owner passes the ownership gate (must NOT be the ownership 404; it
	// proceeds past the check — any later status is fine, just not 404).
	if rr := mkReq("acme", "alice"); rr.Code == http.StatusNotFound {
		t.Fatalf("owner continuation got 404 — the ownership guard rejected the legitimate owner")
	}
}

func TestResolvePrincipal_TokenSubstrateHit(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedToken(t, st, "lct_alice", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})

	p, ok := s.resolvePrincipal(context.Background(), "lct_alice")
	if !ok {
		t.Fatal("expected resolution")
	}
	if p.TenantID != "acme" || p.Subject != "alice" || p.Legacy {
		t.Errorf("got %+v", p)
	}
	if !auth.HasScope(p.Scopes, auth.ScopeRunsCreate) {
		t.Errorf("scopes = %v", p.Scopes)
	}
}

func TestResolvePrincipal_UnknownTokenFalls(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	if _, ok := s.resolvePrincipal(context.Background(), "lct_nope"); ok {
		t.Error("unknown token must not resolve")
	}
}

func TestResolvePrincipal_ExpiredVsGrace(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	// Expired: retired_at in the past → no resolution.
	seedToken(t, st, "lct_old", "acme", "old", []string{auth.ScopeAdmin}, time.Now().Add(-time.Hour))
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Error("expired token must not resolve")
	}
	// Grace: retired_at in the future → still valid.
	seedToken(t, st, "lct_grace", "acme", "grace", []string{auth.ScopeAdmin}, time.Now().Add(time.Hour))
	if _, ok := s.resolvePrincipal(context.Background(), "lct_grace"); !ok {
		t.Error("token within the rotation grace window must still resolve")
	}
}

func TestResolvePrincipal_LegacyFallbackAndNoLockout(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	// No admin token yet → legacy works, yields the synthetic default
	// admin principal.
	p, ok := s.resolvePrincipal(context.Background(), "legacy-secret")
	if !ok || !p.Legacy || p.TenantID != "default" || p.Subject != "default" {
		t.Fatalf("legacy fallback: ok=%v p=%+v", ok, p)
	}
	if !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		t.Error("legacy principal must be admin")
	}
	// Once an admin-scoped token exists, the legacy fallback is disabled.
	seedToken(t, st, "lct_admin", "acme", "ops", []string{auth.ScopeAdmin}, time.Time{})
	if _, ok := s.resolvePrincipal(context.Background(), "legacy-secret"); ok {
		t.Error("legacy fallback must be disabled once an admin token exists (no-lockout gate)")
	}
	// ...but the admin token itself still resolves.
	if _, ok := s.resolvePrincipal(context.Background(), "lct_admin"); !ok {
		t.Error("admin token must resolve")
	}
}

func TestRequiredScopeFor(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/v1/runs", auth.ScopeRunsCreate},
		{"POST", "/v1/sessions/s_1/messages", auth.ScopeRunsCreate},
		// Cancel is POST /v1/agents/{id}/cancel — must be runs:create (was a
		// dead DELETE case → any-authenticated).
		{"POST", "/v1/agents/a_1/cancel", auth.ScopeRunsCreate},
		// Interrupt resolve = write, list = read (were any-authenticated).
		{"POST", "/v1/runs/r_1/interrupts/i_1/resolve", auth.ScopeRunsCreate},
		{"GET", "/v1/runs/r_1/interrupts", auth.ScopeRunsRead},
		{"GET", "/v1/agents/a_1", auth.ScopeRunsRead},
		{"GET", "/v1/users/alice/agents", auth.ScopeRunsRead},
		// Per-user channel surface uses the channel scopes (were
		// any-authenticated / wrongly runs:read for peek).
		{"POST", "/v1/users/alice/channels/work/publish", auth.ScopeChannelPublish},
		{"POST", "/v1/users/alice/channels/work/ack", auth.ScopeChannelPublish},
		{"GET", "/v1/users/alice/channels/work/peek", auth.ScopeChannelRead},
		{"POST", "/v1/_operatortokendef", auth.ScopeAdmin},
		{"GET", "/v1/_resolver", auth.ScopeAdmin},
		{"POST", "/v1/_pause", auth.ScopeAdmin},
		{"DELETE", "/v1/hooks/h_1", auth.ScopeAdmin},
		{"GET", "/metrics", auth.ScopeAdmin},
		// Consumer gateway endpoints are NOT admin.
		{"POST", "/v1/_llm/chat", ""},
		{"POST", "/v1/chat/completions", ""},
		{"POST", "/v1/embeddings", ""},
		{"GET", "/healthz", ""},
	}
	for _, c := range cases {
		if got := requiredScopeFor(c.method, c.path); got != c.want {
			t.Errorf("requiredScopeFor(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestAuthMiddleware_403InsufficientScope(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	// A narrow token: runs:read only.
	seedToken(t, st, "lct_ro", "acme", "ro", []string{auth.ScopeRunsRead}, time.Time{})

	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))
	// POST /v1/runs requires runs:create — the read-only token is refused.
	req := httptest.NewRequest("POST", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer lct_ro")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if reached {
		t.Error("handler should not have been reached")
	}
	if wa := rec.Header().Get("WWW-Authenticate"); wa != `Bearer scope="runs:create"` {
		t.Errorf("WWW-Authenticate = %q, want the runs:create hint", wa)
	}
}

func TestAuthMiddleware_StampsPrincipalAndAllowsScopedRoute(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedToken(t, st, "lct_creator", "acme", "alice", []string{auth.ScopeRunsCreate}, time.Time{})

	var gotTenant, gotSubject string
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFromContext(r.Context()); ok {
			gotTenant, gotSubject = p.TenantID, p.Subject
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("POST", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer lct_creator")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotTenant != "acme" || gotSubject != "alice" {
		t.Errorf("principal in ctx = (%q,%q), want (acme,alice)", gotTenant, gotSubject)
	}
}

func TestAuthMiddleware_401UnknownToken(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))
	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	req.Header.Set("Authorization", "Bearer lct_unknown")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("status = %d reached = %v, want 401 + not reached", rec.Code, reached)
	}
}

func TestApplyPrincipal_OverridesWireAndFallsBack(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	// With a principal, the wire tenant/user are ignored.
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice"})
	tenant, subject := s.applyPrincipal(ctx, "forged-tenant", "forged-bob")
	if tenant != "acme" || subject != "alice" {
		t.Errorf("override = (%q,%q), want (acme,alice) — wire claims must be ignored", tenant, subject)
	}
	// Without a principal (open / un-authed), the wire values pass through.
	tenant, subject = s.applyPrincipal(context.Background(), "wire-t", "wire-u")
	if tenant != "wire-t" || subject != "wire-u" {
		t.Errorf("no-principal = (%q,%q), want the wire values", tenant, subject)
	}
}
