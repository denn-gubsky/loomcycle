package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// hangingRunner records every call through a fakeRunner, then — for the targets
// hang selects — blocks until the run's ctx ends, the way a real run stuck on a
// slow model behaves. The plain fakeRunner ignores ctx, so it cannot show which
// budget cut a pass.
type hangingRunner struct {
	*fakeRunner
	hang func(in runner.RunInput) bool
}

func (r hangingRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	if err := r.fakeRunner.RunOnce(ctx, in, cb); err != nil {
		return err
	}
	if r.hang(in) {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// dueRow reads the fixture's single due schedule row, so a test can fire it as
// many times as it needs (a tick advances next_run_at by an hour).
func dueRow(t *testing.T, st store.Store) store.ScheduleDueRow {
	t.Helper()
	due, err := st.ScheduleRunStateListDue(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("ScheduleRunStateListDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due rows = %d, want 1", len(due))
	}
	return due[0]
}

// callsSince returns the "tenant/user" of every dispatch after the first n.
func callsSince(fr *fakeRunner, n int) []string {
	return dispatched(fr)[n:]
}

func scheduleState(t *testing.T, sched *Scheduler, defID string) store.ScheduleRunStateRow {
	t.Helper()
	state, err := sched.store.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("ScheduleRunStateGet: %v", err)
	}
	return state
}

// TestFanout_SlowEarlyTenantDoesNotStarveLaterTenants is sched-3. The bundled
// consolidator is a code agent, so its sweep is serial, and the whole batch
// shared ONE budget in (tenant, user) order. A pass the budget cut does not
// advance its watermark, so it was selected — and went first — again on every
// tick: one slow tenant early in the alphabet spent the budget every time and
// every later tenant was starved forever.
//
// Each pass now gets a slice of the budget, so the second tenant runs in the
// very first tick even though the first one never finishes.
//
// Fails-before: zzz is never dispatched; aaa holds the whole budget.
func TestFanout_SlowEarlyTenantDoesNotStarveLaterTenants(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
		c.FireTimeout = 600 * time.Millisecond
	})
	// No provider resolver: serial, as the code-js consolidator is.
	sched.runner = hangingRunner{fakeRunner: fr, hang: func(in runner.RunInput) bool { return in.TenantID == "aaa" }}

	seedSettledSession(t, st, "aaa", "slow")
	seedSettledSession(t, st, "zzz", "zed")

	fireT(t, sched)

	got := dispatched(fr)
	if len(got) == 0 || got[0] != "aaa/slow" {
		t.Fatalf("dispatched = %v, want the tick to start with aaa (the slow tenant first is the case under test); logs:\n%s", got, logs.all())
	}
	if strings.Join(got, ",") != "aaa/slow,zzz/zed" {
		t.Fatalf("dispatched = %v, want zzz dispatched in the same tick — a slow pass in aaa must not spend the whole budget; logs:\n%s", got, logs.all())
	}
}

// TestFanout_QueueOnlyTargetsAreNotStarvedByTheCap is sched-3's cap path.
// Session-derived candidates were listed before queue-only ones and the cap kept
// the head of the list, so while there were at least `cap` session targets, a
// target with only queued work — a queue restored from a snapshot, or a user
// whose chats aged out of the scan — was cut on every tick, forever.
//
// Fails-before: across every tick only session targets are dispatched.
func TestFanout_QueueOnlyTargetsAreNotStarvedByTheCap(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), func(c *Config) {
		c.MaxConsolidationTargets = 2
	})
	for _, u := range []string{"s1", "s2", "s3"} {
		seedSettledSession(t, st, "", u)
	}
	enqueuePending(t, st, "p_quinn", "", "quinn")
	row := dueRow(t, st)

	// The fake runner never advances a watermark, so every target keeps its work
	// on every tick — the sustained-load case.
	for tick := 0; tick < 3; tick++ {
		sched.fireOne(context.Background(), row, time.Now())
	}

	for _, d := range dispatched(fr) {
		if d == "/quinn" {
			return
		}
	}
	t.Fatalf("the queue-only target was never dispatched in 3 capped ticks; dispatched = %v; logs:\n%s", dispatched(fr), logs.all())
}

