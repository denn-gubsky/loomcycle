package sqlite

// channelsSection: channel messages, cursors, hook state and channel
// definitions, plus the v0.8.6 channel data fixups.
func channelsSection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}, fixups: []string{
		// v0.8.6 data fixups (run after column adds + indexes are in place).
		//   - Backfill visible_at on pre-v0.8.6 rows so the new (visible_at, id)
		//     read order matches publish order for legacy data.
		//   - Wipe pre-v0.8.6 cursors. The v0.8.6 cursor format is
		//     `cur_<hex>_<msg_id>` (tuple-shaped). Legacy `msg_<hex>` cursors
		//     are not parsed — subscribers replay from oldest on first
		//     subscribe after upgrade.
		`UPDATE channel_messages SET visible_at = published_at WHERE visible_at = 0`,
		`DELETE FROM channel_cursors WHERE cursor NOT LIKE 'cur_%' AND cursor != 'cur_0'`,
	}}
}
