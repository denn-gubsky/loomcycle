package resolve

import "testing"

// TestReloadPolicy is the RFC CK config reload: ReloadPolicy replaces the
// library tier candidates + provider priority in place (the next resolution
// sees them) WITHOUT disturbing the availability matrix (a prior probe result
// survives) or the forceProbe wiring.
func TestReloadPolicy(t *testing.T) {
	r := NewResolver([]string{"p1"}, map[string][]Candidate{"low": {{Provider: "p1", Model: "m1"}}})

	// A probe result on p1 before the reload must survive it.
	r.SetReachable("p1", true, []string{"m1"}, "")

	// Before: library policy resolves to p1/m1.
	if cands := r.candidatesFor(AgentRequest{Tier: "low"}); len(cands) != 1 || cands[0].Model != "m1" {
		t.Fatalf("pre-reload candidatesFor = %+v, want [{p1 m1}]", cands)
	}
	if order, _ := r.priorityFor(AgentRequest{}); len(order) != 1 || order[0] != "p1" {
		t.Fatalf("pre-reload priorityFor = %v, want [p1]", order)
	}

	// Reload the policy: new tier candidate + new priority.
	r.ReloadPolicy([]string{"p2"}, map[string][]Candidate{"low": {{Provider: "p2", Model: "m2"}}})

	// After: the next resolution sees the new policy.
	if cands := r.candidatesFor(AgentRequest{Tier: "low"}); len(cands) != 1 || cands[0].Model != "m2" {
		t.Errorf("post-reload candidatesFor = %+v, want [{p2 m2}]", cands)
	}
	if order, _ := r.priorityFor(AgentRequest{}); len(order) != 1 || order[0] != "p2" {
		t.Errorf("post-reload priorityFor = %v, want [p2]", order)
	}

	// The availability matrix is untouched: p1's reachability persists across the
	// policy reload (reachability is a live probe fact, not config).
	snap := r.Snapshot()
	if av, ok := snap["p1"]; !ok || !av.Reachable {
		t.Errorf("p1 reachability lost across ReloadPolicy: %+v", snap["p1"])
	}
}

// TestReloadPolicy_EmptyPriorityFallsBackToDefault mirrors NewResolver's
// empty-priority handling.
func TestReloadPolicy_EmptyPriorityFallsBackToDefault(t *testing.T) {
	r := NewResolver([]string{"p1"}, nil)
	r.ReloadPolicy(nil, nil)
	order, _ := r.priorityFor(AgentRequest{})
	if len(order) == 0 {
		t.Error("empty priority should fall back to the default library priority, got none")
	}
}

// TestReloadPolicy_RaceWithReads runs concurrent policy reads (candidatesFor /
// priorityFor, the resolver's hot path) against ReloadPolicy — under -race this
// fails if the in-place swap is not synchronized.
func TestReloadPolicy_RaceWithReads(t *testing.T) {
	r := NewResolver([]string{"p1"}, map[string][]Candidate{"low": {{Provider: "p1", Model: "m1"}}})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			_ = r.candidatesFor(AgentRequest{Tier: "low"})
			_, _ = r.priorityFor(AgentRequest{})
		}
		close(done)
	}()
	for i := 0; i < 2000; i++ {
		r.ReloadPolicy([]string{"p2"}, map[string][]Candidate{"low": {{Provider: "p2", Model: "m2"}}})
	}
	<-done
}