// TestFanout_StartTenantRotatesAcrossTicks: the sweep starts with a different
// tenant each tick, so the tenant whose turn comes first — the one guaranteed a
// pass when the budget is short — is not always the same.
//
// Fails-before: every tick starts with acme.
func TestFanout_StartTenantRotatesAcrossTicks(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), nil)
	// No provider resolver: serial, so dispatch order is the target order.
	for i, tenant := range []string{"acme", "beta", "gamma"} {
		seedSettledSession(t, st, tenant, fmt.Sprintf("u%d", i))
	}
	row := dueRow(t, st)

	var firsts []string
	for tick := 0; tick < 4; tick++ {
		before := len(fr.Calls())
		sched.fireOne(context.Background(), row, time.Now())
		calls := callsSince(fr, before)
		if len(calls) != 3 {
			t.Fatalf("tick %d dispatched %v, want all three tenants; logs:\n%s", tick, calls, logs.all())
		}
		firsts = append(firsts, strings.SplitN(calls[0], "/", 2)[0])
	}
	if got, want := strings.Join(firsts, ","), "acme,beta,gamma,acme"; got != want {
		t.Errorf("first tenant per tick = %s, want %s", got, want)
	}
}

// deferredFanoutCase runs an operator-layer hooked fan-out where acme/sam is
// refused with refusal and every other target runs, and reports what the
// schedule recorded.
func deferredFanoutCase(t *testing.T, refusal error) {
	t.Helper()
	sched, fr, st, logs := fanoutFixture(t, hookedFanoutDef(), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global"}, true
	})
	fr.resultFor = func(in runner.RunInput) (string, error) {
		if in.TenantID == "acme" && in.UserID == "sam" {
			return "", fmt.Errorf("admit: %w", refusal)
		}
		return runIDPerTenant(in)
	}

	seedSettledSession(t, st, "default", "alice")
	seedSettledSession(t, st, "acme", "sam")
	seedSettledSession(t, st, "acme", "tia")

	fireT(t, sched)

	state := scheduleState(t, sched, "sd-test")
	if state.LastStatus == "failed" {
		t.Errorf("last_status = failed (%q) — a refused admission is a deferral, not a broken schedule; logs:\n%s", state.LastError, logs.all())
	}
	if state.LastStatus != "skipped" {
		t.Errorf("last_status = %q, want skipped — the sweep was not whole", state.LastStatus)
	}
	if !strings.Contains(state.LastError, "token budget or the operator-key restriction") {
		t.Errorf("last_error = %q, want it to name the deferral", state.LastError)
	}
	if state.FireCount != 1 {
		t.Errorf("fire_count = %d, want 1 — a tick refused by a budget still counts toward max_fires", state.FireCount)
	}
	for _, tenant := range []string{"default", "acme"} {
		if n := len(globalLayer(t, st, tenant, "passes")); n != 1 {
			t.Errorf("tenant %q's hooks published %d message(s), want 1 — a deferred pass must not withhold them", tenant, n)
		}
	}
	if msgs := globalLayer(t, st, "acme", "passes"); len(msgs) == 1 {
		if got := hookRunID(t, msgs[0]); got != "r_acme_tia" {
			t.Errorf("acme's hook reports run %q, want the pass that ran (r_acme_tia)", got)
		}
	}
}

// TestFanout_TokenLimitedTargetIsDeferredNotFailed is sched-4. One user over
// their hard token budget was classified as a genuine failure: the operator's
// schedule read 'failed', and that user's whole tenant lost its on_complete
// hooks although nothing broke.
//
// Fails-before: last_status is failed and acme publishes nothing.
func TestFanout_TokenLimitedTargetIsDeferredNotFailed(t *testing.T) {
	deferredFanoutCase(t, runner.ErrTokenLimitExceeded)
}

