package lanegate

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A run's own turn call (the one carrying OnEvent) that has to wait for its
// lane lends the run's global slot while it waits — another run can start —
// and takes it back before it computes. A call that waits for nothing lends
// nothing, and neither does a call without OnEvent: a tool's or a summary's
// call can run beside work that is computing.
func TestCall_ATurnCallWaitingForItsLaneLendsTheRunsSlot(t *testing.T) {
	sem := concurrency.New(1, 8, 0)
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})

	// Another call holds the lane's only slot.
	holder := &fakeProvider{next: make(chan providers.Event)}
	hold, err := Wrap("p", holder, sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	// The run, holding the runtime's only slot, makes its turn call.
	rs := &concurrency.RunSlot{}
	endRun, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "alice", Slot: rs})
	if err != nil {
		t.Fatal(err)
	}
	defer endRun()
	ctx := concurrency.WithRunSlot(context.Background(), rs)
	turn := &fakeProvider{next: make(chan providers.Event)}
	type result struct {
		ch  <-chan providers.Event
		err error
	}
	got := make(chan result, 1)
	go func() {
		ch, err := Wrap("p", turn, sched).Call(ctx, providers.Request{Model: "m", OnEvent: func(providers.Event) {}})
		got <- result{ch, err}
	}()
	waitLane(t, sched, "the turn call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })

	// Lent: another run starts in the runtime's only slot.
	other, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "bob", Reject: true})
	if err != nil {
		t.Fatalf("no run could start while the turn call waited: %v", err)
	}
	if !rs.Lent() {
		t.Fatal("the slot is not lent while the turn call waits")
	}

	// The lane frees: the turn call has its lane slot but no global one until
	// bob's run ends, so the driver is not called yet.
	holder.next <- done()
	close(holder.next)
	for range hold {
	}
	time.Sleep(20 * time.Millisecond)
	if turn.calls.Load() != 0 {
		t.Fatal("the turn call computed without a global slot")
	}
	// Waiting for a global slot, it holds no lane slot: it gave the one it was
	// granted back rather than wait on it.
	if st := laneStats(t, sched); st.Active != 0 {
		t.Fatalf("lane active = %d while the turn call waits for a global slot, want 0", st.Active)
	}
	other()
	r := <-got
	if r.err != nil {
		t.Fatalf("turn call: %v", r.err)
	}
	if rs.Lent() || turn.calls.Load() != 1 {
		t.Fatalf("after bob ended: lent=%v calls=%d, want the slot back and the call made", rs.Lent(), turn.calls.Load())
	}
	if st := sem.Stats(); st.Active != 1 {
		t.Errorf("runtime active = %d, want alice's one", st.Active)
	}
	close(turn.next)
	for range r.ch {
	}
}

// Only the run's turn call lends: a call without OnEvent waits with the run's
// slot held, and so does a call made under a ctx carrying no slot (a
// sub-agent's, which runSubAgent clears).
func TestCall_OnlyTheTurnCallLends(t *testing.T) {
	for _, c := range []struct {
		name    string
		onEvent bool
		slot    bool
	}{{"no OnEvent", false, true}, {"no slot on ctx", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			sem := concurrency.New(1, 8, 0)
			sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
			holder := &fakeProvider{next: make(chan providers.Event)}
			hold, err := Wrap("p", holder, sched).Call(context.Background(), providers.Request{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			rs := &concurrency.RunSlot{}
			endRun, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "alice", Slot: rs})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if c.slot {
				ctx = concurrency.WithRunSlot(ctx, rs)
			}
			req := providers.Request{Model: "m"}
			if c.onEvent {
				req.OnEvent = func(providers.Event) {}
			}
			call := &fakeProvider{next: make(chan providers.Event)}
			done2 := make(chan struct{})
			go func() {
				defer close(done2)
				if ch, err := Wrap("p", call, sched).Call(ctx, req); err == nil {
					close(call.next)
					for range ch {
					}
				}
			}()
			waitLane(t, sched, "the call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
			if rs.Lent() {
				t.Error("the slot was lent")
			}
			if _, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "bob", Reject: true}); err == nil {
				t.Error("another run started in the slot the waiting call's run holds")
			}
			holder.next <- done()
			close(holder.next)
			for range hold {
			}
			<-done2
			endRun()
		})
	}
}

