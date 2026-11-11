package sqlite

// authSection: users, access-control lists, groups and bindings, the
// sign-in gates (identities, passwords, TOTP, recovery codes, invites, resets),
// tenant bootstraps, login throttling and state, removals, device
// authorizations and tenant auth policies.
func authSection() schemaPlan {
	return schemaPlan{tables: []string{
		// RFC BX Phase 2 / P2a — the tenant-owned first-class users table.
		// Mirrors postgres migration 0068. A "user" was previously derived
		// from runs.user_id (no row to write); this makes identity a real,
		// tenant-owned record a substrate:tenant operator can manage.
		// access_mode is the collaboration dial ('tenant' = whole-tenant
		// collaboration, the default preserving today's behaviour; 'isolated'
		// = sandboxed); status is 'active' | 'disabled'. unix-nano created_at
		// stamped in code (sqlite has no now() default idiom here).
		`CREATE TABLE IF NOT EXISTS users (
			tenant_id    TEXT    NOT NULL DEFAULT '',
			subject      TEXT    NOT NULL,
			display_name TEXT    NOT NULL DEFAULT '',
			access_mode  TEXT    NOT NULL DEFAULT 'tenant',
			status       TEXT    NOT NULL DEFAULT 'active',
			created_at   INTEGER NOT NULL,
			created_by   TEXT,
			PRIMARY KEY (tenant_id, subject)
		)`,
		// RFC EY access control — see internal/store/postgres/migrations/
		// 0109_acl.up.sql for the design. Flat (tenant_id, name) rows with
		// a version bumped on every write; retire is reversible; groups
		// nest via member_kind='group'; one acl_version row per tenant is
		// bumped in the same transaction as every write.
		`CREATE TABLE IF NOT EXISTS acl_defs (
			tenant_id       TEXT    NOT NULL DEFAULT '',
			name            TEXT    NOT NULL,
			version         INTEGER NOT NULL DEFAULT 1,
			content_sha256  TEXT    NOT NULL,
			catalog_version INTEGER NOT NULL,
			rules           TEXT    NOT NULL,
			description     TEXT    NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL,
			created_by      TEXT    NOT NULL DEFAULT '',
			retired_at      INTEGER,
			PRIMARY KEY (tenant_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS user_groups (
			tenant_id   TEXT    NOT NULL DEFAULT '',
			name        TEXT    NOT NULL,
			description TEXT    NOT NULL DEFAULT '',
			external_id TEXT    NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			created_by  TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS user_group_members (
			tenant_id   TEXT    NOT NULL DEFAULT '',
			group_name  TEXT    NOT NULL,
			member_kind TEXT    NOT NULL,
			member_id   TEXT    NOT NULL,
			added_at    INTEGER NOT NULL,
			added_by    TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, group_name, member_kind, member_id)
		)`,
		`CREATE INDEX IF NOT EXISTS user_group_members_by_member ON user_group_members(tenant_id, member_kind, member_id)`,
		`CREATE TABLE IF NOT EXISTS acl_bindings (
			binding_id   TEXT    PRIMARY KEY,
			tenant_id    TEXT    NOT NULL DEFAULT '',
			acl_name     TEXT    NOT NULL,
			subject_kind TEXT    NOT NULL,
			subject_id   TEXT    NOT NULL DEFAULT '',
			expires_at   INTEGER,
			created_at   INTEGER NOT NULL,
			created_by   TEXT    NOT NULL DEFAULT '',
			retired_at   INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS acl_bindings_by_subject ON acl_bindings(tenant_id, subject_kind, subject_id) WHERE retired_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS acl_version (
			tenant_id  TEXT    PRIMARY KEY,
			version    INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// RFC FA P2 password gate. Mirrors postgres migration 0111 (see
		// 0111_password_gate.up.sql for the design): logins → users, and the
		// gate's hashed / peppered / sealed material only. Unix-nano INTEGER
		// timestamps, NULL = unset; INTEGER 0/1 booleans.
		`CREATE TABLE IF NOT EXISTS user_identities (
			tenant_id        TEXT    NOT NULL DEFAULT '',
			subject          TEXT    NOT NULL,
			gate             TEXT    NOT NULL,
			issuer           TEXT    NOT NULL DEFAULT '',
			external_subject TEXT    NOT NULL,
			email            TEXT    NOT NULL DEFAULT '',
			email_verified   INTEGER NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL,
			last_login_at    INTEGER,
			PRIMARY KEY (gate, issuer, external_subject),
			UNIQUE (tenant_id, subject, gate)
		)`,
		`CREATE TABLE IF NOT EXISTS user_credentials (
			tenant_id    TEXT    NOT NULL DEFAULT '',
			subject      TEXT    NOT NULL,
			kind         TEXT    NOT NULL,
			phc          TEXT    NOT NULL,
			pepper_kid   TEXT    NOT NULL DEFAULT '',
			updated_at   INTEGER NOT NULL,
			failed_count INTEGER NOT NULL DEFAULT 0,
			locked_until INTEGER,
			PRIMARY KEY (tenant_id, subject, kind)
		)`,
		`CREATE TABLE IF NOT EXISTS user_totp (
			tenant_id    TEXT    NOT NULL DEFAULT '',
			subject      TEXT    NOT NULL,
			sealed       TEXT    NOT NULL,
			created_at   INTEGER NOT NULL,
			confirmed_at INTEGER,
			last_step    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (tenant_id, subject)
		)`,
		`CREATE TABLE IF NOT EXISTS user_recovery_codes (
			tenant_id  TEXT    NOT NULL DEFAULT '',
			subject    TEXT    NOT NULL,
			code_hash  TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			used_at    INTEGER,
			PRIMARY KEY (tenant_id, subject, code_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS auth_invites (
			invite_id        TEXT    PRIMARY KEY,
			code_hash        TEXT    NOT NULL UNIQUE,
			gate             TEXT    NOT NULL,
			tenant_id        TEXT    NOT NULL DEFAULT '',
			subject          TEXT    NOT NULL DEFAULT '',
			access_mode      TEXT    NOT NULL DEFAULT '',
			groups           TEXT    NOT NULL DEFAULT '[]',
			login            TEXT    NOT NULL DEFAULT '',
			email            TEXT    NOT NULL DEFAULT '',
			return_to        TEXT    NOT NULL DEFAULT '',
			bootstrap        INTEGER NOT NULL DEFAULT 0,
			issued_by_manager INTEGER NOT NULL DEFAULT 0,
			created_by       TEXT    NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			expires_at       INTEGER NOT NULL,
			redeemed_at      INTEGER,
			redeemed_subject TEXT    NOT NULL DEFAULT '',
			revoked_at       INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS auth_invites_by_gate_tenant ON auth_invites(gate, tenant_id)`,
		`CREATE TABLE IF NOT EXISTS auth_resets (
			reset_id   TEXT    PRIMARY KEY,
			code_hash  TEXT    NOT NULL UNIQUE,
			gate       TEXT    NOT NULL,
			tenant_id  TEXT    NOT NULL DEFAULT '',
			subject    TEXT    NOT NULL,
			issued_by_manager INTEGER NOT NULL DEFAULT 0,
			created_by TEXT    NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			used_at    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS auth_resets_by_subject ON auth_resets(tenant_id, subject)`,
		// A tenant that has ever had a user, never deleted (postgres 0111
		// explains why). The backfill is idempotent and runs every open, so
		// a database upgraded across this release marks its tenants too.
		`CREATE TABLE IF NOT EXISTS tenant_bootstraps (
			tenant_id TEXT    PRIMARY KEY,
			at        INTEGER NOT NULL,
			via       TEXT    NOT NULL DEFAULT ''
		)`,
		`INSERT OR IGNORE INTO tenant_bootstraps (tenant_id, at, via)
			SELECT DISTINCT tenant_id, 0, 'backfill' FROM users`,
		`CREATE TABLE IF NOT EXISTS auth_throttle (
			throttle_key  TEXT    PRIMARY KEY,
			failures      INTEGER NOT NULL DEFAULT 0,
			window_start  INTEGER NOT NULL,
			window_end    INTEGER NOT NULL,
			blocked_until INTEGER
		)`,
		// RFC FA P1: started redirect logins (OIDC / OAuth code flows).
		// Mirrors postgres migration 0112 (see its comment for the design).
		`CREATE TABLE IF NOT EXISTS auth_login_states (
			state_hash        TEXT    PRIMARY KEY,
			gate              TEXT    NOT NULL,
			binding_hash      TEXT    NOT NULL,
			purpose           TEXT    NOT NULL DEFAULT 'login',
			return_to         TEXT    NOT NULL DEFAULT '',
			reauth            INTEGER NOT NULL DEFAULT 0,
			link_tenant       TEXT    NOT NULL DEFAULT '',
			link_subject      TEXT    NOT NULL DEFAULT '',
			link_session_hash TEXT    NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			expires_at        INTEGER NOT NULL,
			consumed_at       INTEGER
		)`,
		// RFC FA P4 external gates. Mirrors postgres migration 0115 (see
		// 0115_auth_external_gates.up.sql): the shared assertion replay
		// cache and the user removal stage machine.
		`CREATE TABLE IF NOT EXISTS auth_assertion_jtis (
			gate       TEXT    NOT NULL,
			jti        TEXT    NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY (gate, jti)
		)`,
		`CREATE INDEX IF NOT EXISTS auth_assertion_jtis_by_expiry ON auth_assertion_jtis(expires_at)`,
		`CREATE TABLE IF NOT EXISTS auth_removals (
			tenant_id     TEXT    NOT NULL DEFAULT '',
			subject       TEXT    NOT NULL,
			gate          TEXT    NOT NULL DEFAULT '',
			reason        TEXT    NOT NULL,
			disabled_at   INTEGER NOT NULL,
			unbind_due_at INTEGER NOT NULL,
			erase_due_at  INTEGER,
			unbound_at    INTEGER,
			erased_at     INTEGER,
			created_by    TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, subject)
		)`,
		`CREATE INDEX IF NOT EXISTS auth_removals_by_unbind_due ON auth_removals(unbind_due_at) WHERE unbound_at IS NULL`,
		// RFC FA P5 device login and per-tenant gate policy. Mirrors
		// postgres migration 0118 (see 0118_device_gate.up.sql for the
		// design); times are unix nanos, interval_ms milliseconds.
		`CREATE TABLE IF NOT EXISTS device_authorizations (
			device_code_hash TEXT    PRIMARY KEY,
			user_code_hash   TEXT    NOT NULL UNIQUE,
			client_id        TEXT    NOT NULL,
			source_addr      TEXT    NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			expires_at       INTEGER NOT NULL,
			interval_ms      INTEGER NOT NULL,
			last_polled_at   INTEGER,
			last_poll_early  INTEGER NOT NULL DEFAULT 0,
			status           TEXT    NOT NULL DEFAULT 'pending',
			decided_tenant   TEXT    NOT NULL DEFAULT '',
			decided_subject  TEXT    NOT NULL DEFAULT '',
			decided_gate     TEXT    NOT NULL DEFAULT '',
			decided_at       INTEGER,
			decided_session  TEXT    NOT NULL DEFAULT '',
			decided_cred_mac TEXT    NOT NULL DEFAULT '',
			decided_not_after INTEGER,
			redeemed_at      INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS device_authorizations_by_expiry ON device_authorizations(expires_at)`,
		`CREATE TABLE IF NOT EXISTS tenant_auth_policies (
			tenant_id  TEXT    PRIMARY KEY,
			gates      TEXT    NOT NULL,
			updated_at INTEGER NOT NULL,
			updated_by TEXT    NOT NULL DEFAULT ''
		)`,
		// Per-tenant telemetry destinations (tenant_telemetry.go). Mirrors
		// postgres migration 0133; signals is a JSON array, times unix nanos.
		`CREATE TABLE IF NOT EXISTS tenant_telemetry_policies (
			tenant_id          TEXT    PRIMARY KEY,
			endpoint           TEXT    NOT NULL,
			protocol           TEXT    NOT NULL,
			headers_credential TEXT    NOT NULL DEFAULT '',
			signals            TEXT    NOT NULL,
			sampler_ratio      REAL    NOT NULL,
			updated_at         INTEGER NOT NULL,
			updated_by         TEXT    NOT NULL DEFAULT ''
		)`,
	}}
}

// oauthSection: OAuth codes and refresh tokens, client definitions,
// passkeys, WebAuthn ceremonies and SCIM users and groups.
func oauthSection() schemaPlan {
	return schemaPlan{tables: []string{
		// RFC FL P0 app sign-in. Mirrors postgres migration 0123 (see
		// 0123_oauth_app_sign_in.up.sql): authorization codes, refresh-token
		// families and their refresh tokens — hashes only.
		`CREATE TABLE IF NOT EXISTS oauth_codes (
			code_hash      TEXT    PRIMARY KEY,
			client_id      TEXT    NOT NULL,
			redirect_uri   TEXT    NOT NULL,
			code_challenge TEXT    NOT NULL,
			tenant_id      TEXT    NOT NULL,
			subject        TEXT    NOT NULL,
			gate           TEXT    NOT NULL,
			amr            TEXT    NOT NULL DEFAULT '[]',
			session_hash   TEXT    NOT NULL DEFAULT '',
			scope_cap      TEXT    NOT NULL DEFAULT '[]',
			restrictions   TEXT    NOT NULL DEFAULT '',
			created_at     INTEGER NOT NULL,
			expires_at     INTEGER NOT NULL,
			consumed_at    INTEGER,
			family_id      TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_codes_by_expiry ON oauth_codes(expires_at)`,
		`CREATE INDEX IF NOT EXISTS oauth_codes_by_client ON oauth_codes(client_id) WHERE consumed_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS oauth_refresh_families (
			family_id           TEXT    PRIMARY KEY,
			client_id           TEXT    NOT NULL DEFAULT '',
			tenant_id           TEXT    NOT NULL DEFAULT '',
			subject             TEXT    NOT NULL DEFAULT '',
			gate                TEXT    NOT NULL DEFAULT '',
			amr                 TEXT    NOT NULL DEFAULT '[]',
			scope_cap           TEXT    NOT NULL DEFAULT '[]',
			restrictions        TEXT    NOT NULL DEFAULT '',
			created_at          INTEGER NOT NULL,
			last_used_at        INTEGER NOT NULL,
			idle_expires_at     INTEGER NOT NULL,
			absolute_expires_at INTEGER NOT NULL,
			revoked_at          INTEGER,
			revoke_reason       TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_client ON oauth_refresh_families(client_id) WHERE revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_subject ON oauth_refresh_families(tenant_id, subject)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_expiry ON oauth_refresh_families(absolute_expires_at)`,
		`CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
			token_hash TEXT    PRIMARY KEY,
			family_id  TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			used_at    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_tokens_by_family ON oauth_refresh_tokens(family_id)`,
		// RFC FL P0b runtime app clients. Mirrors postgres migration 0124
		// (see 0124_client_defs.up.sql).
		`CREATE TABLE IF NOT EXISTS client_defs (
			def_id            TEXT    PRIMARY KEY,
			name              TEXT    NOT NULL,
			tenant_id         TEXT    NOT NULL DEFAULT '',
			version           INTEGER NOT NULL,
			definition        TEXT    NOT NULL,
			content_sha256    TEXT    NOT NULL,
			description       TEXT    NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			created_by        TEXT    NOT NULL DEFAULT '',
			created_by_tenant TEXT    NOT NULL DEFAULT '',
			retired_at        INTEGER,
			UNIQUE (name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS client_defs_by_tenant ON client_defs(tenant_id)`,
		// RFC FA P6 passkeys, WebAuthn ceremonies and SCIM resources.
		// Mirrors postgres migration 0126 (see 0126_passkeys_scim.up.sql
		// for the design).
		`CREATE TABLE IF NOT EXISTS user_passkeys (
			credential_id      TEXT    PRIMARY KEY,
			tenant_id          TEXT    NOT NULL DEFAULT '',
			subject            TEXT    NOT NULL,
			user_handle        TEXT    NOT NULL,
			public_key         TEXT    NOT NULL,
			attestation_type   TEXT    NOT NULL DEFAULT '',
			attestation_format TEXT    NOT NULL DEFAULT '',
			transports         TEXT    NOT NULL DEFAULT '[]',
			flags              INTEGER NOT NULL DEFAULT 0,
			aaguid             TEXT    NOT NULL DEFAULT '',
			sign_count         INTEGER NOT NULL DEFAULT 0,
			attachment         TEXT    NOT NULL DEFAULT '',
			discoverable       INTEGER NOT NULL DEFAULT 0,
			name               TEXT    NOT NULL DEFAULT '',
			created_at         INTEGER NOT NULL,
			last_used_at       INTEGER,
			flagged_at         INTEGER,
			flag_reason        TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS user_passkeys_by_subject ON user_passkeys(tenant_id, subject)`,
		`CREATE TABLE IF NOT EXISTS webauthn_ceremonies (
			ceremony_hash TEXT    PRIMARY KEY,
			kind          TEXT    NOT NULL,
			gate          TEXT    NOT NULL DEFAULT '',
			tenant_id     TEXT    NOT NULL DEFAULT '',
			subject       TEXT    NOT NULL DEFAULT '',
			binding       TEXT    NOT NULL DEFAULT '',
			session_data  TEXT    NOT NULL,
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS scim_users (
			gate          TEXT    NOT NULL,
			tenant_id     TEXT    NOT NULL,
			scim_id       TEXT    NOT NULL,
			subject       TEXT    NOT NULL,
			user_name     TEXT    NOT NULL,
			user_name_key TEXT    NOT NULL,
			external_id   TEXT    NOT NULL DEFAULT '',
			attributes    TEXT    NOT NULL DEFAULT '{}',
			version       INTEGER NOT NULL DEFAULT 1,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			deleted_at    INTEGER,
			PRIMARY KEY (gate, tenant_id, scim_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS scim_users_live_name ON scim_users(gate, tenant_id, user_name_key) WHERE deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS scim_users_live_subject ON scim_users(gate, tenant_id, subject) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS scim_groups (
			gate         TEXT    NOT NULL,
			tenant_id    TEXT    NOT NULL,
			scim_id      TEXT    NOT NULL,
			display_name TEXT    NOT NULL,
			display_key  TEXT    NOT NULL,
			external_id  TEXT    NOT NULL DEFAULT '',
			version      INTEGER NOT NULL DEFAULT 1,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (gate, tenant_id, scim_id),
			UNIQUE (gate, tenant_id, display_key)
		)`,
		`CREATE TABLE IF NOT EXISTS scim_group_members (
			gate      TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			group_id  TEXT NOT NULL,
			subject   TEXT NOT NULL,
			PRIMARY KEY (gate, tenant_id, group_id, subject)
		)`,
		`CREATE INDEX IF NOT EXISTS scim_group_members_by_subject ON scim_group_members(gate, tenant_id, subject)`,
	}}
}
