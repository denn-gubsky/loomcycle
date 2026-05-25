// Package heartbeat runs the periodic stale-run sweeper alongside the
// HTTP server. Without it, runs whose host process crashed mid-loop
// stay status="running" forever (UpdateHeartbeat never reaches the
// store, FinishRun never fires) and pollute the active-run lists.
//
// The sweeper is store-agnostic — it calls store.SweepStaleRuns on a
// timer and logs the count. The Store adapter is responsible for the
// atomic UPDATE that picks the right rows.
package heartbeat

import (
	"context"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config carries the sweeper's tuning knobs. Zero/missing values fall
// back to defaults documented per-field.
type Config struct {
	// Interval is how often the sweeper wakes up to look for stale
	// runs. Default 60s. Setting it shorter than StaleAfter is fine
	// (more frequent checks → faster cleanup) but doesn't help below
	// the loop's heartbeat tick rate.
	Interval time.Duration

	// StaleAfter is the cutoff: runs whose last heartbeat (or
	// started_at, if they never heartbeated) is older than this are
	// marked failed. Default 10 minutes.
	//
	// Should be ≥ 2× the longest expected single tool call duration.
	// Heartbeats fire at the top of each loop iteration (BEFORE tool
	// dispatch), so a slow Bash / WebFetch / sub-agent call blocks
	// heartbeat updates for its full duration — a 9-minute tool call
	// looks identical to a crashed run if StaleAfter is set too low.
	// Operators with long-running tool dispatch should raise this
	// (e.g. LOOMCYCLE_HEARTBEAT_STALE_MS=1800000 for a 30-min cap).
	StaleAfter time.Duration

	// Logger is the structured logger used for sweep results. Defaults
	// to log.Printf when nil. Errors are logged at every tick;
	// successful sweeps with zero rows are logged only periodically
	// (the runtime is fine; chattering would just be noise).
	Logger func(format string, args ...any)

	// AdvisoryLock is the v0.12.4 Phase 5 singleton-sweeper gate.
	// When set, each tick acquires the lock before calling
	// sweepOnce — only one replica per cluster runs the sweep per
	// tick. Nil = single-replica mode (every replica sweeps; SQL is
	// idempotent so concurrent sweeps stay correct, just noisy).
	//
	// Interface-typed (not *coord.AdvisoryLock concretely) so this
	// package stays free of an internal/coord import. The interface
	// declares only the surface the sweeper needs.
	AdvisoryLock AdvisoryLocker

	// AdvisoryLockKey is the lock-key int64 (typically coord.LockKeyHeartbeatSweeper).
	// Only consulted when AdvisoryLock is non-nil.
	AdvisoryLockKey int64
}

// AdvisoryLocker is the minimum surface the sweeper needs from
// internal/coord.AdvisoryLock. Defined here so internal/heartbeat
// stays free of the internal/coord import. *coord.AdvisoryLock
// satisfies it implicitly.
type AdvisoryLocker interface {
	TryRun(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error)
}

const (
	defaultInterval   = 60 * time.Second
	defaultStaleAfter = 10 * time.Minute
)

// Sweeper periodically marks stale running rows as failed. Construct
// via New, then call Run(ctx) on a goroutine that owns the lifecycle.
type Sweeper struct {
	store      store.Store
	interval   time.Duration
	staleAfter time.Duration
	logf       func(format string, args ...any)
	lock       AdvisoryLocker
	lockKey    int64
}

// New constructs a Sweeper with the supplied tuning. A nil store means
// "no persistence" (the Server can run without a Store) — Run is then a
// no-op.
func New(st store.Store, cfg Config) *Sweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaultStaleAfter
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Printf
	}
	return &Sweeper{
		store:      st,
		interval:   cfg.Interval,
		staleAfter: cfg.StaleAfter,
		logf:       cfg.Logger,
		lock:       cfg.AdvisoryLock,
		lockKey:    cfg.AdvisoryLockKey,
	}
}

// Run drives the sweep loop until ctx is done. Each tick calls
// SweepStaleRuns with cutoff = now - StaleAfter. The first tick fires
// after Interval (not immediately) so a fresh process doesn't sweep
// rows that legitimately survived a graceful restart.
//
// This function blocks. Caller is expected to start it on a goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.logf("heartbeat: sweeper starting (interval=%s, stale_after=%s)", s.interval, s.staleAfter)
	t := time.NewTicker(s.interval)
	defer t.Stop()

	// Track consecutive zero-row sweeps so the no-op log line is
	// emitted on the first sweep, then suppressed until either a
	// non-zero sweep or every Nth tick (every hour at default
	// interval — enough to confirm the goroutine is alive without
	// flooding the log).
	const noOpHeartbeatEvery = 60
	noOpStreak := 0

	for {
		select {
		case <-ctx.Done():
			s.logf("heartbeat: sweeper stopping (ctx done)")
			return
		case <-t.C:
			// v0.12.4 Phase 5: when an AdvisoryLock is wired (cluster
			// mode), gate the sweep behind the lock so only one
			// replica per tick actually runs the UPDATE. Single-
			// replica path (lock == nil) sweeps unconditionally.
			var (
				n   int
				err error
			)
			if s.lock != nil {
				acquired, lockErr := s.lock.TryRun(ctx, s.lockKey, func(ctx context.Context) error {
					// Capture into outer n + err directly. We deliberately
					// return nil from fn so TryRun's err signals ONLY
					// infra failures (pool acquire, pg_try_advisory_lock
					// failure), keeping the sweep-failed log line below
					// distinct from the advisory-lock-failed log line.
					n, err = s.sweepOnce(ctx)
					return nil
				})
				if lockErr != nil {
					s.logf("heartbeat: advisory lock infra error: %v", lockErr)
					continue
				}
				if !acquired {
					// Another replica is running this tick — silent.
					continue
				}
			} else {
				n, err = s.sweepOnce(ctx)
			}
			if err != nil {
				s.logf("heartbeat: sweep failed: %v", err)
				continue
			}
			if n > 0 {
				s.logf("heartbeat: marked %d stale run(s) as failed", n)
				noOpStreak = 0
				continue
			}
			if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
				s.logf("heartbeat: sweep tick — 0 stale runs (streak=%d)", noOpStreak)
			}
			noOpStreak++
		}
	}
}

// sweepOnce runs one sweep. Exposed as a method (vs inlined in Run) so
// tests can drive ticks deterministically without a real Ticker. ctx
// is the parent's; the SweepStaleRuns call gets a derived 30s timeout
// so a hung backend can't stall the goroutine indefinitely.
func (s *Sweeper) sweepOnce(parent context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cutoff := time.Now().Add(-s.staleAfter)
	return s.store.SweepStaleRuns(ctx, cutoff)
}
