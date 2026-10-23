package scheduler

import (
	"testing"
	"time"
)

func userSchedule(userScope bool) scheduleDef {
	enabled := true
	return scheduleDef{
		Agent: "digest", Schedule: "0 * * * *", Enabled: &enabled, UserID: "bob",
		UserCredentials:      map[string]string{"slack": "$cred:slack"},
		UserScopeCredentials: userScope,
	}
}

// A schedule's run resolves $cred: in its user's own scope only when its
// author could act as that user. One written before the bit existed, or by
// someone else, resolves for its agent and tenant, and each fire says so.
func TestScheduler_UserScopeOnlyWhenTheAuthorCouldActAsTheUser(t *testing.T) {
	for _, tc := range []struct {
		userScope, wantNoUser bool
	}{{false, true}, {true, false}} {
		sched, fr, _, _, _ := schedulerFixture(t, userSchedule(tc.userScope), time.Now().Add(-time.Minute))
		logs := &logCapture{}
		sched.logger = logs.logger()
		fireT(t, sched)
		calls := fr.Calls()
		if len(calls) != 1 {
			t.Fatalf("user_scope_credentials=%t: %d runs", tc.userScope, len(calls))
		}
		if calls[0].CredentialsNoUserScope != tc.wantNoUser {
			t.Errorf("user_scope_credentials=%t: CredentialsNoUserScope = %t", tc.userScope, calls[0].CredentialsNoUserScope)
		}
		if warned := logs.contains("was not written by an operator or by that user"); warned != tc.wantNoUser {
			t.Errorf("user_scope_credentials=%t: warned %t", tc.userScope, warned)
		}
	}
}

// A schedule from the operator's yaml is the operator's: it keeps the user
// scope without the bit.
func TestScheduler_AYamlScheduleKeepsTheUserScope(t *testing.T) {
	sched, fr, _ := fanoutFixtureRow(t, userSchedule(false), "", true)
	fireT(t, sched)
	calls := fr.Calls()
	if len(calls) != 1 || calls[0].CredentialsNoUserScope {
		t.Fatalf("a bootstrapped yaml schedule's run: %+v, want the user scope kept", calls)
	}
}

// A consolidation pass runs as each target's user, whom the schedule was not
// written for: only an operator-written one keeps the user scope.
func TestFanout_OnlyAnOperatorConsolidationKeepsTheUserScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		def        scheduleDef
		wantNoUser bool
	}{
		{"a tenant's", func() scheduleDef { d := fanoutDef(nil); d.UserScopeCredentials = true; return d }(), true},
		{"the operator's", operatorFanoutDef(nil), false},
	} {
		sched, fr, st, _ := fanoutFixture(t, tc.def, nil)
		seedSettledSession(t, st, "", "olga")
		fireT(t, sched)
		calls := fr.Calls()
		if len(calls) == 0 {
			t.Fatalf("%s: no pass dispatched", tc.name)
		}
		for _, c := range calls {
			if c.CredentialsNoUserScope != tc.wantNoUser {
				t.Errorf("%s: pass for %s/%s CredentialsNoUserScope = %t", tc.name, c.TenantID, c.UserID, c.CredentialsNoUserScope)
			}
		}
	}
}
