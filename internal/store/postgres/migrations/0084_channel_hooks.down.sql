DROP TABLE IF EXISTS channel_hook_state;
DROP INDEX IF EXISTS channel_messages_awaiting_hook;
ALTER TABLE channel_messages DROP COLUMN IF EXISTS requested_visible_at;
ALTER TABLE channel_messages DROP COLUMN IF EXISTS hook_tenant;
ALTER TABLE channel_messages DROP COLUMN IF EXISTS origin;
