package scheduler

import (
	"testing"
	"time"
)

// A team tick's walk resolves $cred: in its user's own scope only when the
// schedule's author could act as that user — the rule an agent-run tick
// follows — and its members inherit what the walk carries.
func TestScheduler_TeamDeliveryTakesTheUserScopeOnlyWhenTheAuthorCould(t *testing.T) {
	for _, tc := range []struct {
		userScope, wantNoUser bool
	}{{false, true}, {true, false}} {
		def := teamTick(0)
		def.UserScopeCredentials = tc.userScope
		sched, _, _, _, _ := schedulerFixture(t, def, time.Now().Add(-time.Minute))
		teams := &fakeTeams{}
		sched.SetTeamWalkStarter(teams)
		fireT(t, sched)
		calls := teams.Calls()
		if len(calls) != 1 {
			t.Fatalf("user_scope_credentials=%t: %d walks", tc.userScope, len(calls))
		}
		if calls[0].CredentialsNoUserScope != tc.wantNoUser {
			t.Errorf("user_scope_credentials=%t: walk CredentialsNoUserScope = %t, want %t", tc.userScope, calls[0].CredentialsNoUserScope, tc.wantNoUser)
		}
	}
}
