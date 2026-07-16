package coord

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/turncancel"
)

// causeForTest mirrors loop.TurnCancelCause without importing internal/loop — the
// coordinator is cause-agnostic (it routes the reason string; the owning
// replica's registry rebuilds the cause via its own causeFor).
func causeForTest(reason string) error {
	if reason == "" {
		return errors.New("turn cancelled by operator")
	}
	return fmt.Errorf("turn cancelled by operator: %s", reason)
}

func newTurnCancelCoord(t *testing.T, bp Backplane, replicaID string, runs map[string]store.Run, alive bool, timeout time.Duration) *TurnCancelCoordinator {
	t.Helper()
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	c, err := NewTurnCancelCoordinator(TurnCancelCoordinatorConfig{
		Backplane:    bp,
		ReplicaID:    replicaID,
		Store:        &stubSteerRunStore{runs: runs}, // reused: same narrow GetRun surface
		ReplicaStore: stubLiveness{alive: alive},
		AckTimeout:   timeout,
	})
	if err != nil {
		t.Fatalf("NewTurnCancelCoordinator: %v", err)
	}
	return c
}

func TestTurnCancelCoordinator_NotFound_ReturnsMiss(t *testing.T) {
	c := newTurnCancelCoord(t, newMemBackplane(), "replica-A", map[string]store.Run{}, true, 0)
	found, err := c.CancelRemote(context.Background(), "ghost", "x")
	if err != nil || found {
		t.Errorf("CancelRemote(unknown) = (%v,%v), want (false,nil)", found, err)
	}
}

func TestTurnCancelCoordinator_SelfOwned_ReturnsMiss(t *testing.T) {
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: "replica-A"}}
	c := newTurnCancelCoord(t, newMemBackplane(), "replica-A", runs, true, 0)
	found, _ := c.CancelRemote(context.Background(), "run-1", "x")
	if found {
		t.Errorf("self-owned run = %v, want false — local registry missed ⇒ turn not armed here", found)
	}
}

func TestTurnCancelCoordinator_Terminal_ReturnsMiss(t *testing.T) {
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunCompleted, ReplicaID: "replica-B"}}
	c := newTurnCancelCoord(t, newMemBackplane(), "replica-A", runs, true, 0)
	found, _ := c.CancelRemote(context.Background(), "run-1", "x")
	if found {
		t.Errorf("terminal run = %v, want false", found)
	}
}

func TestTurnCancelCoordinator_DeadOwner_ReturnsMiss(t *testing.T) {
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: "replica-B"}}
	c := newTurnCancelCoord(t, newMemBackplane(), "replica-A", runs, false /* dead */, 0)
	found, _ := c.CancelRemote(context.Background(), "run-1", "x")
	if found {
		t.Errorf("dead-owner run = %v, want false", found)
	}
}

// The full cross-replica round-trip: originator (A) CancelRemote → owner (B)
// RunTurnCancelSubscriber fires B's local armed token + acks → A's ack subscriber
// routes the ack back → CancelRemote returns found=true, and B's turn ctx is
// cancelled with the operator reason.
func TestTurnCancelCoordinator_FiresRemoteOwnersArmedToken(t *testing.T) {
	bp := newMemBackplane()
	const owner = "replica-B"
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: owner}}

	a := newTurnCancelCoord(t, bp, "replica-A", runs, true, 0) // originator
	b := newTurnCancelCoord(t, bp, owner, runs, true, 0)       // owner

	ownerReg := turncancel.NewRegistry()
	ownerReg.SetCauseFor(causeForTest)
	turnCtx, turnCancel := context.WithCancelCause(context.Background())
	defer turnCancel(nil)
	ownerReg.Arm("run-1", turnCancel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunTurnCancelSubscriber(ctx, ownerReg) // owner side
	go a.RunTurnCancelAckSubscriber(ctx)        // originator side
	time.Sleep(30 * time.Millisecond)           // let both Subscribe calls register

	found, err := a.CancelRemote(context.Background(), "run-1", "too slow")
	if err != nil {
		t.Fatalf("CancelRemote: %v", err)
	}
	if !found {
		t.Fatalf("CancelRemote = found=%v, want true", found)
	}
	// The owner's armed turn ctx was cancelled with the routed reason.
	if turnCtx.Err() == nil {
		t.Fatal("owner's armed turn ctx was never cancelled")
	}
	if cause := context.Cause(turnCtx); cause == nil || cause.Error() != "turn cancelled by operator: too slow" {
		t.Fatalf("owner turn cause = %v, want the routed reason", context.Cause(turnCtx))
	}
	// The token was consumed on the owner.
	if ownerReg.IsArmed("run-1") {
		t.Error("owner token still armed after a remote cancel")
	}
}

// A remote owner that is NOT mid-turn (no armed token) never acks, so CancelRemote
// times out and reports not-fired — the handler serves 404.
func TestTurnCancelCoordinator_RemoteOwnerNotMidTurn_TimesOut(t *testing.T) {
	bp := newMemBackplane()
	const owner = "replica-B"
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: owner}}

	a := newTurnCancelCoord(t, bp, "replica-A", runs, true, 150*time.Millisecond)
	b := newTurnCancelCoord(t, bp, owner, runs, true, 150*time.Millisecond)

	ownerReg := turncancel.NewRegistry() // no armed token (run is parked on the owner)
	ownerReg.SetCauseFor(causeForTest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunTurnCancelSubscriber(ctx, ownerReg)
	go a.RunTurnCancelAckSubscriber(ctx)
	time.Sleep(30 * time.Millisecond)

	found, err := a.CancelRemote(context.Background(), "run-1", "")
	if err != nil {
		t.Fatalf("CancelRemote: %v", err)
	}
	if found {
		t.Fatal("CancelRemote reported fired for a not-mid-turn remote owner")
	}
}

func TestTurnCancelCoordinator_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  TurnCancelCoordinatorConfig
		want string
	}{
		{"nil backplane", TurnCancelCoordinatorConfig{}, "backplane"},
		{"nil store", TurnCancelCoordinatorConfig{Backplane: newMemBackplane()}, "store"},
		{"nil replica store", TurnCancelCoordinatorConfig{Backplane: newMemBackplane(), Store: &stubSteerRunStore{}}, "replica store"},
	}
	for _, tc := range cases {
		if _, err := NewTurnCancelCoordinator(tc.cfg); err == nil {
			t.Errorf("%s: want error mentioning %q, got nil", tc.name, tc.want)
		}
	}
}
