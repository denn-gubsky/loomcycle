// Package retention runs the periodic RFC BM data-retention sweeper alongside
// the HTTP server. Without it, retired substrate def versions
// (agent/skill/team/mcp_server/schedule/a2a_server_card/a2a_agent/webhook/
// memory_backend) accumulate forever — a self-evolving fleet forks and retires
// versions continuously, so the *_defs tables grow unbounded even though a
// retired-and-old version is only ever read for lineage history.
//
// The sweeper is store-agnostic: it lists purgeable versions per def-type via
// store.ListPurgeableRetiredDefVersions and deletes them via
// store.DeleteDefVersions. The store owns the SQL (the retired / non-active /
// keep-last-N exclusions + the hardcoded table allowlist).
//
// Unlike the RFC AV usage rollup (a lossless compaction), this DELETES data, so
// it is OPT-IN and defaults OFF: an "off" DefsMode is a no-op. When
// DefsMode=export+prune each version is written to ExportDir as JSON BEFORE it
// is deleted (export-then-delete: a failed export never deletes the row), so the
// retired history survives outside the database.
package retention

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config carries the sweeper's tuning knobs. Zero/missing values fall back to
// the defaults documented per-field.
type Config struct {
	// Interval is how often the sweeper wakes up. Default 1h. The work is cheap
	// and idempotent, so the exact cadence isn't load-bearing.
	Interval time.Duration

	// DefsMode is one of "off" (default / ""), "prune" (delete purgeable retired
	// versions), or "export+prune" (write each to ExportDir as JSON, then
	// delete). Unknown → treated as off. Destructive modes are opt-in.
	DefsMode string

	// DefsMaxAge is the age cutoff: only versions created before Now()-DefsMaxAge
	// are eligible. Zero = no minimum age (every retired non-active version past
	// keep-last-N qualifies).
	DefsMaxAge time.Duration

	// DefsKeepLastN keeps the N most-recent qualifying (retired, old, non-active)
	// versions per (tenant, name) as lineage history; the rest are purged. 0
	// keeps none (purge every qualifying version). The operator-facing default
	// (5) is applied by the config layer — New only guards a negative value to 0,
	// so an explicit 0 stays meaningful.
	DefsKeepLastN int

	// ExportDir is the directory export+prune writes def JSON into (one file per
	// version, under a per-day/per-def-type subdir). Required for export+prune;
	// if empty, the purge is DISABLED (never delete a version we were asked to
	// export but can't).
	ExportDir string

	// DryRun, when true, lists + (in export+prune) exports but NEVER deletes —
	// it only counts what WOULD be purged. Lets an operator preview impact.
	DryRun bool

	// Logger is the structured logger for sweep results. Defaults to log.Printf.
	Logger func(format string, args ...any)

	// AdvisoryLock is the singleton-sweeper gate. When set, each tick acquires
	// the lock before sweeping — only one replica per cluster runs the purge per
	// tick. Nil = single-replica mode. Interface-typed so this package stays free
	// of an internal/coord import.
	AdvisoryLock AdvisoryLocker

	// AdvisoryLockKey is the lock-key int64 (typically
	// coord.LockKeyRetentionSweeper). Only consulted when AdvisoryLock is non-nil.
	AdvisoryLockKey int64

	// Now is the clock sweepOnce reads to compute the age cutoff
	// (cutoff = Now() - DefsMaxAge). Nil defaults to time.Now. Injectable so
	// tests can pin the cutoff deterministically.
	Now func() time.Time
}

// AdvisoryLocker is the minimum surface the sweeper needs from
// internal/coord.AdvisoryLock. Defined here so internal/retention stays free of
// the internal/coord import. *coord.AdvisoryLock satisfies it implicitly.
type AdvisoryLocker interface {
	TryRun(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error)
}

const (
	defaultInterval = 1 * time.Hour
	purgeBatch      = 500   // versions purged per def-type per tick
	reportListCap   = 10000 // DryRunCounts saturates the per-type count at this cap
)

// Result is one sweep's per-def-type + total purge count (deleted, or — under
// DryRun — would-be-deleted).
type Result struct {
	PerType map[string]int
	Total   int
}

// Sweeper periodically exports + purges retired-and-old substrate def versions.
// Construct via New, then call Run(ctx) on a goroutine that owns the lifecycle.
type Sweeper struct {
	store         store.Store
	interval      time.Duration
	defsMode      string
	defsMaxAge    time.Duration
	defsKeepLastN int
	exportDir     string
	dryRun        bool
	logf          func(format string, args ...any)
	lock          AdvisoryLocker
	lockKey       int64
	now           func() time.Time
}

// New constructs a Sweeper. A nil store means "no persistence" — Run is then a
// no-op.
func New(st store.Store, cfg Config) *Sweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	mode := cfg.DefsMode
	if mode == "" {
		mode = "off"
	}
	keep := cfg.DefsKeepLastN
	if keep < 0 {
		// Guard a negative value only. 0 is a valid explicit choice (purge all
		// retired non-active versions); the default of 5 lives in the config layer.
		keep = 0
	}
	return &Sweeper{
		store:         st,
		interval:      cfg.Interval,
		defsMode:      mode,
		defsMaxAge:    cfg.DefsMaxAge,
		defsKeepLastN: keep,
		exportDir:     cfg.ExportDir,
		dryRun:        cfg.DryRun,
		logf:          cfg.Logger,
		lock:          cfg.AdvisoryLock,
		lockKey:       cfg.AdvisoryLockKey,
		now:           cfg.Now,
	}
}

// defsPurgeEnabled reports whether the destructive def purge should run:
// "prune" always, "export+prune" only with a configured ExportDir (never delete
// a version we were asked to export but can't). "off"/unknown → false.
func (s *Sweeper) defsPurgeEnabled() bool {
	switch s.defsMode {
	case "prune":
		return true
	case "export+prune":
		return s.exportDir != ""
	}
	return false
}

