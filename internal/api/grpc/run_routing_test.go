package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A run's own routing block crosses gRPC into the RunInput on Run and on
// Continue. Presence is the point: absent is "the agent's block", and a
// present but EMPTY message is the run turning queue-aware routing off — a
// mapper that read the empty message as absent would leave it on.
func TestRunAndContinue_CarryTheRunsRoutingBlock(t *testing.T) {
	fr := &fakeRunner{
		registered: registrationFrame{AgentID: "a_top", RunID: "r_top", SessionID: "s_top"},
		events:     []proverevent{{typ: "done", stopReason: "end_turn"}},
	}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()

	drain := func(recv func() (*loomcyclepb.Event, error), err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if _, err := recv(); err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}

	run, err := client.Run(context.Background(), &loomcyclepb.RunRequest{
		Agent:   "default",
		Routing: &loomcyclepb.RunRouting{QueueAware: true, SwitchAfterMs: 15000},
	})
	drain(func() (*loomcyclepb.Event, error) { return run.Recv() }, err)
	if got := fr.lastInput.Routing; got == nil || *got != (config.AgentRouting{QueueAware: true, SwitchAfterMS: 15000}) {
		t.Errorf("Run: RunInput.Routing = %+v, want queue_aware with switch_after_ms 15000", got)
	}

	cont, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{
		SessionId: "s_top",
		Routing:   &loomcyclepb.RunRouting{},
	})
	drain(func() (*loomcyclepb.Event, error) { return cont.Recv() }, err)
	if got := fr.lastInput.Routing; got == nil || got.QueueAware || got.SwitchAfterMS != 0 {
		t.Errorf("Continue with an empty block: RunInput.Routing = %+v, want a present, empty block", got)
	}

	plain, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default"})
	drain(func() (*loomcyclepb.Event, error) { return plain.Recv() }, err)
	if got := fr.lastInput.Routing; got != nil {
		t.Errorf("Run with no block: RunInput.Routing = %+v, want nil (the agent's own)", got)
	}
}

// A configured run and every batch child are built by spawnRequestFromProto,
// which must carry the block too (the drift test checks it is set; this checks
// it is the caller's value).
func TestSpawnRequestFromProto_CarriesTheRunsRoutingBlock(t *testing.T) {
	got := spawnRequestFromProto(&loomcyclepb.RunRequest{Routing: &loomcyclepb.RunRouting{QueueAware: true, SwitchAfterMs: 3000}})
	if got.Routing == nil || *got.Routing != (config.AgentRouting{QueueAware: true, SwitchAfterMS: 3000}) {
		t.Errorf("Routing = %+v, want queue_aware with switch_after_ms 3000", got.Routing)
	}
}
