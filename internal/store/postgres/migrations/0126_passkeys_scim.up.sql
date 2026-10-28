-- 0126_passkeys_scim.up.sql — RFC FA P6: passkeys (WebAuthn), their one-time
-- ceremonies, and inbound SCIM 2.0 resources.
--
-- user_passkeys holds what a WebAuthn relying party keeps: the credential
-- id, the PUBLIC key, the authenticator model (aaguid), the signature
-- counter, transports and flags. The private key never leaves the
-- authenticator, so a read of this table authenticates nobody. Binary values
-- are base64url TEXT (aaguid hex), as on the sqlite side. flagged_at marks a
-- credential refused for good (a signature counter that went backwards: a
-- cloned authenticator). Keyed on the credential id alone: credential ids are
-- globally unique, and a discoverable sign-in finds the user from it.
--
-- webauthn_ceremonies is the server's half of a begun ceremony (the
-- challenge and what it was begun for), keyed by the SHA-256 of the id the
-- browser holds. A consumer DELETEs the row to use it, so a challenge is
-- answered at most once across every replica.
--
-- scim_users / scim_groups / scim_group_members are the resources a kind:
-- scim gate's identity provider pushed. A SCIM user is an ordinary gate user
-- (users + user_identities at external subject = scim_id, made by the same
-- provisioning code as every identity gate); this table keeps the attributes
-- the provider expects back. A SCIM DELETE ends the resource (deleted_at) but
-- never the user, which goes through the gate's removal policy (RFC FA
-- §3.7c). user_name_key is the case-folded userName, unique per gate and
-- tenant among live resources; a subject has at most one live resource.
-- scim_group_members is what the provider asserts; the gate's group sync
-- turns it into loomcycle memberships of "<gate>:<display_name>" groups.
--
-- No FK to users: deleting a user removes its passkeys and SCIM resource
-- explicitly (AuthSubjectDelete), as for the password gate's rows.

CREATE TABLE IF NOT EXISTS user_passkeys (
    credential_id      TEXT        PRIMARY KEY,
    tenant_id          TEXT        NOT NULL DEFAULT '',
    subject            TEXT        NOT NULL,
    user_handle        TEXT        NOT NULL,
    public_key         TEXT        NOT NULL,
    attestation_type   TEXT        NOT NULL DEFAULT '',
    attestation_format TEXT        NOT NULL DEFAULT '',
    transports         TEXT        NOT NULL DEFAULT '[]',
    flags              INTEGER     NOT NULL DEFAULT 0,
    aaguid             TEXT        NOT NULL DEFAULT '',
    sign_count         BIGINT      NOT NULL DEFAULT 0,
    attachment         TEXT        NOT NULL DEFAULT '',
    discoverable       BOOLEAN     NOT NULL DEFAULT FALSE,
    name               TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at       TIMESTAMPTZ,
    flagged_at         TIMESTAMPTZ,
    flag_reason        TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS user_passkeys_by_subject ON user_passkeys (tenant_id, subject);

CREATE TABLE IF NOT EXISTS webauthn_ceremonies (
    ceremony_hash TEXT        PRIMARY KEY,
    kind          TEXT        NOT NULL,
    gate          TEXT        NOT NULL DEFAULT '',
    tenant_id     TEXT        NOT NULL DEFAULT '',
    subject       TEXT        NOT NULL DEFAULT '',
    binding       TEXT        NOT NULL DEFAULT '',
    session_data  TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS scim_users (
    gate          TEXT        NOT NULL,
    tenant_id     TEXT        NOT NULL,
    scim_id       TEXT        NOT NULL,
    subject       TEXT        NOT NULL,
    user_name     TEXT        NOT NULL,
    user_name_key TEXT        NOT NULL,
    external_id   TEXT        NOT NULL DEFAULT '',
    attributes    TEXT        NOT NULL DEFAULT '{}',
    version       BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,
    -- When the gate last claimed a re-enable of this user: re-enables are
    -- counted per window, and a re-enabled user does not raise the
    -- removal ceiling's base.
    reactivated_at TIMESTAMPTZ,
    PRIMARY KEY (gate, tenant_id, scim_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS scim_users_live_name ON scim_users (gate, tenant_id, user_name_key) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS scim_users_live_subject ON scim_users (gate, tenant_id, subject) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS scim_groups (
    gate         TEXT        NOT NULL,
    tenant_id    TEXT        NOT NULL,
    scim_id      TEXT        NOT NULL,
    display_name TEXT        NOT NULL,
    display_key  TEXT        NOT NULL,
    external_id  TEXT        NOT NULL DEFAULT '',
    version      BIGINT      NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (gate, tenant_id, scim_id),
    UNIQUE (gate, tenant_id, display_key)
);

CREATE TABLE IF NOT EXISTS scim_group_members (
    gate      TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    group_id  TEXT NOT NULL,
    subject   TEXT NOT NULL,
    PRIMARY KEY (gate, tenant_id, group_id, subject)
);
CREATE INDEX IF NOT EXISTS scim_group_members_by_subject ON scim_group_members (gate, tenant_id, subject);

-- The removal stage machine (0115) gains two columns. stage is the stage
-- the sweeper has claimed and is running ('unbinding' | 'erasing'), '' in
-- between: while it is set nobody re-enables the user, so a re-enable can
-- never race an unbind or an erasure. prior_status is the user's status
-- when the removal disabled it ('pending' carries an approval still owed
-- through a re-enable), '' when unknown.
ALTER TABLE auth_removals ADD COLUMN IF NOT EXISTS stage TEXT NOT NULL DEFAULT '';
ALTER TABLE auth_removals ADD COLUMN IF NOT EXISTS prior_status TEXT NOT NULL DEFAULT '';
