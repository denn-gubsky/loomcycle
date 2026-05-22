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

	// Wait briefly so the subscriber is registered before publishing.
	time.Sleep(20 * time.Millisecond)

	bus.Publish(runstate.RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})  // filtered out
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

	time.Sleep(20 * time.Millisecond)
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
