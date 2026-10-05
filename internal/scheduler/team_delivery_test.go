package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// fakeTeams records the walks a team tick asks for.
type fakeTeams struct {
	mu    sync.Mutex
	calls []runner.TeamWalkInput
	err   error
}

func (f *fakeTeams) StartTeamWalk(_ context.Context, in runner.TeamWalkInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.err != nil {
		return "", f.err
	}
	return "r_walk", nil
}

func (f *fakeTeams) Calls() []runner.TeamWalkInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.TeamWalkInput(nil), f.calls...)
}

func teamTick(maxFires int) scheduleDef {
	enabled := true
	return scheduleDef{
		Delivery: "team", Team: "weekly-report", Schedule: "0 * * * *", Enabled: &enabled,
		Vars: map[string]string{"repo": "loomcycle", "user": "u-42"}, Input: "the digest",
		TenantID: "acme", UserID: "u-42", OperatorKeyRestricted: true, Isolated: true,
		MaxFires: maxFires,
	}
}

// A team tick starts one walk — carrying the def's team, literal vars, input
// and identity — and no agent run. From the schedule's side it is a fire like
// any other: it records the walk's run id, advances, and counts.
func TestScheduler_TeamDeliveryStartsAWalkWithTheDefsVarsAndIdentity(t *testing.T) {
	sched, fr, _, defID, st := schedulerFixture(t, teamTick(0), time.Now().Add(-1*time.Minute))
	teams := &fakeTeams{}
	sched.SetTeamWalkStarter(teams)

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("RunOnce calls = %d, want 0 — a team tick starts a walk, not an agent run", got)
	}
	calls := teams.Calls()
	if len(calls) != 1 {
		t.Fatalf("walks started = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.Team != "weekly-report" || got.Input != "the digest" || got.Vars["repo"] != "loomcycle" || got.Vars["user"] != "u-42" || len(got.Vars) != 2 {
		t.Errorf("walk input = %+v", got)
	}
	if got.TenantID != "acme" || got.UserID != "u-42" || !got.OperatorKeyRestricted || !got.Isolated {
		t.Errorf("walk identity = tenant %q user %q restricted=%v isolated=%v, want the def's own",
			got.TenantID, got.UserID, got.OperatorKeyRestricted, got.Isolated)
	}
	if got.IdempotencyKey != "" || got.DeliveryAltKey != "" {
		t.Errorf("a tick carries delivery keys %q / %q; it has no delivery to dedup", got.IdempotencyKey, got.DeliveryAltKey)
	}
	state, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.LastStatus != "completed" || state.LastRunID != "r_walk" || state.FireCount != 1 {
		t.Errorf("state = status %q run %q fire_count %d, want completed / r_walk / 1", state.LastStatus, state.LastRunID, state.FireCount)
	}
	if state.NextRunAt.Before(time.Now()) {
		t.Errorf("next_run_at = %v, expected the future", state.NextRunAt)
	}
}

// A team that cannot start is a mistake in the definition, not a fire. With
// max_fires=1 the difference is whether the schedule survives it: counted, the
// def retires after one refusal and the misconfiguration reads as a normal
// one-shot. It must be logged and recorded, and must still advance.
func TestScheduler_TeamDeliveryThatCannotStartIsLoggedAndDoesNotBurnMaxFires(t *testing.T) {
	sched, fr, _, defID, st := schedulerFixture(t, teamTick(1), time.Now().Add(-1*time.Minute))
	var logged []string
	sched.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	sched.SetTeamWalkStarter(&fakeTeams{err: fmt.Errorf("%w: no active team %q in tenant %q", runner.ErrTeamNotStartable, "weekly-report", "acme")})
	ctx := context.Background()

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("RunOnce calls = %d, want 0", got)
	}
	state, _ := st.ScheduleRunStateGet(ctx, defID)
	if state.FireCount != 0 {
		t.Errorf("fire_count = %d, want 0 (a team that cannot start must not count toward max_fires)", state.FireCount)
	}
	if state.LastStatus != "failed" || !strings.Contains(state.LastError, "no active team") {
		t.Errorf("state = status %q error %q, want failed with the reason", state.LastStatus, state.LastError)
	}
	if state.NextRunAt.Before(time.Now()) {
		t.Errorf("a refused tick did not advance next_run_at: %v", state.NextRunAt)
	}
	row, err := st.ScheduleDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("def get: %v", err)
	}
	if row.Retired {
		t.Fatal("def retired after a team that could not start — max_fires must not have been reached")
	}
	found := false
	for _, line := range logged {
		if strings.Contains(line, "could not start team") && strings.Contains(line, "weekly-report") {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line names the team that could not start: %q", logged)
	}
}

