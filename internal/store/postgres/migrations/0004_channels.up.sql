-- 0004_channels.up.sql — v0.8.4 Channel tool storage.
--
-- channel_messages holds the actual messages. One row per publish.
-- The id column is a ULID (TEXT, sortable by publish time) assigned
-- in the application layer; the (channel, scope, scope_id, id)
-- composite primary key gives O(log n) per-subscriber range scans
-- without a secondary index for the hot read path.
--
-- payload is JSONB so future server-side filters can use the @> / ->
-- operators without a migration. expires_at is the TTL anchor; NULL
-- means "no expiry" (channel default applies in the application
-- layer, NOT in the DB — so an operator who lowers a default doesn't
-- silently lose already-published rows that pre-date the change).
--
-- The partial index on expires_at keeps ChannelSweepExpired's DELETE
-- cheap — same shape as Memory's sweeper index.
CREATE TABLE channel_messages (
    id           TEXT        NOT NULL,
    channel      TEXT        NOT NULL,
    scope        TEXT        NOT NULL,
    scope_id     TEXT        NOT NULL,
    payload      JSONB       NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ,
    PRIMARY KEY (channel, scope, scope_id, id)
);

CREATE INDEX channel_messages_by_expires_at
    ON channel_messages(expires_at) WHERE expires_at IS NOT NULL;

-- channel_cursors holds one row per subscriber's committed read
-- position. The PK matches the Subscribe / Ack call signature so
-- both ops are single-row index lookups.
--
-- cursor stores the last message id ack'd. An empty / missing row
-- means "no ack yet — read from oldest non-expired". Monotonic ack
-- enforcement (rejecting older cursors) lives in the application
-- layer because the comparison is over ULID lexicographic order;
-- pushing it into a CHECK constraint would require a function and
-- buys nothing.
CREATE TABLE channel_cursors (
    channel    TEXT        NOT NULL,
    scope      TEXT        NOT NULL,
    scope_id   TEXT        NOT NULL,
    cursor     TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (channel, scope, scope_id)
);
