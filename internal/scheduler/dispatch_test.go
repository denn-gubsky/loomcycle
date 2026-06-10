package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// peekScopeCount returns how many messages sit on a channel at a specific
// (scope, scope_id) — the F37 assertion needs scope granularity, which the
// scope-blind ChannelStats can't give.
func peekScopeCount(t *testing.T, st store.Store, channel string, scope store.MemoryScope, scopeID string) int {
	t.Helper()
	msgs, err := st.ChannelPeek(context.Background(), channel, scope, scopeID, "", 100)
	if err != nil {
		t.Fatalf("ChannelPeek(%s/%s): %v", scope, scopeID, err)
	}
	return len(msgs)
}

func channelHookDef(channel string) scheduleDef {
	enabled := true
	return scheduleDef{
		Agent:    "researcher",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		UserID:   "alice",
		OnComplete: []scheduleHook{
			{Kind: "channel.publish", Channel: channel, Payload: map[string]any{"top": 3}},
		},
	}
}

// TestScheduler_OnCompleteChannelPublish_HonorsGlobalScope is the F37 (RFC T)
// regression: a hook publishing to a scope:global channel must land at
// global/"" — where a global reader (admin peek, Channel.await/subscribe
// resolving global) can see it — not under the run's user scope.
//
// Fail-before: without the resolver the publish goes to user/alice, so the
// global peek returns 0 and the user peek returns 1 (both assertions flip).
func TestScheduler_OnCompleteChannelPublish_HonorsGlobalScope(t *testing.T) {
	sched, _, _, _, st := schedulerFixture(t, channelHookDef("exp5-pings"), time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (string, bool) { return "global", true })

	fireT(t, sched)

	if got := peekScopeCount(t, st, "exp5-pings", store.MemoryScopeGlobal, ""); got != 1 {
		t.Errorf("global/\"\" message count = %d, want 1 (F37: hook mis-scoped to user)", got)
	}
	if got := peekScopeCount(t, st, "exp5-pings", store.MemoryScopeUser, "alice"); got != 0 {
		t.Errorf("user/alice message count = %d, want 0 (message should not be at user scope)", got)
	}
}

// TestScheduler_OnCompleteChannelPublish_UserScope: a scope:user channel
// still publishes under user/<user_id> (unchanged from pre-fix).
func TestScheduler_OnCompleteChannelPublish_UserScope(t *testing.T) {
	sched, _, _, _, st := schedulerFixture(t, channelHookDef("results-alice"), time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (string, bool) { return "user", true })

	fireT(t, sched)

	if got := peekScopeCount(t, st, "results-alice", store.MemoryScopeUser, "alice"); got != 1 {
		t.Errorf("user/alice message count = %d, want 1", got)
	}
	if got := peekScopeCount(t, st, "results-alice", store.MemoryScopeGlobal, ""); got != 0 {
		t.Errorf("global/\"\" message count = %d, want 0", got)
	}
}

// TestScheduler_OnCompleteChannelPublish_AgentScope: a scope:agent channel
// publishes under agent/<agent name> (the schedule's def.Agent).
func TestScheduler_OnCompleteChannelPublish_AgentScope(t *testing.T) {
	sched, _, _, _, st := schedulerFixture(t, channelHookDef("agent-bus"), time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (string, bool) { return "agent", true })

	fireT(t, sched)

	if got := peekScopeCount(t, st, "agent-bus", store.MemoryScopeAgent, "researcher"); got != 1 {
		t.Errorf("agent/researcher message count = %d, want 1", got)
	}
}

// TestScheduler_OnCompleteChannelPublish_UndeclaredFails: an undeclared
// channel (resolver ok=false) fails the hook loudly rather than silently
// mis-scoping — nothing is published.
func TestScheduler_OnCompleteChannelPublish_UndeclaredFails(t *testing.T) {
	sched, _, _, _, st := schedulerFixture(t, channelHookDef("ghost"), time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (string, bool) { return "", false })

	fireT(t, sched)

	if got := channelMessageCount(t, st, "ghost"); got != 0 {
		t.Errorf("undeclared channel message count = %d, want 0 (hook should fail, not publish)", got)
	}
}

// TestScheduler_OnCompleteChannelPublish_LegacyNilResolver locks the
// back-compat contract: with no resolver wired, a hook publishes under the
// run's user scope (the pre-F37 behavior), so small embeds that don't inject
// a resolver keep working.
func TestScheduler_OnCompleteChannelPublish_LegacyNilResolver(t *testing.T) {
	sched, _, _, _, st := schedulerFixture(t, channelHookDef("results-alice"), time.Now().Add(-1*time.Minute))
	// No SetChannelScope call → chScope nil → legacy path.

	fireT(t, sched)

	if got := peekScopeCount(t, st, "results-alice", store.MemoryScopeUser, "alice"); got != 1 {
		t.Errorf("legacy user/alice message count = %d, want 1", got)
	}
}