// Every other refusal is classified the way a run's is.
func TestScheduler_TeamDeliveryClassifiesOtherStartErrorsLikeARun(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
		wantCount  int
	}{
		{"a failure nobody classified", errors.New("start team walk: board unreadable"), "failed", 1},
		{"a spent token budget", fmt.Errorf("%w: tenant budget", runner.ErrTokenLimitExceeded), "skipped", 1},
		{"a runtime paused mid-tick", runner.ErrRuntimePaused, "skipped", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sched, _, _, defID, st := schedulerFixture(t, teamTick(0), time.Now().Add(-1*time.Minute))
			sched.SetTeamWalkStarter(&fakeTeams{err: c.err})
			fireT(t, sched)
			state, _ := st.ScheduleRunStateGet(context.Background(), defID)
			if state.LastStatus != c.wantStatus || state.FireCount != c.wantCount {
				t.Errorf("state = status %q fire_count %d, want %q / %d", state.LastStatus, state.FireCount, c.wantStatus, c.wantCount)
			}
		})
	}
}

// With nothing wired to start a walk the tick fails loudly rather than falling
// through to an agent run it has no agent for.
func TestScheduler_TeamDeliveryWithNoStarterIsARecordedFailure(t *testing.T) {
	sched, fr, _, defID, st := schedulerFixture(t, teamTick(0), time.Now().Add(-1*time.Minute))
	fireT(t, sched)
	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("RunOnce calls = %d, want 0", got)
	}
	state, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if state.LastStatus != "failed" || !strings.Contains(state.LastError, "no team walk starter") {
		t.Errorf("state = status %q error %q, want a recorded failure", state.LastStatus, state.LastError)
	}
}

// The kill switch works on a team tick too.
func TestScheduler_TeamDeliveryRespectsDisabled(t *testing.T) {
	def := teamTick(0)
	off := false
	def.Enabled = &off
	sched, _, _, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	teams := &fakeTeams{}
	sched.SetTeamWalkStarter(teams)
	fireT(t, sched)
	if got := len(teams.Calls()); got != 0 {
		t.Fatalf("a disabled team schedule started %d walk(s)", got)
	}
}

// A row a tenant wrote before create stamped its tenant into the body runs its
// walk in the tenant that owns it, as its agent run and its channel tick do.
func TestTeamDelivery_LegacyEmptyBodyTenantWalksInOwningTenant(t *testing.T) {
	def := teamTick(0)
	def.TenantID = ""
	sched, _, _ := fanoutFixtureOwnedBy(t, def, "acme")
	teams := &fakeTeams{}
	sched.SetTeamWalkStarter(teams)
	fireT(t, sched)
	if calls := teams.Calls(); len(calls) != 1 || calls[0].TenantID != "acme" {
		t.Fatalf("a legacy row owned by acme walked as %+v, want one walk in acme", calls)
	}
}

func TestUnmarshalDef_TeamDeliveryNeedsATeam(t *testing.T) {
	if _, err := unmarshalDef([]byte(`{"delivery":"team","schedule":"0 * * * *"}`)); err == nil || !strings.Contains(err.Error(), "no `team` field") {
		t.Fatalf("err = %v, want a refusal of a team tick with no team", err)
	}
	def, err := unmarshalDef([]byte(`{"delivery":"team","team":"weekly-report","vars":{"repo":"x"},"input":"go","schedule":"0 * * * *"}`))
	if err != nil {
		t.Fatalf("a team tick with no agent did not decode: %v", err)
	}
	if def.Team != "weekly-report" || def.Vars["repo"] != "x" || def.Input != "go" {
		t.Errorf("decoded = %+v", def)
	}
}
