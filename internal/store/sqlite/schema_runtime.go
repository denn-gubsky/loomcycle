package sqlite

// runtimeSection: evaluations, process samples, dynamic agents, interrupts
// and snapshots.
func runtimeSection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}}
}
