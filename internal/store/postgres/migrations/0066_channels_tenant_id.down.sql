-- 0066_channels_tenant_id.down.sql — reverse the channel tenant axis.
--
-- SAFE ONLY while every row is tenant_id='' (a single-tenant deployment, or
-- one that never wrote a non-'' tenant). If real per-tenant rows exist,
-- restoring the tenant-blind (channel, scope, scope_id[, id]) / (name) PKs
-- would collide across tenants and the migration would fail — the operator
-- must consolidate to one tenant first. Mirrors the 0037/0059 down caveat.

ALTER TABLE channels DROP CONSTRAINT channels_pkey;
ALTER TABLE channels ADD PRIMARY KEY (name);
ALTER TABLE channels DROP COLUMN tenant_id;

ALTER TABLE channel_cursors DROP CONSTRAINT channel_cursors_pkey;
ALTER TABLE channel_cursors ADD PRIMARY KEY (channel, scope, scope_id);
ALTER TABLE channel_cursors DROP COLUMN tenant_id;

DROP INDEX IF EXISTS channel_messages_by_visible;
ALTER TABLE channel_messages DROP CONSTRAINT channel_messages_pkey;
ALTER TABLE channel_messages ADD PRIMARY KEY (channel, scope, scope_id, id);
ALTER TABLE channel_messages DROP COLUMN tenant_id;
CREATE INDEX channel_messages_by_visible
    ON channel_messages(channel, scope, scope_id, visible_at, id);
