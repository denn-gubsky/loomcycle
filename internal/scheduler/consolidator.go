package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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
	// fanoutMetadataKey is the schedule-def metadata marker that turns a schedule
	// into a fan-out. Config, not a hardcoded agent name, so an operator can point
	// their own agent at it.
	//
	// NOT operator-only, despite the shape. ScheduleDef create/fork accepts
	// Metadata wholesale, so any principal with schedule_def_scopes — including a
	// runtime meta-agent — can set this key, turning one def into up to
	// MaxConsolidationTargets runs per tick, each executing under a DISCOVERED
	// user's identity. There is no discriminator for "materialised from static
	// yaml" (a bootstrapped static schedule and a runtime-authored one produce
	// identical substrate rows, and a fork of a static name is indistinguishable
	// from either), so honouring the marker only for yaml-sourced defs is not
	// something this layer can implement today. What it does instead is refuse to
	// be silent: every fan-out fire logs that it is fanning out and how wide,
	// so a def nobody meant to author is visible in the log rather than only in
	// the bill. Closing the gap properly needs a provenance column on
	// schedule_defs — deferred, tracked as the follow-up to this note.
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
	//
	// KNOWN GAP (deferred): the window can be STARVED. ListSessions orders
	// `pinned DESC, last_activity DESC`, so pinned sessions occupy the front
	// regardless of age, and an empty TenantID filter means "all tenants" at the
	// store layer — so a shared-tenant schedule draws its 500 rows across every
	// tenant's sessions. On a large deployment a target with new work can sit
	// outside the window and never be enumerated. A per-tenant paged scan (or a
	// dedicated distinct-scope-with-work query) is the fix; deferred.
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
// The marker lives in the def's metadata — see fanoutMetadataKey for why that is
// NOT the same as operator-authored.
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

	// One line per fan-out fire, deliberately. The marker is reachable by anything
	// holding schedule_def_scopes (see fanoutMetadataKey), and it multiplies one
	// def into many runs under discovered user identities — the single most
	// expensive metadata key in the system. An operator must be able to find out
	// from the log that a def is fanning out, and how wide, without waiting for
	// the bill. At an hourly cadence this is one line per hour per def.
	s.logf("scheduler: schedule %q (def %s) carries the consolidation fan-out marker — dispatching up to %d run(s) per tick, each under a discovered user's identity; retire the def if you did not author this",
		row.Name, row.DefID, s.cfg.MaxConsolidationTargets)

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
	// The exclusion is pushed into the QUERY rather than applied to the result:
	// the scan window is a fixed 500 rows ordered most-recently-active first, and
	// a pass's own children are by construction the most recent sessions there
	// are. Post-filtering would leave a window full of extractor sessions and
	// starve every real target out of it — the fan-out's existing
	// window-starvation gap, made acute by the thing this exclusion exists for.
	sessions, _, err := s.store.ListSessions(ctx, store.SessionFilter{
		TenantID:      def.TenantID,
		ExcludeAgents: s.excludedAgents(def.Agent),
	}, candidateScanLimit, 0)
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

