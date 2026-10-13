package lanegate

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// waitCalls waits until the fake has been asked n times.
func waitCalls(t *testing.T, what string, calls func() int32, n int32) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if calls() == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s: %d calls reached the driver, want %d", what, calls(), n)
}

// stays asserts the count does not move for a short while.
func stays(t *testing.T, what string, calls func() int32, n int32) {
	t.Helper()
	time.Sleep(60 * time.Millisecond)
	if got := calls(); got != n {
		t.Fatalf("%s: %d calls reached the driver, want it to stay at %d", what, got, n)
	}
}

// TestDecision_WithNoLaneIsBoundedByTheFloor — a provider with no decision lane
// does not mean no bound: the wrapper's built-in floor lets FloorCap decisions
// through at once and queues the rest, as the driver's private limit used to.
// Without it, a fan-out routing dozens of items would ask them all together.
func TestDecision_WithNoLaneIsBoundedByTheFloor(t *testing.T) {
	for name, sched := range map[string]*concurrency.CallScheduler{
		"no scheduler":       nil,
		"no lane configured": concurrency.NewCallScheduler(nil),
		// A lane of another kind on the same provider is not a decision lane.
		"only a chat lane": kindLane(config.ModelKindChat, concurrency.LaneConfig{MaxConcurrent: 1}),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeDecision{gate: make(chan struct{})}
			d := WrapDecision("p", f, sched)
			results := make(chan error, FloorCap+2)
			for i := 0; i < FloorCap+2; i++ {
				go func() {
					_, err := d.Decide(context.Background(), yesNo)
					results <- err
				}()
			}
			waitCalls(t, "at the floor", f.calls.Load, FloorCap)
			stays(t, "past the floor", f.calls.Load, FloorCap)
			close(f.gate)
			for i := 0; i < FloorCap+2; i++ {
				if err := recv(t, "a decision", results); err != nil {
					t.Fatalf("decision %d: %v", i, err)
				}
			}
		})
	}
}

// TestDecision_AConfiguredLaneReplacesTheFloor — the floor never undercuts a
// cap the operator chose: with a decision lane of FloorCap+2, that many
// decisions run at once.
func TestDecision_AConfiguredLaneReplacesTheFloor(t *testing.T) {
	const laneCap = FloorCap + 2
	sched := kindLane(config.ModelKindDecision, concurrency.LaneConfig{MaxConcurrent: laneCap})
	f := &fakeDecision{gate: make(chan struct{})}
	d := WrapDecision("p", f, sched)
	results := make(chan error, laneCap+1)
	for i := 0; i < laneCap+1; i++ {
		go func() {
			_, err := d.Decide(context.Background(), yesNo)
			results <- err
		}()
	}
	waitCalls(t, "at the lane's cap", f.calls.Load, laneCap)
	stays(t, "past the lane's cap", f.calls.Load, laneCap)
	close(f.gate)
	for i := 0; i < laneCap+1; i++ {
		if err := recv(t, "a decision", results); err != nil {
			t.Fatalf("decision %d: %v", i, err)
		}
	}
}

// TestDecision_TheFloorHonoursAQueueBound — a direct decision is bounded even
// where no lane is configured: with the floor full and the bound reached, the
// next one is refused.
func TestDecision_TheFloorHonoursAQueueBound(t *testing.T) {
	f := &fakeDecision{gate: make(chan struct{})}
	d := WrapDecision("p", f, nil)
	results := make(chan error, FloorCap)
	for i := 0; i < FloorCap; i++ {
		go func() {
			_, err := d.Decide(context.Background(), yesNo)
			results <- err
		}()
	}
	waitCalls(t, "the floor full", f.calls.Load, FloorCap)

	_, err := d.Decide(WithQueueBound(context.Background(), 0), yesNo)
	if !concurrency.IsProviderConcurrencyExhausted(err) {
		t.Fatalf("a bounded decision past a full floor = %v, want provider_concurrency_exhausted", err)
	}
	close(f.gate)
	for i := 0; i < FloorCap; i++ {
		if err := recv(t, "a decision", results); err != nil {
			t.Fatal(err)
		}
	}
}

// heldOpenProvider is a driver every call of which stays open until release.
type heldOpenProvider struct {
	fakeProvider
	release chan struct{}
}

