package snapshot

// Snapshot coverage (RFC DP §3.2 a).
//
// Every table in the store schema is classified here: carried by a named
// envelope section, never allowed in an envelope, or deliberately left out for
// a stated reason. TestSnapshotCoverage_EveryTableIsClassified reads the table
// set from a freshly migrated store on each backend and fails in both
// directions — on a table with no entry here, and on an entry naming a table
// the schema no longer has. So adding a table forces the question "does a
// restore need this, and is it safe in a portable file?" on whoever adds it,
// instead of waiting for the next census to find the gap.
//
// The map is data for that guard (and for the restore-side cache refresh the
// later phases add); nothing at run time reads it.

// coverageKind is how a table relates to the snapshot envelope.
type coverageKind string

const (
	// coverSection: the table's rows travel in the named envelope section.
	coverSection coverageKind = "section"
	// coverNever: the table's rows must never reach an envelope — secret
	// material, or billing data (§3.1). The planted-value test enforces it.
	coverNever coverageKind = "never"
	// coverOmitted: not carried, for a stated non-secret reason (derived,
	// instance-local, TTL-bound, operational).
	coverOmitted coverageKind = "omitted"
)

// backendSet says which store backends have a table. Most tables exist on
// both; a few are postgres-only because only a postgres deployment can run
// multi-replica or use pgvector.
type backendSet uint8

const (
	onSQLite backendSet = 1 << iota
	onPostgres
	onBoth = onSQLite | onPostgres
)

// tableCoverage classifies one table.
type tableCoverage struct {
	Kind coverageKind
	// Section is the envelope section key that carries the rows (coverSection
	// only). It must be a key of the Sections struct.
	Section string
	// Secrets says what secret material the carried rows can hold
	// (coverSection only), leading with one of: "none", "by-reference",
	// "stripped:", "reported:".
	Secrets string
	// Reason explains a never / omitted classification, or a partial capture
	// of a section table.
	Reason string
	// Backends is where the table exists.
	Backends backendSet
	// Conditional, when set, says why the table may legitimately be absent
	// from a migrated schema; the guard then does not require it to exist.
	Conditional string
	// Cache names an in-process cache over this table that is filled at boot
	// and that a restore must refresh, or the restored rows are not live until
	// a restart (§4.11).
	Cache string
}

