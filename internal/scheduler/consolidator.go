package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC BL P2 — the consolidation fan-out.
//
// A normal schedule fires ONE run. A consolidation schedule instead has to
// visit every memory TARGET that has unconsolidated work: the pass operates on
// exactly one target (the Memory tool resolves `scope: user` server-side from
// the run's user id), so "consolidate everything" means N runs, not one run
// that loops. This file is that dispatcher.
//
// It deliberately reuses the fire path's runner: each child goes through
// s.runner.RunOnce, so it inherits token-budget admission, per-user quota,
// per-provider concurrency, the pause gate, and usage/cost attribution for
// free. Nothing here re-implements any of that.
//
// Scope note: the fan-out enumerates USER targets only. `scope: agent` resolves
// server-side to the CONSOLIDATOR's own agent name, so an agent-scope run can
// only ever consolidate its own bookkeeping — dispatching per "agent target"
// would silently point every run at the same scope. The consolidator still
// declares memory_scopes: [agent, user] for its own use; only `user` fans out.

const (
	// fanoutMetadataKey is the operator-authored marker (schedule def metadata,
	// never inbound) that turns a schedule into a fan-out. Config, not a
	// hardcoded agent name, so an operator can point their own agent at it.
	fanoutMetadataKey = "memory_consolidation_fanout"
	// fanoutScopeKey optionally names the target scope. Only "user" is
	// supported (see the scope note above); an empty value defaults to it.
	fanoutScopeKey = "memory_consolidation_scope"

	// defaultMaxFanoutTargets / defaultMaxFanoutConcurrency back the
	// Config.MaxConsolidation* knobs (see their field docs for the rationale).
	// Both are operator-tunable; these are only the fall-throughs.
	defaultMaxFanoutTargets     = 32
	defaultMaxFanoutConcurrency = 4
	// candidateScanLimit bounds the session scan that discovers candidate
	// targets. Sessions come back most-recently-active first, so the scan
	// window always contains the targets with new work.
	candidateScanLimit = 500
)

// ProviderResolver reports the provider id a run of this agent would resolve
// to right now. Declared here (rather than importing the HTTP server) to keep
// internal/scheduler free of that dependency; (*http.Server) satisfies it.
//
// The fan-out needs it for ONE decision: whether the dispatch target is a local
// runtime, which must not be hit in parallel.
type ProviderResolver interface {
	ResolveAgentProvider(ctx context.Context, tenantID, userID, agentName, userTier string) (string, error)
}

// AdvisoryLocker is the minimum surface the fan-out needs from
// internal/coord.AdvisoryLock, mirroring internal/retention's declaration so
// the scheduler stays free of the coord import. *coord.AdvisoryLock satisfies
// it implicitly.
type AdvisoryLocker interface {
	TryRun(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error)
}

// SetFanoutCoordination wires the cluster singleton gate for the consolidation
// fan-out. Without it (single-replica, or a sqlite deployment) the fan-out runs
// unguarded, which is correct for one replica. A no-op-safe setter rather than a
// New parameter so existing New(...) call sites stay unchanged, mirroring
// SetChannelScope. Must be called before Start.
//
// lockKeyFn derives the key from the SCHEDULE DEF id — per-def, not one
// process-wide constant. Consolidation schedules fan out and an operator will have
// several (typically one per tenant); on a shared key two defs due in the same
// tick collide and the loser is skip-but-advanced, silently forfeiting its whole
// cadence. A nil lockKeyFn disables the gate along with a nil lock.
func (s *Scheduler) SetFanoutCoordination(lock AdvisoryLocker, lockKeyFn func(defID string) int64) {
	s.fanoutLock = lock
	s.fanoutLockKeyFn = lockKeyFn
}

// SetProviderResolver wires the provider resolution the fan-out uses to decide
// parallel-vs-serial. Nil (the default) means "cannot resolve" — and the
// fan-out then dispatches SERIALLY, because hammering an unknown backend is the
// worse failure. Must be called before Start.
func (s *Scheduler) SetProviderResolver(r ProviderResolver) { s.providerResolver = r }

