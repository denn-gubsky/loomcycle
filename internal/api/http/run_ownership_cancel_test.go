package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Whole-run cancel is a run mutation: the tenant fold alone let an isolated
// member (substrate:user, which implies runs:create) cancel another user's run
// in its tenant, and tell from 200-vs-404 whether another user's agent id
// exists and how its run ended. These pin the ownership rule the steer,
// turn-cancel, retune and review siblings already apply.

// seedFinishedRunInTenant is seedRunInTenant for a run that has already ended.
func seedFinishedRunInTenant(t *testing.T, st store.Store, tenant, user, agentID string) {
	t.Helper()
	runID := seedRunInTenant(t, st, tenant, user, agentID)
	if err := st.FinishRun(context.Background(), runID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

func TestHandleCancelAgent_IsolatedMemberCannotCancelAnotherUsersRun(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	aliceLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "alice", "a_alice"), "a_alice", "alice")
	bobLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "bob", "a_bob"), "a_bob", "bob")
	seedFinishedRunInTenant(t, st, "acme", "alice", "a_alice_done")

	cancelAs := func(ctx context.Context, agentID string) (int, string) {
		r := httptest.NewRequest(http.MethodPost, "/v1/agents/"+agentID+"/cancel", nil)
		r.SetPathValue("agent_id", agentID)
		rr := httptest.NewRecorder()
		s.handleCancelAgent(rr, r.WithContext(ctx))
		return rr.Code, rr.Body.String()
	}
	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)
	if !auth.IsIsolated(auth.Principal{Scopes: []string{auth.ScopeUser}}, true) {
		t.Fatal("substrate:user alone must be an isolated member for this test to mean anything")
	}

	ghostCode, ghostBody := cancelAs(bob, "a_ghost")
	if ghostCode != http.StatusNotFound {
		t.Fatalf("unknown agent id: status %d, want 404", ghostCode)
	}
	// Live and finished alike: another user's run answers exactly as a missing
	// one does, so neither a cancel nor the idempotent 200 reaches it.
	for _, id := range []string{"a_alice", "a_alice_done"} {
		code, body := cancelAs(bob, id)
		if want := strings.ReplaceAll(ghostBody, "a_ghost", id); code != ghostCode || body != want {
			t.Errorf("isolated member cancelling %s = %d %q, want the unknown-id reply %d %q", id, code, body, ghostCode, want)
		}
	}
	if aliceLive() {
		t.Fatal("an isolated member cancelled another user's live run")
	}

	if code, body := cancelAs(bob, "a_bob"); code != http.StatusOK || !bobLive() {
		t.Errorf("isolated member cancelling its own run = %d %s (cancelled=%v), want 200 and cancelled", code, body, bobLive())
	}
	op := tenantPrincipalCtx("acme", "op", auth.ScopeTenant)
	if code, body := cancelAs(op, "a_alice"); code != http.StatusOK || !aliceLive() {
		t.Errorf("tenant operator cancelling a member's run = %d %s (cancelled=%v), want 200 and cancelled", code, body, aliceLive())
	}
}

// connector.CancelRun backs gRPC CancelAgent and MCP cancel_run.
func TestCancelRun_IsolatedMemberCannotCancelAnotherUsersRun(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	aliceLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "alice", "a_alice"), "a_alice", "alice")
	bobLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "bob", "a_bob"), "a_bob", "bob")
	seedFinishedRunInTenant(t, st, "acme", "alice", "a_alice_done")
	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)

	_, ghostErr := s.CancelRun(bob, "a_ghost", "")
	var nf *store.ErrNotFound
	if !errors.As(ghostErr, &nf) {
		t.Fatalf("unknown agent id: err=%v, want *store.ErrNotFound", ghostErr)
	}
	for _, id := range []string{"a_alice", "a_alice_done"} {
		res, err := s.CancelRun(bob, id, "")
		want := strings.ReplaceAll(ghostErr.Error(), "a_ghost", id)
		if !errors.As(err, &nf) || err.Error() != want {
			t.Errorf("isolated member cancelling %s = %+v, %v; want the unknown-id error %q", id, res, err, want)
		}
	}
	if aliceLive() {
		t.Fatal("an isolated member cancelled another user's live run through the connector")
	}

	if res, err := s.CancelRun(bob, "a_bob", ""); err != nil || !res.Cancelled || !bobLive() {
		t.Errorf("isolated member cancelling its own run = %+v, %v; want cancelled", res, err)
	}
	if res, err := s.CancelRun(tenantPrincipalCtx("acme", "op", auth.ScopeTenant), "a_alice", ""); err != nil || !res.Cancelled || !aliceLive() {
		t.Errorf("tenant operator cancelling a member's run = %+v, %v; want cancelled", res, err)
	}
}
