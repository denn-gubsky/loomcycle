package concurrency

import (
	"testing"
	"time"
)

// EstimatedWait reads the lane and the model gate as a new call would meet
// them: nothing while a slot is free, the calls ahead shared over the slots
// once it is full, and one more call when the gate is at its cap without the
// model.
func TestCallScheduler_EstimatedWaitReadsTheLaneAndTheModelGate(t *testing.T) {
	const call = 10 * time.Second
	s := NewCallScheduler(map[string]ProviderLanesConfig{"p": {
		Lanes:             map[string]LaneConfig{"chat": {MaxConcurrent: 2}},
		MaxModelsInFlight: 1,
	}})
	if w := s.EstimatedWait("p", "chat", "m", call); w != 0 {
		t.Fatalf("an idle lane: wait %s, want 0", w)
	}
	rel1 := schedNow(t, s, "p", "chat", CallInfo{Model: "m@65536"})
	if w := s.EstimatedWait("p", "chat", "m", call); w != 0 {
		t.Fatalf("one of two slots taken: wait %s, want 0", w)
	}
	rel2 := schedNow(t, s, "p", "chat", CallInfo{Model: "m@65536"})
	// Full, nobody queued: this call is first in line, half a call on two slots.
	if w := s.EstimatedWait("p", "chat", "m", call); w != 5*time.Second {
		t.Fatalf("a full lane, none queued: wait %s, want 5s", w)
	}
	q := schedAsync(t, s, "p", "chat", CallInfo{Model: "m@65536"})
	eventually(t, "a call queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 1 })
	if w := s.EstimatedWait("p", "chat", "m", call); w != 10*time.Second {
		t.Fatalf("a full lane, one queued: wait %s, want 10s", w)
	}
	// Another model also waits for "m" to drain at the cap of one model.
	if w := s.EstimatedWait("p", "chat", "other", call); w != 20*time.Second {
		t.Fatalf("another model at the model cap: wait %s, want 20s (the lane plus one call)", w)
	}
	// No measured call time: no estimate.
	if w := s.EstimatedWait("p", "chat", "m", 0); w != 0 {
		t.Fatalf("unknown call time: wait %s, want 0", w)
	}
	// A provider nothing caps.
	if w := s.EstimatedWait("uncapped", "chat", "m", call); w != 0 {
		t.Fatalf("an uncapped provider: wait %s, want 0", w)
	}
	rel1()
	mustGrant(t, q, "the queued call")()
	rel2()
}
