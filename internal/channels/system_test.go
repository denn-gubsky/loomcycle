package channels

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func TestStorePublisher_PublishNowImmediate(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()

	bus := NewBus()
	pub := &StorePublisher{Store: s, Bus: bus}
	ctx := context.Background()

	msg, err := pub.PublishNow(ctx, "_system/test", "", store.MemoryScopeGlobal, "",
		json.RawMessage(`{"k":"v"}`), SystemPublisherUserID, 0, 0)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if msg.ID == "" {
		t.Error("publish returned empty msg ID")
	}
	if msg.PublishedByUserID != SystemPublisherUserID {
		t.Errorf("PublishedByUserID = %q, want %q", msg.PublishedByUserID, SystemPublisherUserID)
	}

	// Read back via store directly.
	msgs, _, err := s.ChannelSubscribe(ctx, "", "_system/test", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("subscribe len = %d, want 1", len(msgs))
	}
	if msgs[0].PublishedByUserID != SystemPublisherUserID {
		t.Errorf("stored PublishedByUserID = %q, want %q", msgs[0].PublishedByUserID, SystemPublisherUserID)
	}
}

func TestStorePublisher_PublishDeferredArmsScheduler(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()

	bus := NewBus()
	sched := NewScheduler(bus, 100)
	pub := &StorePublisher{Store: s, Bus: bus, Scheduler: sched}
	ctx := context.Background()

	deferTo := time.Now().Add(120 * time.Millisecond)
	msg, err := pub.Publish(ctx, "_system/test", "", store.MemoryScopeGlobal, "",
		json.RawMessage(`{}`), deferTo, "alice", 0, 0)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if msg.PublishedByUserID != "alice" {
		t.Errorf("PublishedByUserID = %q, want alice", msg.PublishedByUserID)
	}
	if sched.PendingCount() != 1 {
		t.Errorf("scheduler PendingCount = %d, want 1", sched.PendingCount())
	}

	// Wait past deferTo; subscribe should see the message.
	time.Sleep(180 * time.Millisecond)
	msgs, _, err := s.ChannelSubscribe(ctx, "", "_system/test", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("after visible_at: got %d msgs, want 1", len(msgs))
	}
}

func TestStorePublisher_NoStoreErrors(t *testing.T) {
	pub := &StorePublisher{}
	_, err := pub.PublishNow(context.Background(), "_system/test", "", store.MemoryScopeGlobal, "",
		json.RawMessage(`{}`), SystemPublisherUserID, 0, 0)
	if err == nil {
		t.Error("publish with no Store should error")
	}
}

func TestStorePublisher_DefaultTTLApplied(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()

	pub := &StorePublisher{Store: s}
	ctx := context.Background()

	msg, err := pub.PublishNow(ctx, "_system/test", "", store.MemoryScopeGlobal, "",
		json.RawMessage(`{}`), SystemPublisherUserID, 0, 60) // 60 sec TTL
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Verify the row carries an expires_at via re-read.
	msgs, _, _ := s.ChannelSubscribe(ctx, "", "_system/test", store.MemoryScopeGlobal, "", "", 10)
	if len(msgs) != 1 {
		t.Fatalf("subscribe len = %d, want 1", len(msgs))
	}
	got := msgs[0]
	if got.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt zero — expected ~60s in the future")
	}
	if got.ExpiresAt.Before(msg.PublishedAt.Add(50 * time.Second)) {
		t.Errorf("ExpiresAt too soon: %v vs published %v", got.ExpiresAt, msg.PublishedAt)
	}
}

func holdAll(context.Context, string, string) (WriteDef, error) { return WriteDef{Hold: true}, nil }

