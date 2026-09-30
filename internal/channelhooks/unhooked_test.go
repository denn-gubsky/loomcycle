package channelhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Messages written while channel hooks were on wait at the hook instant. A
// replica with hooks off (a restart that turned them off, another replica of
// a cluster) runs RunUnhooked, which delivers them as a write with hooks off
// is delivered — the only thing that ever moves them there.

// hooksOff turns the fixture's writer to hooks off, as a restart with
// LOOMCYCLE_CHANNEL_HOOKS unset would.
func (f *fixture) hooksOff() { f.writer.HooksEnabled = false }

// awaiting reports how many messages still wait for hooks, claimable or not.
func (f *fixture) awaiting() int {
	f.t.Helper()
	snap, err := f.st.SnapshotReadChannelMessages(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, m := range snap {
		if store.IsChannelHookHeld(m.VisibleAt) {
			n++
		}
	}
	return n
}

// A message written with hooks on is delivered, as written, once the replica
// runs with them off — and no hook runs on it.
func TestWorkerUnhooked_DeliversMessagesLeftAwaitingHooks(t *testing.T) {
	f := newFixture(t)
	srv, calls := answer(t, `{"decision":"drop","reason":"a hook ran"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	if res := f.publish("inbox", "", store.MemoryScopeGlobal, `{"text":"hi"}`, nil); !res.AwaitingHooks {
		t.Fatalf("published with hooks on: %+v", res)
	}
	f.hooksOff()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.w.RunUnhooked(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.peek("inbox", "", store.MemoryScopeGlobal)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	msgs := f.peek("inbox", "", store.MemoryScopeGlobal)
	if len(msgs) != 1 || string(msgs[0].Payload) != `{"text":"hi"}` {
		t.Fatalf("delivered %+v, want the message as written", msgs)
	}
	if calls.Load() != 0 {
		t.Fatalf("a hook ran %d time(s) with hooks off", calls.Load())
	}
	if n := f.awaiting(); n != 0 {
		t.Fatalf("%d message(s) still await hooks", n)
	}
}

// A channel that holds takes the message into its hold, as a write with hooks
// off would: releasable by an operator, not delivered past the hold.
func TestWorkerUnhooked_HeldChannelTakesItIntoTheHold(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hold: true, Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
	f.hooksOff()
	f.w.deliverUnhooked(context.Background())
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered past the channel's hold: %+v", msgs)
	}
	released, _, err := f.st.ChannelRelease(context.Background(), "", "inbox", store.MemoryScopeGlobal, "", 10)
	if err != nil || len(released) != 1 {
		t.Fatalf("operator release: %v (err %v), want the message held", released, err)
	}
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 || string(msgs[0].Payload) != `{"n":1}` {
		t.Fatalf("after the release: %+v", msgs)
	}
}

// A deliver_at still applies: the message lands no earlier.
func TestWorkerUnhooked_KeepsTheDeliverAt(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	at := time.Now().Add(time.Hour)
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, func(r *channels.WriteRequest) { r.DeliverAt = at })
	f.hooksOff()
	f.w.deliverUnhooked(context.Background())
	snap, _ := f.st.SnapshotReadChannelMessages(context.Background())
	var got []store.ChannelMessage
	for _, m := range snap {
		if m.Channel == "inbox" {
			got = append(got, m)
		}
	}
	if len(got) != 1 || got[0].VisibleAt.Before(at.Add(-time.Second)) || store.IsChannelReservedVisibleAt(got[0].VisibleAt) {
		t.Fatalf("released %+v, want it at %v", got, at)
	}
}

// A message a live lease holds — a replica with hooks on, deciding it — is
// left to that replica; once its lease runs out, it is delivered here.
func TestWorkerUnhooked_LeavesALiveLeaseAndTakesAnExpiredOne(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
	f.hooksOff()
	ctx := context.Background()
	now := time.Now()
	other, err := f.st.ChannelHookClaim(ctx, "replica-with-hooks-on", now, now.Add(time.Hour), 10)
	if err != nil || len(other) != 1 {
		t.Fatalf("the other replica's claim: %d (err %v)", len(other), err)
	}
	if n := f.w.deliverUnhooked(ctx); n != 0 {
		t.Fatalf("took %d message(s) under another replica's live lease", n)
	}
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered under another replica's live lease: %+v", msgs)
	}
	// Its lease runs out (the replica died): the message is this one's now.
	if ok, err := f.st.ChannelHookSaveProgress(ctx, keyOf(other[0].Message), other[0].Lease, store.ChannelHookProgress{}, time.Now().Add(-time.Second)); err != nil || !ok {
		t.Fatalf("expire the other lease: ok=%v err=%v", ok, err)
	}
	if n := f.w.deliverUnhooked(ctx); n != 1 {
		t.Fatalf("took %d message(s) after the other lease ran out, want 1", n)
	}
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 {
		t.Fatalf("delivered %d after the other lease ran out, want 1", len(msgs))
	}
	if ok, _ := f.st.ChannelReleaseHookHeld(ctx, keyOf(other[0].Message), other[0].Lease, nil, time.Time{}); ok {
		t.Fatal("the lapsed lease settled the message a second time")
	}
}

// What hooks already made of the body — a redaction — is what is delivered;
// turning hooks off must not undo it.
func TestWorkerUnhooked_KeepsTheBodyHooksRewrote(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("redact", srv.URL), webhookEntry("audit", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"text":"secret"}`, nil)
	ctx := context.Background()
	now := time.Now()
	w, _ := f.st.ChannelHookClaim(ctx, "dead-replica", now, now.Add(time.Hour), 1)
	if len(w) != 1 {
		t.Fatalf("claim: %d", len(w))
	}
	// The first hook redacted it; the replica died before the second.
	p := store.ChannelHookProgress{ChainPos: 1, Body: []byte(`{"text":"[redacted]"}`)}
	if ok, err := f.st.ChannelHookSaveProgress(ctx, keyOf(w[0].Message), w[0].Lease, p, time.Now().Add(-time.Second)); err != nil || !ok {
		t.Fatalf("save progress: ok=%v err=%v", ok, err)
	}
	f.hooksOff()
	f.w.deliverUnhooked(ctx)
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 || string(msgs[0].Payload) != `{"text":"[redacted]"}` {
		t.Fatalf("delivered %+v, want the redacted body", msgs)
	}
}

// A channel declared nowhere any more is delivered to at once, as a write to
// it is.
func TestWorkerUnhooked_UndeclaredChannelIsDelivered(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	f.mu.Lock()
	delete(f.defs, "|inbox")
	f.mu.Unlock()
	f.hooksOff()
	f.w.deliverUnhooked(context.Background())
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 {
		t.Fatalf("delivered %d, want 1", len(msgs))
	}
}

// A definition that cannot be read may hold: the message is not delivered
// past it, and is tried again.
func TestWorkerUnhooked_UnreadableDefinitionDeliversNothing(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hold: true, Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	f.hooksOff()
	f.w.cfg.Defs = func(context.Context, string, string) (Def, bool, error) {
		return Def{}, false, errors.New("store down")
	}
	f.w.deliverUnhooked(context.Background())
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered past a definition that could not be read: %+v", msgs)
	}
	if n := f.awaiting(); n != 1 {
		t.Fatalf("%d message(s) await hooks, want the one to be tried again", n)
	}
}

