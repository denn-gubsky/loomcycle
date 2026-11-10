package sqlite

// agentDefsSection: agent, skill, team and hook definitions with their
// active pointers, team webhook arms and webhook deliveries.
func agentDefsSection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}}
}

// serviceDefsSection: MCP server, schedule and A2A definitions with their
// active pointers, and the scheduler run state.
func serviceDefsSection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}}
}

// integrationDefsSection: webhook, memory backend and document source
// definitions with their active pointers.
func integrationDefsSection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}}
}
