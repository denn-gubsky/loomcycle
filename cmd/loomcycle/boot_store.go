package main

import (
	"context"
	"log"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/store/cdc"
)

// openAndMigrateStore is boot phase 5: open the store (opening it migrates
// the schema; on Postgres, when automigrate is on), wrap it for the change
// feed when that is on, backfill legacy rows and seed the yaml memory
// entries.
func (b *boot) openAndMigrateStore() {
	cfg := b.cfg
	embedder := b.embedder

	// Storage: SQLite (default, compact installs) or Postgres
	// (production, hundreds of concurrent agents). Both adapters
	// implement the same store.Store interface; they're tested against
	// a shared contract suite in CI so they can't drift silently.
	storeIface, storeCloser, err := openStore(cfg)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	b.addCleanup(storeCloser)
	// RFC CD Part C — when the change-data-capture feed is enabled, wrap the
	// store so every memory/document write emits a value-free change row. Off by
	// default → the store is unwrapped and the path is byte-identical. Cluster
	// coord below unwraps via asPostgresStore to reach the concrete backend.
	if memoryChangesEnabled() && storeIface != nil {
		storeIface = cdc.Wrap(storeIface, slog.Default())
		log.Printf("memory: change-data-capture feed ENABLED (LOOMCYCLE_MEMORY_CHANGES_ENABLED=1)")
	}

	// v0.9.x content_sha256 backfill — populate the new column on
	// every NULL/empty row left over from a pre-v0.9.x upgrade OR
	// restored from a pre-v0.9.x snapshot. Idempotent + fast: a
	// second boot scans zero NULL rows. Failures here don't fatal
	// the start — the column just stays NULL and `verify` returns
	// matches=false until the row is naturally re-touched via
	// AgentDef set/fork.
	bfCtx, bfCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if n, err := storeIface.BackfillAgentDefContentSHA256(bfCtx, backfillAgentDefSignFn); err != nil {
		log.Printf("agent_defs: backfill content_sha256 partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("agent_defs: backfilled %d rows with content_sha256", n)
	}
	if n, err := storeIface.BackfillSkillDefContentSHA256(bfCtx, backfillSkillDefSignFn); err != nil {
		log.Printf("skill_defs: backfill content_sha256 partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("skill_defs: backfilled %d rows with content_sha256", n)
	}
	// v0.9.x mcp_server_defs backfill — shares the same 30-second
	// bfCtx so the cumulative "boot blocked on slow store" wait is
	// bounded. Documented best-effort posture as for the other two.
	if n, err := storeIface.BackfillMCPServerDefContentSHA256(bfCtx, backfillMCPServerDefSignFn); err != nil {
		log.Printf("mcp_server_defs: backfill content_sha256 partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("mcp_server_defs: backfilled %d rows with content_sha256", n)
	}
	// v0.9.x system_prompt_base backfill — fills the field that PR #186's
	// read-side normalizer derives at runtime. Closes the on-disk data
	// gap for legacy rows; new rows persist the field directly via the
	// write-side mergedDef.normalize() (agentdef.go). Idempotent: a
	// second boot after a full backfill finds zero rows missing the
	// field. Shares the same bfCtx — bounded boot wait.
	if n, err := storeIface.BackfillAgentDefSystemPromptBase(bfCtx); err != nil {
		log.Printf("agent_defs: backfill system_prompt_base partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("agent_defs: backfilled %d rows with system_prompt_base", n)
	}
	bfCancel()

	// v0.11.5 yaml-static memory entries — pre-seed the substrate from
	// the operator-yaml `memory.entries:` block. Idempotent: each
	// (scope, scope_id, key) tuple is skipped if a row already exists
	// (preserves runtime updates from operators / agents that may have
	// rewritten the value between boots). Synchronous: a slow embedder
	// will delay boot — logged below per-entry so the cost is visible.
	if len(cfg.Memory.Entries) > 0 {
		// Seeding is maintenance: its embeddings queue as background work.
		bootstrapMemoryEntries(lanegate.WithBackground(context.Background()), cfg, storeIface, embedder)
	}

	b.storeIface = storeIface
}
