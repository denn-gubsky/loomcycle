package channels

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// Heartbeat runner emits the expected count of messages with the
// fixed-shape payload over a short observation window.
func TestHeartbeatRunner_EmitsAtCadence(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()

	pub := &StorePublisher{Store: s, Bus: NewBus()}
	specs := []HeartbeatSpec{{
		Name:       "_system/heartbeat-test",
		Period:     100 * time.Millisecond,
		DefaultTTL: 60,
	}}
	runner := NewHeartbeatRunner(pub, "v0.8.6-test", specs)
	runner.Start(context.Background())
	// 350ms = 3 expected ticks at 100ms (with some scheduler slack).
	time.Sleep(350 * time.Millisecond)
	runner.Stop()

	msgs, _, err := s.ChannelSubscribe(context.Background(),
		"_system/heartbeat-test", store.MemoryScopeGlobal, "", "cur_0", 100)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) < 2 || len(msgs) > 5 {
		t.Errorf("got %d heartbeats in 350ms at 100ms period, want 2..5", len(msgs))
	}

	// Verify payload shape on the first message.
	var p HeartbeatPayload
	if err := json.Unmarshal(msgs[0].Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.Version != "v0.8.6-test" {
		t.Errorf("payload Version = %q, want v0.8.6-test", p.Version)
	}
	if p.Timestamp == "" {
		t.Error("payload Timestamp empty")
	}
	if p.UptimeSec < 0 {
		t.Errorf("payload UptimeSec = %d, want >= 0", p.UptimeSec)
	}
	if msgs[0].PublishedByUserID != SystemPublisherUserID {
		t.Errorf("PublishedByUserID = %q, want %q", msgs[0].PublishedByUserID, SystemPublisherUserID)
	}
}

// Stop is idempotent + drains all goroutines.
func TestHeartbeatRunner_StopIdempotent(t *testing.T) {
	s, _ := sqlite.Open(":memory:")
	defer s.Close()
	pub := &StorePublisher{Store: s}
	runner := NewHeartbeatRunner(pub, "test", []HeartbeatSpec{
		{Name: "_system/test", Period: 50 * time.Millisecond},
	})
	runner.Start(context.Background())
	time.Sleep(60 * time.Millisecond)
	runner.Stop()
	runner.Stop() // should not panic / hang
}

// Empty specs is a no-op (no goroutines started, Stop returns immediately).
func TestHeartbeatRunner_NoSpecsNoOp(t *testing.T) {
	pub := &StorePublisher{}
	runner := NewHeartbeatRunner(pub, "test", nil)
	runner.Start(context.Background())
	runner.Stop()
}

// LoadHeartbeatSpecs filters channels correctly: only those with
// publisher: system AND non-empty period.
func TestLoadHeartbeatSpecs_Filters(t *testing.T) {
	specs, err := LoadHeartbeatSpecs(map[string]struct {
		Period      string
		Publisher   string
		DefaultTTL  int
		MaxMessages int
	}{
		"_system/heartbeat-1m":    {Period: "1m", Publisher: "system"},
		"_system/heartbeat-5m":    {Period: "5m", Publisher: "system", DefaultTTL: 300},
		"_system/runtime-state":   {Publisher: "system"}, // event-driven, no period — should be skipped
		"_system/alarms/critical": {Publisher: ""},       // not system — should be skipped
		"random/channel":          {Publisher: ""},
	})
	if err != nil {
		t.Fatalf("LoadHeartbeatSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}
	for _, s := range specs {
		if s.Name != "_system/heartbeat-1m" && s.Name != "_system/heartbeat-5m" {
			t.Errorf("unexpected spec %q", s.Name)
		}
	}
}

// Malformed period returns an error.
func TestLoadHeartbeatSpecs_BadPeriod(t *testing.T) {
	_, err := LoadHeartbeatSpecs(map[string]struct {
		Period      string
		Publisher   string
		DefaultTTL  int
		MaxMessages int
	}{
		"_system/bad": {Period: "not-a-duration", Publisher: "system"},
	})
	if err == nil {
		t.Fatal("expected error for malformed period")
	}
}
