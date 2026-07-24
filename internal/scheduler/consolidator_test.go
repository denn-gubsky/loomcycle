package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubProviderResolver reports a fixed provider for every agent, or an error.
type stubProviderResolver struct {
	provider string
	err      error
}

func (s stubProviderResolver) ResolveAgentProvider(_ context.Context, _, _, _, _ string) (string, error) {
	return s.provider, s.err
}

// stubLock mimics coord.AdvisoryLock: acquire controls whether the work runs,
// err simulates an infra fault. Records how many times the work actually ran.
type stubLock struct {
	acquire bool
	err     error
	ran     atomic.Int32
}

func (l *stubLock) TryRun(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if l.err != nil {
		return false, l.err
	}
	if !l.acquire {
		return false, nil
	}
	l.ran.Add(1)
	return true, fn(ctx)
}

// concurrencyProbe wraps a fakeRunner's onRun to record the HIGH-WATER mark of
// simultaneously in-flight runs. The sleep is what makes overlap observable: a
// serialized dispatch can never exceed 1 no matter how many targets there are.
type concurrencyProbe struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	hold     time.Duration
}

func (p *concurrencyProbe) enter() {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()
	time.Sleep(p.hold)
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
}

func (p *concurrencyProbe) Peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// fanoutDef is the schedule def a consolidation fan-out fires from: the marker
// in operator-authored metadata plus a cron.
func fanoutDef(extraMeta map[string]any) scheduleDef {
	enabled := true
	meta := map[string]any{fanoutMetadataKey: true}
	for k, v := range extraMeta {
		meta[k] = v
	}
	return scheduleDef{
		Agent:    "memory/consolidator",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		Metadata: meta,
	}
}

// seedSettledSession creates a session for userID with one COMPLETED run, so
// the store reports it as consolidatable (all runs terminal).
func seedSettledSession(t *testing.T, st store.Store, tenantID, userID string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenantID, "chat", userID)
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", userID, err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a-" + sess.ID, UserID: userID})
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", userID, err)
	}
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatalf("FinishRun(%s): %v", userID, err)
	}
	return sess.ID
}

// fanoutFixture builds a scheduler whose due schedule is a fan-out, with a
// capturing logger so cap/serial decisions can be asserted.
func fanoutFixture(t *testing.T, def scheduleDef, cfgMut func(*Config)) (*Scheduler, *fakeRunner, store.Store, *logCapture) {
	t.Helper()
	logs := &logCapture{}
	sched, fr, _, _, st := schedulerFixture(t, def, time.Now().Add(-time.Minute))
	if cfgMut != nil {
		cfg := sched.cfg
		cfgMut(&cfg)
		sched.cfg = cfg.defaults()
	}
	sched.logf = logs.logf
	return sched, fr, st, logs
}

// logCapture records the scheduler's log lines for assertion.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCapture) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (l *logCapture) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// TestFanout_DispatchesOneRunPerTargetWithItsIdentity is the fan-out's reason to
// exist. A consolidation pass operates on exactly ONE memory target (the Memory
// tool resolves scope=user server-side from the run's user id), so a single
// blanket run could only ever consolidate one user — or, with no user on the
// run, nobody. Each dispatched child must therefore carry its target's identity.
//
// Fails-before without the fan-out: fireOne dispatches one run with the def's
// own (empty) user_id.
func TestFanout_DispatchesOneRunPerTargetWithItsIdentity(t *testing.T) {
	sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	seedSettledSession(t, st, "", "alice")
	seedSettledSession(t, st, "", "bob")

	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 2 {
		t.Fatalf("RunOnce calls = %d, want 2 (one per target); calls=%+v", len(calls), calls)
	}
	got := map[string]bool{}
	for _, c := range calls {
		got[c.UserID] = true
		if c.Agent != "memory/consolidator" {
			t.Errorf("child agent = %q, want memory/consolidator", c.Agent)
		}
	}
	for _, want := range []string{"alice", "bob"} {
		if !got[want] {
			t.Errorf("no child run carried user_id %q — its memory target would never be consolidated (got %v)", want, got)
		}
	}
}

