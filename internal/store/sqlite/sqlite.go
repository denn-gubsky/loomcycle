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
		// RFC AV — per-call token-usage + cost ledger (one row per LLM call).
		// Append-only; the runs row carries the per-run summary. No secrets:
		// token counts, provider/model, the owning credential scope id (already
		// non-secret, like user_id), and the computed/reported cost. cost REAL
		// nullable ⇒ unpriced (unknown model) distinct from a zero cost.
		`CREATE TABLE IF NOT EXISTS token_usage (
			id                    INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id                TEXT NOT NULL,
			session_id            TEXT,
			tenant_id             TEXT NOT NULL DEFAULT '',
			user_id               TEXT,
			agent_id              TEXT,
			parent_run_id         TEXT,
			iteration             INTEGER NOT NULL DEFAULT 0,
			provider              TEXT NOT NULL,
			model                 TEXT NOT NULL,
			credential_source     TEXT NOT NULL,
			credential_scope_id   TEXT NOT NULL DEFAULT '',
			input_tokens          INTEGER NOT NULL DEFAULT 0,
			output_tokens         INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
			cost                  REAL,
			cost_currency         TEXT,
			ts                    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS token_usage_by_run ON token_usage(run_id)`,
		`CREATE INDEX IF NOT EXISTS token_usage_tenant_ts ON token_usage(tenant_id, ts)`,
		`CREATE INDEX IF NOT EXISTS token_usage_source_ts ON token_usage(credential_source, ts)`,
		// RFC AV Phase 2b — the compact usage rollup. The sweeper folds
		// token_usage rows older than the detail-retention window into one row per
		// (period × dimension tuple) then deletes the raw rows. period_start is the
		// day-truncated ts (unix-nano). PK = the full dimension tuple so a re-run
		// folds idempotently. Reports UNION this with recent token_usage.
		`CREATE TABLE IF NOT EXISTS usage_archive (
			period_start          INTEGER NOT NULL,
			tenant_id             TEXT NOT NULL DEFAULT '',
			user_id               TEXT NOT NULL DEFAULT '',
			provider              TEXT NOT NULL,
			model                 TEXT NOT NULL,
			credential_source     TEXT NOT NULL,
			input_tokens          INTEGER NOT NULL DEFAULT 0,
			output_tokens         INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
			cost                  REAL NOT NULL DEFAULT 0,
			cost_currency         TEXT NOT NULL DEFAULT '',
			call_count            INTEGER NOT NULL DEFAULT 0,
			unpriced_calls        INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source)
		)`,
		`CREATE INDEX IF NOT EXISTS usage_archive_tenant_period ON usage_archive(tenant_id, period_start)`,
		// RFC AW — per-scope token budgets (soft + hard). A row's absence =
		// unlimited; a NULL soft/hard = that tier unset. No secrets: scope ids
		// (tenant / subject, already non-secret) + integer token amounts.
		// updated_at is unix-nano. Mirrors postgres migration 0054.
		`CREATE TABLE IF NOT EXISTS token_limits (
			tenant_id   TEXT NOT NULL DEFAULT '',
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL DEFAULT '',
			soft_limit  INTEGER,
			hard_limit  INTEGER,
			updated_at  INTEGER NOT NULL,
			updated_by  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, scope, scope_id)
		)`,
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
		// RFC BE — per-session embedding index backing History op=related
		// (semantic "related chats"). One row per chat; the vector is a plain
		// TEXT column (store.EncodeVector) ranked in Go, so NO sqlite-vec is
		// needed — this works on the default pure-Go build, unlike
		// memory_embeddings. tenant_id/user_id/agent are denormalised from the
		// session so the similarity search folds owner/tenant WITHOUT a join
		// (they are immutable session facts, set once at CreateSession).
		// FK CASCADE drops the row when the session is deleted; updated_at is
		// unix-nano. Mirrors postgres migration 0058.
		`CREATE TABLE IF NOT EXISTS session_embeddings (
			session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			tenant_id   TEXT NOT NULL DEFAULT '',
			user_id     TEXT,
			agent       TEXT NOT NULL DEFAULT '',
			provider    TEXT NOT NULL,
			model       TEXT NOT NULL,
			dimension   INTEGER NOT NULL,
			vector      TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		// The similarity search folds on (tenant_id, user_id, agent) then scans
		// the most-recent candidates; this covers that owner prefix.
		`CREATE INDEX IF NOT EXISTS session_embeddings_by_owner ON session_embeddings(tenant_id, user_id, agent)`,
		// v0.8 Memory tool. RFC BL: tenant-scoped + provenance/access
		// columns. On a FRESH DB the PRIMARY KEY (tenant_id, scope,
		// scope_id, key) isolates same-named agents across tenants; the
		// partial expires_at index keeps the sweeper's DELETE cheap (no
		// full-table scan). On an UPGRADED DB the addColumns ALTER adds
		// tenant_id + the RFC BL columns but SQLite cannot rewrite the PK
		// in place, so the PK stays (scope, scope_id, key) — byte-equivalent
		// for single-tenant (everything tenant_id=''), but multi-tenant
		// memory isolation requires Postgres or a FRESH sqlite DB (mirrors
		// the agent_def_active precedent above). The runtime upserts'
		// ON CONFLICT(tenant_id, scope, scope_id, key) target the
		// uniq_memory_tenant_scope_scope_id_key index in addIndexes below,
		// which exists on both fresh and upgraded DBs.
		`CREATE TABLE IF NOT EXISTS memory (
			scope             TEXT NOT NULL,
			scope_id          TEXT NOT NULL,
			key               TEXT NOT NULL,
			value             TEXT NOT NULL,
			expires_at        INTEGER,
			created_at        INTEGER NOT NULL,
			updated_at        INTEGER NOT NULL,
			tenant_id         TEXT NOT NULL DEFAULT '',
			origin            TEXT,
			class             TEXT,
			source_session_id TEXT,
			source_run_id     TEXT,
			access_count      INTEGER NOT NULL DEFAULT 0,
			last_accessed_at  INTEGER,
			superseded_at     INTEGER,
			PRIMARY KEY (tenant_id, scope, scope_id, key)
		)`,
		`CREATE INDEX IF NOT EXISTS memory_by_expires_at ON memory(expires_at) WHERE expires_at IS NOT NULL`,
		// RFC BL P2 — the durable consolidation substrate. Mirrors Postgres
		// migration 0061. memory_pending is the enqueue queue an Add writes to
		// and the consolidator drains (drained_at = soft-drain marker for
		// idempotent drain + TTL sweeping); memory_cursors is the per-target
		// watermark + lease (composite watermark = (watermark_completed_at,
		// watermark_session_id)). Timestamps are unix-nano like the rest of the
		// sqlite schema; payload is TEXT-encoded JSON (no native JSONB).
		`CREATE TABLE IF NOT EXISTS memory_pending (
			id                TEXT    PRIMARY KEY,
			tenant_id         TEXT    NOT NULL DEFAULT '',
			scope             TEXT    NOT NULL,
			scope_id          TEXT    NOT NULL,
			payload           TEXT    NOT NULL,
			source_session_id TEXT,
			source_run_id     TEXT,
			created_at        INTEGER NOT NULL,
			drained_at        INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS memory_pending_by_target ON memory_pending(tenant_id, scope, scope_id, drained_at)`,
		`CREATE TABLE IF NOT EXISTS memory_cursors (
			tenant_id              TEXT    NOT NULL DEFAULT '',
			scope                  TEXT    NOT NULL,
			scope_id               TEXT    NOT NULL,
			watermark_completed_at INTEGER,
			watermark_session_id   TEXT    NOT NULL DEFAULT '',
			leased_by              TEXT    NOT NULL DEFAULT '',
			lease_expires_at       INTEGER,
			updated_at             INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, scope, scope_id)
		)`,
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
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_name   ON agent_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_parent ON agent_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_run    ON agent_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// agent_defs_by_content_sha256 is created in addIndexes below
		// (runs AFTER addColumns adds the column on upgrade-from-v0.8.x
		// DBs where this CREATE TABLE IF NOT EXISTS is a no-op).
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// — two tenants own the same name independently. On a FRESH DB this
		// CREATE applies the composite PK directly. On an UPGRADED v0.8.x
		// DB this CREATE is a no-op (table exists) and the tenant_id column
		// is added by the addColumns ALTER below; SQLite cannot rewrite a
		// PRIMARY KEY in place, so an upgraded DB keeps PK(name). That is
		// byte-equivalent for single-tenant (everything tenant_id=''); true
		// per-tenant isolation on SQLite requires a fresh DB (Postgres
		// upgrades in place via migration 0037). The contract tests run on
		// a fresh DB, so the isolation guarantee is verified here.
		`CREATE TABLE IF NOT EXISTS agent_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES agent_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		// v0.8.22 SkillDef substrate — mirror of agent_defs with
		// the same identity / lineage / promotion semantics. The
		// `definition` column carries the JSON-encoded skill body
		// + metadata (body / description / tools).
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
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_name   ON skill_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_parent ON skill_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_run    ON skill_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// skill_defs_by_content_sha256 — see agent_defs_by_content_sha256 note above.
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// — two tenants own the same name independently. On a FRESH DB this
		// CREATE applies the composite PK; on an UPGRADED v0.8.x DB the
		// tenant_id column is added by the addColumns ALTER below and the PK
		// stays (name) (SQLite can't rewrite a PK in place — byte-equivalent
		// for single-tenant tenant_id=''). See the agent_def_active note for
		// the full SQLite upgrade caveat.
		`CREATE TABLE IF NOT EXISTS skill_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES skill_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		// TeamDef substrate — mirror of skill_defs with the same
		// identity / lineage / promotion semantics. The `definition`
		// column carries an opaque JSON workflow-graph blob (the store
		// stays content-agnostic).
		`CREATE TABLE IF NOT EXISTS teamdefs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES teamdefs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			content_sha256            TEXT,
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_name   ON teamdefs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_parent ON teamdefs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_run    ON teamdefs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// teamdefs_by_content_sha256 — see agent_defs_by_content_sha256 note above.
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// — two tenants own the same name independently. On a FRESH DB this
		// CREATE applies the composite PK. See the skill_def_active note for
		// the full SQLite upgrade caveat (teamdefs is a fresh table, so no
		// upgrade path applies, but the pattern is identical).
		`CREATE TABLE IF NOT EXISTS teamdef_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES teamdefs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
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
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_name   ON mcp_server_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_parent ON mcp_server_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_run    ON mcp_server_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// mcp_server_defs_by_content_sha256 — see agent_defs_by_content_sha256 note above.
		// RFC N: tenant-scoped active pointer, PRIMARY KEY(tenant_id, name)
		// — two tenants own the same name independently. Fresh-DB-only PK
		// shape; an upgraded v0.9.x DB keeps PK(name) (SQLite can't rewrite
		// a PK in place) which is byte-equivalent for single-tenant. See the
		// agent_def_active note earlier for the full upgrade caveat.
		`CREATE TABLE IF NOT EXISTS mcp_server_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES mcp_server_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
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
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_defs_by_name   ON schedule_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS schedule_defs_by_parent ON schedule_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS schedule_defs_by_run    ON schedule_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// on a FRESH DB; an UPGRADED DB keeps PK(name) and gets the
		// (tenant_id, name) UNIQUE INDEX in addIndexes as the ON CONFLICT
		// target. See the agent_def_active note for the SQLite upgrade caveat.
		`CREATE TABLE IF NOT EXISTS schedule_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES schedule_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
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
			paused_until    INTEGER,
			fire_count      INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_run_state_due ON schedule_run_state(next_run_at)`,
		// v1.x RFC G A2A substrate — two content-addressed Defs mirroring
		// schedule_defs exactly (identity + lineage + promotion), minus
		// the sweeper-only run_state table. a2a_server_card_defs declares
		// which agents are exposed via A2A + AgentCard metadata;
		// a2a_agent_defs declares remote A2A peers callable as tools. See
		// internal/store/postgres/migrations/0031_a2a_defs.up.sql for the
		// full design rationale.
		`CREATE TABLE IF NOT EXISTS a2a_server_card_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES a2a_server_card_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS a2a_server_card_defs_by_name   ON a2a_server_card_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS a2a_server_card_defs_by_parent ON a2a_server_card_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS a2a_server_card_defs_by_run    ON a2a_server_card_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// on a FRESH DB; an UPGRADED DB keeps PK(name) and gets the
		// (tenant_id, name) UNIQUE INDEX in addIndexes as the ON CONFLICT
		// target. See the agent_def_active note for the SQLite upgrade caveat.
		`CREATE TABLE IF NOT EXISTS a2a_server_card_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES a2a_server_card_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS a2a_agent_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES a2a_agent_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS a2a_agent_defs_by_name   ON a2a_agent_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS a2a_agent_defs_by_parent ON a2a_agent_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS a2a_agent_defs_by_run    ON a2a_agent_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// on a FRESH DB; an UPGRADED DB keeps PK(name) and gets the
		// (tenant_id, name) UNIQUE INDEX in addIndexes as the ON CONFLICT
		// target. See the agent_def_active note for the SQLite upgrade caveat.
		`CREATE TABLE IF NOT EXISTS a2a_agent_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES a2a_agent_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		// v1.x RFC H Input Webhooks substrate — a single content-addressed
		// Def mirroring a2a_agent_defs exactly (identity + lineage +
		// promotion), minus the sweeper-only run_state table. webhook_defs
		// declares inbound HTTP webhook endpoints (auth, rate limit,
		// delivery target, payload mapping). See
		// internal/store/postgres/migrations/0032_webhook_defs.up.sql for
		// the full design rationale.
		`CREATE TABLE IF NOT EXISTS webhook_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES webhook_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS webhook_defs_by_name   ON webhook_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS webhook_defs_by_parent ON webhook_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS webhook_defs_by_run    ON webhook_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// on a FRESH DB; an UPGRADED DB keeps PK(name) and gets the
		// (tenant_id, name) UNIQUE INDEX in addIndexes as the ON CONFLICT
		// target. See the agent_def_active note for the SQLite upgrade caveat.
		`CREATE TABLE IF NOT EXISTS webhook_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES webhook_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		// RFC I MR-3a MemoryBackendDef substrate — a faithful mirror of
		// webhook_defs (identity + lineage + promotion), minus the
		// sweeper-only run_state table. memory_backend_defs declares a
		// named memory backend (kind, connection config, tenancy
		// strategy, fallback). See
		// internal/store/postgres/migrations/0034_memory_backend_defs.up.sql
		// for the full design rationale.
		`CREATE TABLE IF NOT EXISTS memory_backend_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES memory_backend_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS memory_backend_defs_by_name   ON memory_backend_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS memory_backend_defs_by_parent ON memory_backend_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS memory_backend_defs_by_run    ON memory_backend_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// on a FRESH DB; an UPGRADED DB keeps PK(name) (SQLite can't rewrite a
		// PK in place) and gets the (tenant_id, name) UNIQUE INDEX in
		// addIndexes below as the ON CONFLICT target. See the agent_def_active
		// note above for the full SQLite upgrade caveat.
		`CREATE TABLE IF NOT EXISTS memory_backend_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES memory_backend_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		// RFC AH Phase 2a — persistent dynamic volumes. FLAT (tenant_id,
		// name) table, NOT the versioned Def shape: a Volume points at
		// mutable on-disk state outside the def, so there is no version,
		// parent_def_id, content_sha256, or active pointer. definition holds
		// the runtime-derived {"path":..,"mode":..}; the path is always
		// <dynamic_root>/<tenant-segment>/<name> derived by the tool.
		`CREATE TABLE IF NOT EXISTS volume_defs (
			tenant_id   TEXT NOT NULL DEFAULT '',
			name        TEXT NOT NULL,
			definition  TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, name)
		)`,
		// RFC AR CredentialDef — the secure per-tenant credential store. Flat
		// (tenant_id, scope, scope_id, name) key; definition holds ONLY sealed
		// ciphertext (inline backend, see internal/credential) or an external
		// pointer — never a plaintext secret. scope/scope_id isolate user A's
		// token from user B's. Excluded from snapshots.
		`CREATE TABLE IF NOT EXISTS credential_defs (
			tenant_id   TEXT NOT NULL DEFAULT '',
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL DEFAULT '',
			name        TEXT NOT NULL,
			backend     TEXT NOT NULL,
			definition  TEXT NOT NULL,
			expires_at  INTEGER,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, scope, scope_id, name)
		)`,
		// RFC AL Path primitive — the dirent (path tree) substrate. Maps a
		// (tenant_id, scope, scope_id, parent_path, name) coordinate to a
		// backing resource (kind + resource_ref json). PK is the full
		// coordinate so each (tenant, scope, scope_id) tree is independent and
		// a name is unique within its parent directory. The composite PK is
		// also the lookup index for resolve/ls; parent_path enables one-level
		// (=) and recursive (prefix) listings.
		`CREATE TABLE IF NOT EXISTS dirents (
			tenant_id    TEXT NOT NULL DEFAULT '',
			scope        TEXT NOT NULL,
			scope_id     TEXT NOT NULL DEFAULT '',
			parent_path  TEXT NOT NULL,
			name         TEXT NOT NULL,
			kind         TEXT NOT NULL,
			resource_ref TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, scope, scope_id, parent_path, name)
		)`,
		// RFC AH Phase 2b — ephemeral (run-tree-scoped) volumes. SEPARATE
		// from volume_defs: PK (root_run_id, name), NOT (tenant_id, name) —
		// two concurrent runs (any tenant) can each own a `work` volume with
		// no clobber. tenant_id is carried for the purge fence; definition is
		// the runtime-derived {"path":..,"mode":..} where path is always
		// <dynamic_root>/_ephemeral/<root_run_id>/<name>. The row backs the
		// inline run-completion purge + the crash-recovery sweeper; the
		// in-memory EphemeralVolumeSet is the resolution source.
		`CREATE TABLE IF NOT EXISTS ephemeral_volume_defs (
			root_run_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			tenant_id   TEXT NOT NULL DEFAULT '',
			definition  TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			PRIMARY KEY (root_run_id, name)
		)`,
		// RFC L OSS multi-tenant authorization — bearer tokens bound to
		// an authoritative principal (tenant_id + subject + allowed_scopes).
		// NOT versioned/forkable: no version, no active pointer, no parent.
		// Rotation via rotated_from; validity via retired_at (NULL or
		// future = valid). token_hash = SHA-256(pepper‖token); plaintext
		// never stored. See internal/store/postgres/migrations/
		// 0035_operator_token_defs.up.sql for the full rationale.
		`CREATE TABLE IF NOT EXISTS operator_token_defs (
			def_id               TEXT    PRIMARY KEY,
			name                 TEXT    NOT NULL,
			tenant_id            TEXT    NOT NULL,
			subject              TEXT    NOT NULL,
			token_hash           TEXT    NOT NULL,
			allowed_scopes       TEXT    NOT NULL,
			created_at           INTEGER NOT NULL,
			created_by_agent_id  TEXT,
			created_by_run_id    TEXT,
			rotated_from         TEXT    REFERENCES operator_token_defs(def_id),
			retired_at           INTEGER,
			UNIQUE(token_hash)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_operator_token_hash ON operator_token_defs(token_hash)`,
		`CREATE INDEX IF NOT EXISTS operator_token_defs_by_name ON operator_token_defs(name, created_at DESC)`,
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
		// RFC N: tenant-scoped — PRIMARY KEY(tenant_id, name). Fresh-DB-only
		// PK shape; see the agent_def_active note above for the SQLite
		// upgrade caveat.
		`CREATE TABLE IF NOT EXISTS dynamic_agents (
			name        TEXT    NOT NULL,
			definition  BLOB    NOT NULL,
			created_at  INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL DEFAULT 0,
			description TEXT,
			tenant_id   TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
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
		// RFC H Decision 10 "Layer 2" durable dedup — optional
		// idempotency_key. NULL on legacy rows + runs with no key. The
		// partial unique index is created below in addIndexes (so the
		// column added here is guaranteed present first). The webhook
		// spawn path sets this = delivery_id for cross-replica /
		// past-Layer-1-TTL dedup.
		`ALTER TABLE runs ADD COLUMN idempotency_key TEXT`,
		// RFC L / Web-UI multi-tenant authz — tenant_id denormalised onto
		// the run row so the per-tenant workspace lists filter without a
		// sessions JOIN. NULL on legacy rows until the backfill below
		// copies it from the parent session. New rows set it at CreateRun.
		`ALTER TABLE runs ADD COLUMN tenant_id TEXT`,
		// RFC N — tenant-scope the agent definition plane. On an upgraded
		// v0.8.x DB the CREATE TABLE statements above were no-ops, so these
		// ALTERs add the tenant_id column to the existing tables. The
		// PRIMARY KEY stays (name) on the upgraded table (SQLite can't
		// rewrite a PK in place) — functionally identical for single-tenant
		// (tenant_id=''); see the CREATE TABLE notes for the isolation
		// caveat. DEFAULT '' backfills existing rows to the shared tenant.
		`ALTER TABLE agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dynamic_agents ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N — tenant-scope the skill definition plane (mirror of the
		// agent ALTERs above). Same SQLite upgrade caveat: the PK stays
		// (name) on the upgraded skill_def_active table; functionally
		// identical for single-tenant (tenant_id=''). DEFAULT '' backfills
		// existing rows to the shared tenant.
		`ALTER TABLE skill_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE skill_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N — tenant-scope the MCP server definition plane (mirror of the
		// agent + skill ALTERs above). Same SQLite upgrade caveat: the PK
		// stays (name) on the upgraded mcp_server_def_active table;
		// functionally identical for single-tenant (tenant_id=''). DEFAULT ''
		// backfills existing rows to the shared tenant.
		`ALTER TABLE mcp_server_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mcp_server_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N (completion) — tenant-scope the remaining definition planes:
		// memory backend / schedule / A2A (server card + agent) / webhook
		// (mirror of the agent + skill + MCP ALTERs above). Same SQLite
		// upgrade caveat: the *_active PK stays (name) on the upgraded table;
		// functionally identical for single-tenant (tenant_id=''). DEFAULT ''
		// backfills existing rows to the shared tenant.
		`ALTER TABLE memory_backend_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_backend_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE schedule_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE schedule_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_server_card_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_server_card_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC S / F36: lifetime fire-count for max_fires self-retirement.
		// Idempotent ALTER for existing DBs; fresh DBs get it in CREATE TABLE.
		`ALTER TABLE schedule_run_state ADD COLUMN fire_count INTEGER NOT NULL DEFAULT 0`,
		// F42 / RFC X Phase 2: persist whether a run is an interactive
		// (persistent, parks-at-end_turn) session so a snapshotted+restored
		// paused run can be re-dispatched with the correct park-vs-complete
		// semantics. NULL/0 on legacy rows + batch runs; CreateRun stamps it.
		`ALTER TABLE runs ADD COLUMN interactive INTEGER NOT NULL DEFAULT 0`,
		// RFC AV — per-run cost + credential-source summary. cost REAL nullable
		// (NULL ⇒ unpriced, distinct from a genuine zero); credential_source is
		// "operator"|"tenant"|"user" for the run's primary key source. Idempotent
		// on fresh deploys (declared in CREATE TABLE runs above once upgraded).
		`ALTER TABLE runs ADD COLUMN cost REAL`,
		`ALTER TABLE runs ADD COLUMN cost_currency TEXT`,
		`ALTER TABLE runs ADD COLUMN credential_source TEXT`,
		`ALTER TABLE runs ADD COLUMN credential_scope_id TEXT`,
		// RFC AX — the negative operator-key permission bit (0 = allowed). Stamped
		// at CreateRun from the principal + gate; read back on resume/restore so a
		// re-dispatched run keeps its restriction. 0 on legacy rows (fail-open).
		`ALTER TABLE runs ADD COLUMN operator_key_restricted INTEGER NOT NULL DEFAULT 0`,
		// RFC BE — human/organizational chat metadata on the session row (the
		// History tool's browse/search/annotate surface). All additive + nullable
		// so legacy rows read the zero value. tags is a JSON array (NULL = never
		// set); archived_at / summary_updated_at are unix nanos (NULL = unset);
		// pinned is 0/1.
		`ALTER TABLE sessions ADD COLUMN title TEXT`,
		`ALTER TABLE sessions ADD COLUMN description TEXT`,
		`ALTER TABLE sessions ADD COLUMN tags TEXT`,
		`ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN archived_at INTEGER`,
		`ALTER TABLE sessions ADD COLUMN summary TEXT`,
		`ALTER TABLE sessions ADD COLUMN summary_updated_at INTEGER`,
		// RFC BL — tenant-scope the Memory store + provenance/access columns.
		// On an UPGRADED DB these ALTERs add the columns to the existing
		// memory table; the PK stays (scope, scope_id, key) (SQLite can't
		// rewrite it in place — see the CREATE TABLE note + the agent_def_active
		// caveat: multi-tenant memory isolation requires Postgres or a fresh
		// sqlite DB). DEFAULT '' backfills existing rows to the legacy tenant.
		`ALTER TABLE memory ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory ADD COLUMN origin TEXT`,
		`ALTER TABLE memory ADD COLUMN class TEXT`,
		`ALTER TABLE memory ADD COLUMN source_session_id TEXT`,
		`ALTER TABLE memory ADD COLUMN source_run_id TEXT`,
		`ALTER TABLE memory ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memory ADD COLUMN last_accessed_at INTEGER`,
		// RFC BL P2 — the soft-archive marker. NULL on every legacy row (live);
		// the consolidator stamps it to hide a consolidated raw row from recall
		// while retaining it. memory_pending / memory_cursors are pure CREATE
		// TABLE (no ALTER needed) so they land via the schema block above.
		`ALTER TABLE memory ADD COLUMN superseded_at INTEGER`,
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

	// One-time backfill of runs.tenant_id from the parent session for
	// rows created before the column existed. Idempotent: only touches
	// NULL/empty rows, so it's a cheap no-op on every boot after the
	// first. (RFC L / Web-UI multi-tenant authz.)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE runs SET tenant_id = (
			SELECT s.tenant_id FROM sessions s WHERE s.id = runs.session_id
		) WHERE tenant_id IS NULL OR tenant_id = ''`); err != nil {
		return fmt.Errorf("migrate backfill runs.tenant_id: %w", err)
	}

	addIndexes := []string{
		// Drives the hot lookup paths for the cancel/get endpoints.
		// Partial indexes (WHERE ... IS NOT NULL) keep the index small —
		// the vast majority of historical rows have no agent_id.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_id        ON runs(agent_id)        WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_parent_agent_id ON runs(parent_agent_id) WHERE parent_agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_user_active     ON runs(user_id, status) WHERE user_id IS NOT NULL`,
		// RFC L / Web-UI multi-tenant authz — tenant-scoped workspace lists.
		`CREATE INDEX IF NOT EXISTS runs_by_tenant_active   ON runs(tenant_id, status) WHERE tenant_id IS NOT NULL`,
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
		`CREATE INDEX IF NOT EXISTS teamdefs_by_content_sha256        ON teamdefs(content_sha256)        WHERE content_sha256 IS NOT NULL`,
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
		// RFC BE — session-metadata listing. Partial indexes stay small: only
		// pinned rows / archived rows carry the flag. Live in addIndexes (not the
		// CREATE TABLE block) so the columns added above are guaranteed present.
		`CREATE INDEX IF NOT EXISTS sessions_by_pinned ON sessions(pinned) WHERE pinned = 1`,
		`CREATE INDEX IF NOT EXISTS sessions_by_archived_at ON sessions(archived_at) WHERE archived_at IS NOT NULL`,
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
		// RFC H Decision 10 "Layer 2" durable dedup — partial unique
		// index on idempotency_key. Lives in addIndexes (not the CREATE
		// TABLE block) so the column added by addColumns above is
		// guaranteed present on the v0.12.x → idempotency-key upgrade
		// path. Partial (WHERE ... IS NOT NULL) so keyless runs are
		// unconstrained and the index stays small. Mirrors the postgres
		// 0033 migration.
		`CREATE UNIQUE INDEX IF NOT EXISTS runs_idempotency_key ON runs(idempotency_key) WHERE idempotency_key IS NOT NULL`,
		// RFC N — ON CONFLICT(tenant_id, name) targets for the def plane.
		// On a FRESH DB these are redundant with the composite PRIMARY KEY /
		// UNIQUE declared in the CREATE TABLE block above (harmless). On an
		// UPGRADED v0.8.x DB the addColumns ALTER added tenant_id but SQLite
		// could NOT rewrite the existing PRIMARY KEY(name) / UNIQUE(name,
		// version) in place — so the runtime upserts' ON CONFLICT(tenant_id,
		// name) and the version-bump UNIQUE(tenant_id, name, version) have NO
		// matching index and SQLite refuses with "ON CONFLICT clause does not
		// match any PRIMARY KEY or UNIQUE constraint", breaking the FIRST
		// promote/register even single-tenant. These idempotent indexes
		// supply that ON CONFLICT target so upserts work on upgraded DBs.
		// Lives in addIndexes (not the CREATE TABLE block) so the tenant_id
		// column added by addColumns above is guaranteed present first.
		//
		// Residual caveat (unchanged): the upgraded agent_def_active /
		// dynamic_agents / skill_def_active / mcp_server_def_active tables
		// still carry PRIMARY KEY(name), so two tenants cannot share a name on
		// a PRE-EXISTING SQLite DB — a fresh DB is required for full
		// multi-tenant on SQLite. These indexes restore single-tenant upgrade
		// functionality only; they do not retrofit the per-tenant isolation a
		// fresh DB's composite PK provides. The skill + mcp planes have NO
		// dynamic_* tier (skills = static skills.Set + the
		// skill_defs/skill_def_active substrate; mcp = static cfg.MCPServers +
		// the mcp_server_defs/mcp_server_def_active substrate), so each needs
		// only two indexes.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_agent_def_active_tenant_name    ON agent_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_dynamic_agents_tenant_name      ON dynamic_agents(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_agent_defs_tenant_name_version  ON agent_defs(tenant_id, name, version)`,
		// Skill plane (RFC N FIX 5-skills) — same in-place-upgrade gap.
		// SkillDefSetActive does ON CONFLICT(tenant_id, name) on
		// skill_def_active and SkillDefCreate inserts against
		// UNIQUE(tenant_id, name, version) on skill_defs; on an upgraded DB
		// neither composite index exists, so the FIRST promote/register
		// fails even single-tenant. No dynamic_skills table to cover.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_skill_def_active_tenant_name    ON skill_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_skill_defs_tenant_name_version  ON skill_defs(tenant_id, name, version)`,
		// Team plane — same ON CONFLICT(tenant_id, name) / UNIQUE(tenant_id,
		// name, version) targets as the skill plane. teamdefs is a fresh
		// table so these are redundant with its CREATE TABLE composite
		// constraints, but kept for parity with the skill plane's pattern.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_teamdef_active_tenant_name     ON teamdef_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_teamdefs_tenant_name_version   ON teamdefs(tenant_id, name, version)`,
		// MCP plane (RFC N FIX 5-mcp) — same in-place-upgrade gap.
		// MCPServerDefSetActive does ON CONFLICT(tenant_id, name) on
		// mcp_server_def_active and MCPServerDefCreate's version-bump inserts
		// against UNIQUE(tenant_id, name, version) on mcp_server_defs; on an
		// upgraded DB neither composite index exists, so the FIRST
		// promote/register fails even single-tenant. No dynamic_mcp table.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_mcp_server_def_active_tenant_name   ON mcp_server_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_mcp_server_defs_tenant_name_version ON mcp_server_defs(tenant_id, name, version)`,
		// MemoryBackend plane (RFC N completion) — same in-place-upgrade gap.
		// MemoryBackendDefSetActive does ON CONFLICT(tenant_id, name) on
		// memory_backend_def_active and MemoryBackendDefCreate's version-bump
		// inserts against UNIQUE(tenant_id, name, version) on
		// memory_backend_defs; on an upgraded DB neither composite index
		// exists, so the FIRST promote/register fails even single-tenant. No
		// dynamic_* table to cover.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_backend_def_active_tenant_name   ON memory_backend_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_backend_defs_tenant_name_version ON memory_backend_defs(tenant_id, name, version)`,
		// A2A agent plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_agent_def_active_tenant_name   ON a2a_agent_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_agent_defs_tenant_name_version ON a2a_agent_defs(tenant_id, name, version)`,
		// Schedule plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_schedule_def_active_tenant_name   ON schedule_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_schedule_defs_tenant_name_version ON schedule_defs(tenant_id, name, version)`,
		// A2A server card plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_server_card_def_active_tenant_name   ON a2a_server_card_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_server_card_defs_tenant_name_version ON a2a_server_card_defs(tenant_id, name, version)`,
		// Webhook plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_webhook_def_active_tenant_name   ON webhook_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_webhook_defs_tenant_name_version ON webhook_defs(tenant_id, name, version)`,
		// RFC BL — the ON CONFLICT(tenant_id, scope, scope_id, key) target for
		// the Memory upserts (MemorySet / MemoryIncrement / MemoryAtomicUpdate).
		// On a FRESH DB it's redundant with the composite PRIMARY KEY; on an
		// UPGRADED DB the PK stays (scope, scope_id, key) (SQLite can't rewrite
		// it in place), so this index supplies the ON CONFLICT target — without
		// it the upserts fail "ON CONFLICT clause does not match ..." even
		// single-tenant. Mirrors the def-plane indexes above.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_tenant_scope_scope_id_key ON memory(tenant_id, scope, scope_id, key)`,
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
	var userID, title, description, tags, summary sql.NullString
	var pinned, archivedNs, summaryUpdNs sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, agent, created_at, user_id,
		        title, description, tags, pinned, archived_at, summary, summary_updated_at
		 FROM sessions WHERE id = ?`, sessionID,
	).Scan(&sess.ID, &sess.TenantID, &sess.Agent, &createdNs, &userID,
		&title, &description, &tags, &pinned, &archivedNs, &summary, &summaryUpdNs)
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
	if title.Valid {
		sess.Title = title.String
	}
	if description.Valid {
		sess.Description = description.String
	}
	if tags.Valid {
		decoded, derr := store.DecodeTags(tags.String)
		if derr != nil {
			return store.Session{}, fmt.Errorf("decode session tags: %w", derr)
		}
		sess.Tags = decoded
	}
	sess.Pinned = pinned.Valid && pinned.Int64 != 0
	if archivedNs.Valid {
		sess.ArchivedAt = time.Unix(0, archivedNs.Int64)
	}
	if summary.Valid {
		sess.Summary = summary.String
	}
	if summaryUpdNs.Valid {
		sess.SummaryUpdatedAt = time.Unix(0, summaryUpdNs.Int64)
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
		`INSERT INTO runs(id, session_id, status, started_at, agent_id, parent_agent_id, parent_run_id, user_id, tenant_id, user_tier, agent_def_id, model, parent_context, idempotency_key, interactive, operator_key_restricted)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, store.RunRunning, now.UnixNano(),
		nilIfEmpty(identity.AgentID),
		nilIfEmpty(identity.ParentAgentID),
		nilIfEmpty(identity.ParentRunID),
		nilIfEmpty(identity.UserID),
		nilIfEmpty(identity.TenantID),
		nilIfEmpty(identity.UserTier),
		nilIfEmpty(identity.AgentDefID),
		nilIfEmpty(identity.Model),
		pcVal,
		nilIfEmpty(identity.IdempotencyKey),
		boolToInt(identity.Interactive),
		boolToInt(identity.OperatorKeyRestricted),
	)
	if err != nil {
		// RFC H Decision 10: a collision on the runs_idempotency_key
		// partial unique index means an earlier run already claimed this
		// key — surface the typed sentinel so the caller dedups instead
		// of failing. Scope the classification to the idempotency case
		// (key != "" AND the violation names that index) so a future
		// UNIQUE constraint elsewhere on runs is never misreported.
		if identity.IdempotencyKey != "" &&
			strings.Contains(err.Error(), "UNIQUE constraint failed") &&
			strings.Contains(err.Error(), "runs.idempotency_key") {
			return store.Run{}, store.ErrDuplicateIdempotencyKey
		}
		return store.Run{}, err
	}
	return store.Run{
		ID:                    id,
		SessionID:             sessionID,
		Status:                store.RunRunning,
		StartedAt:             now,
		AgentID:               identity.AgentID,
		ParentAgentID:         identity.ParentAgentID,
		ParentRunID:           identity.ParentRunID,
		UserID:                identity.UserID,
		TenantID:              identity.TenantID,
		UserTier:              identity.UserTier,
		AgentDefID:            identity.AgentDefID,
		Model:                 identity.Model,
		ParentContext:         identity.ParentContext.Clone(),
		IdempotencyKey:        identity.IdempotencyKey,
		Interactive:           identity.Interactive,
		OperatorKeyRestricted: identity.OperatorKeyRestricted,
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
	// RFC AV: cost is stored NULL when the run was not priced (empty currency),
	// distinct from a genuine zero cost (which carries a currency).
	var costArg interface{}
	if usage.CostCurrency != "" {
		costArg = usage.Cost
	}
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
			error                 = ?,
			cost                  = ?,
			cost_currency         = ?,
			credential_source     = ?,
			credential_scope_id   = ?
		WHERE id = ? AND status = ?`,
		string(status), now, stopReason,
		usage.InputTokens, usage.OutputTokens,
		usage.CacheCreationTokens, usage.CacheReadTokens,
		usage.Model, nilIfEmpty(usage.Provider), errMsg,
		costArg, nilIfEmpty(usage.CostCurrency),
		nilIfEmpty(usage.CredentialSource), nilIfEmpty(usage.CredentialScopeID),
		runID, string(store.RunRunning),
	)
	return err
}

// RecordCallUsage appends one per-call usage row (RFC AV). Append-only insert;
// cost is stored NULL when unpriced (empty currency), distinct from zero.
func (s *Store) RecordCallUsage(ctx context.Context, row store.TokenUsageRow) error {
	ts := row.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	var costArg interface{}
	if row.CostCurrency != "" {
		costArg = row.Cost
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO token_usage (
			run_id, session_id, tenant_id, user_id, agent_id, parent_run_id,
			iteration, provider, model, credential_source, credential_scope_id,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cost, cost_currency, ts
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.RunID, nilIfEmpty(row.SessionID), row.TenantID, nilIfEmpty(row.UserID),
		nilIfEmpty(row.AgentID), nilIfEmpty(row.ParentRunID),
		row.Iteration, row.Provider, row.Model, row.CredentialSource, row.CredentialScopeID,
		row.InputTokens, row.OutputTokens, row.CacheCreationTokens, row.CacheReadTokens,
		costArg, nilIfEmpty(row.CostCurrency), ts.UnixNano(),
	)
	return err
}

// TokenUsageForRun returns all per-call usage rows for a run, oldest first.
func (s *Store) TokenUsageForRun(ctx context.Context, runID string) ([]store.TokenUsageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, session_id, tenant_id, user_id, agent_id, parent_run_id,
			iteration, provider, model, credential_source, credential_scope_id,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cost, cost_currency, ts
		 FROM token_usage WHERE run_id = ? ORDER BY iteration ASC, id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TokenUsageRow
	for rows.Next() {
		var r store.TokenUsageRow
		var sessionID, userID, agentID, parentRunID, costCurrency sql.NullString
		var cost sql.NullFloat64
		var tsNano int64
		if err := rows.Scan(
			&r.RunID, &sessionID, &r.TenantID, &userID, &agentID, &parentRunID,
			&r.Iteration, &r.Provider, &r.Model, &r.CredentialSource, &r.CredentialScopeID,
			&r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens,
			&cost, &costCurrency, &tsNano,
		); err != nil {
			return nil, err
		}
		r.SessionID = sessionID.String
		r.UserID = userID.String
		r.AgentID = agentID.String
		r.ParentRunID = parentRunID.String
		r.Cost = cost.Float64
		r.CostCurrency = costCurrency.String
		r.TS = time.Unix(0, tsNano)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunCostSummary sums a run's per-call token_usage ledger (RFC AV). COUNT(cost)
// counts non-NULL costs (unpriced rows store NULL cost), so priced>0 ⇒ at least
// one call was priced; MAX(cost_currency) ignores NULLs so it returns the currency
// among the priced rows (or ” when none priced ⇒ unpriced run). This makes
// runs.cost == Σ(ledger) by construction (see the interface doc).
func (s *Store) RunCostSummary(ctx context.Context, runID string) (float64, string, bool, error) {
	var cost float64
	var currency string
	var priced int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost),0), COALESCE(MAX(cost_currency),''), COUNT(cost)
		 FROM token_usage WHERE run_id = ?`, runID).Scan(&cost, &currency, &priced)
	if err != nil {
		return 0, "", false, err
	}
	return cost, currency, priced > 0, nil
}

