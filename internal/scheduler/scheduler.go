package scheduler

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config holds the operator-tunable knobs.
type Config struct {
	// TickInterval is how often the sweeper polls for due rows. 0 →
	// default 30s. Lower values trade DB query frequency for tighter
	// schedule punctuality. Most operators leave it at the default.
	TickInterval time.Duration

	// FireTimeout is the per-fire cap on the agent run. 0 → default
	// 10m. Reaching the cap cancels the run via ctx and records
	// last_status=failed last_error="run timeout".
	FireTimeout time.Duration

	// EnvAllowlist is the set of env var names a schedule can read
	// via user_credentials_from_env. The empty allowlist (default)
	// disables env-credential resolution entirely — a safe-by-default
	// posture. Operators opt in via LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST.
	EnvAllowlist map[string]bool

	// MaxConcurrentFires bounds the number of schedules a single tick
	// fires in parallel. 0 → default runtime.NumCPU()*4. Each fire is
	// a goroutine that calls runner.RunOnce synchronously; the tick
	// itself waits for the whole batch to drain before returning so
	// the "one tick at a time" invariant holds. Larger values trade
	// memory + concurrency-store pressure for tighter cascading at
	// burst-fire moments (e.g. cron crossings where 100s of forks
	// become due in the same second).
	MaxConcurrentFires int
}

// defaults applies the documented defaults to a zero-value Config.
func (c Config) defaults() Config {
	if c.TickInterval == 0 {
		c.TickInterval = 30 * time.Second
	}
	if c.FireTimeout == 0 {
		c.FireTimeout = 10 * time.Minute
	}
	if c.MaxConcurrentFires == 0 {
		c.MaxConcurrentFires = runtime.NumCPU() * 4
	}
	return c
}

// Scheduler is the sweeper runtime. One instance per loomcycle
// process; in cluster mode (v0.12+) each replica runs its own
// Scheduler and per-def advisory locks coordinate which fires.
//
// Construction is via New + Start. Stop the goroutine via
// (*Scheduler).Stop or by cancelling the ctx passed to Start.
type Scheduler struct {
	cfg     Config
	store   store.Store
	runner  runner.Runner
	pause   *pause.Manager
	mcp     MCPCaller
	chScope ChannelScopeResolver
	logf    func(format string, args ...any)

	wg     sync.WaitGroup
	stopCh chan struct{}
	once   sync.Once

	// inFlight tracks def_ids whose fire goroutine is currently
	// running (between slot-acquire and RecordResult). Used to
	// suppress double-fire when a fire takes longer than the tick
	// interval and the next tick still sees the same row as due
	// (because RecordResult hasn't advanced next_run_at yet).
	//
	// Surfaced by the compound test at scale=30000 where every
	// schedule fired twice — every fire's RecordResult write was
	// slower than the 100ms tick under heavy concurrent load, so
	// each row stayed "due" for the next tick. The in-memory
	// tracker is single-replica-only; cluster-mode advisory locks
	// (v0.12+) would cover the cross-replica case symmetrically.
	//
	// Entry lifecycle:
	//   tick():
	//     LoadOrStore(def_id, _) before slot-acquire — skip if loaded
	//   fire goroutine (deferred):
	//     Delete(def_id) after fireOne returns or panics
	//
	// A goroutine that hangs leaks the entry until the fireCtx
	// timeout cancels the RunOnce call (default 10m), which is the
	// existing budget. No separate TTL needed.
	inFlight sync.Map
}

// ChannelScopeResolver returns the declared scope ("global" | "user" |
// "agent") of a channel by name. ok=false when the channel is declared
// nowhere (static yaml + runtime substrate). Injected so the on_complete:
// channel.publish hook publishes at the channel's declared scope instead of
// blindly under the run's user scope (F37 / RFC T). Satisfied by
// (*http.Server).ResolveChannelScope; nil leaves the legacy user-scope
// behavior untouched.
type ChannelScopeResolver func(ctx context.Context, channel string) (scope string, ok bool)

// SetChannelScope wires the channel-scope resolver. Must be called before
// Start (the sweeper reads chScope when dispatching on_complete hooks). A
// no-op-safe setter rather than a New parameter so the many existing
// New(...) call sites stay unchanged.
func (s *Scheduler) SetChannelScope(r ChannelScopeResolver) {
	s.chScope = r
}