// TestFanout_SkipsTargetsWithNoNewWork: a target whose watermark is already past
// its newest settled session has nothing to consolidate, and dispatching a run
// for it would spend a full LLM call to discover that. The idle case must be
// free — this is what makes an hourly cadence affordable.
//
// Fails-before if the dispatcher enumerates candidates without consulting each
// one's cursor: alice would get a run despite having no new sessions.
func TestFanout_SkipsTargetsWithNoNewWork(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	seedSettledSession(t, st, "", "alice")
	seedSettledSession(t, st, "", "bob")

	ctx := context.Background()
	// Advance alice's watermark past everything she has: she is fully caught up.
	rows, err := st.ConsolidatableSessions(ctx, "", "alice", "", "", time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ConsolidatableSessions: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("alice should have a settled session to consolidate")
	}
	last := rows[len(rows)-1]
	if _, _, err := st.MemoryCursorLease(ctx, "", store.MemoryScopeUser, "alice", "owner", time.Now(), time.Minute); err != nil {
		t.Fatalf("lease alice: %v", err)
	}
	if err := st.MemoryCursorAdvance(ctx, "", store.MemoryScopeUser, "alice", "owner", last.MaxCompletedAt, last.SessionID); err != nil {
		t.Fatalf("advance alice: %v", err)
	}
	if err := st.MemoryCursorRelease(ctx, "", store.MemoryScopeUser, "alice", "owner"); err != nil {
		t.Fatalf("release alice: %v", err)
	}

	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 1 {
		t.Fatalf("RunOnce calls = %d, want 1 (only bob has new work); calls=%+v\nlogs:\n%s", len(calls), calls, logs.all())
	}
	if calls[0].UserID != "bob" {
		t.Errorf("dispatched target = %q, want bob (alice is caught up)", calls[0].UserID)
	}
}

// TestFanout_NoTargetsIsFreeAndAdvances: with nothing to consolidate anywhere,
// the tick must dispatch zero runs AND still advance next_run_at — otherwise the
// due row re-presents on every single tick.
func TestFanout_NoTargetsIsFreeAndAdvances(t *testing.T) {
	sched, fr, _, _ := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("RunOnce calls = %d, want 0 — an idle deployment must cost nothing", got)
	}
	st, err := sched.store.ScheduleRunStateGet(context.Background(), "sd-test")
	if err != nil {
		t.Fatalf("ScheduleRunStateGet: %v", err)
	}
	if !st.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v, want a future time — an unadvanced row re-fires every tick", st.NextRunAt)
	}
	if st.FireCount != 0 {
		t.Errorf("fire_count = %d, want 0 — a no-target tick is not a fire", st.FireCount)
	}
}

// TestFanout_SerialForLocalModel is the local-hardware guard. A local model
// runtime is ONE shared box: four concurrent consolidation runs queue behind
// each other at best and thrash VRAM at worst. So a target resolving to a local
// provider forces concurrency 1 for the whole batch, while a cloud provider
// dispatches in parallel.
//
// Fails-before without the local check: both cases would show a peak > 1.
func TestFanout_SerialForLocalModel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   string
		wantSerial bool
	}{
		{"local ollama serializes", "ollama-local", true},
		{"operator-named local suffix serializes", "vllm-local", true},
		{"cloud provider parallelizes", "anthropic", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					t.Errorf("peak concurrency = %d against provider %q, want 1 — a local runtime must not be hit in parallel", peak, tc.provider)
				}
				if !logs.contains("SERIALLY") {
					t.Errorf("the serial decision must be logged; logs:\n%s", logs.all())
				}
			} else if peak < 2 {
				t.Errorf("peak concurrency = %d against provider %q, want > 1 — a cloud provider should fan out in parallel", peak, tc.provider)
			}
		})
	}
}

