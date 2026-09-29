-- 0088_usage_carry.up.sql — month-to-date usage carried in by a snapshot restore.
--
-- A snapshot carries one month-to-date token total per (tenant, user) so a
-- tenant near its budget on the source is not back near zero on the target.
-- The target persists it here, and the budget tracker adds it to its own
-- ledger aggregate when it seeds. It is budget state, NOT billing data: no usage
-- report, cost attribution or the Usage page reads this table.
--
-- One row per (tenant, user, month). A restore keeps the LARGER of the stored
-- and the incoming total, so re-restoring a snapshot changes nothing. Rows for a
-- past month are inert (the tracker reads only the current month). A subject
-- erasure deletes the subject's rows.
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- to 61 and replays every later migration, so a bare CREATE here would fail
-- `already exists` on that replay.
CREATE TABLE IF NOT EXISTS usage_carry (
    tenant_id  TEXT        NOT NULL DEFAULT '',
    user_id    TEXT        NOT NULL DEFAULT '',
    month      TIMESTAMPTZ NOT NULL,
    tokens     BIGINT      NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id, month)
);