// TokenLimitPut upserts one per-scope token budget (RFC AW). A nil soft/hard
// stores NULL for that tier. updated_at is stored as unix-nano.
func (s *Store) TokenLimitPut(ctx context.Context, row store.TokenLimitRow) error {
	updatedAt := row.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO token_limits (tenant_id, scope, scope_id, soft_limit, hard_limit, updated_at, updated_by)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT (tenant_id, scope, scope_id) DO UPDATE SET
		   soft_limit = excluded.soft_limit,
		   hard_limit = excluded.hard_limit,
		   updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		row.TenantID, row.Scope, row.ScopeID,
		nullableInt64(row.SoftLimit), nullableInt64(row.HardLimit),
		updatedAt.UnixNano(), row.UpdatedBy,
	)
	return err
}

// TokenLimitDelete removes a scope's budget (→ unlimited). No-op when absent.
func (s *Store) TokenLimitDelete(ctx context.Context, tenantID, scope, scopeID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM token_limits WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, scope, scopeID)
	return err
}

// TokenLimitsAll returns every token-limit row (RFC AW) for the tracker cache.
func (s *Store) TokenLimitsAll(ctx context.Context) ([]store.TokenLimitRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, scope, scope_id, soft_limit, hard_limit, updated_at, updated_by
		 FROM token_limits`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TokenLimitRow
	for rows.Next() {
		var r store.TokenLimitRow
		var soft, hard sql.NullInt64
		var updatedNano int64
		if err := rows.Scan(&r.TenantID, &r.Scope, &r.ScopeID, &soft, &hard, &updatedNano, &r.UpdatedBy); err != nil {
			return nil, err
		}
		if soft.Valid {
			v := soft.Int64
			r.SoftLimit = &v
		}
		if hard.Valid {
			v := hard.Int64
			r.HardLimit = &v
		}
		r.UpdatedAt = time.Unix(0, updatedNano)
		out = append(out, r)
	}
	return out, rows.Err()
}

// nanosPerDay is the day bucket for the usage rollup's period_start (ts is
// stored as unix-nano in sqlite).
const nanosPerDay = int64(86400) * int64(1e9)

// UsageReport aggregates recent per-call token_usage UNION the compact
// usage_archive rollup (RFC AV Phase 2b), so a window that has been pruned to
// the archive still reports. The five dimension columns are SELECTed in
// UsageCanonicalDims order — the column when grouped, else ” — a fixed
// 13-column shape. ts / period_start are unix-nano, so window bounds are nanos.
func (s *Store) UsageReport(ctx context.Context, q store.UsageQuery) ([]store.UsageAggregate, error) {
	dimExprs, groupCols := store.UsageGroupColumns(q.GroupBy)
	// Per-source WHERE (tenant + window). token_usage windows on ts (exact);
	// usage_archive on period_start (day-truncated UTC midnight). Args are
	// appended in placeholder order (token_usage first). floorFromDay floors the
	// `from` bound to its UTC day for the archive branch — see below.
	where := func(tsCol string, floorFromDay bool) (string, []any) {
		var conds []string
		var args []any
		if q.TenantID != "" {
			conds = append(conds, "tenant_id = ?")
			args = append(args, q.TenantID)
		}
		if !q.From.IsZero() {
			conds = append(conds, tsCol+" >= ?")
			from := q.From.UnixNano()
			if floorFromDay {
				// The archive is day-bucketed (period_start = UTC midnight), so an
				// intra-day `from` (e.g. 12:00) compared exactly would drop the whole
				// from-day bucket (period_start 00:00 < 12:00). Floor `from` to its
				// UTC day so that bucket is included. This makes the boundary day
				// over-inclusive at day granularity — never under-inclusive.
				from = (from / nanosPerDay) * nanosPerDay
			}
			args = append(args, from)
		}
		if !q.To.IsZero() {
			conds = append(conds, tsCol+" <= ?")
			args = append(args, q.To.UnixNano())
		}
		if len(conds) == 0 {
			return "", nil
		}
		return " WHERE " + strings.Join(conds, " AND "), args
	}
	tuWhere, tuArgs := where("ts", false)
	uaWhere, uaArgs := where("period_start", true)

	inner := `SELECT tenant_id, COALESCE(user_id,'') AS user_id, provider, model, credential_source,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			COALESCE(cost,0) AS cost, COALESCE(cost_currency,'') AS cost_currency,
			1 AS call_count, CASE WHEN cost IS NULL THEN 1 ELSE 0 END AS unpriced_calls
		FROM token_usage` + tuWhere + `
		UNION ALL
		SELECT tenant_id, user_id, provider, model, credential_source,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cost, cost_currency, call_count, unpriced_calls
		FROM usage_archive` + uaWhere

	query := `SELECT ` + strings.Join(dimExprs, ", ") + `,
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(cost),0), COALESCE(SUM(call_count),0), COALESCE(SUM(unpriced_calls),0),
		cost_currency
		FROM (` + inner + `)`
	// cost_currency is ALWAYS in the GROUP BY so a row never sums across currencies
	// — each output row is single-currency (unpriced rows, currency '', group
	// together). A single-currency deployment still yields one row per bucket.
	groupCols = append(groupCols, "cost_currency")
	query += " GROUP BY " + strings.Join(groupCols, ", ")
	query += " ORDER BY COALESCE(SUM(cost),0) DESC"

	args := append(append([]any{}, tuArgs...), uaArgs...)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.UsageAggregate
	for rows.Next() {
		var a store.UsageAggregate
		if err := rows.Scan(
			&a.TenantID, &a.UserID, &a.Provider, &a.Model, &a.CredentialSource,
			&a.InputTokens, &a.OutputTokens, &a.CacheCreationTokens, &a.CacheReadTokens,
			&a.Cost, &a.CallCount, &a.UnpricedCalls, &a.Currency,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RollupAndPruneUsage folds token_usage rows older than olderThan into
// usage_archive (day-bucketed) and deletes them, in one transaction (RFC AV
// Phase 2b). Idempotent via the archive PK. Returns the raw rows pruned.
func (s *Store) RollupAndPruneUsage(ctx context.Context, olderThan time.Time) (int, error) {
	cutoff := olderThan.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cost, cost_currency, call_count, unpriced_calls)
		SELECT (ts/?)*? AS period_start, tenant_id, COALESCE(user_id,''), provider, model, credential_source,
			SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens),
			SUM(COALESCE(cost,0)), COALESCE(MAX(cost_currency),''),
			COUNT(*), SUM(CASE WHEN cost IS NULL THEN 1 ELSE 0 END)
		FROM token_usage WHERE ts < ?
		GROUP BY period_start, tenant_id, COALESCE(user_id,''), provider, model, credential_source
		ON CONFLICT(period_start, tenant_id, user_id, provider, model, credential_source) DO UPDATE SET
			input_tokens          = input_tokens + excluded.input_tokens,
			output_tokens         = output_tokens + excluded.output_tokens,
			cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
			cache_read_tokens     = cache_read_tokens + excluded.cache_read_tokens,
			cost                  = cost + excluded.cost,
			cost_currency         = CASE WHEN usage_archive.cost_currency = '' THEN excluded.cost_currency ELSE usage_archive.cost_currency END,
			call_count            = call_count + excluded.call_count,
			unpriced_calls        = unpriced_calls + excluded.unpriced_calls`,
		nanosPerDay, nanosPerDay, cutoff,
	); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM token_usage WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// PrunableAgedSessions lists sessions where EVERY run is terminal + old (RFC AV
