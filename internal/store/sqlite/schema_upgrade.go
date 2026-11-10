package sqlite

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// columnsSection1 and columnsSection2 hold the column-add pass, in the order
// the columns were added. That order is the upgrade history and runs across
// areas, so it stays one list here rather than moving to each area's section
// (which would change the order the ALTERs run in).
//
// v0.4 additive columns + indexes for tracking + cancel.
//
// Order matters only in that ALTER must precede the partial indexes
// that reference the new columns.
func columnsSection1() schemaPlan {
	return schemaPlan{columns: []string{
		`ALTER TABLE sessions ADD COLUMN user_id TEXT`,
		`ALTER TABLE runs ADD COLUMN agent_id TEXT`,
		`ALTER TABLE runs ADD COLUMN parent_agent_id TEXT`,
		`ALTER TABLE runs ADD COLUMN parent_run_id TEXT`,
		`ALTER TABLE runs ADD COLUMN user_id TEXT`,
		`ALTER TABLE runs ADD COLUMN last_heartbeat_at INTEGER`,
		// v0.8.2: user_tier marker (PR #52). Nullable on legacy rows;
		// new rows carry the name of the user_tier policy applied at
		// run creation. Compliance + cost-retro queries facet on this.
		`ALTER TABLE runs ADD COLUMN user_tier TEXT`,
		// v0.8.5: agent_def_id audit column. NULL = the run resolved
		// against the static cfg.Agents fallback (no DB-versioned def).
		// Non-NULL = the run targeted a specific (name, version) row
		// in agent_defs. Distinguishes static-resolved from DB-resolved
		// runs without a separate flag.
		`ALTER TABLE runs ADD COLUMN agent_def_id TEXT`,
		// v0.8.17: pause_state for the runtime-wide quiesce protocol.
		// Three values are valid at the Store boundary: 'running'
		// (default), 'pausing' (operator issued pause; loop winding
		// down), 'paused' (loop reached iteration boundary; awaiting
		// resume). SetRunPauseState validates the value; the column
		// has no CHECK constraint because SQLite-version-portability
		// is more valuable than a redundant guard.
		`ALTER TABLE runs ADD COLUMN pause_state TEXT NOT NULL DEFAULT 'running'`,
		// v0.8.6 system channels: visible_at + published_by_user_id on
		// channel_messages. Idempotent ALTER for existing v0.8.4 / v0.8.5
		// schemas; on fresh deploys the CREATE TABLE already declared
		// them and the duplicate-column-name guard short-circuits these.
		`ALTER TABLE channel_messages ADD COLUMN visible_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE channel_messages ADD COLUMN published_by_user_id TEXT`,
		// Channel tenant axis. Idempotent ALTER for existing DBs; the
		// fresh CREATE TABLE above already declares the column (the
		// duplicate-column guard short-circuits these on fresh deploys).
		// SQLite can't rewrite the PK in place, so rebuildTenantBlindKeys
		// replaces an upgraded table's old (channel, scope, scope_id[, id]) /
		// (name) PK once these columns exist.
		`ALTER TABLE channel_messages ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channel_cursors ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channels ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC CY: a held channel stores without delivering until a
		// release. Mirrors postgres migration 0075. Idempotent ALTER for
		// existing DBs; the CREATE TABLE above already declares it.
		`ALTER TABLE channels ADD COLUMN hold INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE channels ADD COLUMN hooks TEXT`,
		// Channel hooks: a message's hook context. Mirrors postgres
		// migration 0084; the CREATE TABLE above already declares them.
		`ALTER TABLE channel_messages ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channel_messages ADD COLUMN hook_tenant TEXT`,
		`ALTER TABLE channel_messages ADD COLUMN requested_visible_at INTEGER`,
		// v0.9.x content_sha256 — see internal/store/postgres/migrations/
		// 0018_agent_defs_content_sha256.up.sql for the rationale. NULL
		// until the boot-time backfill walks pre-migration rows.
		`ALTER TABLE agent_defs ADD COLUMN content_sha256 TEXT`,
		`ALTER TABLE skill_defs ADD COLUMN content_sha256 TEXT`,
		// v0.12.7+ provider — actual provider that served the final
		// successful iteration. Idempotent on fresh deploys (the
		// CREATE TABLE above already declares it).
		`ALTER TABLE runs ADD COLUMN provider TEXT`,
		// v0.12.x replica_id on process_samples — lets a shared
		// (cluster-mode, Postgres) table split per replica. SQLite never
		// runs in cluster mode (boot refuses REPLICA_ID + sqlite), so this
		// stays NULL here; added for schema/struct parity. Idempotent on
		// fresh deploys (the CREATE TABLE above already declares it).
		`ALTER TABLE process_samples ADD COLUMN replica_id TEXT`,
		// v0.12.x parent_context — opaque caller-tracking lineage (JSON),
		// set on the root run and copied onto every sub-agent. NULL on
		// legacy rows + runs with no context. Not a secret (safe to
		// persist). Read back via DecodeParentContext.
		`ALTER TABLE runs ADD COLUMN parent_context TEXT`,
		// RFC H Decision 10 "Layer 2" durable dedup — optional
		// idempotency_key. NULL on legacy rows + runs with no key. The
		// partial unique index is created below in addIndexes (so the
		// column added here is guaranteed present first). The webhook
		// spawn path sets this = delivery_id for cross-replica /
		// past-Layer-1-TTL dedup.
		`ALTER TABLE runs ADD COLUMN idempotency_key TEXT`,
		// RFC L / Web-UI multi-tenant authz — tenant_id denormalised onto
		// the run row so the per-tenant workspace lists filter without a
		// sessions JOIN. NULL on legacy rows until the backfill below
		// copies it from the parent session. New rows set it at CreateRun.
		`ALTER TABLE runs ADD COLUMN tenant_id TEXT`,
		// RFC N — tenant-scope the agent definition plane. On an upgraded
		// v0.8.x DB the CREATE TABLE statements above were no-ops, so these
		// ALTERs add the tenant_id column to the existing tables; the old
		// PRIMARY KEY(name) / UNIQUE(name, version) is then replaced by
		// rebuildTenantBlindKeys (SQLite can't rewrite a key in place).
		// DEFAULT '' backfills existing rows to the shared tenant.
		`ALTER TABLE agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC CY item 8. Default 0 so a legacy row reads as NOT
		// operator-authored — the safe direction: nothing gains authority by
		// having predated the column.
		`ALTER TABLE agent_defs ADD COLUMN operator_authored INTEGER NOT NULL DEFAULT 0`,
		// Same for the TEAM plane: a team node's prompt is authored by the
		// TeamDef, so the widened families inside it are gated on THIS flag
		// and not on the spawned agent's.
		`ALTER TABLE teamdefs ADD COLUMN operator_authored INTEGER NOT NULL DEFAULT 0`,
		// The promoter's confinement on a team's active pointer — what an armed
		// subscription's walks run under. NULLABLE on purpose, no DEFAULT: a
		// pointer promoted before these columns existed must read as "not
		// captured", which the sweep treats as fully confined, never as a 0
		// that would read as unrestricted. See
		// internal/store/postgres/migrations/0087_teamdef_active_promoter.
		`ALTER TABLE teamdef_active ADD COLUMN promoter_operator_key_restricted INTEGER`,
		`ALTER TABLE teamdef_active ADD COLUMN promoter_isolated INTEGER`,
		`ALTER TABLE agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dynamic_agents ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N — tenant-scope the skill definition plane (mirror of the
		// agent ALTERs above), same SQLite upgrade path. DEFAULT ''
		// backfills existing rows to the shared tenant.
		`ALTER TABLE skill_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE skill_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N — tenant-scope the MCP server definition plane (mirror of the
		// agent + skill ALTERs above), same SQLite upgrade path. DEFAULT ''
		// backfills existing rows to the shared tenant.
		`ALTER TABLE mcp_server_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mcp_server_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC N (completion) — tenant-scope the remaining definition planes:
		// memory backend / schedule / A2A (server card + agent) / webhook
		// (mirror of the agent + skill + MCP ALTERs above), same SQLite
		// upgrade path. DEFAULT '' backfills existing rows to the shared
		// tenant.
		`ALTER TABLE memory_backend_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_backend_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE schedule_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE schedule_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_server_card_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_server_card_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// RFC S / F36: lifetime fire-count for max_fires self-retirement.
		// Idempotent ALTER for existing DBs; fresh DBs get it in CREATE TABLE.
		`ALTER TABLE schedule_run_state ADD COLUMN fire_count INTEGER NOT NULL DEFAULT 0`,
		// RFC DZ: the slot each claim takes, and who took it. Idempotent ALTERs
		// for existing DBs; fresh DBs get them in CREATE TABLE.
		`ALTER TABLE schedule_run_state ADD COLUMN slot_at INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN claimed_by TEXT`,
		`ALTER TABLE schedule_run_state ADD COLUMN claimed_at INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN finished_at INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN catch_up_until INTEGER`,
		`ALTER TABLE schedule_run_state ADD COLUMN missed_slots INTEGER NOT NULL DEFAULT 0`,
	}}
}

// columnsSection2 continues columnsSection1.
func columnsSection2() schemaPlan {
	return schemaPlan{columns: []string{
		// F42 / RFC X Phase 2: persist whether a run is an interactive
		// (persistent, parks-at-end_turn) session so a snapshotted+restored
		// paused run can be re-dispatched with the correct park-vs-complete
		// semantics. NULL/0 on legacy rows + batch runs; CreateRun stamps it.
		`ALTER TABLE runs ADD COLUMN interactive INTEGER NOT NULL DEFAULT 0`,
		// RFC AV — per-run cost + credential-source summary. cost REAL nullable
		// (NULL ⇒ unpriced, distinct from a genuine zero); credential_source is
		// "operator"|"tenant"|"user" for the run's primary key source. Idempotent
		// on fresh deploys (declared in CREATE TABLE runs above once upgraded).
		`ALTER TABLE runs ADD COLUMN cost REAL`,
		`ALTER TABLE runs ADD COLUMN cost_currency TEXT`,
		`ALTER TABLE runs ADD COLUMN credential_source TEXT`,
		`ALTER TABLE runs ADD COLUMN credential_scope_id TEXT`,
		// RFC AX — the negative operator-key permission bit (0 = allowed). Stamped
		// at CreateRun from the principal + gate; read back on resume/restore so a
		// re-dispatched run keeps its restriction. 0 on legacy rows (fail-open).
		`ALTER TABLE runs ADD COLUMN operator_key_restricted INTEGER NOT NULL DEFAULT 0`,
		// RFC BX P2b — the isolated-member confinement bit (0 = unconfined). Stamped
		// at CreateRun from the substrate:user principal; read back on resume/restore
		// so a re-dispatched run keeps its data-scope confinement. 0 on legacy rows
		// (fail-open). See internal/store/postgres/migrations/0069_runs_isolated.
		`ALTER TABLE runs ADD COLUMN isolated INTEGER NOT NULL DEFAULT 0`,
		// RFC DD: the run's configuration record — what this run was STARTED
		// with, so resume restores it instead of re-deriving from a definition
		// that may have changed since. Opaque JSON; NULL on legacy rows, which
		// resume treats exactly as it does today (re-derive).
		// See internal/store/postgres/migrations/0078_runs_run_config.
		`ALTER TABLE runs ADD COLUMN run_config TEXT`,
		// RFC DI: the run's answer, written in the same UPDATE as the terminal
		// status. See internal/store/postgres/migrations/0079_runs_result.
		`ALTER TABLE runs ADD COLUMN result TEXT`,
		// RFC DI D5: a configured run's draft — the raw request that will start
		// it, minus secrets. See internal/store/postgres/migrations/0080_runs_draft.
		`ALTER TABLE runs ADD COLUMN draft TEXT`,
		// The team walk a run is a member of — parent_context's walk_id as an
		// indexable column, backfilled below. See
		// internal/store/postgres/migrations/0086_runs_walk_id.
		`ALTER TABLE runs ADD COLUMN walk_id TEXT`,
		// A webhook delivery's second durable dedup key, beside
		// idempotency_key. NULL on legacy rows + runs with one key. Its
		// partial unique index is created in addIndexes. See
		// internal/store/postgres/migrations/0090_runs_delivery_alt_key.
		`ALTER TABLE runs ADD COLUMN delivery_alt_key TEXT`,
		// RFC BE — human/organizational chat metadata on the session row (the
		// History tool's browse/search/annotate surface). All additive + nullable
		// so legacy rows read the zero value. tags is a JSON array (NULL = never
		// set); archived_at / summary_updated_at are unix nanos (NULL = unset);
		// pinned is 0/1.
		`ALTER TABLE sessions ADD COLUMN title TEXT`,
		`ALTER TABLE sessions ADD COLUMN description TEXT`,
		`ALTER TABLE sessions ADD COLUMN tags TEXT`,
		`ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN archived_at INTEGER`,
		`ALTER TABLE sessions ADD COLUMN summary TEXT`,
		`ALTER TABLE sessions ADD COLUMN summary_updated_at INTEGER`,
		// A run's retention mode ('' = stored as usual, 'none' = content never
		// stored), and its session's. Mirrors postgres migration 0101.
		`ALTER TABLE runs ADD COLUMN retention TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN retention TEXT NOT NULL DEFAULT ''`,
		// RFC EI P3: a queued run's request, so a restart can start it. See
		// internal/store/postgres/migrations/0103_runs_queued_input.
		`ALTER TABLE runs ADD COLUMN queued_input TEXT`,
		// Whether a run may resolve $cred: in its user's own scope (0 = may).
		// Read back on resume, which has no trigger to re-derive it from. See
		// internal/store/postgres/migrations/0106_runs_credentials_no_user_scope.
		`ALTER TABLE runs ADD COLUMN credentials_no_user_scope INTEGER NOT NULL DEFAULT 0`,
		// Whether the run's admission took a slot in the shared per-user
		// counter; NULL on rows from before the column and on restored rows.
		// See internal/store/postgres/migrations/0108_runs_user_slot.
		`ALTER TABLE runs ADD COLUMN user_slot INTEGER`,
		// The OpenTelemetry trace the run's span was recorded in ('' when
		// tracing was off or unsampled). See
		// internal/store/postgres/migrations/0132_runs_trace_id.
		`ALTER TABLE runs ADD COLUMN trace_id TEXT NOT NULL DEFAULT ''`,
		// RFC BL — tenant-scope the Memory store + provenance/access columns.
		// On an UPGRADED DB these ALTERs add the columns to the existing
		// memory table; its old (scope, scope_id, key) PK is then replaced by
		// rebuildTenantBlindKeys (see the CREATE TABLE note). DEFAULT ''
		// backfills existing rows to the legacy tenant.
		`ALTER TABLE memory ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory ADD COLUMN origin TEXT`,
		`ALTER TABLE memory ADD COLUMN class TEXT`,
		`ALTER TABLE memory ADD COLUMN source_session_id TEXT`,
		`ALTER TABLE memory ADD COLUMN source_run_id TEXT`,
		`ALTER TABLE memory ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memory ADD COLUMN last_accessed_at INTEGER`,
		// RFC BL P2 — the soft-archive marker. NULL on every legacy row (live);
		// the consolidator stamps it to hide a consolidated raw row from recall
		// while retaining it.
		`ALTER TABLE memory ADD COLUMN superseded_at INTEGER`,
		`ALTER TABLE memory ADD COLUMN observed_at INTEGER`,
		`ALTER TABLE memory ADD COLUMN valid_at INTEGER`,
		`ALTER TABLE memory ADD COLUMN invalid_at INTEGER`,
		// RFC BL P3 — what enqueued a pending row (agent_explicit | compaction),
		// server-set. memory_pending arrived as a pure CREATE TABLE in P2, so an
		// upgraded DB has the table but not this column and needs the ALTER; a
		// fresh DB gets it from the schema block above. NULL on rows enqueued
		// before it existed — deliberately not backfilled to a guess.
		`ALTER TABLE memory_pending ADD COLUMN origin TEXT`,
		// RFC DT — per-call timing on the usage ledger. NULL on every row
		// recorded before it existed; nothing is backfilled to a guess.
		`ALTER TABLE token_usage ADD COLUMN duration_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN ttft_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN load_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN prefill_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN decode_ms INTEGER`,
		`ALTER TABLE token_usage ADD COLUMN queue_ms INTEGER`,
		// A token's fixed lifetime (epoch-nanos; NULL = never expires).
		`ALTER TABLE operator_token_defs ADD COLUMN expires_at INTEGER`,
		// A token's restrictions record as JSON (NULL = unrestricted).
		`ALTER TABLE operator_token_defs ADD COLUMN restrictions TEXT`,
		// Usage counted against a budget without a provider's report of it.
		// Mirrors postgres migration 0100.
		`ALTER TABLE usage_carry ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0`,
		// The run a held webhook delivery key started ('' for none yet).
		// Mirrors postgres migration 0104; its index is in addIndexes.
		`ALTER TABLE webhook_deliveries ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`,
		// A dedup_key claim's fencing token, its time (unix nanos, 0 for a row
		// written before) and the run keys that find its run. Mirrors postgres
		// migration 0107; its index is in addIndexes.
		`ALTER TABLE webhook_deliveries ADD COLUMN claim_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_deliveries ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE webhook_deliveries ADD COLUMN run_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE webhook_deliveries ADD COLUMN run_alt_key TEXT NOT NULL DEFAULT ''`,
		// RFC FA P1: when the person last authenticated (unix nanos, NULL =
		// unknown) and the gate that issued a token. Mirrors postgres 0112.
		`ALTER TABLE ui_sessions ADD COLUMN auth_time INTEGER`,
		`ALTER TABLE operator_token_defs ADD COLUMN issued_by_gate TEXT NOT NULL DEFAULT ''`,
		// What a usage row was for, and the audio an STT call transcribed. A
		// row from before reads back chat with no seconds. Mirrors postgres
		// 0113; the archive's key gains purpose in rebuildWidenedKeys.
		`ALTER TABLE token_usage ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE token_usage ADD COLUMN audio_seconds REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_archive ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE usage_archive ADD COLUMN audio_seconds REAL NOT NULL DEFAULT 0`,
		// The characters a TTS call sent to its engine. A row from before
		// reads back 0. Mirrors postgres 0116; no key changes.
		`ALTER TABLE token_usage ADD COLUMN input_chars INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_archive ADD COLUMN input_chars INTEGER NOT NULL DEFAULT 0`,
		// RFC EY §8: a token's authority mode ('' = frozen scopes, 'inherit' =
		// its user's current access_mode). Every existing row reads back
		// frozen. Mirrors postgres 0117.
		`ALTER TABLE operator_token_defs ADD COLUMN authority TEXT NOT NULL DEFAULT ''`,
		// Whose access-control set binds a team subscription's walks (the
		// promoter). NULLABLE, no DEFAULT: a pointer promoted before it reads
		// as "owner unknown", never as "recorded, nobody". Mirrors postgres
		// 0121; see sqlshared/teamdef_promoter.go for the three states.
		`ALTER TABLE teamdef_active ADD COLUMN promoter_acl_owner TEXT`,
		// The model tier and user tier a row was spent under. A row from
		// before reads back ''. Mirrors postgres 0125; the archive's key
		// gains both in rebuildWidenedKeys.
		`ALTER TABLE token_usage ADD COLUMN tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE token_usage ADD COLUMN user_tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_archive ADD COLUMN tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_archive ADD COLUMN user_tier TEXT NOT NULL DEFAULT ''`,
		// The app sign-in an access token came from (RFC FL): its client and
		// refresh-token family, '' for every other token. Mirrors postgres
		// 0123; the indexes are in addIndexes.
		`ALTER TABLE operator_token_defs ADD COLUMN oauth_client TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE operator_token_defs ADD COLUMN oauth_family TEXT NOT NULL DEFAULT ''`,
		// The removal's claimed sweeper stage, the user's status before it,
		// and the SCIM re-enable stamp (RFC FA P6). Mirrors postgres 0127.
		`ALTER TABLE auth_removals ADD COLUMN stage TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE auth_removals ADD COLUMN prior_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE scim_users ADD COLUMN reactivated_at INTEGER`,
		// The document a change row belongs to, stamped at write time (RFC FV);
		// '' for every row before. Mirrors postgres 0129.
		`ALTER TABLE memory_changes ADD COLUMN document_id TEXT NOT NULL DEFAULT ''`,
		// When a subject erasure ended the account and kept its row as a
		// tombstone (RFC FY Part F); NULL for every live user. Mirrors
		// postgres 0130.
		`ALTER TABLE users ADD COLUMN erased_at INTEGER`,
	}}
}

// indexesSection holds the index pass: indexes that may name a column the
// column-add pass adds, so they run after it. Like the column adds, it stays
// in its historical order.
func indexesSection() schemaPlan {
	return schemaPlan{indexes: []string{
		// Drives the hot lookup paths for the cancel/get endpoints.
		// Partial indexes (WHERE ... IS NOT NULL) keep the index small —
		// the vast majority of historical rows have no agent_id.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_id        ON runs(agent_id)        WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_parent_agent_id ON runs(parent_agent_id) WHERE parent_agent_id IS NOT NULL`,
		// A run's children by its run id: the cascade that ends the runs below
		// one no loop holds, which a team walk's members are only found by.
		`CREATE INDEX IF NOT EXISTS runs_by_parent_run_id   ON runs(parent_run_id)   WHERE parent_run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_user_active     ON runs(user_id, status) WHERE user_id IS NOT NULL`,
		// RFC L / Web-UI multi-tenant authz — tenant-scoped workspace lists.
		`CREATE INDEX IF NOT EXISTS runs_by_tenant_active   ON runs(tenant_id, status) WHERE tenant_id IS NOT NULL`,
		// RFC DI D5: the draft expiry sweep reads configured runs by age; the
		// partial index stays as small as the set of live drafts.
		`CREATE INDEX IF NOT EXISTS runs_configured_by_age  ON runs(started_at)       WHERE status = 'configured'`,
		// ListRunsByWalk: a walk's members in (started_at, id) order. Here, after
		// addColumns, so an upgraded DB has the column before its index.
		`CREATE INDEX IF NOT EXISTS runs_by_walk            ON runs(walk_id, started_at, id) WHERE walk_id IS NOT NULL`,
		// RFC DT: the boot seed reads the latest timed calls per (provider,
		// model). Here, after addColumns, so an upgraded DB has duration_ms
		// before an index names it; partial, so untimed history costs nothing.
		`CREATE INDEX IF NOT EXISTS token_usage_timing      ON token_usage(provider, model, ts) WHERE duration_ms IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS sessions_by_user        ON sessions(user_id)     WHERE user_id IS NOT NULL`,
		// v0.8.5: facets cost retros + experiment audits by which
		// agent_def_id the run actually ran against. Partial index
		// keeps it small — only DB-resolved runs have a non-NULL value.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_def       ON runs(agent_def_id)    WHERE agent_def_id IS NOT NULL`,
		// v0.9.x content_sha256 — partial-index lookup for the verify
		// op. Lives here in addIndexes (not the CREATE TABLE block)
		// so the column added by addColumns above is guaranteed to
		// exist when this index runs. Required for the v0.8.x →
		// v0.9.x upgrade path where the table exists without the column.
		`CREATE INDEX IF NOT EXISTS agent_defs_by_content_sha256      ON agent_defs(content_sha256)      WHERE content_sha256 IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS skill_defs_by_content_sha256      ON skill_defs(content_sha256)      WHERE content_sha256 IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS teamdefs_by_content_sha256        ON teamdefs(content_sha256)        WHERE content_sha256 IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS mcp_server_defs_by_content_sha256 ON mcp_server_defs(content_sha256) WHERE content_sha256 IS NOT NULL`,
		// v0.8.6 channel_messages_by_visible — runs in addIndexes
		// (NOT the earlier `stmts` loop) so the visible_at column
		// added by addColumns above is guaranteed to exist when this
		// index is created. Required for the upgrade path
		// v0.8.4/v0.8.5 → v0.8.6+ (channel_messages table exists
		// from v0.8.4 without visible_at).
		`CREATE INDEX IF NOT EXISTS channel_messages_by_visible ON channel_messages(tenant_id, channel, scope, scope_id, visible_at, id)`,
		// The hook worker's claim scan: only messages awaiting hooks, which
		// are few. The literal is store.ChannelHookHeldVisibleAt in unix
		// nanoseconds (a partial index predicate cannot take a parameter).
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS channel_messages_awaiting_hook ON channel_messages(id) WHERE visible_at = %d`,
			store.ChannelHookHeldVisibleAt().UnixNano()),
		// The channel_cursors upsert (ChannelAck) targets this uniq index
		// via ON CONFLICT(tenant_id, channel, scope, scope_id). It
		// duplicates the PK — on an upgraded DB too, once
		// rebuildTenantBlindKeys has replaced the old tenant-blind one — and
		// is kept so the ON CONFLICT target never depends on a DB's history.
		// Mirrors uniq_memory_tenant_scope_scope_id_key.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_channel_cursors_tenant ON channel_cursors(tenant_id, channel, scope, scope_id)`,
		// v0.8.x process_samples_by_sampled_at. Drives the read
		// path for /v1/_metrics/samples (window scan) and the
		// sweep DELETE WHERE sampled_at < cutoff.
		`CREATE INDEX IF NOT EXISTS process_samples_by_sampled_at ON process_samples(sampled_at)`,
		// v0.8.15 dynamic_agents_by_expires_at. Drives the sweeper
		// (DELETE WHERE expires_at < now() AND expires_at > 0) and the
		// per-Get expiry filter. Partial: only TTL-bearing rows.
		`CREATE INDEX IF NOT EXISTS dynamic_agents_by_expires_at ON dynamic_agents(expires_at) WHERE expires_at > 0`,
		// RFC BE — session-metadata listing. Partial indexes stay small: only
		// pinned rows / archived rows carry the flag. Live in addIndexes (not the
		// CREATE TABLE block) so the columns added above are guaranteed present.
		`CREATE INDEX IF NOT EXISTS sessions_by_pinned ON sessions(pinned) WHERE pinned = 1`,
		`CREATE INDEX IF NOT EXISTS sessions_by_archived_at ON sessions(archived_at) WHERE archived_at IS NOT NULL`,
		// v0.8.16 Interruption tool indexes.
		//   * by_run_status drives "is this run blocked?" + listing.
		//   * by_user_status drives the Web UI inbox view (the
		//     denormalised user_id column makes this a single-table
		//     scan, no JOIN against runs).
		//   * by_expires_pending drives the timeout sweeper.
		// Partial on by_user / by_expires so the index stays small
		// when the bulk of rows are resolved/terminal.
		`CREATE INDEX IF NOT EXISTS interrupts_by_run_status  ON interrupts(run_id, status)`,
		`CREATE INDEX IF NOT EXISTS interrupts_by_user_status ON interrupts(user_id, status) WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS interrupts_by_expires     ON interrupts(expires_at) WHERE expires_at IS NOT NULL AND status = 'pending'`,
		// RFC H Decision 10 "Layer 2" durable dedup — partial unique
		// index on idempotency_key. Lives in addIndexes (not the CREATE
		// TABLE block) so the column added by addColumns above is
		// guaranteed present on the v0.12.x → idempotency-key upgrade
		// path. Partial (WHERE ... IS NOT NULL) so keyless runs are
		// unconstrained and the index stays small. Mirrors the postgres
		// 0033 migration.
		`CREATE UNIQUE INDEX IF NOT EXISTS runs_idempotency_key ON runs(idempotency_key) WHERE idempotency_key IS NOT NULL`,
		// The second durable dedup key — unique for the same reason: two
		// replicas racing on one delivery both miss the lookup, and the
		// index lets only one CreateRun win.
		`CREATE UNIQUE INDEX IF NOT EXISTS runs_delivery_alt_key ON runs(delivery_alt_key) WHERE delivery_alt_key IS NOT NULL`,
		// A failed run's held delivery keys are dropped by run id.
		`CREATE INDEX IF NOT EXISTS webhook_deliveries_by_run_id ON webhook_deliveries(run_id) WHERE run_id <> ''`,
		// A claim's keys are linked, released and taken over by claim id.
		`CREATE INDEX IF NOT EXISTS webhook_deliveries_by_claim_id ON webhook_deliveries(claim_id) WHERE claim_id <> ''`,
		// RFC N — ON CONFLICT(tenant_id, name) targets for the def plane.
		// On a FRESH DB these are redundant with the composite PRIMARY KEY /
		// UNIQUE declared in the CREATE TABLE block above (harmless). On an
		// UPGRADED v0.8.x DB the addColumns ALTER added tenant_id but SQLite
		// could NOT rewrite the existing PRIMARY KEY(name) / UNIQUE(name,
		// version) in place — so the runtime upserts' ON CONFLICT(tenant_id,
		// name) and the version-bump UNIQUE(tenant_id, name, version) have NO
		// matching index and SQLite refuses with "ON CONFLICT clause does not
		// match any PRIMARY KEY or UNIQUE constraint", breaking the FIRST
		// promote/register even single-tenant. These idempotent indexes
		// supply that ON CONFLICT target so upserts work on upgraded DBs.
		// Lives in addIndexes (not the CREATE TABLE block) so the tenant_id
		// column added by addColumns above is guaranteed present first.
		//
		// These indexes alone did not give per-tenant isolation: the old
		// PRIMARY KEY(name) / UNIQUE(name, version) still fired first, so two
		// tenants could not share a name on a pre-existing SQLite DB.
		// rebuildTenantBlindKeys now replaces those keys before this block
		// runs, which leaves these redundant with the key on every DB; they
		// are kept as the stable ON CONFLICT target. The skill + mcp planes have NO
		// dynamic_* tier (skills = static skills.Set + the
		// skill_defs/skill_def_active substrate; mcp = static cfg.MCPServers +
		// the mcp_server_defs/mcp_server_def_active substrate), so each needs
		// only two indexes.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_agent_def_active_tenant_name    ON agent_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_dynamic_agents_tenant_name      ON dynamic_agents(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_agent_defs_tenant_name_version  ON agent_defs(tenant_id, name, version)`,
		// Skill plane (RFC N FIX 5-skills) — same in-place-upgrade gap.
		// SkillDefSetActive does ON CONFLICT(tenant_id, name) on
		// skill_def_active and SkillDefCreate inserts against
		// UNIQUE(tenant_id, name, version) on skill_defs; on an upgraded DB
		// neither composite index exists, so the FIRST promote/register
		// fails even single-tenant. No dynamic_skills table to cover.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_skill_def_active_tenant_name    ON skill_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_skill_defs_tenant_name_version  ON skill_defs(tenant_id, name, version)`,
		// Team plane — same ON CONFLICT(tenant_id, name) / UNIQUE(tenant_id,
		// name, version) targets as the skill plane. teamdefs is a fresh
		// table so these are redundant with its CREATE TABLE composite
		// constraints, but kept for parity with the skill plane's pattern.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_teamdef_active_tenant_name     ON teamdef_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_teamdefs_tenant_name_version   ON teamdefs(tenant_id, name, version)`,
		// MCP plane (RFC N FIX 5-mcp) — same in-place-upgrade gap.
		// MCPServerDefSetActive does ON CONFLICT(tenant_id, name) on
		// mcp_server_def_active and MCPServerDefCreate's version-bump inserts
		// against UNIQUE(tenant_id, name, version) on mcp_server_defs; on an
		// upgraded DB neither composite index exists, so the FIRST
		// promote/register fails even single-tenant. No dynamic_mcp table.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_mcp_server_def_active_tenant_name   ON mcp_server_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_mcp_server_defs_tenant_name_version ON mcp_server_defs(tenant_id, name, version)`,
		// MemoryBackend plane (RFC N completion) — same in-place-upgrade gap.
		// MemoryBackendDefSetActive does ON CONFLICT(tenant_id, name) on
		// memory_backend_def_active and MemoryBackendDefCreate's version-bump
		// inserts against UNIQUE(tenant_id, name, version) on
		// memory_backend_defs; on an upgraded DB neither composite index
		// exists, so the FIRST promote/register fails even single-tenant. No
		// dynamic_* table to cover.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_backend_def_active_tenant_name   ON memory_backend_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_backend_defs_tenant_name_version ON memory_backend_defs(tenant_id, name, version)`,
		// DocumentSource plane — same in-place-upgrade gap as the
		// MemoryBackend plane above (document_source_def_active ON
		// CONFLICT(tenant_id, name) + document_source_defs UNIQUE(tenant_id,
		// name, version)). No dynamic_* table to cover.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_document_source_def_active_tenant_name   ON document_source_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_document_source_defs_tenant_name_version ON document_source_defs(tenant_id, name, version)`,
		// A2A agent plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_agent_def_active_tenant_name   ON a2a_agent_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_agent_defs_tenant_name_version ON a2a_agent_defs(tenant_id, name, version)`,
		// Schedule plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_schedule_def_active_tenant_name   ON schedule_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_schedule_defs_tenant_name_version ON schedule_defs(tenant_id, name, version)`,
		// A2A server card plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_server_card_def_active_tenant_name   ON a2a_server_card_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_a2a_server_card_defs_tenant_name_version ON a2a_server_card_defs(tenant_id, name, version)`,
		// Webhook plane (RFC N completion) — same in-place-upgrade gap.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_webhook_def_active_tenant_name   ON webhook_def_active(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_webhook_defs_tenant_name_version ON webhook_defs(tenant_id, name, version)`,
		// RFC BL — the ON CONFLICT(tenant_id, scope, scope_id, key) target for
		// the Memory upserts (MemorySet / MemoryIncrement / MemoryAtomicUpdate).
		// Redundant with the composite PRIMARY KEY (on an upgraded DB too,
		// once rebuildTenantBlindKeys has replaced the old (scope, scope_id,
		// key) PK), and kept as the stable ON CONFLICT target — without a
		// matching key the upserts fail "ON CONFLICT clause does not match
		// ...". Mirrors the def-plane indexes above.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_tenant_scope_scope_id_key ON memory(tenant_id, scope, scope_id, key)`,
		// MOVED HERE FROM THE CREATE-TABLE BLOCK, and the move is the fix for an
		// upgrade that could not start at all.
		//
		// Each of these names a column that an upgraded DB only gets from the ALTER
		// block above: a `CREATE TABLE IF NOT EXISTS` is a no-op against an existing
		// table, so `memory` keeps whatever shape it was created with and the column
		// arrives later or not at all. Creating the index first therefore failed with
		// "no such column: observed_at" — fatally, since index errors are not
		// swallowed — and the ALTER that would have added it was never reached. Any
		// sqlite deployment whose `memory` table predated RFC CL could not open.
		//
		// A fresh DB is unaffected either way: it gets the columns from CREATE TABLE
		// and the indexes here, which is why every test passed while real upgrades
		// broke.
		//
		// Partial for the reason the originals gave: the predicate column is NULL on
		// every row nobody dated, and on a corpus that was never dated that is all of
		// them. Mirrors postgres migrations 0065 / 0073 / 0074, which do not have this
		// problem because each is its own ordered file.
		`CREATE INDEX IF NOT EXISTS memory_by_source_session ON memory(tenant_id, source_session_id) WHERE source_session_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS memory_by_observed_at ON memory(tenant_id, scope, scope_id, observed_at) WHERE observed_at IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS memory_by_valid_at ON memory(tenant_id, scope, scope_id, valid_at) WHERE valid_at IS NOT NULL`,
		// RFC FL: an app sign-in's access tokens, by family and by client.
		`CREATE INDEX IF NOT EXISTS operator_token_defs_by_oauth_family ON operator_token_defs(oauth_family) WHERE oauth_family <> ''`,
		`CREATE INDEX IF NOT EXISTS operator_token_defs_by_oauth_client ON operator_token_defs(oauth_client) WHERE oauth_client <> ''`,
	}}
}
