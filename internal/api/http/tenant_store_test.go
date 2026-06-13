package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedRunInTenant creates a session+run in the given tenant and returns the
// run's id + agent_id. Mirrors makeRunForInterrupt in the store contract but
// stamps an explicit tenant (CreateRun denormalises identity.TenantID onto the
// run row — that's the column the tenant gate reads).
func seedRunInTenant(t *testing.T, st store.Store, tenant, userID, agentID string) (runID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "agentx", userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: userID, TenantID: tenant})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
}

// The run-scoped interrupt listing exposes the run's pending questions; without
// a tenant gate a token from another TENANT could read them by guessing the
// run_id (run_ids are not secret). The accessor gates on the OWNING run's
// tenant and folds a cross-tenant run into an opaque empty list — same as a
// real run with zero interrupts (no existence oracle). Fails on the pre-gate
// code where the handler queried the store directly.
func TestHandleListRunInterrupts_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_intr")
	id := store.MintInterruptID(time.Now())
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "alice", Question: "secret?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}

	listFor := func(tenant, subject string, scopes []string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/interrupts", nil)
		r.SetPathValue("run_id", runID)
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
			TenantID: tenant, Subject: subject, Scopes: scopes,
		}))
		rr := httptest.NewRecorder()
		s.handleListRunInterrupts(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rr.Code)
		}
		var body struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Total
	}

	// Cross-TENANT → opaque empty (the leak we're closing).
	if got := listFor("evil", "mallory", []string{auth.ScopeRunsRead}); got != 0 {
		t.Errorf("cross-tenant run-interrupt list returned %d rows, want 0 (tenant gate not enforced)", got)
	}
	// The run's own tenant still sees its interrupt.
	if got := listFor("acme", "alice", []string{auth.ScopeRunsRead}); got != 1 {
		t.Errorf("own-tenant list returned %d rows, want 1 (gate rejected a legitimate caller)", got)
	}
	// A same-tenant DIFFERENT subject also sees it (whole-tenant collaboration).
	if got := listFor("acme", "bob", []string{auth.ScopeRunsRead}); got != 1 {
		t.Errorf("same-tenant different-subject list returned %d rows, want 1 (whole-tenant sharing)", got)
	}
	// Super-admin crosses tenants by design.
	if got := listFor("x", "ops", []string{auth.ScopeAdmin}); got != 1 {
		t.Errorf("admin list returned %d rows, want 1", got)
	}
}

// The transcript endpoint exposes a session's full history; the migrated
// handler gates it through the tenant-scoped accessor. A cross-TENANT caller
// gets the same opaque 404 a missing session returns; the owning tenant + admin
// resolve it. Locks the gate on handleTranscript after the accessor migration.
func TestHandleTranscript_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	sess, err := st.CreateSession(context.Background(), "acme", "agentx", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	statusFor := func(tenant, subject string, scopes []string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript", nil)
		r.SetPathValue("id", sess.ID)
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
			TenantID: tenant, Subject: subject, Scopes: scopes,
		}))
		rr := httptest.NewRecorder()
		s.handleTranscript(rr, r)
		return rr.Code
	}

	if got := statusFor("evil", "mallory", []string{auth.ScopeRunsRead}); got != http.StatusNotFound {
		t.Errorf("cross-tenant transcript status=%d, want 404 (tenant gate not enforced)", got)
	}
	if got := statusFor("acme", "alice", []string{auth.ScopeRunsRead}); got == http.StatusNotFound {
		t.Errorf("own-tenant transcript got 404 — gate rejected a legitimate caller")
	}
	if got := statusFor("acme", "bob", []string{auth.ScopeRunsRead}); got == http.StatusNotFound {
		t.Errorf("same-tenant different-subject transcript got 404 — whole-tenant sharing")
	}
	if got := statusFor("x", "ops", []string{auth.ScopeAdmin}); got == http.StatusNotFound {
		t.Errorf("admin transcript got 404 — super-admin must cross tenants")
	}
}

// The user-scoped interrupt inbox (GET /v1/users/{user_id}/interrupts) keyed on
// user_id alone before this change; a token could read another tenant's pending
// questions by passing a user_id that exists in both tenants (user_ids are not
// secret). The handler now passes the principal's tenant down to the store
// JOIN. Fails on the pre-gate code, which returned both tenants' rows.
func TestHandleListUserInterrupts_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()
	const user = "u_shared"
	runAcme := seedRunInTenant(t, st, "acme", user, "a_acme_ui")
	runEvil := seedRunInTenant(t, st, "evil", user, "a_evil_ui")
	for _, rr := range []struct{ run, q string }{{runAcme, "acme Q"}, {runEvil, "evil Q"}} {
		if _, err := st.InterruptCreate(ctx, store.InterruptRow{
			InterruptID: store.MintInterruptID(time.Now()), RunID: rr.run, UserID: user,
			Question: rr.q, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("InterruptCreate: %v", err)
		}
	}

	totalFor := func(tenant, subject string, scopes []string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/users/"+user+"/interrupts?status=all", nil)
		r.SetPathValue("user_id", user)
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
			TenantID: tenant, Subject: subject, Scopes: scopes,
		}))
		rr := httptest.NewRecorder()
		s.handleListUserInterrupts(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rr.Code)
		}
		var body struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Total
	}

	// A tenant principal sees only its own tenant's interrupt for this user.
	if got := totalFor("acme", "alice", []string{auth.ScopeRunsRead}); got != 1 {
		t.Errorf("acme inbox for %q = %d rows, want 1 (cross-tenant leak)", user, got)
	}
	if got := totalFor("evil", "mallory", []string{auth.ScopeRunsRead}); got != 1 {
		t.Errorf("evil inbox for %q = %d rows, want 1 (cross-tenant leak)", user, got)
	}
	// Super-admin sees both tenants' interrupts.
	if got := totalFor("x", "ops", []string{auth.ScopeAdmin}); got != 2 {
		t.Errorf("admin inbox for %q = %d rows, want 2", user, got)
	}
}

// TestTenantStore_GetRunOpaqueCrossTenant locks the accessor's core posture: a
// cross-tenant run is indistinguishable from a missing one (both
// *store.ErrNotFound), while the owning tenant + admin resolve it.
func TestTenantStore_GetRunOpaqueCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_run")

	mustNotFound := func(ctx context.Context, label string) {
		if _, err := s.tenantStore(ctx).GetRun(ctx, runID); !isNotFound(err) {
			t.Errorf("%s: GetRun err=%v, want *store.ErrNotFound", label, err)
		}
	}
	mustResolve := func(ctx context.Context, label string) {
		if _, err := s.tenantStore(ctx).GetRun(ctx, runID); err != nil {
			t.Errorf("%s: GetRun err=%v, want nil", label, err)
		}
	}

	mustNotFound(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "evil", Subject: "m", Scopes: []string{auth.ScopeRunsRead}}), "cross-tenant")
	mustResolve(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}}), "own-tenant")
	mustResolve(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsRead}}), "same-tenant-other-subject")
	mustResolve(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}), "admin")
	mustResolve(context.Background(), "open-mode")

	// A genuinely-unknown run is the SAME error shape as cross-tenant (no oracle).
	uctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}})
	if _, err := s.tenantStore(uctx).GetRun(uctx, "r_does_not_exist"); !isNotFound(err) {
		t.Errorf("unknown run: GetRun err=%v, want *store.ErrNotFound", err)
	}
}
