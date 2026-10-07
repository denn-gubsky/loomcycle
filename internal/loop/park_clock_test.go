package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// clockedProvider is reviewProvider for a run bounded by time, as code-js is:
// the loop gives its runs a clock, and it records the active time each call
// sees.
type clockedProvider struct {
	*reviewProvider
	mu     sync.Mutex
	active []time.Duration
}

func (p *clockedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, UnboundedIterations: true}
}

func (p *clockedProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.active = append(p.active, providers.RunClockFromContext(ctx).State().Active)
	p.mu.Unlock()
	return p.reviewProvider.Call(ctx, req)
}

// A waiting run hands the pause its own ctx, so the pause can stop and record
// the run's clock — a park that handed it anything else would leave a
// clocked run's lifetime running through the pause and its snapshot without
// a clock.
func TestPark_AWaitingRunHandsThePauseItsClock(t *testing.T) {
	for name, mutate := range map[string]func(*RunOptions){
		"held for review":  nil,
		"parked for input": func(o *RunOptions) { o.Review = false; o.Interactive = true },
	} {
		t.Run(name, func(t *testing.T) {
			gate := newIdleGate()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := startReviewRun(t, ctx, func(o *RunOptions) {
				o.Provider = &clockedProvider{reviewProvider: o.Provider.(*reviewProvider)}
				o.PauseGate = gate
				if mutate != nil {
					mutate(o)
				}
			})
			if name == "held for review" {
				r.waitFor(t, providers.EventAwaitingReview)
			} else {
				r.waitFor(t, providers.EventAwaitingInput)
			}
			gate.declare()
			waitCount(t, "paused records", gate.records.Load, 1)
			if gate.clocked.Load() != 1 {
				t.Error("the pause was not handed the waiting run's clock")
			}
			cancel()
			r.finish(t)
		})
	}
}

// A run held for a reviewer, or parked for an operator's next message, is
// waiting on a person: for a run bounded by active time the hold does not
// spend its budget, or a slow reviewer would leave it nothing to revise with.
func TestPark_AHoldForAPersonDoesNotSpendTheBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*RunOptions)
		held   providers.EventType
		answer string
	}{
		"held for review":  {nil, providers.EventAwaitingReview, steer.KindReject},
		"parked for input": {func(o *RunOptions) { o.Review = false; o.Interactive = true }, providers.EventAwaitingInput, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var prov *clockedProvider
			r := startReviewRun(t, ctx, func(o *RunOptions) {
				prov = &clockedProvider{reviewProvider: o.Provider.(*reviewProvider)}
				o.Provider = prov
				if tc.mutate != nil {
					tc.mutate(o)
				}
			})
			r.waitFor(t, tc.held)
			const heldFor = 300 * time.Millisecond
			time.Sleep(heldFor)
			// Sent now: a verdict from before the hold began is stale.
			r.q <- verdict(tc.answer, "go on")
			deadline := time.Now().Add(2 * time.Second)
			for r.prov.calls() < 2 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			prov.mu.Lock()
			active := append([]time.Duration(nil), prov.active...)
			prov.mu.Unlock()
			if len(active) < 2 {
				t.Fatalf("the run made %d calls, want its next turn after the answer", len(active))
			}
			if active[1] > heldFor/2 {
				t.Errorf("the next turn starts with %s active after a %s hold — the wait on a person was spent as budget", active[1], heldFor)
			}
			cancel()
			r.finish(t)
		})
	}
}
