package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
)

// A walk is a run, and its own transitions — start, the pauses it asks a
// person for, end — reach the run-state stream like any run's, on the stream
// filtered by its walk id included.

// streamRunStates subscribes req on the server's run-state stream and returns
// the events it delivers. The subscription ends with the test.
func streamRunStates(t *testing.T, srv *Server, req connector.StreamUserRunStatesRequest) <-chan connector.RunStateEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := make(chan connector.RunStateEvent, 256)
	go func() {
		_ = srv.StreamUserRunStates(ctx, req, func(evt connector.RunStateEvent) error {
			events <- evt
			return nil
		})
	}()
	return events
}

// waitForSubscribers waits until n run-state subscriptions are open, so a
// publish that follows cannot race past one of them.
func waitForSubscribers(t *testing.T, bus *runstate.Bus, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for bus.ActiveSubscriberCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d run-state subscribers never registered", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// walkFrames collects the frames of the walk's OWN run that pass the walk
// filter, until one with status `until` or the deadline.
func walkFrames(t *testing.T, events <-chan connector.RunStateEvent, walkID, until string) []connector.RunStateEvent {
	t.Helper()
	var got []connector.RunStateEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.RunID != walkID || !walkIDMatches(walkID, evt.RunID, evt.ParentContext) {
				continue
			}
			got = append(got, evt)
			if evt.Status == until {
				return got
			}
		case <-deadline:
			t.Fatalf("walk %s: no %q frame; got %+v", walkID, until, got)
			return nil
		}
	}
}

// noWalkFrames fails when another tenant's subscription delivered any frame of
// the walk's own run.
func noWalkFrames(t *testing.T, events <-chan connector.RunStateEvent, walkID string) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-events:
			if evt.RunID == walkID {
				t.Fatalf("another tenant's stream saw walk %s: %+v", walkID, evt)
			}
		case <-deadline:
			return
		}
	}
}

// TestTeamDefRun_WalkStreamCarriesTheWalksOwnStartAndEnd: the walk's own run
// reaches the stream as running, then completed — and only its own tenant's.
// It published neither, so a stream watching the walk saw its members and
// never the walk.
//
// The walk id is not known before the walk starts, so the start frame is
// caught on the tenant's whole stream and put through the walk filter's match.
func TestTeamDefRun_WalkStreamCarriesTheWalksOwnStartAndEnd(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)
	events := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{UserID: "alice", TenantID: "acme", TenantScoped: true})
	other := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{UserID: "alice", TenantID: "evil", TenantScoped: true})
	waitForSubscribers(t, h.bus, 2)

	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"solo","input":"go"}`)

	got := walkFrames(t, events, walkID, "completed")
	if len(got) != 2 || got[0].Status != "running" {
		t.Fatalf("walk frames = %+v, want running then completed", got)
	}
	for _, f := range got {
		if f.Agent != "team:solo" || f.UserID != "alice" || f.AwaitedState != "" {
			t.Errorf("walk frame %+v: want agent team:solo, user alice, no awaited state", f)
		}
	}
	noWalkFrames(t, other, walkID)
}
