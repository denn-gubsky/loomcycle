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
		// Whole-tenant model: a same-tenant DIFFERENT subject is allowed
		// (subjects collaborate within their tenant's workspace).
		{"same tenant, different subject (whole-tenant)", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "mallory"}), true},
		// The cross-TENANT boundary stays hard (the security property).
		{"wrong tenant blocked", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "evil", Subject: "alice"}), false},
		// Super-admin crosses tenants by design.
		{"super-admin sees all", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}), true},
	}
	for _, c := range cases {
		if got := sessionOwnershipOK(c.ctx, sess); got != c.want {
			t.Errorf("%s: sessionOwnershipOK = %v, want %v", c.name, got, c.want)
		}
	}
}

// A continuation runs under the SESSION'S tenant. Without the tenant gate, a
// token from another TENANT could POST to this session id and execute against
// it (cross-tenant memory, transcript replay, fairness evasion). Session ids
// are not secrets. Whole-tenant model: a cross-TENANT continuation gets an
// opaque 404; a same-tenant DIFFERENT subject is allowed (collaboration).
func TestHandleMessages_RejectsCrossTenantSession(t *testing.T) {
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

	// Cross-TENANT → opaque 404 (the security property stays hard).
	if rr := mkReq("evil", "mallory"); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant continuation: status=%d, want 404 (tenant gate not enforced?)", rr.Code)
	}
	// The session's own subject passes the gate (not the ownership 404).
	if rr := mkReq("acme", "alice"); rr.Code == http.StatusNotFound {
		t.Fatalf("owner continuation got 404 — the tenant gate rejected a legitimate caller")
	}
	// Whole-tenant: a DIFFERENT subject in the SAME tenant also passes (this
	// is the loosening from the per-subject QA fix — same-tenant collaboration).
	if rr := mkReq("acme", "bob"); rr.Code == http.StatusNotFound {
		t.Fatalf("same-tenant different-subject continuation got 404 — whole-tenant sharing not enabled")
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
		// RFC Y fan-out is a create op (exact path, not the /v1/runs/ prefix).
		{"POST", "/v1/runs:batch", auth.ScopeRunsCreate},
		{"POST", "/v1/sessions/s_1/messages", auth.ScopeRunsCreate},
		// Cancel is POST /v1/agents/{id}/cancel — must be runs:create (was a
		// dead DELETE case → any-authenticated).
		{"POST", "/v1/agents/a_1/cancel", auth.ScopeRunsCreate},
		// Interrupt resolve = write, list = read (were any-authenticated).
		{"POST", "/v1/runs/r_1/interrupts/i_1/resolve", auth.ScopeRunsCreate},
		{"GET", "/v1/runs/r_1/interrupts", auth.ScopeRunsRead},
		// Compact + operator steering input both MUTATE run state → runs:create
		// (exp7 I1: /input previously fell through to any-authenticated, so a
		// read-only bearer could steer a run).
		{"POST", "/v1/runs/r_1/compact", auth.ScopeRunsCreate},
		{"POST", "/v1/runs/r_1/input", auth.ScopeRunsCreate},
		{"GET", "/v1/agents/a_1", auth.ScopeRunsRead},
		{"GET", "/v1/users/alice/agents", auth.ScopeRunsRead},
		// Per-user channel surface uses the channel scopes (were
		// any-authenticated / wrongly runs:read for peek).
		{"POST", "/v1/users/alice/channels/work/publish", auth.ScopeChannelPublish},
		{"POST", "/v1/users/alice/channels/work/ack", auth.ScopeChannelPublish},
		{"GET", "/v1/users/alice/channels/work/peek", auth.ScopeChannelRead},
		// RFC AF: token minting + runtime admin STAY operator-only (substrate:admin).
		{"POST", "/v1/_operatortokendef", auth.ScopeAdmin},
		{"GET", "/v1/_resolver", auth.ScopeAdmin},
		{"POST", "/v1/_pause", auth.ScopeAdmin},
		{"GET", "/metrics", auth.ScopeAdmin},
		// RFC AG Phase 2: /v1/_mcp (the loomcycle-as-MCP-server HTTP transport) is
		// now substrate:tenant — the transport is per-principal (mcpPrincipalCtx
		// stamps the tenant + a per-tool gate withholds the admin meta-tools), so
		// the route gate only decides "may open a session". substrate:admin still
		// satisfies it (admin sessions unchanged).
		{"POST", "/v1/_mcp", auth.ScopeTenant},
		{"DELETE", "/v1/_mcp", auth.ScopeTenant},
		// RFC AF: def-authoring (8 families, incl. _mcpserverdef) + hooks are
		// tenant-confined (substrate:tenant; substrate:admin still satisfies). The
		// handlers confine a non-admin principal to its own tenant.
		{"POST", "/v1/_agentdef", auth.ScopeTenant},
		{"GET", "/v1/_agentdef/names", auth.ScopeTenant},
		{"POST", "/v1/_skilldef", auth.ScopeTenant},
		{"POST", "/v1/_mcpserverdef", auth.ScopeTenant},
		{"GET", "/v1/_mcpserverdef/names", auth.ScopeTenant},
		{"POST", "/v1/_scheduledef", auth.ScopeTenant},
		{"POST", "/v1/_webhookdef", auth.ScopeTenant},
		{"POST", "/v1/_memorybackenddef", auth.ScopeTenant},
		{"POST", "/v1/_a2aagentdef", auth.ScopeTenant},
		{"POST", "/v1/_a2aservercarddef", auth.ScopeTenant},
		{"GET", "/v1/_a2aservercarddef/names", auth.ScopeTenant},
		// RFC AS Phase 1: the unified Library list views are tenant-reachable
		// (the handler tenant-scopes the result, #575). Without this, the
		// /v1/_* catch-all below 403'd a tenant token before the scoped
		// handler ran — so #575's tenant branch was unreachable.
		{"GET", "/v1/_library/agents", auth.ScopeTenant},
		{"GET", "/v1/_library/skills", auth.ScopeTenant},
		{"GET", "/v1/_library/mcp-servers", auth.ScopeTenant},
		// RFC AS: the schedules surface (list-all + per-def ops) is tenant-
		// reachable; the handlers confine a tenant to its own schedule defs.
		{"GET", "/v1/_schedules/list-all", auth.ScopeTenant},
		{"GET", "/v1/_schedules/sd_1/state", auth.ScopeTenant},
		{"POST", "/v1/_schedules/sd_1/run-now", auth.ScopeTenant},
		{"POST", "/v1/_schedules/sd_1/pause", auth.ScopeTenant},
		{"POST", "/v1/_schedules/sd_1/resume", auth.ScopeTenant},
		// RFC AS: the audit/event log is tenant-reachable (handleListEvents
		// scopes the result via the event's owning session's tenant).
		{"GET", "/v1/_events", auth.ScopeTenant},
		// The routing view is tenant-reachable; the handler strips infra detail
		// for a non-admin caller.
		{"GET", "/v1/_routing", auth.ScopeTenant},
		// Configured model aliases — tenant-readable so a tenant operator's UI
		// can offer aliases in a model picker (non-secret global config).
		{"GET", "/v1/_models", auth.ScopeTenant},
		// RFC AF: hooks are tenant-confined now that the registry is
		// tenant-isolated (stamp on register, tenant-filtered Match, scoped
		// List/Delete). substrate:admin still satisfies.
		{"POST", "/v1/hooks", auth.ScopeTenant},
		{"GET", "/v1/hooks", auth.ScopeTenant},
		{"DELETE", "/v1/hooks/h_1", auth.ScopeTenant},
		// RFC AQ: the embedded preset/env-template read endpoints fall under the
		// /v1/_* operator-admin default (the Settings hub is admin-only).
		{"GET", "/v1/_presets", auth.ScopeAdmin},
		{"GET", "/v1/_presets/base", auth.ScopeAdmin},
		{"GET", "/v1/_env_template", auth.ScopeAdmin},
		// Consumer gateway endpoints are NOT admin.
		{"POST", "/v1/_llm/chat", ""},
		{"POST", "/v1/chat/completions", ""},
		{"POST", "/v1/embeddings", ""},
		{"GET", "/healthz", ""},
		// S2 default-deny: an UNLISTED mutating route falls through to
		// ScopeAdmin (not any-authenticated), so a forgotten new state-changing
		// endpoint can't silently ship reachable by a narrow tenant token. An
		// unlisted READ keeps the any-authenticated default (tenant-gated in the
		// handler).
		{"POST", "/v1/widgets", auth.ScopeAdmin},
		{"PUT", "/v1/widgets/w_1", auth.ScopeAdmin},
		{"PATCH", "/v1/widgets/w_1", auth.ScopeAdmin},
		{"DELETE", "/v1/widgets/w_1", auth.ScopeAdmin},
		{"GET", "/v1/widgets", ""},
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

// TestAuthMiddleware_RFCAFTenantToken drives the full RFC AF posture through
// the real middleware: a substrate:tenant token is ADMITTED on the
// tenant-confined def + hook plane and (RFC AG Phase 2) may OPEN an MCP session
// on /v1/_mcp, but is REFUSED (403) on the operator plane (token minting,
// runtime admin). Inside an MCP session the per-tool gate still withholds the
// admin-only meta-tools — covered by the internal/api/mcp tool-gate tests.
func TestAuthMiddleware_RFCAFTenantToken(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedToken(t, st, "lct_tenant", "jobember", "svc", []string{auth.ScopeTenant}, time.Time{})

	admitted := []struct{ method, path string }{
		{"POST", "/v1/_agentdef"}, // def authoring — confined
		{"GET", "/v1/_agentdef/names"},
		{"POST", "/v1/_mcpserverdef"}, // dynamic MCP ingestion — confined
		{"POST", "/v1/hooks"},         // tenant-isolated hooks
		{"POST", "/v1/_mcp"},          // RFC AG Phase 2: may OPEN an MCP session
	}
	for _, c := range admitted {
		var reached bool
		h := s.authMiddleware(passthroughHandler(&reached))
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer lct_tenant")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !reached || rec.Code == http.StatusForbidden {
			t.Errorf("substrate:tenant on %s %s: code=%d reached=%v, want admitted", c.method, c.path, rec.Code, reached)
		}
	}

	refused := []struct{ method, path string }{
		{"POST", "/v1/_operatortokendef"}, // token minting — operator-only
		{"POST", "/v1/_pause"},            // runtime admin
	}
	for _, c := range refused {
		var reached bool
		h := s.authMiddleware(passthroughHandler(&reached))
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer lct_tenant")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || reached {
			t.Errorf("substrate:tenant on %s %s: code=%d reached=%v, want 403 + not reached", c.method, c.path, rec.Code, reached)
		}
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

// F18: under the legacy LOOMCYCLE_AUTH_TOKEN (single-operator, no-boundary), the
// caller-asserted wire user_id must be HONORED, not clobbered to the placeholder
// "default" — otherwise every spawn_run / POST /v1/runs is scoped to "default"
// (broken per-user fairness, memory/channel scope, attribution). A REAL
// OperatorTokenDef principal keeps the strict override —
// TestApplyPrincipal_OverridesWireAndFallsBack pins that (its principal has
// Legacy=false), so the security boundary is unchanged.
func TestApplyPrincipal_LegacyHonorsWireUserID(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	legacy := auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}
	ctx := auth.WithPrincipal(context.Background(), legacy)

	// Caller asserts user_id → honored; tenant stays the legacy default even if
	// a wire tenant is supplied (tenant routing is a real isolation axis).
	if tenant, subject := s.applyPrincipal(ctx, "ignored-tenant", "exp1"); tenant != "default" || subject != "exp1" {
		t.Errorf("legacy+wire = (%q,%q), want (default,exp1)", tenant, subject)
	}

	// No wire user_id → falls back to the placeholder subject.
	if tenant, subject := s.applyPrincipal(ctx, "", ""); tenant != "default" || subject != "default" {
		t.Errorf("legacy+empty = (%q,%q), want (default,default)", tenant, subject)
	}
}

