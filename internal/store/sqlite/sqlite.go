// Package sqlite implements store.Store backed by SQLite.
//
// Two build modes:
//
//   - Default (no build tag): modernc.org/sqlite — pure Go, no CGO.
//     Single static binary. Vector Memory refuses with
//     ErrVectorUnsupported (see internal/store/sqlite/memory_embeddings.go).
//   - `-tags=sqlite_vec`: github.com/mattn/go-sqlite3 — CGO,
//     loads the sqlite-vec extension at Open() time so Vector
//     Memory works. Requires LOOMCYCLE_SQLITE_VEC_PATH pointing
//     at the extension shared library (e.g. /usr/local/lib/vec0).
//     Operator chooses the trade: portable static binary vs
//     vector-capable CGO binary.
//
// Driver name + DSN are picked by driver_default.go (default tag)
// vs driver_vec.go (`-tags=sqlite_vec`). The rest of this file is
// driver-agnostic.
//
// Single-file database; WAL journal mode for concurrent readers
// during a write.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Store is the SQLite implementation of store.Store.
type Store struct {
	db *sql.DB

	// channelDebug gates the v0.12.7 channel publish diagnostic log.
	// Set via SetChannelDebug after Open() — the SQLite store predates
	// a Config struct, so we wire the operator's LOOMCYCLE_CHANNEL_DEBUG
	// preference in via a setter rather than adding a parallel Open
	// variant. Defaults to off so noise stays out of production logs.
	channelDebug bool

	// closeOnce guards the Close() idempotency contract.
	closeOnce sync.Once
}

// Open opens (or creates) a SQLite database at path and applies the schema.
// path may be an OS path or ":memory:" for an ephemeral test DB.
// The driver name + DSN format are picked by openDB() — see
// driver_default.go (pure-Go modernc) or driver_vec.go (CGO mattn +
// sqlite-vec extension loading), depending on build tag.
func Open(path string) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is single-writer. Cap the connection pool to avoid
	// SQLITE_BUSY storms; one writer + a few readers is plenty for v0.3.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// SetChannelDebug opts the store into the v0.12.7 channel publish
// diagnostic log. See ChannelPublish for what gets logged. Idempotent;
// safe to call from main.go before serving begins. After serving starts
// the flag is read-only on the hot path (no atomic; calling this
// concurrently with a publish would race).
func (s *Store) SetChannelDebug(enabled bool) {
	s.channelDebug = enabled
}

// migrate creates the schema if needed. Idempotent. v0.3 schema is fixed; if
// we add columns post-1.0 we'll add a versioned migration table.
//
// The two phases below are separated because:
//   - Phase 1 (CREATE) is unconditionally idempotent (IF NOT EXISTS).
//   - Phase 2 (ALTER ADD COLUMN) is NOT idempotent in SQLite — re-running
//     the same ADD on an existing column returns "duplicate column name".
//     We swallow exactly that error so a second startup is a no-op without
//     introducing a versioned migrations table for v0.4.
func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent      TEXT NOT NULL,
			created_at INTEGER NOT NULL  -- unix nano
		)`,
		`CREATE TABLE IF NOT EXISTS runs (
			id                       TEXT PRIMARY KEY,
			session_id               TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			status                   TEXT NOT NULL,
			started_at               INTEGER NOT NULL,
			completed_at             INTEGER,
			stop_reason              TEXT,
			input_tokens             INTEGER NOT NULL DEFAULT 0,
			output_tokens            INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens    INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
			model                    TEXT,
			-- v0.12.7+ — actual provider that served the final
			-- successful iteration. Distinct from model so post-run
			-- analysis can tell primary from fallback routing.
			provider                 TEXT,
			error                    TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS runs_by_session ON runs(session_id)`,
		`CREATE TABLE IF NOT EXISTS events (
			seq        INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			ts         INTEGER NOT NULL,
			type       TEXT NOT NULL,
			payload    BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS events_by_session ON events(session_id, seq)`,
		// v0.8.21 audit view — cross-session queries by ts (descending)
		// and ts-with-type-equality. Mirrors the postgres
		// 0014_events_audit_index migration; both stores need the
		// indexes to avoid full-table scans on busy installs.
		`CREATE INDEX IF NOT EXISTS events_by_ts ON events(ts DESC)`,
		`CREATE INDEX IF NOT EXISTS events_by_type_ts ON events(type, ts DESC)`,
		// v0.8.21 awaited-state derivation needs the last event per
		// run cheaply for every running agent — (run_id, seq DESC)
		// is the covering shape.
		`CREATE INDEX IF NOT EXISTS events_by_run_seq ON events(run_id, seq DESC)`,
		// v0.8 Memory tool. PRIMARY KEY (scope, scope_id, key) gives
		// the natural lookup index; the partial expires_at index keeps
		// the sweeper's DELETE cheap (no full-table scan).
		`CREATE TABLE IF NOT EXISTS memory (
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL,
			key         TEXT NOT NULL,
			value       TEXT NOT NULL,
			expires_at  INTEGER,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (scope, scope_id, key)
		)`,
		`CREATE INDEX IF NOT EXISTS memory_by_expires_at ON memory(expires_at) WHERE expires_at IS NOT NULL`,
		// v0.8.4 Channel tool — see internal/store/postgres/migrations/0004_channels.up.sql
		// for the full rationale. SQLite mirrors the shape: TEXT id (ULID-like prefix
		// "msg_<unixnano>_<rand>" — sortable by publish time), per-(channel, scope,
		// scope_id) composite PK so per-subscriber scans are index lookups. payload is
		// TEXT-encoded JSON because SQLite doesn't have a native JSONB type.
		//
		// v0.8.6 system channels: visible_at + published_by_user_id columns
		// land via the addColumns block below (idempotent ALTER pattern,
		// works against both fresh + existing v0.8.4 schemas).
		`CREATE TABLE IF NOT EXISTS channel_messages (
			id                   TEXT    NOT NULL,
			channel              TEXT    NOT NULL,
			scope                TEXT    NOT NULL,
			scope_id             TEXT    NOT NULL,
			payload              TEXT    NOT NULL,
			published_at         INTEGER NOT NULL,
			expires_at           INTEGER,
			visible_at           INTEGER NOT NULL DEFAULT 0,
			published_by_user_id TEXT,
			PRIMARY KEY (channel, scope, scope_id, id)
		)`,
		`CREATE INDEX IF NOT EXISTS channel_messages_by_expires_at ON channel_messages(expires_at) WHERE expires_at IS NOT NULL`,
		// NOTE: channel_messages_by_visible is created in `addIndexes`
		// below, AFTER the `addColumns` block adds the visible_at
		// column. On a fresh deploy the CREATE TABLE above already
		// declares visible_at so the index could be created here too;
		// on an UPGRADE from v0.8.4/v0.8.5 (channel_messages exists
		// without visible_at), the CREATE TABLE IF NOT EXISTS is a
		// no-op and creating the index here would fail with
		// "no such column: visible_at". Keep the index in addIndexes.
		`CREATE TABLE IF NOT EXISTS channel_cursors (
			channel    TEXT    NOT NULL,
			scope      TEXT    NOT NULL,
			scope_id   TEXT    NOT NULL,
			cursor     TEXT    NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (channel, scope, scope_id)
		)`,
		// v0.11.5 runtime-declared channels. yaml-declared channels
		// stay in cfg.Channels (in-memory only); this table holds
		// channels created via the POST /v1/_channels admin endpoint
		// or the Web UI. The HTTP handler merges both at read time
		// with a `source` discriminator. Cascade delete of messages +
		// cursors handled in code (channel_messages doesn't FK to
		// here — yaml channels never had a parent row, so the table
		// can't have a FK to itself).
		`CREATE TABLE IF NOT EXISTS channels (
			name         TEXT    PRIMARY KEY,
			description  TEXT    NOT NULL DEFAULT '',
			scope        TEXT    NOT NULL,
			semantic     TEXT    NOT NULL,
			default_ttl  INTEGER NOT NULL DEFAULT 0,
			max_messages INTEGER NOT NULL DEFAULT 0,
			publisher    TEXT    NOT NULL DEFAULT '',
			period       TEXT    NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL
		)`,
		// v0.8.5 Self-Evolution Substrate — see
		// internal/store/postgres/migrations/0006_agent_defs.up.sql for
		// the full design rationale. SQLite mirrors the shape; INTEGER
		// boolean (0/1) instead of Postgres BOOLEAN, unix-nano INTEGER
		// timestamps instead of TIMESTAMPTZ, TEXT JSON instead of JSONB.
		`CREATE TABLE IF NOT EXISTS agent_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES agent_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			content_sha256            TEXT,
			UNIQUE(name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_name   ON agent_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_parent ON agent_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_run    ON agent_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// agent_defs_by_content_sha256 is created in addIndexes below
		// (runs AFTER addColumns adds the column on upgrade-from-v0.8.x
		// DBs where this CREATE TABLE IF NOT EXISTS is a no-op).
		`CREATE TABLE IF NOT EXISTS agent_def_active (
			name                  TEXT    PRIMARY KEY,
			def_id                TEXT    NOT NULL REFERENCES agent_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT
		)`,
		// v0.8.22 SkillDef substrate — mirror of agent_defs with
		// the same identity / lineage / promotion semantics. The
		// `definition` column carries the JSON-encoded skill body
		// + metadata (body / description / allowed_tools).
		`CREATE TABLE IF NOT EXISTS skill_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES skill_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			content_sha256            TEXT,
			UNIQUE(name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_name   ON skill_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_parent ON skill_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_run    ON skill_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// skill_defs_by_content_sha256 — see agent_defs_by_content_sha256 note above.
		`CREATE TABLE IF NOT EXISTS skill_def_active (
			name                  TEXT    PRIMARY KEY,
			def_id                TEXT    NOT NULL REFERENCES skill_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT
		)`,
		// v0.9.x MCPServerDef substrate — third member of the
		// AgentDef / SkillDef / MCPServerDef family. Mirror of the
		// agent_defs / skill_defs schema; the definition JSONB carries
		// the operator-authored connection metadata (transport / url
		// / headers) plus the cached discovered_tools list refreshed
		// via the tool's `rediscover` op. content_sha256 is computed
		// over the content-bearing subset (excluding discovered_tools).
		`CREATE TABLE IF NOT EXISTS mcp_server_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES mcp_server_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			content_sha256            TEXT,
			UNIQUE(name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_name   ON mcp_server_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_parent ON mcp_server_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_run    ON mcp_server_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// mcp_server_defs_by_content_sha256 — see agent_defs_by_content_sha256 note above.
		`CREATE TABLE IF NOT EXISTS mcp_server_def_active (
			name                  TEXT    PRIMARY KEY,
			def_id                TEXT    NOT NULL REFERENCES mcp_server_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT
		)`,
		// v1.x RFC E Scheduled Agent Runs substrate — mirror of
		// agent_defs/skill_defs/mcp_server_defs with the same identity
		// + lineage + promotion semantics. The `definition` column
		// carries the JSON-encoded schedule body (agent, cron,
		// user_id, user_credentials, on_complete, etc.). See
		// internal/store/postgres/migrations/0029_schedule_defs.up.sql
		// for the full design rationale.
		`CREATE TABLE IF NOT EXISTS schedule_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES schedule_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			UNIQUE(name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_defs_by_name   ON schedule_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS schedule_defs_by_parent ON schedule_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS schedule_defs_by_run    ON schedule_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS schedule_def_active (
			name                  TEXT    PRIMARY KEY,
			def_id                TEXT    NOT NULL REFERENCES schedule_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT
		)`,
		// schedule_run_state — sweeper's runtime view of last/next
		// per def. One row per active schedule; FK + ON DELETE CASCADE
		// so retiring via DELETE auto-cleans state.
		`CREATE TABLE IF NOT EXISTS schedule_run_state (
			def_id          TEXT    PRIMARY KEY REFERENCES schedule_defs(def_id) ON DELETE CASCADE,
			last_run_at     INTEGER,
			last_run_id     TEXT,
			last_status     TEXT,
			last_error      TEXT,
			next_run_at     INTEGER NOT NULL,
			paused_until    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_run_state_due ON schedule_run_state(next_run_at)`,
		// v0.8.5 evaluations table. emitter_role is server-derived in
		// the tool layer; the store stores the string verbatim. Score
		// is REAL (Go float64). Dimensions + Judgement are JSON-as-TEXT
		// (sqlite has no JSONB).
		//
		// NO foreign keys on run_id or def_id: evaluations are an
		// immutable audit log and must survive any future run/def
		// pruning. Referential integrity is enforced at the
		// application layer. A RESTRICT FK would block legitimate
		// admin pruning workflows; CASCADE would silently delete
		// audit data. Mirrors the postgres migration 0008.
		`CREATE TABLE IF NOT EXISTS evaluations (
			eval_id            TEXT    PRIMARY KEY,
			run_id             TEXT    NOT NULL,
			def_id             TEXT,
			score              REAL    NOT NULL,
			dimensions         TEXT,
			judgement          TEXT,
			rationale          TEXT,
			emitter_role       TEXT    NOT NULL,
			emitter_agent_id   TEXT,
			emitter_run_id     TEXT,
			created_at         INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS evaluations_by_run     ON evaluations(run_id)`,
		`CREATE INDEX IF NOT EXISTS evaluations_by_def     ON evaluations(def_id) WHERE def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS evaluations_by_emitter ON evaluations(emitter_agent_id) WHERE emitter_agent_id IS NOT NULL`,
		// v0.8.x Process-resource metrics sampler. process_samples
		// is a time-series table: one row per sample tick while
		// at least one agent run is active. NULLable system_* fields
		// are populated only when LOOMCYCLE_METRICS_COLLECT_SYSTEM=1.
		// See internal/metrics/sampler.go for the write path and
		// the API endpoints under /v1/_metrics/* for read paths.
		`CREATE TABLE IF NOT EXISTS process_samples (
			sample_id                  TEXT    PRIMARY KEY,
			replica_id                 TEXT,
			sampled_at                 INTEGER NOT NULL,
			active_runs                INTEGER NOT NULL,
			queued_runs                INTEGER NOT NULL,
			loomcycle_rss_bytes        INTEGER NOT NULL DEFAULT 0,
			loomcycle_heap_alloc_bytes INTEGER NOT NULL DEFAULT 0,
			loomcycle_heap_inuse_bytes INTEGER NOT NULL DEFAULT 0,
			loomcycle_num_goroutines   INTEGER NOT NULL DEFAULT 0,
			loomcycle_cpu_pct_x100     INTEGER NOT NULL DEFAULT 0,
			system_cpu_pct_x100        INTEGER,
			system_mem_used_mb         INTEGER,
			system_mem_available_mb    INTEGER
		)`,
		// NOTE: process_samples_by_sampled_at is created in `addIndexes`
		// below (defensive: future column additions via ALTER TABLE
		// would otherwise risk the upgrade-path bug that hit
		// channel_messages_by_visible in v0.8.6).

		// v0.8.15 LoomCycle MCP: dynamic_agents — runtime-registered
		// agents from `mcp__loomcycle__register_agent`. Survive restart
		// until TTL expiry (or explicit unregister). definition holds
		// the JSON-encoded config.AgentDef body verbatim (the store
		// doesn't depend on internal/config; same pattern as v0.8.5
		// agent_defs). expires_at = 0 means "no expiry".
		`CREATE TABLE IF NOT EXISTS dynamic_agents (
			name        TEXT PRIMARY KEY,
			definition  BLOB    NOT NULL,
			created_at  INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL DEFAULT 0,
			description TEXT
		)`,
		// v0.8.16 Interruption tool. Agents call Interruption.ask /
		// .notify / .cancel; pending rows block the run until resolved.
		// kind is the closed-enum future-proofing for v0.9.x pause /
		// wait_until / approval — v0.8.16 writes only kind='question'.
		// user_id / agent_id / agent_name denormalised from the run
		// row so listing queries never need a JOIN. Timestamps as
		// INTEGER (unix-nano) per existing sqlite pattern. No FK on
		// run_id (same reasoning as evaluations — immutable audit log
		// must survive any future run pruning).
		`CREATE TABLE IF NOT EXISTS interrupts (
			interrupt_id    TEXT    PRIMARY KEY,
			run_id          TEXT    NOT NULL,
			kind            TEXT    NOT NULL DEFAULT 'question',
			status          TEXT    NOT NULL DEFAULT 'pending',
			question        TEXT,
			options         TEXT,
			context_data    TEXT,
			priority        TEXT    NOT NULL DEFAULT 'normal',
			answer          TEXT,
			answer_meta     TEXT,
			created_at      INTEGER NOT NULL,
			expires_at      INTEGER,
			resolved_at     INTEGER,
			resolved_by     TEXT,
			user_id         TEXT,
			agent_id        TEXT,
			agent_name      TEXT
		)`,
		// v0.8.17 Pause/Resume/Snapshot — full-runtime snapshots
		// captured by the snapshot package. JSON envelope per the
		// pause-resume-snapshot RFC § "Wire surface". JSON stored as
		// TEXT on SQLite (no native JSONB type); Postgres uses JSONB.
		`CREATE TABLE IF NOT EXISTS snapshots (
			id             TEXT PRIMARY KEY,
			created_at     INTEGER NOT NULL,
			label          TEXT,
			schema_version INTEGER NOT NULL,
			byte_size      INTEGER NOT NULL,
			json_content   TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS snapshots_by_created_at_desc ON snapshots(created_at DESC)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	// v0.4 additive columns + indexes for tracking + cancel.
	//
	// Order matters only in that ALTER must precede the partial indexes
	// that reference the new columns.
	addColumns := []string{
		`ALTER TABLE sessions ADD COLUMN user_id TEXT`,
		`ALTER TABLE runs ADD COLUMN agent_id TEXT`,
		`ALTER TABLE runs ADD COLUMN parent_agent_id TEXT`,
		`ALTER TABLE runs ADD COLUMN parent_run_id TEXT`,
		`ALTER TABLE runs ADD COLUMN user_id TEXT`,
		`ALTER TABLE runs ADD COLUMN last_heartbeat_at INTEGER`,
		// v0.8.2: user_tier marker (PR #52). Nullable on legacy rows;
		// new rows carry the name of the user_tier policy applied at
		// run creation. Compliance + cost-retro queries facet on this.
		`ALTER TABLE runs ADD COLUMN user_tier TEXT`,
		// v0.8.5: agent_def_id audit column. NULL = the run resolved
		// against the static cfg.Agents fallback (no DB-versioned def).
		// Non-NULL = the run targeted a specific (name, version) row
		// in agent_defs. Distinguishes static-resolved from DB-resolved
		// runs without a separate flag.
		`ALTER TABLE runs ADD COLUMN agent_def_id TEXT`,
		// v0.8.17: pause_state for the runtime-wide quiesce protocol.
		// Three values are valid at the Store boundary: 'running'
		// (default), 'pausing' (operator issued pause; loop winding
		// down), 'paused' (loop reached iteration boundary; awaiting
		// resume). SetRunPauseState validates the value; the column
		// has no CHECK constraint because SQLite-version-portability
		// is more valuable than a redundant guard.
		`ALTER TABLE runs ADD COLUMN pause_state TEXT NOT NULL DEFAULT 'running'`,
		// v0.8.6 system channels: visible_at + published_by_user_id on
		// channel_messages. Idempotent ALTER for existing v0.8.4 / v0.8.5
		// schemas; on fresh deploys the CREATE TABLE already declared
		// them and the duplicate-column-name guard short-circuits these.
		`ALTER TABLE channel_messages ADD COLUMN visible_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE channel_messages ADD COLUMN published_by_user_id TEXT`,
		// v0.9.x content_sha256 — see internal/store/postgres/migrations/
		// 0018_agent_defs_content_sha256.up.sql for the rationale. NULL
		// until the boot-time backfill walks pre-migration rows.
		`ALTER TABLE agent_defs ADD COLUMN content_sha256 TEXT`,
		`ALTER TABLE skill_defs ADD COLUMN content_sha256 TEXT`,
		// v0.12.7+ provider — actual provider that served the final
		// successful iteration. Idempotent on fresh deploys (the
		// CREATE TABLE above already declares it).
		`ALTER TABLE runs ADD COLUMN provider TEXT`,
		// v0.12.x replica_id on process_samples — lets a shared
		// (cluster-mode, Postgres) table split per replica. SQLite never
		// runs in cluster mode (boot refuses REPLICA_ID + sqlite), so this
		// stays NULL here; added for schema/struct parity. Idempotent on
		// fresh deploys (the CREATE TABLE above already declares it).
		`ALTER TABLE process_samples ADD COLUMN replica_id TEXT`,
		// v0.12.x parent_context — opaque caller-tracking lineage (JSON),
		// set on the root run and copied onto every sub-agent. NULL on
		// legacy rows + runs with no context. Not a secret (safe to
		// persist). Read back via DecodeParentContext.
		`ALTER TABLE runs ADD COLUMN parent_context TEXT`,
	}
	for _, q := range addColumns {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			// SQLite returns errors of the form "duplicate column name: X"
			// when the column already exists. Match on substring rather
			// than introspecting the schema with PRAGMA table_info — the
			// substring check is well-defined for modernc/sqlite and
			// cheaper.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("migrate add column: %w", err)
		}
	}

	addIndexes := []string{
		// Drives the hot lookup paths for the cancel/get endpoints.
		// Partial indexes (WHERE ... IS NOT NULL) keep the index small —
		// the vast majority of historical rows have no agent_id.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_id        ON runs(agent_id)        WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_parent_agent_id ON runs(parent_agent_id) WHERE parent_agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_user_active     ON runs(user_id, status) WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS sessions_by_user        ON sessions(user_id)     WHERE user_id IS NOT NULL`,
		// v0.8.5: facets cost retros + experiment audits by which
		// agent_def_id the run actually ran against. Partial index
		// keeps it small — only DB-resolved runs have a non-NULL value.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_def       ON runs(agent_def_id)    WHERE agent_def_id IS NOT NULL`,
		// v0.9.x content_sha256 — partial-index lookup for the verify
		// op. Lives here in addIndexes (not the CREATE TABLE block)
		// so the column added by addColumns above is guaranteed to
		// exist when this index runs. Required for the v0.8.x →
		// v0.9.x upgrade path where the table exists without the column.
		`CREATE INDEX IF NOT EXISTS agent_defs_by_content_sha256      ON agent_defs(content_sha256)      WHERE content_sha256 IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_content_sha256      ON skill_defs(content_sha256)      WHERE content_sha256 IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_content_sha256 ON mcp_server_defs(content_sha256) WHERE content_sha256 IS NOT NULL`,
		// v0.8.6 channel_messages_by_visible — runs in addIndexes
		// (NOT the earlier `stmts` loop) so the visible_at column
		// added by addColumns above is guaranteed to exist when this
		// index is created. Required for the upgrade path
		// v0.8.4/v0.8.5 → v0.8.6+ (channel_messages table exists
		// from v0.8.4 without visible_at).
		`CREATE INDEX IF NOT EXISTS channel_messages_by_visible ON channel_messages(channel, scope, scope_id, visible_at, id)`,
		// v0.8.x process_samples_by_sampled_at. Drives the read
		// path for /v1/_metrics/samples (window scan) and the
		// sweep DELETE WHERE sampled_at < cutoff.
		`CREATE INDEX IF NOT EXISTS process_samples_by_sampled_at ON process_samples(sampled_at)`,
		// v0.8.15 dynamic_agents_by_expires_at. Drives the sweeper
		// (DELETE WHERE expires_at < now() AND expires_at > 0) and the
		// per-Get expiry filter. Partial: only TTL-bearing rows.
		`CREATE INDEX IF NOT EXISTS dynamic_agents_by_expires_at ON dynamic_agents(expires_at) WHERE expires_at > 0`,
		// v0.8.16 Interruption tool indexes.
		//   * by_run_status drives "is this run blocked?" + listing.
		//   * by_user_status drives the Web UI inbox view (the
		//     denormalised user_id column makes this a single-table
		//     scan, no JOIN against runs).
		//   * by_expires_pending drives the timeout sweeper.
		// Partial on by_user / by_expires so the index stays small
		// when the bulk of rows are resolved/terminal.
		`CREATE INDEX IF NOT EXISTS interrupts_by_run_status  ON interrupts(run_id, status)`,
		`CREATE INDEX IF NOT EXISTS interrupts_by_user_status ON interrupts(user_id, status) WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS interrupts_by_expires     ON interrupts(expires_at) WHERE expires_at IS NOT NULL AND status = 'pending'`,
	}
	for _, q := range addIndexes {
		// Note the asymmetry vs addColumns above: indexes use
		// `CREATE INDEX IF NOT EXISTS` which is unconditionally
		// idempotent, so we don't need to swallow "duplicate"
		// errors. If you ADD a non-IF-NOT-EXISTS statement here for
		// some reason, do NOT copy the column-loop's substring guard —
		// you'd silently suppress real schema errors. Keep the
		// idempotent shape consistent across all index DDL.
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate add index: %w", err)
		}
	}

	// v0.8.6 data fixups (run after column adds + indexes are in place).
	//   - Backfill visible_at on pre-v0.8.6 rows so the new (visible_at, id)
	//     read order matches publish order for legacy data.
	//   - Wipe pre-v0.8.6 cursors. The v0.8.6 cursor format is
	//     `cur_<hex>_<msg_id>` (tuple-shaped). Legacy `msg_<hex>` cursors
	//     are not parsed — subscribers replay from oldest on first
	//     subscribe after upgrade.
	dataFixups := []string{
		`UPDATE channel_messages SET visible_at = published_at WHERE visible_at = 0`,
		`DELETE FROM channel_cursors WHERE cursor NOT LIKE 'cur_%' AND cursor != 'cur_0'`,
	}
	for _, q := range dataFixups {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate data fixup: %w", err)
		}
	}
	return nil
}

