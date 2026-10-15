package http

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// queueRetuneServer is a server with two tiers over two providers, where
// "busy" has one chat slot, taken by another run, and its models take ~40 s a
// call: a queue-aware run start passes over it, and a run with no lease there
// would wait past any switch_after_ms below 20 s.
//
//	low:  busy/m1, free/m2
//	high: busy/h1, free/h2
func queueRetuneServer(t *testing.T, agents map[string]config.AgentDef) (*Server, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = agents
	cfg.Tiers = map[string][]config.TierCandidate{
		"low":  {{Provider: "busy", Model: "m1"}, {Provider: "free", Model: "m2"}},
		"high": {{Provider: "busy", Model: "h1"}, {Provider: "free", Model: "h2"}},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "queue-retune.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provs := &multiResolver{byID: map[string]providers.Provider{"busy": idProvider{"busy"}, "free": idProvider{"free"}}}
	s := New(cfg, provs, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	r := resolve.NewResolver([]string{"busy", "free"}, map[string][]resolve.Candidate{
		"low":  {{Provider: "busy", Model: "m1"}, {Provider: "free", Model: "m2"}},
		"high": {{Provider: "busy", Model: "h1"}, {Provider: "free", Model: "h2"}},
	})
	r.SetReachable("busy", true, []string{"m1", "h1"}, "")
	r.SetReachable("free", true, []string{"m2", "h2"}, "")
	s.SetResolver(r)

	s.throughput = throughput.New(&config.Config{}, nil, nil)
	for i := 0; i < 5; i++ {
		for _, m := range []string{"m1", "h1"} {
			s.throughput.Observe(throughput.Sample{RunID: fmt.Sprintf("%s-%d", m, i), At: time.Now(),
				Key: throughput.Key{Provider: "busy", Model: m}, Output: 100,
				CallTiming: providers.CallTiming{DurationMs: 40000}})
		}
	}
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"busy": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	s.SetCallScheduler(sched)
	release, err := sched.Acquire(context.Background(), "busy", config.ModelKindChat, concurrency.CallInfo{Owner: "someone-else", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return s, st
}

// startQueueRetuneRun records a run of agent that started on model with rec as
// its configuration record.
func startQueueRetuneRun(t *testing.T, st store.Store, agent, model string, rec runConfigRecord) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", agent, "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_" + agent, Model: model, RunConfig: rec.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	// Read back: the row a retune is handed carries the session's agent.
	run, err := st.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

var queueAware15s = &config.AgentRouting{QueueAware: true, SwitchAfterMS: 15000}

// A queue-aware run that started past a busy first candidate stays where it
// started at its operator turns: only a retune of its routing moves it. The
// turn used to re-resolve without the run-start estimate and compare with the
// started model, so the first turn of every such run moved it back onto the
// busy provider and reported an operator change nobody made.
func TestOperatorTurn_KeepsTheQueueAwareStartUntilARoutingRetune(t *testing.T) {
	def := config.AgentDef{Tier: "low", Routing: queueAware15s}
	s, st := queueRetuneServer(t, map[string]config.AgentDef{"worker": def})
	ctx := context.Background()

	pid, model, _, err := s.resolveAgentDef(withRunStart(ctx), def, "", "", "worker", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if pid+"/"+model != "free/m2" {
		t.Fatalf("run start resolved %s/%s, want free/m2 (the fixture's busy lane is full)", pid, model)
	}
	run := startQueueRetuneRun(t, st, "worker", model, runConfigRecord{})
	turn := s.reResolveOnOperatorTurnFn(run.ID, "", "", "worker", "", false, nil, nil)
	stored := func() string {
		t.Helper()
		r, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r.Model
	}

	for i := 1; i <= 2; i++ {
		p, m, _, changed, err := turn(ctx)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if changed {
			t.Fatalf("operator turn %d moved the run to %s/%s with no retune; want it kept on free/m2", i, p.ID(), m)
		}
	}
	if got := stored(); got != "m2" {
		t.Fatalf("runs.model = %q after the turns, want m2", got)
	}

	// A retune of its routing is adopted at the next turn, and only once.
	if _, err := s.retuneRun(ctx, run, &runOverridesWire{Tier: "high"}); err != nil {
		t.Fatalf("retune: %v", err)
	}
	p, m, _, changed, err := turn(ctx)
	if err != nil || !changed || p.ID()+"/"+m != "busy/h1" {
		t.Fatalf("after a tier retune: changed=%v to %v/%s err=%v, want busy/h1", changed, p, m, err)
	}
	if got := stored(); got != "h1" {
		t.Errorf("runs.model = %q after the retune was adopted, want h1", got)
	}
	if _, _, _, changed, err := turn(ctx); err != nil || changed {
		t.Errorf("the turn after adopting reported changed=%v err=%v, want no change", changed, err)
	}
}

// After its lease is gone, a run moves within the routing it runs on NOW: a
// tier it was retuned to, not the tier it started on, and not at all once a
// retune pinned it to a model. The hook used to re-resolve the definition the
// run started with, so a run retuned to tier high moved back into tier low.
func TestQueueMoveFn_MovesWithinTheRetunedRouting(t *testing.T) {
	def := config.AgentDef{Tier: "low", Routing: queueAware15s}
	s, st := queueRetuneServer(t, map[string]config.AgentDef{"worker": def})
	ctx := context.Background()
	run := startQueueRetuneRun(t, st, "worker", "m2", runConfigRecord{})
	move := s.queueMoveFn(def, nil, run.ID, "", "", "worker", "", false)
	if move == nil {
		t.Fatal("a queue-aware tier agent got no move hook")
	}
	ask := func(provider, model string) string {
		t.Helper()
		p, m, _, moved := move(ctx, idProvider{provider}, model)
		if !moved {
			return "stay"
		}
		return p.ID() + "/" + m
	}

	if got := ask("busy", "m1"); got != "free/m2" {
		t.Fatalf("before any retune: %s, want a move to free/m2", got)
	}
	// Retuned to tier high and adopted there, on the busy provider.
	if _, err := s.retuneRun(ctx, run, &runOverridesWire{Tier: "high"}); err != nil {
		t.Fatalf("retune: %v", err)
	}
	if got := ask("busy", "h1"); got != "free/h2" {
		t.Errorf("retuned to tier high: %s, want a move to free/h2 (tier high's free candidate)", got)
	}
	// Pinned by a retune: one place to be, so nowhere to move.
	if _, err := s.retuneRun(ctx, run, &runOverridesWire{Model: "h1"}); err != nil {
		t.Fatalf("retune: %v", err)
	}
	if got := ask("busy", "h1"); got != "stay" {
		t.Errorf("retuned onto a model pin: %s, want stay", got)
	}
}

// A run that started pinned to a model and is retuned onto a queue-aware tier
// moves like any queue-aware tier run. It used to get no move hook at all, so
// the retune's routing block was accepted and never acted on.
func TestQueueMoveFn_APinnedStartRetunedOntoATierCanMove(t *testing.T) {
	def := config.AgentDef{Provider: "busy", Model: "m1"}
	s, st := queueRetuneServer(t, map[string]config.AgentDef{"pinned": def})
	ctx := context.Background()
	run := startQueueRetuneRun(t, st, "pinned", "m1", runConfigRecord{})
	move := s.queueMoveFn(def, nil, run.ID, "", "", "pinned", "", false)
	if move == nil {
		t.Fatal("a pinned run got no move hook, so no retune can ever make it move")
	}
	if _, _, _, moved := move(ctx, idProvider{"busy"}, "m1"); moved {
		t.Fatal("a pinned run moved")
	}
	if _, err := s.retuneRun(ctx, run, &runOverridesWire{Tier: "low", Routing: queueAware15s}); err != nil {
		t.Fatalf("retune: %v", err)
	}
	p, m, _, moved := move(ctx, idProvider{"busy"}, "m1")
	if !moved || p.ID()+"/"+m != "free/m2" {
		t.Errorf("retuned onto queue-aware tier low: moved=%v to %v/%s, want free/m2", moved, p, m)
	}
}

// A child spawned after its parent was retuned inherits the routing the
// parent holds now, as a resumed parent's child already did. A live parent
// used to offer the record it started with.
func TestOverrideInheritance_AChildInheritsItsParentsRetunedRouting(t *testing.T) {
	srv := routingServer(t)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "router", "")
	if err != nil {
		t.Fatal(err)
	}
	startRec := runConfigRecord{Routing: &routingOverride{Tier: "middle"}}
	parent, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_parent", Model: "model-a", RunConfig: startRec.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.updateRunConfig(ctx, parent.ID, func(rec *runConfigRecord, _ bool) error {
		rec.Routing = &routingOverride{Tier: "cheap"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spawnCtx := tools.WithRunOverrides(ctx, tools.RunOverridesValue{
		Record: startRec.marshal(), AgentName: "router", RunID: parent.ID,
	})

	got, cfg := srv.inheritOverridesForChild(spawnCtx, config.AgentDef{Tier: "middle"}, runConfigRecord{}, "", "router")
	if got.Tier != "cheap" {
		t.Errorf("child tier = %q, want cheap (the parent's retuned tier)", got.Tier)
	}
	if cfg.Routing == nil || cfg.Routing.Tier != "cheap" {
		t.Errorf("child record routing = %+v, want the parent's retuned tier recorded", cfg.Routing)
	}
}
