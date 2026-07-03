-- 0051_credential_defs.up.sql — RFC AR CredentialDef: the secure per-tenant
-- credential store.
--
-- A FLAT (tenant_id, scope, scope_id, name) table, like volume_defs — NOT the
-- content-addressed/versioned Def shape. `definition` holds ONLY sealed
-- ciphertext (inline backend: AES-256-GCM under a per-tenant HKDF-derived key,
-- see internal/credential) or an external-backend pointer (vault/aws_sm/gcp_sm/
-- onepassword) — NEVER a plaintext secret.
--
-- scope is 'tenant' | 'user' | 'agent'; scope_id is '' for tenant, the user
-- subject for user scope, the agent name for agent scope. Together with name
-- they form the PK so user A's "telegram_bot_token" can't collide with user B's,
-- and a tenant default coexists with per-user overrides. '' = the shared/
-- operator/legacy tenant (the RFC N axis).
--
-- This table is EXCLUDED from snapshots (secrets don't ride out on backups; a
-- restore re-provisions), mirroring operator_token_defs.

CREATE TABLE credential_defs (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    scope       TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL DEFAULT '',
    name        TEXT        NOT NULL,
    backend     TEXT        NOT NULL,
    definition  JSONB       NOT NULL,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, scope, scope_id, name)
);
