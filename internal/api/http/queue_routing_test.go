package http

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
)

// A queue-aware agent starting a run is sent past a provider whose one chat
// slot is taken and whose calls take ~40 s, to the next candidate; resolving
// the same agent anywhere but a run start (a resume, a report) and an agent
// without the block both keep the cascade's first choice.
func TestResolveAgentDef_QueueAwareRunStartPassesOverABusyProvider(t *testing.T) {
	r := resolve.NewResolver([]string{"busy", "free"}, map[string][]resolve.Candidate{
		"low": {{Provider: "busy", Model: "m1"}, {Provider: "free", Model: "m2"}},
	})
	r.SetReachable("busy", true, []string{"m1"}, "")
	r.SetReachable("free", true, []string{"m2"}, "")
	s := minimalServerWithResolver(t, r)

	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"busy": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	s.SetCallScheduler(sched)
	release, err := sched.Acquire(context.Background(), "busy", config.ModelKindChat, concurrency.CallInfo{Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// The minimal fixture has no estimator; a default config measures, as
	// production does.
	s.throughput = throughput.New(&config.Config{}, nil, nil)
	for i := 0; i < 5; i++ {
		s.throughput.Observe(throughput.Sample{RunID: fmt.Sprintf("r%d", i), At: time.Now(),
			Key: throughput.Key{Provider: "busy", Model: "m1"}, Output: 100,
			CallTiming: providers.CallTiming{DurationMs: 40000}})
	}

	queueAware := config.AgentDef{Tier: "low", Routing: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 20000}}
	resolveAt := func(ctx context.Context, def config.AgentDef) string {
		t.Helper()
		p, m, _, err := s.resolveAgentDef(ctx, def, "", "", "worker", "", false)
		if err != nil {
			t.Fatalf("resolveAgentDef: %v", err)
		}
		return p + "/" + m
	}

	if got := resolveAt(withRunStart(context.Background()), queueAware); got != "free/m2" {
		t.Errorf("queue-aware run start = %s, want free/m2 (busy's estimated wait ~40s > 20s)", got)
	}
	if got := resolveAt(context.Background(), queueAware); got != "busy/m1" {
		t.Errorf("queue-aware, not a run start = %s, want busy/m1 (a resume or a report must not move)", got)
	}
	if got := resolveAt(withRunStart(context.Background()), config.AgentDef{Tier: "low"}); got != "busy/m1" {
		t.Errorf("no routing block = %s, want busy/m1 as always", got)
	}
}

// idProvider is a provider that is only an id: the move hook looks one up.
type idProvider struct{ id string }

func (p idProvider) ID() string                                   { return p.id }
func (p idProvider) Probe(context.Context) error                  { return nil }
func (p idProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p idProvider) Capabilities() providers.Capabilities         { return providers.Capabilities{} }
func (p idProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	return nil, nil
}

// After its start, a queue-aware run moves only when it holds no slot of its
// provider's lane (a lease is its warm prompt cache) AND its next call would
// wait past switch_after_ms there; the move goes where a run start would.
func TestQueueMoveFn_MovesOnlyWithoutALeaseAndPastSwitchAfter(t *testing.T) {
	r := resolve.NewResolver([]string{"busy", "free"}, map[string][]resolve.Candidate{
		"low": {{Provider: "busy", Model: "m1"}, {Provider: "free", Model: "m2"}},
	})
	r.SetReachable("busy", true, []string{"m1"}, "")
	r.SetReachable("free", true, []string{"m2"}, "")
	s := minimalServerWithResolver(t, r)
	s.providers = &multiResolver{byID: map[string]providers.Provider{"busy": idProvider{"busy"}, "free": idProvider{"free"}}}
	s.throughput = throughput.New(&config.Config{}, nil, nil)
	for i := 0; i < 5; i++ {
		s.throughput.Observe(throughput.Sample{RunID: fmt.Sprintf("s%d", i), At: time.Now(),
			Key: throughput.Key{Provider: "busy", Model: "m1"}, Output: 100,
			CallTiming: providers.CallTiming{DurationMs: 40000}})
	}
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"busy": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 2, AffinityHold: time.Minute}}},
	})
	s.SetCallScheduler(sched)
	ctx := context.Background()
	acquire := func(owner string) func() {
		t.Helper()
		rel, err := sched.Acquire(ctx, "busy", config.ModelKindChat, concurrency.CallInfo{Owner: owner, Model: "m1"})
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}

	def := config.AgentDef{Tier: "low", Routing: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 15000}}
	move := s.queueMoveFn(def, "run-me", "", "", "worker", "", false)
	if move == nil {
		t.Fatal("a queue-aware tier agent got no move hook")
	}
	ask := func() string {
		p, m, _, moved := move(ctx, idProvider{"busy"}, "m1")
		if !moved {
			return "stay"
		}
		return p.ID() + "/" + m
	}

	// The run's own call ended and left its slot reserved for it; another run
	// holds the other slot, so the lane is full.
	acquire("run-me")()
	defer acquire("other")()
	if got := ask(); got != "stay" {
		t.Fatalf("holding its lease: %s, want stay (moving would throw its warm slot away)", got)
	}

	// Its lease is gone (the run ended it, as a preemption or an expiry
	// would) and a third run took that slot: both slots are taken, so the
	// next call would wait about (0+1)/2 × 40 s = 20 s, past its 15 s.
	sched.Done("run-me")
	defer acquire("third")()
	if got := ask(); got != "free/m2" {
		t.Fatalf("no lease, ~40s wait: %s, want a move to free/m2", got)
	}

	// Under switch_after_ms it stays even without a lease.
	patient := s.queueMoveFn(config.AgentDef{Tier: "low", Routing: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 120000}},
		"run-patient", "", "", "worker", "", false)
	if _, _, _, moved := patient(ctx, idProvider{"busy"}, "m1"); moved {
		t.Error("a wait under switch_after_ms moved the run")
	}

	// No hook for an agent that is not queue-aware, or is pinned.
	if s.queueMoveFn(config.AgentDef{Tier: "low"}, "r", "", "", "w", "", false) != nil ||
		s.queueMoveFn(config.AgentDef{Provider: "busy", Model: "m1", Routing: &config.AgentRouting{QueueAware: true}}, "r", "", "", "w", "", false) != nil {
		t.Error("a move hook for an agent that is not queue-aware and tier-routed")
	}
}
