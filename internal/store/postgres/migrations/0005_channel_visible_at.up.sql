-- 0005_channel_visible_at.up.sql — v0.8.6 system channels + deferred publish.
--
-- Two additive columns on channel_messages:
--   visible_at           — when this message becomes deliverable.
--                          DEFAULT now() so an immediate publish (the
--                          v0.8.4 / v0.8.5 shape) keeps working
--                          without code-side changes; deferred
--                          publish sets it to deliver_at instead.
--   published_by_user_id — audit column. Populated from the run's
--                          user_id for agent publishes, "_system"
--                          for internal Go publishes, the bearer's
--                          user for admin-endpoint publishes.
--
-- Delivery order changes from pure ID order to (visible_at, id) tuple
-- order so a message published "before" another but deferred for
-- delivery becomes deliverable after the immediate one. Subscribers
-- never silently skip deferred messages just because a later-but-
-- immediate one progressed their cursor.
--
-- channel_cursors is TRUNCATEd in this migration: the cursor format
-- changes from `msg_<hex>` to `cur_<hex>_<msg_id>`, and the v0.8.4
-- shipped state (2 weeks old) has no operator-visible cursors worth
-- preserving. Subscribers replay from the oldest non-expired message
-- on first subscribe after the upgrade — operator note in the
-- release notes.
--
-- Transactionality: golang-migrate's pgx5 driver wraps each
-- migration file in a transaction by default (no `x-no-tx-wrap`
-- param on our DSN). The TRUNCATE + ALTER + UPDATE below are
-- therefore atomic — no in-flight reader sees a partial state.

TRUNCATE TABLE channel_cursors;

ALTER TABLE channel_messages
    ADD COLUMN visible_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN published_by_user_id TEXT;

-- Existing rows: backfill visible_at from published_at so the
-- DEFAULT-now() doesn't make every row "freshly visible at migration
-- time" (which would be wrong if any row had a future-dated
-- expires_at and an absurdly recent visible_at).
UPDATE channel_messages SET visible_at = published_at;

-- Tuple-order index for the new read query:
--   WHERE channel = ? AND scope = ? AND scope_id = ?
--     AND visible_at <= now()
--     AND (visible_at, id) > (?, ?)
--   ORDER BY visible_at ASC, id ASC
CREATE INDEX channel_messages_by_visible
    ON channel_messages(channel, scope, scope_id, visible_at, id);
