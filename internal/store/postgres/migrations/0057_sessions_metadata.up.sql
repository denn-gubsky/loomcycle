-- 0057_sessions_metadata.up.sql — RFC BE: human/organizational chat metadata on
-- the session row (the History tool's browse/search/annotate surface).
--
-- A "chat" is a session; these columns give it a human handle. All additive +
-- nullable so existing rows read the zero value.
--
-- archived_at / summary_updated_at are BIGINT unix-NANOSECONDS (NULL = unset),
-- deliberately NOT TIMESTAMPTZ like sessions.created_at: the store layer
-- round-trips these two through the same int64 path as the SQLite adapter
-- (sessions.archived_at INTEGER), so the cross-backend scan stays uniform.
-- tags is a JSON array stored as TEXT (shared store.EncodeTags/DecodeTags);
-- pinned is a BOOLEAN.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS title              TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS description        TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS tags               TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS pinned             BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS archived_at        BIGINT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS summary            TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS summary_updated_at BIGINT;

-- Partial indexes stay small: only pinned / archived rows carry the flag.
CREATE INDEX IF NOT EXISTS sessions_by_pinned      ON sessions(pinned)      WHERE pinned;
CREATE INDEX IF NOT EXISTS sessions_by_archived_at ON sessions(archived_at) WHERE archived_at IS NOT NULL;