// A hold overrides a deliver_at: the message waits for a release, not a clock.
func TestWriter_HoldOverridesDeliverAt(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	w := &StorePublisher{Store: s, Scheduler: NewScheduler(NewBus(), 10), Defs: holdAll}
	res, err := w.Write(context.Background(), WriteRequest{
		Channel: "gate", Scope: store.MemoryScopeGlobal, Payload: json.RawMessage(`{}`),
		DeliverAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res.Held || res.Deferred || !store.IsChannelHeld(res.Message.VisibleAt) {
		t.Errorf("held=%v deferred=%v visible_at=%v, want held at the reserved instant", res.Held, res.Deferred, res.Message.VisibleAt)
	}
	if w.Scheduler.PendingCount() != 0 {
		t.Errorf("a held message armed a delivery timer")
	}
}

// The writer reports what max_messages trimmed, so every caller can say so.
func TestWriter_ReportsDroppedOldest(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	w := &StorePublisher{Store: s}
	var last WriteResult
	for i := 0; i < 3; i++ {
		last, err = w.Write(context.Background(), WriteRequest{
			Channel: "cap", Scope: store.MemoryScopeGlobal, Payload: json.RawMessage(`{}`), MaxMessages: 2,
		})
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if last.Dropped != 1 {
		t.Errorf("third write into a 2-message channel dropped %d, want 1", last.Dropped)
	}
}

// Only the writer decides a message is held: a caller cannot pass the
// reserved instant as a delivery time.
func TestWriter_RefusesAReservedDeliverAt(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	for _, at := range []time.Time{store.ChannelHeldVisibleAt(), store.ChannelHookHeldVisibleAt()} {
		_, err = (&StorePublisher{Store: s}).Write(context.Background(), WriteRequest{
			Channel: "c", Scope: store.MemoryScopeGlobal, Payload: json.RawMessage(`{}`),
			DeliverAt: at,
		})
		if err == nil {
			t.Errorf("a caller-supplied reserved instant %s was accepted", at.Format(time.RFC3339))
		}
	}
}

func hookedDefs(_ context.Context, _, _ string) (WriteDef, error) {
	return WriteDef{Hooked: true, Hold: true, HookTenant: "owner"}, nil
}

// A write to a channel that carries hooks waits at the hook instant — ahead
// of the channel's hold and of any deliver_at, which apply to what the hooks
// release — carrying what the worker needs: the tenant whose definition
// governs it, the publisher's deliver_at and the origin. It wakes the worker,
// not the channel's readers.
func TestWriter_AHookedChannelAwaitsItsHooks(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := NewBus()
	worker, readers := bus.Register(HookWakeKey), bus.Register("inbox")
	at := time.Now().Add(time.Hour)
	res, err := (&StorePublisher{Store: s, Bus: bus, Defs: hookedDefs, HooksEnabled: true}).Write(context.Background(), WriteRequest{
		Channel: "inbox", TenantID: "writer", Scope: store.MemoryScopeTenant, Payload: json.RawMessage(`{}`),
		DeliverAt: at, Origin: OriginStarterSink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.AwaitingHooks || res.Held || res.Deferred || !store.IsChannelHookHeld(res.Message.VisibleAt) {
		t.Fatalf("result = %+v", res)
	}
	items, err := s.ChannelHookClaim(context.Background(), "w", time.Now(), time.Now().Add(time.Minute), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %d (err %v)", len(items), err)
	}
	m := items[0].Message
	if m.HookTenant != "owner" || !m.RequestedVisibleAt.Equal(at) || m.Origin != OriginStarterSink || m.TenantID != "writer" {
		t.Fatalf("stored %+v", m)
	}
	select {
	case <-worker:
	default:
		t.Error("the worker was not woken")
	}
	select {
	case <-readers:
		t.Error("the channel's readers were woken for a message they cannot see")
	default:
	}
}

// With channel hooks off, a write to a channel that carries them is refused:
// nothing would decide it, and delivering it would open the gate.
func TestWriter_HookedChannelRefusedWhenDisabled(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = (&StorePublisher{Store: s, Defs: hookedDefs}).Write(context.Background(), WriteRequest{
		Channel: "inbox", TenantID: "t", Scope: store.MemoryScopeGlobal, Payload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrChannelHooksDisabled) {
		t.Fatalf("err = %v, want ErrChannelHooksDisabled", err)
	}
	if snap, _ := s.SnapshotReadChannelMessages(context.Background()); len(snap) != 0 {
		t.Fatalf("stored %d message(s) nothing would decide", len(snap))
	}
}
