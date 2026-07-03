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
	"encoding/json"
	"log"
	"os"
	"path/filepath"
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

	// --- RFC AV Phase 2b2: old-run archiver (OFF by default). ---
	// Unlike the usage rollup above (lossless compaction to the archive), this
	// DELETES aged completed runs + their events — the audit trail — so it is
	// opt-in: zero RunRetention or an "off"/"" RunRetentionMode disables it.

	// RunRetention is the cutoff for archiving completed runs: terminal runs
	// whose completed_at is older than this are exported (if the mode says so)
	// and deleted. Zero disables run archival entirely. token_usage is NOT
	// touched — usage retention is DetailRetention above.
	RunRetention time.Duration

	// RunRetentionMode is one of "off" (default / ""), "prune" (delete aged
	// completed runs + events), or "export+prune" (write each run + its events
	// to ExportDir as JSON, then delete). An unknown value is treated as off.
	RunRetentionMode string

	// ExportDir is the directory export+prune writes run JSON into (one file per
	// run under a per-day subdir). Required for export+prune; if empty, run
	// archival is disabled (never delete a run we were asked to export).
	ExportDir string

	// Logger is the structured logger used for sweep results. Defaults
	// to log.Printf when nil. Errors are logged at every tick;
	// successful sweeps with zero rows are logged only periodically
	// (the runtime is fine; chattering would just be noise).
	Logger func(format string, args ...any)

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
	runPruneBatch          = 500             // completed runs archived per tick
)

// Sweeper periodically folds old token_usage rows into usage_archive and
// deletes them, and (opt-in) archives aged completed runs. Construct via New,
// then call Run(ctx) on a goroutine that owns the lifecycle.
type Sweeper struct {
	store           store.Store
	interval        time.Duration
	detailRetention time.Duration
	runRetention    time.Duration
	runMode         string
	exportDir       string
	logf            func(format string, args ...any)
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
		cfg.Logger = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	mode := cfg.RunRetentionMode
	if mode == "" {
		mode = "off"
	}
	return &Sweeper{
		store:           st,
		interval:        cfg.Interval,
		detailRetention: cfg.DetailRetention,
		runRetention:    cfg.RunRetention,
		runMode:         mode,
		exportDir:       cfg.ExportDir,
		logf:            cfg.Logger,
		lock:            cfg.AdvisoryLock,
		lockKey:         cfg.AdvisoryLockKey,
		now:             cfg.Now,
	}
}

// runArchivalEnabled reports whether the opt-in old-run archiver should run:
// a positive RunRetention, a delete-bearing mode, and — for export+prune — a
// configured ExportDir (never delete a run we were asked to export but can't).
func (s *Sweeper) runArchivalEnabled() bool {
	if s.runRetention <= 0 {
		return false
	}
	switch s.runMode {
	case "prune":
		return true
	case "export+prune":
		return s.exportDir != ""
	}
	return false
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
	s.logf("usage: sweeper starting (interval=%s, detail_retention=%s)", s.interval, s.detailRetention)
	if s.runArchivalEnabled() {
		s.logf("usage: old-run archiver ON (mode=%s, run_retention=%s, export_dir=%q)", s.runMode, s.runRetention, s.exportDir)
	} else if s.runRetention > 0 && s.runMode == "export+prune" && s.exportDir == "" {
		s.logf("usage: old-run archiver DISABLED — mode=export+prune requires LOOMCYCLE_USAGE_EXPORT_DIR")
	}
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
			s.logf("usage: sweeper stopping (ctx done)")
			return
		case <-t.C:
			// When an AdvisoryLock is wired (cluster mode), gate the whole
			// tick behind the lock so only one replica per tick runs the
			// prune + run archival. Single-replica path (lock == nil) sweeps
			// unconditionally. Both steps run under the SAME acquisition.
			var (
				n, archived  int
				err, archErr error
				ranArchival  bool
			)
			doWork := func(ctx context.Context) {
				n, err = s.sweepOnce(ctx)
				if s.runArchivalEnabled() {
					ranArchival = true
					archived, archErr = s.archiveRunsOnce(ctx)
				}
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
					s.logf("usage: advisory lock infra error: %v", lockErr)
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
				s.logf("usage: sweep failed: %v", err)
			} else if n > 0 {
				s.logf("usage: rolled up + pruned %d detail row(s)", n)
				noOpStreak = 0
			} else {
				if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
					s.logf("usage: sweep tick — 0 detail rows to prune (streak=%d)", noOpStreak)
				}
				noOpStreak++
			}
			// Old-run archival result (only when enabled).
			if ranArchival {
				if archErr != nil {
					s.logf("usage: run archival failed: %v", archErr)
				} else if archived > 0 {
					s.logf("usage: archived + pruned %d completed run(s) (mode=%s)", archived, s.runMode)
				}
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

// archiveRunsOnce archives one batch of aged completed runs (RFC AV Phase 2b2):
// list terminal runs older than the cutoff, export each (export+prune mode) then
// delete it (run + events). Per-run failures are logged and skipped — a bad
// export never deletes the run. Returns the number deleted. Given a longer
// timeout than the usage prune because export writes files.
func (s *Sweeper) archiveRunsOnce(parent context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	cutoff := s.now().Add(-s.runRetention)
	runs, err := s.store.PrunableCompletedRuns(ctx, cutoff, runPruneBatch)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, r := range runs {
		if s.runMode == "export+prune" {
			if err := s.exportRun(ctx, r); err != nil {
				// Never delete a run we failed to export — retry next tick.
				s.logf("usage: export run %s failed, skipping delete: %v", r.ID, err)
				continue
			}
		}
		if err := s.store.DeleteRunAndEvents(ctx, r.ID); err != nil {
			s.logf("usage: delete run %s failed: %v", r.ID, err)
			continue
		}
		deleted++
	}
	return deleted, nil
}

// exportRun writes a run + its events to ExportDir as JSON, under a per-day
// subdir (bucketing by completed date keeps any single directory bounded). The
// export dir is operator config, never model input.
func (s *Sweeper) exportRun(ctx context.Context, r store.Run) error {
	events, err := s.store.GetRunEventsSince(ctx, r.ID, 0, 1_000_000)
	if err != nil {
		return err
	}
	day := r.CompletedAt
	if day.IsZero() {
		day = r.StartedAt
	}
	dir := filepath.Join(s.exportDir, day.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	payload := struct {
		Run    store.Run     `json:"run"`
		Events []store.Event `json:"events"`
	}{Run: r, Events: events}
	blob, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	// 0o600: run transcripts may hold sensitive tool I/O; operator-only.
	return os.WriteFile(filepath.Join(dir, r.ID+".json"), blob, 0o600)
}
