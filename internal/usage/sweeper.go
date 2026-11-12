// Package usage runs the periodic token-usage rollup-and-prune sweeper
// alongside the HTTP server (RFC AV Phase 2b). Without it, the
// token_usage detail table grows unbounded — one row per LLM call,
// forever — even though anything past the detail-retention window is
// only ever read in aggregate.
//
// The sweeper is store-agnostic — it calls store.RollupAndPruneUsage on
// a timer and logs the count. The Store adapter owns the atomic
// "fold old detail rows into usage_archive (day-bucketed) then delete
// them" transaction. Pruning is a compaction to daily buckets, NOT data
// loss: the archive preserves the exact per-dimension totals
// (tenant/user/provider/model/credential-source), so UsageReport still
// returns the same numbers — it just reads them from the archive union
// instead of scanning individual call rows.
package usage

import (
	"context"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config carries the sweeper's tuning knobs. Zero/missing values fall
// back to defaults documented per-field.
type Config struct {
	// Interval is how often the sweeper wakes up to roll up + prune.
	// Default 1h. The work is cheap and idempotent, so the exact cadence
	// isn't load-bearing — hourly keeps the detail table from drifting
	// far past the retention window between ticks.
	Interval time.Duration

	// DetailRetention is the cutoff: token_usage rows older than this are
	// folded into usage_archive and deleted. Default 720h (30 days).
	// Recent detail is kept verbatim so per-call debugging / attribution
	// stays possible within the window; older activity survives only as
	// day-bucketed archive aggregates.
	DetailRetention time.Duration

	// Logger is the structured logger used for sweep results. Defaults
	// to slog.Default() when nil. Errors are logged at every tick;
	// successful sweeps with zero rows are logged only periodically
	// (the runtime is fine; chattering would just be noise).
	Logger *slog.Logger

	// AdvisoryLock is the singleton-sweeper gate. When set, each tick
	// acquires the lock before calling sweepOnce — only one replica per
	// cluster runs the prune per tick. Nil = single-replica mode (every
	// replica sweeps; the SQL is idempotent so concurrent sweeps stay
	// correct, just noisy).
	//
	// Interface-typed (not *coord.AdvisoryLock concretely) so this
	// package stays free of an internal/coord import. The interface
	// declares only the surface the sweeper needs.
	AdvisoryLock AdvisoryLocker

	// AdvisoryLockKey is the lock-key int64 (typically coord.LockKeyUsageSweeper).
	// Only consulted when AdvisoryLock is non-nil.
	AdvisoryLockKey int64

	// Now is the clock sweepOnce reads to compute the retention cutoff
	// (cutoff = Now() - DetailRetention). Nil defaults to time.Now.
	// Injectable so tests can pin the cutoff deterministically instead of
	// racing real wall-clock elapsed time against DetailRetention.
	Now func() time.Time
}

// AdvisoryLocker is the minimum surface the sweeper needs from
// internal/coord.AdvisoryLock. Defined here so internal/usage stays
// free of the internal/coord import. *coord.AdvisoryLock satisfies it
// implicitly.
type AdvisoryLocker interface {
	TryRun(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error)
}

const (
	defaultInterval        = 1 * time.Hour
	defaultDetailRetention = 720 * time.Hour // 30 days
)

// Sweeper periodically folds old token_usage rows into usage_archive and
// deletes them. Construct via New,
// then call Run(ctx) on a goroutine that owns the lifecycle.
type Sweeper struct {
	store           store.Store
	interval        time.Duration
	detailRetention time.Duration
	logger          *slog.Logger
	lock            AdvisoryLocker
	lockKey         int64
	now             func() time.Time
}

// New constructs a Sweeper with the supplied tuning. A nil store means
// "no persistence" (the Server can run without a Store) — Run is then a
// no-op.
func New(st store.Store, cfg Config) *Sweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.DetailRetention <= 0 {
		cfg.DetailRetention = defaultDetailRetention
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Sweeper{
		store:           st,
		interval:        cfg.Interval,
		detailRetention: cfg.DetailRetention,
		logger:          cfg.Logger,
		lock:            cfg.AdvisoryLock,
		lockKey:         cfg.AdvisoryLockKey,
		now:             cfg.Now,
	}
}

// Run drives the sweep loop until ctx is done. Each tick calls
// RollupAndPruneUsage with cutoff = now - DetailRetention. The first
// tick fires after Interval (not immediately) so a fresh process doesn't
// prune the moment it boots.
//
// This function blocks. Caller is expected to start it on a goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.logger.Info("usage: sweeper starting", "interval", s.interval, "detail_retention", s.detailRetention)
	t := time.NewTicker(s.interval)
	defer t.Stop()

	// Track consecutive zero-row sweeps so the no-op log line is emitted
	// on the first sweep, then suppressed until either a non-zero sweep
	// or every Nth tick (roughly daily at the default 1h interval —
	// enough to confirm the goroutine is alive without flooding the log).
	const noOpHeartbeatEvery = 24
	noOpStreak := 0

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("usage: sweeper stopping (ctx done)")
			return
		case <-t.C:
			// When an AdvisoryLock is wired (cluster mode), gate the whole
			// tick behind the lock so only one replica per tick runs the
			// prune. Single-replica path (lock == nil) sweeps unconditionally.
			var (
				n   int
				err error
			)
			doWork := func(ctx context.Context) {
				n, err = s.sweepOnce(ctx)
			}
			if s.lock != nil {
				acquired, lockErr := s.lock.TryRun(ctx, s.lockKey, func(ctx context.Context) error {
					// Return nil so TryRun's err signals ONLY infra failures
					// (pool acquire, pg_try_advisory_lock), kept distinct from
					// the per-step failure logs below.
					doWork(ctx)
					return nil
				})
				if lockErr != nil {
					s.logger.Warn("usage: advisory lock infra error", "err", lockErr)
					continue
				}
				if !acquired {
					// Another replica is running this tick — silent.
					continue
				}
			} else {
				doWork(ctx)
			}
			// Usage rollup-prune result.
			if err != nil {
				s.logger.Warn("usage: sweep failed", "err", err)
			} else if n > 0 {
				s.logger.Info("usage: rolled up + pruned detail rows", "pruned", n)
				noOpStreak = 0
			} else {
				if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
					s.logger.Info("usage: sweep tick — 0 detail rows to prune", "streak", noOpStreak)
				}
				noOpStreak++
			}
		}
	}
}

// sweepOnce runs one rollup-and-prune. Exposed as a method (vs inlined
// in Run) so tests can drive ticks deterministically without a real
// Ticker. ctx is the parent's; the RollupAndPruneUsage call gets a
// derived 30s timeout so a hung backend can't stall the goroutine
// indefinitely.
func (s *Sweeper) sweepOnce(parent context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cutoff := s.now().Add(-s.detailRetention)
	return s.store.RollupAndPruneUsage(ctx, cutoff)
}
