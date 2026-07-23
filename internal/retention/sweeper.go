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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SQLMemory is the minimal internal/sqlmem.Manager surface the RFC BM Phase 3
// memory-reclamation sweep needs. Defined as an interface so tests can inject a
// fake and so a nil Manager (SQL Memory disabled) cleanly disables only the
// SQL-Memory facet of reclamation (base memory + dirents still reclaim).
// *sqlmem.Manager satisfies it.
type SQLMemory interface {
	// ListScopes enumerates every DURABLE (agent/user) scope. Used to gate
	// export+drop on a scope that actually EXISTS (ExportScope would otherwise
	// provision an empty scope for an agent that never used SQL Memory).
	ListScopes(ctx context.Context) ([]sqlmem.ScopeKey, error)
	// ExportScope returns a logical dump of one scope (export-then-delete).
	ExportScope(ctx context.Context, key sqlmem.ScopeKey) (*sqlmem.ScopeDump, error)
	// DropScope drops one durable scope. removed reports whether it existed.
	DropScope(ctx context.Context, key sqlmem.ScopeKey) (bool, error)
}

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

	// MemMode is the RFC BM Phase 3 memory-reclamation mode: "off" (default / ""),
	// "prune" (reclaim a fully-retired agent's per-scope data), or "export+prune"
	// (dump each facet to ExportDir as JSON, then reclaim). Unknown → off.
	// Independent of DefsMode/ChatsMode. Reclaims THREE facets of a fully-retired
	// (LiveVersionCount==0) + old agent: its SQL-Memory scope (SQL tables +
	// document structure) and its dirents (Path names) — both tenant-qualified, so
	// dropped per (tenant, name) — plus its base-memory k/v (chunk bodies +
	// learned facts), which is keyed by the BARE agent name (the memory table has
	// no tenant column), so it is only dropped when the name is retired in EVERY
	// tenant (globally dead) to avoid deleting a live same-named agent's memory.
	MemMode string

	// MemMaxAge is the age cutoff for reclamation: an agent whose latest def
	// version was updated before Now()-MemMaxAge is eligible. Zero = no minimum age
	// (every fully-retired agent qualifies). Uses AgentDefNameSummary.LastUpdated.
	MemMaxAge time.Duration

	// SQLMem is the SQL Memory manager (nil when SQL Memory is disabled). When nil
	// the mem sweep still reclaims base memory + dirents; only the SQL-Memory scope
	// drop is skipped. Interface-typed for testability.
	SQLMem SQLMemory

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
	purgeBatch      = 500     // versions purged per def-type per tick
	chatsPruneBatch = 500     // aged chat sessions purged per tick
	memReclaimBatch = 500     // retired-agent reclamations per pass per tick
	memExportKeyCap = 1000000 // max base-memory keys exported per agent (safety cap)
	reportListCap   = 10000   // DryRunCounts saturates each count at this cap
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
	// Mem is the number of retired-agent reclamation UNITS this sweep (or, under
	// DryRun, would-be). A unit is one (tenant, name) whose SQL-Memory scope
	// and/or dirents were dropped, PLUS one per name whose globally-dead base
	// memory was dropped — so a single agent that has both a tenant scope and
	// globally-dead base memory counts as two units. It is an activity gauge, not
	// an agent count.
	Mem int
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
	memMode       string
	memMaxAge     time.Duration
	sqlMem        SQLMemory
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
	memMode := cfg.MemMode
	if memMode == "" {
		memMode = "off"
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
		memMode:       memMode,
		memMaxAge:     cfg.MemMaxAge,
		sqlMem:        cfg.SQLMem,
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

// memReclaimEnabled mirrors defsPurgeEnabled for the RFC BM Phase 3
// memory-reclamation sweep: "prune" always, "export+prune" only with a
// configured ExportDir. "off"/unknown → false. Independent of SQLMem being set —
// base memory + dirents reclaim even with SQL Memory disabled.
func (s *Sweeper) memReclaimEnabled() bool {
	switch s.memMode {
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
	s.logf("retention: sweeper starting (interval=%s, defs_mode=%s, defs_max_age=%s, keep_last_n=%d, chats_mode=%s, chats_max_age=%s, mem_mode=%s, mem_max_age=%s, sqlmem=%v, dry_run=%v)",
		s.interval, s.defsMode, s.defsMaxAge, s.defsKeepLastN, s.chatsMode, s.chatsMaxAge, s.memMode, s.memMaxAge, s.sqlMem != nil, s.dryRun)
	if s.defsMode == "export+prune" && s.exportDir == "" {
		s.logf("retention: defs purge DISABLED — mode=export+prune requires LOOMCYCLE_RETENTION_EXPORT_DIR")
	}
	if s.chatsMode == "export+prune" && s.exportDir == "" {
		s.logf("retention: chats purge DISABLED — mode=export+prune requires LOOMCYCLE_RETENTION_EXPORT_DIR")
	}
	if s.memMode == "export+prune" && s.exportDir == "" {
		s.logf("retention: mem reclaim DISABLED — mode=export+prune requires LOOMCYCLE_RETENTION_EXPORT_DIR")
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
			} else if res.Total > 0 || res.Chats > 0 || res.Mem > 0 {
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
				if res.Mem > 0 {
					s.logf("retention: %s %d retired-agent memory reclamation unit(s) (mode=%s)", verb, res.Mem, s.memMode)
				}
				noOpStreak = 0
			} else {
				if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
					s.logf("retention: sweep tick — 0 purgeable def versions / chat sessions / agent memory (streak=%d)", noOpStreak)
				}
				noOpStreak++
			}
		}
	}
}

