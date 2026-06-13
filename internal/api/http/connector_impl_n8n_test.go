package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestConnector_ListChannels_RoundtripThroughInterface(t *testing.T) {
	srv, s, cleanup := systemChannelFixture(t)
	defer cleanup()

	// Publish into the declared channel.
	ctx := t.Context()
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "_system/alarms/critical",
		Scope:   store.MemoryScopeGlobal,
		ScopeID: "global",
		Payload: []byte(`{}`),
	}, 1000); err != nil {
		t.Fatalf("ChannelPublish: %v", err)
	}

	// Dispatch through the Connector interface (NOT the concrete
	// *Server) — this is the call shape MCP / gRPC adapters use.
	var c connector.Connector = srv
	resp, err := c.ListChannels(ctx)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(resp.Channels) == 0 {
		t.Fatal("expected at least one channel")
	}
	var got connector.ChannelDescriptor
	for _, ch := range resp.Channels {
		if ch.Name == "_system/alarms/critical" {
			got = ch
			break
		}
	}
	if got.Name == "" {
		t.Fatalf("alarms/critical missing from response: %+v", resp.Channels)
	}
	if got.MessageCount != 1 {
		t.Errorf("message_count = %d, want 1", got.MessageCount)
	}
	if _, err := time.Parse(time.RFC3339, got.OldestVisibleAt); err != nil {
		t.Errorf("oldest_visible_at not RFC3339: %q (%v)", got.OldestVisibleAt, err)
	}
}

func TestConnector_StreamUserRunStates_VisitsMatchingEvents(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)

	var c connector.Connector = srv
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	received := make(chan connector.RunStateEvent, 4)
	done := make(chan error, 1)
	go func() {
		done <- c.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{
			UserID:   "user-a",
			Statuses: []string{"completed"},
		}, func(evt connector.RunStateEvent) error {
			received <- evt
			return nil
		})
	}()

	// Sync on actual subscription registration rather than a fixed
	// sleep — under the race detector a 20ms sleep is unreliable.
	waitForSubscriber(t, bus)

	bus.Publish(runstate.RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})   // filtered out
	bus.Publish(runstate.RunStateEvent{RunID: "r2", UserID: "user-a", Status: "completed"}) // passes
	bus.Publish(runstate.RunStateEvent{RunID: "r3", UserID: "user-b", Status: "completed"}) // wrong user

	select {
	case evt := <-received:
		if evt.RunID != "r2" || evt.Status != "completed" {
			t.Errorf("wrong event passed: %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered within 1s")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("StreamUserRunStates returned err: %v", err)
	}
}

// A tenant-scoped stream must drop run-state events for runs in OTHER tenants
// even when they share the requested user_id (RFC L/N: run_ids/user_ids aren't
// secret). Mirrors the user-filter test's "publish a dropped event first, then
// a passing one; the first RECEIVED event proves the drop" shape. Fails on the
// pre-filter code, where the cross-tenant event would arrive first.
func TestConnector_StreamUserRunStates_TenantScopedDropsCrossTenant(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)

	var c connector.Connector = srv
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	received := make(chan connector.RunStateEvent, 4)
	done := make(chan error, 1)
	go func() {
		done <- c.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{
			UserID:       "u-shared",
			TenantID:     "acme",
			TenantScoped: true,
		}, func(evt connector.RunStateEvent) error {
			received <- evt
			return nil
		})
	}()

	waitForSubscriber(t, bus)

	// Same user, two tenants. The evil event is published FIRST and must be
	// dropped; the acme event must pass — so the first RECEIVED event is acme.
	bus.Publish(runstate.RunStateEvent{RunID: "r_evil", UserID: "u-shared", TenantID: "evil", Status: "completed"})
	bus.Publish(runstate.RunStateEvent{RunID: "r_acme", UserID: "u-shared", TenantID: "acme", Status: "completed"})

	select {
	case evt := <-received:
		if evt.RunID != "r_acme" {
			t.Errorf("cross-tenant event leaked: got %q, want r_acme (tenant filter not enforced)", evt.RunID)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered within 1s")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("StreamUserRunStates returned err: %v", err)
	}
}

func TestConnector_StreamUserRunStates_StopOnSentinel(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)

	var c connector.Connector = srv
	ctx := t.Context()
	done := make(chan error, 1)
	go func() {
		done <- c.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{UserID: "user-a"},
			func(evt connector.RunStateEvent) error {
				return connector.ErrStopStreaming
			})
	}()

	waitForSubscriber(t, bus)
	bus.Publish(runstate.RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on stop sentinel, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StreamUserRunStates didn't return on sentinel")
	}
}

func TestConnector_StreamUserRunStates_UnavailableWhenBusUnwired(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	// no SetRunStateBus call

	var c connector.Connector = srv
	err := c.StreamUserRunStates(t.Context(), connector.StreamUserRunStatesRequest{UserID: "x"}, func(connector.RunStateEvent) error { return nil })
	if !errors.Is(err, connector.ErrRunStateStreamUnavailable) {
		t.Errorf("expected ErrRunStateStreamUnavailable, got %v", err)
	}
}
