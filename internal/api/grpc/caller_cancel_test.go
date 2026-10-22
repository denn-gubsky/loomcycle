package grpc

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
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

// openTurnProvider streams some text and then holds the turn open until its
// ctx ends, closing the stream with no done event — what a driver does when
// the request it is reading is cancelled.
type openTurnProvider struct{}

func (openTurnProvider) ID() string                  { return "stub" }
func (openTurnProvider) Probe(context.Context) error { return nil }
func (openTurnProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (openTurnProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (openTurnProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	go func() {
		defer close(ch)
		ch <- providers.Event{Type: providers.EventText, Text: "partial"}
		<-ctx.Done()
	}()
	return ch, nil
}

// A gRPC Run whose caller deadline passes while the model is mid-turn ends
// the run cancelled, by its caller. The stream itself cannot be
// written to after the deadline; the row is what records how the run ended.
// Before the fix the row was written completed with an empty stop reason.
func TestRun_CallerDeadlineMidTurnRecordsCancelled(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-caller-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	httpSrv := lchttp.New(cfg, oneProvider{openTurnProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
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
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(gs.Stop)
	t.Cleanup(func() { _ = conn.Close() })
	client := loomcyclepb.NewLoomcycleClient(conn)

	ctx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	stream, err := client.Run(ctx, &loomcyclepb.RunRequest{Agent: "agent", UserId: "u1", Segments: userSegment("go")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var runID string
	for {
		ev, err := stream.Recv()
		if err != nil {
			break // the deadline ended the stream
		}
		if ev.GetType() == "agent" {
			var ids struct {
				RunID string `json:"run_id"`
			}
			_ = json.Unmarshal([]byte(ev.GetError()), &ids)
			runID = ids.RunID
		}
	}
	if runID == "" {
		t.Fatal("no run_id on the agent frame")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		run, err := st.GetRun(context.Background(), runID)
		if err == nil && store.IsTerminalRunStatus(run.Status) {
			// Either reason: when its deadline passes the gRPC client also
			// resets the stream, and the server's ctx ends with whichever of
			// that cancel and its own copy of the deadline lands first.
			reasonOK := run.StopReason == "caller deadline exceeded" || run.StopReason == "client disconnected"
			if run.Status != store.RunCancelled || !reasonOK || run.ErrorMsg != "" {
				t.Fatalf("run row = (%q, stop_reason %q, error %q), want cancelled by the caller, no error", run.Status, run.StopReason, run.ErrorMsg)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not end (status %q, err %v)", runID, run.Status, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
