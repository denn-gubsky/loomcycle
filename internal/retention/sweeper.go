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
//
// RFC BM Phase 2 adds a second, independent family: aged CHAT sessions (a
// session whose runs are all terminal + old, plus its runs + events), gated by
// ChatsMode with the same off / prune / export+prune semantics and the same
// export-then-delete guarantee. Pinned sessions are always exempt. This subsumes
// the RFC AV Phase 2b2 aged-session archiver in internal/usage — cmd/loomcycle
// disables that one whenever this chats sub-sweep is active so a session is never
// deleted by two sweepers at once.
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

	// ChatsMode is the RFC BM Phase 2 aged-chat archiver mode: "off"
	// (default / ""), "prune" (cascade-delete aged sessions + their runs +
	// events), or "export+prune" (write each session bundle to ExportDir as JSON,
	// then delete). Unknown → off. Independent of DefsMode — a deployment can
	// purge defs, chats, both, or neither. This subsumes the RFC AV Phase 2b2
	// usage-sweeper aged-session archiver (main.go disables that one when this is
	// on so the two never double-run). A PINNED session is always exempt (the
	// store's PrunableAgedSessions excludes it).
	ChatsMode string

	// ChatsMaxAge is the age cutoff for the chats archiver: only sessions whose
	// most-recent completed_at is before Now()-ChatsMaxAge are eligible. Zero = no
	// minimum age (every all-terminal, non-pinned session qualifies).
	ChatsMaxAge time.Duration

	// ExportDir is the directory export+prune writes JSON into: def versions under
	// <ExportDir>/<day>/<def-type>/, chat sessions under <ExportDir>/chats/<day>/.
	// Required for export+prune (defs OR chats); if empty, the corresponding
	// export+prune purge is DISABLED (never delete something we were asked to
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
	chatsPruneBatch = 500   // aged chat sessions purged per tick
	reportListCap   = 10000 // DryRunCounts saturates each count at this cap
)

// Result is one sweep's per-def-type + total purge count (deleted, or — under
// DryRun — would-be-deleted), plus the aged-chat-session count.
type Result struct {
	PerType map[string]int
	Total   int
	// Chats is the number of aged chat sessions deleted (or, under DryRun,
	// would-be-deleted) this sweep. Counted separately from the def totals above:
	// a session is not a def version.
	Chats int
}

// Sweeper periodically exports + purges retired-and-old substrate def versions.
// Construct via New, then call Run(ctx) on a goroutine that owns the lifecycle.
type Sweeper struct {
	store         store.Store
	interval      time.Duration
	defsMode      string
	defsMaxAge    time.Duration
	defsKeepLastN int
	chatsMode     string
	chatsMaxAge   time.Duration
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
	chatsMode := cfg.ChatsMode
	if chatsMode == "" {
		chatsMode = "off"
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
		chatsMode:     chatsMode,
		chatsMaxAge:   cfg.ChatsMaxAge,
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

// chatsPruneEnabled mirrors defsPurgeEnabled for the aged-chat archiver:
// "prune" always, "export+prune" only with a configured ExportDir (never delete
// a session we were asked to export but can't). "off"/unknown → false.
func (s *Sweeper) chatsPruneEnabled() bool {
	switch s.chatsMode {
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
	s.logf("retention: sweeper starting (interval=%s, defs_mode=%s, defs_max_age=%s, keep_last_n=%d, chats_mode=%s, chats_max_age=%s, dry_run=%v)",
		s.interval, s.defsMode, s.defsMaxAge, s.defsKeepLastN, s.chatsMode, s.chatsMaxAge, s.dryRun)
	if s.defsMode == "export+prune" && s.exportDir == "" {
		s.logf("retention: defs purge DISABLED — mode=export+prune requires LOOMCYCLE_RETENTION_EXPORT_DIR")
	}
	if s.chatsMode == "export+prune" && s.exportDir == "" {
		s.logf("retention: chats purge DISABLED — mode=export+prune requires LOOMCYCLE_RETENTION_EXPORT_DIR")
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
			} else if res.Total > 0 || res.Chats > 0 {
				verb := "purged"
				if s.dryRun {
					verb = "would purge"
				}
				if res.Total > 0 {
					s.logf("retention: %s %d retired def version(s) across %d type(s) (mode=%s)", verb, res.Total, len(res.PerType), s.defsMode)
				}
				if res.Chats > 0 {
					s.logf("retention: %s %d aged chat session(s) (mode=%s)", verb, res.Chats, s.chatsMode)
				}
				noOpStreak = 0
			} else {
				if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
					s.logf("retention: sweep tick — 0 purgeable def versions / chat sessions (streak=%d)", noOpStreak)
				}
				noOpStreak++
			}
		}
	}
}