// New constructs a Scheduler. All four runtime dependencies are
// required; mcp is optional (nil disables mcp.call hooks with a
// clear error on attempted dispatch). logf may be nil for silent
// operation (tests + small embeds).
func New(cfg Config, st store.Store, r runner.Runner, p *pause.Manager, mcp MCPCaller, logf func(string, ...any)) *Scheduler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Scheduler{
		cfg:    cfg.defaults(),
		store:  st,
		runner: r,
		pause:  p,
		mcp:    mcp,
		logf:   logf,
		stopCh: make(chan struct{}),
	}
}

// Start launches the sweeper goroutine. Returns immediately; the
// goroutine runs until ctx is cancelled OR Stop is called. Safe to
// call only once per instance.
func (s *Scheduler) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop signals the sweeper to exit and blocks until the goroutine
// returns. Idempotent — calling Stop twice is safe but only the
// first call sends the signal.
func (s *Scheduler) Stop() {
	s.once.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// run is the sweeper main loop. Single goroutine — no concurrency
// concerns inside this function.
func (s *Scheduler) run(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()

	s.logf("scheduler: started (tick=%s, fire_timeout=%s)", s.cfg.TickInterval, s.cfg.FireTimeout)
	defer s.logf("scheduler: stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick processes one sweeper iteration. Skips when pause manager
// reports runtime != StateRunning (matches the v0.8.17 pause/resume
// composition rule from RFC E).
//
// Due rows fire in parallel up to cfg.MaxConcurrentFires goroutines.
// The tick waits for the whole batch to drain before returning so
// the "next tick won't start until this one finishes" invariant
// holds — important because each fire's RecordResult advances
// next_run_at; without the wait, the next tick could re-fire a row
// whose status update is still in flight. The bounded semaphore
// keeps memory + per-user-fairness pressure predictable when 100s
// of forks become due in one cron crossing.
func (s *Scheduler) tick(ctx context.Context) {
	if s.pause != nil && s.pause.State() != pause.StateRunning {
		// Paused / pausing — the runtime is quiesced for snapshot.
		// Skip without advancing next_run_at; next tick re-checks.
		return
	}
	now := time.Now()
	due, err := s.store.ScheduleRunStateListDue(ctx, now)
	if err != nil {
		s.logf("scheduler: list due: %v", err)
		return
	}
	if len(due) == 0 {
		return
	}

	// Buffered semaphore caps concurrent fires. Each goroutine
	// acquires a slot before calling fireOne and releases on exit.
	// A nil semaphore (size <= 0) would mean unbounded parallelism;
	// the Config.defaults() floor ensures size is always positive.
	sem := make(chan struct{}, s.cfg.MaxConcurrentFires)
	var wg sync.WaitGroup
	for _, row := range due {
		// In-flight suppression: skip rows whose fire goroutine from
		// a previous tick is still running (RecordResult hasn't yet
		// advanced next_run_at, so the row would otherwise re-fire).
		// LoadOrStore is atomic so the racy "check then store" hole
		// is closed. See the inFlight field's commentary for the
		// lifecycle + why it solves the x30000 over-fire finding.
		if _, alreadyFiring := s.inFlight.LoadOrStore(row.DefID, time.Now()); alreadyFiring {
			continue
		}
		// Slot-acquire is ctx-aware so cancellation during a slow
		// tick doesn't block waiting for slots indefinitely.
		select {
		case <-ctx.Done():
			// We reserved the inFlight slot above but won't fire;
			// release it so the next tick can pick up this def
			// freely.
			s.inFlight.Delete(row.DefID)
			// Skip remaining rows; in-flight fires continue (their
			// own fireCtx still has a timeout). Wait below drains.
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(row store.ScheduleDueRow) {
			defer wg.Done()
			defer func() { <-sem }()
			// Always release the in-flight reservation, even on
			// panic. defer order is LIFO so this runs AFTER the
			// recover() below; that ordering means a panicking fire
			// still clears its in-flight slot before the next tick.
			defer s.inFlight.Delete(row.DefID)
			// Recover panics so one bad fire doesn't bring down
			// the sweeper goroutine. Log + advance with a parked
			// next_run_at so the def doesn't re-present every tick.
			defer func() {
				if r := recover(); r != nil {
					s.logf("scheduler: PANIC in fireOne(def_id=%s): %v", row.DefID, r)
					// Best-effort park — re-use the same 1h fallback
					// the cron-resolve failure path uses.
					if rerr := s.store.ScheduleRunStateRecordResult(context.Background(), store.ScheduleRunResult{
						DefID:      row.DefID,
						LastStatus: "failed",
						LastError:  "panic in fireOne",
						LastRunAt:  time.Now(),
						NextRunAt:  time.Now().Add(1 * time.Hour),
					}); rerr != nil {
						// exp7 I3: if the park write also fails the def re-fires
						// next tick — log so the panic→re-fire chain isn't silent.
						s.logf("scheduler: def %q panic-park record-result failed: %v", row.DefID, rerr)
					}
				}
			}()
			s.fireOne(ctx, row, now)
		}(row)
	}
	wg.Wait()
}

// fireOne handles one due schedule end-to-end: unmarshal the def,
// build RunInput, call runner.RunOnce, record result, advance
// next_run_at, dispatch on_complete hooks. Errors are logged but
// never bubble out — one failed schedule shouldn't block the rest
// of the tick.
func (s *Scheduler) fireOne(ctx context.Context, row store.ScheduleDueRow, now time.Time) {
	def, err := unmarshalDef(row.Definition)
	if err != nil {
		s.recordFireFailure(ctx, row.DefID, "", "decode_def", err, now)
		return
	}
	if def.Enabled != nil && !*def.Enabled {
		// Skip-but-advance: the operator disabled this schedule via the
		// substrate (or the yaml template set enabled:false). Bump
		// next_run_at to keep listDue's bounded set from re-presenting
		// this row every tick.
		s.advanceOnly(ctx, row.DefID, def, "skipped_disabled", now)
		return
	}

	in := buildRunInput(def, s.cfg.EnvAllowlist, s.logf)

	// Cap the per-fire run time. The runner's ctx-cancellation cascades
	// down to provider calls + tool calls so timeout cleanly aborts.
	fireCtx, cancel := context.WithTimeout(ctx, s.cfg.FireTimeout)
	defer cancel()

	var registeredAgentID, registeredRunID string
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, _, _ string) {
			registeredAgentID = agentID
			registeredRunID = runID
		},
	}
	runErr := s.runner.RunOnce(fireCtx, in, cb)
	status := "completed"
	errStr := ""
	// F36: every real fire counts toward max_fires (a wedged/always-failing
	// schedule still retires). F38 exception: a fire that fails AGENT
	// RESOLUTION never started a run — it's a config error (the agent isn't
	// resolvable in the run's tenant), identical on every fire. Counting it
	// would silently burn the cap and retire the schedule after N failures,
	// masking the misconfig as N normal runs. Don't count it; log loudly.
	countAsFire := true
	if runErr != nil {
		status = "failed"
		errStr = runErr.Error()
		// Domain-typed sentinels get a friendlier status label.
		if errors.Is(runErr, runner.ErrBackpressure) {
			status = "skipped"
		}
		if errors.Is(runErr, runner.ErrPerUserQuotaExhausted) {
			status = "skipped"
		}
		if errors.Is(runErr, runner.ErrUnknownAgent) {
			countAsFire = false
			s.logf("scheduler: schedule %q could not resolve agent %q in tenant %q — not counting toward max_fires; check the agent exists in this tenant (F38)",
				row.Name, def.Agent, def.TenantID)
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			// Disambiguate fireCtx (per-fire timeout) from parent ctx
			// (scheduler shutting down). Both surface as
			// context.DeadlineExceeded via errors.Is. Checking
			// fireCtx.Err() lets us emit a more accurate status.
			if fireCtx.Err() != nil && ctx.Err() == nil {
				status = "failed"
				errStr = "fire timeout exceeded"
			} else {
				// Parent ctx deadline (or both — treat as shutdown).
				status = "failed"
				errStr = "scheduler context deadline exceeded"
			}
		}
	}

	// Advance next_run_at + record outcome atomically (single UPDATE).
	next, nextErr := s.computeNext(def, now)
	if nextErr != nil {
		// Without a valid next_run_at, the sweeper would re-fire this
		// def every tick. Park it 1 hour in the future so the operator
		// gets a breathing window to fix the def before re-firing.
		s.logf("scheduler: schedule %q cron-resolve failed: %v — parking 1h", row.Name, nextErr)
		next = now.Add(1 * time.Hour)
	}
	// Use a survival ctx for RecordResult when the parent is already
	// cancelled (e.g. mid-shutdown). Without this, the store write
	// fails silently, next_run_at stays in the past, and the schedule
	// re-fires immediately on the next startup. Bounded 5s timeout
	// prevents the survival path from hanging shutdown indefinitely.
	recordCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		recordCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := s.store.ScheduleRunStateRecordResult(recordCtx, store.ScheduleRunResult{
		DefID:      row.DefID,
		LastRunID:  registeredRunID,
		LastStatus: status,
		LastError:  errStr,
		LastRunAt:  now,
		NextRunAt:  next,
		// RFC S / F36: this IS a fire (any status counts toward the cap, so
		// a wedged/always-failing schedule still retires). The disabled-skip
		// advance (advanceOnly) leaves this false; F38 leaves it false for an
		// unresolved-agent config error (see countAsFire above).
		CountAsFire: countAsFire,
	}); err != nil {
		s.logf("scheduler: record result for %q: %v", row.Name, err)
	}

	// RFC S / F36: auto-retire after the Nth fire. Re-read the just-
	// incremented fire_count (cheap, and only when a cap is set) and retire
	// the def once it reaches max_fires. Retired defs are skipped by the
	// due-query JOIN, so this is the last fire. Uses recordCtx so it still
	// runs mid-shutdown. Multi-replica: fire_count += 1 is atomic, so the
	// cap is exact single-replica and at most over-fired by the racing
	// replica count — acceptable for a lifetime bound.
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

	// Dispatch hooks only on success — RFC E says on_complete fires on
	// "successful runs." Failed/skipped runs don't notify. Use recordCtx (the
	// survival ctx) not the parent: a run that completes just as shutdown
	// begins still recorded its result above, so its on_complete hooks
	// (channel publish / memory set / mcp.call) must fire too rather than be
	// dropped on a cancelled parent ctx.
	if status == "completed" {
		s.dispatchHooks(recordCtx, row.Name, def, registeredRunID, registeredAgentID)
	}
}

// advanceOnly is the disabled-schedule path: bump next_run_at without
// recording a run.
func (s *Scheduler) advanceOnly(ctx context.Context, defID string, def scheduleDef, reason string, now time.Time) {
	next, err := s.computeNext(def, now)
	if err != nil {
		next = now.Add(1 * time.Hour)
	}
	// exp7 I3: a dropped result-write leaves next_run_at unadvanced, so the
	// def re-presents on every subsequent tick (a re-fire loop). A genuinely
	// dead store can't advance state at all — logging is the most this path
	// can do, but it makes the re-fire cause visible instead of silent.
	if err := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastStatus: reason,
		LastRunAt:  now,
		NextRunAt:  next,
	}); err != nil {
		s.logf("scheduler: def %q advance-only (%s) record-result failed: %v", defID, reason, err)
	}
}