// CreateSession inserts a new session with a generated ID and returns it.
// userID may be empty (e.g. legacy callers); the column accepts NULL via the
// pointer-conversion below so empty doesn't shadow as "" on read.
func (s *Store) CreateSession(ctx context.Context, tenantID, agent, userID string) (store.Session, error) {
	id := newID("s_")
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, tenant_id, agent, created_at, user_id) VALUES (?, ?, ?, ?, ?)`,
		id, tenantID, agent, now.UnixNano(), nilIfEmpty(userID),
	)
	if err != nil {
		return store.Session{}, err
	}
	return store.Session{ID: id, TenantID: tenantID, Agent: agent, CreatedAt: now, UserID: userID}, nil
}

// GetSession returns session metadata or *store.ErrNotFound.
func (s *Store) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	var sess store.Session
	var createdNs int64
	var userID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, agent, created_at, user_id FROM sessions WHERE id = ?`, sessionID,
	).Scan(&sess.ID, &sess.TenantID, &sess.Agent, &createdNs, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	if err != nil {
		return store.Session{}, err
	}
	sess.CreatedAt = time.Unix(0, createdNs)
	if userID.Valid {
		sess.UserID = userID.String
	}
	return sess, nil
}

// CreateRun starts a new run inside an existing session. The caller may
// supply identity fields (agent_id, parent linkage, denormalised user_id)
// for v0.4+ tracking; an empty RunIdentity behaves as v0.3 did.
func (s *Store) CreateRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	// Verify the session exists so a missing ID surfaces as ErrNotFound,
	// not a foreign-key error.
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return store.Run{}, err
	}
	id := newID("r_")
	now := time.Now()
	pcJSON, pcOK, pcErr := store.EncodeParentContext(identity.ParentContext)
	if pcErr != nil {
		return store.Run{}, fmt.Errorf("encode parent_context: %w", pcErr)
	}
	var pcVal any
	if pcOK {
		pcVal = pcJSON
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id, session_id, status, started_at, agent_id, parent_agent_id, parent_run_id, user_id, user_tier, agent_def_id, model, parent_context)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, store.RunRunning, now.UnixNano(),
		nilIfEmpty(identity.AgentID),
		nilIfEmpty(identity.ParentAgentID),
		nilIfEmpty(identity.ParentRunID),
		nilIfEmpty(identity.UserID),
		nilIfEmpty(identity.UserTier),
		nilIfEmpty(identity.AgentDefID),
		nilIfEmpty(identity.Model),
		pcVal,
	)
	if err != nil {
		return store.Run{}, err
	}
	return store.Run{
		ID:            id,
		SessionID:     sessionID,
		Status:        store.RunRunning,
		StartedAt:     now,
		AgentID:       identity.AgentID,
		ParentAgentID: identity.ParentAgentID,
		ParentRunID:   identity.ParentRunID,
		UserID:        identity.UserID,
		UserTier:      identity.UserTier,
		AgentDefID:    identity.AgentDefID,
		Model:         identity.Model,
		ParentContext: identity.ParentContext.Clone(),
	}, nil
}

// AppendEvent persists one event. We look up session_id from the run row
// rather than threading it through callers.
func (s *Store) AppendEvent(ctx context.Context, runID string, eventType string, payload []byte) error {
	var sessionID string
	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM runs WHERE id = ?`, runID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.ErrNotFound{Kind: "run", ID: runID}
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO events(session_id, run_id, ts, type, payload) VALUES (?, ?, ?, ?, ?)`,
		sessionID, runID, time.Now().UnixNano(), eventType, payload,
	)
	return err
}

// FinishRun marks a run terminal. Idempotent — if the run is already
// finished, the row's status is unchanged. (We use status='running' as a
// guard so a slow-to-finish goroutine can't overwrite a cancellation.)
func (s *Store) FinishRun(ctx context.Context, runID string, status store.RunStatus, stopReason string, usage store.Usage, errMsg string) error {
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET
			status                = ?,
			completed_at          = ?,
			stop_reason           = ?,
			input_tokens          = ?,
			output_tokens         = ?,
			cache_creation_tokens = ?,
			cache_read_tokens     = ?,
			model                 = ?,
			provider              = ?,
			error                 = ?
		WHERE id = ? AND status = ?`,
		string(status), now, stopReason,
		usage.InputTokens, usage.OutputTokens,
		usage.CacheCreationTokens, usage.CacheReadTokens,
		usage.Model, nilIfEmpty(usage.Provider), errMsg,
		runID, string(store.RunRunning),
	)
	return err
}

// GetTranscript returns all events for a session, ordered by seq ascending.
func (s *Store) GetTranscript(ctx context.Context, sessionID string) ([]store.Event, error) {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, run_id, ts, type, payload FROM events WHERE session_id = ? ORDER BY seq ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Event
	for rows.Next() {
		var ev store.Event
		var ts int64
		if err := rows.Scan(&ev.Seq, &ev.RunID, &ts, &ev.Type, &ev.Payload); err != nil {
			return nil, err
		}
		ev.SessionID = sessionID
		ev.Timestamp = time.Unix(0, ts)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetLastEventForRun returns the latest event by seq for the given
// run. Indexed by events.run_id under the existing schema; the
// composite (session_id, seq) index doesn't cover this query, but
// SQLite's automatic indexing on the run_id column is sufficient
// for the typical N<20 running-agents case.
func (s *Store) GetLastEventForRun(ctx context.Context, runID string) (store.Event, error) {
	var (
		ev store.Event
		ts int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT seq, session_id, run_id, ts, type, payload
		 FROM events WHERE run_id = ? ORDER BY seq DESC LIMIT 1`,
		runID,
	).Scan(&ev.Seq, &ev.SessionID, &ev.RunID, &ts, &ev.Type, &ev.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Event{}, &store.ErrNotFound{Kind: "event", ID: runID}
	}
	if err != nil {
		return store.Event{}, err
	}
	ev.Timestamp = time.Unix(0, ts)
	return ev, nil
}

// ListEvents serves the v0.8.21 audit view's cross-session query.
// Filter clauses are conditionally appended so unset dimensions don't
// constrain the index lookup. Total is computed via a sibling COUNT(*)
// over the same WHERE clause so pagination UIs can show "page N of M"
// without a separate API call.
func (s *Store) ListEvents(ctx context.Context, filter store.EventFilter, limit, offset int) ([]store.Event, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500 // cap to keep memory bounded
	}
	if offset < 0 {
		offset = 0
	}

	var (
		conds []string
		args  []any
	)
	if filter.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, filter.Type)
	}
	if !filter.From.IsZero() {
		conds = append(conds, "ts >= ?")
		args = append(args, filter.From.UnixNano())
	}
	if !filter.To.IsZero() {
		conds = append(conds, "ts <= ?")
		args = append(args, filter.To.UnixNano())
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// COUNT(*) first — same WHERE, but separate to avoid window-fn
	// complexity on SQLite. Cheap because the WHERE narrows via the
	// new indexes.
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx,
		"SELECT seq, session_id, run_id, ts, type, payload FROM events "+where+
			" ORDER BY ts DESC, seq DESC LIMIT ? OFFSET ?",
		args...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]store.Event, 0, limit)
	for rows.Next() {
		var ev store.Event
		var ts int64
		if err := rows.Scan(&ev.Seq, &ev.SessionID, &ev.RunID, &ts, &ev.Type, &ev.Payload); err != nil {
			return nil, 0, err
		}
		ev.Timestamp = time.Unix(0, ts)
		out = append(out, ev)
	}
	return out, total, rows.Err()
}

// scanRun decodes one row from a runs SELECT into a store.Run. The
// SELECT column list MUST match the order in runColumns below.
//
// The trailing `agent` column comes from a LEFT JOIN onto sessions
// — sessions.agent is the YAML-declared agent name. NULL when the
// session row is missing (the JOIN drops the agent name silently
// rather than failing the read; the rest of the run row is still
// useful).
func scanRun(scanner interface{ Scan(...any) error }) (store.Run, error) {
	var r store.Run
	var startedNs, completedNs sql.NullInt64
	var lastHbNs sql.NullInt64
	var stopReason, model, provider, errMsg sql.NullString
	var agentID, parentAgentID, parentRunID, userID, userTier sql.NullString
	var agentDefID sql.NullString
	var pauseState sql.NullString
	var parentContext sql.NullString
	var sessAgent sql.NullString
	var status string
	if err := scanner.Scan(
		&r.ID, &r.SessionID, &status, &startedNs, &completedNs,
		&stopReason,
		&r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens,
		&model, &provider, &errMsg,
		&agentID, &parentAgentID, &parentRunID, &userID, &lastHbNs,
		&userTier,
		&agentDefID, &pauseState, &parentContext,
		&sessAgent,
	); err != nil {
		return store.Run{}, err
	}
	r.Status = store.RunStatus(status)
	if startedNs.Valid {
		r.StartedAt = time.Unix(0, startedNs.Int64)
	}
	if completedNs.Valid {
		r.CompletedAt = time.Unix(0, completedNs.Int64)
	}
	if lastHbNs.Valid {
		r.LastHeartbeatAt = time.Unix(0, lastHbNs.Int64)
	}
	if stopReason.Valid {
		r.StopReason = stopReason.String
	}
	if model.Valid {
		r.Model = model.String
	}
	if provider.Valid {
		r.Provider = provider.String
	}
	if errMsg.Valid {
		r.ErrorMsg = errMsg.String
	}
	if agentID.Valid {
		r.AgentID = agentID.String
	}
	if parentAgentID.Valid {
		r.ParentAgentID = parentAgentID.String
	}
	if parentRunID.Valid {
		r.ParentRunID = parentRunID.String
	}
	if userID.Valid {
		r.UserID = userID.String
	}
	if userTier.Valid {
		r.UserTier = userTier.String
	}
	if agentDefID.Valid {
		r.AgentDefID = agentDefID.String
	}
	if pauseState.Valid {
		r.PauseState = pauseState.String
	}
	if parentContext.Valid {
		pc, err := store.DecodeParentContext(parentContext.String)
		if err != nil {
			return store.Run{}, fmt.Errorf("decode parent_context: %w", err)
		}
		r.ParentContext = pc
	}
	if sessAgent.Valid {
		r.Agent = sessAgent.String
	}
	return r, nil
}

// runColumns is the canonical SELECT column list paired with scanRun.
// Centralised so a future column addition is a one-line change.
//
// The `r.` / `s.` qualifiers + the trailing JOIN clause are required
// because of the sessions.agent column (denormalised onto Run.Agent
// at read time so callers don't have to fetch the session row
// separately). All callers MUST use `runFromTable` to reference the
// table (with its alias) so the qualifiers resolve.
const runColumns = `r.id, r.session_id, r.status, r.started_at, r.completed_at,
		r.stop_reason,
		r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		r.model, r.provider, r.error,
		r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at,
		r.user_tier,
		r.agent_def_id, r.pause_state, r.parent_context,
		s.agent`

// runFromTable is the canonical FROM clause paired with runColumns.
// Provides the `r` and `s` aliases that the column list references.
const runFromTable = `runs r LEFT JOIN sessions s ON r.session_id = s.id`

