package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedRunID is seedRun returning the run id — GetRun's key.
func seedRunID(t *testing.T, st store.Store, tenant, user, agentID string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "default", user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: tenant})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
}

// Two runs share an agent id — as every walk of a team shares `team:<name>` —
// and each is read by its own run id; GetAgent can answer only the latest.
func TestGrpcGetRun_ReadsEachRunOfASharedAgentID(t *testing.T) {
	adapter, st := tenantTestServer(t)
	ctx := context.Background()
	first := seedRunID(t, st, "acme", "alice", "a_shared")
	if err := st.FinishRun(ctx, first, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	second := seedRunID(t, st, "acme", "alice", "a_shared")

	op := scopedCtx("acme", "op", auth.ScopeTenant)
	for _, tc := range []struct{ runID, status string }{
		{first, string(store.RunCompleted)},
		{second, string(store.RunRunning)},
	} {
		got, err := adapter.GetRun(op, &loomcyclepb.GetRunRequest{RunId: tc.runID})
		if err != nil {
			t.Fatalf("GetRun(%s): %v", tc.runID, err)
		}
		if got.GetRunId() != tc.runID || got.GetStatus() != tc.status {
			t.Errorf("GetRun(%s) = (run %q, status %q), want (%q, %q)", tc.runID, got.GetRunId(), got.GetStatus(), tc.runID, tc.status)
		}
	}
}

func TestGrpcGetRun_CrossTenantFoldsNotFound(t *testing.T) {
	adapter, st := tenantTestServer(t)
	runID := seedRunID(t, st, "evil", "mallory", "a_evil")

	if _, err := adapter.GetRun(scopedCtx("acme", "alice", auth.ScopeRunsRead), &loomcyclepb.GetRunRequest{RunId: runID}); status.Code(err) != codes.NotFound {
		t.Errorf("cross-tenant GetRun code=%s, want NotFound", status.Code(err))
	}
	if _, err := adapter.GetRun(scopedCtx("acme", "ops", auth.ScopeAdmin), &loomcyclepb.GetRunRequest{RunId: runID}); err != nil {
		t.Errorf("admin GetRun cross-tenant: %v, want ok", err)
	}
}

func TestGrpcGetRun_IsolatedMemberCannotReadAnotherUsersRun(t *testing.T) {
	adapter, st := tenantTestServer(t)
	alice := seedRunID(t, st, "acme", "alice", "a_alice")
	bob := seedRunID(t, st, "acme", "bob", "a_bob")

	member := scopedCtx("acme", "bob", auth.ScopeUser)
	if _, err := adapter.GetRun(member, &loomcyclepb.GetRunRequest{RunId: alice}); status.Code(err) != codes.NotFound {
		t.Errorf("isolated member reading another user's run: code=%s, want NotFound", status.Code(err))
	}
	if _, err := adapter.GetRun(member, &loomcyclepb.GetRunRequest{RunId: bob}); err != nil {
		t.Errorf("isolated member reading its own run: %v, want ok", err)
	}
	if _, err := adapter.GetRun(scopedCtx("acme", "op", auth.ScopeTenant), &loomcyclepb.GetRunRequest{RunId: alice}); err != nil {
		t.Errorf("tenant operator reading a member's run: %v, want ok", err)
	}
}

func TestGrpcGetRun_UnknownIsNotFoundAndMalformedIsInvalid(t *testing.T) {
	adapter, _ := tenantTestServer(t)
	if _, err := adapter.GetRun(context.Background(), &loomcyclepb.GetRunRequest{RunId: "r_nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown run id code=%s, want NotFound", status.Code(err))
	}
	if _, err := adapter.GetRun(context.Background(), &loomcyclepb.GetRunRequest{RunId: "team:triage"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("malformed run id code=%s, want InvalidArgument", status.Code(err))
	}
}

// live is about THIS run: without the injected check an agent id's registry
// entry counts only when it carries this run id; with it (the HTTP server's,
// which sees team walks) the injected answer wins.
func TestGrpcGetRun_LiveIsTheRunNotItsAgentID(t *testing.T) {
	st := newTestStore(t)
	reg := cancel.NewRegistry()
	runID := seedRunID(t, st, "", "alice", "a_shared")
	_, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	if err := reg.Register(cancel.Entry{AgentID: "a_shared", RunID: "r_newer"}, stop); err != nil {
		t.Fatal(err)
	}

	plain := New(Config{Store: st, CancelReg: reg})
	if got, _ := plain.GetRun(context.Background(), &loomcyclepb.GetRunRequest{RunId: runID}); got.GetLive() {
		t.Error("a run read as live because its agent id's newer run is registered")
	}

	var asked string
	injected := New(Config{Store: st, CancelReg: reg, RunLive: func(_, id string) bool { asked = id; return true }})
	if got, _ := injected.GetRun(context.Background(), &loomcyclepb.GetRunRequest{RunId: runID}); !got.GetLive() || asked != runID {
		t.Errorf("injected liveness: live=%v asked=%q, want true for %q — a walk's liveness is only visible through it", got.GetLive(), asked, runID)
	}
}

func TestRequiredScopeForRPC_GetRunIsARunsRead(t *testing.T) {
	if got := requiredScopeForRPC(grpcMethodPrefix + "GetRun"); got != auth.ScopeRunsRead {
		t.Errorf("GetRun scope = %q, want %q (an unlisted RPC defaults to admin)", got, auth.ScopeRunsRead)
	}
}