// TestFanout_SerialWhenProviderCannotBeResolved: an unresolvable provider (or no
// resolver wired at all) must fall back to SERIAL, not parallel. Dispatching an
// unknown volume of concurrent work at an unknown backend is the worse failure,
// and the reason has to be logged or the degradation is invisible.
func TestFanout_SerialWhenProviderCannotBeResolved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver ProviderResolver
		wantLog  string
	}{
		{"resolver returns an error", stubProviderResolver{err: errors.New("unknown agent")}, "could not be resolved"},
		{"no resolver wired", nil, "no provider resolver wired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), func(c *Config) {
				c.MaxConsolidationConcurrency = 4
			})
			if tc.resolver != nil {
				sched.SetProviderResolver(tc.resolver)
			}
			for _, u := range []string{"u1", "u2", "u3"} {
				seedSettledSession(t, st, "", u)
			}
			probe := &concurrencyProbe{hold: 40 * time.Millisecond}
			fr.onRun = func(runner.RunInput) { probe.enter() }

			fireT(t, sched)

			if got := len(fr.Calls()); got != 3 {
				t.Fatalf("RunOnce calls = %d, want 3; logs:\n%s", got, logs.all())
			}
			if peak := probe.Peak(); peak != 1 {
				t.Errorf("peak concurrency = %d, want 1 — an unresolvable provider must default to serial", peak)
			}
			if !logs.contains(tc.wantLog) {
				t.Errorf("missing the reason for the serial fallback (%q); logs:\n%s", tc.wantLog, logs.all())
			}
		})
	}
}

// TestFanout_LogsCappedTargets: the per-tick target cap defers work rather than
// dropping it (the watermark makes it resumable), but a SILENT truncation reads
// as "every target was covered" — the operator would never know to widen the cap
// or the cadence. The drop count must appear in the log.
//
// Fails-before if the cap is applied with a bare `break`/`continue` and no log.
func TestFanout_LogsCappedTargets(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), func(c *Config) {
		c.MaxConsolidationTargets = 2
	})
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	for _, u := range []string{"u1", "u2", "u3", "u4", "u5"} {
		seedSettledSession(t, st, "", u)
	}

	fireT(t, sched)

	if got := len(fr.Calls()); got != 2 {
		t.Fatalf("RunOnce calls = %d, want 2 (the cap); logs:\n%s", got, logs.all())
	}
	if !logs.contains("capped at 2 targets") {
		t.Errorf("the cap must be logged; logs:\n%s", logs.all())
	}
	if !logs.contains("3 target(s) with new work deferred") {
		t.Errorf("the log must report HOW MANY targets were deferred; logs:\n%s", logs.all())
	}
}