// The lock order that must hold: a run never waits for a global slot while it
// holds a lane slot. Here the run that took alice's lent slot (bob) waits for
// the same one-slot lane with a call that does not lend, holding the only
// global slot. When the lane frees, alice's turn call is granted it with no
// global slot to take back: holding the lane slot while waiting would deadlock
// with bob. It gives the lane slot back instead, bob computes and ends, and
// alice goes on.
func TestCall_ALendingRunNeverWaitsForAGlobalSlotHoldingTheLane(t *testing.T) {
	sem := concurrency.New(1, 8, 0)
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	holder := &fakeProvider{next: make(chan providers.Event)}
	hold, err := Wrap("p", holder, sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	alice := &concurrency.RunSlot{}
	endAlice, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "alice", Slot: alice})
	if err != nil {
		t.Fatal(err)
	}
	defer endAlice()
	aliceCall := &fakeProvider{next: make(chan providers.Event)}
	aliceDone := make(chan error, 1)
	go func() {
		ch, err := Wrap("p", aliceCall, sched).Call(concurrency.WithRunSlot(context.Background(), alice),
			providers.Request{Model: "m", OnEvent: func(providers.Event) {}})
		if err == nil {
			close(aliceCall.next)
			for range ch {
			}
		}
		aliceDone <- err
	}()
	waitLane(t, sched, "alice queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })

	// bob takes alice's lent slot and waits for the lane with a tool's call.
	endBob, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "bob", Reject: true})
	if err != nil {
		t.Fatalf("bob could not start in alice's lent slot: %v", err)
	}
	bobCall := &fakeProvider{next: make(chan providers.Event)}
	bobDone := make(chan error, 1)
	go func() {
		ch, err := Wrap("p", bobCall, sched).Call(context.Background(), providers.Request{Model: "m"})
		if err == nil {
			close(bobCall.next)
			for range ch {
			}
		}
		endBob()
		bobDone <- err
	}()
	waitLane(t, sched, "bob queued behind alice", func(s concurrency.LaneStats) bool { return s.Queued == 2 })

	holder.next <- done()
	close(holder.next)
	for range hold {
	}
	for name, ch := range map[string]chan error{"bob": bobDone, "alice": aliceDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s's call: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s's call never finished — a run waited for a global slot holding the lane slot the other run waited for", name)
		}
	}
	if aliceCall.calls.Load() != 1 || bobCall.calls.Load() != 1 {
		t.Errorf("calls made: alice %d, bob %d; want one each", aliceCall.calls.Load(), bobCall.calls.Load())
	}
}

// giveBackScenario is the order TestCall_ALendingRunNeverWaitsForAGlobalSlotHoldingTheLane
// sets up, for a run that carries its identity on ctx as the loop's runs do:
// alice's turn call waits for the one-slot lane and lends the only global slot,
// bob takes that slot and waits for the lane with a tool's call, and the lane
// then frees. alice is granted the lane slot with no global slot to take back,
// so she gives the lane slot back and waits for bob's.
type giveBackScenario struct {
	sched              *concurrency.CallScheduler
	aliceCall, bobCall *fakeProvider
	aliceLog           eventLog
	aliceDone, bobDone chan error
}