// tableCoverageMap is the classification of every store table, grouped by
// kind and alphabetical within a group.
var tableCoverageMap = map[string]tableCoverage{
	// ---- carried by an existing section ------------------------------------
	// A2A defs hold no secret material: a peer's auth names a per-run
	// credential key, a card's security schemes are public descriptions and
	// sign_with_key_env is an env NAME. Every body is re-validated on restore.
	// The peer tools are enumerated per run from the store, and the card is
	// read per request, so no cache needs a refresh.
	"a2a_agent_def_active":       {Kind: coverSection, Section: "a2a_agent_def_active", Secrets: "none", Backends: onBoth},
	"a2a_agent_defs":             {Kind: coverSection, Section: "a2a_agent_defs", Secrets: "by-reference: auth.bearer_credential_ref (a per-run credential key)", Backends: onBoth},
	"a2a_server_card_def_active": {Kind: coverSection, Section: "a2a_server_card_def_active", Secrets: "none", Backends: onBoth},
	"a2a_server_card_defs":       {Kind: coverSection, Section: "a2a_server_card_defs", Secrets: "by-reference: sign_with_key_env (an env var name)", Backends: onBoth},
	"agent_def_active":           {Kind: coverSection, Section: "agent_def_active", Secrets: "none", Backends: onBoth},
	"agent_defs": {Kind: coverSection, Section: "agent_defs", Backends: onBoth,
		Secrets: "reported: a literal header value in an inline hook block (capture_findings)"},
	"channel_cursors":  {Kind: coverSection, Section: "channels", Secrets: "none", Backends: onBoth},
	"channel_messages": {Kind: coverSection, Section: "channels", Secrets: "none", Backends: onBoth},
	// The `channels` table holds the runtime channel DEFINITIONS; the
	// `channels` envelope section is messages + cursors + the yaml config.
	"channels": {Kind: coverSection, Section: "channel_defs", Backends: onBoth,
		Secrets: "reported: a literal header value in the channel's hooks (capture_findings)"},
	// A Path name is a pointer (a document id, a memory key, a volume name),
	// never content. Restored last, and only when what it names is here; the
	// Path tool reads the store per call, so nothing is cached.
	"dirents": {Kind: coverSection, Section: "dirents", Secrets: "none", Backends: onBoth,
		Reason: "restored last, and a name is skipped when what it names is not on the target"},
	// Memory-backend and document-source defs name env vars, never values:
	// config.api_key_env and tenancy_strategy.env_pattern. Every body is
	// re-validated on restore, since base_url + api_key_env is an
	// exfiltration pair. Both lookups read the active def from the store per
	// call, so no cache needs a refresh.
	"document_source_def_active": {Kind: coverSection, Section: "document_source_def_active", Secrets: "none", Backends: onBoth},
	"document_source_defs": {Kind: coverSection, Section: "document_source_defs", Backends: onBoth,
		Secrets: "by-reference: config.api_key_env, tenancy_strategy.env_pattern (env var names)"},
	"evaluations": {Kind: coverSection, Section: "evaluations", Secrets: "none", Backends: onBoth},
	"events": {Kind: coverSection, Section: "paused_runs", Secrets: "none", Backends: onBoth,
		Reason: "only the transcript events of paused runs travel, inside their run's entry"},
	"hook_def_active": {Kind: coverSection, Section: "hook_def_active", Secrets: "none", Backends: onBoth},
	"hook_defs": {Kind: coverSection, Section: "hook_defs", Backends: onBoth,
		Secrets: "reported: a literal header value in an http hook body (capture_findings)"},
	"mcp_server_def_active": {Kind: coverSection, Section: "mcp_server_def_active", Secrets: "none", Backends: onBoth,
		Cache: "mcp.DynamicRegistry (filled from the active defs at boot)"},
	"mcp_server_defs": {Kind: coverSection, Section: "mcp_server_defs", Backends: onBoth,
		Secrets: "reported: a literal header or stdio env value (capture_findings)",
		Cache:   "mcp.DynamicRegistry (filled from the active defs at boot)"},
	"memory":                    {Kind: coverSection, Section: "memory", Secrets: "none", Backends: onBoth},
	"memory_backend_def_active": {Kind: coverSection, Section: "memory_backend_def_active", Secrets: "none", Backends: onBoth},
	"memory_backend_defs": {Kind: coverSection, Section: "memory_backend_defs", Backends: onBoth,
		Secrets: "by-reference: config.api_key_env, tenancy_strategy.env_pattern (env var names)"},
	"memory_embeddings": {Kind: coverSection, Section: "memory", Secrets: "none", Backends: onPostgres,
		Reason:      "travels as each memory entry's embedding",
		Conditional: "created only when the pgvector extension is available"},
	// The queue's payload is the conversation to consolidate: user data, not
	// an operator secret. Only undrained rows travel, and never a lease (that
	// is memory_cursors). The consolidator reads the queue from the store per
	// pass; nothing is cached.
	"memory_pending": {Kind: coverSection, Section: "memory_pending", Secrets: "none", Backends: onBoth,
		Reason: "undrained rows only, captured before memory (at-least-once) and restored un-drained"},
	"runs": {Kind: coverSection, Section: "paused_runs", Backends: onBoth,
		Secrets: "reported: a literal header value in the run_config's recorded hooks (capture_findings)",
		// user_slot is deliberately not carried: it names a slot in the source
		// cluster's user_quotas, which does not travel, so a restored row reads
		// NULL and its resume gives nothing back (no replica id either).
		Reason: "only runs with pause_state='paused' travel; per-run secrets are not columns; user_slot is omitted (a source-cluster slot)"},
	// A restored schedule's run state rides inside its def's entry, so a fire
	// count never travels without its def. The scheduler re-queries the store
	// every tick; it caches nothing a restore must refresh.
	"schedule_active_runs": {Kind: coverSection, Section: "schedule_defs", Secrets: "none", Backends: onBoth,
		Reason: "travels as the active_runs of its def's entry; the reconciler reads it from the store each tick"},
	"schedule_def_active": {Kind: coverSection, Section: "schedule_def_active", Secrets: "none", Backends: onBoth},
	"schedule_defs": {Kind: coverSection, Section: "schedule_defs", Backends: onBoth,
		Secrets: "stripped: literal user_credentials values (keys kept as stripped_credentials; the def travels with enabled:false)"},
	"schedule_run_state": {Kind: coverSection, Section: "schedule_defs", Secrets: "none", Backends: onBoth,
		Reason: "travels as the run_state of its def's entry"},
	"skill_def_active": {Kind: coverSection, Section: "skill_def_active", Secrets: "none", Backends: onBoth},
	"skill_defs":       {Kind: coverSection, Section: "skill_defs", Secrets: "none", Backends: onBoth},
	"teamdef_active":   {Kind: coverSection, Section: "team_def_active", Secrets: "none", Backends: onBoth},
	"teamdefs": {Kind: coverSection, Section: "team_defs", Backends: onBoth,
		Secrets: "reported: a literal header value in an inline hook block (capture_findings)"},
	// A marker is the only record that an emptied tenant once had an owner,
	// and it is what refuses a second owner bootstrap there. The users rows
	// cannot re-derive it for a tenant that has none left, so a restore that
	// dropped it would hand such a tenant to whoever bootstraps it next.
	"tenant_bootstraps": {Kind: coverSection, Section: "users", Secrets: "none", Backends: onBoth,
		Reason: "carried as users.bootstraps (insert-or-ignore on restore); a snapshot written before that field restores markers only for the tenants whose users it creates"},
	"token_limits": {Kind: coverSection, Section: "token_limits", Secrets: "none", Backends: onBoth,
		Cache: "limits.Tracker (ceilings seeded at boot)"},
	// usage_carry is written FROM the token_limits section's usage_mtd block,
	// and read back into it by the next capture (limits.MonthToDate), so a
	// second hop keeps the first source's usage. Its estimated column (usage
	// counted without a provider's report) is part of that same total.
	"usage_carry": {Kind: coverSection, Section: "token_limits", Secrets: "none", Backends: onBoth,
		Reason: "travels as the aggregate month-to-date total, not as rows",
		Cache:  "limits.Tracker (month-to-date counters seeded at boot)"},
	"users": {Kind: coverSection, Section: "users", Secrets: "none", Backends: onBoth},
	// RFC EY access control (P1). Authority rows, no secret material; their
	// envelope section is a later phase of the RFC, so a snapshot restored
	// on another instance carries no ACL rows yet and the operator re-authors
	// (or re-declares in config) what the source had. Stated here rather
	// than left unclassified so the gap is a decision, not an accident.
	"acl_defs": {Kind: coverOmitted, Backends: onBoth,
		Reason: "RFC EY: ACL rule lists are not yet carried; a snapshot section lands in a later phase"},
	"user_groups": {Kind: coverOmitted, Backends: onBoth,
		Reason: "RFC EY: user groups are not yet carried; a snapshot section lands in a later phase"},
	"user_group_members": {Kind: coverOmitted, Backends: onBoth,
		Reason: "RFC EY: group memberships are not yet carried; a snapshot section lands in a later phase"},
	"acl_bindings": {Kind: coverOmitted, Backends: onBoth,
		Reason: "RFC EY: ACL bindings are not yet carried; a snapshot section lands in a later phase"},
	"acl_version": {Kind: coverOmitted, Backends: onBoth,
		Reason: "a per-tenant change counter the replicas' caches key on; re-derived from the next write"},
	// A volume's stored path is a filesystem grant lookup trusts, so it never
	// travels: the target derives it under its own dynamic root. lookup reads
	// the row per resolution, so nothing is cached.
	"volume_defs": {Kind: coverSection, Section: "volume_defs", Secrets: "none", Backends: onBoth,
		Reason: "name and mode only; the host path is re-derived on the target and the empty directory created there"},
	// The receiver re-reads the active def per delivery; nothing is cached.
	"webhook_def_active": {Kind: coverSection, Section: "webhook_def_active", Secrets: "none", Backends: onBoth},
	"webhook_defs": {Kind: coverSection, Section: "webhook_defs", Backends: onBoth,
		Secrets: "stripped: literal user_credentials values (keys kept as stripped_credentials; the def travels with enabled:false)"},

	// ---- never in an envelope (§3.1) ----------------------------------------
	"credential_defs": {Kind: coverNever, Backends: onBoth,
		Reason: "sealed tenant secrets keyed off the source's LOOMCYCLE_SECRET_KEY; even the names are withheld"},
	"operator_token_defs": {Kind: coverNever, Backends: onBoth,
		Reason: "bearer-authentication material; restoring one would make a source bearer valid on the target"},
	"ui_sessions": {Kind: coverNever, Backends: onBoth,
		Reason: "browser login sessions bound to one browser on THIS deployment; restoring one would let a source cookie authenticate on the target"},
	// RFC FL app sign-ins: hashes of authorization codes and refresh
	// tokens, and the families that bind them. Authentication material like
	// ui_sessions: a restored family would let a source app's refresh token
	// mint access tokens on the target.
	"oauth_codes": {Kind: coverNever, Backends: onBoth,
		Reason: "60-second single-use authorization codes issued on THIS deployment"},
	"oauth_refresh_families": {Kind: coverNever, Backends: onBoth,
		Reason: "app sign-ins on THIS deployment; restoring one would let a source refresh token mint access tokens on the target"},
	"oauth_refresh_tokens": {Kind: coverNever, Backends: onBoth,
		Reason: "hashes of app refresh tokens; carried, one would authenticate a source app on the target"},
	"token_usage": {Kind: coverNever, Backends: onBoth,
		Reason: "the per-call billing ledger; only a month-to-date aggregate may travel, with token_limits"},
	"usage_archive": {Kind: coverNever, Backends: onBoth,
		Reason: "rolled-up billing ledger"},
	// RFC FA P2 auth-gate state, treated like operator_token_defs and
	// credential_defs: authentication material never travels, and the
	// target re-issues logins. The credentials, second factors and codes are
	// peppered, sealed or hashed with keys the target does not hold (so they
	// would not even work there), and each is still offline-attackable or
	// replayable material. An identity holds no secret, but it is an
	// authentication binding: for an OIDC gate the (issuer, sub) row alone
	// lets that external account log in as the user. The users section
	// carries the users (and the tenant_bootstraps markers); a restored user
	// has no login until one is issued on the target.
	"user_identities": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: login-to-user bindings with no secret, but restoring one would let a source login (an OIDC subject, a password login) sign in on the target"},
	"user_credentials": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: password hashes peppered with a key outside the database; secret material like an operator token hash, and useless on a target without that pepper"},
	"user_totp": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: TOTP secrets sealed under the source's deployment key, like credential_defs ciphertext"},
	"user_recovery_codes": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: hashes of single-use second-factor recovery codes; a carried code would bypass the second factor on the target"},
	"auth_invites": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: hashes of single-use sign-up codes; a restored live invite would admit a sign-up (or attach a password to a user) on the target"},
	"auth_resets": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: hashes of single-use password-reset codes; a restored live reset would set a user's password on the target"},
	// RFC FA P4: the replay cache holds only labels of assertions already
	// used here, and a removal is a schedule (unbind, erase) for THIS
	// deployment's lifecycle policy. The users section carries the disabled
	// status, so a restored removed user stays disabled; the target's
	// operator re-starts the removal if its later stages are wanted there.
	"auth_assertion_jtis": {Kind: coverOmitted, Backends: onBoth,
		Reason: "minutes-long replay cache of external-gate assertions used on THIS deployment"},
	"auth_removals": {Kind: coverOmitted, Backends: onBoth,
		Reason: "the source's removal schedule (unbind/erase due times); the carried users keep their disabled status"},
	"retention_overrides": {Kind: coverNever, Backends: onBoth,
		Reason: "a timed decision (24 hours at most) to store one agent's runs on THIS deployment; carrying it would switch storage on somewhere nobody asked for it"},

	// ---- deliberately not carried -------------------------------------------
	"change_subscription_cursors": {Kind: coverOmitted, Backends: onBoth,
		Reason: "positions of external change subscribers; meaningless on another instance"},
	"channel_hook_state": {Kind: coverOmitted, Backends: onBoth,
		Reason: "advisory hook progress owned by the source; the target re-decides held messages"},
	"dynamic_agents": {Kind: coverOmitted, Backends: onBoth,
		Reason: "TTL-bound, run-scoped registrations"},
	"ephemeral_volume_defs": {Kind: coverOmitted, Backends: onBoth,
		Reason: "run-bound scratch volumes"},
	// interrupts: a pending ask blocks inside tool dispatch, and a pause parks a
	// run only at an iteration boundary, so a paused run cannot be waiting on
	// one; everything else in the table is audit. Capture checks the
	// assumption for every paused run it carries and reports a violation as a
	// capture finding (see pausedRunInterruptFindings), so a future interrupt
	// kind that parks at a boundary is loud rather than silently dropped.
	"interrupts": {Kind: coverOmitted, Backends: onBoth,
		Reason: "a pending ask cannot belong to a paused run (capture checks it); the rest is audit"},
	"memory_changes": {Kind: coverOmitted, Backends: onBoth,
		Reason: "derived change log"},
	"memory_cursors": {Kind: coverOmitted, Backends: onBoth,
		Reason: "consolidation watermarks and a lease owned by a source replica"},
	"auth_login_states": {Kind: coverNever, Backends: onBoth,
		Reason: "not carried: ten-minute half-finished redirect logins bound to one browser on THIS deployment"},
	"auth_throttle": {Kind: coverOmitted, Backends: onBoth,
		Reason: "not carried: short-lived login-failure counters of the source (no secret); they mean nothing on an instance that did not see the failures"},
	"process_samples": {Kind: coverOmitted, Backends: onBoth,
		Reason: "operational metrics of the source process"},
	"replicas": {Kind: coverOmitted, Backends: onPostgres,
		Reason: "the source cluster's replica membership"},
	"runtime_state": {Kind: coverOmitted, Backends: onPostgres,
		Reason: "the source cluster's pause/resume state"},
	"schema_migrations": {Kind: coverOmitted, Backends: onPostgres,
		Reason: "the target's own migrator stamps its schema version"},
	"session_embeddings": {Kind: coverOmitted, Backends: onBoth,
		Reason: "derived from transcripts and rebuildable"},
	"sessions": {Kind: coverOmitted, Backends: onBoth,
		Reason: "restore synthesizes a session per paused run from the run entry"},
	"snapshots": {Kind: coverOmitted, Backends: onBoth,
		Reason: "the snapshot store itself"},
	"team_webhook_arms": {Kind: coverOmitted, Backends: onBoth,
		Reason: "leases of walks running on the source; a restored instance runs none, and a walk re-arms its own"},
	"user_quotas": {Kind: coverOmitted, Backends: onPostgres,
		Reason: "live concurrency slots of the source cluster"},
	"webhook_deliveries": {Kind: coverOmitted, Backends: onBoth,
		Reason: "TTL-bound replay guard of the deliveries the source accepted, like the receiver's in-process one"},
	"webhook_delivery_counts": {Kind: coverOmitted, Backends: onBoth,
		Reason: "operational counts of the source's deliveries; a restored instance counts its own from zero"},
}