// TestFanout_SingleReplicaPerTick: the fan-out is gated by an advisory lock so
// exactly one replica per tick enumerates targets and dispatches. Without it
// every replica dispatches a FULL fan-out in the same tick and burns N× the
// tokens before the per-target leases sort it out — the leases prevent duplicate
// WRITES, not duplicate spend.
//
// Fails-before if fireConsolidationFanout ignores s.fanoutLock: the
// lock-not-acquired case would still dispatch.
func TestFanout_SingleReplicaPerTick(t *testing.T) {
	t.Run("lock not acquired dispatches nothing", func(t *testing.T) {
		sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
		sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
		lock := &stubLock{acquire: false}
		sched.SetFanoutCoordination(lock, coord.MemoryConsolidatorLockKey)
		seedSettledSession(t, st, "", "alice")

		fireT(t, sched)

		if got := len(fr.Calls()); got != 0 {
			t.Errorf("RunOnce calls = %d, want 0 — another replica owns this tick", got)
		}
		if lock.ran.Load() != 0 {
			t.Errorf("the work body ran %d times despite the lock not being acquired", lock.ran.Load())
		}
		// Skip-but-advance so the row does not re-present every tick here.
		state, err := sched.store.ScheduleRunStateGet(context.Background(), "sd-test")
		if err != nil {
			t.Fatalf("ScheduleRunStateGet: %v", err)
		}
		if !state.NextRunAt.After(time.Now()) {
			t.Errorf("next_run_at = %v, want advanced even when the lock was lost", state.NextRunAt)
		}
	})

	t.Run("lock acquired dispatches once", func(t *testing.T) {
		sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
		sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
		lock := &stubLock{acquire: true}
		sched.SetFanoutCoordination(lock, coord.MemoryConsolidatorLockKey)
		seedSettledSession(t, st, "", "alice")

		fireT(t, sched)

		if got := len(fr.Calls()); got != 1 {
			t.Errorf("RunOnce calls = %d, want 1", got)
		}
		if lock.ran.Load() != 1 {
			t.Errorf("the work body ran %d times, want exactly 1 per tick", lock.ran.Load())
		}
	})

	t.Run("lock infra fault skips the tick without failing the schedule", func(t *testing.T) {
		sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), nil)
		sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
		sched.SetFanoutCoordination(&stubLock{err: errors.New("pool exhausted")}, coord.MemoryConsolidatorLockKey)
		seedSettledSession(t, st, "", "alice")

		fireT(t, sched)

		if got := len(fr.Calls()); got != 0 {
			t.Errorf("RunOnce calls = %d, want 0 on a lock fault", got)
		}
		if !logs.contains("advisory lock infra error") {
			t.Errorf("a lock fault must be logged; logs:\n%s", logs.all())
		}
	})
}

// keyTrackingLock is a stubLock that remembers which keys were taken and never
// hands the SAME key out twice — the behaviour a real advisory lock has while a
// holder is inside it. That is what makes per-key independence observable.
type keyTrackingLock struct {
	mu   sync.Mutex
	held map[int64]bool
	keys []int64
}

func (l *keyTrackingLock) TryRun(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error) {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[int64]bool{}
	}
	if l.held[lockKey] {
		l.mu.Unlock()
		return false, nil
	}
	l.held[lockKey] = true
	l.keys = append(l.keys, lockKey)
	l.mu.Unlock()
	return true, fn(ctx)
}

func (l *keyTrackingLock) taken() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.keys...)
}

// TestFanout_LockKeyIsPerSchedule is the per-tenant starvation regression.
//
// The gate used to take ONE process-wide key for every consolidation schedule. A
// fan-out schedule is exactly the kind an operator has several of — typically one
// per tenant — so two of them due in the same tick collided, and the loser was
// skip-but-advanced: it silently forfeited its entire cadence to whichever def the
// sweeper reached first, forever, with only a "another replica owns this tick" in
// the log to show for it.
//
// Fails-before with a shared key: the second def is refused the lock and
// dispatches nothing.
func TestFanout_LockKeyIsPerSchedule(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, logs := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	lock := &keyTrackingLock{}
	sched.SetFanoutCoordination(lock, coord.MemoryConsolidatorLockKey)
	seedSettledSession(t, st, "", "alice")

	ctx := context.Background()
	now := time.Now()
	// Two DISTINCT schedule defs, both fan-outs, both due in this tick.
	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-tenant-a", Name: "consol-a"}, def, now)
	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-tenant-b", Name: "consol-b"}, def, now)

	keys := lock.taken()
	if len(keys) != 2 {
		t.Fatalf("distinct schedule defs acquired %d lock(s), want 2 — one def is starving the other; logs:\n%s", len(keys), logs.all())
	}
	if keys[0] == keys[1] {
		t.Errorf("both defs hashed to the same lock key %d — the key must be derived per def", keys[0])
	}
	if got := len(fr.Calls()); got != 2 {
		t.Errorf("RunOnce calls = %d, want 2 (both schedules dispatched alice's pass); logs:\n%s", got, logs.all())
	}

	// The SAME def twice still admits exactly one holder — the cluster singleton
	// property the gate exists for is intact.
	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-tenant-a", Name: "consol-a"}, def, now)
	if got := len(lock.taken()); got != 2 {
		t.Errorf("re-firing the same def acquired the lock again (%d total) — two replicas would both dispatch", got)
	}
}

