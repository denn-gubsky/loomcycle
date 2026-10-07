package coord

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A cancel for a run this replica does not hold asks the replicas table
// whether the run's owner is alive. A dead owner's run is handed to the
// owner-gone finisher; a live owner that does not answer is left alone.

type ownerGoneCall struct {
	run    store.Run
	reason string
}

// ownerGoneCoord is a coordinator for one running run, "a_run", owned by
// "replica-B", on a backplane nobody answers.
func ownerGoneCoord(t *testing.T, live ReplicaLiveness) *CancelCoordinator {
	t.Helper()
	cc, err := NewCancelCoordinator(CancelCoordinatorConfig{
		Backplane: newMemBackplane(),
		ReplicaID: "replica-A",
		Store: &stubCancelRunStore{runs: map[string]store.Run{
			"a_run": {ID: "r_run", AgentID: "a_run", Status: store.RunRunning, ReplicaID: "replica-B"},
		}},
		ReplicaStore: live,
		AckTimeout:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func TestCancelCoordinator_ADeadOwnersRunIsEndedByTheFinisherAsCancelled(t *testing.T) {
	cc := ownerGoneCoord(t, stubLiveness{alive: false})
	var calls []ownerGoneCall
	cc.SetOwnerGoneFinisher(func(_ context.Context, run store.Run, reason string) (bool, []string) {
		calls = append(calls, ownerGoneCall{run, reason})
		return true, []string{"a_child"}
	})
	res, found, err := cc.CancelRemote(context.Background(), "a_run", "stop it")
	if err != nil || !found || !res.Cancelled || res.Reason != ReasonOwnerDeadCancelled || !slices.Equal(res.Cascaded, []string{"a_child"}) {
		t.Fatalf("cancel of a dead owner's run = %+v found %v err %v, want cancelled as %q with what the finisher cascaded to", res, found, err, ReasonOwnerDeadCancelled)
	}
	if len(calls) != 1 || calls[0].run.ID != "r_run" || calls[0].reason != "stop it" {
		t.Errorf("finisher calls = %+v, want the run once, with the caller's reason", calls)
	}
}

// The finisher applies its own test that nothing holds the run. When it
// declines, the run was left as it is, and the answer says so.
func TestCancelCoordinator_AFinisherThatDeclinesLeavesTheRunUnreachable(t *testing.T) {
	cc := ownerGoneCoord(t, stubLiveness{alive: false})
	cc.SetOwnerGoneFinisher(func(context.Context, store.Run, string) (bool, []string) { return false, nil })
	res, found, err := cc.CancelRemote(context.Background(), "a_run", "stop it")
	if err != nil || !found || res.Cancelled || res.Reason != ReasonOwnerUnreachable {
		t.Errorf("cancel the finisher declined = %+v found %v err %v, want not cancelled, %q", res, found, err, ReasonOwnerUnreachable)
	}
}

// With no finisher installed the coordinator ends nothing itself: the cancel
// is a miss, and the caller does what it does for a run no registry knows.
func TestCancelCoordinator_WithNoFinisherADeadOwnersRunIsAMiss(t *testing.T) {
	cc := ownerGoneCoord(t, stubLiveness{alive: false})
	res, found, err := cc.CancelRemote(context.Background(), "a_run", "stop it")
	if err != nil || found || res.Cancelled {
		t.Errorf("cancel with no finisher = %+v found %v err %v, want a miss", res, found, err)
	}
}

// An owner the replicas table says is alive, or one whose liveness cannot be
// read, is never handed to the finisher, however long it stays silent.
func TestCancelCoordinator_ASilentLiveOwnersRunIsNeverHandedToTheFinisher(t *testing.T) {
	for name, live := range map[string]ReplicaLiveness{
		"alive":               stubLiveness{alive: true},
		"liveness unreadable": stubLiveness{err: errors.New("replicas table unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			cc := ownerGoneCoord(t, live)
			called := false
			cc.SetOwnerGoneFinisher(func(context.Context, store.Run, string) (bool, []string) {
				called = true
				return true, nil
			})
			res, found, err := cc.CancelRemote(context.Background(), "a_run", "stop it")
			if called {
				t.Error("a run whose owner is not recorded dead was handed to the finisher")
			}
			if err != nil || !found || res.Cancelled || res.Reason != ReasonOwnerUnreachable {
				t.Errorf("cancel with no ack = %+v found %v err %v, want not cancelled, %q", res, found, err, ReasonOwnerUnreachable)
			}
		})
	}
}