// Phase 2b2). completed_at is unix-nano here. A session qualifies only when it
// has no run in a non-terminal state (running/paused/pausing, by status OR
// pause_state) and its most-recent completed_at is before olderThan — so an
// aged run inside a still-active session is never pruned out from under a
// continuation's whole-session transcript replay.
//
// A PINNED session (sessions.pinned = 1) is never returned — pinning is the
// operator's explicit "keep this" and must exempt the chat from ALL automated
// retention paths (RFC BM Phase 2 chats sweeper + the legacy RFC AV archiver,
// both of which consume this list).
func (s *Store) PrunableAgedSessions(ctx context.Context, olderThan time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id FROM runs
		 GROUP BY session_id
		 HAVING SUM(CASE WHEN status NOT IN (?, ?, ?)
		                   OR pause_state IN ('paused', 'pausing')
		                 THEN 1 ELSE 0 END) = 0
		    AND MAX(completed_at) IS NOT NULL
		    AND MAX(completed_at) < ?
		    AND session_id NOT IN (SELECT id FROM sessions WHERE pinned = 1)
		 ORDER BY MAX(completed_at) ASC
		 LIMIT ?`,
		string(store.RunCompleted), string(store.RunFailed), string(store.RunCancelled),
		olderThan.UnixNano(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RunsForSession returns every run in the session (any status), oldest first.
func (s *Store) RunsForSession(ctx context.Context, sessionID string) ([]store.Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.session_id = ? ORDER BY r.started_at ASC`,
		sessionID,
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

// ListSessions returns sessions matching the filter, rolled up with their runs'
// aggregates (RFC BE). The aggregation runs in a derived subquery so the
// session-level filters (tenant/user/agent/tag/title/pinned/archived) narrow the
// GROUP BY, and the derived filters (status + the last-activity time window)
// apply to the aggregate — the count and page queries share the exact filtered
// set. See store.SessionSummary for the derived Status + LastActivity semantics.
func (s *Store) ListSessions(ctx context.Context, f store.SessionFilter, limit, offset int) ([]store.SessionSummary, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500 // cap to keep memory bounded (mirrors ListEvents)
	}
	if offset < 0 {
		offset = 0
	}

	// Session-level filters (applied inside the GROUP BY subquery).
	var innerConds []string
	var innerArgs []any
	if f.TenantID != "" {
		innerConds = append(innerConds, "s.tenant_id = ?")
		innerArgs = append(innerArgs, f.TenantID)
	}
	if f.UserID != "" {
		innerConds = append(innerConds, "s.user_id = ?")
		innerArgs = append(innerArgs, f.UserID)
	}
	if f.AgentName != "" {
		innerConds = append(innerConds, "s.agent = ?")
		innerArgs = append(innerArgs, f.AgentName)
	}
	if !f.IncludeArchived {
		innerConds = append(innerConds, "s.archived_at IS NULL")
	}
	if f.IncludePinned {
		innerConds = append(innerConds, "s.pinned = 1")
	}
	if f.Tag != "" {
		// Substring match against the JSON-array text. The needle is the tag
		// JSON-encoded (store.EncodeTagMatch) so it matches the stored element
		// byte-for-byte even when the tag contains a quote/backslash (which
		// EncodeTags escapes); its surrounding quotes keep `"q3"` from matching
		// inside `"q3-plan"`; INSTR (not LIKE) avoids interpreting `%`/`_`.
		needle, err := store.EncodeTagMatch(f.Tag)
		if err != nil {
			return nil, 0, fmt.Errorf("encode tag filter: %w", err)
		}
		innerConds = append(innerConds, "INSTR(s.tags, ?) > 0")
		innerArgs = append(innerArgs, needle)
	}
	if f.TitleContains != "" {
		innerConds = append(innerConds, "INSTR(LOWER(s.title), LOWER(?)) > 0")
		innerArgs = append(innerArgs, f.TitleContains)
	}
	innerWhere := ""
	if len(innerConds) > 0 {
		innerWhere = "WHERE " + strings.Join(innerConds, " AND ")
	}

	// Derived filters (applied to the aggregate).
	var outerConds []string
	var outerArgs []any
	if f.Status != "" {
		outerConds = append(outerConds, "agg.status = ?")
		outerArgs = append(outerArgs, string(f.Status))
	}
	if !f.From.IsZero() {
		outerConds = append(outerConds, "agg.last_activity >= ?")
		outerArgs = append(outerArgs, f.From.UnixNano())
	}
	if !f.To.IsZero() {
		outerConds = append(outerConds, "agg.last_activity <= ?")
		outerArgs = append(outerArgs, f.To.UnixNano())
	}
	outerWhere := ""
	if len(outerConds) > 0 {
		outerWhere = "WHERE " + strings.Join(outerConds, " AND ")
	}

	// last_activity = MAX of each run's completed-or-started time, falling back
	// to the session's created_at when it has no runs. status = "running" if any
	// run is active, else the most recent run's status ("" for a runless
	// session). Both are computed as columns so the outer query can filter them.
	inner := `SELECT
			s.id, s.tenant_id, s.agent, s.user_id, s.created_at,
			s.title, s.description, s.tags, s.pinned, s.archived_at, s.summary,
			COUNT(r.id) AS run_count,
			COALESCE(SUM(r.input_tokens), 0) AS in_tok,
			COALESCE(SUM(r.output_tokens), 0) AS out_tok,
			COALESCE(SUM(r.cost), 0) AS cost,
			COALESCE(MAX(CASE WHEN r.completed_at IS NOT NULL AND r.completed_at > r.started_at
			                  THEN r.completed_at ELSE r.started_at END), s.created_at) AS last_activity,
			CASE WHEN MAX(CASE WHEN r.status = 'running' THEN 1 ELSE 0 END) = 1 THEN 'running'
			     ELSE COALESCE((SELECT r2.status FROM runs r2 WHERE r2.session_id = s.id
			                    ORDER BY r2.started_at DESC LIMIT 1), '') END AS status
		FROM sessions s
		LEFT JOIN runs r ON r.session_id = s.id
		` + innerWhere + `
		GROUP BY s.id`

	countArgs := append(append([]any{}, innerArgs...), outerArgs...)
	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ("+inner+") agg "+outerWhere, countArgs...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, innerArgs...), outerArgs...)
	pageArgs = append(pageArgs, limit, offset)
	rows, err := s.db.QueryContext(ctx,
		"SELECT agg.id, agg.tenant_id, agg.agent, agg.user_id, agg.created_at, "+
			"agg.title, agg.description, agg.tags, agg.pinned, agg.archived_at, agg.summary, "+
			"agg.run_count, agg.in_tok, agg.out_tok, agg.cost, agg.last_activity, agg.status "+
			"FROM ("+inner+") agg "+outerWhere+
			" ORDER BY agg.pinned DESC, agg.last_activity DESC LIMIT ? OFFSET ?",
		pageArgs...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]store.SessionSummary, 0, limit)
	for rows.Next() {
		var ss store.SessionSummary
		var userID, title, description, tags, summary sql.NullString
		var createdNs, lastActNs int64
		var archivedNs sql.NullInt64
		var pinnedInt int64
		var statusStr string
		if err := rows.Scan(
			&ss.SessionID, &ss.TenantID, &ss.Agent, &userID, &createdNs,
			&title, &description, &tags, &pinnedInt, &archivedNs, &summary,
			&ss.RunCount, &ss.InputTokens, &ss.OutputTokens, &ss.Cost, &lastActNs, &statusStr,
		); err != nil {
			return nil, 0, err
		}
		ss.CreatedAt = time.Unix(0, createdNs)
		ss.LastActivity = time.Unix(0, lastActNs)
		if userID.Valid {
			ss.UserID = userID.String
		}
		if title.Valid {
			ss.Title = title.String
		}
		if description.Valid {
			ss.Description = description.String
		}
		if summary.Valid {
			ss.Summary = summary.String
		}
		if tags.Valid {
			decoded, derr := store.DecodeTags(tags.String)
			if derr != nil {
				return nil, 0, fmt.Errorf("decode session tags: %w", derr)
			}
			ss.Tags = decoded
		}
		ss.Pinned = pinnedInt != 0
		ss.Archived = archivedNs.Valid
		if statusStr != "" {
			ss.Status = store.RunStatus(statusStr)
		}
		out = append(out, ss)
	}
	return out, total, rows.Err()
}

