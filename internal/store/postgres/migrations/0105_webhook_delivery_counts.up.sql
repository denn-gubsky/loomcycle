-- 0105_webhook_delivery_counts.up.sql — how a webhook's deliveries ended.
--
-- One row per webhook definition, outcome and refusal reason, summed over
-- every replica. The receiver counts in memory and adds what it counted every
-- half minute, so an unauthenticated request never writes here by itself.
-- static marks the operator's yaml webhook, which may share a name with a
-- WebhookDef in the shared tenant. No body text, no header values.
CREATE TABLE IF NOT EXISTS webhook_delivery_counts (
    tenant_id TEXT        NOT NULL,
    name      TEXT        NOT NULL,
    static    BOOLEAN     NOT NULL,
    outcome   TEXT        NOT NULL,
    reason    TEXT        NOT NULL,
    count     BIGINT      NOT NULL,
    last_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, name, static, outcome, reason)
);
