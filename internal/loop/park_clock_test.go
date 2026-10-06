package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
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
