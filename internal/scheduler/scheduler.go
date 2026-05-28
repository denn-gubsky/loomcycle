package scheduler

import (
	"context"
	"errors"
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
}

// defaults applies the documented defaults to a zero-value Config.
func (c Config) defaults() Config {
	if c.TickInterval == 0 {
		c.TickInterval = 30 * time.Second
	}
	if c.FireTimeout == 0 {
		c.FireTimeout = 10 * time.Minute
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
	cfg    Config
	store  store.Store
	runner runner.Runner
	pause  *pause.Manager
	mcp    MCPCaller
	logf   func(format string, args ...any)

	wg     sync.WaitGroup
	stopCh chan struct{}
	once   sync.Once
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
	for _, row := range due {
		if ctxDone(ctx) {
			return
		}
		s.fireOne(ctx, row, now)
	}
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
		if errors.Is(runErr, context.DeadlineExceeded) {
			status = "failed"
			errStr = "fire timeout exceeded"
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
	if err := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      row.DefID,
		LastRunID:  registeredRunID,
		LastStatus: status,
		LastError:  errStr,
		LastRunAt:  now,
		NextRunAt:  next,
	}); err != nil {
		s.logf("scheduler: record result for %q: %v", row.Name, err)
	}

	// Dispatch hooks only on success — RFC E says on_complete fires on
	// "successful runs." Failed/skipped runs don't notify.
	if status == "completed" {
		s.dispatchHooks(ctx, row.Name, def, registeredRunID, registeredAgentID)
	}
}

// advanceOnly is the disabled-schedule path: bump next_run_at without
// recording a run.
func (s *Scheduler) advanceOnly(ctx context.Context, defID string, def scheduleDef, reason string, now time.Time) {
	next, err := s.computeNext(def, now)
	if err != nil {
		next = now.Add(1 * time.Hour)
	}
	_ = s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastStatus: reason,
		LastRunAt:  now,
		NextRunAt:  next,
	})
}

// recordFireFailure records an outcome when we never reached the
// runner (decode failure, etc.). Same advance-by-1h fallback if
// the def's cron can't be resolved.
func (s *Scheduler) recordFireFailure(ctx context.Context, defID, runID, status string, err error, now time.Time) {
	s.logf("scheduler: def %q fire-failed (%s): %v", defID, status, err)
	_ = s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastRunID:  runID,
		LastStatus: status,
		LastError:  err.Error(),
		LastRunAt:  now,
		NextRunAt:  now.Add(1 * time.Hour),
	})
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