// sweepOnce runs one pass: the def-version purge (when defsPurgeEnabled) followed
// by the aged-chat-session purge (when chatsPruneEnabled). Exposed as a method so
// tests can drive it deterministically without a real Ticker. A no-op when both
// are disabled. The two sub-sweeps are independent — each carries its own derived
// deadline and its own failures are logged and skipped so neither can block the
// other. Returns nil error even on a per-sub-sweep fault (both are logged); the
// error slot is reserved for a future fatal condition.
func (s *Sweeper) sweepOnce(parent context.Context) (Result, error) {
	res := Result{PerType: map[string]int{}}
	if s.defsPurgeEnabled() {
		s.sweepDefsOnce(parent, &res)
	}
	if s.chatsPruneEnabled() {
		n, err := s.sweepChatsOnce(parent)
		if err != nil {
			s.logf("retention: chats sweep failed: %v", err)
		}
		res.Chats += n
	}
	return res, nil
}

// sweepDefsOnce purges one batch of retired-and-old def versions per def-type,
// accumulating into res. The batch shares one 2-minute deadline; per-type
// failures are logged and skipped so one bad type never blocks the others.
func (s *Sweeper) sweepDefsOnce(parent context.Context, res *Result) {
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
}

// sweepChatsOnce archives one batch of aged chat sessions (RFC BM Phase 2): list
// sessions whose runs are ALL terminal + old (pinned sessions are excluded by the
// store), export each (export+prune mode) then cascade-delete it (session + runs
// + events). Prunes by SESSION, not by run — the continuation path replays the
// whole-session transcript, so pruning one aged run inside a still-continued
// session would corrupt it. This subsumes usage.archiveSessionsOnce (main.go
// disables that archiver when this sub-sweep is on so the two never double-run).
// Own 2-minute deadline (export writes files); per-session failures are logged +
// skipped, and the batch breaks cleanly once the shared deadline expires.
func (s *Sweeper) sweepChatsOnce(parent context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	cutoff := s.now().Add(-s.chatsMaxAge)
	sessions, err := s.store.PrunableAgedSessions(ctx, cutoff, chatsPruneBatch)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, sid := range sessions {
		// The whole batch shares one deadline; once it expires every remaining
		// export/DeleteSessionCascade would fail with the same ctx error — stop
		// cleanly and return what we finished. The next tick resumes from where
		// this one stopped (PrunableAgedSessions is re-queried).
		if ctx.Err() != nil {
			break
		}
		if s.chatsMode == "export+prune" {
			if err := s.exportSession(ctx, sid); err != nil {
				// Never delete a session we failed to export — retry next tick.
				s.logf("retention: export chat session %s failed, skipping delete: %v", sid, err)
				continue
			}
		}
		if s.dryRun {
			// Preview only: count what WOULD be deleted (export, if any, has run —
			// mirroring the defs dry-run), touch no rows.
			pruned++
			continue
		}
		if err := s.store.DeleteSessionCascade(ctx, sid); err != nil {
			s.logf("retention: delete chat session %s failed: %v", sid, err)
			continue
		}
		pruned++
	}
	return pruned, nil
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

// exportSession writes one aged chat session (its runs + all events) to
// <ExportDir>/chats/<day>/<sid>.json, bucketed by the session's latest completed
// date so any single directory stays bounded. Lifted from usage.exportSession —
// the RFC BM chats sweeper subsumes the RFC AV aged-session archiver; the
// "chats/" segment keeps these bundles separate from the def-version exports
// under the same ExportDir. The export dir is operator config, never model input.
func (s *Sweeper) exportSession(ctx context.Context, sessionID string) error {
	runs, err := s.store.RunsForSession(ctx, sessionID)
	if err != nil {
		return err
	}
	events, err := s.store.GetTranscript(ctx, sessionID)
	if err != nil {
		return err
	}
	// Bucket by the session's latest completed_at (fall back to latest start,
	// then to now if a session somehow has no timestamps).
	var day time.Time
	for _, r := range runs {
		if !r.CompletedAt.IsZero() && r.CompletedAt.After(day) {
			day = r.CompletedAt
		}
	}
	if day.IsZero() {
		for _, r := range runs {
			if r.StartedAt.After(day) {
				day = r.StartedAt
			}
		}
	}
	if day.IsZero() {
		day = s.now()
	}
	dir := filepath.Join(s.exportDir, "chats", day.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	payload := struct {
		SessionID string        `json:"session_id"`
		Runs      []store.Run   `json:"runs"`
		Events    []store.Event `json:"events"`
	}{SessionID: sessionID, Runs: runs, Events: events}
	blob, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	// 0o600: run transcripts may hold sensitive tool I/O; operator-only.
	return os.WriteFile(filepath.Join(dir, sessionID+".json"), blob, 0o600)
}

// DryRunCounts returns the count of currently-purgeable items per def-type PLUS
// an aged-chat-session count under the "chats" key, WITHOUT deleting anything —
// backing the read-only GET /v1/_retention report. It reports what the CONFIGURED
// age + keep-last-N (defs) / age (chats) would purge regardless of mode (so an
// operator can preview impact before enabling a destructive mode). Each count
// saturates at reportListCap.
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
	// Aged-chat sessions preview — mode-independent, like the def counts. Keyed
	// "chats" (never a def-type name, so no collision). Pinned sessions are
	// excluded by the store.
	chatsCutoff := s.now().Add(-s.chatsMaxAge)
	sessions, err := s.store.PrunableAgedSessions(ctx, chatsCutoff, reportListCap)
	if err != nil {
		return nil, err
	}
	out["chats"] = len(sessions)
	return out, nil
}
