package lanegate

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestCall_CarriesItsLaneWaitOnTheDoneUsage — a call that waited for its lane
// slot reports the wait on its done event's usage, so the caller's call timer
// (started before Provider.Call) counts it as queue time, not as the model's
// work. The driver's own usage is not written to, and a call that waited for
// nothing carries no wait.
func TestCall_CarriesItsLaneWaitOnTheDoneUsage(t *testing.T) {
	const hold = 100 * time.Millisecond
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	holder := &fakeProvider{next: make(chan providers.Event, 1)}
	hold1, err := Wrap("p", holder, sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	waiter := &fakeProvider{next: make(chan providers.Event, 1)}
	got := make(chan (<-chan providers.Event), 1)
	go func() {
		ch, err := Wrap("p", waiter, sched).Call(context.Background(), providers.Request{Model: "m"})
		if err != nil {
			t.Errorf("waiting call: %v", err)
			close(got)
			return
		}
		got <- ch
	}()
	waitLane(t, sched, "the second call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	time.Sleep(hold)

	holderDone := done()
	holder.next <- holderDone
	close(holder.next)
	for ev := range hold1 {
		if ev.Type == providers.EventDone && (ev.Usage != holderDone.Usage || ev.Usage.LaneWaitMs != 0) {
			t.Errorf("a call that waited for nothing: usage %p (driver's %p), lane wait %d ms; want the driver's usage untouched",
				ev.Usage, holderDone.Usage, ev.Usage.LaneWaitMs)
		}
	}

	ch, ok := <-got
	if !ok {
		t.FailNow()
	}
	waiterDone := done()
	waiter.next <- waiterDone
	close(waiter.next)
	var wait int64 = -1
	for ev := range ch {
		if ev.Type == providers.EventDone {
			wait = ev.Usage.LaneWaitMs
		}
	}
	if wait < hold.Milliseconds() {
		t.Fatalf("lane wait on the done usage = %d ms, want at least the %v the call waited", wait, hold)
	}
	if waiterDone.Usage.LaneWaitMs != 0 {
		t.Error("the driver's own usage was written to")
	}
}

// TestCall_CountsItsRunSlotWaitInItsLaneWait — a turn call granted its lane slot
// with no global slot to take back gives the lane slot back and waits for a
// global one first. That wait is still not the model's work: it is counted in
// the call's wait like the lane's.
func TestCall_CountsItsRunSlotWaitInItsLaneWait(t *testing.T) {
	const hold = 100 * time.Millisecond
	sem := concurrency.New(1, 8, 0)
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	holder := &fakeProvider{next: make(chan providers.Event, 1)}
	hold1, err := Wrap("p", holder, sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	rs := &concurrency.RunSlot{}
	endRun, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "alice", Slot: rs})
	if err != nil {
		t.Fatal(err)
	}
	defer endRun()
	turn := &fakeProvider{next: make(chan providers.Event, 1)}
	got := make(chan (<-chan providers.Event), 1)
	go func() {
		ch, err := Wrap("p", turn, sched).Call(concurrency.WithRunSlot(context.Background(), rs),
			providers.Request{Model: "m", OnEvent: func(providers.Event) {}})
		if err != nil {
			t.Errorf("turn call: %v", err)
			close(got)
			return
		}
		got <- ch
	}()
	waitLane(t, sched, "the turn call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	other, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "bob", Reject: true})
	if err != nil {
		t.Fatalf("no run could start in the lent slot: %v", err)
	}

	// The lane frees at once; the global slot only after hold.
	holder.next <- done()
	close(holder.next)
	for range hold1 {
	}
	time.Sleep(hold)
	other()

	ch, ok := <-got
	if !ok {
		t.FailNow()
	}
	turn.next <- done()
	close(turn.next)
	var wait int64 = -1
	for ev := range ch {
		if ev.Type == providers.EventDone {
			wait = ev.Usage.LaneWaitMs
		}
	}
	if wait < hold.Milliseconds() {
		t.Fatalf("wait on the done usage = %d ms, want at least the %v spent taking the run slot back", wait, hold)
	}
}
