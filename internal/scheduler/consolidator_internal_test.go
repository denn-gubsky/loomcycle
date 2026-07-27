package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestFanout_IgnoresInternalAgentsSessions is the fan-out half of the
// self-consuming-consolidator regression.
//
// TestFanout_IgnoresItsOwnPastRuns already covers the pass's OWN sessions. This
// covers its CHILDREN: a pass spawns one extractor run per chat it reads, each
// child is a session under the target's user id, and each child's transcript
// CONTAINS the chat it was extracting. Nothing excluded them, so a fully
// caught-up target reported new work on every tick forever, and each pass
// re-extracted nested copies of its own input — 7 of the last 8 chats on the
// live store, growing ~15 a pass with no bound.
//
// Fails-before with the extractor sessions counted: the target looks like it has
// work and RunOnce is called.
func TestFanout_IgnoresInternalAgentsSessions(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, logs := fanoutFixture(t, def, func(c *Config) {
		c.InternalAgents = []string{"memory/consolidator", "memory/extractor"}
	})
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	// alice has one real chat, and the watermark is already past it.
	seedSettledSession(t, st, "", "alice")
	ctx := context.Background()
	rows, err := st.ConsolidatableSessions(ctx, "", "alice", "", []string{def.Agent}, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ConsolidatableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("alice should have exactly 1 real settled chat, got %d", len(rows))
	}
	last := rows[0]
	if _, _, err := st.MemoryCursorLease(ctx, "", store.MemoryScopeUser, "alice", "o", time.Now(), time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := st.MemoryCursorAdvance(ctx, "", store.MemoryScopeUser, "alice", "o", last.MaxCompletedAt, last.SessionID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := st.MemoryCursorRelease(ctx, "", store.MemoryScopeUser, "alice", "o"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// …and then a pass's worth of extractor children lands, plus one session
	// from a differently-named peer consolidator (which self-exclusion, keyed on
	// def.Agent, could never reach).
	for i := 0; i < 5; i++ {
		seedSettledSessionAs(t, st, "", "alice", "memory/extractor")
	}
	seedSettledSessionAs(t, st, "", "alice", "memory/consolidator")

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Errorf("RunOnce calls = %d, want 0 — a caught-up target's own extractor children must not count as new work; logs:\n%s", got, logs.all())
	}
}

// TestFanout_InternalAgentRunsDoNotMakeATargetACandidate: the same exclusion at
// candidate ENUMERATION. It matters more here than for the self case, because
// the candidate scan reads a fixed 500-row window ordered most-recently-active
// first — and a pass's children are by construction the most recent sessions
// there are, so leaving them in eventually starves every real target out of the
// window entirely.
func TestFanout_InternalAgentRunsDoNotMakeATargetACandidate(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, _ := fanoutFixture(t, def, func(c *Config) {
		c.InternalAgents = []string{"memory/extractor"}
	})
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	// A user who never chatted — only the runtime extracted under their id.
	seedSettledSessionAs(t, st, "", "ghost", "memory/extractor")

	fireT(t, sched)

	for _, c := range fr.Calls() {
		if c.UserID == "ghost" {
			t.Error("dispatched a pass for a user whose only sessions are the runtime's own extractor children")
		}
	}
}

// TestFanout_ExcludedAgentsUnionsSelfAndInternal covers the set the two probes
// share. The schedule's own agent has to survive alongside the declared set (an
// operator pointing a schedule at a NON-internal agent still needs
// self-exclusion), and a schedule whose agent IS internal must not produce a
// duplicate — a repeated name in a NOT IN list is harmless in SQL but makes the
// Postgres placeholder run longer than the caller counted.
func TestFanout_ExcludedAgentsUnionsSelfAndInternal(t *testing.T) {
	sched, _, _, _ := fanoutFixture(t, fanoutDef(nil), func(c *Config) {
		c.InternalAgents = []string{"memory/extractor", "memory/consolidator"}
	})

	got := sched.excludedAgents("memory/consolidator")
	want := []string{"memory/consolidator", "memory/extractor"}
	if len(got) != len(want) {
		t.Fatalf("excludedAgents = %v, want %v (deduped)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("excludedAgents = %v, want %v (sorted, deduped)", got, want)
		}
	}
	if got := sched.excludedAgents("operator-own-agent"); len(got) != 3 {
		t.Errorf("excludedAgents(non-internal schedule agent) = %v, want the 2 internal names plus the schedule's own", got)
	}
	// An empty declaration must leave the self-exclusion intact — that is the
	// pre-feature behaviour every existing fan-out test depends on.
	bare, _, _, _ := fanoutFixture(t, fanoutDef(nil), nil)
	if got := bare.excludedAgents("solo"); len(got) != 1 || got[0] != "solo" {
		t.Errorf("excludedAgents with no InternalAgents = %v, want just the schedule's own agent", got)
	}
}
