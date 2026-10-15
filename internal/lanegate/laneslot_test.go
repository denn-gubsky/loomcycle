package lanegate

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// slotProvider records the lane slot on the ctx each call is made under.
type slotProvider struct {
	fakeProvider
	slots []int // -1 for a call with none
}

func (p *slotProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	slot, ok := providers.LaneSlot(ctx)
	if !ok {
		slot = -1
	}
	p.slots = append(p.slots, slot)
	ch := make(chan providers.Event, 1)
	ch <- done()
	close(ch)
	return ch, nil
}

// The driver of a capped provider is called under the number of the lane slot
// the call was granted — the same one for a run's next call while its
// reservation holds it — and an uncapped provider's driver gets none.
func TestCall_HandsTheDriverItsLaneSlot(t *testing.T) {
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"p": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 2, AffinityHold: 60e9}}},
	})
	inner := &slotProvider{}
	p := Wrap("p", inner, sched)
	run := func(id string) {
		ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{RunID: id})
		// A turn call: only those hold a reservation.
		ch, err := p.Call(ctx, providers.Request{Model: "m", OnEvent: func(providers.Event) {}})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		for range ch {
		}
	}
	run("run-a")
	run("run-b")
	run("run-a")
	if want := []int{0, 1, 0}; len(inner.slots) != 3 || inner.slots[0] != want[0] || inner.slots[1] != want[1] || inner.slots[2] != want[2] {
		t.Fatalf("slots = %v, want %v (run-a back on its reserved slot 0)", inner.slots, want)
	}

	uncapped := &slotProvider{}
	ch, err := Wrap("q", uncapped, sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(uncapped.slots) != 1 || uncapped.slots[0] != -1 {
		t.Fatalf("uncapped provider's call slots = %v, want none", uncapped.slots)
	}
}

// callOn makes one call on p under ctx and drains it.
func callOn(ctx context.Context, t *testing.T, p providers.Provider, req providers.Request) {
	t.Helper()
	ch, err := p.Call(ctx, req)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
}

// A call made on the provider Hold returns runs on the lane slot Hold took for
// it, and its driver is told that slot as a Provider.Call's driver is. Sent
// unpinned, a pinning backend would pick a slot itself — possibly one the lane
// keeps reserved for a run's prompt cache, or one another call is pinned to.
func TestHold_CallRunsOnTheHeldLaneSlot(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 2})
	inner := &slotProvider{}
	p := Wrap("p", inner, sched)
	first, releaseFirst, err := Hold(context.Background(), p, providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	defer releaseFirst()
	second, releaseSecond, err := Hold(context.Background(), p, providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("second Hold: %v", err)
	}
	defer releaseSecond()

	// The gateway makes its call under the request's ctx, which carries no slot.
	callOn(context.Background(), t, second, providers.Request{Model: "m"})
	callOn(context.Background(), t, first, providers.Request{Model: "m"})
	if len(inner.slots) != 2 || inner.slots[0] != 1 || inner.slots[1] != 0 {
		t.Fatalf("driver saw slots %v, want [1 0] (each call on the slot its Hold took)", inner.slots)
	}
	if st := laneStats(t, sched); st.Active != 2 {
		t.Fatalf("active = %d while both holds are open, want 2: a held call took a slot of its own", st.Active)
	}
	if got, ok := providers.As[*slotProvider](second); !ok || got != inner {
		t.Fatal("the driver is not reachable through the provider Hold returned")
	}
}

// A call a run makes beside its own turns — a tool's listwise rerank, a summary;
// it carries no OnEvent — neither runs on the slot the lane keeps reserved for
// the run nor starts a reservation of its own. On the reserved slot it would be
// pinned to the backend slot holding the run's prompt cache, and its prompt
// would overwrite that cache before the run's next turn.
func TestCall_SideCallStaysOffTheRunsReservedSlot(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 2, AffinityHold: time.Hour})
	inner := &slotProvider{}
	p := Wrap("p", inner, sched)
	ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{RunID: "run-a", TenantID: "t", UserID: "u"})
	turn := providers.Request{Model: "m", OnEvent: func(providers.Event) {}}

	callOn(ctx, t, p, turn)
	callOn(ctx, t, p, providers.Request{Model: "m"})
	if st := laneStats(t, sched); st.Reserved != 1 {
		t.Fatalf("reserved = %d after the side call, want 1 (the run's turn reservation only)", st.Reserved)
	}
	callOn(ctx, t, p, turn)
	if want := []int{0, 1, 0}; len(inner.slots) != 3 || inner.slots[0] != want[0] || inner.slots[1] != want[1] || inner.slots[2] != want[2] {
		t.Fatalf("slots = %v, want %v (the side call off the run's reserved slot 0)", inner.slots, want)
	}
}

// With no other slot free, a run's side call must not wait behind the run's own
// reservation: the run cannot make its next turn until the side call is over,
// so it would wait out the whole hold. The reservation is given up instead.
func TestCall_SideCallDoesNotWaitBehindItsOwnRunsReservation(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1, AffinityHold: time.Hour})
	inner := &slotProvider{}
	p := Wrap("p", inner, sched)
	ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{RunID: "run-a", TenantID: "t", UserID: "u"})
	callOn(ctx, t, p, providers.Request{Model: "m", OnEvent: func(providers.Event) {}})

	side, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	callOn(side, t, p, providers.Request{Model: "m"})
	if st := laneStats(t, sched); st.Reserved != 0 || st.Active != 0 {
		t.Fatalf("after the side call: active=%d reserved=%d, want 0/0", st.Active, st.Reserved)
	}
}
