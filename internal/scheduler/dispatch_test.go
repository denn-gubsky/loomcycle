package scheduler

import (
	"context"
	"encoding/json"
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

// TestScheduler_OnCompleteHookUsesSurvivalCtx is the exp7 regression: a run
// that completes just as shutdown begins records its result on the survival
// ctx (recordCtx) — but dispatchHooks used the parent ctx, so the on_complete
// hook was dropped on a cancelled context. Fire with an already-cancelled
// parent ctx and assert the channel.publish hook still lands.
//
// FAIL-BEFORE: with dispatchHooks(ctx) the ChannelPublish runs on the cancelled
// ctx, fails, and the global peek returns 0.
func TestScheduler_OnCompleteHookUsesSurvivalCtx(t *testing.T) {
	def := channelHookDef("ctx-survival")
	sched, _, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (string, bool) { return "global", true })

	// Parent ctx already cancelled — the run still "completes" (the fake
	// runner ignores ctx), so status=="completed" and the survival ctx kicks
	// in for both the result-write and (post-fix) the hook dispatch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	sched.fireOne(ctx, store.ScheduleDueRow{DefID: defID, Name: "sched-test", Definition: defJSON}, time.Now())

	if got := peekScopeCount(t, st, "ctx-survival", store.MemoryScopeGlobal, ""); got != 1 {
		t.Errorf("on_complete publish landed %d messages, want 1 (hook must use the survival ctx)", got)
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
