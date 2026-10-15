package lanegate

import (
	"context"
	"testing"

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
		ch, err := p.Call(ctx, providers.Request{Model: "m"})
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
