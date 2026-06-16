package loop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// slowCallProvider simulates a slow local model: Call blocks (a long prefill)
// before returning the stream, with no events emitted during the block.
type slowCallProvider struct {
	delay time.Duration
}

func (p *slowCallProvider) ID() string                                   { return "slowcall-test" }
func (p *slowCallProvider) Probe(context.Context) error                  { return nil }
func (p *slowCallProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *slowCallProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *slowCallProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1}}
	close(ch)
	return ch, nil
}

// TestRun_HeartbeatPulsedDuringSlowCall locks the run-lifetime heartbeat: a
// single iteration whose model call blocks far longer than the heartbeat
// cadence (a large-context prefill on a slow local model, or same-provider
// retry backoff) must keep pulsing OnHeartbeat, so the stale-run sweeper
// doesn't reap a live-but-slow run as crashed (the heartbeat_timeout that
// killed a slow ollama review). Fail-before: with OnHeartbeat firing ONLY at
// iteration start, a one-iteration run pulses exactly once and this fails.
func TestRun_HeartbeatPulsedDuringSlowCall(t *testing.T) {
	orig := parkHeartbeatInterval
	parkHeartbeatInterval = 15 * time.Millisecond
	defer func() { parkHeartbeatInterval = orig }()

	var beats atomic.Int64
	prov := &slowCallProvider{delay: 120 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := Run(ctx, RunOptions{
		Provider:      prov,
		Model:         "x",
		Tools:         []tools.Tool{noopTool{}},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:      steerSegs(),
		MaxIterations: 1,
		OnHeartbeat:   func() { beats.Add(1) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One iteration's per-iteration pulse fires once; the run-lifetime ticker
	// adds ~8 more during the 120ms blocking call at a 15ms cadence. Without
	// the ticker only the single iteration-start pulse fires.
	if n := beats.Load(); n < 3 {
		t.Fatalf("OnHeartbeat fired %d times during a slow model call, want >= 3 "+
			"(the run-lifetime ticker must pulse while the call blocks)", n)
	}
}