// consolidationTarget is one fan-out destination: a (tenant, scope, scope_id)
// memory target. UserID is the scope_id for the only supported scope, and it is
// what the dispatched run carries as its identity so the Memory tool's
// server-side scope resolution lands on this target.
type consolidationTarget struct {
	TenantID string
	Scope    store.MemoryScope
	UserID   string
}

// isConsolidationFanout reports whether this schedule dispatches per-target.
// The marker lives in the def's operator-authored metadata, so it is trusted
// config — never inbound, never model-supplied.
func isConsolidationFanout(def scheduleDef) bool {
	v, ok := def.Metadata[fanoutMetadataKey]
	if !ok {
		return false
	}
	// YAML/JSON round-trips a bool as bool; accept the string spellings too so
	// a hand-edited substrate def does not silently disable the fan-out.
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	default:
		return false
	}
}

// fanoutScope returns the target scope for this schedule. Only `user` is
// supported; anything else (including an explicit `agent`) is refused so a
// misconfigured scope fails loudly instead of pointing every dispatched run at
// the consolidator's own agent scope.
func fanoutScope(def scheduleDef) (store.MemoryScope, error) {
	raw, _ := def.Metadata[fanoutScopeKey].(string)
	switch strings.TrimSpace(raw) {
	case "", string(store.MemoryScopeUser):
		return store.MemoryScopeUser, nil
	default:
		return "", fmt.Errorf("%s=%q is not supported (only %q fans out; the agent scope resolves to the consolidator's own name)",
			fanoutScopeKey, raw, store.MemoryScopeUser)
	}
}

// fireConsolidationFanout is fireOne's per-target twin. It enumerates the
// targets with new work, dispatches one child run each, and records ONE result
// for the schedule — so the schedule's next_run_at, fire count, and
// on_complete hooks behave exactly as they do for a single-run fire.
//
// The whole batch shares fireOne's per-fire budget (cfg.FireTimeout), so a
// consolidation schedule never consumes more wall-clock than any other fire and
// can never wedge the tick. Targets left undispatched when the budget runs out
// are picked up next tick — the per-target watermark makes that resumable.
func (s *Scheduler) fireConsolidationFanout(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time) {
	scope, err := fanoutScope(def)
	if err != nil {
		s.recordFireFailure(ctx, row.DefID, "", "failed", fmt.Errorf("consolidation fan-out: %w", err), now)
		return
	}

	batchCtx, cancel := context.WithTimeout(ctx, s.cfg.FireTimeout)
	defer cancel()

	// Cluster singleton: without this every replica would dispatch a full
	// fan-out in the same tick and burn N× the tokens before the per-target
	// leases sorted it out. TryRun's error is infra-only (the work function
	// swallows its own failures), so a lock fault skips this tick rather than
	// marking the schedule failed.
	dispatch := func(ctx context.Context) {
		s.dispatchConsolidationTargets(ctx, row, def, scope, now)
	}
	if s.fanoutLock != nil && s.fanoutLockKeyFn != nil {
		acquired, lockErr := s.fanoutLock.TryRun(batchCtx, s.fanoutLockKeyFn(row.DefID), func(ctx context.Context) error {
			dispatch(ctx)
			return nil
		})
		// `acquired` is checked FIRST. When it is true the work body already ran
		// and already wrote this schedule's result, so reacting to a (future)
		// non-nil closure error here would overwrite a finished outcome with a
		// skip. Today the closure cannot fail; the ordering is what keeps that
		// from becoming a bug the next time someone gives it a return value.
		if acquired {
			return
		}
		if lockErr != nil {
			s.logf("scheduler: consolidation fan-out %q advisory lock infra error: %v — skipping this tick", row.Name, lockErr)
			s.advanceOnly(ctx, row.DefID, def, "skipped", now)
			return
		}
		// Another replica owns this tick. Skip-but-advance so the row does not
		// re-present every tick on this replica.
		s.advanceOnly(ctx, row.DefID, def, "skipped", now)
		return
	}
	dispatch(batchCtx)
}

