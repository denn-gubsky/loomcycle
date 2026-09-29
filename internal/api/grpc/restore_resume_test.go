package grpc

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A restore over gRPC resumes the paused runs it writes, as the HTTP restore
// does — against the REAL HTTP server as the connector, since the resume lives
// there and a fake connector would only test the field mapping.

// endTurnProvider ends every turn, so a resumed run reaches a terminal state.
type endTurnProvider struct{ calls atomic.Int32 }

func (p *endTurnProvider) ID() string                  { return "stub" }
func (p *endTurnProvider) Probe(context.Context) error { return nil }
func (p *endTurnProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *endTurnProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *endTurnProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.calls.Add(1)
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "resumed and done"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// capturePausedRunEnvelope builds, on a store of its own, one paused run of
// agent waiting on a user turn, and returns its id and the snapshot envelope.
func capturePausedRunEnvelope(t *testing.T, agent string) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	sess, err := src.CreateSession(ctx, "acme", agent, "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := src.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_grpc_restore", UserID: "alice", TenantID: "acme", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
	})
	if err := src.AppendEvent(ctx, run.ID, "user_input", b); err != nil {
		t.Fatal(err)
	}
	if err := src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID, raw
}

func TestGrpcRestoreSnapshot_ResumesTheRestoredPausedRun(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"resumer": {Model: "stub-model", SystemPrompt: "you resume work"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	prov := &endTurnProvider{}
	httpSrv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
	gs := googlegrpc.NewServer(
		googlegrpc.UnaryInterceptor(adapter.UnaryAuthInterceptor()),
		googlegrpc.StreamInterceptor(adapter.StreamAuthInterceptor()),
	)
	loomcyclepb.RegisterLoomcycleServer(gs, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := loomcyclepb.NewLoomcycleClient(conn)

	runID, raw := capturePausedRunEnvelope(t, "resumer")
	resp, err := client.RestoreSnapshot(context.Background(), &loomcyclepb.RestoreSnapshotRequest{RawJson: raw})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if resp.GetPausedRunsRestored() != 1 || resp.GetPausedRunsResumed() != 1 {
		t.Errorf("paused_runs_restored = %d, paused_runs_resumed = %d; want 1 and 1 (warnings %v)",
			resp.GetPausedRunsRestored(), resp.GetPausedRunsResumed(), resp.GetWarnings())
	}
	if got := resp.GetRestored()["paused_runs_resumed"]; got != 1 {
		t.Errorf("restored[paused_runs_resumed] = %d, want 1", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := st.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status == store.RunCompleted {
			if run.PauseState != store.PauseStateRunning {
				t.Errorf("resumed run pause_state = %q, want %q", run.PauseState, store.PauseStateRunning)
			}
			break
		}
		if run.Status == store.RunFailed {
			t.Fatalf("resumed run failed: %s", run.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run restored over gRPC was never resumed (status=%q pause_state=%q)", run.Status, run.PauseState)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if prov.calls.Load() < 1 {
		t.Error("the provider was never called — the restored run's loop did not run")
	}
}