// TestMemoryConsolidatorLockKey_StableAndDistinct pins the derivation itself: the
// key must be stable for a def id (so a re-fire collides with a concurrent holder
// on another replica) and different across def ids (so schedules do not starve
// each other).
func TestMemoryConsolidatorLockKey_StableAndDistinct(t *testing.T) {
	a := coord.MemoryConsolidatorLockKey("sd-a")
	if a != coord.MemoryConsolidatorLockKey("sd-a") {
		t.Error("the key for one def id is not stable — a second replica would not collide with the holder")
	}
	if a == coord.MemoryConsolidatorLockKey("sd-b") {
		t.Error("two def ids hash to the same key — one schedule would starve the other")
	}
	if a == 0 {
		t.Error("key is 0 — an all-zero key would collide with an unkeyed caller")
	}
}

// seedConsolidatorRun creates a settled session AUTHORED BY the consolidator
// itself for userID — exactly what a completed pass leaves behind.
func seedConsolidatorRun(t *testing.T, st store.Store, tenantID, userID, agentName string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenantID, agentName, userID)
	if err != nil {
		t.Fatalf("CreateSession(self %s): %v", userID, err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "self-" + sess.ID, UserID: userID})
	if err != nil {
		t.Fatalf("CreateRun(self %s): %v", userID, err)
	}
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatalf("FinishRun(self %s): %v", userID, err)
	}
	return sess.ID
}

// TestFanout_IgnoresItsOwnPastRuns is the self-consolidation-loop regression.
//
// Every pass creates a SESSION under the target's own user id (that is how
// scope=user resolves), and a pass never consolidates itself — so its session
// sits past the watermark forever. Without excluding self-authored sessions,
// a fully caught-up target reports new work on every single tick: the schedule
// becomes a perpetual pass whose only input is its own previous reports,
// compounding cost and polluting memory with the consolidator's own output.
//
// This is the exact loop the end-to-end pipeline test surfaced. Fails-before if
// the has-new-work probe (or the candidate scan) counts self-authored sessions.
func TestFanout_IgnoresItsOwnPastRuns(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, logs := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	// alice has a real chat, consolidated up to date, plus several settled
	// consolidator runs of her own — the steady state after a few passes.
	seedSettledSession(t, st, "", "alice")
	ctx := context.Background()
	rows, err := st.ConsolidatableSessions(ctx, "", "alice", "", def.Agent, time.Time{}, "", 10)
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
	for i := 0; i < 3; i++ {
		seedConsolidatorRun(t, st, "", "alice", def.Agent)
	}

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Errorf("RunOnce calls = %d, want 0 — a caught-up target's OWN past passes must not count as new work (this is the perpetual-pass loop); logs:\n%s", got, logs.all())
	}
}

// TestFanout_SelfRunsDoNotMakeATargetACandidate: the same exclusion has to hold
// at candidate ENUMERATION too. A user whose only sessions are consolidator runs
// is not a consolidation target at all — otherwise the fan-out pays a store probe
// per tick for a user who never chatted.
func TestFanout_SelfRunsDoNotMakeATargetACandidate(t *testing.T) {
	def := fanoutDef(nil)
	sched, fr, st, _ := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	seedConsolidatorRun(t, st, "", "ghost", def.Agent) // never chatted, only consolidated

	fireT(t, sched)

	for _, c := range fr.Calls() {
		if c.UserID == "ghost" {
			t.Error("dispatched a pass for a user whose only sessions are the consolidator's own runs")
		}
	}
}

