package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// These regression tests close the cross-tenant cancel + interrupt-resolve gap:
// both mutations were keyed only by id with no tenant-ownership check (unlike
// the steer + compact siblings), so a tenant-B token could cancel a tenant-A
// run (cross-tenant DoS, cluster-broadcast) or resolve a tenant-A pending
// interrupt (steering another tenant's paused run). Covered on all three
// transports: the HTTP handlers directly, and connector.{CancelRun,
// InterruptionResolve} which back gRPC + MCP.

func tenantPrincipalCtx(tenant, subject string, scopes ...string) context.Context {
	return auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes})
}

// registerLiveCancel registers a live cancel handle for agentID; the returned
// func reports whether it was ever cancelled.
func registerLiveCancel(t *testing.T, s *Server, runID, agentID, user string) func() bool {
	t.Helper()
	var cancelled atomic.Bool
	if err := s.cancelReg.Register(cancel.Entry{
		AgentID: agentID, RunID: runID, UserID: user, StartedAt: time.Now(),
	}, func(error) { cancelled.Store(true) }); err != nil {
		t.Fatalf("cancelReg.Register: %v", err)
	}
	t.Cleanup(func() { s.cancelReg.Deregister(agentID) })
	return cancelled.Load
}

func TestHandleCancelAgent_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_cx")
	wasCancelled := registerLiveCancel(t, s, runID, "a_acme_cx", "alice")

	cancelFor := func(tenant, subject string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/agents/a_acme_cx/cancel", nil)
		r.SetPathValue("agent_id", "a_acme_cx")
		r = r.WithContext(tenantPrincipalCtx(tenant, subject, auth.ScopeRunsCreate))
		rr := httptest.NewRecorder()
		s.handleCancelAgent(rr, r)
		return rr.Code
	}

	// Cross-tenant → opaque 404 and the run is NOT cancelled.
	if code := cancelFor("evil", "mallory"); code != http.StatusNotFound {
		t.Errorf("cross-tenant cancel status=%d, want 404", code)
	}
	if wasCancelled() {
		t.Fatal("cross-tenant cancel actually cancelled another tenant's run")
	}
	// Own tenant → cancels.
	if code := cancelFor("acme", "alice"); code != http.StatusOK {
		t.Errorf("own-tenant cancel status=%d, want 200", code)
	}
	if !wasCancelled() {
		t.Error("own-tenant cancel did not cancel the run")
	}
}

func TestCancelRun_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_conn")
	wasCancelled := registerLiveCancel(t, s, runID, "a_acme_conn", "alice")

	// Cross-tenant (gRPC/MCP path) → ErrNotFound, run untouched.
	_, err := s.CancelRun(tenantPrincipalCtx("evil", "mallory", auth.ScopeRunsCreate), "a_acme_conn", "")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("cross-tenant CancelRun err=%v, want *store.ErrNotFound", err)
	}
	if wasCancelled() {
		t.Fatal("cross-tenant CancelRun cancelled another tenant's run")
	}
	// Own tenant → cancels.
	if _, err := s.CancelRun(tenantPrincipalCtx("acme", "alice", auth.ScopeRunsCreate), "a_acme_conn", ""); err != nil {
		t.Errorf("own-tenant CancelRun: %v", err)
	}
	if !wasCancelled() {
		t.Error("own-tenant CancelRun did not cancel the run")
	}
}

func TestHandleResolveInterrupt_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_intr")
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: "intr_http", RunID: runID, UserID: "alice", Question: "approve?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}

	resolveFor := func(tenant, subject string) int {
		r := httptest.NewRequest(http.MethodPost,
			"/v1/runs/"+runID+"/interrupts/intr_http/resolve",
			strings.NewReader(`{"kind":"question","answer":"yes"}`))
		r.SetPathValue("run_id", runID)
		r.SetPathValue("interrupt_id", "intr_http")
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(tenantPrincipalCtx(tenant, subject, auth.ScopeRunsCreate))
		rr := httptest.NewRecorder()
		s.handleResolveInterrupt(rr, r)
		return rr.Code
	}

	// Cross-tenant → opaque 404 and the interrupt stays PENDING (not steered).
	if code := resolveFor("evil", "mallory"); code != http.StatusNotFound {
		t.Errorf("cross-tenant resolve status=%d, want 404", code)
	}
	if row, _ := st.InterruptGet(ctx, "intr_http"); row.Status != store.InterruptStatusPending {
		t.Fatalf("cross-tenant resolve changed interrupt status to %q — another tenant's run was steered", row.Status)
	}
	// Own tenant → the gate passes (not a 404).
	if code := resolveFor("acme", "alice"); code == http.StatusNotFound {
		t.Errorf("own-tenant resolve got 404 — the tenant gate rejected a legitimate caller")
	}
}

func TestInterruptionResolve_RejectsCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_intr2")
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: "intr_conn", RunID: runID, UserID: "alice", Question: "approve?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}

	// Cross-tenant (MCP path) → ErrNotFound, interrupt stays pending.
	_, err := s.InterruptionResolve(tenantPrincipalCtx("evil", "mallory", auth.ScopeRunsCreate),
		connector.InterruptionResolveRequest{InterruptID: "intr_conn", RunID: runID, Answer: "yes"})
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("cross-tenant InterruptionResolve err=%v, want *store.ErrNotFound", err)
	}
	if row, _ := st.InterruptGet(ctx, "intr_conn"); row.Status != store.InterruptStatusPending {
		t.Fatalf("cross-tenant InterruptionResolve changed status to %q", row.Status)
	}
	// Own tenant → the gate passes (no ErrNotFound; may proceed to resolve).
	_, err = s.InterruptionResolve(tenantPrincipalCtx("acme", "alice", auth.ScopeRunsCreate),
		connector.InterruptionResolveRequest{InterruptID: "intr_conn", RunID: runID, Answer: "yes"})
	if errors.As(err, &nf) {
		t.Errorf("own-tenant InterruptionResolve got ErrNotFound — the tenant gate rejected a legitimate caller")
	}
}
