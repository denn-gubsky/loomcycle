-- v0.11.5 runtime-declared channels.
--
-- yaml-declared channels stay in cfg.Channels (in-memory only);
-- runtime channels created via POST /v1/_channels (admin endpoint
-- or Web UI) persist here. The HTTP admin layer merges both at
-- read time with a `source` discriminator ("yaml" vs "runtime").
--
-- Cascade delete of messages + cursors is handled in application
-- code (channel_messages doesn't FK to here — yaml-declared
-- channels never had a parent row, so the table can't have a FK
-- to itself).

CREATE TABLE IF NOT EXISTS channels (
    name         TEXT        PRIMARY KEY,
    description  TEXT        NOT NULL DEFAULT '',
    scope        TEXT        NOT NULL,
    semantic     TEXT        NOT NULL,
    default_ttl  INTEGER     NOT NULL DEFAULT 0,
    max_messages INTEGER     NOT NULL DEFAULT 0,
    publisher    TEXT        NOT NULL DEFAULT '',
    period       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
