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