// GetRunByAgentID returns the most recently started run carrying the
// given agent_id, or *store.ErrNotFound. Multiple historical runs may
// share an agent_id (a caller reused it after the first terminated);
// we surface the latest, which is the one any cancel/status caller
// would mean.
func (s *Store) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	if agentID == "" {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.agent_id = ? ORDER BY r.started_at DESC LIMIT 1`,
		agentID,
	)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	return r, err
}

// GetRun returns one row by run_id (the primary key on runs).
func (s *Store) GetRun(ctx context.Context, runID string) (store.Run, error) {
	if runID == "" {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.id = ?`,
		runID,
	)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	if err != nil {
		// Match postgres adapter's wrapping shape — without this, raw
		// database/sql errors leak through to the tool layer with no
		// "get run" context, and the two adapters diverge in their
		// error message format.
		return store.Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// ListUsers returns one row per distinct user_id with summary stats.
// Drives the v0.7.3 Web UI user picker.
//
// SQLite COUNT(CASE WHEN ...) is the conventional shape for grouped
// counts by category; both backends produce identical row sets.
func (s *Store) ListUsers(ctx context.Context) ([]store.UserSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			user_id,
			COUNT(CASE WHEN status = 'running' THEN 1 END) AS running_count,
			COUNT(*) AS total_count,
			MAX(started_at) AS last_started_at
		FROM runs
		WHERE user_id IS NOT NULL AND user_id != ''
		GROUP BY user_id
		ORDER BY last_started_at DESC
		LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.UserSummary
	for rows.Next() {
		var u store.UserSummary
		var lastNanos int64
		if err := rows.Scan(&u.UserID, &u.RunningCount, &u.TotalCount, &lastNanos); err != nil {
			return nil, err
		}
		u.LastStartedAt = time.Unix(0, lastNanos).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListActiveRunsByUser returns runs for userID whose status matches the
// supplied filter. An empty status returns ALL statuses. Capped at 100
// rows ordered by started_at DESC.
func (s *Store) ListActiveRunsByUser(ctx context.Context, userID string, status store.RunStatus) ([]store.Run, error) {
	if userID == "" {
		return nil, nil
	}
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.user_id = ? ORDER BY r.started_at DESC LIMIT 100`,
			userID,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.user_id = ? AND r.status = ? ORDER BY r.started_at DESC LIMIT 100`,
			userID, string(status),
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsByParentAgentID returns the runs whose parent_agent_id
// matches. Drives cascade-cancel discovery (every direct child of a
// parent agent_id). Recursion (grandchildren) is the caller's job —
// keeps this query simple and lets the cancel handler walk the tree
// however it wants.
func (s *Store) ListRunsByParentAgentID(ctx context.Context, parentAgentID string) ([]store.Run, error) {
	if parentAgentID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.parent_agent_id = ? ORDER BY r.started_at ASC`,
		parentAgentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateHeartbeat sets last_heartbeat_at to the current time. No-op for
// runs that aren't currently running (the WHERE guard prevents a slow
// hb update from un-finishing a terminal run that just got cancelled).
func (s *Store) UpdateHeartbeat(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET last_heartbeat_at = ? WHERE id = ? AND status = ?`,
		time.Now().UnixNano(), runID, string(store.RunRunning),
	)
	return err
}

// SweepStaleRuns implements store.Store. Runs whose last_heartbeat_at
// is older than cutoff (or whose started_at is older than cutoff and
// who never heartbeated) are flipped to status="failed" with
// error="heartbeat timeout". Single atomic UPDATE so concurrent
// sweepers race correctly.
func (s *Store) SweepStaleRuns(ctx context.Context, cutoff time.Time) (int, error) {
	cutoffNs := cutoff.UnixNano()
	completedNs := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET
			status = ?,
			completed_at = ?,
			error = ?,
			stop_reason = ?
		 WHERE status = ?
		   AND (
			 (last_heartbeat_at IS NOT NULL AND last_heartbeat_at < ?)
			 OR (last_heartbeat_at IS NULL AND started_at < ?)
		   )`,
		string(store.RunFailed), completedNs,
		"heartbeat timeout", "heartbeat_timeout",
		string(store.RunRunning),
		cutoffNs, cutoffNs,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Some drivers don't report RowsAffected; this isn't fatal —
		// the UPDATE landed, we just don't know the count. Return 0.
		return 0, nil
	}
	return int(n), nil
}

// SetRunPauseState implements store.Store. Validates the state at the
// boundary (refuses anything outside the PauseState* constants) and
// writes runs.pause_state. Returns *ErrNotFound when no row matches.
func (s *Store) SetRunPauseState(ctx context.Context, runID, state string) error {
	switch state {
	case store.PauseStateRunning, store.PauseStatePausing, store.PauseStatePaused:
	default:
		return fmt.Errorf("set run pause_state: unknown state %q", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET pause_state = ? WHERE id = ?`, state, runID)
	if err != nil {
		return fmt.Errorf("set run pause_state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Driver doesn't report RowsAffected — assume the UPDATE landed
		// (the alternative is double-write on retry which is worse).
		return nil
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "run", ID: runID}
	}
	return nil
}

// ListPausedRuns implements store.Store. Returns runs at pause_state =
// 'paused' (NOT 'pausing' — the column distinguishes in-flight pause
// transitions from at-rest paused state). Ordered by started_at ASC
// so resume processes oldest pauses first.
func (s *Store) ListPausedRuns(ctx context.Context) ([]store.Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+`
		 WHERE r.pause_state = ?
		 ORDER BY r.started_at ASC`,
		store.PauseStatePaused,
	)
	if err != nil {
		return nil, fmt.Errorf("list paused runs: %w", err)
	}
	defer rows.Close()
	var out []store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v0.8.17 Pause/Resume/Snapshot — Snapshot storage (PR 2) ----

// SnapshotCreate inserts one row. Returns *store.ErrConflict when the
// id collides with an existing row (caller's id allocation is the
// source of truth; UUID-shaped IDs make this rare in practice).
func (s *Store) SnapshotCreate(ctx context.Context, row store.SnapshotRow) error {
	if row.ID == "" {
		return fmt.Errorf("snapshot create: id required")
	}
	if len(row.JSONContent) == 0 {
		return fmt.Errorf("snapshot create: json_content required")
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var label any
	if row.Label != "" {
		label = row.Label
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshots(id, created_at, label, schema_version, byte_size, json_content)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		row.ID, createdAt.UnixNano(), label, row.SchemaVersion, row.ByteSize, string(row.JSONContent),
	)
	if err != nil {
		// SQLite's UNIQUE-constraint-violation surfaces as a string;
		// match defensively rather than depending on driver-specific
		// error types. The PK violation is the only conflict path
		// snapshots can take (no other UNIQUE constraints exist).
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return &store.ErrConflict{Kind: "snapshot", ID: row.ID}
		}
		return fmt.Errorf("snapshot create: %w", err)
	}
	return nil
}

// SnapshotGet returns the full snapshot row including the JSON
// payload. Returns *store.ErrNotFound when no row matches.
func (s *Store) SnapshotGet(ctx context.Context, id string) (store.SnapshotRow, error) {
	if id == "" {
		return store.SnapshotRow{}, &store.ErrNotFound{Kind: "snapshot", ID: id}
	}
	var (
		row         store.SnapshotRow
		createdNs   int64
		label       sql.NullString
		jsonContent string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, label, schema_version, byte_size, json_content
		 FROM snapshots WHERE id = ?`, id,
	).Scan(&row.ID, &createdNs, &label, &row.SchemaVersion, &row.ByteSize, &jsonContent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.SnapshotRow{}, &store.ErrNotFound{Kind: "snapshot", ID: id}
		}
		return store.SnapshotRow{}, fmt.Errorf("snapshot get: %w", err)
	}
	row.CreatedAt = time.Unix(0, createdNs)
	if label.Valid {
		row.Label = label.String
	}
	row.JSONContent = []byte(jsonContent)
	return row, nil
}

// SnapshotList returns metadata projections (no JSON payload),
// optionally filtered by case-insensitive label substring and capped
// at limit (0 = no cap; recommend bounding at the handler layer).
func (s *Store) SnapshotList(ctx context.Context, labelContains string, limit int) ([]store.SnapshotListEntry, error) {
	var (
		rows *sql.Rows
		err  error
	)
	query := `SELECT id, created_at, label, schema_version, byte_size
	          FROM snapshots `
	args := []any{}
	if labelContains != "" {
		// LOWER + LIKE for case-insensitive substring match. The
		// %...% wildcards on both sides — operators search for
		// label fragments without needing to know exact case.
		query += `WHERE LOWER(COALESCE(label, '')) LIKE LOWER(?) `
		args = append(args, "%"+labelContains+"%")
	}
	query += `ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err = s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("snapshot list: %w", err)
	}
	defer rows.Close()
	var out []store.SnapshotListEntry
	for rows.Next() {
		var (
			e         store.SnapshotListEntry
			createdNs int64
			label     sql.NullString
		)
		if err := rows.Scan(&e.ID, &createdNs, &label, &e.SchemaVersion, &e.ByteSize); err != nil {
			return nil, fmt.Errorf("snapshot list scan: %w", err)
		}
		e.CreatedAt = time.Unix(0, createdNs)
		if label.Valid {
			e.Label = label.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotDelete removes one row by id. Returns true when a row was
// removed; false when nothing matched (idempotent — the operator
// scripting `loomcycle snapshot delete` repeatedly never errors).
func (s *Store) SnapshotDelete(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("snapshot delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Driver doesn't report; assume delete landed (the operator
		// retry will idempotently see 0 rows on the next call).
		return true, nil
	}
	return n > 0, nil
}

// v0.12.5 Phase 6 hook-registry stubs. SQLite never runs in cluster
// mode (the openStore guard refuses LOOMCYCLE_REPLICA_ID + sqlite at
// boot), so these methods are unreachable in production. They satisfy
// the Store interface so the SQLite path compiles. If called via a
// test that bypasses the boot guard, the error message points at the
// design intent.

var errHooksSQLiteUnsupported = errors.New("hooks: SQLite backend is single-replica only; hook DB methods require Postgres (set LOOMCYCLE_REPLICA_ID to enter cluster mode)")

func (s *Store) CreateHook(ctx context.Context, h store.HookRow) error {
	return errHooksSQLiteUnsupported
}

func (s *Store) DeleteHook(ctx context.Context, hookID string) error {
	return errHooksSQLiteUnsupported
}

func (s *Store) ListHooks(ctx context.Context) ([]store.HookRow, error) {
	// Return empty slice (not error) so a single-replica boot that
	// somehow probes the table doesn't fatal — matches the
	// "hooks live in memory, not DB" v0.11.x semantics.
	return nil, nil
}

func (s *Store) GetHookByID(ctx context.Context, hookID string) (store.HookRow, error) {
	return store.HookRow{}, errHooksSQLiteUnsupported
}

// ---- v0.8.17 Snapshot capture — bulk readers (PR 2.3a) ----

// SnapshotReadAgentDefs implements store.Store.
func (s *Store) SnapshotReadAgentDefs(ctx context.Context) ([]store.AgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_id, name, version, parent_def_id, definition, description,
		        created_at, created_by_agent_id, created_by_run_id,
		        retired, bootstrapped_from_static
		 FROM agent_defs
		 ORDER BY name ASC, version ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot read agent_defs: %w", err)
	}
	defer rows.Close()
	var out []store.AgentDefRow
	for rows.Next() {
		var (
			r           store.AgentDefRow
			createdNs   int64
			parentDefID sql.NullString
			description sql.NullString
			createdBy   sql.NullString
			createdRun  sql.NullString
			definition  string
			retiredInt  int
			bootstrap   int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version, &parentDefID,
			&definition, &description,
			&createdNs, &createdBy, &createdRun,
			&retiredInt, &bootstrap,
		); err != nil {
			return nil, fmt.Errorf("scan agent_def: %w", err)
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdNs)
		if parentDefID.Valid {
			r.ParentDefID = parentDefID.String
		}
		if description.Valid {
			r.Description = description.String
		}
		if createdBy.Valid {
			r.CreatedByAgentID = createdBy.String
		}
		if createdRun.Valid {
			r.CreatedByRunID = createdRun.String
		}
		r.Retired = retiredInt != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SnapshotReadAgentDefActive implements store.Store.
func (s *Store) SnapshotReadAgentDefActive(ctx context.Context) ([]store.AgentDefActiveEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, def_id, promoted_at, promoted_by_agent_id
		 FROM agent_def_active
		 ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read agent_def_active: %w", err)
	}
	defer rows.Close()
	var out []store.AgentDefActiveEntry
	for rows.Next() {
		var (
			e          store.AgentDefActiveEntry
			promotedNs int64
			promoter   sql.NullString
		)
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter); err != nil {
			return nil, fmt.Errorf("scan agent_def_active: %w", err)
		}
		e.PromotedAt = time.Unix(0, promotedNs)
		if promoter.Valid {
			e.PromotedByAgentID = promoter.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotReadSkillDefs implements store.Store. Mirror of
// SnapshotReadAgentDefs against the skill_defs table.
func (s *Store) SnapshotReadSkillDefs(ctx context.Context) ([]store.SkillDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_id, name, version, parent_def_id, definition, description,
		        created_at, created_by_agent_id, created_by_run_id,
		        retired, bootstrapped_from_static
		 FROM skill_defs
		 ORDER BY name ASC, version ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot read skill_defs: %w", err)
	}
	defer rows.Close()
	var out []store.SkillDefRow
	for rows.Next() {
		var (
			r           store.SkillDefRow
			createdNs   int64
			parentDefID sql.NullString
			description sql.NullString
			createdBy   sql.NullString
			createdRun  sql.NullString
			definition  string
			retiredInt  int
			bootstrap   int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version, &parentDefID,
			&definition, &description,
			&createdNs, &createdBy, &createdRun,
			&retiredInt, &bootstrap,
		); err != nil {
			return nil, fmt.Errorf("scan skill_def: %w", err)
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdNs)
		if parentDefID.Valid {
			r.ParentDefID = parentDefID.String
		}
		if description.Valid {
			r.Description = description.String
		}
		if createdBy.Valid {
			r.CreatedByAgentID = createdBy.String
		}
		if createdRun.Valid {
			r.CreatedByRunID = createdRun.String
		}
		r.Retired = retiredInt != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SnapshotReadSkillDefActive implements store.Store.
func (s *Store) SnapshotReadSkillDefActive(ctx context.Context) ([]store.SkillDefActiveEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, def_id, promoted_at, promoted_by_agent_id
		 FROM skill_def_active
		 ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read skill_def_active: %w", err)
	}
	defer rows.Close()
	var out []store.SkillDefActiveEntry
	for rows.Next() {
		var (
			e          store.SkillDefActiveEntry
			promotedNs int64
			promoter   sql.NullString
		)
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter); err != nil {
			return nil, fmt.Errorf("scan skill_def_active: %w", err)
		}
		e.PromotedAt = time.Unix(0, promotedNs)
		if promoter.Valid {
			e.PromotedByAgentID = promoter.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotReadMCPServerDefs — v0.9.x mirror.
func (s *Store) SnapshotReadMCPServerDefs(ctx context.Context) ([]store.MCPServerDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_id, name, version, parent_def_id, definition, description,
		        created_at, created_by_agent_id, created_by_run_id,
		        retired, bootstrapped_from_static, content_sha256
		 FROM mcp_server_defs
		 ORDER BY name ASC, version ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot read mcp_server_defs: %w", err)
	}
	defer rows.Close()
	var out []store.MCPServerDefRow
	for rows.Next() {
		var (
			r           store.MCPServerDefRow
			createdNs   int64
			parentDefID sql.NullString
			description sql.NullString
			createdBy   sql.NullString
			createdRun  sql.NullString
			definition  string
			retiredInt  int
			bootstrap   int
			contentHash sql.NullString
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version, &parentDefID,
			&definition, &description,
			&createdNs, &createdBy, &createdRun,
			&retiredInt, &bootstrap, &contentHash,
		); err != nil {
			return nil, fmt.Errorf("scan mcp_server_def: %w", err)
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdNs)
		if parentDefID.Valid {
			r.ParentDefID = parentDefID.String
		}
		if description.Valid {
			r.Description = description.String
		}
		if createdBy.Valid {
			r.CreatedByAgentID = createdBy.String
		}
		if createdRun.Valid {
			r.CreatedByRunID = createdRun.String
		}
		r.Retired = retiredInt != 0
		r.BootstrappedFromStatic = bootstrap != 0
		if contentHash.Valid {
			r.ContentSHA256 = contentHash.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SnapshotReadMCPServerDefActive — v0.9.x mirror.
func (s *Store) SnapshotReadMCPServerDefActive(ctx context.Context) ([]store.MCPServerDefActiveEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, def_id, promoted_at, promoted_by_agent_id
		 FROM mcp_server_def_active
		 ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read mcp_server_def_active: %w", err)
	}
	defer rows.Close()
	var out []store.MCPServerDefActiveEntry
	for rows.Next() {
		var (
			e          store.MCPServerDefActiveEntry
			promotedNs int64
			promoter   sql.NullString
		)
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter); err != nil {
			return nil, fmt.Errorf("scan mcp_server_def_active: %w", err)
		}
		e.PromotedAt = time.Unix(0, promotedNs)
		if promoter.Valid {
			e.PromotedByAgentID = promoter.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotReadMemory implements store.Store. Filters expired rows
// (consistent with MemoryGet's behaviour at the same layer).
func (s *Store) SnapshotReadMemory(ctx context.Context) ([]store.MemorySnapshotEntry, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, scope_id, key, value, expires_at, created_at, updated_at
		 FROM memory
		 WHERE expires_at IS NULL OR expires_at > ?
		 ORDER BY scope ASC, scope_id ASC, key ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("snapshot read memory: %w", err)
	}
	defer rows.Close()
	var out []store.MemorySnapshotEntry
	for rows.Next() {
		var (
			e         store.MemorySnapshotEntry
			scopeStr  string
			value     string
			expiresNs sql.NullInt64
			createdNs int64
			updatedNs int64
		)
		if err := rows.Scan(
			&scopeStr, &e.ScopeID, &e.Key, &value,
			&expiresNs, &createdNs, &updatedNs,
		); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		e.Scope = store.MemoryScope(scopeStr)
		e.Value = json.RawMessage(value)
		if expiresNs.Valid {
			e.ExpiresAt = time.Unix(0, expiresNs.Int64)
		}
		e.CreatedAt = time.Unix(0, createdNs)
		e.UpdatedAt = time.Unix(0, updatedNs)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotReadChannelMessages implements store.Store. Filters expired
// rows. Ordered by natural delivery sequence so restore replays
// messages in their original order.
func (s *Store) SnapshotReadChannelMessages(ctx context.Context) ([]store.ChannelMessage, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel, scope, scope_id, payload, published_at, expires_at, visible_at, published_by_user_id
		 FROM channel_messages
		 WHERE expires_at IS NULL OR expires_at > ?
		 ORDER BY channel ASC, scope ASC, scope_id ASC, visible_at ASC, id ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("snapshot read channel_messages: %w", err)
	}
	defer rows.Close()
	var out []store.ChannelMessage
	for rows.Next() {
		var (
			m           store.ChannelMessage
			scopeStr    string
			payload     string
			publishedNs int64
			expiresNs   sql.NullInt64
			visibleNs   sql.NullInt64
			publishedBy sql.NullString
		)
		if err := rows.Scan(
			&m.ID, &m.Channel, &scopeStr, &m.ScopeID, &payload,
			&publishedNs, &expiresNs, &visibleNs, &publishedBy,
		); err != nil {
			return nil, fmt.Errorf("scan channel_message: %w", err)
		}
		m.Scope = store.MemoryScope(scopeStr)
		m.Payload = json.RawMessage(payload)
		m.PublishedAt = time.Unix(0, publishedNs)
		if expiresNs.Valid {
			m.ExpiresAt = time.Unix(0, expiresNs.Int64)
		}
		if visibleNs.Valid {
			m.VisibleAt = time.Unix(0, visibleNs.Int64)
		}
		if publishedBy.Valid {
			m.PublishedByUserID = publishedBy.String
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SnapshotReadChannelCursors implements store.Store.
func (s *Store) SnapshotReadChannelCursors(ctx context.Context) ([]store.ChannelCursorEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, scope, scope_id, cursor, updated_at
		 FROM channel_cursors
		 ORDER BY channel ASC, scope ASC, scope_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read channel_cursors: %w", err)
	}
	defer rows.Close()
	var out []store.ChannelCursorEntry
	for rows.Next() {
		var (
			c         store.ChannelCursorEntry
			scopeStr  string
			updatedNs int64
		)
		if err := rows.Scan(&c.Channel, &scopeStr, &c.ScopeID, &c.Cursor, &updatedNs); err != nil {
			return nil, fmt.Errorf("scan channel_cursor: %w", err)
		}
		c.Scope = store.MemoryScope(scopeStr)
		c.UpdatedAt = time.Unix(0, updatedNs)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SnapshotReadEvaluations implements store.Store. Ordered by
// created_at ASC so the envelope's evaluations section preserves
// submission order; post-restore aggregates see the same time series.
func (s *Store) SnapshotReadEvaluations(ctx context.Context) ([]store.EvaluationRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT eval_id, run_id, def_id, score, dimensions, judgement, rationale,
		        emitter_role, emitter_agent_id, emitter_run_id, created_at
		 FROM evaluations
		 ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read evaluations: %w", err)
	}
	defer rows.Close()
	var out []store.EvaluationRow
	for rows.Next() {
		var (
			r              store.EvaluationRow
			defID          sql.NullString
			dimensions     sql.NullString
			judgement      sql.NullString
			rationale      sql.NullString
			emitterAgentID sql.NullString
			emitterRunID   sql.NullString
			createdNs      int64
		)
		if err := rows.Scan(
			&r.EvalID, &r.RunID, &defID, &r.Score,
			&dimensions, &judgement, &rationale,
			&r.EmitterRole, &emitterAgentID, &emitterRunID,
			&createdNs,
		); err != nil {
			return nil, fmt.Errorf("scan evaluation: %w", err)
		}
		if defID.Valid {
			r.DefID = defID.String
		}
		if dimensions.Valid && dimensions.String != "" {
			var dim map[string]float64
			if err := json.Unmarshal([]byte(dimensions.String), &dim); err == nil {
				r.Dimensions = dim
			}
		}
		if judgement.Valid && judgement.String != "" {
			r.Judgement = json.RawMessage(judgement.String)
		}
		if rationale.Valid {
			r.Rationale = rationale.String
		}
		if emitterAgentID.Valid {
			r.EmitterAgentID = emitterAgentID.String
		}
		if emitterRunID.Valid {
			r.EmitterRunID = emitterRunID.String
		}
		r.CreatedAt = time.Unix(0, createdNs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v0.8.17 Snapshot restore — idempotent raw inserts (PR 3.2a) ----

// SnapshotRestoreSession implements store.Store. Preserves caller-
// supplied ID + CreatedAt. INSERT OR IGNORE → idempotent.
func (s *Store) SnapshotRestoreSession(ctx context.Context, sess store.Session) (bool, error) {
	if sess.ID == "" {
		return false, fmt.Errorf("snapshot restore session: id required")
	}
	createdNs := sess.CreatedAt.UnixNano()
	if sess.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO sessions(id, tenant_id, agent, created_at, user_id) VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.TenantID, sess.Agent, createdNs, nilIfEmpty(sess.UserID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore session: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreRun implements store.Store. Preserves every field
// including PauseState. INSERT OR IGNORE → idempotent.
func (s *Store) SnapshotRestoreRun(ctx context.Context, r store.Run) (bool, error) {
	if r.ID == "" || r.SessionID == "" {
		return false, fmt.Errorf("snapshot restore run: id and session_id required")
	}
	startedNs := r.StartedAt.UnixNano()
	if r.StartedAt.IsZero() {
		startedNs = time.Now().UnixNano()
	}
	var completedNs any
	if !r.CompletedAt.IsZero() {
		completedNs = r.CompletedAt.UnixNano()
	}
	var lastHbNs any
	if !r.LastHeartbeatAt.IsZero() {
		lastHbNs = r.LastHeartbeatAt.UnixNano()
	}
	status := string(r.Status)
	if status == "" {
		status = string(store.RunRunning)
	}
	pauseState := r.PauseState
	if pauseState == "" {
		pauseState = store.PauseStateRunning
	}
	pcJSON, pcOK, pcErr := store.EncodeParentContext(r.ParentContext)
	if pcErr != nil {
		return false, fmt.Errorf("snapshot restore run: encode parent_context: %w", pcErr)
	}
	var pcVal any
	if pcOK {
		pcVal = pcJSON
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO runs(
			id, session_id, status, started_at, completed_at, stop_reason,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			model, provider, error,
			agent_id, parent_agent_id, parent_run_id, user_id, last_heartbeat_at,
			user_tier, agent_def_id, pause_state, parent_context
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SessionID, status, startedNs, completedNs, nilIfEmpty(r.StopReason),
		r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
		nilIfEmpty(r.Model), nilIfEmpty(r.Provider), nilIfEmpty(r.ErrorMsg),
		nilIfEmpty(r.AgentID), nilIfEmpty(r.ParentAgentID), nilIfEmpty(r.ParentRunID),
		nilIfEmpty(r.UserID), lastHbNs,
		nilIfEmpty(r.UserTier), nilIfEmpty(r.AgentDefID), pauseState, pcVal,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore run: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreEvent implements store.Store. Writes one event with
// caller-supplied seq + run_id. INSERT OR IGNORE on (run_id, seq) →
// idempotent. seq is normally AUTOINCREMENT but restore needs the
// explicit value to preserve transcript order across the boundary.
func (s *Store) SnapshotRestoreEvent(ctx context.Context, e store.Event) (bool, error) {
	if e.RunID == "" || e.SessionID == "" {
		return false, fmt.Errorf("snapshot restore event: run_id and session_id required")
	}
	tsNs := e.Timestamp.UnixNano()
	if e.Timestamp.IsZero() {
		tsNs = time.Now().UnixNano()
	}
	// Build the INSERT to include seq when non-zero. SQLite's
	// INSERT OR IGNORE on the (seq) PK is the idempotency anchor.
	if e.Seq != 0 {
		res, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO events(seq, session_id, run_id, ts, type, payload) VALUES (?, ?, ?, ?, ?, ?)`,
			e.Seq, e.SessionID, e.RunID, tsNs, e.Type, e.Payload,
		)
		if err != nil {
			return false, fmt.Errorf("snapshot restore event: %w", err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	// Caller didn't supply a seq — let the AUTOINCREMENT mint one.
	// Used when the snapshot envelope's event had seq=0 (rare;
	// usually all events carry a seq from capture). Always counts as
	// inserted because no PK collision is possible.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events(session_id, run_id, ts, type, payload) VALUES (?, ?, ?, ?, ?)`,
		e.SessionID, e.RunID, tsNs, e.Type, e.Payload,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore event (auto-seq): %w", err)
	}
	return true, nil
}

// SnapshotRestoreAgentDef implements store.Store. Preserves DefID +
// Version + parent linkage. INSERT OR IGNORE → idempotent.
func (s *Store) SnapshotRestoreAgentDef(ctx context.Context, r store.AgentDefRow) (bool, error) {
	if r.DefID == "" || r.Name == "" {
		return false, fmt.Errorf("snapshot restore agent_def: def_id and name required")
	}
	createdNs := r.CreatedAt.UnixNano()
	if r.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO agent_defs(
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore agent_def: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreAgentDefActive implements store.Store. INSERT OR
// IGNORE on the name PK — first restore writes the snapshot's
// promoted_at + def_id; subsequent restores leave the existing row
// alone so the (bool, error) return reads as "not inserted" and the
// caller's counter stays honest.
func (s *Store) SnapshotRestoreAgentDefActive(ctx context.Context, e store.AgentDefActiveEntry) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore agent_def_active: name and def_id required")
	}
	promotedNs := e.PromotedAt.UnixNano()
	if e.PromotedAt.IsZero() {
		promotedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO agent_def_active(name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?)`,
		e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore agent_def_active: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreSkillDef implements store.Store. Mirror of
// SnapshotRestoreAgentDef against skill_defs.
func (s *Store) SnapshotRestoreSkillDef(ctx context.Context, r store.SkillDefRow) (bool, error) {
	if r.DefID == "" || r.Name == "" {
		return false, fmt.Errorf("snapshot restore skill_def: def_id and name required")
	}
	createdNs := r.CreatedAt.UnixNano()
	if r.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO skill_defs(
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore skill_def: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreSkillDefActive implements store.Store. Mirror of
// SnapshotRestoreAgentDefActive against skill_def_active.
func (s *Store) SnapshotRestoreSkillDefActive(ctx context.Context, e store.SkillDefActiveEntry) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore skill_def_active: name and def_id required")
	}
	promotedNs := e.PromotedAt.UnixNano()
	if e.PromotedAt.IsZero() {
		promotedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO skill_def_active(name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?)`,
		e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore skill_def_active: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreMCPServerDef — v0.9.x mirror.
func (s *Store) SnapshotRestoreMCPServerDef(ctx context.Context, r store.MCPServerDefRow) (bool, error) {
	if r.DefID == "" || r.Name == "" {
		return false, fmt.Errorf("snapshot restore mcp_server_def: def_id and name required")
	}
	createdNs := r.CreatedAt.UnixNano()
	if r.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO mcp_server_defs(
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore mcp_server_def: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreMCPServerDefActive — v0.9.x mirror.
func (s *Store) SnapshotRestoreMCPServerDefActive(ctx context.Context, e store.MCPServerDefActiveEntry) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore mcp_server_def_active: name and def_id required")
	}
	promotedNs := e.PromotedAt.UnixNano()
	if e.PromotedAt.IsZero() {
		promotedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO mcp_server_def_active(name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?)`,
		e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore mcp_server_def_active: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreMemory implements store.Store. Preserves CreatedAt /
// UpdatedAt / ExpiresAt / Value. INSERT OR IGNORE on (scope, scope_id,
// key) PK → idempotent.
func (s *Store) SnapshotRestoreMemory(ctx context.Context, e store.MemorySnapshotEntry) (bool, error) {
	if e.Scope == "" || e.Key == "" {
		return false, fmt.Errorf("snapshot restore memory: scope and key required")
	}
	createdNs := e.CreatedAt.UnixNano()
	if e.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	updatedNs := e.UpdatedAt.UnixNano()
	if e.UpdatedAt.IsZero() {
		updatedNs = createdNs
	}
	var expiresNs any
	if !e.ExpiresAt.IsZero() {
		expiresNs = e.ExpiresAt.UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(e.Scope), e.ScopeID, e.Key, string(e.Value), expiresNs, createdNs, updatedNs,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore memory: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreChannelMessage implements store.Store. INSERT OR
// IGNORE on id PK → idempotent.
func (s *Store) SnapshotRestoreChannelMessage(ctx context.Context, m store.ChannelMessage) (bool, error) {
	if m.ID == "" || m.Channel == "" {
		return false, fmt.Errorf("snapshot restore channel_message: id and channel required")
	}
	publishedNs := m.PublishedAt.UnixNano()
	if m.PublishedAt.IsZero() {
		publishedNs = time.Now().UnixNano()
	}
	var expiresNs any
	if !m.ExpiresAt.IsZero() {
		expiresNs = m.ExpiresAt.UnixNano()
	}
	visibleNs := int64(0)
	if !m.VisibleAt.IsZero() {
		visibleNs = m.VisibleAt.UnixNano()
	} else {
		visibleNs = publishedNs
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO channel_messages(id, channel, scope, scope_id, payload, published_at, expires_at, visible_at, published_by_user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Channel, string(m.Scope), m.ScopeID, string(m.Payload),
		publishedNs, expiresNs, visibleNs, nilIfEmpty(m.PublishedByUserID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore channel_message: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreChannelCursor implements store.Store. INSERT OR
// IGNORE on (channel, scope, scope_id) — first restore writes the
// snapshot's cursor; subsequent restores leave an evolved live cursor
// alone so the (bool, error) return reads as "not inserted."
func (s *Store) SnapshotRestoreChannelCursor(ctx context.Context, c store.ChannelCursorEntry) (bool, error) {
	if c.Channel == "" || c.Cursor == "" {
		return false, fmt.Errorf("snapshot restore channel_cursor: channel and cursor required")
	}
	updatedNs := c.UpdatedAt.UnixNano()
	if c.UpdatedAt.IsZero() {
		updatedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO channel_cursors(channel, scope, scope_id, cursor, updated_at) VALUES (?, ?, ?, ?, ?)`,
		c.Channel, string(c.Scope), c.ScopeID, c.Cursor, updatedNs,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore channel_cursor: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreEvaluation implements store.Store. INSERT OR IGNORE
// on eval_id PK → idempotent.
func (s *Store) SnapshotRestoreEvaluation(ctx context.Context, r store.EvaluationRow) (bool, error) {
	if r.EvalID == "" || r.RunID == "" {
		return false, fmt.Errorf("snapshot restore evaluation: eval_id and run_id required")
	}
	createdNs := r.CreatedAt.UnixNano()
	if r.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	var dimensions, judgement sql.NullString
	if len(r.Dimensions) > 0 {
		b, err := json.Marshal(r.Dimensions)
		if err == nil {
			dimensions = sql.NullString{String: string(b), Valid: true}
		}
	}
	if len(r.Judgement) > 0 {
		judgement = sql.NullString{String: string(r.Judgement), Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO evaluations(
			eval_id, run_id, def_id, score, dimensions, judgement, rationale,
			emitter_role, emitter_agent_id, emitter_run_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.EvalID, r.RunID, nilIfEmpty(r.DefID), r.Score,
		dimensions, judgement, nilIfEmpty(r.Rationale),
		r.EmitterRole, nilIfEmpty(r.EmitterAgentID), nilIfEmpty(r.EmitterRunID),
		createdNs,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore evaluation: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MemorySet upserts a Memory row. ttl > 0 sets expires_at = now+ttl;
// ttl <= 0 clears the column to NULL (no expiry).
//
// Stored as JSON text in a TEXT column — SQLite has no native JSON
// type beyond what JSON1 functions consume; the tool layer is the
// source of truth for shape validation. (We also use the textual
// representation for the JSON-number parse in MemoryIncrement.)
func (s *Store) MemorySet(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error {
	now := time.Now().UnixNano()
	var expiresAt any
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UnixNano()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at`,
		string(scope), scopeID, key, string(value), expiresAt, now, now,
	)
	return err
}

// MemoryGet returns the entry or *ErrNotFound. Expired rows are
// surfaced as ErrNotFound regardless of whether the sweeper has
// reaped them yet.
func (s *Store) MemoryGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	var (
		valueText string
		expiresAt sql.NullInt64
		createdAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at, created_at, updated_at
		 FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
	}
	if err != nil {
		return store.MemoryEntry{}, err
	}
	if expiresAt.Valid && time.Now().UnixNano() > expiresAt.Int64 {
		return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
	}
	out := store.MemoryEntry{
		Key:       key,
		Value:     json.RawMessage(valueText),
		CreatedAt: time.Unix(0, createdAt),
		UpdatedAt: time.Unix(0, updatedAt),
	}
	if expiresAt.Valid {
		out.ExpiresAt = time.Unix(0, expiresAt.Int64)
	}
	return out, nil
}

// MemoryDelete removes a row. Returns whether a row was actually
// deleted; both outcomes are non-error.
func (s *Store) MemoryDelete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// MemoryList enumerates entries for a (scope, scopeID), filtered by
// prefix and capped at limit rows. Expired rows are filtered in the
// WHERE clause so callers never see them. truncated == true when the
// underlying query found at least limit+1 rows.
func (s *Store) MemoryList(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	nowNs := time.Now().UnixNano()
	// Fetch limit+1 to detect truncation without a separate COUNT(*).
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, expires_at, created_at, updated_at
		 FROM memory
		 WHERE scope = ? AND scope_id = ? AND key LIKE ? ESCAPE '\'
		   AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY key ASC
		 LIMIT ?`,
		string(scope), scopeID, escapeLikePrefix(prefix)+"%", nowNs, limit+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []store.MemoryEntry
	for rows.Next() {
		var (
			key       string
			valueText string
			expiresAt sql.NullInt64
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(&key, &valueText, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, false, err
		}
		entry := store.MemoryEntry{
			Key:       key,
			Value:     json.RawMessage(valueText),
			CreatedAt: time.Unix(0, createdAt),
			UpdatedAt: time.Unix(0, updatedAt),
		}
		if expiresAt.Valid {
			entry.ExpiresAt = time.Unix(0, expiresAt.Int64)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

// MemoryIncrement is the atomic counter primitive. SQLite has no
// native compare-and-set on JSON values, so we wrap the read +
// arithmetic + write in an IMMEDIATE transaction (a write lock at
// the BEGIN). Concurrent increments serialise on the lock; the
// loop is contention-free in the absence of writes.
//
// modernc/sqlite's database/sql driver does NOT translate
// `sql.LevelSerializable` to `BEGIN IMMEDIATE` — it only honors
// `_txlock=immediate` in the DSN (which would affect every
// transaction, including read paths where DEFERRED is preferred).
// We therefore pin a connection from the pool and issue
// `BEGIN IMMEDIATE` / `COMMIT` raw, scoping the lock-on-BEGIN
// behaviour to this one operation. Verified by a 100-goroutine
// regression test in storetest (counter must hit exactly 100).
func (s *Store) MemoryIncrement(ctx context.Context, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var (
		valueText sql.NullString
		expiresAt sql.NullInt64
	)
	err = conn.QueryRowContext(ctx,
		`SELECT value, expires_at FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt)
	now := time.Now()
	nowNs := now.UnixNano()

	var current int64
	rowExists := !errors.Is(err, sql.ErrNoRows)
	if rowExists && err != nil {
		return 0, err
	}
	// Treat expired rows as missing — increment from zero rather than
	// the stale value.
	if rowExists && expiresAt.Valid && nowNs > expiresAt.Int64 {
		rowExists = false
	}
	if rowExists {
		text := strings.TrimSpace(valueText.String)
		n, parseErr := strconv.ParseInt(text, 10, 64)
		if parseErr != nil {
			// Fall back to JSON parse: covers floats expressed as
			// integers ("3.0") which strconv rejects but JSON allows.
			var f float64
			if jsonErr := json.Unmarshal([]byte(text), &f); jsonErr != nil {
				return 0, store.ErrMemoryWrongType
			}
			if f != float64(int64(f)) {
				return 0, store.ErrMemoryWrongType
			}
			n = int64(f)
		}
		current = n
	}
	next := current + delta
	nextText := strconv.FormatInt(next, 10)

	var newExpires any
	switch {
	case ttl > 0:
		newExpires = now.Add(ttl).UnixNano()
	case rowExists && expiresAt.Valid:
		newExpires = expiresAt.Int64 // preserve existing expiry
	}

	_, err = conn.ExecContext(ctx,
		`INSERT INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at`,
		string(scope), scopeID, key, nextText, newExpires, nowNs, nowNs,
	)
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	committed = true
	return next, nil
}

// MemoryAtomicUpdate is the generic read-modify-write primitive the
// v0.12.x reducer ops (Memory.merge / append_dedupe / bounded_list)
// build on. Mirrors MemoryIncrement's BEGIN IMMEDIATE locking, but
// hands the value-derivation step to the caller's reducer closure.
//
// SQLite serialises writes anyway (single-writer / WAL), so BEGIN
// IMMEDIATE is the cleanest correctness fence: any concurrent
// MemoryAtomicUpdate on the same DB will queue at the transaction
// boundary regardless of which key it targets. Coarser than
// Postgres's per-key advisory lock, but appropriate for SQLite's
// concurrency model.
func (s *Store) MemoryAtomicUpdate(
	ctx context.Context,
	scope store.MemoryScope,
	scopeID, key string,
	ttl time.Duration,
	reducer func(existing json.RawMessage) (json.RawMessage, error),
) (json.RawMessage, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var (
		valueText sql.NullString
		expiresAt sql.NullInt64
	)
	err = conn.QueryRowContext(ctx,
		`SELECT value, expires_at FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt)
	now := time.Now()
	nowNs := now.UnixNano()

	rowExists := !errors.Is(err, sql.ErrNoRows)
	if rowExists && err != nil {
		return nil, err
	}
	if rowExists && expiresAt.Valid && nowNs > expiresAt.Int64 {
		// Treat expired rows as missing — reducer sees empty input.
		rowExists = false
	}

	var existing json.RawMessage
	if rowExists {
		existing = json.RawMessage(valueText.String)
	}
	next, err := reducer(existing)
	if err != nil {
		// Tool layer wraps for the agent-visible message.
		return nil, err
	}
	if !json.Valid(next) {
		return nil, fmt.Errorf("memory atomic update: reducer returned invalid JSON")
	}

	var newExpires any
	switch {
	case ttl > 0:
		newExpires = now.Add(ttl).UnixNano()
	case rowExists && expiresAt.Valid:
		newExpires = expiresAt.Int64
	}

	_, err = conn.ExecContext(ctx,
		`INSERT INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at`,
		string(scope), scopeID, key, string(next), newExpires, nowNs, nowNs,
	)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return next, nil
}

// MemoryListScopeIDs returns distinct scope_ids under scope with
// summary stats. Excludes expired rows so operators see live state
// only. Capped at 200 rows ordered by updated_at DESC.
func (s *Store) MemoryListScopeIDs(ctx context.Context, scope store.MemoryScope) ([]store.MemoryScopeIDSummary, error) {
	nowNs := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			scope_id,
			COUNT(*)                                              AS key_count,
			COALESCE(SUM(LENGTH(key) + LENGTH(value)), 0)          AS bytes,
			MAX(updated_at)                                        AS updated_at
		FROM memory
		WHERE scope = ? AND (expires_at IS NULL OR expires_at > ?)
		GROUP BY scope_id
		ORDER BY updated_at DESC
		LIMIT 200`,
		string(scope), nowNs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MemoryScopeIDSummary
	for rows.Next() {
		var (
			summary   store.MemoryScopeIDSummary
			updatedNs int64
		)
		if err := rows.Scan(&summary.ScopeID, &summary.KeyCount, &summary.Bytes, &updatedNs); err != nil {
			return nil, err
		}
		summary.UpdatedAt = time.Unix(0, updatedNs).UTC()
		out = append(out, summary)
	}
	return out, rows.Err()
}

// MemorySweep deletes every Memory row whose expires_at has passed.
func (s *Store) MemorySweep(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().UnixNano(),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// ---- v0.8.4 Channel tool ----
//
// All five methods are single-table operations against
// channel_messages / channel_cursors. Per-(channel, scope, scope_id)
// reads use the composite primary key directly — no extra index
// needed. The expires_at filter happens at read time AND in the
// sweeper; readers never see expired rows even if the sweeper has
// lagged.

// ChannelPublish appends one message and trims the channel down to
// maxMessages inside the same txn (oldest rows go first). maxMessages
// <= 0 disables the trim. Returns the assigned id and the trim count.
//
// v0.8.6: visible_at + published_by_user_id are honoured. Deferred
// publishes (VisibleAt > now) land in storage immediately but are
// hidden from reads until visible_at <= now; the tool layer's
// scheduler schedules a Bus.Notify(channel) at visible_at so
// long-poll subscribers wake on time.
func (s *Store) ChannelPublish(ctx context.Context, msg store.ChannelMessage, maxMessages int) (string, int, error) {
	now := time.Now()
	msg.ID = store.MintChannelMessageID(now)
	msg.PublishedAt = now
	if msg.VisibleAt.IsZero() || msg.VisibleAt.Before(now) {
		msg.VisibleAt = now
	}

	var expiresAt any
	if !msg.ExpiresAt.IsZero() {
		expiresAt = msg.ExpiresAt.UnixNano()
	}
	var publishedByUserID any
	if msg.PublishedByUserID != "" {
		publishedByUserID = msg.PublishedByUserID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel_messages(id, channel, scope, scope_id, payload, published_at, expires_at, visible_at, published_by_user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.Channel, string(msg.Scope), msg.ScopeID, string(msg.Payload),
		now.UnixNano(), expiresAt, msg.VisibleAt.UnixNano(), publishedByUserID,
	); err != nil {
		return "", 0, err
	}

	dropped := 0
	if maxMessages > 0 {
		// Trim by deleting the oldest rows beyond maxMessages. We
		// ORDER BY id (= publish time) so the trim is deterministic.
		// The subquery selects the surviving "keep" set; everything
		// older is dropped. This is the lossy-on-overflow shape from
		// the v0.8.4 RFC — publisher never blocks.
		//
		// The `id != ?` clause protects against the Postgres
		// READ-COMMITTED race where two concurrent publishers to the
		// same (channel, scope, scope_id) can each see the other's
		// committed row inside their own trim subquery. Without this
		// guard, A's INSERT X + concurrent B's commit of Y > X means
		// A's trim subquery picks Y as top-N (excluding X by lex
		// order) and A's DELETE removes its own just-inserted X.
		// A then commits and reports success to its caller, but X
		// is gone. With the guard, the just-inserted row is never
		// in the DELETE candidate set under any race.
		//
		// SQLite is single-writer (WAL) so the race doesn't occur,
		// but the guard adds no cost and keeps the two backends'
		// SQL identical.
		//
		// v0.8.6: the trim subquery orders by (visible_at, id) DESC
		// to match the read path's delivery order. With pure id DESC
		// (= publish-time order) a deferred message published earlier
		// but with future visible_at would sort BEFORE a later
		// immediate publish — the trim would drop the deferred row
		// silently, even though it represents pending work the
		// subscriber will eventually want to see.
		res, err := tx.ExecContext(ctx,
			`DELETE FROM channel_messages
			 WHERE channel = ? AND scope = ? AND scope_id = ?
			   AND id != ?
			   AND id NOT IN (
			     SELECT id FROM channel_messages
			      WHERE channel = ? AND scope = ? AND scope_id = ?
			      ORDER BY visible_at DESC, id DESC
			      LIMIT ?
			   )`,
			msg.Channel, string(msg.Scope), msg.ScopeID, msg.ID,
			msg.Channel, string(msg.Scope), msg.ScopeID,
			maxMessages,
		)
		if err != nil {
			return "", 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			dropped = int(n)
		}
	}
	commitStart := time.Now()
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	if s.channelDebug {
		// Parity with the postgres backend's diagnostic — see
		// internal/store/postgres/postgres.go for the hypothesis
		// background. SQLite is single-writer (WAL) so the
		// commit-visibility race shouldn't apply, but logging here
		// lets us A/B the same load against both backends and
		// confirm the race is Postgres-specific.
		log.Printf("channel %q publish: id=%s commit_us=%d",
			msg.Channel, msg.ID, time.Since(commitStart).Microseconds())
	}
	return msg.ID, dropped, nil
}

// ChannelSubscribe reads up to `limit` messages newer than fromCursor.
// fromCursor == "" || "cur_0" → from the oldest non-expired row.
// Returns the batch + the id of the LAST message as nextCursor.
func (s *Store) ChannelSubscribe(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, string, error) {
	return s.channelRead(ctx, channel, scope, scopeID, fromCursor, limit)
}

// ChannelPeek is identical to ChannelSubscribe (non-consuming — the
// cursor table is never touched on either path). The semantic
// difference lives entirely in the tool layer: Subscribe optionally
// commits the returned cursor on the next call, Peek never does.
func (s *Store) ChannelPeek(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, error) {
	msgs, _, err := s.channelRead(ctx, channel, scope, scopeID, fromCursor, limit)
	return msgs, err
}

func (s *Store) ChannelStats(ctx context.Context) ([]store.ChannelStats, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel, COUNT(*), MIN(visible_at), MAX(visible_at)
		FROM channel_messages
		WHERE (expires_at IS NULL OR expires_at > ?)
		GROUP BY channel
		ORDER BY channel`, now)
	if err != nil {
		return nil, fmt.Errorf("channel stats query: %w", err)
	}
	defer rows.Close()

	var out []store.ChannelStats
	for rows.Next() {
		var (
			name             string
			count            int64
			oldestNS, newest sql.NullInt64
		)
		if err := rows.Scan(&name, &count, &oldestNS, &newest); err != nil {
			return nil, fmt.Errorf("channel stats scan: %w", err)
		}
		st := store.ChannelStats{Channel: name, MessageCount: count}
		if oldestNS.Valid {
			st.OldestVisibleAt = time.Unix(0, oldestNS.Int64).UTC()
		}
		if newest.Valid {
			st.NewestVisibleAt = time.Unix(0, newest.Int64).UTC()
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("channel stats iterate: %w", err)
	}
	return out, nil
}

// BackfillAgentDefSystemPromptBase walks rows missing the
// system_prompt_base JSON field + copies system_prompt into it. See
// store.Store doc for the rationale (PR #186 follow-up).
func (s *Store) BackfillAgentDefSystemPromptBase(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT def_id, definition FROM agent_defs`)
	if err != nil {
		return 0, fmt.Errorf("backfill system_prompt_base read: %w", err)
	}
	type pending struct {
		DefID string
		Def   []byte
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.DefID, &p.Def); err != nil {
			rows.Close()
			return 0, fmt.Errorf("backfill system_prompt_base scan: %w", err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("backfill system_prompt_base iterate: %w", err)
	}

	n := 0
	for _, p := range todo {
		updated, ok, err := backfillSystemPromptBase(p.Def)
		if err != nil {
			// Hand-edited row with broken JSON — log + skip rather than
			// abort the whole backfill. The read-side normalizer in
			// internal/lookup will still fill SystemPromptBase at runtime,
			// but this log line is the operator's only signal that a row
			// was left untouched.
			log.Printf("agent_defs: backfill system_prompt_base: def_id=%s: JSON parse failed, skipping: %v", p.DefID, err)
			continue
		}
		if !ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE agent_defs SET definition = ? WHERE def_id = ?`,
			updated, p.DefID); err != nil {
			return n, fmt.Errorf("backfill system_prompt_base update %s: %w", p.DefID, err)
		}
		n++
	}
	return n, nil
}

// backfillSystemPromptBase is the JSON-layer transform shared by
// the sqlite + postgres backfill methods. Returns (newDef, true,
// nil) when the row needed a fill; (nil, false, nil) when it didn't;
// (nil, false, err) on JSON parse failure.
func backfillSystemPromptBase(def []byte) ([]byte, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(def, &raw); err != nil {
		return nil, false, err
	}
	existing, _ := raw["system_prompt_base"].(string)
	if existing != "" {
		return nil, false, nil
	}
	sp, _ := raw["system_prompt"].(string)
	if sp == "" {
		// No system_prompt either — nothing to backfill from. Leave
		// the row as-is; the read-side normalizer is a no-op too.
		return nil, false, nil
	}
	raw["system_prompt_base"] = sp
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// BackfillAgentDefContentSHA256 walks NULL/empty rows + populates the
// column via the injected signFn. See store.Store doc for invariants.
func (s *Store) BackfillAgentDefContentSHA256(ctx context.Context, signFn func(name string, def []byte) (string, error)) (int, error) {
	return s.backfillContentSHA256(ctx, "agent_defs", signFn)
}

// BackfillSkillDefContentSHA256 — mirror against skill_defs.
func (s *Store) BackfillSkillDefContentSHA256(ctx context.Context, signFn func(name string, def []byte) (string, error)) (int, error) {
	return s.backfillContentSHA256(ctx, "skill_defs", signFn)
}

func (s *Store) backfillContentSHA256(ctx context.Context, table string, signFn func(name string, def []byte) (string, error)) (int, error) {
	if table != "agent_defs" && table != "skill_defs" && table != "mcp_server_defs" {
		return 0, fmt.Errorf("backfill: unexpected table %q", table)
	}
	// Table name is whitelisted above, so safe to interpolate; we
	// avoid prepared-statement reuse on the read because the column
	// list is identical for both tables.
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_id, name, definition FROM `+table+` WHERE content_sha256 IS NULL OR content_sha256 = ''`)
	if err != nil {
		return 0, fmt.Errorf("backfill %s read: %w", table, err)
	}
	type pending struct {
		DefID string
		Name  string
		Def   []byte
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.DefID, &p.Name, &p.Def); err != nil {
			rows.Close()
			return 0, fmt.Errorf("backfill %s scan: %w", table, err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("backfill %s iterate: %w", table, err)
	}

	n := 0
	for _, p := range todo {
		hash, err := signFn(p.Name, p.Def)
		if err != nil {
			return n, fmt.Errorf("backfill %s sign def_id=%s: %w", table, p.DefID, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE `+table+` SET content_sha256 = ? WHERE def_id = ?`,
			hash, p.DefID); err != nil {
			return n, fmt.Errorf("backfill %s update def_id=%s: %w", table, p.DefID, err)
		}
		n++
	}
	return n, nil
}

// channelRead is the shared read body for Subscribe + Peek. expired-
// at-read-time filter applies on both paths.
//
// v0.8.6: cursor is a (visible_at, msg_id) tuple. Delivery order =
// visible_at first, then msg_id within the same visible_at. The
// `visible_at <= now` filter hides deferred publishes until their
// delivery time. nextCursor encodes the LAST row's (visible_at, id)
// so subscribers can pick up exactly where they left off, including
// deferred messages that become visible later than other messages
// published in between.
func (s *Store) channelRead(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, string, error) {
	if limit <= 0 {
		limit = 10
	}
	cursorVisibleAt, cursorMsgID, fromOldest, err := store.DecodeChannelCursor(fromCursor)
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UnixNano()
	var (
		rows  *sql.Rows
		qErr  error
		query string
	)
	if fromOldest {
		query = `SELECT id, payload, published_at, expires_at, visible_at, published_by_user_id
			 FROM channel_messages
			 WHERE channel = ? AND scope = ? AND scope_id = ?
			   AND visible_at <= ?
			   AND (expires_at IS NULL OR expires_at > ?)
			 ORDER BY visible_at ASC, id ASC
			 LIMIT ?`
		rows, qErr = s.db.QueryContext(ctx, query,
			channel, string(scope), scopeID, now, now, limit)
	} else {
		// Strictly-greater-than tuple comparison: (visible_at, id) > (cv, cid).
		query = `SELECT id, payload, published_at, expires_at, visible_at, published_by_user_id
			 FROM channel_messages
			 WHERE channel = ? AND scope = ? AND scope_id = ?
			   AND visible_at <= ?
			   AND (expires_at IS NULL OR expires_at > ?)
			   AND (visible_at > ? OR (visible_at = ? AND id > ?))
			 ORDER BY visible_at ASC, id ASC
			 LIMIT ?`
		rows, qErr = s.db.QueryContext(ctx, query,
			channel, string(scope), scopeID, now, now,
			cursorVisibleAt.UnixNano(), cursorVisibleAt.UnixNano(), cursorMsgID,
			limit)
	}
	if qErr != nil {
		return nil, "", qErr
	}
	defer rows.Close()

	var msgs []store.ChannelMessage
	var lastVisibleAt time.Time
	var lastID string
	for rows.Next() {
		var (
			id                string
			payload           string
			publishedAt       int64
			expiresAt         sql.NullInt64
			visibleAt         int64
			publishedByUserID sql.NullString
		)
		if err := rows.Scan(&id, &payload, &publishedAt, &expiresAt, &visibleAt, &publishedByUserID); err != nil {
			return nil, "", err
		}
		msg := store.ChannelMessage{
			ID:          id,
			Channel:     channel,
			Scope:       scope,
			ScopeID:     scopeID,
			Payload:     json.RawMessage(payload),
			PublishedAt: time.Unix(0, publishedAt),
			VisibleAt:   time.Unix(0, visibleAt),
		}
		if expiresAt.Valid {
			msg.ExpiresAt = time.Unix(0, expiresAt.Int64)
		}
		if publishedByUserID.Valid {
			msg.PublishedByUserID = publishedByUserID.String
		}
		msgs = append(msgs, msg)
		lastVisibleAt = msg.VisibleAt
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var nextCursor string
	if lastID != "" {
		nextCursor = store.EncodeChannelCursor(lastVisibleAt, lastID)
	}
	return msgs, nextCursor, nil
}

// ChannelAck commits cursor to the per-subscriber row. Rejects cursor
// values older than the currently committed one (lexicographic order
// matches tuple order because the v0.8.6 cursor format encodes
// visible_at as a fixed-width hex prefix). Idempotent re-ack of the
// SAME cursor is a no-op.
func (s *Store) ChannelAck(ctx context.Context, channel string, scope store.MemoryScope, scopeID, cursor string) error {
	if cursor == "" || cursor == "cur_0" {
		return nil // nothing to commit
	}
	// Validate format — reject legacy `msg_<hex>` cursors and garbage
	// rather than silently storing them.
	if _, _, _, err := store.DecodeChannelCursor(cursor); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT cursor FROM channel_cursors WHERE channel = ? AND scope = ? AND scope_id = ?`,
		channel, string(scope), scopeID,
	).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if existing != "" && cursor < existing {
		return store.ErrChannelCursorRegression
	}
	if existing == cursor {
		return tx.Commit() // idempotent
	}

	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel_cursors(channel, scope, scope_id, cursor, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(channel, scope, scope_id) DO UPDATE SET
		    cursor = excluded.cursor,
		    updated_at = excluded.updated_at`,
		channel, string(scope), scopeID, cursor, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ChannelCommittedCursor returns the last cursor ack'd for a
// subscriber, or empty string when none.
func (s *Store) ChannelCommittedCursor(ctx context.Context, channel string, scope store.MemoryScope, scopeID string) (string, error) {
	var cursor string
	err := s.db.QueryRowContext(ctx,
		`SELECT cursor FROM channel_cursors WHERE channel = ? AND scope = ? AND scope_id = ?`,
		channel, string(scope), scopeID,
	).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cursor, nil
}

// ChannelListCursorsForScope — see store.Store doc. v0.9.x
// introspection. Ordered by channel ASC for deterministic UI render.
func (s *Store) ChannelListCursorsForScope(ctx context.Context, scope store.MemoryScope, scopeID string) ([]store.ChannelCursorEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, scope, scope_id, cursor, updated_at
		 FROM channel_cursors
		 WHERE scope = ? AND scope_id = ?
		 ORDER BY channel ASC`,
		string(scope), scopeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.ChannelCursorEntry{}
	for rows.Next() {
		var entry store.ChannelCursorEntry
		var scopeStr string
		var updatedNanos int64
		if err := rows.Scan(&entry.Channel, &scopeStr, &entry.ScopeID, &entry.Cursor, &updatedNanos); err != nil {
			return nil, err
		}
		entry.Scope = store.MemoryScope(scopeStr)
		entry.UpdatedAt = time.Unix(0, updatedNanos).UTC()
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ChannelSweepExpired deletes every expired row. Mirror of MemorySweep.
func (s *Store) ChannelSweepExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM channel_messages WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().UnixNano(),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// ---- v0.8.5 Self-Evolution Substrate ----
//
// AgentDef methods. Append-only. Version is allocated under a
// per-name lock (BEGIN IMMEDIATE in sqlite — coarse but correct for
// single-writer WAL, mirrors MemoryIncrement's pattern).

// AgentDefCreate allocates the next version for row.Name under a
// per-name lock and inserts. The caller supplies row.DefID (UUID/
// ULID-ish opaque string). Validates parent_def_id when set.
//
// SQLite concurrency: uses the same BEGIN IMMEDIATE + pinned-conn
// pattern as MemoryIncrement — pinning the connection scopes the
// write lock to this one transaction, and IMMEDIATE means concurrent
// writers see SQLITE_BUSY at BEGIN time (database/sql retries) rather
// than upgrade-deadlocking mid-tx. Without this, two concurrent
// AgentDefCreate calls against the same name both start a DEFERRED
// tx, both SELECT MAX(version) (returning the same value), then both
// try to INSERT with the same version — one succeeds, one fails on
// the UNIQUE(name, version) constraint.
func (s *Store) AgentDefCreate(ctx context.Context, row store.AgentDefRow) (store.AgentDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.AgentDefRow{}, fmt.Errorf("agent_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.AgentDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.AgentDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.AgentDefRow{}, err
		}
		if n == 0 {
			return store.AgentDefRow{}, store.ErrAgentDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM agent_defs WHERE name = ?`, row.Name,
	).Scan(&maxVer); err != nil {
		return store.AgentDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO agent_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256),
	); err != nil {
		return store.AgentDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.AgentDefRow{}, err
	}
	committed = true
	return row, nil
}

// AgentDefGet returns one row by def_id.
func (s *Store) AgentDefGet(ctx context.Context, defID string) (store.AgentDefRow, error) {
	row, err := s.scanAgentDef(s.db.QueryRowContext(ctx, agentDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	return row, err
}

// AgentDefGetByNameVersion returns one row by (name, version).
func (s *Store) AgentDefGetByNameVersion(ctx context.Context, name string, version int) (store.AgentDefRow, error) {
	row, err := s.scanAgentDef(s.db.QueryRowContext(ctx, agentDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

// AgentDefListByName returns rows for one name, version DESC.
func (s *Store) AgentDefListByName(ctx context.Context, name string) ([]store.AgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, agentDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAgentDefRows(rows)
}

// AgentDefListChildren returns immediate children (parent_def_id == arg).
func (s *Store) AgentDefListChildren(ctx context.Context, parentDefID string) ([]store.AgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, agentDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAgentDefRows(rows)
}

// AgentDefListNames returns one summary row per distinct name. Joins
// agent_def_active to surface the active def_id when one exists.
func (s *Store) AgentDefListNames(ctx context.Context) ([]store.AgentDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM agent_defs d
		LEFT JOIN agent_def_active a ON a.name = d.name
		GROUP BY d.name, a.def_id
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.AgentDefNameSummary
	for rows.Next() {
		var s store.AgentDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&s.Name, &s.VersionCount, &s.LatestVersion, &updatedAt, &s.ActiveDefID); err != nil {
			return nil, err
		}
		s.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentDefSetActive UPSERTs the agent_def_active pointer for name.
func (s *Store) AgentDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error {
	// Validate def_id exists + matches name (defence-in-depth; the
	// FK isn't enforced without foreign_keys PRAGMA).
	var rowName string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM agent_defs WHERE def_id = ?`, defID).Scan(&rowName)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("agent_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_def_active (name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

// AgentDefGetActive returns the active row for name. *ErrNotFound
// when no pointer exists — caller falls through to cfg.Agents.
func (s *Store) AgentDefGetActive(ctx context.Context, name string) (store.AgentDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM agent_def_active WHERE name = ?`, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def_active", ID: name}
	}
	if err != nil {
		return store.AgentDefRow{}, err
	}
	return s.AgentDefGet(ctx, defID)
}

// AgentDefSetRetired flips the `retired` flag on one row. The row
// stays visible in lineage queries.
func (s *Store) AgentDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	return nil
}

// agentDefSelect is the column list shared by every read. Kept in
// one place so column additions (a future tenant_id, similarity_score,
// ...) need only a single touch-point.
const agentDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	COALESCE(content_sha256, '')
FROM agent_defs`

func (s *Store) scanAgentDef(row *sql.Row) (store.AgentDefRow, error) {
	var (
		out        store.AgentDefRow
		definition string
		createdAt  int64
		retired    int
		bootstrap  int
	)
	err := row.Scan(
		&out.DefID, &out.Name, &out.Version,
		&out.ParentDefID,
		&definition,
		&out.Description,
		&createdAt,
		&out.CreatedByAgentID, &out.CreatedByRunID,
		&retired, &bootstrap,
		&out.ContentSHA256,
	)
	if err != nil {
		return store.AgentDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanAgentDefRows(rows *sql.Rows) ([]store.AgentDefRow, error) {
	var out []store.AgentDefRow
	for rows.Next() {
		var (
			r          store.AgentDefRow
			definition string
			createdAt  int64
			retired    int
			bootstrap  int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version,
			&r.ParentDefID,
			&definition,
			&r.Description,
			&createdAt,
			&r.CreatedByAgentID, &r.CreatedByRunID,
			&retired, &bootstrap,
			&r.ContentSHA256,
		); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		r.Retired = retired != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v0.8.22 SkillDef substrate ----
//
// Direct mirror of the AgentDef methods above. Same per-name lock
// for version monotonicity, same scan/select helpers, same error
// surfaces. The only divergence is the table name and the typed
// error (ErrSkillDefParentNotFound instead of ErrAgentDefParentNotFound).
// If you fix a bug in one of these methods, fix it in the AgentDef
// twin as well.

func (s *Store) SkillDefCreate(ctx context.Context, row store.SkillDefRow) (store.SkillDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.SkillDefRow{}, fmt.Errorf("skill_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.SkillDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.SkillDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.SkillDefRow{}, err
		}
		if n == 0 {
			return store.SkillDefRow{}, store.ErrSkillDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM skill_defs WHERE name = ?`, row.Name,
	).Scan(&maxVer); err != nil {
		return store.SkillDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO skill_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256),
	); err != nil {
		return store.SkillDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.SkillDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) SkillDefGet(ctx context.Context, defID string) (store.SkillDefRow, error) {
	row, err := s.scanSkillDef(s.db.QueryRowContext(ctx, skillDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.SkillDefRow{}, &store.ErrNotFound{Kind: "skill_def", ID: defID}
	}
	return row, err
}

func (s *Store) SkillDefGetByNameVersion(ctx context.Context, name string, version int) (store.SkillDefRow, error) {
	row, err := s.scanSkillDef(s.db.QueryRowContext(ctx, skillDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.SkillDefRow{}, &store.ErrNotFound{Kind: "skill_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) SkillDefListByName(ctx context.Context, name string) ([]store.SkillDefRow, error) {
	rows, err := s.db.QueryContext(ctx, skillDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanSkillDefRows(rows)
}

func (s *Store) SkillDefListChildren(ctx context.Context, parentDefID string) ([]store.SkillDefRow, error) {
	rows, err := s.db.QueryContext(ctx, skillDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanSkillDefRows(rows)
}

func (s *Store) SkillDefListNames(ctx context.Context) ([]store.SkillDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM skill_defs d
		LEFT JOIN skill_def_active a ON a.name = d.name
		GROUP BY d.name, a.def_id
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.SkillDefNameSummary
	for rows.Next() {
		var ns store.SkillDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

func (s *Store) SkillDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error {
	var rowName string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM skill_defs WHERE def_id = ?`, defID).Scan(&rowName)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "skill_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("skill_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO skill_def_active (name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) SkillDefGetActive(ctx context.Context, name string) (store.SkillDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM skill_def_active WHERE name = ?`, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.SkillDefRow{}, &store.ErrNotFound{Kind: "skill_def_active", ID: name}
	}
	if err != nil {
		return store.SkillDefRow{}, err
	}
	return s.SkillDefGet(ctx, defID)
}

func (s *Store) SkillDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE skill_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "skill_def", ID: defID}
	}
	return nil
}

const skillDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	COALESCE(content_sha256, '')
FROM skill_defs`

func (s *Store) scanSkillDef(row *sql.Row) (store.SkillDefRow, error) {
	var (
		out        store.SkillDefRow
		definition string
		createdAt  int64
		retired    int
		bootstrap  int
	)
	err := row.Scan(
		&out.DefID, &out.Name, &out.Version,
		&out.ParentDefID,
		&definition,
		&out.Description,
		&createdAt,
		&out.CreatedByAgentID, &out.CreatedByRunID,
		&retired, &bootstrap,
		&out.ContentSHA256,
	)
	if err != nil {
		return store.SkillDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanSkillDefRows(rows *sql.Rows) ([]store.SkillDefRow, error) {
	var out []store.SkillDefRow
	for rows.Next() {
		var (
			r          store.SkillDefRow
			definition string
			createdAt  int64
			retired    int
			bootstrap  int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version,
			&r.ParentDefID,
			&definition,
			&r.Description,
			&createdAt,
			&r.CreatedByAgentID, &r.CreatedByRunID,
			&retired, &bootstrap,
			&r.ContentSHA256,
		); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		r.Retired = retired != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v0.9.x MCPServerDef substrate ----
//
// Direct mirror of the SkillDef methods above. Same per-name lock
// for version monotonicity, same scan/select helpers, same error
// surfaces. The only divergence is the table name and the typed
// error (ErrMCPServerDefParentNotFound instead of ErrSkillDefParentNotFound).

func (s *Store) MCPServerDefCreate(ctx context.Context, row store.MCPServerDefRow) (store.MCPServerDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.MCPServerDefRow{}, fmt.Errorf("mcp_server_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.MCPServerDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.MCPServerDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_server_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.MCPServerDefRow{}, err
		}
		if n == 0 {
			return store.MCPServerDefRow{}, store.ErrMCPServerDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM mcp_server_defs WHERE name = ?`, row.Name,
	).Scan(&maxVer); err != nil {
		return store.MCPServerDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO mcp_server_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256),
	); err != nil {
		return store.MCPServerDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.MCPServerDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) MCPServerDefGet(ctx context.Context, defID string) (store.MCPServerDefRow, error) {
	row, err := s.scanMCPServerDef(s.db.QueryRowContext(ctx, mcpServerDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.MCPServerDefRow{}, &store.ErrNotFound{Kind: "mcp_server_def", ID: defID}
	}
	return row, err
}

func (s *Store) MCPServerDefGetByNameVersion(ctx context.Context, name string, version int) (store.MCPServerDefRow, error) {
	row, err := s.scanMCPServerDef(s.db.QueryRowContext(ctx, mcpServerDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.MCPServerDefRow{}, &store.ErrNotFound{Kind: "mcp_server_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) MCPServerDefListByName(ctx context.Context, name string) ([]store.MCPServerDefRow, error) {
	rows, err := s.db.QueryContext(ctx, mcpServerDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanMCPServerDefRows(rows)
}

func (s *Store) MCPServerDefListChildren(ctx context.Context, parentDefID string) ([]store.MCPServerDefRow, error) {
	rows, err := s.db.QueryContext(ctx, mcpServerDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanMCPServerDefRows(rows)
}

func (s *Store) MCPServerDefListNames(ctx context.Context) ([]store.MCPServerDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM mcp_server_defs d
		LEFT JOIN mcp_server_def_active a ON a.name = d.name
		GROUP BY d.name, a.def_id
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.MCPServerDefNameSummary
	for rows.Next() {
		var ns store.MCPServerDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

func (s *Store) MCPServerDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error {
	var rowName string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM mcp_server_defs WHERE def_id = ?`, defID).Scan(&rowName)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "mcp_server_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("mcp_server_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mcp_server_def_active (name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) MCPServerDefGetActive(ctx context.Context, name string) (store.MCPServerDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM mcp_server_def_active WHERE name = ?`, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.MCPServerDefRow{}, &store.ErrNotFound{Kind: "mcp_server_def_active", ID: name}
	}
	if err != nil {
		return store.MCPServerDefRow{}, err
	}
	return s.MCPServerDefGet(ctx, defID)
}

func (s *Store) MCPServerDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mcp_server_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "mcp_server_def", ID: defID}
	}
	return nil
}

// BackfillMCPServerDefContentSHA256 — see store.Store doc.
func (s *Store) BackfillMCPServerDefContentSHA256(ctx context.Context, signFn func(name string, def []byte) (string, error)) (int, error) {
	return s.backfillContentSHA256(ctx, "mcp_server_defs", signFn)
}

const mcpServerDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	COALESCE(content_sha256, '')
FROM mcp_server_defs`

func (s *Store) scanMCPServerDef(row *sql.Row) (store.MCPServerDefRow, error) {
	var (
		out        store.MCPServerDefRow
		definition string
		createdAt  int64
		retired    int
		bootstrap  int
	)
	err := row.Scan(
		&out.DefID, &out.Name, &out.Version,
		&out.ParentDefID,
		&definition,
		&out.Description,
		&createdAt,
		&out.CreatedByAgentID, &out.CreatedByRunID,
		&retired, &bootstrap,
		&out.ContentSHA256,
	)
	if err != nil {
		return store.MCPServerDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanMCPServerDefRows(rows *sql.Rows) ([]store.MCPServerDefRow, error) {
	var out []store.MCPServerDefRow
	for rows.Next() {
		var (
			r          store.MCPServerDefRow
			definition string
			createdAt  int64
			retired    int
			bootstrap  int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version,
			&r.ParentDefID,
			&definition,
			&r.Description,
			&createdAt,
			&r.CreatedByAgentID, &r.CreatedByRunID,
			&retired, &bootstrap,
			&r.ContentSHA256,
		); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		r.Retired = retired != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v1.x RFC E ScheduleDef substrate ----
//
// Mirror of MCPServerDef* without content_sha256.

func (s *Store) ScheduleDefCreate(ctx context.Context, row store.ScheduleDefRow) (store.ScheduleDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.ScheduleDefRow{}, fmt.Errorf("schedule_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.ScheduleDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.ScheduleDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedule_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.ScheduleDefRow{}, err
		}
		if n == 0 {
			return store.ScheduleDefRow{}, store.ErrScheduleDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schedule_defs WHERE name = ?`, row.Name,
	).Scan(&maxVer); err != nil {
		return store.ScheduleDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schedule_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
	); err != nil {
		return store.ScheduleDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.ScheduleDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) ScheduleDefGet(ctx context.Context, defID string) (store.ScheduleDefRow, error) {
	row, err := s.scanScheduleDef(s.db.QueryRowContext(ctx, scheduleDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.ScheduleDefRow{}, &store.ErrNotFound{Kind: "schedule_def", ID: defID}
	}
	return row, err
}

func (s *Store) ScheduleDefGetByNameVersion(ctx context.Context, name string, version int) (store.ScheduleDefRow, error) {
	row, err := s.scanScheduleDef(s.db.QueryRowContext(ctx, scheduleDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.ScheduleDefRow{}, &store.ErrNotFound{Kind: "schedule_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) ScheduleDefListByName(ctx context.Context, name string) ([]store.ScheduleDefRow, error) {
	rows, err := s.db.QueryContext(ctx, scheduleDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanScheduleDefRows(rows)
}

func (s *Store) ScheduleDefListChildren(ctx context.Context, parentDefID string) ([]store.ScheduleDefRow, error) {
	rows, err := s.db.QueryContext(ctx, scheduleDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanScheduleDefRows(rows)
}

func (s *Store) ScheduleDefListNames(ctx context.Context) ([]store.ScheduleDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM schedule_defs d
		LEFT JOIN schedule_def_active a ON a.name = d.name
		GROUP BY d.name, a.def_id
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.ScheduleDefNameSummary
	for rows.Next() {
		var ns store.ScheduleDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

func (s *Store) ScheduleDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error {
	var rowName string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM schedule_defs WHERE def_id = ?`, defID).Scan(&rowName)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "schedule_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("schedule_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO schedule_def_active (name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) ScheduleDefGetActive(ctx context.Context, name string) (store.ScheduleDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM schedule_def_active WHERE name = ?`, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.ScheduleDefRow{}, &store.ErrNotFound{Kind: "schedule_def_active", ID: name}
	}
	if err != nil {
		return store.ScheduleDefRow{}, err
	}
	return s.ScheduleDefGet(ctx, defID)
}

func (s *Store) ScheduleDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "schedule_def", ID: defID}
	}
	return nil
}

const scheduleDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static
FROM schedule_defs`

func (s *Store) scanScheduleDef(row *sql.Row) (store.ScheduleDefRow, error) {
	var (
		out        store.ScheduleDefRow
		definition string
		createdAt  int64
		retired    int
		bootstrap  int
	)
	err := row.Scan(
		&out.DefID, &out.Name, &out.Version,
		&out.ParentDefID,
		&definition,
		&out.Description,
		&createdAt,
		&out.CreatedByAgentID, &out.CreatedByRunID,
		&retired, &bootstrap,
	)
	if err != nil {
		return store.ScheduleDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanScheduleDefRows(rows *sql.Rows) ([]store.ScheduleDefRow, error) {
	var out []store.ScheduleDefRow
	for rows.Next() {
		var (
			r          store.ScheduleDefRow
			definition string
			createdAt  int64
			retired    int
			bootstrap  int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version,
			&r.ParentDefID,
			&definition,
			&r.Description,
			&createdAt,
			&r.CreatedByAgentID, &r.CreatedByRunID,
			&retired, &bootstrap,
		); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		r.Retired = retired != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v1.x RFC E ScheduleDef runtime (sweeper-side) ----

func (s *Store) ScheduleRunStateSeed(ctx context.Context, defID string, nextRunAt time.Time) error {
	// INSERT OR IGNORE keeps existing last_* fields when the row is
	// already present (re-promote of the same def doesn't reset its
	// completion history). Caller updates next_run_at separately if
	// needed via Pause/Resume + RecordResult.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schedule_run_state (def_id, next_run_at) VALUES (?, ?)
		 ON CONFLICT(def_id) DO UPDATE SET next_run_at = excluded.next_run_at`,
		defID, nextRunAt.UnixNano(),
	)
	return err
}

func (s *Store) ScheduleRunStateGet(ctx context.Context, defID string) (store.ScheduleRunStateRow, error) {
	var (
		out         store.ScheduleRunStateRow
		lastRunAt   sql.NullInt64
		lastRunID   sql.NullString
		lastStatus  sql.NullString
		lastError   sql.NullString
		nextRunAt   int64
		pausedUntil sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT def_id, last_run_at, last_run_id, last_status, last_error, next_run_at, paused_until
		 FROM schedule_run_state WHERE def_id = ?`, defID,
	).Scan(&out.DefID, &lastRunAt, &lastRunID, &lastStatus, &lastError, &nextRunAt, &pausedUntil)
	if err == sql.ErrNoRows {
		return store.ScheduleRunStateRow{}, &store.ErrNotFound{Kind: "schedule_run_state", ID: defID}
	}
	if err != nil {
		return store.ScheduleRunStateRow{}, err
	}
	if lastRunAt.Valid {
		out.LastRunAt = time.Unix(0, lastRunAt.Int64)
	}
	out.LastRunID = lastRunID.String
	out.LastStatus = lastStatus.String
	out.LastError = lastError.String
	out.NextRunAt = time.Unix(0, nextRunAt)
	if pausedUntil.Valid {
		out.PausedUntil = time.Unix(0, pausedUntil.Int64)
	}
	return out, nil
}

func (s *Store) ScheduleRunStateListDue(ctx context.Context, now time.Time) ([]store.ScheduleDueRow, error) {
	// JOIN: state ⨝ active ⨝ defs. The active-pointer JOIN drops any
	// state row whose def is no longer the active version (the sweeper
	// shouldn't fire stale forks). The retired = 0 filter drops
	// retired-via-flag rows; CASCADE on DELETE handles delete-retired.
	// The paused_until filter drops paused-until-future rows.
	rows, err := s.db.QueryContext(ctx,
		`SELECT srs.def_id, sd.name, sd.definition, srs.next_run_at
		 FROM schedule_run_state srs
		 JOIN schedule_def_active sda ON sda.def_id = srs.def_id
		 JOIN schedule_defs sd ON sd.def_id = srs.def_id
		 WHERE srs.next_run_at <= ?
		   AND sd.retired = 0
		   AND (srs.paused_until IS NULL OR srs.paused_until <= ?)
		 ORDER BY srs.next_run_at ASC`,
		now.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ScheduleDueRow
	for rows.Next() {
		var (
			r          store.ScheduleDueRow
			definition string
			nextRunAt  int64
		)
		if err := rows.Scan(&r.DefID, &r.Name, &definition, &nextRunAt); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.NextRunAt = time.Unix(0, nextRunAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ScheduleRunStateRecordResult(ctx context.Context, in store.ScheduleRunResult) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule_run_state SET
			last_run_at = ?,
			last_run_id = ?,
			last_status = ?,
			last_error = ?,
			next_run_at = ?
		 WHERE def_id = ?`,
		in.LastRunAt.UnixNano(), in.LastRunID, in.LastStatus, in.LastError,
		in.NextRunAt.UnixNano(), in.DefID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "schedule_run_state", ID: in.DefID}
	}
	return nil
}

func (s *Store) ScheduleRunStatePause(ctx context.Context, defID string, until time.Time) error {
	var arg any = nil
	if !until.IsZero() {
		arg = until.UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule_run_state SET paused_until = ? WHERE def_id = ?`,
		arg, defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "schedule_run_state", ID: defID}
	}
	return nil
}

// ---- Evaluation (pure-insert, no concurrency lock) ----

// EvaluationSubmit inserts one evaluation row. CreatedAt set by store.
func (s *Store) EvaluationSubmit(ctx context.Context, row store.EvaluationRow) (store.EvaluationRow, error) {
	if row.EvalID == "" || row.RunID == "" || row.EmitterRole == "" {
		return store.EvaluationRow{}, fmt.Errorf("evaluation: eval_id, run_id, emitter_role required")
	}
	row.CreatedAt = time.Now()
	var dimsJSON, judgementJSON sql.NullString
	if len(row.Dimensions) > 0 {
		b, err := json.Marshal(row.Dimensions)
		if err != nil {
			return store.EvaluationRow{}, fmt.Errorf("evaluation: marshal dimensions: %w", err)
		}
		dimsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(row.Judgement) > 0 {
		judgementJSON = sql.NullString{String: string(row.Judgement), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (
			eval_id, run_id, def_id, score, dimensions, judgement, rationale,
			emitter_role, emitter_agent_id, emitter_run_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.EvalID, row.RunID, nilIfEmpty(row.DefID), row.Score,
		dimsJSON, judgementJSON, nilIfEmpty(row.Rationale),
		row.EmitterRole, nilIfEmpty(row.EmitterAgentID), nilIfEmpty(row.EmitterRunID),
		row.CreatedAt.UnixNano(),
	)
	if err != nil {
		return store.EvaluationRow{}, err
	}
	return row, nil
}

// EvaluationGet returns one row by eval_id.
func (s *Store) EvaluationGet(ctx context.Context, evalID string) (store.EvaluationRow, error) {
	row, err := s.scanEvaluation(s.db.QueryRowContext(ctx, evaluationSelect+` WHERE eval_id = ?`, evalID))
	if err == sql.ErrNoRows {
		return store.EvaluationRow{}, &store.ErrNotFound{Kind: "evaluation", ID: evalID}
	}
	return row, err
}

// EvaluationListForRun returns evals targeting one run.
func (s *Store) EvaluationListForRun(ctx context.Context, runID string, limit int) ([]store.EvaluationRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, evaluationSelect+` WHERE run_id = ? ORDER BY created_at DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanEvaluationRows(rows)
}

// EvaluationListForDef returns evals targeting one def.
func (s *Store) EvaluationListForDef(ctx context.Context, defID string, limit int) ([]store.EvaluationRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, evaluationSelect+` WHERE def_id = ? ORDER BY created_at DESC LIMIT ?`, defID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanEvaluationRows(rows)
}

// EvaluationAggregate computes the score+dimension+by-role aggregates
// for a def_id. When opts.IncludeLineage is true, walks parent_def_id
// chain depth-first and includes ancestors.
func (s *Store) EvaluationAggregate(ctx context.Context, defID string, opts store.AggregateOpts) (store.AggregateResult, error) {
	defIDs := []string{defID}
	if opts.IncludeLineage {
		ancestors, err := s.walkAncestors(ctx, defID)
		if err != nil {
			return store.AggregateResult{}, err
		}
		defIDs = append(defIDs, ancestors...)
	}

	// Build the IN list. Limit defensively at 1000 ancestors so a
	// pathological lineage can't blow query parser limits — the
	// aggregator caller is responsible for not building megacycles.
	if len(defIDs) > 1000 {
		defIDs = defIDs[:1000]
	}
	placeholders := make([]string, len(defIDs))
	args := make([]any, len(defIDs))
	for i, id := range defIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := evaluationSelect + ` WHERE def_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return store.AggregateResult{}, err
	}
	defer rows.Close()
	evals, err := s.scanEvaluationRows(rows)
	if err != nil {
		return store.AggregateResult{}, err
	}
	return computeAggregate(defID, evals, opts.IncludeLineage), nil
}

// walkAncestors returns the parent_def_id chain for defID (NOT
// including defID itself). Depth-first, bounded at 100 hops to
// protect against the (impossible-by-construction-but-let's-be-safe)
// case of a cycle. Empty when defID has no parent.
func (s *Store) walkAncestors(ctx context.Context, defID string) ([]string, error) {
	var ancestors []string
	seen := map[string]bool{defID: true}
	cur := defID
	for i := 0; i < 100; i++ {
		var parent sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT parent_def_id FROM agent_defs WHERE def_id = ?`, cur).Scan(&parent)
		if err == sql.ErrNoRows || !parent.Valid || parent.String == "" {
			return ancestors, nil
		}
		if err != nil {
			return nil, err
		}
		if seen[parent.String] {
			return ancestors, nil // cycle guard
		}
		seen[parent.String] = true
		ancestors = append(ancestors, parent.String)
		cur = parent.String
	}
	return ancestors, nil
}

const evaluationSelect = `SELECT
	eval_id, run_id,
	COALESCE(def_id, ''),
	score,
	COALESCE(dimensions, ''),
	COALESCE(judgement, ''),
	COALESCE(rationale, ''),
	emitter_role,
	COALESCE(emitter_agent_id, ''),
	COALESCE(emitter_run_id, ''),
	created_at
FROM evaluations`

func (s *Store) scanEvaluation(row *sql.Row) (store.EvaluationRow, error) {
	var (
		out                   store.EvaluationRow
		dimensions, judgement string
		createdAt             int64
	)
	if err := row.Scan(
		&out.EvalID, &out.RunID, &out.DefID, &out.Score,
		&dimensions, &judgement, &out.Rationale,
		&out.EmitterRole, &out.EmitterAgentID, &out.EmitterRunID,
		&createdAt,
	); err != nil {
		return store.EvaluationRow{}, err
	}
	if dimensions != "" {
		_ = json.Unmarshal([]byte(dimensions), &out.Dimensions)
	}
	if judgement != "" {
		out.Judgement = json.RawMessage(judgement)
	}
	out.CreatedAt = time.Unix(0, createdAt)
	return out, nil
}

func (s *Store) scanEvaluationRows(rows *sql.Rows) ([]store.EvaluationRow, error) {
	var out []store.EvaluationRow
	for rows.Next() {
		var (
			r                     store.EvaluationRow
			dimensions, judgement string
			createdAt             int64
		)
		if err := rows.Scan(
			&r.EvalID, &r.RunID, &r.DefID, &r.Score,
			&dimensions, &judgement, &r.Rationale,
			&r.EmitterRole, &r.EmitterAgentID, &r.EmitterRunID,
			&createdAt,
		); err != nil {
			return nil, err
		}
		if dimensions != "" {
			_ = json.Unmarshal([]byte(dimensions), &r.Dimensions)
		}
		if judgement != "" {
			r.Judgement = json.RawMessage(judgement)
		}
		r.CreatedAt = time.Unix(0, createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// computeAggregate is the pure-Go aggregation kernel — shared by
// sqlite + postgres adapters. Given a flat list of evaluations,
// produce summary stats. Empty input → zero-valued AggregateResult
// with Count=0 (well-defined "no evaluations yet").
func computeAggregate(defID string, evals []store.EvaluationRow, lineageIncluded bool) store.AggregateResult {
	out := store.AggregateResult{
		DefID:           defID,
		Count:           len(evals),
		LineageIncluded: lineageIncluded,
	}
	if len(evals) == 0 {
		return out
	}

	scores := make([]float64, len(evals))
	dimAcc := map[string][]float64{}
	roleAcc := map[string][]float64{}
	for i, e := range evals {
		scores[i] = e.Score
		for k, v := range e.Dimensions {
			dimAcc[k] = append(dimAcc[k], v)
		}
		roleAcc[e.EmitterRole] = append(roleAcc[e.EmitterRole], e.Score)
	}
	out.Score = statsOf(scores)
	out.Score.Latest = evals[len(evals)-1].Score // evals is ASC by created_at

	if len(dimAcc) > 0 {
		out.Dimensions = make(map[string]store.ScoreStats, len(dimAcc))
		for k, v := range dimAcc {
			out.Dimensions[k] = statsOf(v)
		}
	}
	if len(roleAcc) > 0 {
		out.ByEmitterRole = make(map[string]store.ScoreStats, len(roleAcc))
		for k, v := range roleAcc {
			out.ByEmitterRole[k] = statsOf(v)
		}
	}
	return out
}

// statsOf computes Mean/Median/Min/Max/Count for a non-empty slice.
// Latest is set here as vals[len-1]: callers MUST append in
// created_at ASC order so the last element is the newest. For the
// top-level Score axis the caller currently overwrites Latest after
// returning (the input slice for that axis is built differently); for
// the Dimensions and ByEmitterRole axes the value set here stands.
func statsOf(vals []float64) store.ScoreStats {
	if len(vals) == 0 {
		return store.ScoreStats{}
	}
	out := store.ScoreStats{Count: len(vals), Min: vals[0], Max: vals[0]}
	sum := 0.0
	for _, v := range vals {
		sum += v
		if v < out.Min {
			out.Min = v
		}
		if v > out.Max {
			out.Max = v
		}
	}
	out.Mean = sum / float64(len(vals))
	out.Latest = vals[len(vals)-1] // overwritten by caller for the top-level Score; ok for dim/role

	// Median — sort a copy to avoid mutating caller's slice.
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		out.Median = sorted[mid]
	} else {
		out.Median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLikePrefix escapes the LIKE wildcards in `prefix` so an agent
// passing "events_2026" doesn't get treated as "events" + any-char +
// "2026". The ESCAPE clause in the LIKE statement uses backslash.
func escapeLikePrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return r.Replace(prefix)
}

// nilIfEmpty returns nil when s is empty so the SQL driver writes NULL
// rather than an empty string. Callers should prefer NULL for "no
// value" so that COUNT(column) and IS NULL queries behave correctly.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- v0.8.x Process-resource metrics sampler ----

// MetricsWriteSample inserts one process_samples row. Nullable
// system_* fields are stored as NULL when their pointer is nil.
func (s *Store) MetricsWriteSample(ctx context.Context, sample store.ProcessSample) error {
	var (
		sysCPU, sysMemUsed, sysMemAvail sql.NullInt64
	)
	if sample.SystemCPUPctX100 != nil {
		sysCPU = sql.NullInt64{Valid: true, Int64: int64(*sample.SystemCPUPctX100)}
	}
	if sample.SystemMemUsedMB != nil {
		sysMemUsed = sql.NullInt64{Valid: true, Int64: int64(*sample.SystemMemUsedMB)}
	}
	if sample.SystemMemAvailableMB != nil {
		sysMemAvail = sql.NullInt64{Valid: true, Int64: int64(*sample.SystemMemAvailableMB)}
	}
	var replicaID sql.NullString
	if sample.ReplicaID != "" {
		replicaID = sql.NullString{Valid: true, String: sample.ReplicaID}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO process_samples(
		sample_id, replica_id, sampled_at, active_runs, queued_runs,
		loomcycle_rss_bytes, loomcycle_heap_alloc_bytes, loomcycle_heap_inuse_bytes,
		loomcycle_num_goroutines, loomcycle_cpu_pct_x100,
		system_cpu_pct_x100, system_mem_used_mb, system_mem_available_mb
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sample.SampleID, replicaID, sample.SampledAt.UnixNano(), sample.ActiveRuns, sample.QueuedRuns,
		sample.LoomcycleRSSBytes, sample.LoomcycleHeapAlloc, sample.LoomcycleHeapInuse,
		sample.LoomcycleGoroutines, sample.LoomcycleCPUPctX100,
		sysCPU, sysMemUsed, sysMemAvail,
	)
	if err != nil {
		return fmt.Errorf("metrics: write sample: %w", err)
	}
	return nil
}

// MetricsSampleWindow returns samples in [since, until] ordered by
// sampled_at ASC then sample_id ASC. Cursor is the sample_id of the
// last row from the previous page (empty = from start). limit ≤ 0
// → 200, capped at 1000.
func (s *Store) MetricsSampleWindow(ctx context.Context, since, until time.Time, limit int, cursor string) ([]store.ProcessSample, string, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	// Use sampled_at as the primary order key; sample_id as
	// tie-breaker. Cursor is just the last seen sample_id since
	// MintSampleID encodes sampled_at in its prefix — strictly
	// monotonic per ns.
	args := []any{since.UnixNano(), until.UnixNano()}
	q := `SELECT sample_id, replica_id, sampled_at, active_runs, queued_runs,
	             loomcycle_rss_bytes, loomcycle_heap_alloc_bytes, loomcycle_heap_inuse_bytes,
	             loomcycle_num_goroutines, loomcycle_cpu_pct_x100,
	             system_cpu_pct_x100, system_mem_used_mb, system_mem_available_mb
	      FROM process_samples
	      WHERE sampled_at BETWEEN ? AND ?`
	if cursor != "" {
		q += ` AND sample_id > ?`
		args = append(args, cursor)
	}
	q += ` ORDER BY sampled_at ASC, sample_id ASC LIMIT ?`
	args = append(args, limit+1) // fetch one extra to detect next page
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("metrics: query window: %w", err)
	}
	defer rows.Close()
	out := make([]store.ProcessSample, 0, limit)
	for rows.Next() {
		var (
			rec                          store.ProcessSample
			replicaID                    sql.NullString
			sampledAtNs                  int64
			sysCPU, sysMemU, sysMemAvail sql.NullInt64
		)
		if err := rows.Scan(
			&rec.SampleID, &replicaID, &sampledAtNs, &rec.ActiveRuns, &rec.QueuedRuns,
			&rec.LoomcycleRSSBytes, &rec.LoomcycleHeapAlloc, &rec.LoomcycleHeapInuse,
			&rec.LoomcycleGoroutines, &rec.LoomcycleCPUPctX100,
			&sysCPU, &sysMemU, &sysMemAvail,
		); err != nil {
			return nil, "", fmt.Errorf("metrics: scan sample: %w", err)
		}
		if replicaID.Valid {
			rec.ReplicaID = replicaID.String
		}
		rec.SampledAt = time.Unix(0, sampledAtNs).UTC()
		if sysCPU.Valid {
			v := int(sysCPU.Int64)
			rec.SystemCPUPctX100 = &v
		}
		if sysMemU.Valid {
			v := int(sysMemU.Int64)
			rec.SystemMemUsedMB = &v
		}
		if sysMemAvail.Valid {
			v := int(sysMemAvail.Int64)
			rec.SystemMemAvailableMB = &v
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("metrics: iterate samples: %w", err)
	}
	nextCursor := ""
	if len(out) > limit {
		// We fetched limit+1; the (limit+1)-th row is the marker
		// for "more available." Truncate and return its predecessor
		// as the cursor.
		out = out[:limit]
		nextCursor = out[len(out)-1].SampleID
	}
	return out, nextCursor, nil
}

// MetricsRunSummary aggregates process_samples whose sampled_at
// overlaps the run's [started_at, COALESCE(completed_at, now)]
// window. Returns *ErrNotFound when the run row doesn't exist.
func (s *Store) MetricsRunSummary(ctx context.Context, runID string) (store.MetricsRunWindow, error) {
	var (
		startedAtNs   int64
		completedAtNs sql.NullInt64
	)
	row := s.db.QueryRowContext(ctx, `SELECT started_at, completed_at FROM runs WHERE id = ?`, runID)
	if err := row.Scan(&startedAtNs, &completedAtNs); err != nil {
		if err == sql.ErrNoRows {
			return store.MetricsRunWindow{}, &store.ErrNotFound{Kind: "run", ID: runID}
		}
		return store.MetricsRunWindow{}, fmt.Errorf("metrics: read run %s: %w", runID, err)
	}
	upper := time.Now().UnixNano()
	if completedAtNs.Valid {
		upper = completedAtNs.Int64
	}
	var (
		sampleCount   int
		peakRSS       sql.NullInt64
		meanRSS       sql.NullFloat64
		maxCPUPctX100 sql.NullInt64
	)
	row = s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		MAX(loomcycle_rss_bytes),
		AVG(loomcycle_rss_bytes),
		MAX(loomcycle_cpu_pct_x100)
	FROM process_samples
	WHERE sampled_at BETWEEN ? AND ?`, startedAtNs, upper)
	if err := row.Scan(&sampleCount, &peakRSS, &meanRSS, &maxCPUPctX100); err != nil {
		return store.MetricsRunWindow{}, fmt.Errorf("metrics: aggregate run %s: %w", runID, err)
	}
	out := store.MetricsRunWindow{
		RunID:       runID,
		StartedAt:   time.Unix(0, startedAtNs).UTC(),
		SampleCount: sampleCount,
	}
	if completedAtNs.Valid {
		out.CompletedAt = time.Unix(0, completedAtNs.Int64).UTC()
	}
	if peakRSS.Valid {
		out.PeakRSSBytes = peakRSS.Int64
	}
	if meanRSS.Valid {
		out.MeanRSSBytes = int64(meanRSS.Float64)
	}
	if maxCPUPctX100.Valid {
		out.MaxCPUPctX100 = int(maxCPUPctX100.Int64)
	}
	return out, nil
}

// MetricsSweep deletes samples with sampled_at < cutoff. Returns
// the count deleted.
func (s *Store) MetricsSweep(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM process_samples WHERE sampled_at < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("metrics: sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- v0.8.15 dynamic_agents (LoomCycle MCP runtime registration) ---

func (s *Store) DynamicAgentUpsert(ctx context.Context, a store.DynamicAgent) error {
	if a.Name == "" {
		return fmt.Errorf("dynamic_agents: name required")
	}
	if len(a.Definition) == 0 {
		return fmt.Errorf("dynamic_agents: definition required")
	}
	createdAt := a.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	var expiresAtNS int64
	if !a.ExpiresAt.IsZero() {
		expiresAtNS = a.ExpiresAt.UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dynamic_agents (name, definition, created_at, expires_at, description)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			definition  = excluded.definition,
			created_at  = excluded.created_at,
			expires_at  = excluded.expires_at,
			description = excluded.description
	`, a.Name, a.Definition, createdAt.UnixNano(), expiresAtNS, a.Description)
	if err != nil {
		return fmt.Errorf("dynamic_agents: upsert: %w", err)
	}
	return nil
}

func (s *Store) DynamicAgentGet(ctx context.Context, name string) (store.DynamicAgent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, definition, created_at, expires_at, COALESCE(description, '')
		FROM dynamic_agents
		WHERE name = ? AND (expires_at = 0 OR expires_at > ?)
	`, name, time.Now().UnixNano())

	var a store.DynamicAgent
	var createdAtNS, expiresAtNS int64
	if err := row.Scan(&a.Name, &a.Definition, &createdAtNS, &expiresAtNS, &a.Description); err != nil {
		if err == sql.ErrNoRows {
			return store.DynamicAgent{}, &store.ErrNotFound{Kind: "dynamic_agent", ID: name}
		}
		return store.DynamicAgent{}, fmt.Errorf("dynamic_agents: get: %w", err)
	}
	a.CreatedAt = time.Unix(0, createdAtNS)
	if expiresAtNS > 0 {
		a.ExpiresAt = time.Unix(0, expiresAtNS)
	}
	return a, nil
}

func (s *Store) DynamicAgentList(ctx context.Context) ([]store.DynamicAgent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, definition, created_at, expires_at, COALESCE(description, '')
		FROM dynamic_agents
		WHERE expires_at = 0 OR expires_at > ?
		ORDER BY created_at DESC
		LIMIT 200
	`, time.Now().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("dynamic_agents: list: %w", err)
	}
	defer rows.Close()

	out := []store.DynamicAgent{}
	for rows.Next() {
		var a store.DynamicAgent
		var createdAtNS, expiresAtNS int64
		if err := rows.Scan(&a.Name, &a.Definition, &createdAtNS, &expiresAtNS, &a.Description); err != nil {
			return nil, fmt.Errorf("dynamic_agents: list scan: %w", err)
		}
		a.CreatedAt = time.Unix(0, createdAtNS)
		if expiresAtNS > 0 {
			a.ExpiresAt = time.Unix(0, expiresAtNS)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DynamicAgentDelete(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dynamic_agents WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("dynamic_agents: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) DynamicAgentSweep(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM dynamic_agents
		WHERE expires_at > 0 AND expires_at < ?
	`, time.Now().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("dynamic_agents: sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---- Interruption (v0.8.16) ----------------------------------------

// nanosOrNull returns NULL when t is zero, unix-nanos otherwise. The
// zero-time → NULL contract is documented on InterruptRow.ExpiresAt.
func nanosOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

// nullableNanos scans an INTEGER-or-NULL column into a time. NULL
// becomes time.Time{} (zero value).
func nullableNanos(ns sql.NullInt64) time.Time {
	if !ns.Valid || ns.Int64 == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns.Int64)
}

// nullableString scans a TEXT-or-NULL column into a string. NULL
// becomes "" — matches how the upper layer treats absent values.
func nullableString(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// nullableRawJSON scans a TEXT-or-NULL column holding JSON. NULL
// returns nil, not the JSON null literal; callers distinguish via
// `len(...) == 0`.
func nullableRawJSON(ns sql.NullString) json.RawMessage {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	return json.RawMessage(ns.String)
}

func (s *Store) InterruptCreate(ctx context.Context, r store.InterruptRow) (string, error) {
	if r.InterruptID == "" {
		return "", fmt.Errorf("interrupts: interrupt_id required")
	}
	if r.RunID == "" {
		return "", fmt.Errorf("interrupts: run_id required")
	}
	if r.Kind == "" {
		r.Kind = store.InterruptKindQuestion
	}
	if r.Status == "" {
		r.Status = store.InterruptStatusPending
	}
	if r.Priority == "" {
		r.Priority = store.InterruptPriorityNormal
	}
	createdAt := r.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	// Question/options/context_data are NULLed when empty so the
	// column distinguishes "not set" from "empty string". Same
	// pattern as channel_messages payload.
	var question, contextData, answer, resolvedBy any
	if r.Question != "" {
		question = r.Question
	}
	if r.ContextData != "" {
		contextData = r.ContextData
	}
	if r.Answer != "" {
		answer = r.Answer
	}
	if r.ResolvedBy != "" {
		resolvedBy = r.ResolvedBy
	}
	var options, answerMeta any
	if len(r.Options) > 0 {
		options = string(r.Options)
	}
	if len(r.AnswerMeta) > 0 {
		answerMeta = string(r.AnswerMeta)
	}
	var userID, agentID, agentName any
	if r.UserID != "" {
		userID = r.UserID
	}
	if r.AgentID != "" {
		agentID = r.AgentID
	}
	if r.AgentName != "" {
		agentName = r.AgentName
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO interrupts (
			interrupt_id, run_id, kind, status,
			question, options, context_data, priority,
			answer, answer_meta,
			created_at, expires_at, resolved_at, resolved_by,
			user_id, agent_id, agent_name
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)
	`,
		r.InterruptID, r.RunID, r.Kind, r.Status,
		question, options, contextData, r.Priority,
		answer, answerMeta,
		createdAt.UnixNano(), nanosOrNull(r.ExpiresAt), resolvedBy,
		userID, agentID, agentName,
	)
	if err != nil {
		return "", fmt.Errorf("interrupts: create: %w", err)
	}
	return r.InterruptID, nil
}

func (s *Store) InterruptGet(ctx context.Context, interruptID string) (store.InterruptRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT interrupt_id, run_id, kind, status,
		       question, options, context_data, priority,
		       answer, answer_meta,
		       created_at, expires_at, resolved_at, resolved_by,
		       user_id, agent_id, agent_name
		FROM interrupts
		WHERE interrupt_id = ?
	`, interruptID)
	r, err := scanInterruptRow(row)
	if err == sql.ErrNoRows {
		return store.InterruptRow{}, &store.ErrNotFound{Kind: "interrupt", ID: interruptID}
	}
	if err != nil {
		return store.InterruptRow{}, fmt.Errorf("interrupts: get: %w", err)
	}
	return r, nil
}

// rowScanner abstracts *sql.Row / *sql.Rows so scanInterruptRow works
// for both Get (single row) and List (loop). Same trick used by other
// adapter helpers.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInterruptRow(row rowScanner) (store.InterruptRow, error) {
	var r store.InterruptRow
	var question, contextData, answer, resolvedBy, userID, agentID, agentName sql.NullString
	var options, answerMeta sql.NullString
	var createdAtNS int64
	var expiresAtNS, resolvedAtNS sql.NullInt64
	if err := row.Scan(
		&r.InterruptID, &r.RunID, &r.Kind, &r.Status,
		&question, &options, &contextData, &r.Priority,
		&answer, &answerMeta,
		&createdAtNS, &expiresAtNS, &resolvedAtNS, &resolvedBy,
		&userID, &agentID, &agentName,
	); err != nil {
		return store.InterruptRow{}, err
	}
	r.Question = nullableString(question)
	r.ContextData = nullableString(contextData)
	r.Answer = nullableString(answer)
	r.ResolvedBy = nullableString(resolvedBy)
	r.UserID = nullableString(userID)
	r.AgentID = nullableString(agentID)
	r.AgentName = nullableString(agentName)
	r.Options = nullableRawJSON(options)
	r.AnswerMeta = nullableRawJSON(answerMeta)
	r.CreatedAt = time.Unix(0, createdAtNS)
	r.ExpiresAt = nullableNanos(expiresAtNS)
	r.ResolvedAt = nullableNanos(resolvedAtNS)
	return r, nil
}

func (s *Store) InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error {
	var meta any
	if len(answerMeta) > 0 {
		meta = string(answerMeta)
	}
	// Gated by status='pending' so the resolve loses cleanly when
	// another resolver / sweeper has already finalised the row.
	// RowsAffected==0 distinguishes "row missing" (still 0 affected)
	// from "row already terminal" (also 0 affected) — we resolve the
	// ambiguity with a follow-up SELECT.
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `
		UPDATE interrupts
		SET status      = ?,
		    answer      = ?,
		    answer_meta = ?,
		    resolved_at = ?,
		    resolved_by = ?
		WHERE interrupt_id = ?
		  AND status = ?
		  AND (expires_at IS NULL OR expires_at > ?)
	`,
		store.InterruptStatusResolved,
		answer, meta,
		now.UnixNano(), resolvedBy,
		interruptID, store.InterruptStatusPending, now.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("interrupts: resolve: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Disambiguate three failure modes:
		//   - missing row → ErrNotFound
		//   - already-terminal row → ErrInterruptAlreadyTerminal
		//   - still-pending row whose expires_at < now → also
		//     ErrInterruptAlreadyTerminal (the row is on track to
		//     timed_out via the sweeper; treat the same as "already
		//     terminal" from the caller's POV).
		var existing string
		var expiresAtNS sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT status, expires_at FROM interrupts WHERE interrupt_id = ?`, interruptID).Scan(&existing, &expiresAtNS)
		if err == sql.ErrNoRows {
			return &store.ErrNotFound{Kind: "interrupt", ID: interruptID}
		}
		if err != nil {
			return fmt.Errorf("interrupts: resolve probe: %w", err)
		}
		_ = existing
		_ = expiresAtNS
		return store.ErrInterruptAlreadyTerminal
	}
	return nil
}

func (s *Store) InterruptFinish(ctx context.Context, interruptID, status, resolvedBy string) error {
	switch status {
	case store.InterruptStatusTimedOut, store.InterruptStatusCancelled:
		// ok
	default:
		return fmt.Errorf("interrupts: finish: invalid terminal status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE interrupts
		SET status      = ?,
		    resolved_at = ?,
		    resolved_by = ?
		WHERE interrupt_id = ? AND status = ?
	`,
		status,
		time.Now().UnixNano(), resolvedBy,
		interruptID, store.InterruptStatusPending,
	)
	if err != nil {
		return fmt.Errorf("interrupts: finish: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var existing string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM interrupts WHERE interrupt_id = ?`, interruptID).Scan(&existing)
		if err == sql.ErrNoRows {
			return &store.ErrNotFound{Kind: "interrupt", ID: interruptID}
		}
		if err != nil {
			return fmt.Errorf("interrupts: finish probe: %w", err)
		}
		return store.ErrInterruptAlreadyTerminal
	}
	return nil
}

func (s *Store) InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error) {
	return s.interruptList(ctx, "run_id", runID, statusFilter)
}

func (s *Store) InterruptListByUser(ctx context.Context, userID, statusFilter string) ([]store.InterruptRow, error) {
	return s.interruptList(ctx, "user_id", userID, statusFilter)
}

// interruptList is the shared SELECT body for ListByRun / ListByUser.
// `col` is the indexed filter column; we never embed user input here,
// so a static switch keeps the query parameter-binding safe.
func (s *Store) interruptList(ctx context.Context, col, val, statusFilter string) ([]store.InterruptRow, error) {
	if col != "run_id" && col != "user_id" {
		return nil, fmt.Errorf("interrupts: list: unknown filter column %q", col)
	}
	q := `
		SELECT interrupt_id, run_id, kind, status,
		       question, options, context_data, priority,
		       answer, answer_meta,
		       created_at, expires_at, resolved_at, resolved_by,
		       user_id, agent_id, agent_name
		FROM interrupts
		WHERE ` + col + ` = ?`
	args := []any{val}
	if statusFilter != "" {
		q += ` AND status = ?`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("interrupts: list: %w", err)
	}
	defer rows.Close()

	out := []store.InterruptRow{}
	for rows.Next() {
		r, err := scanInterruptRow(rows)
		if err != nil {
			return nil, fmt.Errorf("interrupts: list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InterruptCountPendingByRun(ctx context.Context, runID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM interrupts WHERE run_id = ? AND status = ?
	`, runID, store.InterruptStatusPending).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("interrupts: count pending: %w", err)
	}
	return n, nil
}

func (s *Store) InterruptSweepExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE interrupts
		SET status      = ?,
		    resolved_at = ?,
		    resolved_by = ?
		WHERE status = ? AND expires_at IS NOT NULL AND expires_at < ?
	`,
		store.InterruptStatusTimedOut,
		time.Now().UnixNano(), store.InterruptResolvedByTimeout,
		store.InterruptStatusPending, time.Now().UnixNano(),
	)
	if err != nil {
		return 0, fmt.Errorf("interrupts: sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Close closes the underlying *sql.DB. Idempotent.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.db.Close() })
	return err
}

// newID generates a short opaque ID with a prefix. 16 hex chars = 64 bits of
// entropy — plenty for v0.3 single-tenant scale; can swap for ULID/UUID later.
func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
