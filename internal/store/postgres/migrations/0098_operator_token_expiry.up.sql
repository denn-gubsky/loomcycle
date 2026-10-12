-- 0098_operator_token_expiry.up.sql — a token can carry a fixed lifetime.
--
-- retired_at is moved by retire and by rotation; expires_at is set once, at
-- mint, and never moved. A token minted for a client machine is given a short
-- life so that a copy that leaks stops working on its own, without anyone
-- having to find it and retire it. NULL = never expires, which is every row
-- that existed before this column.
--
-- VALIDITY becomes: (retired_at IS NULL OR now < retired_at)
--               AND (expires_at IS NULL OR now < expires_at).
ALTER TABLE operator_token_defs ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

-- The expired-row sweep deletes by expires_at. Partial: most rows have none.
CREATE INDEX IF NOT EXISTS operator_token_defs_by_expires_at
    ON operator_token_defs (expires_at) WHERE expires_at IS NOT NULL;