// TestFanout_OperatorKeyRefusalIsDeferredNotFailed: a foreign tenant's pass the
// operator-key restriction refuses — at routing, or in the driver for a pinned
// agent — waits for that tenant's own key; it is not a broken sweep.
//
// Fails-before: last_status is failed.
func TestFanout_OperatorKeyRefusalIsDeferredNotFailed(t *testing.T) {
	for _, refusal := range []error{resolve.ErrOperatorKeyRestricted, providers.ErrOperatorKeyForbidden} {
		t.Run(refusal.Error(), func(t *testing.T) { deferredFanoutCase(t, refusal) })
	}
}

// TestFanout_PauseMidBatchStopsDispatch is sched-4's pause path. The scheduler
// checks the pause only at the start of a tick, so a pause that began mid-sweep
// refused every remaining target in turn, each recorded as a failure, and the
// tick used up one of the schedule's max_fires.
//
// Now the first refusal stops the sweep, the schedule reads skipped, and the
// tick does not count: the first fire after resume finishes it.
//
// Fails-before: all three targets are dispatched, status failed, fire_count 1.
func TestFanout_PauseMidBatchStopsDispatch(t *testing.T) {
	def := fanoutDef(nil)
	def.MaxFires = 1
	sched, fr, st, logs := fanoutFixture(t, def, nil)
	// No provider resolver: serial, so "after the pause" is well defined.
	var (
		mu sync.Mutex
		n  int
	)
	fr.resultFor = func(in runner.RunInput) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return "r_first", nil
		}
		return "", fmt.Errorf("admit: %w", runner.ErrRuntimePaused)
	}
	for _, u := range []string{"u1", "u2", "u3"} {
		seedSettledSession(t, st, "", u)
	}

	fireT(t, sched)

	if got := len(fr.Calls()); got != 2 {
		t.Errorf("RunOnce calls = %d, want 2 — the first pause refusal must stop the sweep; logs:\n%s", got, logs.all())
	}
	state := scheduleState(t, sched, "sd-test")
	if state.LastStatus != "skipped" {
		t.Errorf("last_status = %q (%q), want skipped — a pause is not a failure", state.LastStatus, state.LastError)
	}
	if state.FireCount != 0 {
		t.Errorf("fire_count = %d, want 0 — a sweep the pause cut short must not use up a fire", state.FireCount)
	}
	if !logs.contains("paused mid-sweep") {
		t.Errorf("stopping the sweep must be logged; logs:\n%s", logs.all())
	}
	if row, err := st.ScheduleDefGet(context.Background(), "sd-test"); err != nil {
		t.Fatalf("def get: %v", err)
	} else if row.Retired {
		t.Error("max_fires=1 retired the schedule on a tick the pause cut short")
	}
}

// TestFireOne_TokenLimitIsSkipped: a single-run schedule refused by its hard
// token budget is not a broken schedule. It counts toward max_fires — the
// refusal repeats until the budget period rolls over.
//
// Fails-before: last_status is failed.
func TestFireOne_TokenLimitIsSkipped(t *testing.T) {
	sched, fr, _, defID, _ := schedulerFixture(t, channelHookDef("passes"), time.Now().Add(-time.Minute))
	fr.runErr = fmt.Errorf("admit: %w", runner.ErrTokenLimitExceeded)

	fireT(t, sched)

	state := scheduleState(t, sched, defID)
	if state.LastStatus != "skipped" {
		t.Errorf("last_status = %q, want skipped", state.LastStatus)
	}
	if state.FireCount != 1 {
		t.Errorf("fire_count = %d, want 1 — a budget refusal counts toward max_fires", state.FireCount)
	}
	if n := channelMessageCount(t, sched.store, "passes"); n != 0 {
		t.Errorf("on_complete published %d message(s) for a run that never happened", n)
	}
}

// TestFireOne_PausedIsNotCountedAsFire: a fire the runtime pause refused (the
// pause began after the tick's own check) did no work, so it neither reads
// 'failed' nor uses up one of the schedule's max_fires.
//
// Fails-before: last_status failed, fire_count 1.
func TestFireOne_PausedIsNotCountedAsFire(t *testing.T) {
	sched, fr, _, defID, _ := schedulerFixture(t, channelHookDef("passes"), time.Now().Add(-time.Minute))
	fr.runErr = fmt.Errorf("admit: %w", runner.ErrRuntimePaused)

	fireT(t, sched)

	state := scheduleState(t, sched, defID)
	if state.LastStatus != "skipped" {
		t.Errorf("last_status = %q, want skipped", state.LastStatus)
	}
	if state.FireCount != 0 {
		t.Errorf("fire_count = %d, want 0 — a paused fire did no work", state.FireCount)
	}
}

