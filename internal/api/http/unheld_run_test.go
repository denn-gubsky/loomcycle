package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A run's row can outlive its loop: a crash, a replica that died, a pause
// never resumed. A cancel aimed at such a row finishes it as cancelled with
// the canceller's reason, and never touches a row a live loop still holds.

// leftRun files a running row with no loop behind it, as a crash leaves one.
func leftRun(t *testing.T, st store.Store, identity store.RunIdentity) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, identity.TenantID, "child", identity.UserID)
	if err != nil {
		t.Fatal(err)
	}
	identity.Model = "stub-model"
	run, err := st.CreateRun(ctx, sess.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// leftResident is leftRun for a resident child parked after one turn.
func leftResident(t *testing.T, st store.Store, agentID, replica string) store.Run {
	t.Helper()
	run := leftRun(t, st, store.RunIdentity{AgentID: agentID, ReplicaID: replica,
		RunConfig: runConfigRecord{Spawn: &spawnRecord{Resident: true}}.marshal()})
	for _, ev := range []struct{ typ, payload string }{
		{"user_input", `[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]`},
		{string(providers.EventText), `{"type":"text","text":"last answer"}`},
		{string(providers.EventAwaitingInput), `{"type":"awaiting_input"}`},
	} {
		if err := st.AppendEvent(context.Background(), run.ID, ev.typ, []byte(ev.payload)); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func runNow(t *testing.T, st store.Store, id string) store.Run {
	t.Helper()
	run, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// On a single node a resident child a crash left behind is closed by its
// parent's close, and ended by a cancel of a turn it will never run; a row
// started a moment ago is left alone, since its loop may be about to
// register.
func TestUnheldRun_AResidentChildLeftByACrashIsEndedByCloseAndCancel(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := residentParentCtx("parent-agent", "")

	fresh := leftResident(t, srv.store, "a_left_fresh", "")
	if err := srv.closeResidentChild(ctx, fresh.ID); err == nil || !strings.Contains(err.Error(), "not held by this replica") {
		t.Errorf("close of a row started a moment ago: %v, want it left alone", err)
	}
	if got := runNow(t, srv.store, fresh.ID); got.Status != store.RunRunning {
		t.Fatalf("a row inside the grace was finished %s", got.Status)
	}

	srv.unheldRunGrace = time.Nanosecond
	closed := leftResident(t, srv.store, "a_left_closed", "")
	if err := srv.closeResidentChild(ctx, closed.ID); err != nil {
		t.Fatalf("close of a child left by a crash: %v", err)
	}
	if got := runNow(t, srv.store, closed.ID); got.Status != store.RunCancelled || got.StopReason != residentReasonClosedByParent {
		t.Errorf("closed row = %s %q, want cancelled as closed by its parent", got.Status, got.StopReason)
	}
	if _, _, err := srv.pollResidentChild(ctx, closed.ID, 0); err == nil || !strings.Contains(err.Error(), "was closed by its parent") {
		t.Errorf("poll after the close: %v, want it closed by its parent", err)
	}

	cancelled := leftResident(t, srv.store, "a_left_cancelled", "")
	if _, _, err := srv.cancelResidentChildTurn(ctx, cancelled.ID); err == nil || !strings.Contains(err.Error(), "the replica running it is gone") {
		t.Errorf("cancel of a child left by a crash: %v, want it ended as gone", err)
	}
	if got := runNow(t, srv.store, cancelled.ID); got.Status != store.RunCancelled {
		t.Errorf("cancelled row = %s, want cancelled", got.Status)
	}
	if _, _, err := srv.sendResidentChild(ctx, leftResident(t, srv.store, "a_left_sent", "").ID, "next", 0); err == nil || !strings.Contains(err.Error(), "the replica running it is gone") {
		t.Errorf("send to a child left by a crash: %v, want it ended as gone", err)
	}
}

// In a cluster only the replicas table says a row's replica is gone: while
// it is alive the row is left and the caller told there is no route; once it
// is not, the close ends the row. A failed read of the table, and a row that
// names no replica, are not proof either.
func TestUnheldRun_AClusterEndsARowOnlyWhenItsReplicaIsRecordedGone(t *testing.T) {
	a, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	_ = a
	b.unheldRunGrace = time.Nanosecond
	ctx := residentParentCtx("parent-agent", "")
	child := leftResident(t, b.store, "a_left_cluster", "replica-a")

	if err := b.closeResidentChild(ctx, child.ID); err == nil || !strings.Contains(err.Error(), "not held by this replica") {
		t.Errorf("close while its replica is alive: %v, want no route", err)
	}
	replicasOf(b).set("replica-a", false)
	replicasOf(b).err = errors.New("replicas table unreachable")
	if err := b.closeResidentChild(ctx, child.ID); err == nil {
		t.Error("close while the replicas table cannot be read succeeded")
	}
	if got := runNow(t, b.store, child.ID); got.Status != store.RunRunning {
		t.Fatalf("the row was finished %s without proof its replica is gone", got.Status)
	}
	unstamped := leftResident(t, b.store, "a_left_unstamped", "")
	replicasOf(b).err = nil
	if err := b.closeResidentChild(ctx, unstamped.ID); err == nil || runNow(t, b.store, unstamped.ID).Status != store.RunRunning {
		t.Errorf("close of a row naming no replica in a cluster: %v, want it left alone", err)
	}

	if err := b.closeResidentChild(ctx, child.ID); err != nil {
		t.Fatalf("close once its replica is recorded gone: %v", err)
	}
	if got := runNow(t, b.store, child.ID); got.Status != store.RunCancelled || got.StopReason != residentReasonClosedByParent {
		t.Errorf("row = %s %q, want cancelled as closed by its parent", got.Status, got.StopReason)
	}
}

// A paused child its parent's timeout cancels, and that no resume ever
// started, ends cancelled — with everything below it that nobody holds — and
// is not found by a later resume pass. Before, the parent was told "timeout"
// while the row stayed paused, and the next pass started the child.
func TestUnheldRun_ACancelledPausedChildIsNotResumedLater(t *testing.T) {
	srv := newResidentTestServer(t)
	srv.unheldRunGrace = time.Nanosecond
	ctx := context.Background()
	parent := leftRun(t, srv.store, store.RunIdentity{AgentID: "a_parent"})
	child := leftRun(t, srv.store, store.RunIdentity{AgentID: "a_paused_child", ParentAgentID: parent.AgentID, ParentRunID: parent.ID})
	below := leftRun(t, srv.store, store.RunIdentity{AgentID: "a_below", ParentAgentID: child.AgentID, ParentRunID: child.ID})
	if err := srv.store.SetRunPauseState(ctx, child.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	cause := builtin.ChildTimeoutCause(100)
	srv.cancelRestoredChild(tools.ChildSpec{RunID: child.ID, Agent: "child", Index: -1}, cause)

	got := runNow(t, srv.store, child.ID)
	if got.Status != store.RunCancelled || got.StopReason == "" || got.PauseState == store.PauseStatePaused {
		t.Errorf("the paused child = %s %q pause %q, want cancelled with the timeout's reason and no longer paused", got.Status, got.StopReason, got.PauseState)
	}
	if got := runNow(t, srv.store, below.ID); got.Status != store.RunCancelled {
		t.Errorf("the run below it = %s, want cancelled with it", got.Status)
	}
	paused, err := srv.store.ListPausedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range paused {
		if r.ID == child.ID {
			t.Error("a later resume pass would still find the cancelled child")
		}
	}

	// The fan-out reconcile's timeout cancels the same way.
	other := leftRun(t, srv.store, store.RunIdentity{AgentID: "a_other_child", ParentAgentID: parent.AgentID, ParentRunID: parent.ID})
	srv.cancelRunWherever(ctx, other, "timed out")
	if got := runNow(t, srv.store, other.ID); got.Status != store.RunCancelled || got.StopReason != "timed out" {
		t.Errorf("a child cancelled by its row = %s %q, want cancelled with the reason", got.Status, got.StopReason)
	}
}

// The run-id cancel and the agent cancel end a run whose replica is gone, on
// every transport; while that replica is alive they answer as before.
func TestUnheldRun_TheCancelRoutesEndARunWhoseReplicaIsGone(t *testing.T) {
	_, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	b.unheldRunGrace = time.Nanosecond
	ctx := context.Background()

	byRun := leftRun(t, b.store, store.RunIdentity{AgentID: "a_gone_by_run", ReplicaID: "replica-a"})
	if _, _, err := b.CancelTurn(ctx, byRun.ID, "stop it"); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("run-id cancel while its replica is alive: %v, want no in-flight run", err)
	}
	if got := runNow(t, b.store, byRun.ID); got.Status != store.RunRunning {
		t.Fatalf("a run on a live replica was finished %s", got.Status)
	}
	replicasOf(b).set("replica-a", false)
	stopped, parked, err := b.CancelTurn(ctx, byRun.ID, "stop it")
	if err != nil || !stopped || parked {
		t.Errorf("run-id cancel once its replica is gone = %v %v %v, want stopped", stopped, parked, err)
	}
	if got := runNow(t, b.store, byRun.ID); got.Status != store.RunCancelled || got.StopReason != "stop it" {
		t.Errorf("row = %s %q, want cancelled with the caller's reason", got.Status, got.StopReason)
	}

	byConnector := leftRun(t, b.store, store.RunIdentity{AgentID: "a_gone_by_connector", ReplicaID: "replica-a"})
	if res, err := b.CancelRun(ctx, byConnector.AgentID, "stop it"); err != nil || !res.Cancelled {
		t.Errorf("agent cancel (connector) once its replica is gone = %+v %v, want cancelled", res, err)
	}
	if got := runNow(t, b.store, byConnector.ID); got.Status != store.RunCancelled {
		t.Errorf("row = %s, want cancelled", got.Status)
	}

	byHTTP := leftRun(t, b.store, store.RunIdentity{AgentID: "a_gone_by_http", ReplicaID: "replica-a"})
	ts := httptest.NewServer(b.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/agents/"+byHTTP.AgentID+"/cancel", "application/json", strings.NewReader(`{"reason":"stop it"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body cancelResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.Cancelled {
		t.Errorf("agent cancel (HTTP) once its replica is gone = %+v (%v), want cancelled", body, err)
	}
	if got := runNow(t, b.store, byHTTP.ID); got.Status != store.RunCancelled || got.StopReason != "stop it" {
		t.Errorf("row = %s %q, want cancelled with the caller's reason", got.Status, got.StopReason)
	}
}

// A run a live loop holds here is never finished by its row, whatever the
// replicas table says: the cancel reaches the loop, which ends it itself.
func TestUnheldRun_ARunHeldHereIsNeverFinishedByItsRow(t *testing.T) {
	srv := newResidentTestServer(t)
	srv.unheldRunGrace = time.Nanosecond
	ctx := residentParentCtx("parent-agent", "")
	runID, _, _, err := srv.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if unheld, err := srv.runUnheld(context.Background(), runNow(t, srv.store, runID)); err != nil || unheld {
		t.Errorf("a live resident child reads as unheld (%v, %v)", unheld, err)
	}
	if srv.finishUnheldRun(context.Background(), runNow(t, srv.store, runID), "x") {
		t.Error("a live resident child's row was finished from under it")
	}
	_ = srv.closeResidentChild(ctx, runID)
	waitResidentGone(t, srv, runID)
}

// A wait on a resident child held by another replica ends when that replica
// is recorded gone, with the child ended and told as such — it does not sit
// until the caller's own bound, or the stale-run sweeper, ends it.
func TestResidentElsewhere_AWaitEndsWhenTheChildsReplicaIsGone(t *testing.T) {
	_, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	b.unheldRunGrace = time.Nanosecond
	ctx := residentParentCtx("parent-agent", "")
	child := leftResident(t, b.store, "a_left_running", "replica-a")
	// A turn its replica was running when it died: an instruction taken, no park.
	if err := b.store.AppendEvent(context.Background(), child.ID, "user_input", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, state, err := b.pollResidentChild(ctx, child.ID, 0); err != nil || state != "running" {
		t.Fatalf("poll while its replica is alive = %q %v, want running", state, err)
	}

	type polled struct {
		state string
		err   error
	}
	done := make(chan polled, 1)
	start := time.Now()
	before := replicasOf(b).readCount()
	go func() {
		_, state, err := b.pollResidentChild(ctx, child.ID, 30_000)
		done <- polled{state, err}
	}()
	// The replica dies once the wait has seen it alive, so it is the wait's
	// later re-check that must notice.
	waitFor(t, "the wait to check the child's replica", func() bool { return replicasOf(b).readCount() > before })
	replicasOf(b).set("replica-a", false)
	select {
	case p := <-done:
		if p.err == nil || !strings.Contains(p.err.Error(), "the replica running it is gone") {
			t.Errorf("the wait ended %q %v after %s, want the child ended as gone", p.state, p.err, time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait went on though the child's replica is gone")
	}
	if got := runNow(t, b.store, child.ID); got.Status != store.RunCancelled || got.StopReason != residentReasonOwnerGone {
		t.Errorf("row = %s %q, want cancelled as its replica gone", got.Status, got.StopReason)
	}
}