// SetSessionMeta writes only the non-nil fields of the patch onto the session
// row (RFC BE). Returns *store.ErrNotFound when the session does not exist.
func (s *Store) SetSessionMeta(ctx context.Context, sessionID string, p store.SessionMetaPatch) error {
	now := time.Now().UnixNano()
	var sets []string
	var args []any
	if p.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, nilIfEmpty(*p.Title))
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, nilIfEmpty(*p.Description))
	}
	if p.Summary != nil {
		// A summary write always stamps summary_updated_at (idempotent recap).
		sets = append(sets, "summary = ?", "summary_updated_at = ?")
		args = append(args, nilIfEmpty(*p.Summary), now)
	}
	if p.Tags != nil {
		encoded, err := store.EncodeTags(*p.Tags)
		if err != nil {
			return fmt.Errorf("encode session tags: %w", err)
		}
		sets = append(sets, "tags = ?")
		args = append(args, encoded)
	}
	if p.Pinned != nil {
		pv := int64(0)
		if *p.Pinned {
			pv = 1
		}
		sets = append(sets, "pinned = ?")
		args = append(args, pv)
	}
	if p.Archived != nil {
		if *p.Archived {
			sets = append(sets, "archived_at = ?")
			args = append(args, now)
		} else {
			sets = append(sets, "archived_at = NULL")
		}
	}
	if len(sets) == 0 {
		// Empty patch: still surface ErrNotFound for a missing session so the
		// caller sees consistent semantics.
		_, err := s.GetSession(ctx, sessionID)
		return err
	}
	args = append(args, sessionID)
	res, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	return nil
}

