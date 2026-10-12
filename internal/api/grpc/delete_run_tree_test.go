package grpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DeleteRunTree over gRPC, against the REAL HTTP server as the connector: the
// authorization, the walk and the delete live there, and the RPC must not
// answer differently from the HTTP routes.

// seedFinishedRun writes one run of (tenant, user) under parent with an event,
// in a session of its own.
func seedFinishedRun(t *testing.T, st store.Store, tenant, user, agent, parent string, done bool) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "agentx", user)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agent, ParentRunID: parent, UserID: user, TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, r.ID, "text", []byte(`{"text":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if done {
		if err := st.FinishRun(ctx, r.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func stored(t *testing.T, st store.Store, runID string) bool {
	t.Helper()
	_, err := st.GetRun(context.Background(), runID)
	var nf *store.ErrNotFound
	if err != nil && !errors.As(err, &nf) {
		t.Fatal(err)
	}
	return err == nil
}

// The interceptor's gate: runs:delete, which an admin and a tenant operator
// hold and a member or an isolated user does not unless it was granted.
func TestGrpcEnforceScope_DeleteRunTreeNeedsRunsDelete(t *testing.T) {
	const method = grpcMethodPrefix + "DeleteRunTree"
	if got := requiredScopeForRPC(method); got != auth.ScopeRunsDelete {
		t.Fatalf("DeleteRunTree scope = %q, want %q", got, auth.ScopeRunsDelete)
	}
	member, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	for name, scopes := range map[string][]string{
		"a member's delegated scopes": member,
		"an isolated user":            {auth.ScopeUser},
		"runs:create + runs:read":     {auth.ScopeRunsCreate, auth.ScopeRunsRead},
	} {
		if err := enforceScope(scopedCtx("acme", "alice", scopes...), method); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v, want PermissionDenied", name, err)
		}
	}
	for name, scopes := range map[string][]string{
		"substrate:admin":                   {auth.ScopeAdmin},
		"substrate:tenant":                  {auth.ScopeTenant},
		"a member granted runs:delete":      append(append([]string{}, member...), auth.ScopeRunsDelete),
		"an isolated user granted the same": {auth.ScopeUser, auth.ScopeRunsDelete},
	} {
		if err := enforceScope(scopedCtx("acme", "alice", scopes...), method); err != nil {
			t.Errorf("%s: %v, want admitted", name, err)
		}
	}
}

func TestGrpcDeleteRunTree_DeletesTheRunAndItsDescendants(t *testing.T) {
	adapter, _, st, _ := ownershipGRPC(t)
	parent := seedFinishedRun(t, st, "acme", "alice", "lead", "", true)
	child := seedFinishedRun(t, st, "acme", "alice", "worker", parent.ID, true)
	other := seedFinishedRun(t, st, "acme", "alice", "elsewhere", "", true)
	op := scopedCtx("acme", "op", auth.ScopeTenant)

	res, err := adapter.DeleteRunTree(op, &loomcyclepb.DeleteRunTreeRequest{RunId: parent.ID})
	if err != nil {
		t.Fatalf("DeleteRunTree: %v", err)
	}
	c := res.GetCounts()
	if res.GetDeletedRuns() != 2 || c["runs"] != 2 || c["events"] != 2 || c["sessions"] != 2 || res.GetRunId() != parent.ID {
		t.Errorf("result = %+v, want 2 runs, 2 events, 2 sessions", res)
	}
	if res.GetKept()["usage_ledger"] == "" || len(res.GetNotReached()) == 0 {
		t.Errorf("result names nothing kept or unreached: %+v", res)
	}
	if stored(t, st, parent.ID) || stored(t, st, child.ID) || !stored(t, st, other.ID) {
		t.Error("stored runs after the delete: want the tree gone and the other run kept")
	}

	// Repeated: zero counts, no error. So is another tenant's caller.
	again, err := adapter.DeleteRunTree(op, &loomcyclepb.DeleteRunTreeRequest{RunId: parent.ID})
	if err != nil || again.GetDeletedRuns() != 0 || len(again.GetCounts()) == 0 || len(again.GetNotReached()) != 0 {
		t.Errorf("second call = %+v, %v; want zero counts and no error", again, err)
	}
	foreign, err := adapter.DeleteRunTree(scopedCtx("other", "op2", auth.ScopeTenant), &loomcyclepb.DeleteRunTreeRequest{RunId: other.ID})
	if err != nil || foreign.GetDeletedRuns() != 0 || !stored(t, st, other.ID) {
		t.Errorf("another tenant's call = %+v, %v; want zero counts and the run kept", foreign, err)
	}

	// By session.
	bySession, err := adapter.DeleteRunTree(op, &loomcyclepb.DeleteRunTreeRequest{SessionId: other.SessionID})
	if err != nil || bySession.GetDeletedRuns() != 1 || bySession.GetSessionId() != other.SessionID || stored(t, st, other.ID) {
		t.Errorf("by session = %+v, %v; want the session's 1 run deleted", bySession, err)
	}
}

func TestGrpcDeleteRunTree_MapsTheRefusals(t *testing.T) {
	adapter, _, st, _ := ownershipGRPC(t)
	parent := seedFinishedRun(t, st, "acme", "alice", "lead", "", true)
	live := seedFinishedRun(t, st, "acme", "alice", "worker", parent.ID, false)
	op := scopedCtx("acme", "op", auth.ScopeTenant)

	for name, tc := range map[string]struct {
		ctx  context.Context
		req  *loomcyclepb.DeleteRunTreeRequest
		want codes.Code
	}{
		"a live run in the tree": {op, &loomcyclepb.DeleteRunTreeRequest{RunId: parent.ID}, codes.FailedPrecondition},
		"neither id":             {op, &loomcyclepb.DeleteRunTreeRequest{}, codes.InvalidArgument},
		"both ids":               {op, &loomcyclepb.DeleteRunTreeRequest{RunId: parent.ID, SessionId: parent.SessionID}, codes.InvalidArgument},
		"a malformed run id":     {op, &loomcyclepb.DeleteRunTreeRequest{RunId: "team:triage"}, codes.InvalidArgument},
		// Past the interceptor (a handler called directly): the connector
		// still refuses a caller without the scope.
		"no runs:delete, isolated": {scopedCtx("acme", "alice", auth.ScopeUser), &loomcyclepb.DeleteRunTreeRequest{RunId: parent.ID}, codes.PermissionDenied},
		"no runs:delete, member":   {scopedCtx("acme", "alice", auth.ScopeRunsCreate, auth.ScopeRunsRead), &loomcyclepb.DeleteRunTreeRequest{RunId: parent.ID}, codes.PermissionDenied},
	} {
		if _, err := adapter.DeleteRunTree(tc.ctx, tc.req); status.Code(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
	if !stored(t, st, parent.ID) || !stored(t, st, live.ID) {
		t.Error("a refused call deleted a run")
	}
}
