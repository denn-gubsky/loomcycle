package search

import (
	"errors"
	"testing"
)

func TestResolver_Cascade(t *testing.T) {
	r := NewResolver([]string{"brave", "serper", "exa"})
	// No per-agent list → global default order.
	if got := r.Cascade(nil); !equal(got, []string{"brave", "serper", "exa"}) {
		t.Errorf("Cascade(nil) = %v, want global order", got)
	}
	// Per-agent list is a full override, in the agent's order.
	if got := r.Cascade([]string{"serper", "brave"}); !equal(got, []string{"serper", "brave"}) {
		t.Errorf("Cascade(agent) = %v, want the agent override", got)
	}
	// Cascade returns a copy — mutating it must not corrupt the resolver.
	c := r.Cascade(nil)
	c[0] = "TAMPERED"
	if r.Cascade(nil)[0] != "brave" {
		t.Error("Cascade must return a defensive copy")
	}
}

func TestResolver_MarkOutcomeAvailability(t *testing.T) {
	r := NewResolver([]string{"brave"})
	if !r.Available("brave") {
		t.Fatal("fresh provider should be available")
	}
	// A failure opens a cooldown → unavailable.
	r.MarkOutcome("brave", errors.New("boom"))
	if r.Available("brave") {
		t.Error("provider should be in cooldown after a failure")
	}
	// A success clears it → available again.
	r.MarkOutcome("brave", nil)
	if !r.Available("brave") {
		t.Error("provider should be available after a success clears the cooldown")
	}
	// A zero cooldown expires immediately (proves it's time-gated, not a hard flag).
	r.cooldown = 0
	r.MarkOutcome("brave", errors.New("boom"))
	if !r.Available("brave") {
		t.Error("a zero-cooldown failure should not linger")
	}
}

func TestResolver_Snapshot(t *testing.T) {
	r := NewResolver([]string{"brave", "serper"})
	// Fresh: everything reachable, no error.
	snap := r.Snapshot()
	if len(snap) != 2 || !snap["brave"].Reachable || snap["brave"].LastError != "" {
		t.Fatalf("fresh snapshot = %+v", snap)
	}
	// A failure records reachable=false + the last error text.
	r.MarkOutcome("serper", errors.New("429 rate limited"))
	snap = r.Snapshot()
	if snap["serper"].Reachable {
		t.Error("serper should be unreachable after a failure")
	}
	if snap["serper"].LastError != "429 rate limited" {
		t.Errorf("serper LastError = %q, want the failure text", snap["serper"].LastError)
	}
	if snap["serper"].StalledUntil.IsZero() {
		t.Error("serper StalledUntil should be set")
	}
	if !snap["brave"].Reachable {
		t.Error("brave should stay reachable")
	}
	// Snapshot only covers providers in the priority order.
	if _, ok := r.Snapshot()["exa"]; ok {
		t.Error("Snapshot should not include a provider outside the priority")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
