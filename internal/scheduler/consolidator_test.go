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
	rows, err := st.ConsolidatableSessions(ctx, "", "alice", "", time.Time{}, "", 10)
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
		sched.SetFanoutCoordination(lock, 12345)
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
		sched.SetFanoutCoordination(lock, 12345)
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
		sched.SetFanoutCoordination(&stubLock{err: errors.New("pool exhausted")}, 12345)
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