// sweepOnce runs one pass. The MEM reclaim runs FIRST, before the def-version
// purge: the mem sweep identifies a fully-retired agent via AgentDefListNames, so
// it must run while the agent's def rows still exist — otherwise a same-tick defs
// purge with keep_last_n=0 could remove the agent's last version and orphan its
// SQL-Memory scope + base memory + dirents (nothing left to key the reclaim off).
// Chats is independent of both. Exposed as a method so tests can drive it without
// a real Ticker. A no-op when all three are disabled. Each sub-sweep carries its
// own derived deadline and logs+skips its own failures so none blocks the others.
// Returns nil error even on a per-sub-sweep fault (all are logged); the error
// slot is reserved for a future fatal condition.
func (s *Sweeper) sweepOnce(parent context.Context) (Result, error) {
	res := Result{PerType: map[string]int{}}
	if s.memReclaimEnabled() {
		n, err := s.sweepMemOnce(parent)
		if err != nil {
			s.logf("retention: mem sweep failed: %v", err)
		}
		res.Mem += n
	}
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

// sweepMemOnce reclaims a fully-retired agent's accumulated per-scope data (RFC
// BM Phase 3). An agent whose every def version is retired (LiveVersionCount==0)
// and whose latest version is older than MemMaxAge is dead — but its data spans
// three stores with two different tenant-keying schemes, so reclamation runs in
// two passes:
//
//	Pass 1 (per tenant, name — tenant-safe): drop the agent's SQL-Memory scope
//	(SQL tables + document structure) and its dirents (Path names). Both are keyed
//	(tenant, "agent", name), so dropping tenant A's copy never touches tenant B's.
//
//	Pass 2 (per name, globally-dead only): drop the agent's base-memory k/v
//	(document chunk bodies + learned facts). Base memory is keyed by the BARE
//	agent name (the memory table has no tenant column), so it is SHARED by
//	same-named agents across tenants — dropping it is safe ONLY when the name is
//	retired in EVERY tenant. A name live in any tenant keeps its base memory.
//
// Own 2-minute deadline; per-agent failures are logged + skipped (retried next
// tick); a failed SQL-Memory scope drop aborts that agent's dirent drop so names
// are never orphaned from a still-present scope.
func (s *Sweeper) sweepMemOnce(parent context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()

	pass1, pass2, err := s.memEligible(ctx)
	if err != nil {
		return 0, err
	}

	reclaimed := 0
	// Pass 1 — tenant-qualified SQL-Memory scope + dirents.
	for i, t := range pass1 {
		if ctx.Err() != nil || i >= memReclaimBatch {
			break
		}
		if s.reclaimAgentScope(ctx, t.tenant, t.name, t.hasSQL) {
			reclaimed++
		}
	}
	// Pass 2 — globally-dead base memory (cross-tenant-shared).
	for i, name := range pass2 {
		if ctx.Err() != nil || i >= memReclaimBatch {
			break
		}
		if s.reclaimBaseMemory(ctx, name) {
			reclaimed++
		}
	}
	return reclaimed, nil
}

// memTarget is one pass-1 (tenant-qualified) reclamation candidate.
type memTarget struct {
	tenant string
	name   string
	hasSQL bool // an existing SQL-Memory scope to export+drop
}

// memEligible computes the two reclamation candidate sets shared by the sweep
// and the read-only report, so the eligibility logic can't drift between them.
// pass1 = every fully-retired (LiveVersionCount==0) + old (tenant, name) with its
// SQL-Memory-scope-exists flag. pass2 = every name that is retired in EVERY
// tenant (globally dead) + old AND actually holds base-memory k/v — the only
// names whose cross-tenant-shared base memory is safe to drop.
func (s *Sweeper) memEligible(ctx context.Context) (pass1 []memTarget, pass2 []string, err error) {
	summaries, err := s.store.AgentDefListNames(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Existing SQL-Memory agent scopes, so we export+drop only a scope that
	// actually exists — ExportScope on an absent scope would provision an empty
	// one. Nil SQLMem (SQL Memory disabled) leaves the set empty → pass 1 does
	// dirents only.
	sqlScopes := map[[2]string]bool{}
	if s.sqlMem != nil {
		scopes, lerr := s.sqlMem.ListScopes(ctx)
		if lerr != nil {
			s.logf("retention: mem sweep — list sqlmem scopes failed: %v", lerr)
		}
		for _, k := range scopes {
			if k.Scope == string(store.MemoryScopeAgent) {
				sqlScopes[[2]string{k.Tenant, k.ScopeID}] = true
			}
		}
	}

	// Per-name aggregate for the globally-dead base-memory gate: a name is
	// globally dead iff EVERY (tenant, name) row is fully retired; newest is the
	// most-recent LastUpdated across all its tenant rows.
	type nameAgg struct {
		allDead bool
		newest  time.Time
	}
	byName := map[string]*nameAgg{}
	for _, sm := range summaries {
		agg := byName[sm.Name]
		if agg == nil {
			agg = &nameAgg{allDead: true}
			byName[sm.Name] = agg
		}
		if sm.LiveVersionCount > 0 {
			agg.allDead = false
		}
		if sm.LastUpdated.After(agg.newest) {
			agg.newest = sm.LastUpdated
		}
	}

	cutoff := s.now().Add(-s.memMaxAge)
	for _, sm := range summaries {
		if sm.LiveVersionCount != 0 || !sm.LastUpdated.Before(cutoff) {
			continue
		}
		pass1 = append(pass1, memTarget{
			tenant: sm.TenantID,
			name:   sm.Name,
			hasSQL: sqlScopes[[2]string{sm.TenantID, sm.Name}],
		})
	}
	// Pass 2 = every globally-dead + old name. We deliberately do NOT pre-filter
	// on "has base memory": the only listing available (MemoryListScopeIDs) is
	// capped at the 200 most-RECENTLY-updated scopes, and a retired agent's memory
	// is stale by definition — it would sort out of that window and be skipped
	// forever. reclaimBaseMemory instead calls MemoryDeleteScope directly (a
	// no-op returning 0 for a name with no memory), and the report probes each
	// name for existence, so neither depends on the 200-cap.
	for name, agg := range byName {
		if agg.allDead && agg.newest.Before(cutoff) {
			pass2 = append(pass2, name)
		}
	}
	return pass1, pass2, nil
}

// hasBaseMemory reports whether a name holds ANY base-memory k/v, via a limit-1
// existence probe (NOT MemoryListScopeIDs, which is capped at the 200
// most-recently-updated scopes and so would miss a stale retired agent). Used by
// the dry-run + report count so they match what the real sweep would drop.
func (s *Sweeper) hasBaseMemory(ctx context.Context, name string) bool {
	entries, _, err := s.store.MemoryList(ctx, "", store.MemoryScopeAgent, name, "", 1)
	return err == nil && len(entries) > 0
}

// reclaimAgentScope drops one fully-retired agent's TENANT-QUALIFIED per-scope
// data: its SQL-Memory scope (when hasSQL) and its dirents. Returns true if
// anything was reclaimed. export+prune exports each facet before dropping it
// (export-then-delete); a failed export or a failed SQL-Memory drop skips the
// rest for this agent so nothing is half-reclaimed. Under DryRun it neither
// exports nor deletes — it only reports whether there is something to reclaim
// (the read-only GET /v1/_retention report is the intended preview; SQL-Memory
// dumps are too heavy to write on a preview tick).
func (s *Sweeper) reclaimAgentScope(ctx context.Context, tenant, name string, hasSQL bool) bool {
	if s.dryRun {
		if hasSQL {
			return true
		}
		rows, err := s.store.DirentListUnder(ctx, tenant, string(store.MemoryScopeAgent), name, "/")
		return err == nil && len(rows) > 0
	}

	did := false
	if hasSQL && s.sqlMem != nil {
		key := sqlmem.ScopeKey{Tenant: tenant, Scope: string(store.MemoryScopeAgent), ScopeID: name}
		if s.memMode == "export+prune" {
			if err := s.exportSQLMemScope(ctx, tenant, name, key); err != nil {
				s.logf("retention: export sqlmem scope %s/%s failed, skipping agent: %v", tenant, name, err)
				return false
			}
		}
		removed, err := s.sqlMem.DropScope(ctx, key)
		if err != nil {
			// Don't drop the dirents out from under a still-present scope.
			s.logf("retention: drop sqlmem scope %s/%s failed, skipping agent: %v", tenant, name, err)
			return false
		}
		did = removed
	}

	// dirents (Path names). "/" is the scope root — its descendants are every
	// named entry in the scope.
	if s.memMode == "export+prune" {
		if err := s.exportDirents(ctx, tenant, name); err != nil {
			s.logf("retention: export dirents %s/%s failed, skipping dirent delete: %v", tenant, name, err)
			return did
		}
	}
	n, err := s.store.DirentDeleteUnder(ctx, tenant, string(store.MemoryScopeAgent), name, "/")
	if err != nil {
		s.logf("retention: delete dirents %s/%s failed: %v", tenant, name, err)
	} else if n > 0 {
		did = true
	}
	return did
}

// reclaimBaseMemory drops a globally-dead agent's base-memory k/v (every key
// under scope=agent, scope_id=name). The caller MUST have verified the name is
// retired in every tenant (base memory has no tenant column). export+prune dumps
// the entries first. Under DryRun it reports would-reclaim without touching rows.
func (s *Sweeper) reclaimBaseMemory(ctx context.Context, name string) bool {
	if s.dryRun {
		return s.hasBaseMemory(ctx, name) // count only names that actually have memory
	}
	if s.memMode == "export+prune" {
		if err := s.exportBaseMemory(ctx, name); err != nil {
			s.logf("retention: export agent memory %q failed, skipping: %v", name, err)
			return false
		}
	}
	n, err := s.store.MemoryDeleteScope(ctx, "", store.MemoryScopeAgent, name)
	if err != nil {
		s.logf("retention: delete agent memory %q failed: %v", name, err)
		return false
	}
	return n > 0
}

// exportSQLMemScope dumps one agent SQL-Memory scope to
// <ExportDir>/agents/<day>/sqlmem/<tenant>__<name>.json before it is dropped.
func (s *Sweeper) exportSQLMemScope(ctx context.Context, tenant, name string, key sqlmem.ScopeKey) error {
	dump, err := s.sqlMem.ExportScope(ctx, key)
	if err != nil {
		return err
	}
	return s.writeMemExport("sqlmem", tenant, name, dump)
}

// exportDirents dumps one agent scope's dirents (Path names) before they are
// deleted. A scope with no dirents writes nothing.
func (s *Sweeper) exportDirents(ctx context.Context, tenant, name string) error {
	rows, err := s.store.DirentListUnder(ctx, tenant, string(store.MemoryScopeAgent), name, "/")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return s.writeMemExport("dirents", tenant, name, rows)
}

// exportBaseMemory dumps one globally-dead agent's base-memory k/v before it is
// deleted. Bucketed under agent-memory/ with NO tenant segment (base memory is
// cross-tenant-shared). If the entry set is truncated at the cap, it returns an
// ERROR so the caller skips the delete — export-then-delete must never delete
// more than it exported. Such an agent (>memExportKeyCap keys) is retried each
// tick, staying put with a loud warning rather than losing the un-exported tail.
func (s *Sweeper) exportBaseMemory(ctx context.Context, name string) error {
	entries, truncated, err := s.store.MemoryList(ctx, "", store.MemoryScopeAgent, name, "", memExportKeyCap)
	if err != nil {
		return err
	}
	if truncated {
		return fmt.Errorf("agent memory %q exceeds the %d-key export cap — refusing to delete more than exported", name, memExportKeyCap)
	}
	if len(entries) == 0 {
		return nil
	}
	return s.writeMemExport("agent-memory", "", name, entries)
}

// writeMemExport marshals one reclamation-export payload to
// <ExportDir>/agents/<day>/<kind>/<tenant>__<name>.json (0600). Agent names may
// contain '/' (name grouping) and tenants are operator-controlled, so both are
// filesystem-sanitized to keep every export a single flat file. The export dir
// is operator config, never model input.
func (s *Sweeper) writeMemExport(kind, tenant, name string, payload any) error {
	day := s.now().UTC().Format("2006-01-02")
	dir := filepath.Join(s.exportDir, "agents", day, kind)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	fname := fsSafe(tenant) + "__" + fsSafe(name) + ".json"
	blob, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	// 0o600: an agent's memory/SQL data may hold sensitive material; operator-only.
	return os.WriteFile(filepath.Join(dir, fname), blob, 0o600)
}

// fsSafe replaces path separators + control characters in an export filename
// component so a '/'-grouped agent name or an odd tenant can't escape the export
// subtree or create nested directories.
func fsSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r < 0x20 {
			return '_'
		}
		return r
	}, s)
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

	// Retired-agent memory-reclamation preview — mode-independent, keyed "mem"
	// (never a def-type name). Counts the same reclamation units the sweep would
	// take: pass-1 (tenant, name) with an SQL-Memory scope OR dirents, plus every
	// globally-dead pass-2 name that holds base memory. Saturates at reportListCap.
	memCount, err := s.countReclaimableMem(ctx)
	if err != nil {
		return nil, err
	}
	out["mem"] = memCount
	return out, nil
}

// countReclaimableMem counts the reclamation units the mem sweep would take
// right now, WITHOUT touching anything (backs the GET /v1/_retention report). It
// reuses memEligible so the count can't drift from the sweep; a pass-1 candidate
// counts only when it has something to reclaim (an SQL-Memory scope OR dirents).
func (s *Sweeper) countReclaimableMem(ctx context.Context) (int, error) {
	pass1, pass2, err := s.memEligible(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	// Pass 2: count only names that actually hold base memory (probe, not the
	// 200-capped MemoryListScopeIDs).
	for _, name := range pass2 {
		if count >= reportListCap {
			break
		}
		if s.hasBaseMemory(ctx, name) {
			count++
		}
	}
	for _, t := range pass1 {
		if count >= reportListCap {
			break
		}
		if t.hasSQL {
			count++
			continue
		}
		rows, derr := s.store.DirentListUnder(ctx, t.tenant, string(store.MemoryScopeAgent), t.name, "/")
		if derr == nil && len(rows) > 0 {
			count++
		}
	}
	if count > reportListCap {
		count = reportListCap
	}
	return count, nil
}
