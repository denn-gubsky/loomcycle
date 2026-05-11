-- 0004_channels.down.sql — drop the v0.8.4 Channel tool tables.
DROP INDEX IF EXISTS channel_messages_by_expires_at;
DROP TABLE IF EXISTS channel_cursors;
DROP TABLE IF EXISTS channel_messages;