func startGiveBack(t *testing.T, lane concurrency.LaneConfig) *giveBackScenario {
	t.Helper()
	g := &giveBackScenario{
		sched:     chatLane(lane),
		aliceCall: &fakeProvider{next: make(chan providers.Event)},
		bobCall:   &fakeProvider{next: make(chan providers.Event)},
		aliceDone: make(chan error, 1),
		bobDone:   make(chan error, 1),
	}
	sem := concurrency.New(1, 8, 0)
	holder := &fakeProvider{next: make(chan providers.Event)}
	hold, err := Wrap("p", holder, g.sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	alice := &concurrency.RunSlot{}
	endAlice, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "alice", Slot: alice})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endAlice)
	// Cleanups run last-registered first: a failed test ends both calls' waits
	// before alice's run ends, which waits for a Reclaim in progress.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	aliceCtx := providers.WithRunMeta(concurrency.WithRunSlot(ctx, alice),
		providers.RunMeta{RunID: "alice", UserID: "alice"})
	go func() {
		ch, err := Wrap("p", g.aliceCall, g.sched).Call(aliceCtx,
			providers.Request{Model: "m", OnEvent: g.aliceLog.on})
		if err == nil {
			close(g.aliceCall.next)
			for range ch {
			}
		}
		g.aliceDone <- err
	}()
	waitLane(t, g.sched, "alice queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })

	endBob, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "bob", Reject: true})
	if err != nil {
		t.Fatalf("bob could not start in alice's lent slot: %v", err)
	}
	bobCtx := providers.WithRunMeta(ctx, providers.RunMeta{RunID: "bob", UserID: "bob"})
	go func() {
		ch, err := Wrap("p", g.bobCall, g.sched).Call(bobCtx, providers.Request{Model: "m"})
		if err == nil {
			for range ch {
			}
		}
		// bob's run ends, as a run's end does: its reservation goes, then its
		// global slot.
		g.sched.Done("bob")
		endBob()
		g.bobDone <- err
	}()
	waitLane(t, g.sched, "bob queued behind alice", func(s concurrency.LaneStats) bool { return s.Queued == 2 })

	holder.next <- done()
	close(holder.next)
	for range hold {
	}
	return g
}

func eventTypes(evs []providers.Event) []providers.EventType {
	out := make([]providers.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

// finish ends bob's call, which lets alice take his global slot and make hers.
func (g *giveBackScenario) finish(t *testing.T) {
	t.Helper()
	close(g.bobCall.next)
	for name, ch := range map[string]chan error{"bob": g.bobDone, "alice": g.aliceDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s's call: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s's call never finished", name)
		}
	}
}

// The lane slot a lending run gives back was never used: it goes to the next
// waiter at once instead of being reserved for the lending run for its
// affinity hold, and the lane does not count it as a grant. Kept for alice, it
// would hold bob — the run whose global slot alice is waiting for — out of the
// lane for the whole hold.
func TestCall_AGivenBackLaneSlotGoesToTheNextWaiterUnreserved(t *testing.T) {
	g := startGiveBack(t, concurrency.LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	waitCalls(t, "bob's call once alice gave the lane slot back", g.bobCall.calls.Load, 1)
	if g.aliceCall.calls.Load() != 0 {
		t.Fatal("alice's call computed without a global slot")
	}
	g.finish(t)
	if g.aliceCall.calls.Load() != 1 || g.bobCall.calls.Load() != 1 {
		t.Fatalf("calls made: alice %d, bob %d; want one each", g.aliceCall.calls.Load(), g.bobCall.calls.Load())
	}
	// The holder's, bob's and alice's real one: the slot alice gave back was
	// never a call.
	if st := laneStats(t, g.sched); st.Grants != 3 {
		t.Fatalf("lane grants = %d, want 3 (the given-back slot is not a grant)", st.Grants)
	}
}

// A call that gives its lane slot back to wait for a global slot is not
// admitted yet: it reports call_admitted only once it holds both, so a client
// sees one wait from call_queued to call_admitted covering both.
func TestCall_AGivenBackCallReportsItsAdmissionOnlyOnceItHoldsBothSlots(t *testing.T) {
	g := startGiveBack(t, concurrency.LaneConfig{MaxConcurrent: 1})
	waitCalls(t, "bob's call once alice gave the lane slot back", g.bobCall.calls.Load, 1)
	// bob computes in the global slot alice waits for.
	if evs := g.aliceLog.list(); len(evs) != 1 || evs[0].Type != providers.EventCallQueued {
		t.Fatalf("while alice waits for a global slot she reported %v, want only call_queued", eventTypes(evs))
	}
	g.finish(t)
	evs := g.aliceLog.list()
	if len(evs) != 2 || evs[0].Type != providers.EventCallQueued || evs[1].Type != providers.EventCallAdmitted {
		t.Fatalf("alice reported %v, want call_queued then call_admitted", eventTypes(evs))
	}
	if a := evs[1].Admission; a == nil || a.Provider != "p" || a.Kind != config.ModelKindChat {
		t.Fatalf("call_admitted payload = %+v, want provider p, kind chat", a)
	}
}