// excludedAgents is the set of agent names whose sessions the fan-out must look
// past: the schedule's own agent plus every agent declared `internal:`.
//
// Self-exclusion alone was never enough. Each pass creates a session under the
// target's user id, and a pass never consolidates itself, so those sessions sit
// past the watermark forever — but so do its CHILDREN's. A pass spawns one
// extractor run per chat it reads, each child's session transcript contains the
// chat it was extracting, and on the next tick those became candidates in their
// own right: on the live store 7 of the last 8 chats were extractor sessions,
// growing ~15 a pass, with every pass re-extracting nested copies of its own
// input. Sorted so a caller logging the set gets a stable order.
func (s *Scheduler) excludedAgents(selfAgent string) []string {
	out := make([]string, 0, len(s.cfg.InternalAgents)+1)
	seen := map[string]bool{}
	for _, n := range append(append([]string{}, s.cfg.InternalAgents...), selfAgent) {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// targetHasNewWork reports whether this target has anything to consolidate:
// either a settled session past its watermark, or an un-drained queue item.
// Both are cheap point reads with limit 1 — the fan-out must not pay for the
// batch it is only deciding whether to dispatch.
//
// The session probe looks past the schedule's own agent AND every internal one
// — see excludedAgents for why one name was not enough. Without the exclusion
// every target reports new work on every tick and the schedule becomes a
// perpetual pass consuming its own output.
func (s *Scheduler) targetHasNewWork(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, selfAgent string) (bool, error) {
	cursor, err := s.store.MemoryCursorGet(ctx, tenantID, scope, scopeID)
	if err != nil {
		return false, fmt.Errorf("cursor get: %w", err)
	}
	sessions, err := s.store.ConsolidatableSessions(ctx, tenantID, scopeID, "", s.excludedAgents(selfAgent), cursor.WatermarkCompletedAt, cursor.WatermarkSessionID, 1)
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
//
// A SYNTHETIC provider is that same "cannot be resolved" case wearing a
// resolvable name. An in-process provider (code-js, mock) makes no external
// model call at all, so its id says nothing about where this batch's model load
// will actually land — for a code agent that load is entirely in the sub-agents
// it spawns, which this probe cannot see. Answering "code-js is not local,
// therefore parallel" is answering a question nobody asked. So it serializes,
// on the same reasoning as an unresolvable provider.
//
// KNOWN GAPS (both deferred):
//
//  1. The synthetic check is a CONSERVATIVE STAND-IN, not the fix. The honest
//     fix is a probe that follows the spawn tree — resolve the providers the
//     scheduled agent's children would use and decide on those — which needs a
//     way to enumerate reachable sub-agents from a def and is its own change.
//     Until then a code-agent orchestrator whose children are all cloud-hosted
//     is serialized unnecessarily; that costs throughput, where the inverse
//     error costs an operator's GPU box. LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY
//     is the escape hatch for anyone who knows their children are parallel-safe.
//  2. The probe resolves with the operator-key restriction OFF while the fire
//     passes the def's actual restriction bit (see
//     (*http.Server).ResolveAgentProvider). With
//     LOOMCYCLE_OPERATOR_KEY_RESTRICTION on and a restricted def, the probe can
//     answer "anthropic" while the children re-resolve to ollama-local — a batch
//     judged parallel-safe then lands N-wide on the local box.
func (s *Scheduler) dispatchSerially(ctx context.Context, def scheduleDef, targets []consolidationTarget) (bool, string) {
	if s.providerResolver == nil {
		return true, "no provider resolver wired — defaulting to serial"
	}
	for _, target := range targets {
		providerID, err := s.providerResolver.ResolveAgentProvider(ctx, target.TenantID, target.UserID, def.Agent, def.UserTier)
		if err != nil {
			return true, fmt.Sprintf("provider for agent %q could not be resolved (%v) — defaulting to serial", def.Agent, err)
		}
		// Checked BEFORE the local test so the operator gets the specific
		// reason: "this probe cannot see where the load goes" is actionable,
		// "provider is not local" would not have been.
		if isSyntheticProvider(providerID) {
			return true, fmt.Sprintf(
				"agent %q resolves to the in-process provider %q, which makes no model call itself — this batch's real model load is in sub-agents this probe cannot see, so parallel-safety is unknown. Set LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY if you know those children are not all on one box",
				def.Agent, providerID)
		}
		if isLocalProvider(providerID) {
			return true, fmt.Sprintf("provider %q is a local runtime", providerID)
		}
	}
	return false, ""
}

// isSyntheticProvider reports whether a provider id names an IN-PROCESS
// provider that never calls an external model: the synthetic code provider and
// the load-test mocks. For these the resolved id carries no information about
// the batch's real model load, which lives in whatever sub-agents the run
// spawns.
//
// Matched by exact id rather than by a naming convention, deliberately — unlike
// "local", synthetic-ness has no established convention in the config, and
// inventing one here would be a rule operators have never been told about. The
// cost is the mirror of isLocalProvider's gap: an operator who declares their
// own provider on the `code-js` or `mock` DRIVER under a different id reads as
// remote and dispatches in parallel. Same proper fix as that gap — a declared
// flag on the `providers:` entry instead of the scheduler guessing from a
// string.
func isSyntheticProvider(providerID string) bool {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "code-js", "mock", "mock-stable":
		return true
	}
	return false
}

// isLocalProvider reports whether a provider id names a runtime on the
// operator's own hardware. There is no capability flag for this — "local" is a
// provider-ID NAMING CONVENTION in the config (`ollama-local`), so the
// convention is what we match: the exact id, plus the `-local` suffix / `local-`
// prefix forms an operator may use for their own registrations.
//
// KNOWN GAP (deferred): name-matching FALSE-NEGATIVES real local runtimes that do
// not follow the convention — `localai`, `lmstudio`, `vllm`, or a config-declared
// `homebox` on the ollama driver all read as remote and get dispatched in
// parallel at one box. The proper fix is an explicit `local: true` on the
// `providers:` config entry, so the operator declares it instead of the scheduler
// guessing from a string; that is a config-schema change and is deferred.
// LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY is the escape hatch until then.
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
//
// This is also where the pass's telemetry is emitted — see observePass for why
// here and not inside the run.
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

	// One loomcycle.memory.consolidate span per pass. The run's ctx is the SPAN's
	// ctx, so the run's own loomcycle.run / provider.call spans nest underneath —
	// tokens, model, and per-attempt latency stay sourced from where they are
	// already authoritative.
	ctx, span := lcotel.RecordMemoryConsolidate(ctx, lcotel.MemoryConsolidateAttrs{
		Scope:     string(target.Scope),
		ScopeID:   target.UserID,
		AgentName: def.Agent,
		Tier:      def.UserTier,
	})
	defer span.End()
	before := s.observePass(ctx, target, def.Agent, span.IsRecording())

	var runID string
	var usage struct {
		provider string
		model    string
	}
	var usageMu sync.Mutex
	cb := runner.RunCallbacks{
		OnRegistered: func(_, id, _, _ string) { runID = id },
		// The loop populates Usage.Provider/Model with the identity that ACTUALLY
		// served the call — tryProviderFallback mutates it in place — so reading
		// them here is drift-free, unlike re-resolving at the dispatcher. OnEvent
		// may fire from the loop's goroutine, hence the mutex.
		OnEvent: func(ev providers.Event) {
			if ev.Usage == nil {
				return
			}
			usageMu.Lock()
			defer usageMu.Unlock()
			if ev.Usage.Provider != "" {
				usage.provider = ev.Usage.Provider
			}
			if ev.Usage.Model != "" {
				usage.model = ev.Usage.Model
			}
		},
	}
	runErr := s.runner.RunOnce(ctx, in, cb)

	if span.IsRecording() {
		after := s.observePass(ctx, target, def.Agent, true)
		usageMu.Lock()
		provider, model := usage.provider, usage.model
		usageMu.Unlock()
		lcotel.SetMemoryConsolidateResult(span, before.diff(after, provider, model, runErr))
	}
	return runID, runErr
}

// consolidateObserveCap bounds the observation window. A target's memory scope is
// already quota-bounded to a handful of summary keys, so this is only the floor
// that keeps a pathological scope from turning telemetry into a table walk — and
// when it trips the counts are omitted rather than under-reported.
const consolidateObserveCap = 500

// passObservation is the store state a pass is measured against. Two of these —
// one before, one after — are how the runtime learns what a pass actually DID.
//
// ATTRIBUTION CAVEAT: the diff is of the whole target scope across the run
// window, so it is what CHANGED during the pass, not provably what the pass
// changed. Any other writer to that scope while the pass runs — a second
// concurrent pass, or the user's own chat agent doing `Memory set` under
// scope: user — lands in these counts. There is no per-writer attribution in the
// memory table to key off, and the alternative (trusting the pass's prose report)
// is worse.
//
// WHY OBSERVE THE STORE RATHER THAN READ THE PASS'S REPORT. The pass reports its
// own added/updated/superseded counts in prose, and it is an LLM: parsing that
// report into metrics would make the operator's dashboard a measure of the
// model's phrasing, and a pass that silently wrote nothing while claiming
// success would look healthy. Row sets, cursor position, and queue depth are
// facts. So the counts here are a diff of store state, and the two outcomes the
// runtime genuinely cannot see (a duplicate the pass chose to merge; a fact it
// chose not to store) are documented as such on the attribute keys rather than
// given counters that would have to be invented.
type passObservation struct {
	// keys maps live memory key -> its updated_at, for the add/update/supersede
	// diff. nil when unobserved.
	keys map[string]time.Time
	// sessionsPastWatermark is the backlog the pass was handed.
	sessionsPastWatermark int
	// pendingUndrained is the queue depth.
	pendingUndrained int
	// watermark is the cursor's position; zero when never advanced.
	watermark time.Time

	// EVERY read has its own known-bit, and a value with a false bit is OMITTED
	// from the span rather than emitted as 0. This is not defensive padding: each
	// of these counters has a benign-looking zero (nothing added, nothing
	// drained, no lag), so a pass whose reads failed — the batch budget expiring
	// mid-pass is the ordinary way that happens — would otherwise render as a
	// perfectly healthy one. keysKnown additionally covers TRUNCATION, since a
	// partial key set produces a plausible-but-wrong diff.
	keysKnown      bool
	sessionsKnown  bool
	pendingKnown   bool
	watermarkKnown bool

	// observed is false when telemetry is off, so diff produces a zero outcome.
	observed bool
}

// observePass reads the target's consolidation-visible state. It is SKIPPED
// entirely when the span is not recording: with OTEL unconfigured the tracer is
// a no-op, and paying for store reads to feed a no-op would tax every operator
// who never enabled tracing. Read faults degrade to a partial observation rather
// than failing the pass — telemetry must never break the work it measures.
func (s *Scheduler) observePass(ctx context.Context, target consolidationTarget, selfAgent string, recording bool) passObservation {
	if !recording {
		return passObservation{}
	}
	obs := passObservation{keys: map[string]time.Time{}, observed: true}

	entries, truncated, err := s.store.MemoryList(ctx, target.TenantID, target.Scope, target.UserID, "", consolidateObserveCap)
	if err == nil && !truncated {
		obs.keysKnown = true
		for _, e := range entries {
			obs.keys[e.Key] = e.UpdatedAt
		}
	}

	cursor, err := s.store.MemoryCursorGet(ctx, target.TenantID, target.Scope, target.UserID)
	if err == nil {
		obs.watermarkKnown = true
		obs.watermark = cursor.WatermarkCompletedAt
	}
	// The session scan is SKIPPED when the watermark read failed, because
	// MemoryCursorGet returns a ZERO row on error and a zero watermark means "from
	// the beginning of time": scanning against it would count the target's entire
	// history — up to the observe cap — and report it as backlog with
	// sessionsKnown=true. That is the fabricated-zero bug inverted: unknown
	// rendered as maximally alarming instead of as healthy, and equally wrong.
	//
	// The same exclusion set the dispatcher's has-new-work probe uses — the
	// schedule's own agent AND every internal one. Each pass creates its own
	// settled session under the target's user id, and one per extractor child,
	// and never consolidates any of them, so they sit past the watermark forever:
	// counting them would make sessions_read climb on every tick and turn the
	// backlog gauge into a child counter. The gauge has to measure the same set
	// the scan does or it reports a backlog no pass will ever work through.
	if obs.watermarkKnown {
		sessions, serr := s.store.ConsolidatableSessions(ctx, target.TenantID, target.UserID, "", s.excludedAgents(selfAgent),
			obs.watermark, cursor.WatermarkSessionID, consolidateObserveCap)
		if serr == nil {
			obs.sessionsKnown = true
			obs.sessionsPastWatermark = len(sessions)
		}
	}
	// pending_drain is a READ (the ack is the side effect), so peeking here does
	// not consume the queue the pass is about to work on.
	pending, err := s.store.MemoryPendingDrain(ctx, target.TenantID, target.Scope, target.UserID, consolidateObserveCap)
	if err == nil {
		obs.pendingKnown = true
		obs.pendingUndrained = len(pending)
	}
	return obs
}

// diff turns a before/after pair into the outcome the span carries.
//
// SessionsRead comes from the BEFORE observation (the backlog the pass was
// handed), while the lag comes from the AFTER one (how far behind it still is) —
// the two answer different operator questions and taking both from one side
// would make one of them useless.
func (before passObservation) diff(after passObservation, provider, model string, err error) lcotel.ConsolidateOutcome {
	out := lcotel.ConsolidateOutcome{
		SessionsRead:      before.sessionsPastWatermark,
		SessionsReadKnown: before.observed && before.sessionsKnown,
		Provider:          provider,
		Model:             model,
		Err:               err,
	}
	if !before.observed || !after.observed {
		// CountsTruncated is what SUPPRESSES added/updated/superseded/noop on the
		// span. Returning here without it would emit added=0, updated=0,
		// superseded=0, noop=true — the fabricated-healthy-pass shape this whole
		// known-bit scheme exists to prevent. Unreachable today (the caller only
		// diffs a recording pair), which is exactly why it is set explicitly.
		out.CountsTruncated = true
		return out
	}
	if before.pendingKnown && after.pendingKnown {
		out.PendingDrainedKnown = true
		// A negative delta means the target ENQUEUED more during the pass than the
		// pass acked, which is not a drain; clamp so the counter cannot read as a
		// negative drain.
		if drained := before.pendingUndrained - after.pendingUndrained; drained > 0 {
			out.PendingDrained = drained
		}
	}
	// A zero watermark means the target has NEVER consolidated, which is not a
	// lag of zero — see AttrConsolidateWatermarkLagMs.
	if after.watermarkKnown && !after.watermark.IsZero() {
		if lag := time.Since(after.watermark); lag > 0 {
			out.WatermarkLagKnown = true
			out.WatermarkLag = lag
		}
	}
	if !before.keysKnown || !after.keysKnown {
		out.CountsTruncated = true
		return out
	}
	for key, updatedAt := range after.keys {
		wasAt, existed := before.keys[key]
		switch {
		case !existed:
			out.Added++
		case updatedAt.After(wasAt):
			out.Updated++
		}
	}
	for key := range before.keys {
		if _, still := after.keys[key]; !still {
			out.Superseded++
		}
	}
	return out
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