// recordFireFailure records an outcome when we never reached the
// runner (decode failure, etc.). Same advance-by-1h fallback if
// the def's cron can't be resolved.
func (s *Scheduler) recordFireFailure(ctx context.Context, defID, runID, status string, err error, now time.Time) {
	s.logf("scheduler: def %q fire-failed (%s): %v", defID, status, err)
	// exp7 I3: surface a dropped result-write — see advanceOnly above.
	if rerr := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastRunID:  runID,
		LastStatus: status,
		LastError:  err.Error(),
		LastRunAt:  now,
		NextRunAt:  now.Add(1 * time.Hour),
	}); rerr != nil {
		s.logf("scheduler: def %q record fire-failure result failed: %v", defID, rerr)
	}
}

// computeNext picks the cron from the def and returns the next-fire time
// strictly after `now`.
func (s *Scheduler) computeNext(def scheduleDef, now time.Time) (time.Time, error) {
	expr, err := ResolveCron(def.Schedule, def.UserTierSchedules, def.UserTier)
	if err != nil {
		return time.Time{}, err
	}
	return NextFireAfter(expr, def.Timezone, now)
}

// ctxDone is a non-blocking ctx-cancellation check used inside the
// fire loop so a Stop() mid-tick gets honoured promptly.
func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
