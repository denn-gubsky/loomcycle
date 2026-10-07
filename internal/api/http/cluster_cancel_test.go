package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// In a cluster a cancel for a run this replica does not hold goes through the
// cancel coordinator. A run whose replica is recorded dead ends there exactly
// as a run nothing holds ends anywhere else: cancelled, with the caller's
// reason, and with what is below it. A replica that is alive and merely
// silent keeps its run.

// silentBackplane carries a cancel to nobody: no replica ever acks.
type silentBackplane struct{}

func (silentBackplane) Publish(context.Context, string, []byte) error { return nil }
func (silentBackplane) Subscribe(context.Context, string) (<-chan coord.Event, error) {
	return make(chan coord.Event), nil
}
func (silentBackplane) Close() error { return nil }

// withCancelCoordinator gives b the real cluster canceller, wired as main.go
// wires it, over b's store and replicas table.
func withCancelCoordinator(t *testing.T, b *Server) {
	t.Helper()
	cc, err := coord.NewCancelCoordinator(coord.CancelCoordinatorConfig{
		Backplane:    silentBackplane{},
		ReplicaID:    b.replicaID,
		Store:        b.store,
		ReplicaStore: replicasOf(b),
		AckTimeout:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cc.SetOwnerGoneFinisher(b.FinishRunOwnerGone)
	b.cancelReg.SetClusterCanceller(cc)
}

func postAgentCancel(t *testing.T, ts *httptest.Server, agentID, reason string) cancelResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/agents/"+agentID+"/cancel", "application/json", strings.NewReader(`{"reason":"`+reason+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body cancelResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("cancel response (HTTP %d): %v", resp.StatusCode, err)
	}
	return body
}

func TestClusterCancel_ARunOnADeadReplicaEndsCancelledWithTheCallersReason(t *testing.T) {
	_, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	b.unheldRunGrace = time.Nanosecond
	withCancelCoordinator(t, b)
	ts := httptest.NewServer(b.Mux())
	defer ts.Close()
	run := leftRun(t, b.store, store.RunIdentity{AgentID: "a_cluster_run", ReplicaID: "replica-a"})
	below := leftRun(t, b.store, store.RunIdentity{AgentID: "a_cluster_below", ReplicaID: "replica-a", ParentAgentID: run.AgentID, ParentRunID: run.ID})

	// Its replica is alive and does not answer: nothing is finished.
	if got := postAgentCancel(t, ts, run.AgentID, "stop it"); got.Cancelled || got.Reason != coord.ReasonOwnerUnreachable {
		t.Errorf("cancel while its replica is alive but silent = %+v, want not cancelled, %q", got, coord.ReasonOwnerUnreachable)
	}
	for _, r := range []store.Run{run, below} {
		if got := runNow(t, b.store, r.ID); got.Status != store.RunRunning {
			t.Fatalf("run %s on a live replica was finished %s on a missing ack", r.AgentID, got.Status)
		}
	}

	replicasOf(b).set("replica-a", false)
	got := postAgentCancel(t, ts, run.AgentID, "stop it")
	if !got.Cancelled || got.Reason != coord.ReasonOwnerDeadCancelled || !slices.Contains(got.Cascaded, below.AgentID) {
		t.Errorf("cancel once its replica is recorded dead = %+v, want cancelled as %q, cascaded to the run below", got, coord.ReasonOwnerDeadCancelled)
	}
	for _, r := range []store.Run{run, below} {
		now := runNow(t, b.store, r.ID)
		if now.Status != store.RunCancelled || now.StopReason != "stop it" {
			t.Errorf("run %s = %s %q (%s), want cancelled with the caller's reason", r.AgentID, now.Status, now.StopReason, now.ErrorMsg)
		}
	}

	// The connector's cancel — what gRPC CancelAgent and MCP cancel_run call.
	other := leftRun(t, b.store, store.RunIdentity{AgentID: "a_cluster_other", ReplicaID: "replica-a"})
	if res, err := b.CancelRun(context.Background(), other.AgentID, "stop it"); err != nil || !res.Cancelled {
		t.Errorf("connector cancel of a run on a dead replica = %+v %v, want cancelled", res, err)
	}
	if now := runNow(t, b.store, other.ID); now.Status != store.RunCancelled || now.StopReason != "stop it" {
		t.Errorf("row = %s %q, want cancelled with the caller's reason", now.Status, now.StopReason)
	}
}

// The runtime's own cancels take the same path: a resident child's close and
// a parent's timeout on a paused child end the run when its replica is dead,
// and report it unreachable — not closed — while it is alive and silent.
func TestClusterCancel_TheRuntimesOwnCancelsEndARunOnADeadReplica(t *testing.T) {
	_, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	b.unheldRunGrace = time.Nanosecond
	withCancelCoordinator(t, b)
	ctx := residentParentCtx("parent-agent", "")
	child := leftResident(t, b.store, "a_cluster_resident", "replica-a")

	if err := b.closeResidentChild(ctx, child.ID); err == nil || !strings.Contains(err.Error(), "not held by this replica") {
		t.Errorf("close while the child's replica is alive but silent: %v, want it unreachable", err)
	}
	if got := runNow(t, b.store, child.ID); got.Status != store.RunRunning {
		t.Fatalf("a resident child on a live replica was finished %s", got.Status)
	}
	paused := leftRun(t, b.store, store.RunIdentity{AgentID: "a_cluster_paused", ReplicaID: "replica-a"})
	if err := b.store.SetRunPauseState(context.Background(), paused.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	replicasOf(b).set("replica-a", false)
	if err := b.closeResidentChild(ctx, child.ID); err != nil {
		t.Fatalf("close once the child's replica is recorded dead: %v", err)
	}
	if got := runNow(t, b.store, child.ID); got.Status != store.RunCancelled || got.StopReason != residentReasonClosedByParent {
		t.Errorf("resident row = %s %q, want cancelled as closed by its parent", got.Status, got.StopReason)
	}
	b.cancelRestoredChild(tools.ChildSpec{RunID: paused.ID, Agent: "child", Index: -1}, builtin.ChildTimeoutCause(100))
	if got := runNow(t, b.store, paused.ID); got.Status != store.RunCancelled || got.PauseState == store.PauseStatePaused {
		t.Errorf("paused child = %s pause %q (%q), want cancelled and no longer paused", got.Status, got.PauseState, got.StopReason)
	}
}

// A schedule's replace stops the run before it: an agent run through the
// cancel coordinator, a team walk through the turn-cancel route. Either, on
// a replica recorded dead, is ended as cancelled and the slot freed; on a
// live replica that does not answer it is left, and the slot is not.
func TestClusterCancel_AScheduleReplaceEndsARunOnADeadReplica(t *testing.T) {
	_, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	b.unheldRunGrace = time.Nanosecond
	withCancelCoordinator(t, b)
	ctx := context.Background()
	agentRun := leftRun(t, b.store, store.RunIdentity{AgentID: "a_scheduled", ReplicaID: "replica-a"})
	walk := leftRun(t, b.store, store.RunIdentity{AgentID: teamWalkAgentPrefix + "nightly", ReplicaID: "replica-a"})

	for _, r := range []store.Run{agentRun, walk} {
		if stopped, _ := b.CancelScheduledRun(ctx, r.ID, "replaced"); stopped {
			t.Errorf("replace of %s on a live, silent replica reported it stopped", r.AgentID)
		}
		if got := runNow(t, b.store, r.ID); got.Status != store.RunRunning {
			t.Fatalf("%s on a live replica was finished %s", r.AgentID, got.Status)
		}
	}
	replicasOf(b).set("replica-a", false)
	for _, r := range []store.Run{agentRun, walk} {
		if stopped, err := b.CancelScheduledRun(ctx, r.ID, "replaced"); err != nil || !stopped {
			t.Errorf("replace of %s on a dead replica = %v %v, want stopped", r.AgentID, stopped, err)
		}
		if got := runNow(t, b.store, r.ID); got.Status != store.RunCancelled || got.StopReason != "replaced" {
			t.Errorf("%s = %s %q, want cancelled with the replace's reason", r.AgentID, got.Status, got.StopReason)
		}
	}
}
