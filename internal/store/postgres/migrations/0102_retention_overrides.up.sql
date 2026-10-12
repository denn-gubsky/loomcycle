-- 0102_retention_overrides.up.sql — timed overrides of an agent's no-trace mode.
--
-- An agent whose runs store no content cannot be debugged from its
-- transcript. An operator may lift the mode for ONE agent in ONE tenant for a
-- limited time: runs of that agent started inside the window are ordinary
-- stored runs. It lifts a mode that came from the agent's definition or from
-- the run request, never one that came from the caller's token.
--
-- A runtime record and not a field of the definition: an agent definition is
-- content-addressed and often static config, and a switch that turns itself
-- off does not belong in either. One row per (tenant, agent); `until` is when
-- it stops applying. Setting one replaces the last. Expired rows are inert
-- and are removed when the next one is set.
--
-- Never carried by a snapshot: an override is a decision about this
-- deployment, for the next few hours.
CREATE TABLE IF NOT EXISTS retention_overrides (
    tenant_id  TEXT        NOT NULL DEFAULT '',
    agent      TEXT        NOT NULL,
    until      TIMESTAMPTZ NOT NULL,
    set_by     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, agent)
);
