package sqlite

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// schemaPlan is the statements migrate executes, grouped by pass.
type schemaPlan struct {
	tables  []string
	columns []string
	indexes []string
	fixups  []string
}

// migrationPlan is every statement migrate executes from a fixed list, by
// pass: the tables (with the indexes that need only CREATE-time columns), the
// column adds, the indexes that may name an added column, and the data fixups.
// migrate runs the passes in that order; the backfills and table rebuilds
// between the column adds and the indexes stay in migrate.
func migrationPlan() schemaPlan {
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
			-- RFC DD: the run's own configuration record (opaque JSON).
			-- NULL on a run that overrode nothing.
			run_config            TEXT,
			-- RFC DI: the run's answer (opaque JSON), written with the terminal
			-- status. NULL while running and on a finish with nothing to report.
			result                TEXT,
			input_tokens          INTEGER NOT NULL DEFAULT 0,
			output_tokens         INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
			cost                  REAL,
			cost_currency         TEXT,
			ts                    INTEGER NOT NULL,
			-- RFC DT: how long the call took, in ms. NULL = not measured
			-- (a row from before timing existed, or a phase the driver could
			-- not see) — never "took no time".
			duration_ms           INTEGER,
			ttft_ms               INTEGER,
			load_ms               INTEGER,
			prefill_ms            INTEGER,
			decode_ms             INTEGER,
			queue_ms              INTEGER,
			-- What the call was for (chat | ocr | stt | tts) and, for speech,
			-- how many seconds of audio it transcribed or produced. Mirrors
			-- postgres 0113.
			purpose               TEXT NOT NULL DEFAULT 'chat',
			audio_seconds         REAL NOT NULL DEFAULT 0,
			-- The characters a text-to-speech call sent to its engine; 0 on
			-- every other row. Mirrors postgres 0116.
			input_chars           INTEGER NOT NULL DEFAULT 0,
			-- The model tier the call was resolved through ('pinned' for an
			-- agent that names its model) and the run's user tier; '' when
			-- unknown. Mirrors postgres 0125.
			tier                  TEXT NOT NULL DEFAULT '',
			user_tier             TEXT NOT NULL DEFAULT ''
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
			-- Purpose is part of the key, so an archived day keeps chat, OCR
			-- and STT apart; audio_seconds is summed. On a database created
			-- before it, rebuildWidenedKeys swaps the old six-column key for
			-- this one. Mirrors postgres 0113.
			purpose               TEXT NOT NULL DEFAULT 'chat',
			audio_seconds         REAL NOT NULL DEFAULT 0,
			-- Summed like audio_seconds; the key is unchanged. Mirrors
			-- postgres 0116.
			input_chars           INTEGER NOT NULL DEFAULT 0,
			-- Tier and user tier join the key, so an archived day keeps
			-- them apart; rebuildWidenedKeys swaps an older key for this
			-- one. Mirrors postgres 0125.
			tier                  TEXT NOT NULL DEFAULT '',
			user_tier             TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source, purpose, tier, user_tier)
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
		// Month-to-date usage a snapshot restore carried in, per (tenant, user,
		// month). Budget state only: the tracker adds it to the ledger aggregate,
		// and no usage report reads it. month is the unix-nano UTC month start.
		// Mirrors postgres migration 0088.
		// Timed overrides of an agent's no-trace mode, one per (tenant, agent).
		// until and created_at are unix nanos. Mirrors postgres migration 0102.
		`CREATE TABLE IF NOT EXISTS retention_overrides (
			tenant_id  TEXT    NOT NULL DEFAULT '',
			agent      TEXT    NOT NULL,
			until      INTEGER NOT NULL,
			set_by     TEXT    NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, agent)
		)`,
		// The store-backed audit stream (RFC FZ Part E): one hash chain per
		// tenant, and its head (the append lock's row, plus the anchor a
		// retention prune leaves). ts is unix nanos so the hash reproduces
		// exactly. Mirrors postgres migration 0131.
		`CREATE TABLE IF NOT EXISTS audit_events (
			tenant_id TEXT    NOT NULL,
			seq       INTEGER NOT NULL,
			ts        INTEGER NOT NULL,
			actor     TEXT    NOT NULL DEFAULT '',
			action    TEXT    NOT NULL,
			subject   TEXT    NOT NULL DEFAULT '',
			payload   TEXT    NOT NULL,
			prev_hash TEXT    NOT NULL DEFAULT '',
			hash      TEXT    NOT NULL,
			mac       TEXT    NOT NULL,
			PRIMARY KEY (tenant_id, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS audit_heads (
			tenant_id   TEXT    PRIMARY KEY,
			seq         INTEGER NOT NULL DEFAULT 0,
			hash        TEXT    NOT NULL DEFAULT '',
			mac         TEXT    NOT NULL DEFAULT '',
			anchor_seq  INTEGER NOT NULL DEFAULT 0,
			anchor_hash TEXT    NOT NULL DEFAULT '',
			anchor_mac  TEXT    NOT NULL DEFAULT '',
			anchor_state TEXT   NOT NULL DEFAULT ''
		)`,
		// Web UI login sessions (RFC FA §5), keyed by the SHA-256 of the cookie
		// id; every time is unix nanos, revoked_at NULL = live. Mirrors postgres
		// migration 0110.
		`CREATE TABLE IF NOT EXISTS ui_sessions (
			session_hash        TEXT    PRIMARY KEY,
			tenant_id           TEXT    NOT NULL,
			subject             TEXT    NOT NULL,
			gate                TEXT    NOT NULL,
			amr                 TEXT    NOT NULL DEFAULT '[]',
			cred_kind           TEXT    NOT NULL,
			cred_ref            TEXT    NOT NULL DEFAULT '',
			cred_mac            TEXT    NOT NULL,
			ua_hash             TEXT    NOT NULL DEFAULT '',
			ip_hash             TEXT    NOT NULL DEFAULT '',
			created_at          INTEGER NOT NULL,
			last_seen_at        INTEGER NOT NULL,
			idle_expires_at     INTEGER NOT NULL,
			absolute_expires_at INTEGER NOT NULL,
			revoked_at          INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS usage_carry (
			tenant_id  TEXT    NOT NULL DEFAULT '',
			user_id    TEXT    NOT NULL DEFAULT '',
			month      INTEGER NOT NULL,
			tokens     INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, user_id, month)
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
		// RFC CD Part C: the opt-in memory/document change-data-capture feed.
		// Populated only when LOOMCYCLE_MEMORY_CHANGES_ENABLED (via the CDC store
		// decorator); a monotonic seq is the SSE cursor. Mirrors postgres
		// migration 0070.
		`CREATE TABLE IF NOT EXISTS memory_changes (
			seq         INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id   TEXT NOT NULL,
			change_type TEXT NOT NULL,
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL,
			key         TEXT NOT NULL DEFAULT '',
			chunk_id    TEXT NOT NULL DEFAULT '',
			at          INTEGER NOT NULL  -- unix nano
		)`,
		`CREATE INDEX IF NOT EXISTS memory_changes_by_tenant ON memory_changes(tenant_id, seq)`,
		`CREATE INDEX IF NOT EXISTS memory_changes_by_at ON memory_changes(at)`,
		// RFC CD Part C (push): the persisted per-subscription delivery cursor,
		// so outbound change delivery resumes at-least-once across a restart.
		// Mirrors postgres migration 0071.
		`CREATE TABLE IF NOT EXISTS change_subscription_cursors (
			name       TEXT PRIMARY KEY,
			last_seq   INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL  -- unix nano
		)`,
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
		// tenant_id + the RFC BL columns, and since SQLite cannot rewrite a
		// PK in place, rebuildTenantBlindKeys (tenant_key_rebuild.go) then
		// replaces the table carrying the old (scope, scope_id, key) PK with
		// this CREATE. The runtime upserts' ON CONFLICT(tenant_id, scope,
		// scope_id, key) target the uniq_memory_tenant_scope_scope_id_key
		// index in addIndexes below, which exists on every DB.
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
			observed_at       INTEGER,
			valid_at          INTEGER,
			invalid_at        INTEGER,
			PRIMARY KEY (tenant_id, scope, scope_id, key)
		)`,
		// Provenance, searchable at last. Mirrors postgres migration 0065: partial,
		// because source_session_id is NULL on every row not distilled from a chat
		// and indexing those pays for the majority to serve a query that never asks
		// about them. See MemoryListBySourceSessions for why the direction matters.
		//
		// memory_by_source_session, memory_by_observed_at and memory_by_valid_at are NOT
		// here: every column they name is added by ALTER further down, so on an upgraded
		// DB this block runs before the column exists. They live in addIndexes, after
		// the ALTERs. Only expires_at is part of the original CREATE TABLE, so only its
		// index is safe in this block.
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
			origin            TEXT,
			source_session_id TEXT,
			source_run_id     TEXT,
			created_at        INTEGER NOT NULL,
			drained_at        INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS memory_pending_by_target ON memory_pending(tenant_id, scope, scope_id, drained_at)`,
		// The operator-layer consolidation fan-out and the snapshot read ask for
		// undrained rows under one scope with NO tenant, which the by-target index
		// cannot serve. Partial, so it stays the size of the live queue, not of
		// its history. Mirrors Postgres migration 0089. Every column it names is
		// in the CREATE TABLE above, so it is safe in this block on an upgraded DB.
		`CREATE INDEX IF NOT EXISTS memory_pending_undrained_by_scope ON memory_pending(scope, created_at) WHERE drained_at IS NULL`,
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
		// tenant_id (leading the PK) isolates same-named agents/users
		// across tenants — same rationale + SQLite upgrade path as the
		// memory table above (an UPGRADED DB's old tenant-blind PK is
		// replaced by rebuildTenantBlindKeys).
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
			tenant_id            TEXT    NOT NULL DEFAULT '',
			origin               TEXT    NOT NULL DEFAULT '',
			hook_tenant          TEXT,
			requested_visible_at INTEGER,
			PRIMARY KEY (tenant_id, channel, scope, scope_id, id)
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
			tenant_id  TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, channel, scope, scope_id)
		)`,
		// Channel hooks: a hook chain's durable progress and lease on one
		// message awaiting its channel's hooks. Mirrors postgres migration
		// 0084. Advisory — the message's visible_at says whether it still
		// awaits a decision.
		`CREATE TABLE IF NOT EXISTS channel_hook_state (
			tenant_id       TEXT    NOT NULL,
			channel         TEXT    NOT NULL,
			scope           TEXT    NOT NULL,
			scope_id        TEXT    NOT NULL,
			id              TEXT    NOT NULL,
			run_id          TEXT    NOT NULL DEFAULT '',
			chain_pos       INTEGER NOT NULL DEFAULT 0,
			body            TEXT,
			journal         TEXT,
			attempts        INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error      TEXT    NOT NULL DEFAULT '',
			lease_owner     TEXT    NOT NULL DEFAULT '',
			lease_until     INTEGER NOT NULL DEFAULT 0,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, channel, scope, scope_id, id)
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
			name         TEXT    NOT NULL,
			description  TEXT    NOT NULL DEFAULT '',
			scope        TEXT    NOT NULL,
			semantic     TEXT    NOT NULL,
			default_ttl  INTEGER NOT NULL DEFAULT 0,
			max_messages INTEGER NOT NULL DEFAULT 0,
			publisher    TEXT    NOT NULL DEFAULT '',
			period       TEXT    NOT NULL DEFAULT '',
			hold         INTEGER NOT NULL DEFAULT 0,
			hooks        TEXT,
			created_at   INTEGER NOT NULL,
			tenant_id    TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, name)
		)`,
		// RFC BX Phase 2 / P2a — the tenant-owned first-class users table.
		// Mirrors postgres migration 0068. A "user" was previously derived
		// from runs.user_id (no row to write); this makes identity a real,
		// tenant-owned record a substrate:tenant operator can manage.
		// access_mode is the collaboration dial ('tenant' = whole-tenant
		// collaboration, the default preserving today's behaviour; 'isolated'
		// = sandboxed); status is 'active' | 'disabled'. unix-nano created_at
		// stamped in code (sqlite has no now() default idiom here).
		`CREATE TABLE IF NOT EXISTS users (
			tenant_id    TEXT    NOT NULL DEFAULT '',
			subject      TEXT    NOT NULL,
			display_name TEXT    NOT NULL DEFAULT '',
			access_mode  TEXT    NOT NULL DEFAULT 'tenant',
			status       TEXT    NOT NULL DEFAULT 'active',
			created_at   INTEGER NOT NULL,
			created_by   TEXT,
			PRIMARY KEY (tenant_id, subject)
		)`,
		// RFC EY access control — see internal/store/postgres/migrations/
		// 0109_acl.up.sql for the design. Flat (tenant_id, name) rows with
		// a version bumped on every write; retire is reversible; groups
		// nest via member_kind='group'; one acl_version row per tenant is
		// bumped in the same transaction as every write.
		`CREATE TABLE IF NOT EXISTS acl_defs (
			tenant_id       TEXT    NOT NULL DEFAULT '',
			name            TEXT    NOT NULL,
			version         INTEGER NOT NULL DEFAULT 1,
			content_sha256  TEXT    NOT NULL,
			catalog_version INTEGER NOT NULL,
			rules           TEXT    NOT NULL,
			description     TEXT    NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL,
			created_by      TEXT    NOT NULL DEFAULT '',
			retired_at      INTEGER,
			PRIMARY KEY (tenant_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS user_groups (
			tenant_id   TEXT    NOT NULL DEFAULT '',
			name        TEXT    NOT NULL,
			description TEXT    NOT NULL DEFAULT '',
			external_id TEXT    NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			created_by  TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS user_group_members (
			tenant_id   TEXT    NOT NULL DEFAULT '',
			group_name  TEXT    NOT NULL,
			member_kind TEXT    NOT NULL,
			member_id   TEXT    NOT NULL,
			added_at    INTEGER NOT NULL,
			added_by    TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, group_name, member_kind, member_id)
		)`,
		`CREATE INDEX IF NOT EXISTS user_group_members_by_member ON user_group_members(tenant_id, member_kind, member_id)`,
		`CREATE TABLE IF NOT EXISTS acl_bindings (
			binding_id   TEXT    PRIMARY KEY,
			tenant_id    TEXT    NOT NULL DEFAULT '',
			acl_name     TEXT    NOT NULL,
			subject_kind TEXT    NOT NULL,
			subject_id   TEXT    NOT NULL DEFAULT '',
			expires_at   INTEGER,
			created_at   INTEGER NOT NULL,
			created_by   TEXT    NOT NULL DEFAULT '',
			retired_at   INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS acl_bindings_by_subject ON acl_bindings(tenant_id, subject_kind, subject_id) WHERE retired_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS acl_version (
			tenant_id  TEXT    PRIMARY KEY,
			version    INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// RFC FA P2 password gate. Mirrors postgres migration 0111 (see
		// 0111_password_gate.up.sql for the design): logins → users, and the
		// gate's hashed / peppered / sealed material only. Unix-nano INTEGER
		// timestamps, NULL = unset; INTEGER 0/1 booleans.
		`CREATE TABLE IF NOT EXISTS user_identities (
			tenant_id        TEXT    NOT NULL DEFAULT '',
			subject          TEXT    NOT NULL,
			gate             TEXT    NOT NULL,
			issuer           TEXT    NOT NULL DEFAULT '',
			external_subject TEXT    NOT NULL,
			email            TEXT    NOT NULL DEFAULT '',
			email_verified   INTEGER NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL,
			last_login_at    INTEGER,
			PRIMARY KEY (gate, issuer, external_subject),
			UNIQUE (tenant_id, subject, gate)
		)`,
		`CREATE TABLE IF NOT EXISTS user_credentials (
			tenant_id    TEXT    NOT NULL DEFAULT '',
			subject      TEXT    NOT NULL,
			kind         TEXT    NOT NULL,
			phc          TEXT    NOT NULL,
			pepper_kid   TEXT    NOT NULL DEFAULT '',
			updated_at   INTEGER NOT NULL,
			failed_count INTEGER NOT NULL DEFAULT 0,
			locked_until INTEGER,
			PRIMARY KEY (tenant_id, subject, kind)
		)`,
		`CREATE TABLE IF NOT EXISTS user_totp (
			tenant_id    TEXT    NOT NULL DEFAULT '',
			subject      TEXT    NOT NULL,
			sealed       TEXT    NOT NULL,
			created_at   INTEGER NOT NULL,
			confirmed_at INTEGER,
			last_step    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (tenant_id, subject)
		)`,
		`CREATE TABLE IF NOT EXISTS user_recovery_codes (
			tenant_id  TEXT    NOT NULL DEFAULT '',
			subject    TEXT    NOT NULL,
			code_hash  TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			used_at    INTEGER,
			PRIMARY KEY (tenant_id, subject, code_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS auth_invites (
			invite_id        TEXT    PRIMARY KEY,
			code_hash        TEXT    NOT NULL UNIQUE,
			gate             TEXT    NOT NULL,
			tenant_id        TEXT    NOT NULL DEFAULT '',
			subject          TEXT    NOT NULL DEFAULT '',
			access_mode      TEXT    NOT NULL DEFAULT '',
			groups           TEXT    NOT NULL DEFAULT '[]',
			login            TEXT    NOT NULL DEFAULT '',
			email            TEXT    NOT NULL DEFAULT '',
			return_to        TEXT    NOT NULL DEFAULT '',
			bootstrap        INTEGER NOT NULL DEFAULT 0,
			issued_by_manager INTEGER NOT NULL DEFAULT 0,
			created_by       TEXT    NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			expires_at       INTEGER NOT NULL,
			redeemed_at      INTEGER,
			redeemed_subject TEXT    NOT NULL DEFAULT '',
			revoked_at       INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS auth_invites_by_gate_tenant ON auth_invites(gate, tenant_id)`,
		`CREATE TABLE IF NOT EXISTS auth_resets (
			reset_id   TEXT    PRIMARY KEY,
			code_hash  TEXT    NOT NULL UNIQUE,
			gate       TEXT    NOT NULL,
			tenant_id  TEXT    NOT NULL DEFAULT '',
			subject    TEXT    NOT NULL,
			issued_by_manager INTEGER NOT NULL DEFAULT 0,
			created_by TEXT    NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			used_at    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS auth_resets_by_subject ON auth_resets(tenant_id, subject)`,
		// A tenant that has ever had a user, never deleted (postgres 0111
		// explains why). The backfill is idempotent and runs every open, so
		// a database upgraded across this release marks its tenants too.
		`CREATE TABLE IF NOT EXISTS tenant_bootstraps (
			tenant_id TEXT    PRIMARY KEY,
			at        INTEGER NOT NULL,
			via       TEXT    NOT NULL DEFAULT ''
		)`,
		`INSERT OR IGNORE INTO tenant_bootstraps (tenant_id, at, via)
			SELECT DISTINCT tenant_id, 0, 'backfill' FROM users`,
		`CREATE TABLE IF NOT EXISTS auth_throttle (
			throttle_key  TEXT    PRIMARY KEY,
			failures      INTEGER NOT NULL DEFAULT 0,
			window_start  INTEGER NOT NULL,
			window_end    INTEGER NOT NULL,
			blocked_until INTEGER
		)`,
		// RFC FA P1: started redirect logins (OIDC / OAuth code flows).
		// Mirrors postgres migration 0112 (see its comment for the design).
		`CREATE TABLE IF NOT EXISTS auth_login_states (
			state_hash        TEXT    PRIMARY KEY,
			gate              TEXT    NOT NULL,
			binding_hash      TEXT    NOT NULL,
			purpose           TEXT    NOT NULL DEFAULT 'login',
			return_to         TEXT    NOT NULL DEFAULT '',
			reauth            INTEGER NOT NULL DEFAULT 0,
			link_tenant       TEXT    NOT NULL DEFAULT '',
			link_subject      TEXT    NOT NULL DEFAULT '',
			link_session_hash TEXT    NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			expires_at        INTEGER NOT NULL,
			consumed_at       INTEGER
		)`,
		// RFC FA P4 external gates. Mirrors postgres migration 0115 (see
		// 0115_auth_external_gates.up.sql): the shared assertion replay
		// cache and the user removal stage machine.
		`CREATE TABLE IF NOT EXISTS auth_assertion_jtis (
			gate       TEXT    NOT NULL,
			jti        TEXT    NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY (gate, jti)
		)`,
		`CREATE INDEX IF NOT EXISTS auth_assertion_jtis_by_expiry ON auth_assertion_jtis(expires_at)`,
		`CREATE TABLE IF NOT EXISTS auth_removals (
			tenant_id     TEXT    NOT NULL DEFAULT '',
			subject       TEXT    NOT NULL,
			gate          TEXT    NOT NULL DEFAULT '',
			reason        TEXT    NOT NULL,
			disabled_at   INTEGER NOT NULL,
			unbind_due_at INTEGER NOT NULL,
			erase_due_at  INTEGER,
			unbound_at    INTEGER,
			erased_at     INTEGER,
			created_by    TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, subject)
		)`,
		`CREATE INDEX IF NOT EXISTS auth_removals_by_unbind_due ON auth_removals(unbind_due_at) WHERE unbound_at IS NULL`,
		// RFC FA P5 device login and per-tenant gate policy. Mirrors
		// postgres migration 0118 (see 0118_device_gate.up.sql for the
		// design); times are unix nanos, interval_ms milliseconds.
		`CREATE TABLE IF NOT EXISTS device_authorizations (
			device_code_hash TEXT    PRIMARY KEY,
			user_code_hash   TEXT    NOT NULL UNIQUE,
			client_id        TEXT    NOT NULL,
			source_addr      TEXT    NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			expires_at       INTEGER NOT NULL,
			interval_ms      INTEGER NOT NULL,
			last_polled_at   INTEGER,
			last_poll_early  INTEGER NOT NULL DEFAULT 0,
			status           TEXT    NOT NULL DEFAULT 'pending',
			decided_tenant   TEXT    NOT NULL DEFAULT '',
			decided_subject  TEXT    NOT NULL DEFAULT '',
			decided_gate     TEXT    NOT NULL DEFAULT '',
			decided_at       INTEGER,
			decided_session  TEXT    NOT NULL DEFAULT '',
			decided_cred_mac TEXT    NOT NULL DEFAULT '',
			decided_not_after INTEGER,
			redeemed_at      INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS device_authorizations_by_expiry ON device_authorizations(expires_at)`,
		`CREATE TABLE IF NOT EXISTS tenant_auth_policies (
			tenant_id  TEXT    PRIMARY KEY,
			gates      TEXT    NOT NULL,
			updated_at INTEGER NOT NULL,
			updated_by TEXT    NOT NULL DEFAULT ''
		)`,
		// RFC FL P0 app sign-in. Mirrors postgres migration 0123 (see
		// 0123_oauth_app_sign_in.up.sql): authorization codes, refresh-token
		// families and their refresh tokens — hashes only.
		`CREATE TABLE IF NOT EXISTS oauth_codes (
			code_hash      TEXT    PRIMARY KEY,
			client_id      TEXT    NOT NULL,
			redirect_uri   TEXT    NOT NULL,
			code_challenge TEXT    NOT NULL,
			tenant_id      TEXT    NOT NULL,
			subject        TEXT    NOT NULL,
			gate           TEXT    NOT NULL,
			amr            TEXT    NOT NULL DEFAULT '[]',
			session_hash   TEXT    NOT NULL DEFAULT '',
			scope_cap      TEXT    NOT NULL DEFAULT '[]',
			restrictions   TEXT    NOT NULL DEFAULT '',
			created_at     INTEGER NOT NULL,
			expires_at     INTEGER NOT NULL,
			consumed_at    INTEGER,
			family_id      TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_codes_by_expiry ON oauth_codes(expires_at)`,
		`CREATE INDEX IF NOT EXISTS oauth_codes_by_client ON oauth_codes(client_id) WHERE consumed_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS oauth_refresh_families (
			family_id           TEXT    PRIMARY KEY,
			client_id           TEXT    NOT NULL DEFAULT '',
			tenant_id           TEXT    NOT NULL DEFAULT '',
			subject             TEXT    NOT NULL DEFAULT '',
			gate                TEXT    NOT NULL DEFAULT '',
			amr                 TEXT    NOT NULL DEFAULT '[]',
			scope_cap           TEXT    NOT NULL DEFAULT '[]',
			restrictions        TEXT    NOT NULL DEFAULT '',
			created_at          INTEGER NOT NULL,
			last_used_at        INTEGER NOT NULL,
			idle_expires_at     INTEGER NOT NULL,
			absolute_expires_at INTEGER NOT NULL,
			revoked_at          INTEGER,
			revoke_reason       TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_client ON oauth_refresh_families(client_id) WHERE revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_subject ON oauth_refresh_families(tenant_id, subject)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_expiry ON oauth_refresh_families(absolute_expires_at)`,
		`CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
			token_hash TEXT    PRIMARY KEY,
			family_id  TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			used_at    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_tokens_by_family ON oauth_refresh_tokens(family_id)`,
		// RFC FL P0b runtime app clients. Mirrors postgres migration 0124
		// (see 0124_client_defs.up.sql).
		`CREATE TABLE IF NOT EXISTS client_defs (
			def_id            TEXT    PRIMARY KEY,
			name              TEXT    NOT NULL,
			tenant_id         TEXT    NOT NULL DEFAULT '',
			version           INTEGER NOT NULL,
			definition        TEXT    NOT NULL,
			content_sha256    TEXT    NOT NULL,
			description       TEXT    NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			created_by        TEXT    NOT NULL DEFAULT '',
			created_by_tenant TEXT    NOT NULL DEFAULT '',
			retired_at        INTEGER,
			UNIQUE (name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS client_defs_by_tenant ON client_defs(tenant_id)`,
		// RFC FA P6 passkeys, WebAuthn ceremonies and SCIM resources.
		// Mirrors postgres migration 0126 (see 0126_passkeys_scim.up.sql
		// for the design).
		`CREATE TABLE IF NOT EXISTS user_passkeys (
			credential_id      TEXT    PRIMARY KEY,
			tenant_id          TEXT    NOT NULL DEFAULT '',
			subject            TEXT    NOT NULL,
			user_handle        TEXT    NOT NULL,
			public_key         TEXT    NOT NULL,
			attestation_type   TEXT    NOT NULL DEFAULT '',
			attestation_format TEXT    NOT NULL DEFAULT '',
			transports         TEXT    NOT NULL DEFAULT '[]',
			flags              INTEGER NOT NULL DEFAULT 0,
			aaguid             TEXT    NOT NULL DEFAULT '',
			sign_count         INTEGER NOT NULL DEFAULT 0,
			attachment         TEXT    NOT NULL DEFAULT '',
			discoverable       INTEGER NOT NULL DEFAULT 0,
			name               TEXT    NOT NULL DEFAULT '',
			created_at         INTEGER NOT NULL,
			last_used_at       INTEGER,
			flagged_at         INTEGER,
			flag_reason        TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS user_passkeys_by_subject ON user_passkeys(tenant_id, subject)`,
		`CREATE TABLE IF NOT EXISTS webauthn_ceremonies (
			ceremony_hash TEXT    PRIMARY KEY,
			kind          TEXT    NOT NULL,
			gate          TEXT    NOT NULL DEFAULT '',
			tenant_id     TEXT    NOT NULL DEFAULT '',
			subject       TEXT    NOT NULL DEFAULT '',
			binding       TEXT    NOT NULL DEFAULT '',
			session_data  TEXT    NOT NULL,
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS scim_users (
			gate          TEXT    NOT NULL,
			tenant_id     TEXT    NOT NULL,
			scim_id       TEXT    NOT NULL,
			subject       TEXT    NOT NULL,
			user_name     TEXT    NOT NULL,
			user_name_key TEXT    NOT NULL,
			external_id   TEXT    NOT NULL DEFAULT '',
			attributes    TEXT    NOT NULL DEFAULT '{}',
			version       INTEGER NOT NULL DEFAULT 1,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			deleted_at    INTEGER,
			PRIMARY KEY (gate, tenant_id, scim_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS scim_users_live_name ON scim_users(gate, tenant_id, user_name_key) WHERE deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS scim_users_live_subject ON scim_users(gate, tenant_id, subject) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS scim_groups (
			gate         TEXT    NOT NULL,
			tenant_id    TEXT    NOT NULL,
			scim_id      TEXT    NOT NULL,
			display_name TEXT    NOT NULL,
			display_key  TEXT    NOT NULL,
			external_id  TEXT    NOT NULL DEFAULT '',
			version      INTEGER NOT NULL DEFAULT 1,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (gate, tenant_id, scim_id),
			UNIQUE (gate, tenant_id, display_key)
		)`,
		`CREATE TABLE IF NOT EXISTS scim_group_members (
			gate      TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			group_id  TEXT NOT NULL,
			subject   TEXT NOT NULL,
			PRIMARY KEY (gate, tenant_id, group_id, subject)
		)`,
		`CREATE INDEX IF NOT EXISTS scim_group_members_by_subject ON scim_group_members(gate, tenant_id, subject)`,
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
			operator_authored         INTEGER NOT NULL DEFAULT 0,
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
		// PRIMARY KEY in place, so rebuildTenantBlindKeys
		// (tenant_key_rebuild.go) then replaces a table still carrying
		// PK(name) with this CREATE (Postgres upgrades in place via
		// migration 0037). The other def-plane tables that predate the
		// tenant axis follow the same upgrade path.
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
		// CREATE applies the composite PK. See the agent_def_active note for
		// the SQLite upgrade path.
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
			operator_authored         INTEGER NOT NULL DEFAULT 0,
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_name   ON teamdefs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_parent ON teamdefs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_run    ON teamdefs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// teamdefs_by_content_sha256 — see agent_defs_by_content_sha256 note above.
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// — two tenants own the same name independently. On a FRESH DB this
		// CREATE applies the composite PK (teamdefs was born tenant-scoped,
		// so no SQLite upgrade path applies).
		`CREATE TABLE IF NOT EXISTS teamdef_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES teamdefs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			promoter_operator_key_restricted INTEGER,
			promoter_isolated                INTEGER,
			promoter_acl_owner               TEXT,
			PRIMARY KEY(tenant_id, name)
		)`,
		// A team's own webhooks, armed by running walks: one lease per
		// (tenant, team, webhook, walk run), so any replica can accept a
		// delivery. The primary key serves the receiver's (tenant, team,
		// name) lookup and a walk's (tenant, team, walk) delete. Instants are
		// unix nanos. Mirrors postgres migration 0092.
		`CREATE TABLE IF NOT EXISTS team_webhook_arms (
			tenant_id   TEXT    NOT NULL DEFAULT '',
			team        TEXT    NOT NULL,
			name        TEXT    NOT NULL,
			walk_run_id TEXT    NOT NULL,
			def_id      TEXT    NOT NULL,
			user_id     TEXT    NOT NULL DEFAULT '',
			armed_at    INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL,
			PRIMARY KEY(tenant_id, team, name, walk_run_id)
		)`,
		// Accepted deliveries of webhooks that start no run, by dedup key,
		// until expires_at (unix nanos). Mirrors postgres migration 0093.
		`CREATE TABLE IF NOT EXISTS webhook_deliveries (
			delivery_key TEXT    PRIMARY KEY,
			expires_at   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS webhook_deliveries_by_expires_at ON webhook_deliveries(expires_at)`,
		// How a webhook definition's deliveries ended, by outcome, summed over
		// every replica. last_at is unix nanos. Mirrors postgres migration 0105.
		`CREATE TABLE IF NOT EXISTS webhook_delivery_counts (
			tenant_id TEXT    NOT NULL,
			name      TEXT    NOT NULL,
			static    INTEGER NOT NULL,
			outcome   TEXT    NOT NULL,
			reason    TEXT    NOT NULL,
			count     INTEGER NOT NULL,
			last_at   INTEGER NOT NULL,
			PRIMARY KEY(tenant_id, name, static, outcome, reason)
		)`,
		// HookDef substrate — mirror of teamdefs minus the two authority
		// columns (bootstrapped_from_static, operator_authored) a HookDef
		// has no use for. A fresh table born tenant-aware, so the
		// composite UNIQUE / PK live in the CREATE and no ALTER or
		// upgrade-path index is needed.
		`CREATE TABLE IF NOT EXISTS hook_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES hook_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			content_sha256            TEXT,
			tenant_id                 TEXT    NOT NULL DEFAULT '',
			UNIQUE(tenant_id, name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS hook_defs_by_name           ON hook_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS hook_defs_by_parent         ON hook_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS hook_defs_by_run            ON hook_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS hook_defs_by_content_sha256 ON hook_defs(content_sha256) WHERE content_sha256 IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS hook_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES hook_defs(def_id),
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
		// — two tenants own the same name independently. See the
		// agent_def_active note earlier for the SQLite upgrade path.
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
		// on a FRESH DB. See the agent_def_active note for the SQLite
		// upgrade path.
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
			fire_count      INTEGER NOT NULL DEFAULT 0,
			slot_at         INTEGER,
			claimed_by      TEXT,
			claimed_at      INTEGER,
			finished_at     INTEGER,
			catch_up_until  INTEGER,
			missed_slots    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_run_state_due ON schedule_run_state(next_run_at)`,
		// RFC DZ: one row per started, unfinished scheduled run. Whoever deletes
		// a run's row finishes it (records the outcome, dispatches its hooks),
		// so a run is finished once however the finishers race.
		`CREATE TABLE IF NOT EXISTS schedule_active_runs (
			run_id     TEXT PRIMARY KEY,
			def_id     TEXT NOT NULL,
			slot_at    INTEGER NOT NULL,
			catch_up   INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL,
			claimed_by TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_active_runs_def ON schedule_active_runs(def_id, started_at)`,
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
		// on a FRESH DB. See the agent_def_active note for the SQLite
		// upgrade path.
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
		// on a FRESH DB. See the agent_def_active note for the SQLite
		// upgrade path.
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
		// on a FRESH DB. See the agent_def_active note for the SQLite
		// upgrade path.
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
		// on a FRESH DB. See the agent_def_active note for the SQLite
		// upgrade path.
		`CREATE TABLE IF NOT EXISTS memory_backend_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES memory_backend_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT,
			tenant_id             TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY(tenant_id, name)
		)`,
		// DocumentSourceDef substrate — a faithful structural mirror of
		// memory_backend_defs (identity + lineage + promotion), minus the
		// sweeper-only run_state table. document_source_defs declares a
		// named external document source (kind, connection config, tenancy
		// strategy, fallback). See
		// internal/store/postgres/migrations/0072_document_source_defs.up.sql
		// for the full design rationale.
		`CREATE TABLE IF NOT EXISTS document_source_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES document_source_defs(def_id),
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
		`CREATE INDEX IF NOT EXISTS document_source_defs_by_name   ON document_source_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS document_source_defs_by_parent ON document_source_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS document_source_defs_by_run    ON document_source_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		// RFC N: tenant-scoped active pointer. PRIMARY KEY(tenant_id, name)
		// on a FRESH DB. See the agent_def_active note for the SQLite
		// upgrade path.
		`CREATE TABLE IF NOT EXISTS document_source_def_active (
			name                  TEXT    NOT NULL,
			def_id                TEXT    NOT NULL REFERENCES document_source_defs(def_id),
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
		// token from user B's. Excluded from snapshots. A row under scope
		// 'system' is a marker v1.119.0 wrote for its boot-time token rewrap
		// (a non-secret note, not ciphertext); nothing reads that scope now.
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
		// The reverse of a dirent (RFC FQ): which names a document has. An
		// expression index, so no column and no writer has to keep it current;
		// sqlshared.direntDocumentID must spell the expression identically.
		`CREATE INDEX IF NOT EXISTS dirents_document_id ON dirents (tenant_id, scope, scope_id, json_extract(resource_ref, '$.document_id')) WHERE kind = 'document'`,
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
		// RFC N: tenant-scoped — PRIMARY KEY(tenant_id, name); see the
		// agent_def_active note above for the SQLite upgrade path.
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
		// Channel tenant axis. Idempotent ALTER for existing DBs; the
		// fresh CREATE TABLE above already declares the column (the
		// duplicate-column guard short-circuits these on fresh deploys).
		// SQLite can't rewrite the PK in place, so rebuildTenantBlindKeys
		// replaces an upgraded table's old (channel, scope, scope_id[, id]) /
		// (name) PK once these columns exist.
		`ALTER TABLE channel_messages ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channel_cursors ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channels ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC CY: a held channel stores without delivering until a
		// release. Mirrors postgres migration 0075. Idempotent ALTER for
		// existing DBs; the CREATE TABLE above already declares it.
		`ALTER TABLE channels ADD COLUMN hold INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE channels ADD COLUMN hooks TEXT`,
		// Channel hooks: a message's hook context. Mirrors postgres
		// migration 0084; the CREATE TABLE above already declares them.
		`ALTER TABLE channel_messages ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channel_messages ADD COLUMN hook_tenant TEXT`,
		`ALTER TABLE channel_messages ADD COLUMN requested_visible_at INTEGER`,
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
		// ALTERs add the tenant_id column to the existing tables; the old
		// PRIMARY KEY(name) / UNIQUE(name, version) is then replaced by
		// rebuildTenantBlindKeys (SQLite can't rewrite a key in place).
		// DEFAULT '' backfills existing rows to the shared tenant.
		`ALTER TABLE agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC CY item 8. Default 0 so a legacy row reads as NOT
		// operator-authored — the safe direction: nothing gains authority by
		// having predated the column.
		`ALTER TABLE agent_defs ADD COLUMN operator_authored INTEGER NOT NULL DEFAULT 0`,
		// Same for the TEAM plane: a team node's prompt is authored by the
		// TeamDef, so the widened families inside it are gated on THIS flag
		// and not on the spawned agent's.
		`ALTER TABLE teamdefs ADD COLUMN operator_authored INTEGER NOT NULL DEFAULT 0`,
		// The promoter's confinement on a team's active pointer — what an armed
		// subscription's walks run under. NULLABLE on purpose, no DEFAULT: a
		// pointer promoted before these columns existed must read as "not
		// captured", which the sweep treats as fully confined, never as a 0
		// that would read as unrestricted. See
		// internal/store/postgres/migrations/0087_teamdef_active_promoter.
		`ALTER TABLE teamdef_active ADD COLUMN promoter_operator_key_restricted INTEGER`,
		`ALTER TABLE teamdef_active ADD COLUMN promoter_isolated INTEGER`,
		`ALTER TABLE agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dynamic_agents ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N — tenant-scope the skill definition plane (mirror of the
		// agent ALTERs above), same SQLite upgrade path. DEFAULT ''
		// backfills existing rows to the shared tenant.
		`ALTER TABLE skill_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE skill_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N — tenant-scope the MCP server definition plane (mirror of the
		// agent + skill ALTERs above), same SQLite upgrade path. DEFAULT ''
		// backfills existing rows to the shared tenant.
		`ALTER TABLE mcp_server_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mcp_server_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N (completion) — tenant-scope the remaining definition planes:
		// memory backend / schedule / A2A (server card + agent) / webhook
		// (mirror of the agent + skill + MCP ALTERs above), same SQLite
		// upgrade path. DEFAULT '' backfills existing rows to the shared
		// tenant.
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
		// RFC DZ: the slot each claim takes, and who took it. Idempotent ALTERs
		// for existing DBs; fresh DBs get them in CREATE TABLE.
		`ALTER TABLE schedule_run_state ADD COLUMN slot_at INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN claimed_by TEXT`,
		`ALTER TABLE schedule_run_state ADD COLUMN claimed_at INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN finished_at INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN catch_up_until INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN missed_slots INTEGER NOT NULL DEFAULT 0`,
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
		// RFC BX P2b — the isolated-member confinement bit (0 = unconfined). Stamped
		// at CreateRun from the substrate:user principal; read back on resume/restore
		// so a re-dispatched run keeps its data-scope confinement. 0 on legacy rows
		// (fail-open). See internal/store/postgres/migrations/0069_runs_isolated.
		`ALTER TABLE runs ADD COLUMN isolated INTEGER NOT NULL DEFAULT 0`,
		// RFC DD: the run's configuration record — what this run was STARTED
		// with, so resume restores it instead of re-deriving from a definition
		// that may have changed since. Opaque JSON; NULL on legacy rows, which
		// resume treats exactly as it does today (re-derive).
		// See internal/store/postgres/migrations/0078_runs_run_config.
		`ALTER TABLE runs ADD COLUMN run_config TEXT`,
		// RFC DI: the run's answer, written in the same UPDATE as the terminal
		// status. See internal/store/postgres/migrations/0079_runs_result.
		`ALTER TABLE runs ADD COLUMN result TEXT`,
		// RFC DI D5: a configured run's draft — the raw request that will start
		// it, minus secrets. See internal/store/postgres/migrations/0080_runs_draft.
		`ALTER TABLE runs ADD COLUMN draft TEXT`,
		// The team walk a run is a member of — parent_context's walk_id as an
		// indexable column, backfilled below. See
		// internal/store/postgres/migrations/0086_runs_walk_id.
		`ALTER TABLE runs ADD COLUMN walk_id TEXT`,
		// A webhook delivery's second durable dedup key, beside
		// idempotency_key. NULL on legacy rows + runs with one key. Its
		// partial unique index is created in addIndexes. See
		// internal/store/postgres/migrations/0090_runs_delivery_alt_key.
		`ALTER TABLE runs ADD COLUMN delivery_alt_key TEXT`,
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
		// A run's retention mode ('' = stored as usual, 'none' = content never
		// stored), and its session's. Mirrors postgres migration 0101.
		`ALTER TABLE runs ADD COLUMN retention TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN retention TEXT NOT NULL DEFAULT ''`,
		// RFC EI P3: a queued run's request, so a restart can start it. See
		// internal/store/postgres/migrations/0103_runs_queued_input.
		`ALTER TABLE runs ADD COLUMN queued_input TEXT`,
		// Whether a run may resolve $cred: in its user's own scope (0 = may).
		// Read back on resume, which has no trigger to re-derive it from. See
		// internal/store/postgres/migrations/0106_runs_credentials_no_user_scope.
		`ALTER TABLE runs ADD COLUMN credentials_no_user_scope INTEGER NOT NULL DEFAULT 0`,
		// Whether the run's admission took a slot in the shared per-user
		// counter; NULL on rows from before the column and on restored rows.
		// See internal/store/postgres/migrations/0108_runs_user_slot.
		`ALTER TABLE runs ADD COLUMN user_slot INTEGER`,
		// The OpenTelemetry trace the run's span was recorded in ('' when
		// tracing was off or unsampled). See
		// internal/store/postgres/migrations/0132_runs_trace_id.
		`ALTER TABLE runs ADD COLUMN trace_id TEXT NOT NULL DEFAULT ''`,
		// RFC BL — tenant-scope the Memory store + provenance/access columns.
		// On an UPGRADED DB these ALTERs add the columns to the existing
		// memory table; its old (scope, scope_id, key) PK is then replaced by
		// rebuildTenantBlindKeys (see the CREATE TABLE note). DEFAULT ''
		// backfills existing rows to the legacy tenant.
		`ALTER TABLE memory ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory ADD COLUMN origin TEXT`,
		`ALTER TABLE memory ADD COLUMN class TEXT`,
		`ALTER TABLE memory ADD COLUMN source_session_id TEXT`,
		`ALTER TABLE memory ADD COLUMN source_run_id TEXT`,
		`ALTER TABLE memory ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memory ADD COLUMN last_accessed_at INTEGER`,
		// RFC BL P2 — the soft-archive marker. NULL on every legacy row (live);
		// the consolidator stamps it to hide a consolidated raw row from recall
		// while retaining it.
		`ALTER TABLE memory ADD COLUMN superseded_at INTEGER`,
		`ALTER TABLE memory ADD COLUMN observed_at INTEGER`,
		`ALTER TABLE memory ADD COLUMN valid_at INTEGER`,
		`ALTER TABLE memory ADD COLUMN invalid_at INTEGER`,
		// RFC BL P3 — what enqueued a pending row (agent_explicit | compaction),
		// server-set. memory_pending arrived as a pure CREATE TABLE in P2, so an
		// upgraded DB has the table but not this column and needs the ALTER; a
		// fresh DB gets it from the schema block above. NULL on rows enqueued
		// before it existed — deliberately not backfilled to a guess.
		`ALTER TABLE memory_pending ADD COLUMN origin TEXT`,
		// RFC DT — per-call timing on the usage ledger. NULL on every row
		// recorded before it existed; nothing is backfilled to a guess.
		`ALTER TABLE token_usage ADD COLUMN duration_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN ttft_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN load_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN prefill_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN decode_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN queue_ms INTEGER`,
		// A token's fixed lifetime (epoch-nanos; NULL = never expires).
		`ALTER TABLE operator_token_defs ADD COLUMN expires_at INTEGER`,
		// A token's restrictions record as JSON (NULL = unrestricted).
		`ALTER TABLE operator_token_defs ADD COLUMN restrictions TEXT`,
		// Usage counted against a budget without a provider's report of it.
		// Mirrors postgres migration 0100.
		`ALTER TABLE usage_carry ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0`,
		// The run a held webhook delivery key started ('' for none yet).
		// Mirrors postgres migration 0104; its index is in addIndexes.
		`ALTER TABLE webhook_deliveries ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`,
		// A dedup_key claim's fencing token, its time (unix nanos, 0 for a row
		// written before) and the run keys that find its run. Mirrors postgres
		// migration 0107; its index is in addIndexes.
		`ALTER TABLE webhook_deliveries ADD COLUMN claim_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_deliveries ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE webhook_deliveries ADD COLUMN run_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_deliveries ADD COLUMN run_alt_key TEXT NOT NULL DEFAULT ''`,
		// RFC FA P1: when the person last authenticated (unix nanos, NULL =
		// unknown) and the gate that issued a token. Mirrors postgres 0112.
		`ALTER TABLE ui_sessions ADD COLUMN auth_time INTEGER`,
		`ALTER TABLE operator_token_defs ADD COLUMN issued_by_gate TEXT NOT NULL DEFAULT ''`,
		// What a usage row was for, and the audio an STT call transcribed. A
		// row from before reads back chat with no seconds. Mirrors postgres
		// 0113; the archive's key gains purpose in rebuildWidenedKeys.
		`ALTER TABLE token_usage ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE token_usage ADD COLUMN audio_seconds REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_archive ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE usage_archive ADD COLUMN audio_seconds REAL NOT NULL DEFAULT 0`,
		// The characters a TTS call sent to its engine. A row from before
		// reads back 0. Mirrors postgres 0116; no key changes.
		`ALTER TABLE token_usage ADD COLUMN input_chars INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_archive ADD COLUMN input_chars INTEGER NOT NULL DEFAULT 0`,
		// RFC EY §8: a token's authority mode ('' = frozen scopes, 'inherit' =
		// its user's current access_mode). Every existing row reads back
		// frozen. Mirrors postgres 0117.
		`ALTER TABLE operator_token_defs ADD COLUMN authority TEXT NOT NULL DEFAULT ''`,
		// Whose access-control set binds a team subscription's walks (the
		// promoter). NULLABLE, no DEFAULT: a pointer promoted before it reads
		// as "owner unknown", never as "recorded, nobody". Mirrors postgres
		// 0121; see sqlshared/teamdef_promoter.go for the three states.
		`ALTER TABLE teamdef_active ADD COLUMN promoter_acl_owner TEXT`,
		// The model tier and user tier a row was spent under. A row from
		// before reads back ''. Mirrors postgres 0125; the archive's key
		// gains both in rebuildWidenedKeys.
		`ALTER TABLE token_usage ADD COLUMN tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE token_usage ADD COLUMN user_tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_archive ADD COLUMN tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_archive ADD COLUMN user_tier TEXT NOT NULL DEFAULT ''`,
		// The app sign-in an access token came from (RFC FL): its client and
		// refresh-token family, '' for every other token. Mirrors postgres
		// 0123; the indexes are in addIndexes.
		`ALTER TABLE operator_token_defs ADD COLUMN oauth_client TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE operator_token_defs ADD COLUMN oauth_family TEXT NOT NULL DEFAULT ''`,
		// The removal's claimed sweeper stage, the user's status before it,
		// and the SCIM re-enable stamp (RFC FA P6). Mirrors postgres 0127.
		`ALTER TABLE auth_removals ADD COLUMN stage TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE auth_removals ADD COLUMN prior_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE scim_users ADD COLUMN reactivated_at INTEGER`,
		// The document a change row belongs to, stamped at write time (RFC FV);
		// '' for every row before. Mirrors postgres 0129.
		`ALTER TABLE memory_changes ADD COLUMN document_id TEXT NOT NULL DEFAULT ''`,
		// When a subject erasure ended the account and kept its row as a
		// tombstone (RFC FY Part F); NULL for every live user. Mirrors
		// postgres 0130.
		`ALTER TABLE users ADD COLUMN erased_at INTEGER`,
	}

	addIndexes := []string{
		// Drives the hot lookup paths for the cancel/get endpoints.
		// Partial indexes (WHERE ... IS NOT NULL) keep the index small —
		// the vast majority of historical rows have no agent_id.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_id        ON runs(agent_id)        WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_parent_agent_id ON runs(parent_agent_id) WHERE parent_agent_id IS NOT NULL`,
		// A run's children by its run id: the cascade that ends the runs below
		// one no loop holds, which a team walk's members are only found by.
		`CREATE INDEX IF NOT EXISTS runs_by_parent_run_id   ON runs(parent_run_id)   WHERE parent_run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_user_active     ON runs(user_id, status) WHERE user_id IS NOT NULL`,
		// RFC L / Web-UI multi-tenant authz — tenant-scoped workspace lists.
		`CREATE INDEX IF NOT EXISTS runs_by_tenant_active   ON runs(tenant_id, status) WHERE tenant_id IS NOT NULL`,
		// RFC DI D5: the draft expiry sweep reads configured runs by age; the
		// partial index stays as small as the set of live drafts.
		`CREATE INDEX IF NOT EXISTS runs_configured_by_age  ON runs(started_at)       WHERE status = 'configured'`,
		// ListRunsByWalk: a walk's members in (started_at, id) order. Here, after
		// addColumns, so an upgraded DB has the column before its index.
		`CREATE INDEX IF NOT EXISTS runs_by_walk            ON runs(walk_id, started_at, id) WHERE walk_id IS NOT NULL`,
		// RFC DT: the boot seed reads the latest timed calls per (provider,
		// model). Here, after addColumns, so an upgraded DB has duration_ms
		// before an index names it; partial, so untimed history costs nothing.
		`CREATE INDEX IF NOT EXISTS token_usage_timing      ON token_usage(provider, model, ts) WHERE duration_ms IS NOT NULL`,
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
		`CREATE INDEX IF NOT EXISTS channel_messages_by_visible ON channel_messages(tenant_id, channel, scope, scope_id, visible_at, id)`,
		// The hook worker's claim scan: only messages awaiting hooks, which
		// are few. The literal is store.ChannelHookHeldVisibleAt in unix
		// nanoseconds (a partial index predicate cannot take a parameter).
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS channel_messages_awaiting_hook ON channel_messages(id) WHERE visible_at = %d`,
			store.ChannelHookHeldVisibleAt().UnixNano()),
		// The channel_cursors upsert (ChannelAck) targets this uniq index
		// via ON CONFLICT(tenant_id, channel, scope, scope_id). It
		// duplicates the PK — on an upgraded DB too, once
		// rebuildTenantBlindKeys has replaced the old tenant-blind one — and
		// is kept so the ON CONFLICT target never depends on a DB's history.
		// Mirrors uniq_memory_tenant_scope_scope_id_key.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_channel_cursors_tenant ON channel_cursors(tenant_id, channel, scope, scope_id)`,
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
		// The second durable dedup key — unique for the same reason: two
		// replicas racing on one delivery both miss the lookup, and the
		// index lets only one CreateRun win.
		`CREATE UNIQUE INDEX IF NOT EXISTS runs_delivery_alt_key ON runs(delivery_alt_key) WHERE delivery_alt_key IS NOT NULL`,
		// A failed run's held delivery keys are dropped by run id.
		`CREATE INDEX IF NOT EXISTS webhook_deliveries_by_run_id ON webhook_deliveries(run_id) WHERE run_id <> ''`,
		// A claim's keys are linked, released and taken over by claim id.
		`CREATE INDEX IF NOT EXISTS webhook_deliveries_by_claim_id ON webhook_deliveries(claim_id) WHERE claim_id <> ''`,
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
		// These indexes alone did not give per-tenant isolation: the old
		// PRIMARY KEY(name) / UNIQUE(name, version) still fired first, so two
		// tenants could not share a name on a pre-existing SQLite DB.
		// rebuildTenantBlindKeys now replaces those keys before this block
		// runs, which leaves these redundant with the key on every DB; they
		// are kept as the stable ON CONFLICT target. The skill + mcp planes have NO
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
		// DocumentSource plane — same in-place-upgrade gap as the
		// MemoryBackend plane above (document_source_def_active ON
		// CONFLICT(tenant_id, name) + document_source_defs UNIQUE(tenant_id,
		// name, version)). No dynamic_* table to cover.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_document_source_def_active_tenant_name   ON document_source_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_document_source_defs_tenant_name_version ON document_source_defs(tenant_id, name, version)`,
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
		// Redundant with the composite PRIMARY KEY (on an upgraded DB too,
		// once rebuildTenantBlindKeys has replaced the old (scope, scope_id,
		// key) PK), and kept as the stable ON CONFLICT target — without a
		// matching key the upserts fail "ON CONFLICT clause does not match
		// ...". Mirrors the def-plane indexes above.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_tenant_scope_scope_id_key ON memory(tenant_id, scope, scope_id, key)`,
		// MOVED HERE FROM THE CREATE-TABLE BLOCK, and the move is the fix for an
		// upgrade that could not start at all.
		//
		// Each of these names a column that an upgraded DB only gets from the ALTER
		// block above: a `CREATE TABLE IF NOT EXISTS` is a no-op against an existing
		// table, so `memory` keeps whatever shape it was created with and the column
		// arrives later or not at all. Creating the index first therefore failed with
		// "no such column: observed_at" — fatally, since index errors are not
		// swallowed — and the ALTER that would have added it was never reached. Any
		// sqlite deployment whose `memory` table predated RFC CL could not open.
		//
		// A fresh DB is unaffected either way: it gets the columns from CREATE TABLE
		// and the indexes here, which is why every test passed while real upgrades
		// broke.
		//
		// Partial for the reason the originals gave: the predicate column is NULL on
		// every row nobody dated, and on a corpus that was never dated that is all of
		// them. Mirrors postgres migrations 0065 / 0073 / 0074, which do not have this
		// problem because each is its own ordered file.
		`CREATE INDEX IF NOT EXISTS memory_by_source_session ON memory(tenant_id, source_session_id) WHERE source_session_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS memory_by_observed_at ON memory(tenant_id, scope, scope_id, observed_at) WHERE observed_at IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS memory_by_valid_at ON memory(tenant_id, scope, scope_id, valid_at) WHERE valid_at IS NOT NULL`,
		// RFC FL: an app sign-in's access tokens, by family and by client.
		`CREATE INDEX IF NOT EXISTS operator_token_defs_by_oauth_family ON operator_token_defs(oauth_family) WHERE oauth_family <> ''`,
		`CREATE INDEX IF NOT EXISTS operator_token_defs_by_oauth_client ON operator_token_defs(oauth_client) WHERE oauth_client <> ''`,
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
	return schemaPlan{tables: stmts, columns: addColumns, indexes: addIndexes, fixups: dataFixups}
}
