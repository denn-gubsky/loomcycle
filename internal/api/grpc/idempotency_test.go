package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// dupRunner refuses every run as a duplicate of an existing one, as RunOnce
// does when the request's idempotency_key is already held.
type dupRunner struct{ lastInput runner.RunInput }

func (d *dupRunner) RunOnce(_ context.Context, in runner.RunInput, _ runner.RunCallbacks) error {
	d.lastInput = in
	return &runner.DuplicateRunError{RunID: "r_first", AgentID: "a_first", SessionID: "s_first"}
}

func startTestServerWithRunnerAndConnector(t *testing.T, r runner.Runner, mc connector.Connector) loomcyclepb.LoomcycleClient {
	t.Helper()
	adapter := New(Config{Store: newTestStore(t), CancelReg: cancel.NewRegistry(), Runner: r, Connector: mc})
	grpcSrv := googlegrpc.NewServer()
	loomcyclepb.RegisterLoomcycleServer(grpcSrv, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcSrv.Serve(lis) }()
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
	})
	return loomcyclepb.NewLoomcycleClient(conn)
}

// A Run whose key is already held starts nothing. Its stream is the existing
// run's: the same two opening frames a fresh run sends — the agent frame
// marked deduplicated — and then that run's events from its first.
func TestRun_ADuplicateKeyStreamsTheExistingRun(t *testing.T) {
	dr := &dupRunner{}
	mc := &interactiveMock{streamEvents: []providers.Event{
		{Type: providers.EventText, Text: "the first answer"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	client := startTestServerWithRunnerAndConnector(t, dr, mc)

	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "judge", IdempotencyKey: "ccq-score:c1:abc"})
	if err != nil {
		t.Fatal(err)
	}
	var frames []*loomcyclepb.Event
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		frames = append(frames, ev)
	}

	if dr.lastInput.ClientIdempotencyKey != "ccq-score:c1:abc" || dr.lastInput.IdempotencyKey != "" {
		t.Errorf("RunOnce got ClientIdempotencyKey=%q IdempotencyKey=%q; want the caller's key in the first only",
			dr.lastInput.ClientIdempotencyKey, dr.lastInput.IdempotencyKey)
	}
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want session, agent, text, done: %v", len(frames), frames)
	}
	if frames[0].GetType() != "session" || frames[0].GetText() != "s_first" {
		t.Errorf("frame 0 = %v, want the existing session", frames[0])
	}
	var env struct {
		AgentID, RunID, SessionID string
		Deduplicated              bool
	}
	raw := struct {
		AgentID      *string `json:"agent_id"`
		RunID        *string `json:"run_id"`
		SessionID    *string `json:"session_id"`
		Deduplicated *bool   `json:"deduplicated"`
	}{&env.AgentID, &env.RunID, &env.SessionID, &env.Deduplicated}
	if frames[1].GetType() != "agent" || frames[1].GetText() != "a_first" || json.Unmarshal([]byte(frames[1].GetError()), &raw) != nil {
		t.Fatalf("frame 1 = %v, want the existing run's agent frame", frames[1])
	}
	if env.RunID != "r_first" || env.SessionID != "s_first" || !env.Deduplicated {
		t.Errorf("agent frame envelope = %+v, want the existing run, deduplicated", env)
	}
	if frames[2].GetText() != "the first answer" || frames[3].GetType() != "done" {
		t.Errorf("events = %v, %v; want the existing run's", frames[2], frames[3])
	}
	if mc.gotStreamRunID != "r_first" || mc.gotStreamFrom != 0 {
		t.Errorf("tailed run %q from seq %d, want r_first from 0", mc.gotStreamRunID, mc.gotStreamFrom)
	}
}

// A fresh run's agent frame carries no deduplicated flag at all, so a client
// that predates it sees the envelope it always did.
func TestAgentFrameJSON_OmitsDeduplicatedOnAFreshRun(t *testing.T) {
	if got, want := agentFrameJSON("a", "r", "s", "", false), `{"agent_id":"a","run_id":"r","session_id":"s","parent_agent_id":""}`; got != want {
		t.Errorf("fresh agent frame = %s, want %s", got, want)
	}
}

// The batch carries the key in and the flag out.
func TestSpawnRunBatch_CarriesTheKeyAndTheDeduplicatedFlag(t *testing.T) {
	if r := spawnRequestFromProto(&loomcyclepb.RunRequest{IdempotencyKey: "k1"}); r.IdempotencyKey != "k1" {
		t.Errorf("spawnRequestFromProto key = %q, want k1", r.IdempotencyKey)
	}
	if p := spawnResultToProto(connector.SpawnRunResult{RunID: "r", Deduplicated: true}); !p.GetDeduplicated() {
		t.Error("spawnResultToProto dropped deduplicated")
	}
	if p := spawnResultToProto(connector.SpawnRunResult{RunID: "r"}); p.GetDeduplicated() {
		t.Error("a fresh child is reported deduplicated")
	}
}

// max_wall_seconds reaches the runner from Run and a batch child alike.
func TestRun_MaxWallSecondsReachesTheRunner(t *testing.T) {
	fr := &fakeRunner{registered: registrationFrame{AgentID: "a", RunID: "r", SessionID: "s"}}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()
	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default", MaxWallSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	if fr.lastInput.MaxWallSeconds != 60 {
		t.Errorf("Run: RunInput.MaxWallSeconds = %d, want 60", fr.lastInput.MaxWallSeconds)
	}
	if r := spawnRequestFromProto(&loomcyclepb.RunRequest{MaxWallSeconds: 90}); r.MaxWallSeconds != 90 {
		t.Errorf("spawnRequestFromProto: MaxWallSeconds = %d, want 90", r.MaxWallSeconds)
	}
}