// TestFanout_RefusesUnsupportedScope: scope=agent resolves server-side to the
// CONSOLIDATOR's own agent name, so fanning out over "agent targets" would point
// every dispatched run at the same scope while looking like it covered many.
// That has to fail loudly at dispatch, not silently misbehave.
func TestFanout_RefusesUnsupportedScope(t *testing.T) {
	def := fanoutDef(map[string]any{fanoutScopeKey: "agent"})
	sched, fr, st, logs := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", "alice")

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Errorf("RunOnce calls = %d, want 0 for an unsupported fan-out scope", got)
	}
	if !logs.contains("is not supported") {
		t.Errorf("the refusal must be logged; logs:\n%s", logs.all())
	}
}

// TestFanout_ConfinesTargetsToTheDefsTenant: the def's tenant is the authority
// for which tenant a fired run executes as, and an EMPTY tenant filter means
// "all tenants" at the store layer — so a shared-tenant schedule must not
// dispatch runs for another tenant's users. This asserts the OUTCOME, which two
// layers enforce independently (the candidate scan's per-session tenant check
// and the tenant-filtered has-new-work reads); removing either one alone leaves
// this green, which is the point of having both.
func TestFanout_ConfinesTargetsToTheDefsTenant(t *testing.T) {
	def := fanoutDef(nil)
	def.TenantID = "" // the shared/legacy tenant
	sched, fr, st, _ := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	seedSettledSession(t, st, "", "shared-user")
	seedSettledSession(t, st, "acme", "acme-user")

	fireT(t, sched)

	for _, c := range fr.Calls() {
		if c.UserID == "acme-user" {
			t.Errorf("dispatched a run for tenant acme's user from a shared-tenant schedule — the tenant filter leaked")
		}
		if c.TenantID != "" {
			t.Errorf("child TenantID = %q, want the def's tenant \"\"", c.TenantID)
		}
	}
	if got := len(fr.Calls()); got != 1 {
		t.Errorf("RunOnce calls = %d, want 1 (only the shared-tenant user)", got)
	}
}

