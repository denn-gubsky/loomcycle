-- 0066_channels_tenant_id.up.sql — tenant-scope the Channel primitive
-- (the message log, the per-subscriber cursors, and the runtime-declared
-- channel definitions).
--
-- RFC L isolated runs/sessions; RFC N isolated the definition plane; 0059
-- isolated the base Memory store. Channels stayed tenant-blind: the message
-- + cursor tables key on (channel, scope, scope_id) with no tenant axis, and
-- the `channels` def table keys on a bare `name`. An agent scope_id is the
-- bare agent name, so two tenants running a same-named agent collided on one
-- global (channel, scope, scope_id) keyspace, and two tenants could not each
-- own a channel of the same name. This migration adds the tenant axis so the
-- routes can move off the substrate:admin catch-all onto substrate:tenant.
--
-- '' = the shared/operator/legacy tenant. Existing rows backfill to '' via
-- the column DEFAULT, so a single-tenant deployment reads exactly as before
-- (it IS entirely '') — no separate UPDATE needed.
--
-- Unlike 0059 (Memory), the channel tables carry NO foreign keys between
-- channel_messages/channel_cursors and the `channels` def table (the cascade
-- is application code — see ChannelsDelete), so there is no FK-lockstep
-- dance: each table's PK swaps independently.

-- channel_messages: the tenant axis, leading the PK so per-subscriber range
-- scans stay index lookups within a tenant.
--
-- IF NOT EXISTS / IF EXISTS make this migration re-runnable over an
-- already-migrated DB: the migration test suite rewinds the version pointer
-- (forceMigrationVersion) and re-applies every migration above 61, so each
-- must no-op cleanly on a second pass (mirrors 0063's ADD COLUMN IF NOT
-- EXISTS). The DROP-then-ADD-PRIMARY-KEY pair is idempotent because the
-- guarded DROP always clears the current pkey first.
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE channel_messages DROP CONSTRAINT IF EXISTS channel_messages_pkey;
ALTER TABLE channel_messages ADD PRIMARY KEY (tenant_id, channel, scope, scope_id, id);

-- The visible-order read index must lead with tenant_id to match the
-- tenant-qualified WHERE clause; the expires_at partial sweeper index is
-- tenant-agnostic (it deletes across all tenants) and stays as-is.
DROP INDEX IF EXISTS channel_messages_by_visible;
CREATE INDEX channel_messages_by_visible
    ON channel_messages(tenant_id, channel, scope, scope_id, visible_at, id);

-- channel_cursors: the per-subscriber committed read position, tenant-keyed.
ALTER TABLE channel_cursors ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE channel_cursors DROP CONSTRAINT IF EXISTS channel_cursors_pkey;
ALTER TABLE channel_cursors ADD PRIMARY KEY (tenant_id, channel, scope, scope_id);

-- channels: the runtime-declared def table. (tenant_id, name) so two tenants
-- can each own a same-named channel and ChannelGet is tenant-confinable.
ALTER TABLE channels ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_pkey;
ALTER TABLE channels ADD PRIMARY KEY (tenant_id, name);