// dispatchConsolidationTargets is the fan-out body, run at most once per tick
// per cluster. It records the schedule's result itself so the advisory-lock
// wrapper stays a thin gate.
func (s *Scheduler) dispatchConsolidationTargets(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, scope store.MemoryScope, now time.Time) {
	targets, dropped, err := s.consolidationTargets(ctx, def, scope)
	if err != nil {
		s.recordFireFailure(ctx, row.DefID, "", "failed", fmt.Errorf("consolidation fan-out: enumerate targets: %w", err), now)
		return
	}
	if dropped > 0 {
		// A silent truncation reads as "everything was covered". The watermark
		// makes the remainder resumable, so this is deferral, not loss — but the
		// operator needs to see it to widen the cap or the cadence.
		s.logf("scheduler: consolidation fan-out %q capped at %d targets — %d target(s) with new work deferred to the next tick",
			row.Name, len(targets), dropped)
	}
	if len(targets) == 0 {
		// Skip-but-advance: an idle deployment must cost nothing. No run, no
		// fire counted, no hooks.
		s.advanceOnly(ctx, row.DefID, def, "skipped_no_targets", now)
		return
	}

	serial, reason := s.dispatchSerially(ctx, def, targets)
	concurrency := s.cfg.MaxConsolidationConcurrency
	if serial {
		concurrency = 1
		s.logf("scheduler: consolidation fan-out %q running SERIALLY over %d target(s): %s", row.Name, len(targets), reason)
	}

	var (
		mu        sync.Mutex
		lastRunID string
		tally     fanoutTally
		skipped   int
	)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		// The batch budget is the stop condition: a serial run over many
		// targets can exhaust it, and the remainder waits for the next tick.
		if ctx.Err() != nil {
			mu.Lock()
			skipped++
			mu.Unlock()
			continue
		}
		select {
		case <-ctx.Done():
			mu.Lock()
			skipped++
			mu.Unlock()
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(target consolidationTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			runID, runErr := s.runConsolidationTarget(ctx, def, target)
			mu.Lock()
			defer mu.Unlock()
			tally.dispatched++
			if runID != "" {
				lastRunID = runID
			}
			if runErr != nil {
				tally.classify(runErr)
				// Per-target failures are logged and counted, never fatal to
				// the batch: one user's wedged consolidation must not stop
				// everyone else's.
				s.logf("scheduler: consolidation fan-out %q target (tenant=%q user=%q): %v",
					row.Name, target.TenantID, target.UserID, runErr)
			}
		}(target)
	}
	wg.Wait()

	if skipped > 0 {
		s.logf("scheduler: consolidation fan-out %q ran out of its %s budget — %d target(s) not dispatched this tick",
			row.Name, s.cfg.FireTimeout, skipped)
	}

	status, errStr, countAsFire := tally.outcome(def.Agent)
	if tally.unknownAgent > 0 && !countAsFire {
		// F38, mirrored: agent resolution failed for EVERY target, so no run ever
		// started. That is one config error repeating, not N fires — counting it
		// would burn max_fires and retire the schedule, hiding the misconfig
		// behind a retired def. Log it as loudly as fireOne does.
		s.logf("scheduler: consolidation fan-out %q could not resolve agent %q in tenant %q for any of %d target(s) — not counting toward max_fires; check the agent exists in this tenant (F38)",
			row.Name, def.Agent, def.TenantID, tally.dispatched)
	}
	s.recordFanoutResult(ctx, row, def, now, status, errStr, lastRunID, countAsFire)
	if status == "completed" {
		s.dispatchHooks(ctx, row.Name, def, lastRunID, "")
	}
}