// TestFanout_ChildMetadataIsNotSharedAcrossTargets: every child is built from
// the SAME def, so a dispatcher that adds per-target context to def.Metadata
// in place would leak one target's context into the next (and race while doing
// it). Each child must get its own map.
func TestFanout_ChildMetadataIsNotSharedAcrossTargets(t *testing.T) {
	sched, fr, st, _ := fanoutFixture(t, fanoutDef(map[string]any{"operator_note": "keep"}), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", "alice")
	seedSettledSession(t, st, "", "bob")

	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 2 {
		t.Fatalf("RunOnce calls = %d, want 2", len(calls))
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.Metadata["operator_note"] != "keep" {
			t.Errorf("child lost the def's operator metadata: %+v", c.Metadata)
		}
		if got := fmt.Sprint(c.Metadata[fanoutScopeKey]); got != string(store.MemoryScopeUser) {
			t.Errorf("child %s scope metadata = %q, want user", c.UserID, got)
		}
		seen[fmt.Sprintf("%p", c.Metadata)] = true
	}
	if len(seen) != 2 {
		t.Error("both children share ONE metadata map — per-target context would leak between them")
	}
}

// TestFanout_PerTargetFailureDoesNotStopTheBatch: one user's wedged
// consolidation must not stop everyone else's. The schedule records failed (so
// the operator sees it) but every target still got its dispatch.
func TestFanout_PerTargetFailureDoesNotStopTheBatch(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	for _, u := range []string{"u1", "u2", "u3"} {
		seedSettledSession(t, st, "", u)
	}
	fr.runErr = errors.New("provider exploded")

	fireT(t, sched)

	if got := len(fr.Calls()); got != 3 {
		t.Fatalf("RunOnce calls = %d, want 3 — a failing target must not abort the batch; logs:\n%s", got, logs.all())
	}
	state, err := sched.store.ScheduleRunStateGet(context.Background(), "sd-test")
	if err != nil {
		t.Fatalf("ScheduleRunStateGet: %v", err)
	}
	if state.LastStatus != "failed" {
		t.Errorf("last_status = %q, want failed (3 of 3 targets failed)", state.LastStatus)
	}
	if !strings.Contains(state.LastError, "3 of 3") {
		t.Errorf("last_error = %q, want the failed/total count", state.LastError)
	}
}

// TestFanout_UnresolvedAgentDoesNotBurnMaxFires is the F38 regression, in the
// fan-out. recordFanoutResult set CountAsFire unconditionally, so a schedule whose
// agent cannot be resolved in its tenant burned one fire per tick and retired
// itself after max_fires — presenting a pure config error as N normal runs, with
// the def retired so the operator's next look shows a finished schedule rather
// than a broken one. fireOne has refused to count this since F38; the fan-out did
// not inherit that.
//
// Fails-before with CountAsFire: true — fire_count reaches 1 and the log says
// nothing about resolution.
func TestFanout_UnresolvedAgentDoesNotBurnMaxFires(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", "alice")
	seedSettledSession(t, st, "", "bob")
	fr.runErr = fmt.Errorf("dispatch: %w", runner.ErrUnknownAgent)

	fireT(t, sched)

	state, err := sched.store.ScheduleRunStateGet(context.Background(), "sd-test")
	if err != nil {
		t.Fatalf("ScheduleRunStateGet: %v", err)
	}
	if state.FireCount != 0 {
		t.Errorf("fire_count = %d, want 0 — an all-targets-unresolved tick is one config error, not a fire (it would retire the schedule)", state.FireCount)
	}
	if state.LastStatus != "failed" {
		t.Errorf("last_status = %q, want failed — the operator still has to see it", state.LastStatus)
	}
	if !strings.Contains(state.LastError, "could not resolve agent") {
		t.Errorf("last_error = %q, want it to name agent resolution rather than a generic failure", state.LastError)
	}
	if !logs.contains("not counting toward max_fires") {
		t.Errorf("the F38 exception must be logged loudly; logs:\n%s", logs.all())
	}
}

// TestFanout_BackpressureIsSkippedNotFailed: the backpressure family is transient
// LOAD, not failure. fireOne labels each of these "skipped" so a saturated
// provider does not read as a broken schedule; the fan-out labelled every
// per-target error "failed" and summed them into a failure count an operator would
// alert on. It also must not fire on_complete hooks — the batch largely did not
// run.
//
// Fails-before: status is "failed" for all three sentinels.
func TestFanout_BackpressureIsSkippedNotFailed(t *testing.T) {
	for _, sentinel := range []error{
		runner.ErrBackpressure,
		runner.ErrPerUserQuotaExhausted,
		runner.ErrProviderConcurrencyExhausted,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
			sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
			seedSettledSession(t, st, "", "alice")
			fr.runErr = fmt.Errorf("admit: %w", sentinel)

			fireT(t, sched)

			state, err := sched.store.ScheduleRunStateGet(context.Background(), "sd-test")
			if err != nil {
				t.Fatalf("ScheduleRunStateGet: %v", err)
			}
			if state.LastStatus != "skipped" {
				t.Errorf("last_status = %q for %v, want skipped — transient load is not a broken schedule", state.LastStatus, sentinel)
			}
			if !strings.Contains(state.LastError, "deferred under load") {
				t.Errorf("last_error = %q, want it to read as deferral rather than failure", state.LastError)
			}
			// A throttled batch still counts as a fire (a run was attempted), the
			// same way fireOne counts a "skipped" fire.
			if state.FireCount != 1 {
				t.Errorf("fire_count = %d, want 1 — the tick did attempt work", state.FireCount)
			}
		})
	}
}

