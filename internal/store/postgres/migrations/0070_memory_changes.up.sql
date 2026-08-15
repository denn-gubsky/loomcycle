-- RFC CD Part C: the opt-in memory/document change-data-capture feed.
-- Populated only when LOOMCYCLE_MEMORY_CHANGES_ENABLED (via the CDC store
-- decorator), so a default deployment carries an empty table and pays nothing.
-- Value-free rows: each names WHAT changed (the coordinate), not the value — a
-- subscriber pulls the current value via the data API. The BIGSERIAL seq is the
-- SSE cursor (the same model as run events). A retention sweeper prunes by at.
CREATE TABLE IF NOT EXISTS memory_changes (
	seq         BIGSERIAL PRIMARY KEY,
	tenant_id   TEXT NOT NULL,
	change_type TEXT NOT NULL,
	scope       TEXT NOT NULL,
	scope_id    TEXT NOT NULL,
	key         TEXT NOT NULL DEFAULT '',
	chunk_id    TEXT NOT NULL DEFAULT '',
	at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS memory_changes_by_tenant ON memory_changes(tenant_id, seq);
CREATE INDEX IF NOT EXISTS memory_changes_by_at ON memory_changes(at);