// An ask the message's hooks left pending is cancelled — nobody waits for its
// answer — and the hook run it was filed under ends.
func TestWorkerUnhooked_CancelsAPendingAskAndEndsItsRun(t *testing.T) {
	f, runs, _ := askFixture(t, time.Hour)
	hold, calls := answer(t, `{"decision":"hold","reason":"needs a look"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)

	// What a replica killed mid-ask leaves (hooks on): the message's hook run,
	// a pending ask under it, and a lease that has run out.
	ctx := context.Background()
	_, deadRun, err := runs.Open(ctx, "", "hook:gate", "")
	if err != nil {
		t.Fatal(err)
	}
	stale := store.InterruptRow{InterruptID: store.MintInterruptID(time.Now()), RunID: deadRun, Kind: store.InterruptKindQuestion,
		Status: store.InterruptStatusPending, Question: "deliver?", CreatedAt: time.Now(), UserID: "_system"}
	if _, err := f.st.InterruptCreate(ctx, stale); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w, _ := f.st.ChannelHookClaim(ctx, "dead-replica", now, now.Add(time.Hour), 1)
	if len(w) != 1 {
		t.Fatalf("claim: %d", len(w))
	}
	if ok, err := f.st.ChannelHookSaveProgress(ctx, keyOf(w[0].Message), w[0].Lease, store.ChannelHookProgress{RunID: deadRun}, time.Now().Add(-time.Second)); err != nil || !ok {
		t.Fatalf("save progress: ok=%v err=%v", ok, err)
	}

	f.hooksOff()
	if n := f.w.deliverUnhooked(ctx); n != 1 {
		t.Fatalf("took %d, want 1", n)
	}
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 {
		t.Fatalf("delivered %d, want 1", len(msgs))
	}
	if row, err := f.st.InterruptGet(ctx, stale.InterruptID); err != nil || row.Status != store.InterruptStatusCancelled {
		t.Fatalf("the pending ask is %s by %s (err %v), want it cancelled", row.Status, row.ResolvedBy, err)
	}
	if _, finished := runs.snapshot(); finished[deadRun] == "" {
		t.Fatalf("the hook run %s did not end: %v", deadRun, finished)
	}
	if calls.Load() != 0 {
		t.Fatalf("the hold hook ran %d time(s) with hooks off", calls.Load())
	}
}