// TestFanout_MixedOutcomesSeparateFailureFromLoad: with one genuine failure and
// one throttled target, the summary must not present both as failures — an
// operator sizing an incident needs the split.
func TestFanout_MixedOutcomesSeparateFailureFromLoad(t *testing.T) {
	tally := fanoutTally{dispatched: 4, failures: 1, backpressure: 2, unknownAgent: 1}
	status, errStr, countAsFire := tally.outcome("memory/consolidator")
	if status != "failed" {
		t.Errorf("status = %q, want failed (a genuine failure is present)", status)
	}
	if !countAsFire {
		t.Error("countAsFire = false, but some targets ran — only an ALL-unresolved tick is exempt")
	}
	if !strings.Contains(errStr, "2 of 4 consolidation target(s) failed") {
		t.Errorf("errStr = %q, want the broken count (failures + unresolved) out of dispatched", errStr)
	}
	if !strings.Contains(errStr, "1 could not resolve agent") {
		t.Errorf("errStr = %q, want the agent-resolution class called out", errStr)
	}
	if !strings.Contains(errStr, "2 deferred under load") {
		t.Errorf("errStr = %q, want throttled targets counted SEPARATELY from failures", errStr)
	}
}

// TestIsConsolidationFanout_MarkerForms: the marker round-trips through YAML and
// then through the substrate's JSON, and a hand-edited def may spell it as a
// string. A def WITHOUT the marker must keep the ordinary single-run path.
func TestIsConsolidationFanout_MarkerForms(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want bool
	}{
		{nil, false},
		{map[string]any{}, false},
		{map[string]any{fanoutMetadataKey: true}, true},
		{map[string]any{fanoutMetadataKey: false}, false},
		{map[string]any{fanoutMetadataKey: "true"}, true},
		{map[string]any{fanoutMetadataKey: "1"}, true},
		{map[string]any{fanoutMetadataKey: "no"}, false},
		{map[string]any{fanoutMetadataKey: 1}, false}, // an int is not a spelling we accept
	}
	for _, tc := range cases {
		if got := isConsolidationFanout(scheduleDef{Metadata: tc.meta}); got != tc.want {
			t.Errorf("isConsolidationFanout(%v) = %v, want %v", tc.meta, got, tc.want)
		}
	}
}

// TestFanout_NonFanoutScheduleKeepsTheSingleRunPath: the seam in fireOne must be
// inert for every existing schedule — a def with no marker fires exactly once,
// with the def's own identity, as it always did.
func TestFanout_NonFanoutScheduleKeepsTheSingleRunPath(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled, UserID: "u-static"}
	sched, fr, _, _, st := schedulerFixture(t, def, time.Now().Add(-time.Minute))
	seedSettledSession(t, st, "", "alice") // would be a fan-out target if the marker were set

	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 1 {
		t.Fatalf("RunOnce calls = %d, want 1 for an ordinary schedule", len(calls))
	}
	if calls[0].UserID != "u-static" {
		t.Errorf("user_id = %q, want the def's own u-static", calls[0].UserID)
	}
}

// TestIsLocalProvider_NamingConvention pins the convention the serial decision
// keys off. There is no capability flag for "local" — it is a provider-ID naming
// convention in the config, so a change here changes real dispatch behaviour.
func TestIsLocalProvider_NamingConvention(t *testing.T) {
	local := []string{"ollama-local", "OLLAMA-LOCAL", " ollama-local ", "vllm-local", "local-llamacpp"}
	remote := []string{"", "anthropic", "openai", "deepseek", "ollama", "gemini", "localish", "mock"}
	for _, id := range local {
		if !isLocalProvider(id) {
			t.Errorf("isLocalProvider(%q) = false, want true", id)
		}
	}
	for _, id := range remote {
		if isLocalProvider(id) {
			t.Errorf("isLocalProvider(%q) = true, want false", id)
		}
	}
}
