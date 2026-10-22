package grpc

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// heldProvider answers once per token on gate and otherwise holds the turn
// open until its ctx ends, so a run it drives never finishes on its own.
type heldProvider struct{ gate chan struct{} }

func (p *heldProvider) ID() string                  { return "stub" }
func (p *heldProvider) Probe(context.Context) error { return nil }
func (p *heldProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *heldProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *heldProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	go func() {
		defer close(ch)
		select {
		case <-p.gate:
			ch <- providers.Event{Type: providers.EventText, Text: "done"}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// batchJoinGRPC serves the gRPC adapter over the real HTTP server as its
// connector, with the given server join bound and a held provider whose
// gate the test feeds. track names a run cleanup lets write its end (it
// releases the provider) before the store closes.
func batchJoinGRPC(t *testing.T, bound time.Duration) (client loomcyclepb.LoomcycleClient, st store.Store, gate chan struct{}, track func(runID string)) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-batch-join.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.BatchJoinTimeout = bound
	prov := &heldProvider{gate: make(chan struct{}, 4)}
	httpSrv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
	gs := googlegrpc.NewServer()
	loomcyclepb.RegisterLoomcycleServer(gs, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(lis) }()
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered first: release the held runs and let them
	// end, then stop serving, then close the store they write to.
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(gs.Stop)
	t.Cleanup(func() { _ = conn.Close() })
	var held []string
	t.Cleanup(func() {
		for range held {
			select {
			case prov.gate <- struct{}{}:
			default:
			}
		}
		for _, id := range held {
			for i := 0; i < 100; i++ {
				if run, err := st.GetRun(context.Background(), id); err == nil && store.IsTerminalRunStatus(run.Status) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	return loomcyclepb.NewLoomcycleClient(conn), st, prov.gate, func(id string) { held = append(held, id) }
}

// joinGRPCWithin calls SpawnRunBatch with no deadline and fails the test if it
// has not returned in 10s: before the bound it waited for ever.
func joinGRPCWithin(t *testing.T, client loomcyclepb.LoomcycleClient, req *loomcyclepb.BatchSpawnRequest) *loomcyclepb.SpawnResult {
	t.Helper()
	type joined struct {
		res *loomcyclepb.BatchSpawnResult
		err error
	}
	done := make(chan joined, 1)
	go func() {
		res, err := client.SpawnRunBatch(context.Background(), req)
		done <- joined{res, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("join: %v", got.err)
		}
		return got.res.GetResults()[0]
	case <-time.After(10 * time.Second):
		t.Fatal("SpawnRunBatch did not return within 10s: the join waits on a run that never ends")
		return nil
	}
}

// waitStatus polls a run's row until it reaches want.
func waitStatus(t *testing.T, st store.Store, runID string, want store.RunStatus) {
	t.Helper()
	for i := 0; i < 150; i++ {
		if run, err := st.GetRun(context.Background(), runID); err == nil && run.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	run, err := st.GetRun(context.Background(), runID)
	t.Fatalf("run %s is %q (err %v), want %q", runID, run.Status, err, want)
}

// SpawnRunBatch in join mode, over the real HTTP server as the connector: a
// child keyed to a run that never ends comes back at the server's join bound,
// reported running with that run's ids and the reason it has no result, and
// the run itself goes on — it belongs to the request that started it.
func TestSpawnRunBatch_JoinOnAHeldKeyEndsAtTheServerBound(t *testing.T) {
	client, st, _, track := batchJoinGRPC(t, 300*time.Millisecond)

	child := &loomcyclepb.RunRequest{Agent: "agent", UserId: "u1", IdempotencyKey: "held", Segments: userSegment("go")}
	started, err := client.SpawnRunBatch(context.Background(), &loomcyclepb.BatchSpawnRequest{Mode: "detach", Spawns: []*loomcyclepb.RunRequest{child}})
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	held := started.GetResults()[0]
	if held.GetStatus() != "running" || held.GetRunId() == "" {
		t.Fatalf("held run = %+v, want a fresh running run", held)
	}
	track(held.GetRunId())

	r := joinGRPCWithin(t, client, &loomcyclepb.BatchSpawnRequest{Spawns: []*loomcyclepb.RunRequest{child}})
	if !r.GetDeduplicated() || r.GetRunId() != held.GetRunId() || r.GetSessionId() != held.GetSessionId() || r.GetStatus() != "running" {
		t.Fatalf("joined child = %+v, want the held run reported running, deduplicated", r)
	}
	if !strings.Contains(r.GetError(), "join timed out") {
		t.Errorf("joined child error = %q, want it to say the join timed out", r.GetError())
	}
	time.Sleep(100 * time.Millisecond)
	waitStatus(t, st, held.GetRunId(), store.RunRunning)
}

// A fresh child still going at the server's bound comes back running with its
// ids and outlives the RPC — whose ctx ends as it returns — to complete.
func TestSpawnRunBatch_AFreshChildOutlivesTheServerBound(t *testing.T) {
	client, st, gate, track := batchJoinGRPC(t, 300*time.Millisecond)

	r := joinGRPCWithin(t, client, &loomcyclepb.BatchSpawnRequest{Spawns: []*loomcyclepb.RunRequest{
		{Agent: "agent", UserId: "u1", Segments: userSegment("go")},
	}})
	if r.GetStatus() != "running" || r.GetRunId() == "" || r.GetSessionId() == "" || r.GetDeduplicated() {
		t.Fatalf("child = %+v, want this batch's run reported running with its ids", r)
	}
	if !strings.Contains(r.GetError(), "join timed out") {
		t.Errorf("child error = %q, want it to say the join timed out", r.GetError())
	}
	track(r.GetRunId())
	time.Sleep(100 * time.Millisecond)
	waitStatus(t, st, r.GetRunId(), store.RunRunning)
	gate <- struct{}{}
	waitStatus(t, st, r.GetRunId(), store.RunCompleted)
}