func (p *heldOpenProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.calls.Add(1)
	ch := make(chan providers.Event, 1)
	go func() {
		defer close(ch)
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		ch <- done()
	}()
	return ch, nil
}

func drain(ch <-chan providers.Event) {
	for range ch {
	}
}

// TestWrapWithFloor_BoundsAServicesCallsOnAnUncappedProvider — a service's own
// driver (the reranker) wrapped with a floor is bounded at FloorCap while its
// provider has no chat lane; the same driver wrapped WITHOUT one is not, which
// is what a registry provider is: an uncapped provider's chat calls stay
// uncapped.
func TestWrapWithFloor_BoundsAServicesCallsOnAnUncappedProvider(t *testing.T) {
	run := func(p *Provider, inner *heldOpenProvider) {
		for i := 0; i < FloorCap+2; i++ {
			go func() {
				ch, err := p.Call(context.Background(), providers.Request{Model: "m"})
				if err != nil {
					t.Errorf("Call: %v", err)
					return
				}
				drain(ch)
			}()
		}
	}

	floored := &heldOpenProvider{release: make(chan struct{})}
	run(WrapWithFloor("p", floored, concurrency.NewCallScheduler(nil)), floored)
	waitCalls(t, "at the floor", floored.calls.Load, FloorCap)
	stays(t, "past the floor", floored.calls.Load, FloorCap)
	close(floored.release)
	waitCalls(t, "once the floor frees up", floored.calls.Load, FloorCap+2)

	plain := &heldOpenProvider{release: make(chan struct{})}
	run(Wrap("p", plain, concurrency.NewCallScheduler(nil)), plain)
	waitCalls(t, "with no floor", plain.calls.Load, FloorCap+2)
	close(plain.release)
}

// TestHoldEmbedder_TheEmbedUnderItTakesNoSecondSlot — a request that takes the
// embedder's slot up front (to learn at admission whether the provider can take
// it) then makes its Embed under the ctx HoldEmbedder returns. On a lane of one
// slot that Embed must not wait for a second one: it would wait for itself.
// HoldEmbedder finds the lane through whatever wraps the embedder.
func TestHoldEmbedder_TheEmbedUnderItTakesNoSecondSlot(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	f := &fakeEmbedder{}
	e := providers.Embedder(providers.ObserveEmbedder(WrapEmbedder("p", f, sched)))

	ctx, release, err := HoldEmbedder(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if st := kindStats(sched, config.ModelKindEmbedder); st.Active != 1 {
		t.Fatalf("after HoldEmbedder: %+v, want the slot held", st)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := e.Embed(ctx, []string{"x"})
		errc <- err
	}()
	if err := recv(t, "the Embed under the held ctx", errc); err != nil {
		t.Fatal(err)
	}
	if st := kindStats(sched, config.ModelKindEmbedder); st.Active != 1 || st.Queued != 0 {
		t.Fatalf("after the Embed: %+v, want still exactly the one held slot", st)
	}

	// An Embed NOT under that ctx is a different call: it waits for the slot.
	other := make(chan error, 1)
	go func() {
		_, err := e.Embed(context.Background(), []string{"y"})
		other <- err
	}()
	waitKind(t, sched, config.ModelKindEmbedder, "one call queued", func(st concurrency.LaneStats) bool { return st.Queued == 1 })
	release()
	if err := recv(t, "the other Embed", other); err != nil {
		t.Fatal(err)
	}
	waitKind(t, sched, config.ModelKindEmbedder, "idle", func(st concurrency.LaneStats) bool { return st.Active == 0 && st.Queued == 0 })

	// A bounded hold past the bound is refused, and an ungated embedder holds
	// nothing.
	relA, err := sched.Acquire(context.Background(), "p", config.ModelKindEmbedder, concurrency.CallInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := HoldEmbedder(WithQueueBound(context.Background(), 0), e); !concurrency.IsProviderConcurrencyExhausted(err) {
		t.Fatalf("HoldEmbedder past the bound = %v, want provider_concurrency_exhausted", err)
	}
	relA()
	if _, rel, err := HoldEmbedder(context.Background(), &fakeEmbedder{}); err != nil {
		t.Fatalf("HoldEmbedder on an ungated embedder: %v", err)
	} else {
		rel()
	}
}
