-- 0058_session_embeddings.up.sql — RFC BE: the per-session embedding index
-- backing History op=related (semantic "related chats").
--
-- One row per chat. The vector is a plain TEXT column (store.EncodeVector),
-- ranked in Go over a small per-tenant candidate set — deliberately NOT
-- pgvector: the session index is tiny compared to memory_embeddings, so it needs
-- no extension and runs identically on the SQLite adapter (and on a Postgres
-- WITHOUT pgvector). tenant_id / user_id / agent are denormalised from the
-- session so the similarity search folds owner/tenant WITHOUT a join (they are
-- immutable session facts, set once at CreateSession). FK CASCADE drops the row
-- when the session is deleted; updated_at is BIGINT unix-NANOSECONDS (matching
-- sessions.archived_at so the cross-backend scan stays uniform).
CREATE TABLE IF NOT EXISTS session_embeddings (
    session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    tenant_id   TEXT    NOT NULL DEFAULT '',
    user_id     TEXT,
    agent       TEXT    NOT NULL DEFAULT '',
    provider    TEXT    NOT NULL,
    model       TEXT    NOT NULL,
    dimension   INTEGER NOT NULL,
    vector      TEXT    NOT NULL,
    updated_at  BIGINT  NOT NULL
);

-- The similarity search folds on (tenant_id, user_id, agent) then scans the
-- most-recent candidates; this covers that owner prefix.
CREATE INDEX IF NOT EXISTS session_embeddings_by_owner ON session_embeddings(tenant_id, user_id, agent);
