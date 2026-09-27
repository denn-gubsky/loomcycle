-- Channel hooks: a message awaiting its channel's hooks, and the hook
-- chain's durable progress on it.
--
-- A message awaiting hooks is stored at the reserved visible_at
-- 2200-01-02 (store.ChannelHookHeldVisibleAt), so every read path — which
-- filters visible_at <= now — keeps it hidden until a hook chain releases
-- or drops it. It carries its hook context on the row, so the message
-- alone can be re-hooked after a restart or a snapshot restore:
--   origin               — what wrote it ("starter_sink": a Starter's per-run
--                          result, which a hook must not make disappear)
--   hook_tenant          — the tenant whose channel definition governs it (a
--                          global-scope message is stored under tenant '')
--   requested_visible_at — the publisher's deliver_at, kept while it waits
--
-- channel_hook_state is advisory progress: the lease that makes one worker
-- decide one message, the chain position, the rewritten body and the
-- recorded asks. The message's visible_at is the truth about whether it
-- still awaits a decision.
--
-- IF NOT EXISTS throughout: migrations after 0061 are replayed by a test.

ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT '';
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS hook_tenant TEXT;
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS requested_visible_at TIMESTAMPTZ;

-- The worker's claim scan: only messages awaiting hooks, which are few. The
-- literal must match store.ChannelHookHeldVisibleAt.
CREATE INDEX IF NOT EXISTS channel_messages_awaiting_hook
    ON channel_messages(id) WHERE visible_at = '2200-01-02 00:00:00+00';

CREATE TABLE IF NOT EXISTS channel_hook_state (
    tenant_id       TEXT        NOT NULL,
    channel         TEXT        NOT NULL,
    scope           TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    id              TEXT        NOT NULL,
    run_id          TEXT        NOT NULL DEFAULT '',
    chain_pos       INTEGER     NOT NULL DEFAULT 0,
    body            JSONB,
    journal         JSONB,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    last_error      TEXT        NOT NULL DEFAULT '',
    lease_owner     TEXT        NOT NULL DEFAULT '',
    lease_until     TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, channel, scope, scope_id, id)
);
