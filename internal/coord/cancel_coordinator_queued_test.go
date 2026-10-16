package coord

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A queued run is admitted but not started: its cancel registration and its
// wait for a run slot live on the replica that queued it. A cancel arriving on
// any other replica must be routed there like a running run's, or the run
// later starts and completes although the caller was told it was handled.

// queuedCancelPair wires two coordinators on one backplane: "replica-A", where
// the cancel arrives, and "replica-B", which holds the registration of the
// queued run "a_queued". It returns A and the context B's registration was
// given, which a delivered cancel cancels.
func queuedCancelPair(t *testing.T, ackTimeout time.Duration) (*CancelCoordinator, context.Context) {
	t.Helper()
	bp := newMemBackplane()
	runs := &stubCancelRunStore{runs: map[string]store.Run{
		"a_queued": {ID: "r_queued", AgentID: "a_queued", Status: store.RunQueued, ReplicaID: "replica-B"},
	}}
	newCoord := func(id string) *CancelCoordinator {
		c, err := NewCancelCoordinator(CancelCoordinatorConfig{
			Backplane: bp, ReplicaID: id, Store: runs,
			ReplicaStore: stubLiveness{alive: true}, AckTimeout: ackTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := newCoord("replica-A"), newCoord("replica-B")

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	regB := cancel.NewRegistry()
	runCtx, cancelRun := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancelRun(nil) })
	if err := regB.Register(cancel.Entry{AgentID: "a_queued", RunID: "r_queued", StartedAt: time.Now()}, cancelRun); err != nil {
		t.Fatal(err)
	}
	// The memBackplane delivers only to subscribers already present, so both
	// subscriptions are in place before the cancel is published.
	go b.RunCancelSubscriber(ctx, regB)
	go a.RunAckSubscriber(ctx)
	waitForSubscribers(t, bp, topicCancel, topicCancelAck)
	return a, runCtx
}

// waitForSubscribers blocks until every named topic has a subscriber.
func waitForSubscribers(t *testing.T, bp *memBackplane, topics ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		bp.mu.Lock()
		ready := true
		for _, topic := range topics {
			if len(bp.subs[topic]) == 0 {
				ready = false
			}
		}
		bp.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscribers for %v never appeared", topics)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCancelRemote_QueuedRunOnALiveOwnerIsCancelledThere(t *testing.T) {
	a, runCtx := queuedCancelPair(t, 2*time.Second)

	res, found, err := a.CancelRemote(context.Background(), "a_queued", "stop it")
	if err != nil || !found || !res.Cancelled || res.Reason != "stop it" {
		t.Fatalf("cancel of a queued run on a live owner = %+v found %v err %v, want cancelled with the caller's reason", res, found, err)
	}
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the owner's registration of the queued run was never cancelled")
	}
}

// A queued run whose owner does not answer is still queued when the wait ends,
// and the answer must say the owner was unreachable, not report "queued" as
// though it were the state the run ended in.
func TestCancelRemote_QueuedRunWhoseOwnerIsSilentIsReportedUnreachable(t *testing.T) {
	cc, err := NewCancelCoordinator(CancelCoordinatorConfig{
		Backplane: newMemBackplane(), ReplicaID: "replica-A",
		Store: &stubCancelRunStore{runs: map[string]store.Run{
			"a_queued": {ID: "r_queued", AgentID: "a_queued", Status: store.RunQueued, ReplicaID: "replica-B"},
		}},
		ReplicaStore: stubLiveness{alive: true}, AckTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, found, err := cc.CancelRemote(context.Background(), "a_queued", "stop it")
	if err != nil || !found || res.Cancelled || res.Reason != ReasonOwnerUnreachable {
		t.Errorf("cancel of a queued run with a silent owner = %+v found %v err %v, want not cancelled, %q", res, found, err, ReasonOwnerUnreachable)
	}
}
