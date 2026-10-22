package scheduler

import (
	"context"
	"testing"

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
			if !logs.contains(`user="alice"`) {
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