// TestResolvePrincipal_DeclaredPrincipal drives the full bearer resolver through
// RFC AO's added step: a config-declared principal resolves to its
// (tenant, subject, scopes); the minted substrate (step 1) and the legacy
// fallback (step 3) still resolve to their own principals — the declared step
// sits between them and only matches its own secret. Fail-before: remove the
// MatchDeclared block in resolvePrincipalUncached and the declared bearer falls
// through to legacy (≠ its value) → unresolved.
func TestResolvePrincipal_DeclaredPrincipal(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	s.cfg.ResolvedPrincipals = []auth.DeclaredPrincipal{
		{Secret: "lct_marketing", Principal: auth.Principal{TenantID: "acme", Subject: "marketing", Scopes: []string{auth.ScopeTenant}, TokenDefID: "cfg:marketing"}},
	}
	// A minted def whose token value differs from the declared one.
	seedToken(t, st, "lct_minted", "beta", "svc", []string{auth.ScopeRunsCreate}, time.Time{})

	// Declared bearer → its own principal (non-legacy).
	if p, ok := s.resolvePrincipal(context.Background(), "lct_marketing"); !ok || p.TenantID != "acme" || p.Subject != "marketing" || p.Legacy {
		t.Errorf("declared resolve = (%+v, %v), want acme/marketing non-legacy", p, ok)
	}
	// Minted def still resolves (precedence step 1, unaffected).
	if p, ok := s.resolvePrincipal(context.Background(), "lct_minted"); !ok || p.TenantID != "beta" || p.Subject != "svc" {
		t.Errorf("minted resolve = (%+v, %v), want beta/svc", p, ok)
	}
	// Legacy token still resolves (precedence step 3; declared is tried first
	// but only matches its own secret).
	if p, ok := s.resolvePrincipal(context.Background(), "legacy-secret"); !ok || !p.Legacy {
		t.Errorf("legacy resolve = (%+v, %v), want the legacy principal", p, ok)
	}
	// Unknown bearer → unresolved.
	if _, ok := s.resolvePrincipal(context.Background(), "lct_unknown"); ok {
		t.Error("unknown bearer must not resolve")
	}
}
