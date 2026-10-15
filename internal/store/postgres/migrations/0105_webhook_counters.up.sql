-- 0105_webhook_counters.up.sql — how a webhook's deliveries were answered.
--
-- One row per webhook (its state key: owner and name) and outcome — accepted,
-- ping, ignored, filtered, duplicate, refused.<reason> — holding a running
-- count. Each replica counts in memory and adds its increments here every 30
-- seconds: a write per delivery would let unauthenticated noise write to the
-- database at will. Non-secret: no body text, no header value.
CREATE TABLE IF NOT EXISTS webhook_counters (
    webhook_key TEXT        NOT NULL,
    outcome     TEXT        NOT NULL,
    count       BIGINT      NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (webhook_key, outcome)
);