// DeleteSessionCascade deletes a session + all its runs + events in one tx (RFC
// AV Phase 2b2). Events + runs are removed explicitly (not via FK cascade) so
// behavior is identical regardless of the foreign_keys pragma; token_usage is
// intentionally left (independent retention).
func (s *Store) DeleteSessionCascade(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
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

// GetRunEventsSince returns a run's events with seq > afterSeq, ordered by seq
// ascending, capped at limit. Incremental run-scoped read for the interactive
// SSE tail — indexed on events.run_id like GetLastEventForRun.
func (s *Store) GetRunEventsSince(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, session_id, run_id, ts, type, payload
		 FROM events WHERE run_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Event
	for rows.Next() {
		var ev store.Event
		var ts int64
		if err := rows.Scan(&ev.Seq, &ev.SessionID, &ev.RunID, &ts, &ev.Type, &ev.Payload); err != nil {
			return nil, err
		}
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
	// Columns are qualified with the `events.` table name so the optional
	// sessions JOIN below stays unambiguous (valid with or without the JOIN).
	if filter.Type != "" {
		conds = append(conds, "events.type = ?")
		args = append(args, filter.Type)
	}
	if !filter.From.IsZero() {
		conds = append(conds, "events.ts >= ?")
		args = append(args, filter.From.UnixNano())
	}
	if !filter.To.IsZero() {
		conds = append(conds, "events.ts <= ?")
		args = append(args, filter.To.UnixNano())
	}
	// RFC AS: tenant-scope via the owning session. events has no tenant column,
	// but events.session_id is NOT NULL → sessions.tenant_id is the event's
	// tenant. Empty TenantID = no filter (all tenants — the admin view).
	from := "events"
	if filter.TenantID != "" {
		from = "events JOIN sessions ON sessions.id = events.session_id"
		conds = append(conds, "sessions.tenant_id = ?")
		args = append(args, filter.TenantID)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// COUNT(*) first — same WHERE, but separate to avoid window-fn
	// complexity on SQLite. Cheap because the WHERE narrows via the
	// new indexes.
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+from+" "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx,
		"SELECT events.seq, events.session_id, events.run_id, events.ts, events.type, events.payload FROM "+from+" "+where+
			" ORDER BY events.ts DESC, events.seq DESC LIMIT ? OFFSET ?",
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
	var idempotencyKey sql.NullString
	var tenantID sql.NullString
	var interactive sql.NullInt64
	var operatorKeyRestricted sql.NullInt64
	var cost sql.NullFloat64
	var costCurrency, credentialSource, credentialScopeID sql.NullString
	var sessAgent sql.NullString
	var status string
	if err := scanner.Scan(
		&r.ID, &r.SessionID, &status, &startedNs, &completedNs,
		&stopReason,
		&r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens,
		&model, &provider, &errMsg,
		&agentID, &parentAgentID, &parentRunID, &userID, &lastHbNs,
		&userTier,
		&agentDefID, &pauseState, &parentContext, &idempotencyKey, &tenantID,
		&interactive, &operatorKeyRestricted,
		&cost, &costCurrency, &credentialSource, &credentialScopeID,
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
	if idempotencyKey.Valid {
		r.IdempotencyKey = idempotencyKey.String
	}
	if tenantID.Valid {
		r.TenantID = tenantID.String
	}
	r.Interactive = interactive.Valid && interactive.Int64 != 0
	r.OperatorKeyRestricted = operatorKeyRestricted.Valid && operatorKeyRestricted.Int64 != 0
	if cost.Valid {
		v := cost.Float64
		r.Cost = &v
	}
	if costCurrency.Valid {
		r.CostCurrency = costCurrency.String
	}
	if credentialSource.Valid {
		r.CredentialSource = credentialSource.String
	}
	if credentialScopeID.Valid {
		r.CredentialScopeID = credentialScopeID.String
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
		r.agent_def_id, r.pause_state, r.parent_context, r.idempotency_key, r.tenant_id,
		r.interactive, r.operator_key_restricted,
		r.cost, r.cost_currency, r.credential_source, r.credential_scope_id,
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

// RunByIdempotencyKey returns the run created with the given RFC H
// Decision 10 idempotency key. An empty key short-circuits to
// (Run{}, false, nil); a key with no matching row returns the same.
// The runs_idempotency_key partial unique index guarantees at most one
// match.
func (s *Store) RunByIdempotencyKey(ctx context.Context, key string) (store.Run, bool, error) {
	if key == "" {
		return store.Run{}, false, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.idempotency_key = ? LIMIT 1`,
		key,
	)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Run{}, false, nil
	}
	if err != nil {
		return store.Run{}, false, fmt.Errorf("run by idempotency_key: %w", err)
	}
	return r, true, nil
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
func (s *Store) ListUsers(ctx context.Context, tenantID string) ([]store.UserSummary, error) {
	// tenantID "" = all tenants; otherwise scope to the denormalised
	// runs.tenant_id (RFC L per-tenant workspace + super-admin focus).
	q := `
		SELECT
			user_id,
			COUNT(CASE WHEN status = 'running' THEN 1 END) AS running_count,
			COUNT(*) AS total_count,
			MAX(started_at) AS last_started_at
		FROM runs
		WHERE user_id IS NOT NULL AND user_id != ''`
	args := []any{}
	if tenantID != "" {
		q += ` AND tenant_id = ?`
		args = append(args, tenantID)
	}
	q += `
		GROUP BY user_id
		ORDER BY last_started_at DESC
		LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q, args...)
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
		   -- F42 / RFC X Phase 2: a paused/pausing run is INTENTIONALLY parked
		   -- (no heartbeat by design), not crashed — never sweep it stale. A
		   -- snapshotted+restored paused run carries an old started_at + NULL
		   -- heartbeat; without this guard the sweeper would kill it before
		   -- resume re-dispatches it.
		   AND COALESCE(pause_state, 'running') NOT IN ('paused', 'pausing')
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

// errHooksSQLiteUnsupported wraps the exported store.ErrHooksUnsupported so
// callers can errors.Is against the backend-agnostic sentinel (e.g. the store
// contract suite skips the cluster-hook path), while keeping the detailed
// SQLite-specific guidance in the message.
var errHooksSQLiteUnsupported = fmt.Errorf("hooks: SQLite backend is single-replica only; hook DB methods require Postgres (set LOOMCYCLE_REPLICA_ID to enter cluster mode): %w", store.ErrHooksUnsupported)

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
		        retired, bootstrapped_from_static, tenant_id
		 FROM agent_defs
		 ORDER BY tenant_id ASC, name ASC, version ASC`,
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
			&retiredInt, &bootstrap, &r.TenantID,
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
		`SELECT name, def_id, promoted_at, promoted_by_agent_id, tenant_id
		 FROM agent_def_active
		 ORDER BY tenant_id ASC, name ASC`)
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
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter, &e.TenantID); err != nil {
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
		        retired, bootstrapped_from_static, tenant_id
		 FROM skill_defs
		 ORDER BY tenant_id ASC, name ASC, version ASC`,
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
			&retiredInt, &bootstrap, &r.TenantID,
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
		`SELECT name, def_id, promoted_at, promoted_by_agent_id, tenant_id
		 FROM skill_def_active
		 ORDER BY tenant_id ASC, name ASC`)
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
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter, &e.TenantID); err != nil {
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

// SnapshotReadTeamDefs implements store.Store. Mirror of
// SnapshotReadSkillDefs against the teamdefs table.
func (s *Store) SnapshotReadTeamDefs(ctx context.Context) ([]store.TeamDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_id, name, version, parent_def_id, definition, description,
		        created_at, created_by_agent_id, created_by_run_id,
		        retired, bootstrapped_from_static, tenant_id, content_sha256
		 FROM teamdefs
		 ORDER BY tenant_id ASC, name ASC, version ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot read teamdefs: %w", err)
	}
	defer rows.Close()
	var out []store.TeamDefRow
	for rows.Next() {
		var (
			r           store.TeamDefRow
			createdNs   int64
			parentDefID sql.NullString
			description sql.NullString
			createdBy   sql.NullString
			createdRun  sql.NullString
			definition  string
			retiredInt  int
			bootstrap   int
			contentSHA  sql.NullString
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version, &parentDefID,
			&definition, &description,
			&createdNs, &createdBy, &createdRun,
			&retiredInt, &bootstrap, &r.TenantID, &contentSHA,
		); err != nil {
			return nil, fmt.Errorf("scan team_def: %w", err)
		}
		if contentSHA.Valid {
			r.ContentSHA256 = contentSHA.String
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

// SnapshotReadTeamDefActive implements store.Store.
func (s *Store) SnapshotReadTeamDefActive(ctx context.Context) ([]store.TeamDefActiveEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, def_id, promoted_at, promoted_by_agent_id, tenant_id
		 FROM teamdef_active
		 ORDER BY tenant_id ASC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read teamdef_active: %w", err)
	}
	defer rows.Close()
	var out []store.TeamDefActiveEntry
	for rows.Next() {
		var (
			e          store.TeamDefActiveEntry
			promotedNs int64
			promoter   sql.NullString
		)
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter, &e.TenantID); err != nil {
			return nil, fmt.Errorf("scan teamdef_active: %w", err)
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
		        retired, bootstrapped_from_static, content_sha256, tenant_id
		 FROM mcp_server_defs
		 ORDER BY tenant_id ASC, name ASC, version ASC`,
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
			&retiredInt, &bootstrap, &contentHash, &r.TenantID,
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
		`SELECT name, def_id, promoted_at, promoted_by_agent_id, tenant_id
		 FROM mcp_server_def_active
		 ORDER BY tenant_id ASC, name ASC`)
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
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter, &e.TenantID); err != nil {
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
		`SELECT COALESCE(tenant_id, ''), scope, scope_id, key, value, expires_at, created_at, updated_at
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
			&e.TenantID, &scopeStr, &e.ScopeID, &e.Key, &value,
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
			user_tier, agent_def_id, pause_state, parent_context, interactive, operator_key_restricted
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SessionID, status, startedNs, completedNs, nilIfEmpty(r.StopReason),
		r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
		nilIfEmpty(r.Model), nilIfEmpty(r.Provider), nilIfEmpty(r.ErrorMsg),
		nilIfEmpty(r.AgentID), nilIfEmpty(r.ParentAgentID), nilIfEmpty(r.ParentRunID),
		nilIfEmpty(r.UserID), lastHbNs,
		nilIfEmpty(r.UserTier), nilIfEmpty(r.AgentDefID), pauseState, pcVal,
		boolToInt(r.Interactive), boolToInt(r.OperatorKeyRestricted),
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
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256), r.TenantID,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore agent_def: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreAgentDefActive implements store.Store. INSERT OR
// IGNORE on the (tenant_id, name) PK — first restore writes the
// snapshot's promoted_at + def_id; subsequent restores leave the
// existing row alone so the (bool, error) return reads as "not
// inserted" and the caller's counter stays honest.
func (s *Store) SnapshotRestoreAgentDefActive(ctx context.Context, e store.AgentDefActiveEntry) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore agent_def_active: name and def_id required")
	}
	promotedNs := e.PromotedAt.UnixNano()
	if e.PromotedAt.IsZero() {
		promotedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO agent_def_active(tenant_id, name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?, ?)`,
		e.TenantID, e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
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
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256), r.TenantID,
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
		`INSERT OR IGNORE INTO skill_def_active(tenant_id, name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?, ?)`,
		e.TenantID, e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore skill_def_active: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreTeamDef implements store.Store. Mirror of
// SnapshotRestoreSkillDef against teamdefs.
func (s *Store) SnapshotRestoreTeamDef(ctx context.Context, r store.TeamDefRow) (bool, error) {
	if r.DefID == "" || r.Name == "" {
		return false, fmt.Errorf("snapshot restore team_def: def_id and name required")
	}
	createdNs := r.CreatedAt.UnixNano()
	if r.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO teamdefs(
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256), r.TenantID,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore team_def: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SnapshotRestoreTeamDefActive implements store.Store. Mirror of
// SnapshotRestoreSkillDefActive against teamdef_active.
func (s *Store) SnapshotRestoreTeamDefActive(ctx context.Context, e store.TeamDefActiveEntry) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore teamdef_active: name and def_id required")
	}
	promotedNs := e.PromotedAt.UnixNano()
	if e.PromotedAt.IsZero() {
		promotedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO teamdef_active(tenant_id, name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?, ?)`,
		e.TenantID, e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore teamdef_active: %w", err)
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
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic),
		nilIfEmpty(r.ContentSHA256), r.TenantID,
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
		`INSERT OR IGNORE INTO mcp_server_def_active(tenant_id, name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?, ?)`,
		e.TenantID, e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
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
		`INSERT OR IGNORE INTO memory(tenant_id, scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TenantID, string(e.Scope), e.ScopeID, e.Key, string(e.Value), expiresNs, createdNs, updatedNs,
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
func (s *Store) MemorySet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error {
	now := time.Now().UnixNano()
	var expiresAt any
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UnixNano()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory(tenant_id, scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at,
		    superseded_at = NULL`,
		tenantID, string(scope), scopeID, key, string(value), expiresAt, now, now,
	)
	return err
}

// MemoryGet returns the entry or *ErrNotFound. Expired rows are
// surfaced as ErrNotFound regardless of whether the sweeper has
// reaped them yet.
func (s *Store) MemoryGet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	var (
		valueText string
		expiresAt sql.NullInt64
		createdAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at, created_at, updated_at
		 FROM memory WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?
		   AND superseded_at IS NULL`,
		tenantID, string(scope), scopeID, key,
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
func (s *Store) MemoryDelete(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?`,
		tenantID, string(scope), scopeID, key,
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

// MemoryDeleteScope removes every entry under (tenantID, scope, scopeID) (RFC
// BM retention) and returns the memory-table row count. It also clears the
// scope's consolidation state — memory_pending (the queue) and memory_cursors
// (the watermark/lease) — which are NOT FK-linked to memory (no cascade), so a
// memory-only delete would orphan them. All three deletes run in one
// transaction so a reclaim is atomic. memory_embeddings rows cascade via their
// ON DELETE CASCADE FK (Postgres-only; SQLite has no such table).
func (s *Store) MemoryDeleteScope(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM memory WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, string(scope), scopeID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_pending WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, string(scope), scopeID,
	); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_cursors WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, string(scope), scopeID,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// MemoryList enumerates entries for a (scope, scopeID), filtered by
// prefix and capped at limit rows. Expired rows are filtered in the
// WHERE clause so callers never see them. truncated == true when the
// underlying query found at least limit+1 rows.
func (s *Store) MemoryList(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	nowNs := time.Now().UnixNano()
	// Fetch limit+1 to detect truncation without a separate COUNT(*).
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, expires_at, created_at, updated_at
		 FROM memory
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key LIKE ? ESCAPE '\'
		   AND (expires_at IS NULL OR expires_at > ?)
		   AND superseded_at IS NULL
		 ORDER BY key ASC
		 LIMIT ?`,
		tenantID, string(scope), scopeID, escapeLikePrefix(prefix)+"%", nowNs, limit+1,
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
func (s *Store) MemoryIncrement(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
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
		`SELECT value, expires_at FROM memory WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?`,
		tenantID, string(scope), scopeID, key,
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
		`INSERT INTO memory(tenant_id, scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at,
		    superseded_at = NULL`,
		tenantID, string(scope), scopeID, key, nextText, newExpires, nowNs, nowNs,
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
	tenantID string,
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
		`SELECT value, expires_at FROM memory WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?`,
		tenantID, string(scope), scopeID, key,
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
		`INSERT INTO memory(tenant_id, scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at,
		    superseded_at = NULL`,
		tenantID, string(scope), scopeID, key, string(next), newExpires, nowNs, nowNs,
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
func (s *Store) MemoryListScopeIDs(ctx context.Context, tenantID string, scope store.MemoryScope) ([]store.MemoryScopeIDSummary, error) {
	nowNs := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			scope_id,
			COUNT(*)                                              AS key_count,
			COALESCE(SUM(LENGTH(key) + LENGTH(value)), 0)          AS bytes,
			MAX(updated_at)                                        AS updated_at
		FROM memory
		WHERE tenant_id = ? AND scope = ? AND (expires_at IS NULL OR expires_at > ?)
		GROUP BY scope_id
		ORDER BY updated_at DESC
		LIMIT 200`,
		tenantID, string(scope), nowNs,
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

// MemoryListTenantsForScope implements store.Store (RFC BL). DISTINCT over every
// row for (scope, scopeID) regardless of expiry — reclamation deletes expired
// rows too, so an all-expired partition must still be enumerated.
func (s *Store) MemoryListTenantsForScope(ctx context.Context, scope store.MemoryScope, scopeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT COALESCE(tenant_id, '') FROM memory WHERE scope = ? AND scope_id = ?`,
		string(scope), scopeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MemoryFullTextSearch is the RFC BL keyword-retrieval leg. SQLite ships no
// tsvector index (the vectors themselves are Postgres-only), so per the
// documented degrade posture this returns (nil, nil) rather than erroring —
// the in-process backend then falls back to pure-vector fusion. We do NOT add
// full-text infra to SQLite.
func (s *Store) MemoryFullTextSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, keyPrefix, queryText string, topK int) ([]store.MemorySearchEntry, error) {
	return nil, nil
}

// SupportsFullText reports false for SQLite regardless of build tag — it ships
// no tsvector index (RFC BL adds full-text only on Postgres+pgvector), so the
// in-process backend takes the cheap pure-vector path rather than paying for a
// keyword round-trip that would always return (nil, nil).
func (s *Store) SupportsFullText() bool {
	return false
}

// MemoryBumpAccessBatch applies access-count deltas additively (RFC BL
// hybrid retrieval, OQ #4). See the Store interface for the semantics.
// SQLite's memory table carries access_count/last_accessed_at (PR1), so this
// is a real update even though SQLite has no vector search: it's wired
// unconditionally and simply never receives bumps when search is unavailable.
// One transaction per flush; each per-row UPDATE reads-then-adds, so
// sequential flushes sum. SQLite serialises writers, so a bump for a row that
// was concurrently deleted is a clean 0-row no-op.
func (s *Store) MemoryBumpAccessBatch(ctx context.Context, bumps []store.MemoryAccessBump) error {
	if len(bumps) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("MemoryBumpAccessBatch begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, b := range bumps {
		ts := b.LastAccess.UnixNano()
		// MAX(COALESCE(last_accessed_at, ts), ts): a NULL last_accessed_at
		// collapses to ts, otherwise take the later of the two.
		if _, err := tx.ExecContext(ctx,
			`UPDATE memory
			    SET access_count = access_count + ?,
			        last_accessed_at = MAX(COALESCE(last_accessed_at, ?), ?)
			  WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?`,
			b.CountDelta, ts, ts, b.TenantID, string(b.Scope), b.ScopeID, b.Key,
		); err != nil {
			return fmt.Errorf("MemoryBumpAccessBatch update: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("MemoryBumpAccessBatch commit: %w", err)
	}
	return nil
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

// ---- RFC BL P2: background memory consolidation substrate ----
//
// Timestamps are unix-nano INTEGER (like expires_at / last_accessed_at) and
// nullable timestamps use sql.NullInt64. payload is TEXT-encoded JSON. The
// lease + watermark ops that need a read-decide-write take a pinned connection
// with `BEGIN IMMEDIATE` (a write lock at BEGIN) so the CAS is atomic — same
// pattern MemoryIncrement uses; modernc/sqlite does not map an isolation level
// to BEGIN IMMEDIATE, so we issue it raw.

// MemorySupersede soft-archives one base-memory row. `superseded_at IS NULL`
// makes it idempotent; a missing key is a clean 0-row no-op.
func (s *Store) MemorySupersede(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE memory SET superseded_at = ?
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?
		   AND superseded_at IS NULL`,
		time.Now().UnixNano(), tenantID, string(scope), scopeID, key,
	)
	return err
}

// MemoryPendingEnqueue appends one durable consolidation-queue row. ID is
// generated when empty; CreatedAt defaults to now(); an empty payload stores
// JSON null.
func (s *Store) MemoryPendingEnqueue(ctx context.Context, row store.MemoryPendingRow) error {
	if row.ID == "" {
		row.ID = newID("mp_")
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	payload := row.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_pending
		   (id, tenant_id, scope, scope_id, payload, source_session_id, source_run_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.TenantID, string(row.Scope), row.ScopeID, string(payload),
		row.SourceSessionID, row.SourceRunID, createdAt.UnixNano(),
	)
	return err
}

// MemoryPendingDrain returns up to limit un-drained rows oldest-first without
// marking them drained (ack is separate — at-least-once).
func (s *Store) MemoryPendingDrain(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string, limit int) ([]store.MemoryPendingRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, scope, scope_id, payload, source_session_id, source_run_id, created_at, drained_at
		 FROM memory_pending
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND drained_at IS NULL
		 ORDER BY created_at ASC, id ASC
		 LIMIT ?`,
		tenantID, string(scope), scopeID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MemoryPendingRow
	for rows.Next() {
		var (
			r          store.MemoryPendingRow
			scopeStr   string
			payload    string
			srcSession sql.NullString
			srcRun     sql.NullString
			createdNs  int64
			drainedNs  sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &r.TenantID, &scopeStr, &r.ScopeID, &payload, &srcSession, &srcRun, &createdNs, &drainedNs); err != nil {
			return nil, err
		}
		r.Scope = store.MemoryScope(scopeStr)
		r.Payload = json.RawMessage(payload)
		r.SourceSessionID = srcSession.String
		r.SourceRunID = srcRun.String
		r.CreatedAt = time.Unix(0, createdNs)
		if drainedNs.Valid {
			r.DrainedAt = time.Unix(0, drainedNs.Int64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MemoryPendingAck marks the given ids drained. `drained_at IS NULL` keeps it
// idempotent; an unknown id is skipped; an empty slice is a no-op.
func (s *Store) MemoryPendingAck(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+4)
	args = append(args, time.Now().UnixNano(), tenantID, string(scope), scopeID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE memory_pending SET drained_at = ?
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
		   AND drained_at IS NULL AND id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	return err
}

// MemoryCursorGet is a get-or-default: a target with no row returns a
// zero-watermark, unleased row rather than ErrNotFound.
func (s *Store) MemoryCursorGet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string) (store.MemoryCursorRow, error) {
	out := store.MemoryCursorRow{TenantID: tenantID, Scope: scope, ScopeID: scopeID}
	var (
		wmComp    sql.NullInt64
		leaseExp  sql.NullInt64
		updatedNs int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT watermark_completed_at, watermark_session_id, leased_by, lease_expires_at, updated_at
		 FROM memory_cursors WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, string(scope), scopeID,
	).Scan(&wmComp, &out.WatermarkSessionID, &out.LeasedBy, &leaseExp, &updatedNs)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return store.MemoryCursorRow{}, err
	}
	if wmComp.Valid {
		out.WatermarkCompletedAt = time.Unix(0, wmComp.Int64)
	}
	if leaseExp.Valid {
		out.LeaseExpiresAt = time.Unix(0, leaseExp.Int64)
	}
	out.UpdatedAt = time.Unix(0, updatedNs)
	return out, nil
}

// MemoryCursorLease acquires (or re-acquires) the lease inside a BEGIN
// IMMEDIATE transaction so the read-decide-write CAS is atomic against a
// racing replica. Acquires iff unleased, expired, or already owner.
func (s *Store) MemoryCursorLease(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, owner string, now time.Time, ttl time.Duration) (store.MemoryCursorRow, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.MemoryCursorRow{}, false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.MemoryCursorRow{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	nowNs := now.UnixNano()
	expNs := now.Add(ttl).UnixNano()
	var (
		leasedBy string
		leaseExp sql.NullInt64
	)
	err = conn.QueryRowContext(ctx,
		`SELECT leased_by, lease_expires_at FROM memory_cursors
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, string(scope), scopeID,
	).Scan(&leasedBy, &leaseExp)
	exists := !errors.Is(err, sql.ErrNoRows)
	if exists && err != nil {
		return store.MemoryCursorRow{}, false, err
	}
	canAcquire := !exists || leasedBy == "" || !leaseExp.Valid || leaseExp.Int64 <= nowNs || leasedBy == owner
	acquired := false
	if canAcquire {
		if exists {
			if _, err := conn.ExecContext(ctx,
				`UPDATE memory_cursors SET leased_by = ?, lease_expires_at = ?, updated_at = ?
				 WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
				owner, expNs, nowNs, tenantID, string(scope), scopeID,
			); err != nil {
				return store.MemoryCursorRow{}, false, err
			}
		} else {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO memory_cursors (tenant_id, scope, scope_id, leased_by, lease_expires_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				tenantID, string(scope), scopeID, owner, expNs, nowNs,
			); err != nil {
				return store.MemoryCursorRow{}, false, err
			}
		}
		acquired = true
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.MemoryCursorRow{}, false, err
	}
	committed = true

	row, gerr := s.MemoryCursorGet(ctx, tenantID, scope, scopeID)
	if gerr != nil {
		return store.MemoryCursorRow{}, acquired, gerr
	}
	return row, acquired, nil
}

// MemoryCursorAdvance advances the composite watermark monotonically IFF owner
// holds a non-expired lease. Uses a BEGIN IMMEDIATE transaction: the ownership
// check errors with "not lease owner"; a backward/equal advance is a no-op.
func (s *Store) MemoryCursorAdvance(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, owner string, completedAt time.Time, sessionID string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	nowNs := time.Now().UnixNano()
	var (
		leasedBy string
		leaseExp sql.NullInt64
		wmComp   sql.NullInt64
		wmSess   string
	)
	err = conn.QueryRowContext(ctx,
		`SELECT leased_by, lease_expires_at, watermark_completed_at, watermark_session_id
		 FROM memory_cursors WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
		tenantID, string(scope), scopeID,
	).Scan(&leasedBy, &leaseExp, &wmComp, &wmSess)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("memory cursor advance: not lease owner (owner=%q, no lease)", owner)
	}
	if err != nil {
		return err
	}
	if leasedBy != owner || !leaseExp.Valid || leaseExp.Int64 <= nowNs {
		return fmt.Errorf("memory cursor advance: not lease owner (owner=%q)", owner)
	}
	completedNs := completedAt.UnixNano()
	monotonic := !wmComp.Valid || completedNs > wmComp.Int64 ||
		(completedNs == wmComp.Int64 && sessionID > wmSess)
	if monotonic {
		if _, err := conn.ExecContext(ctx,
			`UPDATE memory_cursors SET watermark_completed_at = ?, watermark_session_id = ?, updated_at = ?
			 WHERE tenant_id = ? AND scope = ? AND scope_id = ?`,
			completedNs, sessionID, nowNs, tenantID, string(scope), scopeID,
		); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// MemoryCursorRelease clears the lease IFF held by owner. Idempotent no-op
// otherwise; the watermark is untouched.
func (s *Store) MemoryCursorRelease(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, owner string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE memory_cursors SET leased_by = '', lease_expires_at = NULL, updated_at = ?
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND leased_by = ?`,
		time.Now().UnixNano(), tenantID, string(scope), scopeID, owner,
	)
	return err
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
		updated, ok, err := store.BackfillSystemPromptBase(p.Def)
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
// AgentDefCreate calls against the same (tenant, name) both start a
// DEFERRED tx, both SELECT MAX(version) (returning the same value),
// then both try to INSERT with the same version — one succeeds, one
// fails on the UNIQUE(tenant_id, name, version) constraint. RFC N:
// version allocation is scoped per-tenant.
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
		`SELECT MAX(version) FROM agent_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
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
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256), row.TenantID,
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

// AgentDefListNames returns one summary row per distinct (tenant, name).
// Joins agent_def_active to surface the active def_id when one exists.
// RFC N: names are per-tenant, so the grouping includes tenant_id — a
// name owned by N tenants yields N rows.
func (s *Store) AgentDefListNames(ctx context.Context) ([]store.AgentDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                              AS version_count,
			COUNT(*) FILTER (WHERE d.retired = 0) AS live_version_count,
			MAX(d.version)                        AS latest_version,
			MAX(d.created_at)                     AS last_updated,
			COALESCE(a.def_id, '')                AS active_def_id,
			COALESCE(ad.retired, 0)               AS active_retired
		FROM agent_defs d
		LEFT JOIN agent_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		LEFT JOIN agent_defs ad      ON ad.def_id = a.def_id
		GROUP BY d.tenant_id, d.name, a.def_id, ad.retired
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.AgentDefNameSummary
	for rows.Next() {
		var s store.AgentDefNameSummary
		var updatedAt int64
		var activeRetired int
		if err := rows.Scan(&s.TenantID, &s.Name, &s.VersionCount, &s.LiveVersionCount, &s.LatestVersion, &updatedAt, &s.ActiveDefID, &activeRetired); err != nil {
			return nil, err
		}
		s.LastUpdated = time.Unix(0, updatedAt)
		s.ActiveRetired = activeRetired != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListPurgeableRetiredDefVersions — RFC BM retention. created_at is an INTEGER
// unix-nano column here, so olderThan binds as .UnixNano(). SQLite (>=3.25)
// supports the row_number() window; as in Postgres it's computed in a subselect
// (SQL disallows window functions in WHERE) so the outer query can filter
// rn > keepLastN and apply LIMIT.
func (s *Store) ListPurgeableRetiredDefVersions(ctx context.Context, defType string, olderThan time.Time, keepLastN, limit int) ([]store.RetiredDefRef, error) {
	defsTable, activeTable, ok := store.RetentionDefTables(defType)
	if !ok {
		return nil, fmt.Errorf("retention: unknown def type %q", defType)
	}
	if keepLastN < 0 {
		keepLastN = 0
	}
	if limit <= 0 {
		limit = 500
	}
	// defsTable/activeTable are allowlisted names (never caller input) — safe to
	// interpolate. Values stay parameterized. The two NOT EXISTS clauses exclude
	// (a) the active version and (b) any version still referenced as a
	// parent_def_id — so we never purge a version whose descendant survives. This
	// keeps lineage intact AND avoids the postgres parent_def_id FK violation that
	// a single batched DELETE of a still-referenced parent would trigger; a
	// retired chain drains leaf-first over successive ticks.
	q := fmt.Sprintf(`
		SELECT def_id, tenant_id, name, version, definition, created_at
		FROM (
			SELECT d.def_id, d.tenant_id, d.name, d.version, d.definition, d.created_at,
			       ROW_NUMBER() OVER (PARTITION BY d.tenant_id, d.name ORDER BY d.version DESC) AS rn
			FROM %s d
			WHERE d.retired = 1
			  AND d.created_at < ?
			  AND NOT EXISTS (SELECT 1 FROM %s a WHERE a.def_id = d.def_id)
			  AND NOT EXISTS (SELECT 1 FROM %s c WHERE c.parent_def_id = d.def_id)
		) sub
		WHERE sub.rn > ?
		ORDER BY sub.tenant_id, sub.name, sub.version DESC
		LIMIT ?`, defsTable, activeTable, defsTable)
	rows, err := s.db.QueryContext(ctx, q, olderThan.UnixNano(), keepLastN, limit)
	if err != nil {
		return nil, fmt.Errorf("retention list %s: %w", defType, err)
	}
	defer rows.Close()

	var out []store.RetiredDefRef
	for rows.Next() {
		ref := store.RetiredDefRef{DefType: defType}
		var definition string
		var createdAt int64
		if err := rows.Scan(&ref.DefID, &ref.TenantID, &ref.Name, &ref.Version, &definition, &createdAt); err != nil {
			return nil, err
		}
		ref.Definition = json.RawMessage(definition)
		ref.CreatedAt = time.Unix(0, createdAt)
		out = append(out, ref)
	}
	return out, rows.Err()
}

// DeleteDefVersions — RFC BM retention hard-delete. See the store interface.
func (s *Store) DeleteDefVersions(ctx context.Context, defType string, defIDs []string) (int, error) {
	defsTable, _, ok := store.RetentionDefTables(defType)
	if !ok {
		return 0, fmt.Errorf("retention: unknown def type %q", defType)
	}
	if len(defIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(defIDs))
	args := make([]any, len(defIDs))
	for i, id := range defIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	// defsTable is allowlisted; the ids are parameterized.
	q := fmt.Sprintf(`DELETE FROM %s WHERE def_id IN (%s)`, defsTable, strings.Join(placeholders, ", "))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("retention delete %s: %w", defType, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// AgentDefSetActive UPSERTs the agent_def_active pointer for
// (tenantID, name). RFC N: validates the def belongs to BOTH the named
// agent AND the supplied tenant — a def can only be promoted within its
// own tenant.
func (s *Store) AgentDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	// Validate def_id exists + matches (name, tenant) (defence-in-depth;
	// the FK isn't enforced without foreign_keys PRAGMA).
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM agent_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("agent_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("agent_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

// AgentDefGetActive returns the active row for (tenantID, name).
// *ErrNotFound when no pointer exists — caller falls through to
// cfg.Agents.
func (s *Store) AgentDefGetActive(ctx context.Context, tenantID, name string) (store.AgentDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM agent_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var name, tenant string
	err = tx.QueryRowContext(ctx, `SELECT name, tenant_id FROM agent_defs WHERE def_id = ?`, defID).Scan(&name, &tenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	); err != nil {
		return err
	}
	if retired {
		// Clear the active pointer ONLY when it points at THIS def — the
		// `def_id = ?` guard leaves a non-active version's pointer alone, so
		// the name becomes reclaimable and runs stop resolving a retired def.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agent_def_active WHERE tenant_id = ? AND name = ? AND def_id = ?`,
			tenant, name, defID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// agentDefSelect is the column list shared by every read. Kept in
// one place so column additions need only a single touch-point.
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
	COALESCE(content_sha256, ''),
	tenant_id
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
		&out.TenantID,
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
			&r.TenantID,
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
		`SELECT MAX(version) FROM skill_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
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
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256), row.TenantID,
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
	// RFC N: names are per-tenant; group by tenant_id so a name owned by
	// N tenants yields N rows (one per tenant) rather than merging them.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                              AS version_count,
			COUNT(*) FILTER (WHERE d.retired = 0) AS live_version_count,
			MAX(d.version)                        AS latest_version,
			MAX(d.created_at)                     AS last_updated,
			COALESCE(a.def_id, '')                AS active_def_id,
			COALESCE(ad.retired, 0)               AS active_retired
		FROM skill_defs d
		LEFT JOIN skill_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		LEFT JOIN skill_defs ad      ON ad.def_id = a.def_id
		GROUP BY d.tenant_id, d.name, a.def_id, ad.retired
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.SkillDefNameSummary
	for rows.Next() {
		var ns store.SkillDefNameSummary
		var updatedAt int64
		var activeRetired int
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LiveVersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID, &activeRetired); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		ns.ActiveRetired = activeRetired != 0
		out = append(out, ns)
	}
	return out, rows.Err()
}

// SkillDefSetActive UPSERTs the skill_def_active pointer for
// (tenantID, name). RFC N: validates the def belongs to BOTH the named
// skill AND the supplied tenant — a def can only be promoted within its
// own tenant.
func (s *Store) SkillDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM skill_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "skill_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("skill_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("skill_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO skill_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) SkillDefGetActive(ctx context.Context, tenantID, name string) (store.SkillDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM skill_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
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
	COALESCE(content_sha256, ''),
	tenant_id
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
		&out.TenantID,
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
			&r.TenantID,
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

// ---- TeamDef substrate ----
//
// Direct mirror of the SkillDef methods above. Same per-name lock
// for version monotonicity, same scan/select helpers, same error
// surfaces. The only divergence is the table name and the typed
// error (ErrTeamDefParentNotFound instead of ErrSkillDefParentNotFound).
// If you fix a bug in one of these methods, fix it in the SkillDef
// twin as well.

func (s *Store) TeamDefCreate(ctx context.Context, row store.TeamDefRow) (store.TeamDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.TeamDefRow{}, fmt.Errorf("team_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.TeamDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.TeamDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM teamdefs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.TeamDefRow{}, err
		}
		if n == 0 {
			return store.TeamDefRow{}, store.ErrTeamDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM teamdefs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
	).Scan(&maxVer); err != nil {
		return store.TeamDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO teamdefs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256), row.TenantID,
	); err != nil {
		return store.TeamDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.TeamDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) TeamDefGet(ctx context.Context, defID string) (store.TeamDefRow, error) {
	row, err := s.scanTeamDef(s.db.QueryRowContext(ctx, teamDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.TeamDefRow{}, &store.ErrNotFound{Kind: "team_def", ID: defID}
	}
	return row, err
}

func (s *Store) TeamDefGetByNameVersion(ctx context.Context, name string, version int) (store.TeamDefRow, error) {
	row, err := s.scanTeamDef(s.db.QueryRowContext(ctx, teamDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.TeamDefRow{}, &store.ErrNotFound{Kind: "team_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) TeamDefListByName(ctx context.Context, name string) ([]store.TeamDefRow, error) {
	rows, err := s.db.QueryContext(ctx, teamDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanTeamDefRows(rows)
}

func (s *Store) TeamDefListChildren(ctx context.Context, parentDefID string) ([]store.TeamDefRow, error) {
	rows, err := s.db.QueryContext(ctx, teamDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanTeamDefRows(rows)
}

func (s *Store) TeamDefListNames(ctx context.Context) ([]store.TeamDefNameSummary, error) {
	// RFC N: names are per-tenant; group by tenant_id so a name owned by
	// N tenants yields N rows (one per tenant) rather than merging them.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                              AS version_count,
			COUNT(*) FILTER (WHERE d.retired = 0) AS live_version_count,
			MAX(d.version)                        AS latest_version,
			MAX(d.created_at)                     AS last_updated,
			COALESCE(a.def_id, '')                AS active_def_id,
			COALESCE(ad.retired, 0)               AS active_retired
		FROM teamdefs d
		LEFT JOIN teamdef_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		LEFT JOIN teamdefs ad      ON ad.def_id = a.def_id
		GROUP BY d.tenant_id, d.name, a.def_id, ad.retired
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.TeamDefNameSummary
	for rows.Next() {
		var ns store.TeamDefNameSummary
		var updatedAt int64
		var activeRetired int
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LiveVersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID, &activeRetired); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		ns.ActiveRetired = activeRetired != 0
		out = append(out, ns)
	}
	return out, rows.Err()
}

// TeamDefSetActive UPSERTs the teamdef_active pointer for
// (tenantID, name). RFC N: validates the def belongs to BOTH the named
// team AND the supplied tenant — a def can only be promoted within its
// own tenant.
func (s *Store) TeamDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM teamdefs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "team_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("teamdef_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("teamdef_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO teamdef_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) TeamDefGetActive(ctx context.Context, tenantID, name string) (store.TeamDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM teamdef_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.TeamDefRow{}, &store.ErrNotFound{Kind: "teamdef_active", ID: name}
	}
	if err != nil {
		return store.TeamDefRow{}, err
	}
	return s.TeamDefGet(ctx, defID)
}

// TeamDefDelete hard-deletes every version of (tenantID, name) and its active
// pointer in one transaction (mirrors DynamicAgentDelete). Returns whether any
// version row was removed.
func (s *Store) TeamDefDelete(ctx context.Context, tenantID, name string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("teamdef delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM teamdef_active WHERE tenant_id = ? AND name = ?`, tenantID, name); err != nil {
		return false, fmt.Errorf("teamdef delete active: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM teamdefs WHERE tenant_id = ? AND name = ?`, tenantID, name)
	if err != nil {
		return false, fmt.Errorf("teamdef delete rows: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("teamdef delete: commit: %w", err)
	}
	return n > 0, nil
}

func (s *Store) TeamDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE teamdefs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "team_def", ID: defID}
	}
	return nil
}

const teamDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	COALESCE(content_sha256, ''),
	tenant_id
FROM teamdefs`

func (s *Store) scanTeamDef(row *sql.Row) (store.TeamDefRow, error) {
	var (
		out        store.TeamDefRow
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
		&out.TenantID,
	)
	if err != nil {
		return store.TeamDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanTeamDefRows(rows *sql.Rows) ([]store.TeamDefRow, error) {
	var out []store.TeamDefRow
	for rows.Next() {
		var (
			r          store.TeamDefRow
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
			&r.TenantID,
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

	// RFC N: version allocation is scoped per (tenant, name) so two
	// tenants' v1 don't collide on UNIQUE(tenant_id, name, version).
	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM mcp_server_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
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
			retired, bootstrapped_from_static, content_sha256, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
		nilIfEmpty(row.ContentSHA256), row.TenantID,
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
			d.tenant_id,
			d.name,
			COUNT(*)                              AS version_count,
			COUNT(*) FILTER (WHERE d.retired = 0) AS live_version_count,
			MAX(d.version)                        AS latest_version,
			MAX(d.created_at)                     AS last_updated,
			COALESCE(a.def_id, '')                AS active_def_id,
			COALESCE(ad.retired, 0)               AS active_retired
		FROM mcp_server_defs d
		LEFT JOIN mcp_server_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		LEFT JOIN mcp_server_defs ad      ON ad.def_id = a.def_id
		GROUP BY d.tenant_id, d.name, a.def_id, ad.retired
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.MCPServerDefNameSummary
	for rows.Next() {
		var ns store.MCPServerDefNameSummary
		var updatedAt int64
		var activeRetired int
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LiveVersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID, &activeRetired); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		ns.ActiveRetired = activeRetired != 0
		out = append(out, ns)
	}
	return out, rows.Err()
}

// MCPServerDefSetActive UPSERTs the active pointer for (tenantID, name).
// RFC N: validates the def belongs to BOTH the named server AND the
// supplied tenant — a def can only be promoted within its own tenant.
func (s *Store) MCPServerDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM mcp_server_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "mcp_server_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("mcp_server_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("mcp_server_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mcp_server_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

// MCPServerDefGetActive returns the active row for (tenantID, name).
// *ErrNotFound when no pointer exists. RFC N: tenantID "" = the shared/
// operator/legacy tenant.
func (s *Store) MCPServerDefGetActive(ctx context.Context, tenantID, name string) (store.MCPServerDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM mcp_server_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
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
	COALESCE(content_sha256, ''),
	tenant_id
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
		&out.TenantID,
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
			&r.TenantID,
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
		`SELECT MAX(version) FROM schedule_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
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
			retired, bootstrapped_from_static, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic), row.TenantID,
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
			d.tenant_id,
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM schedule_defs d
		LEFT JOIN schedule_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		GROUP BY d.tenant_id, d.name, a.def_id
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.ScheduleDefNameSummary
	for rows.Next() {
		var ns store.ScheduleDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// ScheduleDefSetActive UPSERTs the schedule_def_active pointer for
// (tenantID, name). RFC N: validates the def belongs to BOTH the named
// schedule AND the supplied tenant.
func (s *Store) ScheduleDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM schedule_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "schedule_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("schedule_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("schedule_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO schedule_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) ScheduleDefGetActive(ctx context.Context, tenantID, name string) (store.ScheduleDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM schedule_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
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
	bootstrapped_from_static,
	tenant_id
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
		&out.TenantID,
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
			&r.TenantID,
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

// ---- v1.x RFC G A2AServerCardDef substrate ----
//
// Mirror of ScheduleDef* without the sweeper run_state table.

func (s *Store) A2AServerCardDefCreate(ctx context.Context, row store.A2AServerCardDefRow) (store.A2AServerCardDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.A2AServerCardDefRow{}, fmt.Errorf("a2a_server_card_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM a2a_server_card_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.A2AServerCardDefRow{}, err
		}
		if n == 0 {
			return store.A2AServerCardDefRow{}, store.ErrA2AServerCardDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM a2a_server_card_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
	).Scan(&maxVer); err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO a2a_server_card_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic), row.TenantID,
	); err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) A2AServerCardDefGet(ctx context.Context, defID string) (store.A2AServerCardDefRow, error) {
	row, err := s.scanA2AServerCardDef(s.db.QueryRowContext(ctx, a2aServerCardDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def", ID: defID}
	}
	return row, err
}

func (s *Store) A2AServerCardDefGetByNameVersion(ctx context.Context, name string, version int) (store.A2AServerCardDefRow, error) {
	row, err := s.scanA2AServerCardDef(s.db.QueryRowContext(ctx, a2aServerCardDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) A2AServerCardDefListByName(ctx context.Context, name string) ([]store.A2AServerCardDefRow, error) {
	rows, err := s.db.QueryContext(ctx, a2aServerCardDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanA2AServerCardDefRows(rows)
}

func (s *Store) A2AServerCardDefListChildren(ctx context.Context, parentDefID string) ([]store.A2AServerCardDefRow, error) {
	rows, err := s.db.QueryContext(ctx, a2aServerCardDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanA2AServerCardDefRows(rows)
}

func (s *Store) A2AServerCardDefListNames(ctx context.Context) ([]store.A2AServerCardDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM a2a_server_card_defs d
		LEFT JOIN a2a_server_card_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		GROUP BY d.tenant_id, d.name, a.def_id
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.A2AServerCardDefNameSummary
	for rows.Next() {
		var ns store.A2AServerCardDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// A2AServerCardDefSetActive UPSERTs the a2a_server_card_def_active pointer
// for (tenantID, name). RFC N: validates the def belongs to BOTH the named
// card AND the supplied tenant.
func (s *Store) A2AServerCardDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM a2a_server_card_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "a2a_server_card_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("a2a_server_card_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("a2a_server_card_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO a2a_server_card_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) A2AServerCardDefGetActive(ctx context.Context, tenantID, name string) (store.A2AServerCardDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM a2a_server_card_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def_active", ID: name}
	}
	if err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	return s.A2AServerCardDefGet(ctx, defID)
}

func (s *Store) A2AServerCardDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE a2a_server_card_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "a2a_server_card_def", ID: defID}
	}
	return nil
}

const a2aServerCardDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	tenant_id
FROM a2a_server_card_defs`

func (s *Store) scanA2AServerCardDef(row *sql.Row) (store.A2AServerCardDefRow, error) {
	var (
		out        store.A2AServerCardDefRow
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
		&out.TenantID,
	)
	if err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanA2AServerCardDefRows(rows *sql.Rows) ([]store.A2AServerCardDefRow, error) {
	var out []store.A2AServerCardDefRow
	for rows.Next() {
		var (
			r          store.A2AServerCardDefRow
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
			&r.TenantID,
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

// ---- v1.x RFC G A2AAgentDef substrate ----
//
// Mirror of ScheduleDef* without the sweeper run_state table.

func (s *Store) A2AAgentDefCreate(ctx context.Context, row store.A2AAgentDefRow) (store.A2AAgentDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.A2AAgentDefRow{}, fmt.Errorf("a2a_agent_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.A2AAgentDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.A2AAgentDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM a2a_agent_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.A2AAgentDefRow{}, err
		}
		if n == 0 {
			return store.A2AAgentDefRow{}, store.ErrA2AAgentDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM a2a_agent_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
	).Scan(&maxVer); err != nil {
		return store.A2AAgentDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO a2a_agent_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic), row.TenantID,
	); err != nil {
		return store.A2AAgentDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.A2AAgentDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) A2AAgentDefGet(ctx context.Context, defID string) (store.A2AAgentDefRow, error) {
	row, err := s.scanA2AAgentDef(s.db.QueryRowContext(ctx, a2aAgentDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.A2AAgentDefRow{}, &store.ErrNotFound{Kind: "a2a_agent_def", ID: defID}
	}
	return row, err
}

func (s *Store) A2AAgentDefGetByNameVersion(ctx context.Context, name string, version int) (store.A2AAgentDefRow, error) {
	row, err := s.scanA2AAgentDef(s.db.QueryRowContext(ctx, a2aAgentDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.A2AAgentDefRow{}, &store.ErrNotFound{Kind: "a2a_agent_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) A2AAgentDefListByName(ctx context.Context, name string) ([]store.A2AAgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, a2aAgentDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanA2AAgentDefRows(rows)
}

func (s *Store) A2AAgentDefListChildren(ctx context.Context, parentDefID string) ([]store.A2AAgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, a2aAgentDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanA2AAgentDefRows(rows)
}

func (s *Store) A2AAgentDefListNames(ctx context.Context) ([]store.A2AAgentDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM a2a_agent_defs d
		LEFT JOIN a2a_agent_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		GROUP BY d.tenant_id, d.name, a.def_id
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.A2AAgentDefNameSummary
	for rows.Next() {
		var ns store.A2AAgentDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// A2AAgentDefSetActive UPSERTs the a2a_agent_def_active pointer for
// (tenantID, name). RFC N: validates the def belongs to BOTH the named
// peer AND the supplied tenant.
func (s *Store) A2AAgentDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM a2a_agent_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "a2a_agent_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("a2a_agent_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("a2a_agent_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO a2a_agent_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) A2AAgentDefGetActive(ctx context.Context, tenantID, name string) (store.A2AAgentDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM a2a_agent_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.A2AAgentDefRow{}, &store.ErrNotFound{Kind: "a2a_agent_def_active", ID: name}
	}
	if err != nil {
		return store.A2AAgentDefRow{}, err
	}
	return s.A2AAgentDefGet(ctx, defID)
}

func (s *Store) A2AAgentDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE a2a_agent_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "a2a_agent_def", ID: defID}
	}
	return nil
}

const a2aAgentDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	tenant_id
FROM a2a_agent_defs`

func (s *Store) scanA2AAgentDef(row *sql.Row) (store.A2AAgentDefRow, error) {
	var (
		out        store.A2AAgentDefRow
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
		&out.TenantID,
	)
	if err != nil {
		return store.A2AAgentDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanA2AAgentDefRows(rows *sql.Rows) ([]store.A2AAgentDefRow, error) {
	var out []store.A2AAgentDefRow
	for rows.Next() {
		var (
			r          store.A2AAgentDefRow
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
			&r.TenantID,
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

// ---- v1.x RFC H WebhookDef substrate ----
//
// Mirror of A2AAgentDef* without the sweeper run_state table.

func (s *Store) WebhookDefCreate(ctx context.Context, row store.WebhookDefRow) (store.WebhookDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.WebhookDefRow{}, fmt.Errorf("webhook_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.WebhookDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.WebhookDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.WebhookDefRow{}, err
		}
		if n == 0 {
			return store.WebhookDefRow{}, store.ErrWebhookDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM webhook_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
	).Scan(&maxVer); err != nil {
		return store.WebhookDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO webhook_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic), row.TenantID,
	); err != nil {
		return store.WebhookDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.WebhookDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) WebhookDefGet(ctx context.Context, defID string) (store.WebhookDefRow, error) {
	row, err := s.scanWebhookDef(s.db.QueryRowContext(ctx, webhookDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.WebhookDefRow{}, &store.ErrNotFound{Kind: "webhook_def", ID: defID}
	}
	return row, err
}

func (s *Store) WebhookDefGetByNameVersion(ctx context.Context, name string, version int) (store.WebhookDefRow, error) {
	row, err := s.scanWebhookDef(s.db.QueryRowContext(ctx, webhookDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.WebhookDefRow{}, &store.ErrNotFound{Kind: "webhook_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) WebhookDefListByName(ctx context.Context, name string) ([]store.WebhookDefRow, error) {
	rows, err := s.db.QueryContext(ctx, webhookDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanWebhookDefRows(rows)
}

func (s *Store) WebhookDefListChildren(ctx context.Context, parentDefID string) ([]store.WebhookDefRow, error) {
	rows, err := s.db.QueryContext(ctx, webhookDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanWebhookDefRows(rows)
}

func (s *Store) WebhookDefListNames(ctx context.Context) ([]store.WebhookDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM webhook_defs d
		LEFT JOIN webhook_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		GROUP BY d.tenant_id, d.name, a.def_id
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.WebhookDefNameSummary
	for rows.Next() {
		var ns store.WebhookDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// WebhookDefSetActive UPSERTs the webhook_def_active pointer for
// (tenantID, name). RFC N: validates the def belongs to BOTH the named
// webhook AND the supplied tenant.
func (s *Store) WebhookDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM webhook_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "webhook_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("webhook_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("webhook_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO webhook_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) WebhookDefGetActive(ctx context.Context, tenantID, name string) (store.WebhookDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM webhook_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.WebhookDefRow{}, &store.ErrNotFound{Kind: "webhook_def_active", ID: name}
	}
	if err != nil {
		return store.WebhookDefRow{}, err
	}
	return s.WebhookDefGet(ctx, defID)
}

func (s *Store) WebhookDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhook_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "webhook_def", ID: defID}
	}
	return nil
}

const webhookDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	tenant_id
FROM webhook_defs`

func (s *Store) scanWebhookDef(row *sql.Row) (store.WebhookDefRow, error) {
	var (
		out        store.WebhookDefRow
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
		&out.TenantID,
	)
	if err != nil {
		return store.WebhookDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanWebhookDefRows(rows *sql.Rows) ([]store.WebhookDefRow, error) {
	var out []store.WebhookDefRow
	for rows.Next() {
		var (
			r          store.WebhookDefRow
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
			&r.TenantID,
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

// ---- RFC I MR-3a MemoryBackendDef substrate ----
//
// Faithful mirror of WebhookDef* (which itself mirrors A2AAgentDef*
// without the sweeper run_state table).

func (s *Store) MemoryBackendDefCreate(ctx context.Context, row store.MemoryBackendDefRow) (store.MemoryBackendDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.MemoryBackendDefRow{}, fmt.Errorf("memory_backend_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_backend_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.MemoryBackendDefRow{}, err
		}
		if n == 0 {
			return store.MemoryBackendDefRow{}, store.ErrMemoryBackendDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM memory_backend_defs WHERE tenant_id = ? AND name = ?`, row.TenantID, row.Name,
	).Scan(&maxVer); err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO memory_backend_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic), row.TenantID,
	); err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	committed = true
	return row, nil
}

func (s *Store) MemoryBackendDefGet(ctx context.Context, defID string) (store.MemoryBackendDefRow, error) {
	row, err := s.scanMemoryBackendDef(s.db.QueryRowContext(ctx, memoryBackendDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.MemoryBackendDefRow{}, &store.ErrNotFound{Kind: "memory_backend_def", ID: defID}
	}
	return row, err
}

func (s *Store) MemoryBackendDefGetByNameVersion(ctx context.Context, name string, version int) (store.MemoryBackendDefRow, error) {
	row, err := s.scanMemoryBackendDef(s.db.QueryRowContext(ctx, memoryBackendDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.MemoryBackendDefRow{}, &store.ErrNotFound{Kind: "memory_backend_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

func (s *Store) MemoryBackendDefListByName(ctx context.Context, name string) ([]store.MemoryBackendDefRow, error) {
	rows, err := s.db.QueryContext(ctx, memoryBackendDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanMemoryBackendDefRows(rows)
}

func (s *Store) MemoryBackendDefListChildren(ctx context.Context, parentDefID string) ([]store.MemoryBackendDefRow, error) {
	rows, err := s.db.QueryContext(ctx, memoryBackendDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanMemoryBackendDefRows(rows)
}

func (s *Store) MemoryBackendDefListNames(ctx context.Context) ([]store.MemoryBackendDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.tenant_id,
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM memory_backend_defs d
		LEFT JOIN memory_backend_def_active a ON a.name = d.name AND a.tenant_id = d.tenant_id
		GROUP BY d.tenant_id, d.name, a.def_id
		ORDER BY d.tenant_id, d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.MemoryBackendDefNameSummary
	for rows.Next() {
		var ns store.MemoryBackendDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&ns.TenantID, &ns.Name, &ns.VersionCount, &ns.LatestVersion, &updatedAt, &ns.ActiveDefID); err != nil {
			return nil, err
		}
		ns.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// MemoryBackendDefSetActive UPSERTs the memory_backend_def_active pointer
// for (tenantID, name). RFC N: validates the def belongs to BOTH the named
// backend AND the supplied tenant — a def can only be promoted within its
// own tenant.
func (s *Store) MemoryBackendDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error {
	var (
		rowName   string
		rowTenant string
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, tenant_id FROM memory_backend_defs WHERE def_id = ?`, defID).Scan(&rowName, &rowTenant)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "memory_backend_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("memory_backend_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	if rowTenant != tenantID {
		return fmt.Errorf("memory_backend_def_active: def_id %q belongs to tenant %q, refusing to promote under tenant %q", defID, rowTenant, tenantID)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO memory_backend_def_active (tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		tenantID, name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

func (s *Store) MemoryBackendDefGetActive(ctx context.Context, tenantID, name string) (store.MemoryBackendDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM memory_backend_def_active WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.MemoryBackendDefRow{}, &store.ErrNotFound{Kind: "memory_backend_def_active", ID: name}
	}
	if err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	return s.MemoryBackendDefGet(ctx, defID)
}

func (s *Store) MemoryBackendDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memory_backend_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "memory_backend_def", ID: defID}
	}
	return nil
}

const memoryBackendDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static,
	tenant_id
FROM memory_backend_defs`

func (s *Store) scanMemoryBackendDef(row *sql.Row) (store.MemoryBackendDefRow, error) {
	var (
		out        store.MemoryBackendDefRow
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
		&out.TenantID,
	)
	if err != nil {
		return store.MemoryBackendDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanMemoryBackendDefRows(rows *sql.Rows) ([]store.MemoryBackendDefRow, error) {
	var out []store.MemoryBackendDefRow
	for rows.Next() {
		var (
			r          store.MemoryBackendDefRow
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
			&r.TenantID,
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
		fireCount   int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT def_id, last_run_at, last_run_id, last_status, last_error, next_run_at, paused_until, fire_count
		 FROM schedule_run_state WHERE def_id = ?`, defID,
	).Scan(&out.DefID, &lastRunAt, &lastRunID, &lastStatus, &lastError, &nextRunAt, &pausedUntil, &fireCount)
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
	out.FireCount = fireCount
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
	// fire_count += 1 only on a real fire (CountAsFire); the disabled-skip
	// advance passes false so a disabled schedule keeps its max_fires budget.
	fireInc := 0
	if in.CountAsFire {
		fireInc = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule_run_state SET
			last_run_at = ?,
			last_run_id = ?,
			last_status = ?,
			last_error = ?,
			next_run_at = ?,
			fire_count = fire_count + ?
		 WHERE def_id = ?`,
		in.LastRunAt.UnixNano(), in.LastRunID, in.LastStatus, in.LastError,
		in.NextRunAt.UnixNano(), fireInc, in.DefID,
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
		if err := json.Unmarshal([]byte(dimensions), &out.Dimensions); err != nil {
			// Malformed dimensions JSON (e.g. a hand-edited row) — log + leave
			// Dimensions nil rather than silently dropping the parse error.
			log.Printf("evaluations: scan eval_id=%s: dimensions JSON parse failed, skipping: %v", out.EvalID, err)
		}
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
			if err := json.Unmarshal([]byte(dimensions), &r.Dimensions); err != nil {
				log.Printf("evaluations: scan eval_id=%s: dimensions JSON parse failed, skipping: %v", r.EvalID, err)
			}
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

// nullableInt64 returns nil when p is nil so the SQL driver writes NULL — for
// a genuinely-unset tier, distinct from a zero limit (RFC AW token budgets).
func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
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
		INSERT INTO dynamic_agents (tenant_id, name, definition, created_at, expires_at, description)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			definition  = excluded.definition,
			created_at  = excluded.created_at,
			expires_at  = excluded.expires_at,
			description = excluded.description
	`, a.TenantID, a.Name, a.Definition, createdAt.UnixNano(), expiresAtNS, a.Description)
	if err != nil {
		return fmt.Errorf("dynamic_agents: upsert: %w", err)
	}
	return nil
}

func (s *Store) DynamicAgentGet(ctx context.Context, tenantID, name string) (store.DynamicAgent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tenant_id, name, definition, created_at, expires_at, COALESCE(description, '')
		FROM dynamic_agents
		WHERE tenant_id = ? AND name = ? AND (expires_at = 0 OR expires_at > ?)
	`, tenantID, name, time.Now().UnixNano())

	var a store.DynamicAgent
	var createdAtNS, expiresAtNS int64
	if err := row.Scan(&a.TenantID, &a.Name, &a.Definition, &createdAtNS, &expiresAtNS, &a.Description); err != nil {
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
		SELECT tenant_id, name, definition, created_at, expires_at, COALESCE(description, '')
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
		if err := rows.Scan(&a.TenantID, &a.Name, &a.Definition, &createdAtNS, &expiresAtNS, &a.Description); err != nil {
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

func (s *Store) DynamicAgentDelete(ctx context.Context, tenantID, name string) (bool, error) {
	// RFC N: scope the delete to (tenant_id, name) — a principal must not be
	// able to unregister another tenant's same-named agent (exp7 C1).
	res, err := s.db.ExecContext(ctx, `DELETE FROM dynamic_agents WHERE tenant_id = ? AND name = ?`, tenantID, name)
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
	case store.InterruptStatusTimedOut, store.InterruptStatusCancelled, store.InterruptStatusDeclined:
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

func (s *Store) InterruptListByUser(ctx context.Context, userID, tenantID, statusFilter string) ([]store.InterruptRow, error) {
	// Whole-tenant isolation (RFC L/N): when tenantID is set, JOIN runs and
	// filter the OWNING run's tenant so a caller can't read another tenant's
	// interrupts by guessing a user_id. "" = all tenants (super-admin / open).
	// Columns are aliased with i. for the JOIN; scanInterruptRow reads by
	// position so the SELECT list order is unchanged.
	q := `
		SELECT i.interrupt_id, i.run_id, i.kind, i.status,
		       i.question, i.options, i.context_data, i.priority,
		       i.answer, i.answer_meta,
		       i.created_at, i.expires_at, i.resolved_at, i.resolved_by,
		       i.user_id, i.agent_id, i.agent_name
		FROM interrupts i`
	args := []any{userID}
	if tenantID != "" {
		q += ` JOIN runs r ON r.id = i.run_id`
	}
	q += ` WHERE i.user_id = ?`
	if tenantID != "" {
		q += ` AND r.tenant_id = ?`
		args = append(args, tenantID)
	}
	if statusFilter != "" {
		q += ` AND i.status = ?`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY i.created_at DESC LIMIT 200`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("interrupts: list by user: %w", err)
	}
	defer rows.Close()

	out := []store.InterruptRow{}
	for rows.Next() {
		r, err := scanInterruptRow(rows)
		if err != nil {
			return nil, fmt.Errorf("interrupts: list by user scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
