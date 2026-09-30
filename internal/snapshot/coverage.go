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
	// coverPending: not carried YET; a later phase of RFC DP adds its section.
	// Phase names the PR that flips it. The last phase deletes this kind.
	coverPending coverageKind = "pending"
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
	// Reason explains a never / omitted / pending classification, or a partial
	// capture of a section table.
	Reason string
	// Phase is the RFC DP phase that adds the section (coverPending only).
	Phase string
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
		Secrets: "reported: a literal header value (capture_findings)",
		Cache:   "mcp.DynamicRegistry (filled from the active defs at boot)"},
	"memory":                    {Kind: coverSection, Section: "memory", Secrets: "none", Backends: onBoth},
	"memory_backend_def_active": {Kind: coverSection, Section: "memory_backend_def_active", Secrets: "none", Backends: onBoth},
	"memory_backend_defs": {Kind: coverSection, Section: "memory_backend_defs", Backends: onBoth,
		Secrets: "by-reference: config.api_key_env, tenancy_strategy.env_pattern (env var names)"},
	"memory_embeddings": {Kind: coverSection, Section: "memory", Secrets: "none", Backends: onPostgres,
		Reason:      "travels as each memory entry's embedding",
		Conditional: "created only when the pgvector extension is available"},
	"runs": {Kind: coverSection, Section: "paused_runs", Backends: onBoth,
		Secrets: "reported: a literal header value in the run_config's recorded hooks (capture_findings)",
		Reason:  "only runs with pause_state='paused' travel; per-run secrets are not columns"},
	// A restored schedule's run state rides inside its def's entry, so a fire
	// count never travels without its def. The scheduler re-queries the store
	// every tick; it caches nothing a restore must refresh.
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
	"token_limits": {Kind: coverSection, Section: "token_limits", Secrets: "none", Backends: onBoth,
		Cache: "limits.Tracker (ceilings seeded at boot)"},
	// usage_carry is written FROM the token_limits section's usage_mtd block,
	// and read back into it by the next capture (limits.MonthToDate), so a
	// second hop keeps the first source's usage.
	"usage_carry": {Kind: coverSection, Section: "token_limits", Secrets: "none", Backends: onBoth,
		Reason: "travels as the aggregate month-to-date total, not as rows",
		Cache:  "limits.Tracker (month-to-date counters seeded at boot)"},
	"users": {Kind: coverSection, Section: "users", Secrets: "none", Backends: onBoth},
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
	"token_usage": {Kind: coverNever, Backends: onBoth,
		Reason: "the per-call billing ledger; only a month-to-date aggregate may travel, with token_limits"},
	"usage_archive": {Kind: coverNever, Backends: onBoth,
		Reason: "rolled-up billing ledger"},

	// ---- deliberately not carried -------------------------------------------
	"change_subscription_cursors": {Kind: coverOmitted, Backends: onBoth,
		Reason: "positions of external change subscribers; meaningless on another instance"},
	"channel_hook_state": {Kind: coverOmitted, Backends: onBoth,
		Reason: "advisory hook progress owned by the source; the target re-decides held messages"},
	"dynamic_agents": {Kind: coverOmitted, Backends: onBoth,
		Reason: "TTL-bound, run-scoped registrations"},
	"ephemeral_volume_defs": {Kind: coverOmitted, Backends: onBoth,
		Reason: "run-bound scratch volumes"},
	// interrupts: a pending ask blocks inside tool dispatch, so it cannot belong
	// to a run parked for a pause; everything else in the table is audit.
	"interrupts": {Kind: coverOmitted, Backends: onBoth,
		Reason: "a pending ask cannot belong to a paused run; the rest is audit"},
	"memory_changes": {Kind: coverOmitted, Backends: onBoth,
		Reason: "derived change log"},
	"memory_cursors": {Kind: coverOmitted, Backends: onBoth,
		Reason: "consolidation watermarks and a lease owned by a source replica"},
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
	"user_quotas": {Kind: coverOmitted, Backends: onPostgres,
		Reason: "live concurrency slots of the source cluster"},

	// ---- pending: a later phase adds the section ----------------------------
	"dirents":        {Kind: coverPending, Phase: "DP-P7", Backends: onBoth},
	"memory_pending": {Kind: coverPending, Phase: "DP-P6", Backends: onBoth},
}
