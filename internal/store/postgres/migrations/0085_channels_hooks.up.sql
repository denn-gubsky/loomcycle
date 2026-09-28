-- 0085_channels_hooks.up.sql — a runtime channel carries hooks.
--
-- The channel's channel_publish hooks, as JSON ({"channel_publish": [...]}):
-- each entry a HookDef name or an inline webhook. They decide on each message
-- published to the channel before any reader sees it. NULL = no hooks, so
-- every existing channel keeps delivering exactly as before.
ALTER TABLE channels ADD COLUMN IF NOT EXISTS hooks JSONB;
