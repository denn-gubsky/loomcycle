package concurrency

import (
	"context"
	"testing"
	"time"
)

// TestSemaphore_SetCaps: RFC CK live concurrency reload — raising the cap admits
// runs that were backpressured at the old cap; both are read under the mutex.
func TestSemaphore_SetCaps(t *testing.T) {
	s := New(1, 0, 50*time.Millisecond) // cap 1, no queue
	rel1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := s.Acquire(context.Background()); !IsBackpressure(err) {
		t.Fatalf("at cap 1 + no queue, want backpressure, got %v", err)
	}
	s.SetCaps(3, 0) // raise to 3
	rel2, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("after SetCaps(3): %v", err)
	}
	rel3, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("third slot after SetCaps(3): %v", err)
	}
	rel1()
	rel2()
	rel3()
}

// TestCallScheduler_GatedFollowsReload — Gated is what lets a caller skip all
// bookkeeping for an uncapped call, so it must be false for a provider nobody
// capped, true for a lane and for a models-in-flight cap with no lane, and
// follow a Reload both ways. LaneCap reports the lane's cap.
func TestCallScheduler_GatedFollowsReload(t *testing.T) {
	var nilSched *CallScheduler
	if nilSched.Gated("p", "chat") || nilSched.LaneCap("p", "chat") != 0 {
		t.Fatal("a nil scheduler gates nothing")
	}
	s := NewCallScheduler(map[string]ProviderLanesConfig{
		"laned":  {Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 3}}},
		"models": {MaxModelsInFlight: 1},
	})
	if !s.Gated("laned", "chat") || s.LaneCap("laned", "chat") != 3 {
		t.Errorf("laned/chat: gated=%v cap=%d, want true/3", s.Gated("laned", "chat"), s.LaneCap("laned", "chat"))
	}
	if s.Gated("laned", "decision") {
		t.Error("a kind with no lane on a provider with no models cap is not gated")
	}
	if !s.Gated("models", "chat") || s.LaneCap("models", "chat") != 0 {
		t.Error("a models-in-flight cap gates every kind, with no lane cap")
	}
	if s.Gated("other", "chat") {
		t.Error("an uncapped provider is not gated")
	}
	s.Reload(map[string]ProviderLanesConfig{"other": {Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 1}}}})
	if s.Gated("laned", "chat") || s.Gated("models", "chat") || !s.Gated("other", "chat") {
		t.Errorf("after reload: laned=%v models=%v other=%v, want false/false/true",
			s.Gated("laned", "chat"), s.Gated("models", "chat"), s.Gated("other", "chat"))
	}
}

// TestCallScheduler_ReloadRace: Acquire/Gated/Stats against Reload — under
// -race this fails if the provider map swap is not synchronized.
func TestCallScheduler_ReloadRace(t *testing.T) {
	cfg := func(n int) map[string]ProviderLanesConfig {
		return map[string]ProviderLanesConfig{"a": {Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: n}}}}
	}
	s := NewCallScheduler(cfg(5))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			if rel, err := s.Acquire(context.Background(), "a", "chat", CallInfo{}); err == nil {
				rel()
			}
			s.Gated("a", "chat")
			s.Stats()
		}
	}()
	for i := 0; i < 2000; i++ {
		s.Reload(cfg(1 + i%5))
	}
	<-done
}
