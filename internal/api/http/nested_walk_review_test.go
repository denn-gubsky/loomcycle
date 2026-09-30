package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// nothingArmed is the nested walk's own breakpoint set: nobody armed anything
// on it.
type nothingArmed struct{}

func (nothingArmed) Armed(string, teamrun.BreakpointPhase) bool { return false }

// nestedWalkTool stands in for TeamDef op=run called by a member: it walks one
// agent+consolidator state on the ctx the member's tool call receives, with the
// member-spawning SpawnFunc and a review source, as the TeamDef tool wires it.
type nestedWalkTool struct {
	srv *Server
	// toolArmed records whether the member's tool call still carried a review
	// arming — the enclosing walk's, since nothing else set one.
	toolArmed atomic.Bool
	// finished is set before the tool returns, so the member's own hold —
	// which begins only after — never reads the nested walk's short deadline.
	finished atomic.Bool
	walkErr  chan error
}

func (*nestedWalkTool) Name() string                 { return "nest" }
func (*nestedWalkTool) Description() string          { return "run a nested team" }
func (*nestedWalkTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n *nestedWalkTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	n.toolArmed.Store(teamrun.ReviewArming(ctx) != nil)
	r := teamrun.NewAgentRunner(n.srv.runTeamMember, teamrun.WithMemberReview(nothingArmed{}, 0))
	st := teamgraph.State{ID: "inner", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerAgent, Agent: "inner", Consolidator: "judge"}}
	_, err := r.RunHandler(ctx, st, &teamrun.Task{Input: "go"})
	n.finished.Store(true)
	n.walkErr <- err
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: "nested walk done"}, nil
}

// A walk run from inside a member of a review-armed state (a member that calls
// TeamDef op=run) holds nothing the operator did not arm on IT: its
// consolidator finishes without parking for review, so the nested walk
// completes. The enclosing walk's own review of the member still holds it.
//
// The enclosing walk's deadline is short while the nested walk runs, so a
// consolidator wrongly held on the outer arming expires rejected and fails the
// nested walk — the failure the operator would see — rather than hanging.
func TestTeamMember_NestedWalkDoesNotInheritTheEnclosingReviewArming(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"outer": {Model: "stub-model", Tools: []string{"nest"}, SystemPrompt: "run the inner team"},
			"inner": {Model: "stub-model", SystemPrompt: "write"},
			"judge": {Model: "stub-model", SystemPrompt: "judge"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	text := func(s string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventText, Text: s},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		{ // the outer member calls the nested team
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_nest", Name: "nest", Input: json.RawMessage(`{}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
		text("inner work"),                   // the nested walk's member
		text("looks right\nsignal: success"), // its consolidator
		text("outer answer"),                 // the outer member, after the tool result
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "nested.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	nest := &nestedWalkTool{walkErr: make(chan error, 1)}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{nest}, concurrency.New(4, 4, 100*time.Millisecond), st)
	nest.srv = srv
	srv.SetSteerRegistry(steer.NewRegistry(0))
	h := &reviewHarness{t: t, srv: srv, st: st}
	h.ts = httptest.NewServer(srv.Mux())
	t.Cleanup(h.ts.Close)

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	ctx = teamrun.WithReviewArming(ctx, func(context.Context) bool { return true })
	ctx = teamrun.WithReviewTTL(ctx, func() time.Duration {
		if nest.finished.Load() {
			return 0
		}
		return 200 * time.Millisecond
	})
	done := make(chan teamrun.SpawnResult, 1)
	go func() {
		res, _ := srv.runTeamMember(ctx, "outer", teamrun.Prompt{Input: "go"}, "")
		done <- res
	}()

	select {
	case err := <-nest.walkErr:
		if err != nil {
			t.Errorf("the nested walk failed: %v — its consolidator was held on the enclosing walk's arming", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the nested walk never finished")
	}
	if nest.toolArmed.Load() {
		t.Error("the member's tool call carried the enclosing walk's review arming")
	}

	// The enclosing walk still reviews its own member.
	var member string
	waitFor(t, "the outer member to be held", func() bool {
		runs, _ := st.ListActiveRunsByUser(context.Background(), "", "u1", store.RunRunning)
		for _, r := range runs {
			if heldForReview(context.Background(), st, r.ID) {
				member = r.ID
			}
		}
		return member != ""
	})
	if code, body := h.review(member, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	if res := awaitMember(t, done); res.Status != string(store.RunCompleted) || res.RunID != member {
		t.Errorf("outer member result = %+v, want %s completed", res, member)
	}
}
