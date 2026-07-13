DROP INDEX IF EXISTS sessions_by_archived_at;
DROP INDEX IF EXISTS sessions_by_pinned;
ALTER TABLE sessions DROP COLUMN IF EXISTS summary_updated_at;
ALTER TABLE sessions DROP COLUMN IF EXISTS summary;
ALTER TABLE sessions DROP COLUMN IF EXISTS archived_at;
ALTER TABLE sessions DROP COLUMN IF EXISTS pinned;
ALTER TABLE sessions DROP COLUMN IF EXISTS tags;
ALTER TABLE sessions DROP COLUMN IF EXISTS description;
ALTER TABLE sessions DROP COLUMN IF EXISTS title;
