package grpc

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Run mutations over gRPC, against the REAL HTTP server as the connector — the
// ownership rule lives there. An isolated member (substrate:user, which implies
// runs:create) must get the NotFound an unknown id gets for another user's run
// in its tenant, and that run must be left alone.

func ownershipGRPC(t *testing.T) (*Server, *lchttp.Server, store.Store, *answerProvider) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-ownership.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agentx": {Model: "stub-model", SystemPrompt: "hi", Tools: []string{}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	prov := &answerProvider{}
	httpSrv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
	return adapter, httpSrv, st, prov
}

// liveCancel registers agentID as live in the HTTP server's cancel registry
// (the one the connector cancels through) and reports whether it was cancelled.
func liveCancel(t *testing.T, httpSrv *lchttp.Server, st store.Store, agentID string) func() bool {
	t.Helper()
	run, err := st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled atomic.Bool
	reg := httpSrv.CancelRegistry()
	if err := reg.Register(cancel.Entry{AgentID: agentID, RunID: run.ID, UserID: run.UserID, StartedAt: time.Now()},
		func(error) { cancelled.Store(true) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Deregister(agentID) })
	return cancelled.Load
}

func TestGrpcCancelAgent_IsolatedMemberCannotCancelAnotherUsersRun(t *testing.T) {
	adapter, httpSrv, st, _ := ownershipGRPC(t)
	seedRun(t, st, "acme", "alice", "a_alice")
	seedRun(t, st, "acme", "bob", "a_bob")
	seedRun(t, st, "acme", "alice", "a_alice_done")
	done, _ := st.GetRunByAgentID(context.Background(), "a_alice_done")
	if err := st.FinishRun(context.Background(), done.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	aliceLive := liveCancel(t, httpSrv, st, "a_alice")
	bobLive := liveCancel(t, httpSrv, st, "a_bob")

	cancelAs := func(ctx context.Context, id string) (*loomcyclepb.CancelAgentResponse, error) {
		return adapter.CancelAgent(ctx, &loomcyclepb.CancelAgentRequest{AgentId: id})
	}
	bob := scopedCtx("acme", "bob", auth.ScopeUser)
	_, ghostErr := cancelAs(bob, "a_ghost")
	if status.Code(ghostErr) != codes.NotFound {
		t.Fatalf("unknown agent id: %v, want NotFound", ghostErr)
	}
	for _, id := range []string{"a_alice", "a_alice_done"} {
		resp, err := cancelAs(bob, id)
		want := strings.ReplaceAll(status.Convert(ghostErr).Message(), "a_ghost", id)
		if status.Code(err) != codes.NotFound || status.Convert(err).Message() != want {
			t.Errorf("isolated member cancelling %s = %v, %v; want NotFound %q", id, resp, err, want)
		}
	}
	if aliceLive() {
		t.Fatal("an isolated member cancelled another user's live run over gRPC")
	}

	if resp, err := cancelAs(bob, "a_bob"); err != nil || resp.GetCancelledCount() != 1 || !bobLive() {
		t.Errorf("isolated member cancelling its own run = %v, %v; want one cancelled", resp, err)
	}
	if resp, err := cancelAs(scopedCtx("acme", "op", auth.ScopeTenant), "a_alice"); err != nil || resp.GetCancelledCount() != 1 || !aliceLive() {
		t.Errorf("tenant operator cancelling a member's run = %v, %v; want one cancelled", resp, err)
	}
}
