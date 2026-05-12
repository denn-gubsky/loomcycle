-- 0005_channel_visible_at.down.sql — drop v0.8.6 system-channels columns.
--
-- Reverses the additive ALTER TABLE columns and the tuple-order
-- index. Cursor TRUNCATE is one-way (the v0.8.6 up.sql wiped existing
-- subscriber positions; down doesn't restore them).

DROP INDEX IF EXISTS channel_messages_by_visible;

ALTER TABLE channel_messages
    DROP COLUMN IF EXISTS visible_at,
    DROP COLUMN IF EXISTS published_by_user_id;
