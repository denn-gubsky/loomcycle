package sqlite

// volumesSection: volume, credential and ephemeral volume definitions,
// dirents (the Path tree) and operator tokens.
func volumesSection() schemaPlan {
	return schemaPlan{tables: []string{
		// RFC AH Phase 2a — persistent dynamic volumes. FLAT (tenant_id,
		// name) table, NOT the versioned Def shape: a Volume points at
		// mutable on-disk state outside the def, so there is no version,
		// parent_def_id, content_sha256, or active pointer. definition holds
		// the runtime-derived {"path":..,"mode":..}; the path is always
		// <dynamic_root>/<tenant-segment>/<name> derived by the tool.
		`CREATE TABLE IF NOT EXISTS volume_defs (
			tenant_id   TEXT NOT NULL DEFAULT '',
			name        TEXT NOT NULL,
			definition  TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, name)
		)`,
		// RFC AR CredentialDef — the secure per-tenant credential store. Flat
		// (tenant_id, scope, scope_id, name) key; definition holds ONLY sealed
		// ciphertext (inline backend, see internal/credential) or an external
		// pointer — never a plaintext secret. scope/scope_id isolate user A's
		// token from user B's. Excluded from snapshots. A row under scope
		// 'system' is a marker v1.119.0 wrote for its boot-time token rewrap
		// (a non-secret note, not ciphertext); nothing reads that scope now.
		`CREATE TABLE IF NOT EXISTS credential_defs (
			tenant_id   TEXT NOT NULL DEFAULT '',
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL DEFAULT '',
			name        TEXT NOT NULL,
			backend     TEXT NOT NULL,
			definition  TEXT NOT NULL,
			expires_at  INTEGER,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, scope, scope_id, name)
		)`,
		// RFC AL Path primitive — the dirent (path tree) substrate. Maps a
		// (tenant_id, scope, scope_id, parent_path, name) coordinate to a
		// backing resource (kind + resource_ref json). PK is the full
		// coordinate so each (tenant, scope, scope_id) tree is independent and
		// a name is unique within its parent directory. The composite PK is
		// also the lookup index for resolve/ls; parent_path enables one-level
		// (=) and recursive (prefix) listings.
		`CREATE TABLE IF NOT EXISTS dirents (
			tenant_id    TEXT NOT NULL DEFAULT '',
			scope        TEXT NOT NULL,
			scope_id     TEXT NOT NULL DEFAULT '',
			parent_path  TEXT NOT NULL,
			name         TEXT NOT NULL,
			kind         TEXT NOT NULL,
			resource_ref TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, scope, scope_id, parent_path, name)
		)`,
		// The reverse of a dirent (RFC FQ): which names a document has. An
		// expression index, so no column and no writer has to keep it current;
		// sqlshared.direntDocumentID must spell the expression identically.
		`CREATE INDEX IF NOT EXISTS dirents_document_id ON dirents (tenant_id, scope, scope_id, json_extract(resource_ref, '$.document_id')) WHERE kind = 'document'`,
		// RFC AH Phase 2b — ephemeral (run-tree-scoped) volumes. SEPARATE
		// from volume_defs: PK (root_run_id, name), NOT (tenant_id, name) —
		// two concurrent runs (any tenant) can each own a `work` volume with
		// no clobber. tenant_id is carried for the purge fence; definition is
		// the runtime-derived {"path":..,"mode":..} where path is always
		// <dynamic_root>/_ephemeral/<root_run_id>/<name>. The row backs the
		// inline run-completion purge + the crash-recovery sweeper; the
		// in-memory EphemeralVolumeSet is the resolution source.
		`CREATE TABLE IF NOT EXISTS ephemeral_volume_defs (
			root_run_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			tenant_id   TEXT NOT NULL DEFAULT '',
			definition  TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			PRIMARY KEY (root_run_id, name)
		)`,
		// RFC L OSS multi-tenant authorization — bearer tokens bound to
		// an authoritative principal (tenant_id + subject + allowed_scopes).
		// NOT versioned/forkable: no version, no active pointer, no parent.
		// Rotation via rotated_from; validity via retired_at (NULL or
		// future = valid). token_hash = SHA-256(pepper‖token); plaintext
		// never stored. See internal/store/postgres/migrations/
		// 0035_operator_token_defs.up.sql for the full rationale.
		`CREATE TABLE IF NOT EXISTS operator_token_defs (
			def_id               TEXT    PRIMARY KEY,
			name                 TEXT    NOT NULL,
			tenant_id            TEXT    NOT NULL,
			subject              TEXT    NOT NULL,
			token_hash           TEXT    NOT NULL,
			allowed_scopes       TEXT    NOT NULL,
			created_at           INTEGER NOT NULL,
			created_by_agent_id  TEXT,
			created_by_run_id    TEXT,
			rotated_from         TEXT    REFERENCES operator_token_defs(def_id),
			retired_at           INTEGER,
			UNIQUE(token_hash)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_operator_token_hash ON operator_token_defs(token_hash)`,
		`CREATE INDEX IF NOT EXISTS operator_token_defs_by_name ON operator_token_defs(name, created_at DESC)`,
	}}
}
