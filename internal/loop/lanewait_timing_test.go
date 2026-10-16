package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
)

// pacedProvider answers end_turn after computing for d, so a call's own
// service time is known.
type pacedProvider struct{ d time.Duration }

func (p pacedProvider) ID() string                                   { return "paced" }
func (p pacedProvider) Probe(context.Context) error                  { return nil }
func (p pacedProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p pacedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p pacedProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event)
	go func() {
		defer close(ch)
		select {
		case <-time.After(p.d):
		case <-ctx.Done():
			return
		}
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
			Usage: &providers.Usage{InputTokens: 100, OutputTokens: 50, Model: "m"}}
	}()
	return ch, nil
}

// TestRun_ALaneWaitIsQueueTimeNotServiceTime — a turn call that waits in its
// provider's lane spends that wait inside Provider.Call, after the loop has
// started the call's clock. The wait is load on the provider, not the model's
// speed: the call's timing must carry it as queue time, so the service time
// queue-aware routing and timeout scaling learn from is the call's own work.
// Only a driver that reports its phases (Ollama) used to get this; for any
// other provider the whole wait read as a slow model.
func TestRun_ALaneWaitIsQueueTimeNotServiceTime(t *testing.T) {
	const (
		hold    = 300 * time.Millisecond
		compute = 50 * time.Millisecond
	)
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"p": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	gated := lanegate.Wrap("p", pacedProvider{d: compute}, sched)

	// Another call holds the lane's only slot for a while.
	_, releaseLane, err := lanegate.Hold(context.Background(), gated, providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var usage *providers.Usage
	queued := make(chan struct{})
	var once sync.Once
	go func() {
		<-queued
		time.Sleep(hold)
		releaseLane()
	}()
	_, err = Run(context.Background(), RunOptions{
		Provider: gated,
		Model:    "m",
		Segments: steerSegs(),
		OnEvent: func(ev providers.Event) {
			switch ev.Type {
			case providers.EventCallQueued:
				once.Do(func() { close(queued) })
			case providers.EventUsage:
				mu.Lock()
				u := *ev.Usage
				usage = &u
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if usage == nil || usage.Timing == nil {
		t.Fatalf("no timed usage event: %+v", usage)
	}
	tm := *usage.Timing
	if tm.DurationMs < hold.Milliseconds() {
		t.Fatalf("DurationMs = %d, want at least the %v lane wait — the call never waited", tm.DurationMs, hold)
	}
	if service := tm.DurationMs - tm.QueueMs; service >= hold.Milliseconds() {
		t.Fatalf("service time = %d ms (DurationMs %d, QueueMs %d): the %v lane wait counted as the model's work",
			service, tm.DurationMs, tm.QueueMs, hold)
	}

	// What the estimator learns from it is the call's own work.
	est := throughput.New(&config.Config{}, nil, nil)
	s, ok := throughput.SampleFromUsage("r1", time.Now(), usage)
	if !ok || !est.Observe(s) {
		t.Fatalf("the call did not become a throughput sample: ok=%v sample=%+v", ok, s)
	}
	if got := est.MedianCallMs(usage.Provider, usage.Model); got >= hold.Milliseconds() {
		t.Fatalf("MedianCallMs = %d, want the ~%v the model computed, not the lane wait", got, compute)
	}
}
