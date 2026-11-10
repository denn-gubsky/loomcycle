package sqlite

// memorySection: the event log, memory change feed and its cursors, session
// embeddings, memory, the pending (consolidation) queue and its cursors.
func memorySection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}}
}
