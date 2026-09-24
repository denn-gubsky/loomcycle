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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

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

// RFC DI configured runs over gRPC, against the REAL HTTP server as both the
// connector and the runner — the rules under test live there, and a fake
// connector would only test the mapping.

type answerProvider struct{ last *providers.Request }

func (p *answerProvider) ID() string                  { return "stub" }
func (p *answerProvider) Probe(context.Context) error { return nil }
func (p *answerProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *answerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *answerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	r := req
	p.last = &r
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "done"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

type oneProvider struct{ p providers.Provider }

func (r oneProvider) Get(string) (providers.Provider, error) { return r.p, nil }

func configuredGRPC(t *testing.T) (loomcyclepb.LoomcycleClient, *answerProvider, store.Store) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-configured.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	prov := &answerProvider{}
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
	return loomcyclepb.NewLoomcycleClient(conn), prov, st
}

func userSegment(text string) []*loomcyclepb.PromptSegment {
	return []*loomcyclepb.PromptSegment{{Role: "user", Content: []*loomcyclepb.PromptContentBlock{{Type: "trusted-text", Text: text}}}}
}

// Create, edit, read, start and — for a second draft — discard, end to end.
func TestConfiguredRunRPCs_CreateEditStartDiscard(t *testing.T) {
	client, prov, st := configuredGRPC(t)
	ctx := context.Background()

	created, err := client.CreateConfiguredRun(ctx, &loomcyclepb.RunRequest{Agent: "agent", UserId: "u1", Segments: userSegment("first")})
	if err != nil {
		t.Fatalf("CreateConfiguredRun: %v", err)
	}
	if created.GetStatus() != "configured" || created.GetRunId() == "" || len(created.GetDraft()) == 0 {
		t.Fatalf("created = %+v", created)
	}
	if prov.last != nil {
		t.Error("the provider was called for a draft")
	}

	updated, err := client.UpdateConfiguredRun(ctx, &loomcyclepb.UpdateConfiguredRunRequest{
		RunId: created.GetRunId(), Patch: []byte(`{"prompt":"edited","sampling":{"temperature":0.3}}`),
	})
	if err != nil {
		t.Fatalf("UpdateConfiguredRun: %v", err)
	}
	var draft map[string]any
	if err := json.Unmarshal(updated.GetDraft(), &draft); err != nil || draft["sampling"] == nil {
		t.Errorf("updated draft = %s (%v)", updated.GetDraft(), err)
	}

	agent, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: created.GetAgentId()})
	if err != nil || agent.GetStatus() != "configured" || len(agent.GetDraft()) == 0 {
		t.Fatalf("GetAgent of a draft = %+v (%v), want its status and draft", agent, err)
	}

	stream, err := client.StartConfiguredRun(ctx, &loomcyclepb.StartConfiguredRunRequest{RunId: created.GetRunId()})
	if err != nil {
		t.Fatal(err)
	}
	var sawDone bool
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.GetType() == "done" {
			sawDone = true
		}
	}
	if !sawDone || prov.last == nil || prov.last.Temperature == nil || *prov.last.Temperature != 0.3 {
		t.Errorf("start: done=%v, provider saw %+v — want the patched draft run to completion", sawDone, prov.last)
	}
	if run, _ := st.GetRun(ctx, created.GetRunId()); run.Status != store.RunCompleted {
		t.Errorf("row after start = %q, want completed", run.Status)
	}
	// Started: no longer a draft.
	if _, err := client.UpdateConfiguredRun(ctx, &loomcyclepb.UpdateConfiguredRunRequest{RunId: created.GetRunId(), Patch: []byte(`{"prompt":"late"}`)}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Update after start = %v, want FailedPrecondition", err)
	}
	if err := drainStart(client, created.GetRunId()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second start = %v, want FailedPrecondition", err)
	}

	second, err := client.CreateConfiguredRun(ctx, &loomcyclepb.RunRequest{Agent: "agent", UserId: "u1", Segments: userSegment("throwaway")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteConfiguredRun(ctx, &loomcyclepb.DeleteConfiguredRunRequest{RunId: second.GetRunId()}); err != nil {
		t.Fatalf("DeleteConfiguredRun: %v", err)
	}
	if _, err := client.DeleteConfiguredRun(ctx, &loomcyclepb.DeleteConfiguredRunRequest{RunId: second.GetRunId()}); status.Code(err) != codes.NotFound {
		t.Errorf("second delete = %v, want NotFound", err)
	}
}

// The refusals the HTTP routes answer with a status reach gRPC as the matching
// code: secrets at create and an identity patch are InvalidArgument.
func TestConfiguredRunRPCs_RefusalsMapToCodes(t *testing.T) {
	client, _, _ := configuredGRPC(t)
	ctx := context.Background()
	if _, err := client.CreateConfiguredRun(ctx, &loomcyclepb.RunRequest{
		Agent: "agent", Segments: userSegment("x"), UserBearer: "abcdefghijklmnopqrstuvwxyz",
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("create with a secret = %v, want InvalidArgument", err)
	}
	created, err := client.CreateConfiguredRun(ctx, &loomcyclepb.RunRequest{Agent: "agent", Segments: userSegment("x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateConfiguredRun(ctx, &loomcyclepb.UpdateConfiguredRunRequest{RunId: created.GetRunId(), Patch: []byte(`{"agent":"other"}`)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("identity patch = %v, want InvalidArgument", err)
	}
	if _, err := client.UpdateConfiguredRun(ctx, &loomcyclepb.UpdateConfiguredRunRequest{RunId: "r_missing", Patch: []byte(`{"prompt":"x"}`)}); status.Code(err) != codes.NotFound {
		t.Errorf("patch of a missing run = %v, want NotFound", err)
	}
}

// The new RPCs are runs:create, like Run — not the admin default an unmapped
// RPC falls to.
func TestConfiguredRunRPCs_AreRunsCreate(t *testing.T) {
	for _, m := range []string{"CreateConfiguredRun", "UpdateConfiguredRun", "StartConfiguredRun", "DeleteConfiguredRun"} {
		if got, ok := grpcConsumerScopes[m]; !ok || got != grpcConsumerScopes["Run"] {
			t.Errorf("%s scope = %q (mapped %v), want Run's %q", m, got, ok, grpcConsumerScopes["Run"])
		}
	}
}

func drainStart(client loomcyclepb.LoomcycleClient, runID string) error {
	stream, err := client.StartConfiguredRun(context.Background(), &loomcyclepb.StartConfiguredRunRequest{RunId: runID})
	if err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