// TestFanout_ExplicitConcurrencyOverridesCodeJSSerialisation: a code-agent
// consolidator is serial by default because its own provider id says nothing
// about where its children's load lands. The comment and docs promised that
// setting LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY lifts that, but the serial
// branch forced 1 whatever it was set to. An explicit setting now applies; a
// local runtime still serializes, since no setting makes one box parallel.
//
// Fails-before: peak concurrency is 1 for code-js with the setting at 4.
func TestFanout_ExplicitConcurrencyOverridesCodeJSSerialisation(t *testing.T) {
	for _, tc := range []struct {
		provider   string
		wantSerial bool
	}{
		{"code-js", false},
		{"ollama-local", true},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), func(c *Config) {
				c.MaxConsolidationConcurrency = 4
			})
			sched.SetProviderResolver(stubProviderResolver{provider: tc.provider})
			for _, u := range []string{"u1", "u2", "u3", "u4"} {
				seedSettledSession(t, st, "", u)
			}
			probe := &concurrencyProbe{hold: 40 * time.Millisecond}
			fr.onRun = func(runner.RunInput) { probe.enter() }

			fireT(t, sched)

			if got := len(fr.Calls()); got != 4 {
				t.Fatalf("RunOnce calls = %d, want 4; logs:\n%s", got, logs.all())
			}
			peak := probe.Peak()
			if tc.wantSerial {
				if peak != 1 {
					t.Errorf("peak = %d against %q, want 1 — an explicit width must not parallelize a local runtime", peak, tc.provider)
				}
				return
			}
			if peak < 2 {
				t.Errorf("peak = %d against %q with LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY=4 set, want > 1; logs:\n%s", peak, tc.provider, logs.all())
			}
			if !logs.contains("set explicitly") {
				t.Errorf("lifting the serial default must be logged; logs:\n%s", logs.all())
			}
		})
	}
}

// TestTargetBudget_IsAFixedFractionOfTheFireBudget pins the slice: the whole
// budget for one target, an even split up to fanoutBudgetSlices, and never
// smaller than that however many targets there are.
func TestTargetBudget_IsAFixedFractionOfTheFireBudget(t *testing.T) {
	const fire = 10 * time.Minute
	for _, tc := range []struct {
		targets int
		want    time.Duration
	}{
		{0, fire},
		{1, fire},
		{2, fire / 2},
		{4, fire / 4},
		{32, fire / 4},
	} {
		if got := targetBudget(fire, tc.targets); got != tc.want {
			t.Errorf("targetBudget(%s, %d) = %s, want %s", fire, tc.targets, got, tc.want)
		}
	}
}

// TestClassifyFire_SharedLadder pins the classes both fire paths rely on, wrapped
// the way the runner wraps them.
func TestClassifyFire_SharedLadder(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status string
		counts bool
	}{
		{nil, "completed", true},
		{errors.New("boom"), "failed", true},
		{runner.ErrUnknownAgent, "failed", false},
		{runner.ErrBackpressure, "skipped", true},
		{runner.ErrPerUserQuotaExhausted, "skipped", true},
		{runner.ErrProviderConcurrencyExhausted, "skipped", true},
		{runner.ErrTokenLimitExceeded, "skipped", true},
		{resolve.ErrOperatorKeyRestricted, "skipped", true},
		{providers.ErrOperatorKeyForbidden, "skipped", true},
		{runner.ErrRuntimePaused, "skipped", false},
	} {
		err := tc.err
		if err != nil {
			err = fmt.Errorf("wrapped: %w", err)
		}
		c := classifyFire(err)
		if c.status() != tc.status || c.countsAsFire() != tc.counts {
			t.Errorf("classifyFire(%v) → status %q counts %v, want %q %v", err, c.status(), c.countsAsFire(), tc.status, tc.counts)
		}
	}
}
