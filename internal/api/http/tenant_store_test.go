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
