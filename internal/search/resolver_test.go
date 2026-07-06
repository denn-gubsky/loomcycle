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