// fanoutTally classifies per-target outcomes the way fireOne classifies a single
// fire. Without this the fan-out labelled every error "failed" and counted every
// tick as a fire, so it lost two behaviours fireOne has deliberately:
//
//   - runner.ErrUnknownAgent is a CONFIG error — no run started, and it will fail
//     identically on every fire. fireOne does not count it toward max_fires (F38)
//     because doing so retires the schedule after N ticks and presents a misconfig
//     as N normal runs.
//   - the backpressure family (ErrBackpressure, ErrPerUserQuotaExhausted,
//     ErrProviderConcurrencyExhausted) is transient LOAD, not failure. fireOne
//     labels it "skipped"; a saturated provider must not read as a broken
//     schedule, and must not be summed into a failure count an operator alerts on.
type fanoutTally struct {
	dispatched   int
	failures     int // genuine per-target failures
	backpressure int // transient load — deferred, not broken
	unknownAgent int // config error — no run started
}

// classify buckets one per-target error through the same errors.Is ladder fireOne
// uses, so the two paths cannot drift.
func (t *fanoutTally) classify(err error) {
	switch {
	case errors.Is(err, runner.ErrUnknownAgent):
		t.unknownAgent++
	case errors.Is(err, runner.ErrBackpressure),
		errors.Is(err, runner.ErrPerUserQuotaExhausted),
		errors.Is(err, runner.ErrProviderConcurrencyExhausted):
		t.backpressure++
	default:
		t.failures++
	}
}

// outcome renders the schedule's status, error summary, and whether this tick
// counts toward max_fires.
//
// countAsFire is false only when EVERY dispatched target failed agent resolution
// — the whole tick was one config error. A tick where some targets ran is a real
// fire regardless of what the others did.
func (t fanoutTally) outcome(agent string) (status, errStr string, countAsFire bool) {
	countAsFire = !(t.unknownAgent > 0 && t.unknownAgent == t.dispatched)
	broken := t.failures + t.unknownAgent
	switch {
	case broken > 0:
		status = "failed"
		errStr = fmt.Sprintf("%d of %d consolidation target(s) failed", broken, t.dispatched)
		if t.unknownAgent > 0 {
			errStr += fmt.Sprintf(" (%d could not resolve agent %q)", t.unknownAgent, agent)
		}
		if t.backpressure > 0 {
			errStr += fmt.Sprintf("; %d deferred under load", t.backpressure)
		}
	case t.backpressure > 0:
		// Nothing broke — the batch was throttled. Deliberately not "failed", so
		// this does not page anyone, and not "completed", so on_complete hooks do
		// not fire for a batch that largely did not run.
		status = "skipped"
		errStr = fmt.Sprintf("%d of %d consolidation target(s) deferred under load", t.backpressure, t.dispatched)
	default:
		status = "completed"
	}
	return status, errStr, countAsFire
}

