package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedLivePass writes a consolidation pass for (tenantID, userID) whose run has
// not ended: the shape a pass has once a resume re-dispatched it after a
// restart (pause_state running), or while it waits parked for one (paused).
// Its run went through no fan-out of this process.
func seedLivePass(t *testing.T, st store.Store, tenantID, userID, agentName, pauseState string) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenantID, agentName, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "pass-" + sess.ID, UserID: userID, TenantID: tenantID})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.SetRunPauseState(ctx, run.ID, pauseState); err != nil {
		t.Fatalf("SetRunPauseState: %v", err)
	}
}

// A pass resumed after a restart runs outside the fan-out that dispatched it,
// so nothing in the next tick's sweep knows of it. The tick must still leave
// its target alone, or two passes consolidate one target at once. The run row
// is the record of it, and it is shared by every replica.
func TestFanout_ATargetWithALivePassIsNotDispatchedAgain(t *testing.T) {
	for _, pauseState := range []string{store.PauseStateRunning, store.PauseStatePaused} {
		t.Run(pauseState, func(t *testing.T) {
			def := fanoutDef(nil)
			sched, fr, st, logs := fanoutFixture(t, def, nil)
			sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
			seedSettledSession(t, st, "", "alice")
			seedSettledSession(t, st, "", "bob")
			seedLivePass(t, st, "", "alice", def.Agent, pauseState)

			fireT(t, sched)

			calls := fr.Calls()
			if len(calls) != 1 || calls[0].UserID != "bob" {
				users := make([]string, 0, len(calls))
				for _, c := range calls {
					users = append(users, c.UserID)
				}
				t.Fatalf("dispatched %v, want only bob: alice's pass is still live", users)
			}
			if !logs.containsLine("still has a pass live", "user=alice") {
				t.Errorf("the skip is not logged; logs:\n%s", logs.all())
			}
		})
	}
}

// Only the target's own tenant counts: the same user id in another tenant is
// another target, whose live pass says nothing about this one.
func TestFanout_ALivePassInAnotherTenantDoesNotHoldTheTarget(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, _ := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", "alice")
	seedLivePass(t, st, "other", "alice", def.Agent, store.PauseStateRunning)

	fireT(t, sched)

	if calls := fr.Calls(); len(calls) != 1 || calls[0].UserID != "alice" {
		t.Fatalf("calls = %+v, want one pass for alice", calls)
	}
}

// The check reads past a busy user's newer runs. A resumed pass keeps its old
// started_at, so it is the OLDEST running row of a user whose chats kept
// going; a read of the user's newest runs missed it and started a second
// pass.
func TestFanout_ALivePassBehindManyNewerRunsStillHoldsTheTarget(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, _ := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", "alice")
	seedLivePass(t, st, "", "alice", def.Agent, store.PauseStateRunning)
	ctx := context.Background()
	busy, err := st.CreateSession(ctx, "", "chat", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 105; i++ {
		if _, err := st.CreateRun(ctx, busy.ID, store.RunIdentity{AgentID: fmt.Sprintf("busy-%d", i), UserID: "alice"}); err != nil {
			t.Fatal(err)
		}
	}

	fireT(t, sched)

	if calls := fr.Calls(); len(calls) != 0 {
		t.Fatalf("dispatched %d pass(es) for alice, want none: her pass is still live behind 105 newer runs", len(calls))
	}
}

// A pass parked for its resume holds its target only for so long: one that is
// never resumed (its agent gone, resumes failing) must not stop the target's
// consolidation for good. Past the bound the tick runs a new pass, and says
// which run it is ignoring.
func TestFanout_APassParkedPastTheBoundNoLongerHoldsTheTarget(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, logs := fanoutFixture(t, def, func(c *Config) { c.stalePausedPassAfter = time.Millisecond })
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", "alice")
	seedLivePass(t, st, "", "alice", def.Agent, store.PauseStatePaused)
	time.Sleep(20 * time.Millisecond)

	fireT(t, sched)

	if calls := fr.Calls(); len(calls) != 1 || calls[0].UserID != "alice" {
		t.Fatalf("calls = %+v, want one pass for alice: the parked pass is past the bound", calls)
	}
	if !logs.contains("parked") {
		t.Errorf("ignoring the stuck pass is not logged; logs:\n%s", logs.all())
	}
}
