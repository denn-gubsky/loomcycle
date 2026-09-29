package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// connector.Run is what MCP get_run returns. It lacked the lineage, the
// interactive / replica marks and the awaited state the HTTP and gRPC reads
// carry, so an MCP caller could neither attribute a sub-agent's cost to its
// root request nor tell a run parked on a channel from one that is working.
// Both single-run connector reads must carry them.
func TestConnectorRunReads_CarryLineageAndAwaitedStateForAParkedRun(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_parked", UserID: "alice", Interactive: true,
		ParentContext: &store.ParentContext{FunctionKey: "fk-1", WalkID: "walk-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AppendEvent(ctx, run.ID, "tool_call",
		[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`)); err != nil {
		t.Fatal(err)
	}

	byAgent, err := srv.GetRun(ctx, "a_parked")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	byRun, err := srv.GetRunByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRunByRunID: %v", err)
	}
	for name, got := range map[string]connector.Run{"GetRun": byAgent, "GetRunByRunID": byRun} {
		if got.RunID != run.ID {
			t.Errorf("%s run_id = %q, want %q", name, got.RunID, run.ID)
		}
		if got.ParentContext == nil || got.ParentContext.FunctionKey != "fk-1" || got.ParentContext.WalkID != "walk-1" {
			t.Errorf("%s parent_context = %+v, want the run's lineage", name, got.ParentContext)
		}
		if got.AwaitedState != "channel" || got.AwaitedOn != "findings" {
			t.Errorf("%s awaited = (%q, %q), want (channel, findings)", name, got.AwaitedState, got.AwaitedOn)
		}
		if !got.Interactive {
			t.Errorf("%s interactive = false, want true", name)
		}
	}
	// SQLite does not persist replica_id, so its copy is checked on the row
	// conversion itself.
	if got := storeRunToConnector(store.Run{ReplicaID: "replica-2"}); got.ReplicaID != "replica-2" {
		t.Errorf("replica_id = %q, want replica-2", got.ReplicaID)
	}
}

// GetRunByRunID applies the gates GET /v1/runs/{run_id} does: another tenant
// and an isolated member reading another user's run get the same opaque
// not-found a missing run gets.
func TestConnectorGetRunByRunID_FoldsOtherTenantAndOtherUserIntoNotFound(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	run := seedTenantRun(t, srv.store, "acme", "alice", "a_alice")

	isNotFound := func(err error) bool {
		var nf *store.ErrNotFound
		return errors.As(err, &nf)
	}
	if _, err := srv.GetRunByRunID(tenantPrincipal("other"), run.ID); !isNotFound(err) {
		t.Errorf("another tenant's read = %v, want the opaque not-found", err)
	}
	bob := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser}})
	if _, err := srv.GetRunByRunID(bob, run.ID); !isNotFound(err) {
		t.Errorf("isolated member reading another user's run = %v, want the opaque not-found", err)
	}
	if got, err := srv.GetRunByRunID(tenantPrincipal("acme"), run.ID); err != nil || got.RunID != run.ID {
		t.Errorf("own-tenant read = (%q, %v), want the run", got.RunID, err)
	}
	if _, err := srv.GetRunByRunID(tenantPrincipal("acme"), "r_missing"); !isNotFound(err) {
		t.Errorf("unknown run id = %v, want not-found", err)
	}
}
