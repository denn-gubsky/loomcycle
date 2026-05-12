package channels

import (
	"context"
	"encoding/json"
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

	msg, err := pub.PublishNow(ctx, "_system/test", store.MemoryScopeGlobal, "",
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
	msgs, _, err := s.ChannelSubscribe(ctx, "_system/test", store.MemoryScopeGlobal, "", "", 10)
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
	msg, err := pub.Publish(ctx, "_system/test", store.MemoryScopeGlobal, "",
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
	msgs, _, err := s.ChannelSubscribe(ctx, "_system/test", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("after visible_at: got %d msgs, want 1", len(msgs))
	}
}

func TestStorePublisher_NoStoreErrors(t *testing.T) {
	pub := &StorePublisher{}
	_, err := pub.PublishNow(context.Background(), "_system/test", store.MemoryScopeGlobal, "",
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

	msg, err := pub.PublishNow(ctx, "_system/test", store.MemoryScopeGlobal, "",
		json.RawMessage(`{}`), SystemPublisherUserID, 0, 60) // 60 sec TTL
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Verify the row carries an expires_at via re-read.
	msgs, _, _ := s.ChannelSubscribe(ctx, "_system/test", store.MemoryScopeGlobal, "", "", 10)
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
