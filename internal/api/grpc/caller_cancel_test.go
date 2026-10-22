package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
// callerCancelGRPC serves the gRPC adapter over the real HTTP server, driven
// by openTurnProvider, and returns the client, the store and the HTTP server.
func callerCancelGRPC(t *testing.T) (loomcyclepb.LoomcycleClient, store.Store, *lchttp.Server) {
	t.Helper()
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
	return loomcyclepb.NewLoomcycleClient(conn), st, httpSrv
}

func TestRun_CallerDeadlineMidTurnRecordsCancelled(t *testing.T) {
	client, st, _ := callerCancelGRPC(t)

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

// An API cancel while the model is mid-turn ends a gRPC Run's stream with
// exactly one error frame, and the run's transcript ends with that error
// event. The mid-turn cancel exit used to emit nothing: the stream closed OK
// after bare text, and a re-attach replayed no end event at all.
func TestRun_APICancelMidTurnEndsWithOneErrorFrame(t *testing.T) {
	client, st, httpSrv := callerCancelGRPC(t)
	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "agent", UserId: "u1", Segments: userSegment("go")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ids struct {
		AgentID   string `json:"agent_id"`
		SessionID string `json:"session_id"`
	}
	var frames []*loomcyclepb.Event
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean end after the error frame", err)
		}
		frames = append(frames, ev)
		switch ev.GetType() {
		case "agent":
			_ = json.Unmarshal([]byte(ev.GetError()), &ids)
		case "text":
			if _, err := httpSrv.CancelRun(context.Background(), ids.AgentID, "stop"); err != nil {
				t.Fatalf("CancelRun: %v", err)
			}
		}
	}
	errorFrames := 0
	for _, f := range frames {
		if f.GetType() == "error" {
			errorFrames++
		}
	}
	if errorFrames != 1 || frames[len(frames)-1].GetType() != "error" {
		t.Fatalf("frames = %v, want the stream to end with exactly one error frame", frameTypes(frames))
	}

	events, err := st.GetTranscript(context.Background(), ids.SessionID)
	if err != nil || len(events) == 0 {
		t.Fatalf("GetTranscript: %d events, err %v", len(events), err)
	}
	if last := events[len(events)-1]; last.Type != "error" {
		t.Fatalf("the transcript ends with %q, want the error event", last.Type)
	}
}

func frameTypes(frames []*loomcyclepb.Event) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.GetType()
	}
	return out
}