// Run drives the sweep loop until ctx is done. The first tick fires after
// Interval (not immediately) so a fresh process doesn't purge the moment it
// boots. Blocks — caller starts it on a goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.logf("retention: sweeper starting (interval=%s, defs_mode=%s, defs_max_age=%s, keep_last_n=%d, dry_run=%v)",
		s.interval, s.defsMode, s.defsMaxAge, s.defsKeepLastN, s.dryRun)
	if s.defsMode == "export+prune" && s.exportDir == "" {
		s.logf("retention: defs purge DISABLED — mode=export+prune requires LOOMCYCLE_RETENTION_EXPORT_DIR")
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()

	// Suppress the no-op log after the first tick until a non-zero sweep or every
	// Nth tick (roughly daily at the default 1h interval) — enough to confirm the
	// goroutine is alive without flooding the log.
	const noOpHeartbeatEvery = 24
	noOpStreak := 0

	for {
		select {
		case <-ctx.Done():
			s.logf("retention: sweeper stopping (ctx done)")
			return
		case <-t.C:
			var (
				res Result
				err error
			)
			doWork := func(ctx context.Context) {
				res, err = s.sweepOnce(ctx)
			}
			if s.lock != nil {
				acquired, lockErr := s.lock.TryRun(ctx, s.lockKey, func(ctx context.Context) error {
					// Return nil so TryRun's err signals ONLY infra failures, kept
					// distinct from the per-sweep failure log below.
					doWork(ctx)
					return nil
				})
				if lockErr != nil {
					s.logf("retention: advisory lock infra error: %v", lockErr)
					continue
				}
				if !acquired {
					// Another replica is running this tick — silent.
					continue
				}
			} else {
				doWork(ctx)
			}
			if err != nil {
				s.logf("retention: sweep failed: %v", err)
			} else if res.Total > 0 {
				verb := "purged"
				if s.dryRun {
					verb = "would purge"
				}
				s.logf("retention: %s %d retired def version(s) across %d type(s) (mode=%s)", verb, res.Total, len(res.PerType), s.defsMode)
				noOpStreak = 0
			} else {
				if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
					s.logf("retention: sweep tick — 0 purgeable def versions (streak=%d)", noOpStreak)
				}
				noOpStreak++
			}
		}
	}
}

// sweepOnce runs one pass over every def-type. Exposed as a method so tests can
// drive it deterministically without a real Ticker. A no-op when the purge is
// disabled (mode=off, or export+prune with no ExportDir). The batch shares one
// 2-minute deadline; per-type failures are logged and skipped so one bad type
// never blocks the others.
func (s *Sweeper) sweepOnce(parent context.Context) (Result, error) {
	res := Result{PerType: map[string]int{}}
	if !s.defsPurgeEnabled() {
		return res, nil
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	cutoff := s.now().Add(-s.defsMaxAge)
	for _, defType := range store.RetentionDefTypes {
		if ctx.Err() != nil {
			break
		}
		refs, err := s.store.ListPurgeableRetiredDefVersions(ctx, defType, cutoff, s.defsKeepLastN, purgeBatch)
		if err != nil {
			s.logf("retention: list %s failed: %v", defType, err)
			continue
		}
		if len(refs) == 0 {
			continue
		}
		ids := make([]string, 0, len(refs))
		for _, ref := range refs {
			if s.defsMode == "export+prune" {
				if err := s.exportDef(ref); err != nil {
					// Never delete a version we failed to export — retry next tick.
					s.logf("retention: export %s/%s failed, skipping delete: %v", defType, ref.DefID, err)
					continue
				}
			}
			ids = append(ids, ref.DefID)
		}
		if len(ids) == 0 {
			continue
		}
		if s.dryRun {
			// Preview only: count what WOULD be deleted, touch nothing.
			res.PerType[defType] += len(ids)
			res.Total += len(ids)
			continue
		}
		n, err := s.store.DeleteDefVersions(ctx, defType, ids)
		if err != nil {
			s.logf("retention: delete %s failed: %v", defType, err)
			continue
		}
		res.PerType[defType] += n
		res.Total += n
	}
	return res, nil
}

// exportDef writes one retired def version to ExportDir as JSON, under a
// per-day/per-def-type subdir (bucketing by the version's created date keeps any
// single directory bounded). The export dir is operator config, never model
// input.
func (s *Sweeper) exportDef(ref store.RetiredDefRef) error {
	day := ref.CreatedAt
	if day.IsZero() {
		day = s.now()
	}
	dir := filepath.Join(s.exportDir, day.UTC().Format("2006-01-02"), ref.DefType)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	// 0o600: a def body may hold sensitive prompt / config material; operator-only.
	return os.WriteFile(filepath.Join(dir, ref.DefID+".json"), blob, 0o600)
}

// DryRunCounts returns the per-def-type count of currently-purgeable versions
// WITHOUT deleting anything — backing the read-only GET /v1/_retention report.
// It reports the counts the CONFIGURED age + keep-last-N would purge regardless
// of DefsMode (so an operator can preview impact before enabling a destructive
// mode). Each count saturates at reportListCap.
func (s *Sweeper) DryRunCounts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	if s.store == nil {
		return out, nil
	}
	cutoff := s.now().Add(-s.defsMaxAge)
	for _, defType := range store.RetentionDefTypes {
		refs, err := s.store.ListPurgeableRetiredDefVersions(ctx, defType, cutoff, s.defsKeepLastN, reportListCap)
		if err != nil {
			return nil, err
		}
		out[defType] = len(refs)
	}
	return out, nil
}