// consolidationTargets enumerates the targets with unconsolidated work, plus a
// count of targets that had work but did not fit the cap.
//
// Candidates come from the session list (most-recently-active first) rather than
// from ConsolidatableSessions directly: that query is ascending from the
// beginning of time, so a large already-consolidated backlog would fill the scan
// window and permanently starve newly-active targets. Each candidate is then
// confirmed against its OWN watermark before it earns a dispatch.
func (s *Scheduler) consolidationTargets(ctx context.Context, def scheduleDef, scope store.MemoryScope) ([]consolidationTarget, int, error) {
	sessions, _, err := s.store.ListSessions(ctx, store.SessionFilter{TenantID: def.TenantID}, candidateScanLimit, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("list sessions: %w", err)
	}

	// Distinct candidate scope ids, in first-seen (most-recently-active) order
	// so the cap below trims the least-recently-active candidates.
	seen := map[string]bool{}
	var candidates []string
	for _, sess := range sessions {
		// An empty TenantID filter means "all tenants" at the store layer, so
		// re-assert the def's authoritative tenant here: a fan-out must never
		// dispatch a run for a session outside the tenant the def declares.
		// This is the FIRST of two layers — targetHasNewWork's reads are also
		// tenant-filtered, so a cross-tenant candidate would report no work
		// anyway. Filtering here saves the pointless round-trip and keeps the
		// confinement visible at the place the target list is built.
		if sess.TenantID != def.TenantID {
			continue
		}
		if sess.UserID == "" {
			continue // no user id ⇒ no user-scope memory target
		}
		if sess.Agent == def.Agent {
			// The consolidator's own past runs. They are settled sessions under
			// the target's user id, so counting them as a candidate signal would
			// keep a fully-consolidated target permanently "active".
			continue
		}
		if seen[sess.UserID] {
			continue
		}
		seen[sess.UserID] = true
		candidates = append(candidates, sess.UserID)
	}

	maxTargets := s.cfg.MaxConsolidationTargets
	var targets []consolidationTarget
	dropped := 0
	for _, userID := range candidates {
		hasWork, err := s.targetHasNewWork(ctx, def.TenantID, scope, userID, def.Agent)
		if err != nil {
			// A per-candidate read fault must not abort the whole fan-out;
			// log it and let the next tick retry that candidate.
			s.logf("scheduler: consolidation fan-out: check target (tenant=%q user=%q): %v", def.TenantID, userID, err)
			continue
		}
		if !hasWork {
			continue
		}
		if len(targets) >= maxTargets {
			dropped++
			continue
		}
		targets = append(targets, consolidationTarget{TenantID: def.TenantID, Scope: scope, UserID: userID})
	}
	// Stable order so a capped fan-out is reproducible and testable.
	sort.Slice(targets, func(i, j int) bool { return targets[i].UserID < targets[j].UserID })
	return targets, dropped, nil
}

// targetHasNewWork reports whether this target has anything to consolidate:
// either a settled session past its watermark, or an un-drained queue item.
// Both are cheap point reads with limit 1 — the fan-out must not pay for the
// batch it is only deciding whether to dispatch.
//
// selfAgent is the consolidator's OWN name, excluded from the session probe.
// Each pass creates a session under the target's user id, and a pass never
// consolidates itself, so those sessions sit past the watermark forever: without
// the exclusion every target reports new work on every tick and the schedule
// becomes a perpetual pass that only ever consolidates its own reports.
func (s *Scheduler) targetHasNewWork(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, selfAgent string) (bool, error) {
	cursor, err := s.store.MemoryCursorGet(ctx, tenantID, scope, scopeID)
	if err != nil {
		return false, fmt.Errorf("cursor get: %w", err)
	}
	sessions, err := s.store.ConsolidatableSessions(ctx, tenantID, scopeID, "", selfAgent, cursor.WatermarkCompletedAt, cursor.WatermarkSessionID, 1)
	if err != nil {
		return false, fmt.Errorf("consolidatable sessions: %w", err)
	}
	if len(sessions) > 0 {
		return true, nil
	}
	// pending_drain is a READ (the ack is the side effect), so peeking one row
	// here does not consume it.
	pending, err := s.store.MemoryPendingDrain(ctx, tenantID, scope, scopeID, 1)
	if err != nil {
		return false, fmt.Errorf("pending drain: %w", err)
	}
	return len(pending) > 0, nil
}

// dispatchSerially decides whether the batch runs one-at-a-time, and why.
//
// A LOCAL model runtime is a single shared box: firing four concurrent runs at
// it queues them behind one another at best and thrashes VRAM at worst. So any
// target resolving to a local provider serializes the whole batch — as does a
// target whose provider cannot be resolved at all, because dispatching an
// unknown volume of parallel work at an unknown backend is the worse failure.
func (s *Scheduler) dispatchSerially(ctx context.Context, def scheduleDef, targets []consolidationTarget) (bool, string) {
	if s.providerResolver == nil {
		return true, "no provider resolver wired — defaulting to serial"
	}
	for _, target := range targets {
		providerID, err := s.providerResolver.ResolveAgentProvider(ctx, target.TenantID, target.UserID, def.Agent, def.UserTier)
		if err != nil {
			return true, fmt.Sprintf("provider for agent %q could not be resolved (%v) — defaulting to serial", def.Agent, err)
		}
		if isLocalProvider(providerID) {
			return true, fmt.Sprintf("provider %q is a local runtime", providerID)
		}
	}
	return false, ""
}

