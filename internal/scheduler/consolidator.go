package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
	// user's identity — within the author's own tenant. That is allowed (a
	// tenant may consolidate its own users), so this layer refuses to be silent
	// instead: every fan-out fire logs that it is fanning out and how wide, so a
	// def nobody meant to author is visible in the log rather than only in the
	// bill.
	//
	// Reach follows the def's tenant: a tenant's def fans out within that
	// tenant; a def in the operator layer (tenant "") fans out across every
	// tenant — but only when the OPERATOR wrote it: a row bootstrapped from the
	// yaml, or a version carrying the server-stamped operator_layer bit (written
	// by an admin, or an open-mode / stdio operator). Tenant "" alone is not
	// proof: a config principal with no tenant and no substrate:admin, or an
	// agent in a run that executes in "", writes there too. See
	// operatorLayerFanout.
	fanoutMetadataKey = config.ConsolidationFanoutMetadataKey
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
	return config.IsConsolidationFanout(def.Metadata)
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
// for the schedule — so the schedule's next_run_at and fire count behave
// exactly as they do for a single-run fire. on_complete hooks fire per tenant
// the runs executed in (see dispatchFanoutHooks).
//
// The whole batch shares fireOne's per-fire budget (cfg.FireTimeout), so a
// consolidation schedule never consumes more wall-clock than any other fire and
// can never wedge the tick. Targets left undispatched when the budget runs out
// are picked up next tick — the per-target watermark makes that resumable.
//
// That resumability is only fair if the order changes and no one target can
// spend the whole budget: a cut pass does not advance its watermark, so it is
// re-selected next tick, and in a fixed order it went first again — one slow
// tenant early in the alphabet starved every later tenant on every tick. So
// targets are interleaved across tenants from a start that moves each tick
// (fairTargetOrder), and each pass gets a slice of the budget (targetBudget).
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
	allTenants := s.operatorLayerFanout(row, def)
	reach := fmt.Sprintf("in tenant %q", def.TenantID)
	if allTenants {
		reach = "across every tenant, each run in its target's own tenant"
	}
	s.logf("scheduler: schedule %q (def %s) carries the consolidation fan-out marker — dispatching up to %d run(s) per tick %s, each under a discovered user's identity; retire the def if you did not author this",
		row.Name, row.DefID, s.cfg.MaxConsolidationTargets, reach)

	batchCtx, cancel := context.WithTimeout(ctx, s.cfg.FireTimeout)
	defer cancel()

	// Cluster singleton: without this every replica would dispatch a full
	// fan-out in the same tick and burn N× the tokens before the per-target
	// leases sorted it out. TryRun's error is infra-only (the work function
	// swallows its own failures), so a lock fault skips this tick rather than
	// marking the schedule failed.
	dispatch := func(ctx context.Context) {
		s.dispatchConsolidationTargets(ctx, row, def, scope, allTenants, now)
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

// operatorLayerFanout reports whether this fan-out belongs to the operator
// layer, and so reaches every tenant.
//
// Tenant "" is the operator layer, not a tenant users sign in to: minted
// tokens carry a tenant, and so does the legacy LOOMCYCLE_AUTH_TOKEN
// ("default"), so their runs, sessions and memory never land in "". A
// yaml `scheduled_runs:` entry without `tenant_id` materializes there, and
// confining its fan-out to "" meant it consolidated nobody who signs in — the
// bundled memory-consolidation schedule never reached a legacy-bearer user.
// Reaching every tenant is what an operator-level sweep means, and it grants
// nothing new: the operator can already point one yaml schedule at each tenant
// with `tenant_id`. Each target's run still executes in that target's tenant.
//
// BOTH tenants must be "": the def body's (where its runs execute) and the
// owning row's. (A row a tenant wrote before create stamped the author's tenant
// into the body has an empty body tenant but a real owner; fireOne has already
// re-homed it to that owner, so it arrives here with a tenant.)
//
// And the OPERATOR must have written it, because tenant "" is not proof: a
// config principal with no tenant and no substrate:admin, or an agent in a run
// that executes in "" (a yaml schedule or webhook with no tenant_id, an
// open-mode run), stores its defs there too, and would otherwise run its own
// agent and prompt as every user of every tenant. The operator's are the rows
// bootstrapped from the yaml and the versions the ScheduleDef tool stamped
// operator_layer (written by an admin, or an open-mode / stdio operator). Any
// other "" fan-out stays in "" and says why.
func (s *Scheduler) operatorLayerFanout(row store.ScheduleDueRow, def scheduleDef) bool {
	if def.TenantID != "" || row.OwnerTenantID != "" {
		return false
	}
	if def.OperatorLayer || row.BootstrappedFromStatic {
		return true
	}
	s.logf("scheduler: consolidation fan-out %q (def %s) has no tenant but was not authored with operator authority — confining it to tenant \"\" instead of sweeping every tenant; re-save it as an admin to sweep",
		row.Name, row.DefID)
	return false
}

// dispatchConsolidationTargets is the fan-out body, run at most once per tick
// per cluster. It records the schedule's result itself so the advisory-lock
// wrapper stays a thin gate.
func (s *Scheduler) dispatchConsolidationTargets(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, scope store.MemoryScope, allTenants bool, now time.Time) {
	targets, dropped, err := s.consolidationTargets(ctx, def, scope, allTenants, s.nextFanoutRotation(row.DefID))
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

	if len(def.UserCredentials)+len(def.UserCredentialsFromEnv) > 0 {
		for _, target := range targets {
			if target.TenantID != def.TenantID {
				s.logf("scheduler: consolidation fan-out %q: runs dispatched outside tenant %q carry none of this schedule's credentials — see runConsolidationTarget",
					row.Name, def.TenantID)
				break
			}
		}
	}

	serial, reason := s.dispatchSerially(ctx, def, targets)
	concurrency, _ := s.cfg.consolidationConcurrency()
	if serial {
		concurrency = 1
		s.logf("scheduler: consolidation fan-out %q running SERIALLY over %d target(s): %s", row.Name, len(targets), reason)
	} else if reason != "" {
		s.logf("scheduler: consolidation fan-out %q running up to %d-wide over %d target(s): %s", row.Name, concurrency, len(targets), reason)
	}
	perTarget := targetBudget(s.cfg.FireTimeout, len(targets))

	var (
		mu        sync.Mutex
		lastRunID string
		tally     fanoutTally
		skipped   int
		// paused is set by the first target the runtime pause refused. A
		// pause is runtime-wide, so every later target would be refused the
		// same way: stop dispatching rather than log N copies of one refusal.
		paused        bool
		stoppedPaused int
		// Per tenant as well as overall: hooks fire per tenant (see
		// dispatchFanoutHooks), the schedule's result is recorded overall.
		byTenant = map[string]*tenantBatch{}
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
		// Checked AFTER the slot is held: a serial batch's previous target
		// releases the slot only once it has recorded its outcome, so a pause
		// refusal is always seen before the next dispatch.
		mu.Lock()
		stop := paused
		if stop {
			stoppedPaused++
		}
		mu.Unlock()
		if stop {
			<-sem
			continue
		}
		wg.Add(1)
		go func(target consolidationTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			targetCtx, cancelTarget := context.WithTimeout(ctx, perTarget)
			defer cancelTarget()
			runID, runErr := s.runConsolidationTarget(targetCtx, def, target)
			mu.Lock()
			defer mu.Unlock()
			tb := byTenant[target.TenantID]
			if tb == nil {
				tb = &tenantBatch{}
				byTenant[target.TenantID] = tb
			}
			tally.dispatched++
			tb.tally.dispatched++
			if runID != "" {
				lastRunID = runID
				tb.lastRunID = runID
			}
			class := tally.classify(runErr)
			tb.tally.classify(runErr)
			if class == firePaused {
				paused = true
			}
			if runErr != nil {
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
	if stoppedPaused > 0 {
		s.logf("scheduler: consolidation fan-out %q stopped: the runtime paused mid-sweep — %d target(s) not dispatched this tick, and this tick does not count toward max_fires",
			row.Name, stoppedPaused)
	}

	status, errStr, countAsFire := tally.outcome(def.Agent)
	if tally.unknownAgent > 0 && tally.unknownAgent == tally.dispatched {
		// F38, mirrored: agent resolution failed for EVERY target, so no run ever
		// started. That is one config error repeating, not N fires — counting it
		// would burn max_fires and retire the schedule, hiding the misconfig
		// behind a retired def. Log it as loudly as fireOne does.
		where := fmt.Sprintf("tenant %q", def.TenantID)
		if allTenants {
			where = "any target's tenant"
		}
		s.logf("scheduler: consolidation fan-out %q could not resolve agent %q in %s for any of %d target(s) — not counting toward max_fires; check the agent exists in this tenant (F38)",
			row.Name, def.Agent, where, tally.dispatched)
	}
	s.recordFanoutResult(ctx, row, def, now, status, errStr, lastRunID, countAsFire)
	s.dispatchFanoutHooks(ctx, row.Name, def, byTenant)
}

// tenantBatch is one tenant's share of a fan-out fire: how its targets' runs
// went, and the last run it started.
type tenantBatch struct {
	tally     fanoutTally
	lastRunID string
}

// dispatchFanoutHooks fires the schedule's on_complete hooks once per tenant
// whose runs completed, IN that tenant, reporting that tenant's run.
//
// A hook's writes belong where the run executed. Fired once in the def's layer,
// an operator-layer sweep's hooks wrote into "": a global channel's operator
// layer, which every tenant reads, got a message naming one tenant's run, and a
// memory.set landed in a tenant nobody signs in to. Per tenant is what the
// operator would get from one yaml schedule per tenant with `tenant_id`, which
// is the equivalence the operator-layer sweep rests on (operatorLayerFanout) —
// including that one tenant's failed pass does not withhold another's hooks.
//
// A tenant's fan-out has one tenant, its own, so it fires once, in its tenant.
//
// Which tenants fire is fanoutTally.hooksFire: none of the tenant's passes
// broke, and at least one ran. A DEFERRED pass — load, an exhausted token
// budget, no usable provider key, a pause — does not withhold the tenant's
// hooks: nothing broke, and the passes that did run are what the hooks report.
// One user over budget used to silence their whole tenant's completion.
func (s *Scheduler) dispatchFanoutHooks(ctx context.Context, scheduleName string, def scheduleDef, byTenant map[string]*tenantBatch) {
	if len(def.OnComplete) == 0 {
		return
	}
	tenants := make([]string, 0, len(byTenant))
	for tenant := range byTenant {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	for _, tenant := range tenants {
		b := byTenant[tenant]
		if !b.tally.hooksFire() {
			continue
		}
		hookDef := def
		hookDef.TenantID = tenant
		s.dispatchHooks(ctx, scheduleName, hookDef, b.lastRunID, "")
	}
}

// fanoutTally counts per-target outcomes, each classified by classifyFire — the
// same classifier fireOne uses, so a sentinel one path reads correctly cannot be
// misread by the other. Without it the fan-out labelled every error "failed"
// and counted every tick as a fire.
type fanoutTally struct {
	dispatched   int
	completed    int // ran without error
	failures     int // genuine per-target failures
	backpressure int // transient load — deferred, not broken
	deferred     int // token budget or operator-key restriction — deferred, not broken
	paused       int // refused by a runtime pause — deferred, and the sweep stopped
	unknownAgent int // config error — no run started
}

// classify counts one target's outcome (nil = it ran) and returns its class.
func (t *fanoutTally) classify(err error) fireClass {
	class := classifyFire(err)
	switch class {
	case fireRan:
		t.completed++
	case fireUnknownAgent:
		t.unknownAgent++
	case fireBackpressure:
		t.backpressure++
	case fireDeferred:
		t.deferred++
	case firePaused:
		t.paused++
	default:
		t.failures++
	}
	return class
}

// hooksFire reports whether this tally's on_complete hooks fire: none of its
// passes broke, and at least one ran. See dispatchFanoutHooks.
func (t fanoutTally) hooksFire() bool {
	return t.failures+t.unknownAgent == 0 && t.completed > 0
}

// deferredNotes names each deferral class present, for the error summary. Kept
// separate from the failure count: an operator sizing an incident needs to
// know which targets broke and which only waited.
func (t fanoutTally) deferredNotes() []string {
	var notes []string
	if t.backpressure > 0 {
		notes = append(notes, fmt.Sprintf("%d deferred under load", t.backpressure))
	}
	if t.deferred > 0 {
		notes = append(notes, fmt.Sprintf("%d deferred by a token budget or the operator-key restriction", t.deferred))
	}
	if t.paused > 0 {
		notes = append(notes, fmt.Sprintf("%d deferred by a runtime pause", t.paused))
	}
	return notes
}

// outcome renders the schedule's status, error summary, and whether this tick
// counts toward max_fires.
//
// countAsFire is false in two cases. When EVERY dispatched target failed agent
// resolution, the whole tick was one config error (F38). When the runtime pause
// refused any target, the sweep stopped part-way: the operator paused the
// runtime, so the tick must not use up one of the schedule's fires, and the
// first fire after resume finishes the sweep (targets that already ran have no
// new work left). Otherwise a tick where some targets ran is a real fire
// regardless of what the others did — including a target refused by its token
// budget, which counts (the refusal repeats until the period rolls over).
func (t fanoutTally) outcome(agent string) (status, errStr string, countAsFire bool) {
	countAsFire = t.paused == 0 && !(t.unknownAgent > 0 && t.unknownAgent == t.dispatched)
	broken := t.failures + t.unknownAgent
	notes := t.deferredNotes()
	switch {
	case broken > 0:
		status = "failed"
		errStr = fmt.Sprintf("%d of %d consolidation target(s) failed", broken, t.dispatched)
		if t.unknownAgent > 0 {
			errStr += fmt.Sprintf(" (%d could not resolve agent %q)", t.unknownAgent, agent)
		}
		if len(notes) > 0 {
			errStr += "; " + strings.Join(notes, "; ")
		}
	case len(notes) > 0:
		// Nothing broke — targets waited. Deliberately not "failed", so this
		// does not page anyone, and not "completed", so the summary still
		// says the sweep was not whole. Hooks are decided per tenant.
		status = "skipped"
		errStr = fmt.Sprintf("%s — of %d consolidation target(s) dispatched", strings.Join(notes, "; "), t.dispatched)
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
// window and permanently starve newly-active targets. Targets with queued
// work follow, longest-waiting first. Each candidate is then confirmed against
// its OWN watermark and queue before it earns a dispatch.
//
// allTenants (see operatorLayerFanout) widens the candidate set to every
// tenant; otherwise it is confined to the def's tenant. Either way a target
// carries the tenant its session or queue row lives in, and every read and the
// dispatched run use that tenant — never the def's.
//
// The targets with work are put in fairTargetOrder (rotation picks the starting
// tenant) BEFORE the cap trims them, and are dispatched in that order.
func (s *Scheduler) consolidationTargets(ctx context.Context, def scheduleDef, scope store.MemoryScope, allTenants bool, rotation int) ([]consolidationTarget, int, error) {
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

	// Distinct candidates, each list in first-seen order: sessions most recently
	// active first, queues longest-waiting first, so within a tenant the cap
	// trims the least-recently-active. Keyed by tenant AND user: the same user
	// id in two tenants is two targets. A target in both lists stays in the
	// session list.
	seen := map[consolidationTarget]bool{}
	var fromSessions, fromQueue []consolidationTarget
	addTo := func(list *[]consolidationTarget, tenantID, userID string) {
		c := consolidationTarget{TenantID: tenantID, Scope: scope, UserID: userID}
		if userID == "" || seen[c] {
			return // no user id ⇒ no user-scope memory target
		}
		seen[c] = true
		*list = append(*list, c)
	}
	for _, sess := range sessions {
		// An empty TenantID filter means "all tenants" at the store layer, so
		// unless this is the operator-layer sweep, re-assert the def's
		// authoritative tenant here: a tenant's fan-out must never dispatch a run
		// for a session outside the tenant the def declares. This is the FIRST of
		// two layers — targetHasNewWork reads on the candidate's own tenant, so a
		// candidate from another tenant would find no work there anyway.
		// Filtering here keeps the confinement visible where the list is built.
		if !allTenants && sess.TenantID != def.TenantID {
			continue
		}
		addTo(&fromSessions, sess.TenantID, sess.UserID)
	}

	// Targets whose queue holds work, in their own list. A queue
	// can outlive every session the scan sees: a snapshot restore brings the
	// queue but not the sessions, and a user whose chats have aged out of the
	// scan window can still have rows banked by a compaction. Sessions alone
	// would leave such a queue undrained forever. The read is exact on the
	// def's tenant unless this is the operator-layer sweep, and the candidates
	// still go through targetHasNewWork.
	if allTenants {
		queued, err := s.store.MemoryPendingTargetsAllTenants(ctx, scope, candidateScanLimit)
		if err != nil {
			return nil, 0, fmt.Errorf("list queued targets: %w", err)
		}
		for _, q := range queued {
			addTo(&fromQueue, q.TenantID, q.ScopeID)
		}
	} else {
		queued, err := s.store.MemoryPendingTargets(ctx, def.TenantID, scope, candidateScanLimit)
		if err != nil {
			return nil, 0, fmt.Errorf("list queued targets: %w", err)
		}
		for _, userID := range queued {
			addTo(&fromQueue, def.TenantID, userID)
		}
	}

	withWork := func(candidates []consolidationTarget) []consolidationTarget {
		var out []consolidationTarget
		for _, c := range candidates {
			hasWork, err := s.targetHasNewWork(ctx, c.TenantID, scope, c.UserID, def.Agent)
			if err != nil {
				// A per-candidate read fault must not abort the whole fan-out;
				// log it and let the next tick retry that candidate.
				s.logf("scheduler: consolidation fan-out: check target (tenant=%q user=%q): %v", c.TenantID, c.UserID, err)
				continue
			}
			if hasWork {
				out = append(out, c)
			}
		}
		return out
	}
	ordered := fairTargetOrder(withWork(fromSessions), withWork(fromQueue), rotation)

	maxTargets := s.cfg.MaxConsolidationTargets
	if len(ordered) <= maxTargets {
		return ordered, 0, nil
	}
	return ordered[:maxTargets], len(ordered) - maxTargets, nil
}

// fairTargetOrder is the order a fan-out dispatches in, and so also what its
// cap keeps.
//
// Round-robin across tenants: one target from each tenant, then a second from
// each, and so on. Within a tenant it alternates a session-derived target with
// a queue-derived one. Tenants are taken in sorted order starting at
// rotation (mod the tenant count), which the caller advances every tick.
//
// Each part closes a starvation path the old (tenant, user) sort had:
//   - A cut pass keeps its watermark, so it is selected again next tick. In a
//     fixed order it also went FIRST again, and a slow tenant early in the
//     alphabet spent the budget every tick while later tenants never ran. The
//     moving start puts each tenant first in turn.
//   - Session candidates were appended before queue-only ones and the cap kept
//     the head of that list, so under sustained session load a queue-only
//     target (one restored from a snapshot, or whose chats aged out of the
//     scan) was cut on every tick. Alternating gives queues every other slot.
//   - One tenant with many targets no longer fills the cap ahead of the rest.
//
// Deterministic for a given input and rotation, so a capped fan-out stays
// reproducible and testable.
func fairTargetOrder(fromSessions, fromQueue []consolidationTarget, rotation int) []consolidationTarget {
	type lanes struct{ sessions, queued []consolidationTarget }
	byTenant := map[string]*lanes{}
	var tenants []string
	lane := func(tenant string) *lanes {
		l := byTenant[tenant]
		if l == nil {
			l = &lanes{}
			byTenant[tenant] = l
			tenants = append(tenants, tenant)
		}
		return l
	}
	for _, c := range fromSessions {
		l := lane(c.TenantID)
		l.sessions = append(l.sessions, c)
	}
	for _, c := range fromQueue {
		l := lane(c.TenantID)
		l.queued = append(l.queued, c)
	}
	if len(tenants) == 0 {
		return nil
	}
	sort.Strings(tenants)

	perTenant := make([][]consolidationTarget, len(tenants))
	for i, tenant := range tenants {
		l := byTenant[tenant]
		var mixed []consolidationTarget
		for j := 0; j < len(l.sessions) || j < len(l.queued); j++ {
			if j < len(l.sessions) {
				mixed = append(mixed, l.sessions[j])
			}
			if j < len(l.queued) {
				mixed = append(mixed, l.queued[j])
			}
		}
		perTenant[i] = mixed
	}

	start := rotation % len(tenants)
	if start < 0 {
		start += len(tenants)
	}
	var out []consolidationTarget
	for round := 0; ; round++ {
		added := false
		for k := range tenants {
			mixed := perTenant[(start+k)%len(tenants)]
			if round < len(mixed) {
				out = append(out, mixed[round])
				added = true
			}
		}
		if !added {
			return out
		}
	}
}

// nextFanoutRotation returns this fan-out def's rotation for the tick being
// dispatched and advances it. See the fanoutRotation field.
func (s *Scheduler) nextFanoutRotation(defID string) int {
	s.fanoutRotationMu.Lock()
	defer s.fanoutRotationMu.Unlock()
	if s.fanoutRotation == nil {
		s.fanoutRotation = map[string]int{}
	}
	r := s.fanoutRotation[defID]
	s.fanoutRotation[defID] = r + 1
	return r
}

// fanoutBudgetSlices is the most ways a fan-out fire's budget is split. Each
// pass may spend at most FireTimeout / min(targets, fanoutBudgetSlices) — the
// whole budget for one target, half for two, a quarter from four up.
//
// Why a fixed fraction and not an even share per target: a consolidation pass
// is several model calls deep (one extractor child per chat it reads), and an
// even share of the 32-target cap is under 19 seconds at the default 10-minute
// budget, which would cut healthy passes. A quarter (2.5 minutes at the
// default) still bounds what one slow or wedged pass can take, so in a serial
// sweep at least four passes get a turn every tick — and with the start tenant
// rotating (fairTargetOrder), every tenant's turn comes round. Assumes a healthy
// pass fits in a quarter of the budget; an operator whose passes do not can
// raise LOOMCYCLE_SCHEDULER_FIRE_TIMEOUT_SECONDS.
const fanoutBudgetSlices = 4

// targetBudget is the wall-clock one target's pass may spend. The pass's ctx is
// derived from the batch's, so it also never outlives what is left of the
// batch budget.
func targetBudget(fireTimeout time.Duration, targets int) time.Duration {
	slices := targets
	if slices > fanoutBudgetSlices {
		slices = fanoutBudgetSlices
	}
	if slices < 1 {
		slices = 1
	}
	return fireTimeout / time.Duration(slices)
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

// dispatchSerially decides whether the batch runs one-at-a-time, and why. When
// it answers parallel, the reason is empty unless an explicit setting lifted a
// serial default (see gap 1 below), so the log can say so.
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
//     is serialized by default; that costs throughput, where the inverse
//     error costs an operator's GPU box. Setting
//     LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY explicitly is the escape hatch
//     for anyone who knows their children are parallel-safe: it lifts this
//     serial default (only this one — a target that resolves to a local
//     runtime, or cannot be resolved, still serializes). Unset, the default
//     width of 4 does not lift it.
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
	width, explicit := s.cfg.consolidationConcurrency()
	overridden := ""
	for _, target := range targets {
		providerID, err := s.providerResolver.ResolveAgentProvider(ctx, target.TenantID, target.UserID, def.Agent, def.UserTier)
		if err != nil {
			return true, fmt.Sprintf("provider for agent %q could not be resolved (%v) — defaulting to serial", def.Agent, err)
		}
		// Checked BEFORE the local test so the operator gets the specific
		// reason: "this probe cannot see where the load goes" is actionable,
		// "provider is not local" would not have been.
		if isSyntheticProvider(providerID) {
			if explicit {
				// Keep probing: another target (a tenant's fork of the agent)
				// may still resolve to a local runtime, which no setting lifts.
				overridden = fmt.Sprintf(
					"agent %q resolves to the in-process provider %q, which is serial by default; LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY=%d is set explicitly, which lifts that",
					def.Agent, providerID, width)
				continue
			}
			return true, fmt.Sprintf(
				"agent %q resolves to the in-process provider %q, which makes no model call itself — this batch's real model load is in sub-agents this probe cannot see, so parallel-safety is unknown. Set LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY if you know those children are not all on one box",
				def.Agent, providerID)
		}
		if isLocalProvider(providerID) {
			return true, fmt.Sprintf("provider %q is a local runtime", providerID)
		}
	}
	return false, overridden
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
	if target.TenantID != def.TenantID {
		// The operator-layer sweep dispatching into another tenant. That run
		// resolves its agent in the target's tenant, where a tenant's fork of the
		// consolidator (and of every agent it spawns) wins over the static one —
		// code the operator did not author.
		//
		// So the schedule's literal credentials are withheld from it, and, while
		// the deployment restricts the operator's provider key, the run is
		// restricted too. The def's captured bit cannot be trusted here: an
		// operator or admin author is never restricted, so it is false, and
		// copying it would run tenant code on the operator's key every tick. A
		// tenant with its own provider credential still consolidates on that
		// key, and a provider that needs no key (a local model, code-js) still
		// runs; anything else is refused (operator_key_restricted) until the
		// tenant adds a key or an admin gives it its own schedule. Gate off:
		// unchanged.
		in.UserCredentials = nil
		if s.cfg.OperatorKeyRestriction {
			in.OperatorKeyRestricted = true
		}
		// Isolated stays the def's bit. It confines a substrate:user member's
		// OWN runs to its user/agent scopes; it says nothing about the target.
		// This run's identity is the target user in the target tenant, so
		// unconfined it can reach that tenant's shared scope (and global, if the
		// agent's memory policy declares it) — what any run by a non-isolated
		// author in that tenant can — but not another tenant's, since the run's
		// tenant is the target's. Whether the TARGET user is an isolated member
		// is not known here (membership lives on its token).
	}
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
