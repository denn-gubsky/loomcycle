-- 0131_audit_events.up.sql — RFC FZ Part E: a durable, tamper-evident audit
-- trail written by every replica.
--
-- audit_events is one hash chain per tenant (tenant_id '' is the operator
-- chain): hash = SHA-256(prev_hash || canonical row), and mac signs each hash
-- under a key derived from LOOMCYCLE_SECRET_KEY, so a database writer
-- without the key cannot rewrite history and recompute the chain. ts is unix
-- nanoseconds (BIGINT, not TIMESTAMPTZ) so the hashed time reproduces
-- exactly; TIMESTAMPTZ would round it to microseconds.
--
-- audit_heads is the per-tenant head an append locks (SELECT ... FOR UPDATE),
-- so two replicas appending to one tenant are serialized and the chain never
-- forks; the (tenant_id, seq) primary key backs that up. anchor_* is the last
-- row a retention prune removed, signed, so the rows left still verify.
CREATE TABLE IF NOT EXISTS audit_events (
    tenant_id TEXT   NOT NULL,
    seq       BIGINT NOT NULL,
    ts        BIGINT NOT NULL,
    actor     TEXT   NOT NULL DEFAULT '',
    action    TEXT   NOT NULL,
    subject   TEXT   NOT NULL DEFAULT '',
    payload   TEXT   NOT NULL,
    prev_hash TEXT   NOT NULL DEFAULT '',
    hash      TEXT   NOT NULL,
    mac       TEXT   NOT NULL,
    PRIMARY KEY (tenant_id, seq)
);

CREATE TABLE IF NOT EXISTS audit_heads (
    tenant_id   TEXT   PRIMARY KEY,
    seq         BIGINT NOT NULL DEFAULT 0,
    hash        TEXT   NOT NULL DEFAULT '',
    mac         TEXT   NOT NULL DEFAULT '',
    anchor_seq  BIGINT NOT NULL DEFAULT 0,
    anchor_hash TEXT   NOT NULL DEFAULT '',
    anchor_mac  TEXT   NOT NULL DEFAULT ''
);