// isLocalProvider reports whether a provider id names a runtime on the
// operator's own hardware. There is no capability flag for this — "local" is a
// provider-ID NAMING CONVENTION in the config (`ollama-local`), so the
// convention is what we match: the exact id, plus the `-local` suffix / `local-`
// prefix forms an operator may use for their own registrations.
func isLocalProvider(providerID string) bool {
	id := strings.ToLower(strings.TrimSpace(providerID))
	if id == "" {
		return false
	}
	return id == "ollama-local" || strings.HasSuffix(id, "-local") || strings.HasPrefix(id, "local-")
}

// runConsolidationTarget dispatches ONE target's pass and returns its run id.
//
// The run's identity IS the target: UserID is what the Memory tool's
// server-side `scope: user` resolution keys off, so setting it here is what
// points the pass at this target and nothing else. The def's own user_id is
// deliberately overridden.
func (s *Scheduler) runConsolidationTarget(ctx context.Context, def scheduleDef, target consolidationTarget) (string, error) {
	in := buildRunInput(def, s.cfg.EnvAllowlist, s.logf)
	in.UserID = target.UserID
	in.TenantID = target.TenantID
	// Copy the metadata before adding to it: def.Metadata is shared across
	// every child of this fan-out, and mutating it would leak one target's
	// context into the next.
	meta := make(map[string]any, len(in.Metadata)+1)
	for k, v := range in.Metadata {
		meta[k] = v
	}
	meta[fanoutScopeKey] = string(target.Scope)
	in.Metadata = meta

	var runID string
	cb := runner.RunCallbacks{
		OnRegistered: func(_, id, _, _ string) { runID = id },
	}
	return runID, s.runner.RunOnce(ctx, in, cb)
}

// recordFanoutResult writes the schedule's outcome + next_run_at, mirroring
// fireOne's bookkeeping (including the survival ctx for a mid-shutdown write
// and the max_fires retirement check).
//
// countAsFire comes from the caller's outcome classification rather than being
// hardcoded true: an all-targets-unresolved tick must not consume the max_fires
// budget (F38).
func (s *Scheduler) recordFanoutResult(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time, status, errStr, runID string, countAsFire bool) {
	next, nextErr := s.computeNext(def, now)
	if nextErr != nil {
		s.logf("scheduler: schedule %q cron-resolve failed: %v — parking 1h", row.Name, nextErr)
		next = now.Add(1 * time.Hour)
	}
	recordCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		recordCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := s.store.ScheduleRunStateRecordResult(recordCtx, store.ScheduleRunResult{
		DefID:       row.DefID,
		LastRunID:   runID,
		LastStatus:  status,
		LastError:   errStr,
		LastRunAt:   now,
		NextRunAt:   next,
		CountAsFire: countAsFire,
	}); err != nil {
		s.logf("scheduler: record fan-out result for %q: %v", row.Name, err)
	}
	if def.MaxFires > 0 {
		if st, gerr := s.store.ScheduleRunStateGet(recordCtx, row.DefID); gerr != nil {
			s.logf("scheduler: max_fires read state for %q: %v", row.Name, gerr)
		} else if st.FireCount >= def.MaxFires {
			if rerr := s.store.ScheduleDefSetRetired(recordCtx, row.DefID, true); rerr != nil {
				s.logf("scheduler: max_fires retire %q (def %s) after %d fires: %v", row.Name, row.DefID, st.FireCount, rerr)
			} else {
				s.logf("scheduler: %q reached max_fires=%d — retired def %s", row.Name, def.MaxFires, row.DefID)
			}
		}
	}
}
