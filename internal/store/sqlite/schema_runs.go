package sqlite

// runsSection: sessions and runs, the token-usage ledger and its rollup,
// token limits, retention overrides, the audit log, Web UI sessions and the
// usage carry.
func runsSection() schemaPlan {
	return schemaPlan{tables: []string{
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
	}}
}
