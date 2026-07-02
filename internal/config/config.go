// Package config loads loomcycle.yaml + env vars and validates them.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// Config is the top-level YAML structure plus env-derived fields.
type Config struct {
	Defaults Defaults            `yaml:"defaults"`
	Models   map[string]ModelRef `yaml:"models"`
	Agents   map[string]AgentDef `yaml:"agents"`
	// Skills defines skills INLINE, at the same level as `agents:` — a
	// name→SkillSpec map whose bodies an agent's `skills: [name]` field
	// references, with no LOOMCYCLE_SKILLS_ROOT directory of SKILL.md files
	// required. Inline skills merge by key across config layers (RFC AN)
	// like `agents`/`models`, and OVERLAY the root directory on a name
	// collision (resolveSkills); the root stays supported as a fallback.
	Skills      map[string]SkillSpec `yaml:"skills,omitempty"`
	MCPServers  map[string]MCPServer `yaml:"mcp_servers"`
	Concurrency Concurrency          `yaml:"concurrency"`
	Cache       CacheConfig          `yaml:"cache"`

	// ContextPlugins is the runtime-wide chain of context-transform plugins
	// (RFC Z / F43) — fast, built-in transforms applied to a COPY of the
	// outbound LLM request on every turn (e.g. secret redaction), in declared
	// order. Empty = no chain. Runtime-wide only in this version; per-agent +
	// tenant scopes (with floor composition) are a follow-up. Built once at
	// server start and shared read-only across runs; the synthetic code-js
	// provider is exempt (the loop skips the chain for it).
	ContextPlugins []ContextPluginSpec `yaml:"context_plugins,omitempty"`

	// LocalAPI declares the OpenAPI-derived MCP gateway (v0.4.0+).
	// One tool is generated per operation; tools forward calls to
	// BaseURL with the agent's `bearer` field as Authorization.
	// Empty SpecPath disables the gateway. See
	// internal/tools/localapi for the wire model.
	LocalAPI LocalAPIConfig `yaml:"local_api"`
	// Storage selects the persistence backend. SQLite (default)
	// covers compact/dev installs; Postgres unblocks horizontal
	// scaling for production deployments. See StorageConfig.
	Storage StorageConfig `yaml:"storage"`

	// ProviderPriority is the library-wide order the resolver walks
	// when an agent declares a tier (low/middle/high) without a
	// per-agent `providers:` override. Cost-floor-first by default
	// (deepseek > ollama > openai > anthropic) — try the cheapest
	// reasonable backend first, escalate when the lower options
	// stall. Empty = use the hardcoded default in
	// internal/resolve/. Per-agent `providers:` fully replaces this.
	ProviderPriority []string `yaml:"provider_priority"`

	// Tiers is the library-wide tier → ordered candidate list.
	// Operator-editable so model wire aliases stay out of the
	// binary as the catalog churns. The resolver consults this when
	// an agent declares a tier without a per-agent `models:`
	// override. See doc-internal/rfcs/model-resolution-matrix.md
	// for the May-2026 default matrix; loomcycle.example.yaml has
	// the full operator-facing example.
	Tiers map[string][]TierCandidate `yaml:"tiers"`

	// UserTiers is the v0.8.2 user-facing-tier policy map. Each entry
	// names a tier (e.g. "free" / "low" / "medium" / "high" /
	// "default") and carries the per-tier provider-and-model policy
	// the resolver overlays for runs that carry that user_tier in the
	// POST /v1/runs request body.
	//
	// When this map is set, a "default" entry is REQUIRED — it covers
	// runs that don't specify a user_tier (back-compat with v0.7.x
	// clients) and runs from clients that haven't been bumped yet.
	// When this map is empty / nil, the v0.8.2 user_tier feature is
	// disabled and resolution falls back to the v0.7-era ProviderPriority
	// + Tiers + per-agent override path unchanged.
	//
	// Overlay precedence (low → high):
	//   library ProviderPriority + Tiers   (fallback when no overlay)
	//   user_tier (this map's named entry) (when user_tier in request)
	//   agent.Providers / agent.Models     (per-agent yaml overrides)
	//
	// agent.Providers ∩ user_tier.ProviderPriority is enforced — when
	// empty, the resolver refuses with ErrTierAgentNotAvailable so the
	// client can surface "this agent isn't available for your tier".
	//
	// See docs/PLAN.md → v0.8.2 for the full design rationale.
	UserTiers map[string]UserTier `yaml:"user_tiers"`

	// Channels is the v0.8.4 Channel-tool registry. Operators
	// declare channels explicitly here — no auto-creation — so the
	// namespace is operator-owned and ACL rules in
	// AgentDef.Channels can validate against a closed set.
	//
	// Empty / nil = Channel tool is effectively disabled (every
	// publish/subscribe op refuses with "channel not declared").
	// Re-uses the existing Memory scope vocabulary (agent / user)
	// plus a new "global" scope for cross-tenant fan-out streams.
	Channels map[string]Channel `yaml:"channels"`

	// ScheduledRuns is the v1.x RFC E scheduled-runs registry. Each
	// entry declares either a TEMPLATE (no user_id; orchestrators
	// fork per-user via the ScheduleDef tool) or a STANDALONE
	// schedule (operator-owned periodic cron with explicit user_id +
	// user_credentials_from_env).
	//
	// Empty / nil = the scheduled-runs subsystem is effectively
	// disabled (no yaml templates; substrate forks still work via
	// ScheduleDef tool ops). When set, yaml entries get bootstrapped
	// into v1 substrate rows on first fork — same posture as
	// cfg.Agents + agent_defs.
	//
	// See `Context.help scheduled-runs` for the operator reference
	// and rfcs/scheduled-agent-runs.md for the locked design.
	ScheduledRuns map[string]ScheduledRun `yaml:"scheduled_runs"`

	// A2AServerCards is the v1.x RFC G registry of A2A server-card
	// declarations — which loomcycle agents are exposed over the A2A
	// protocol + the AgentCard metadata served at the well-known URI.
	// Yaml entries are the operator-blessed root; the A2AServerCardDef
	// tool produces the DERIVED layer of orchestrator-authored forks.
	// Empty / nil = no statically-declared cards (substrate forks still
	// work via the tool).
	A2AServerCards map[string]A2AServerCard `yaml:"a2a_server_cards"`

	// A2AAgents is the v1.x RFC G registry of REMOTE A2A peer
	// declarations — how to reach another A2A-speaking agent (its
	// well-known card URL or a direct endpoint+binding) plus the auth
	// + expected-skills manifest. Yaml entries are the operator-blessed
	// root; the A2AAgentDef tool produces the derived fork layer.
	// Empty / nil = no statically-declared peers.
	A2AAgents map[string]A2AAgent `yaml:"a2a_agents"`

	// Webhooks is the v1.x RFC H registry of inbound HTTP webhook
	// declarations — how an external system reaches loomcycle to
	// trigger an agent run (or publish to a channel), plus the auth,
	// rate limit, payload mapping, and on_complete hooks. Yaml entries
	// are the operator-blessed root; the WebhookDef tool produces the
	// derived fork layer. Empty / nil = no statically-declared webhooks.
	Webhooks map[string]Webhook `yaml:"webhooks"`

	// MemoryBackends is the RFC I MR-3a registry of named memory
	// backend declarations — which backend (inprocess or mem9), its
	// connection config, tenancy strategy, and fallback. Yaml entries
	// are the operator-blessed root; the MemoryBackendDef tool produces
	// the derived fork layer. Empty / nil = no statically-declared
	// backends. Nothing consumes these yet — the per-agent routing +
	// factory land in MR-3b.
	MemoryBackends map[string]MemoryBackend `yaml:"memory_backends"`

	// Volumes is the RFC AH registry of named filesystem volumes — the
	// universe of ro/rw roots an AgentDef may bind to (the filesystem
	// analog of "registered tools" for allowed_tools). Each entry's `path`
	// MUST already exist and be a directory (validated at config-load; the
	// runtime never mkdir's a static volume); at most one entry may be
	// `default: true`.
	//
	// RFC AH Phase 3: Volumes are the SOLE filesystem mechanism — the legacy
	// LOOMCYCLE_READ_ROOT / WRITE_ROOT / BASH_CWD jail is gone. No binding =
	// no disk access (sandbox-by-default): an agent bound to no volume (and a
	// deployment with no `default` volume) has every file/exec tool refuse.
	// To restore the old single shared jail, declare one `default` volume:
	//   volumes:
	//     default: { path: /work/sandbox, mode: rw, default: true }
	// Unbound agents bind to `default` when it exists. The dynamic VolumeDef
	// substrate (Phase 2) is a separate, later feature.
	Volumes map[string]Volume `yaml:"volumes"`

	// Interruption is the v0.8.16 top-level config block for the
	// Interruption tool. Operator picks the delivery backend
	// (webui / mcp_server:<name> / cli) and the env-cap defaults.
	// Empty (zero-value) = backend=webui implicitly.
	Interruption InterruptionConfig `yaml:"interruption"`

	// Hooks is the v0.8.17 top-level config block for the tool-use
	// hooks subsystem. Today it only carries the host-widen
	// permission allowlist; the existing hook-registration HTTP
	// endpoints (POST /v1/hooks) are unchanged. See HooksConfig.
	Hooks HooksConfig `yaml:"hooks"`

	// Memory is the v0.9.0 top-level config block for the Memory
	// tool's vector / semantic features. Only sub-field today is
	// `embedder:` (provider + model + timeouts). When unset,
	// vector ops on the Memory tool refuse with
	// embedder_not_configured. K/V Memory is unaffected.
	Memory MemoryConfig `yaml:"memory"`

	// Principals declares static (tenant, subject, scopes) logins, each bound
	// to a bearer secret held in an env var (RFC AO). The map key is an
	// informational handle. Lets an operator declare a stable service identity
	// in config and hand the same token to the Web UI and an MCP thin client so
	// both authenticate as the same (tenant, subject). The secret never appears
	// in yaml — only `token_env` (the env-var name) does.
	Principals map[string]PrincipalDef `yaml:"principals,omitempty"`

	// ResolvedPrincipals is the boot-resolved token→Principal table built from
	// Principals during validate (each token_env read from the environment).
	// The auth-layer bearer resolver matches a presented token against it.
	// Not in YAML; carries the resolved secrets, so it is never serialized.
	ResolvedPrincipals []auth.DeclaredPrincipal `yaml:"-"`

	// Env-derived; not in YAML.
	Env Env `yaml:"-"`

	// Warnings holds non-fatal config advisories accumulated during
	// validate() — surfaced once at boot by main.go (log "config: WARNING:
	// …"), never returned over the wire. Today: the "tool is in allowed_tools
	// but its capability gate is unset, so every call default-denies" footgun
	// (e.g. Memory without memory_scopes — F21). Not in YAML.
	Warnings []string `yaml:"-"`

	// configDir is the directory of the loaded YAML, kept so relative
	// paths inside the config (system_prompt_file, local_api.spec) can
	// be resolved against it.
	configDir string `yaml:"-"`
}

// LocalAPIConfig is the operator-supplied config for the local-api
// gateway. Mirrors localapi.Config but lives in the config package so
// tests don't need to import the localapi package.
type LocalAPIConfig struct {
	SpecPath       string `yaml:"spec"`
	BaseURL        string `yaml:"base_url"`
	ToolNamePrefix string `yaml:"tool_name_prefix"`
}

// InterruptionConfig is the v0.8.16 top-level config block for the
// Interruption tool. Operator selects which delivery surface the
// "ask" op uses for human input, plus the timeout / heartbeat env
// caps. Most fields have env-var equivalents (see Env.Interruption*)
// for ops who prefer env over yaml; env wins where both are set.
type InterruptionConfig struct {
	// Backend selects the delivery surface. Valid values:
	//   - "webui"             (default) — humans answer via /ui/interrupts
	//   - "mcp_server:<name>" — loomcycle calls <name>'s MCP server
	//                            tool (e.g. mcp__myconsumer__ask)
	//   - "cli"               — local-dev only; a separate process
	//                            (loomcycle-interrupt-cli) subscribes
	//                            to _system/interrupts/pending and
	//                            resolves via the HTTP endpoint
	// Empty = webui.
	Backend string `yaml:"backend"`

	// DefaultTimeoutMS is the timeout applied when an ask doesn't
	// pass timeout_ms. 0 = no timeout (interruption sits pending
	// indefinitely; operators relying on long-running questions
	// SHOULD set a sweeper-friendly value).
	DefaultTimeoutMS int `yaml:"default_timeout_ms"`

	// MaxTimeoutMS is the hard ceiling. timeout_ms above this is
	// clamped down. 0 = no ceiling. Useful for capping
	// model-passed timeouts so the model can't pin a run open for
	// arbitrary time.
	MaxTimeoutMS int `yaml:"max_timeout_ms"`

	// MaxPendingPerRun caps simultaneous pending interrupts on one
	// run. 0 = no cap (operator trusts agent yaml's max_pending
	// alone). Per-agent yaml may narrow further.
	MaxPendingPerRun int `yaml:"max_pending_per_run"`

	// HeartbeatIntervalMS governs the during-block heartbeat
	// ticker. 0 = use the 30-second default. Tighter intervals
	// shorten the post-crash detection window (the sweeper sees
	// missed heartbeats sooner) at the cost of more DB write
	// traffic.
	HeartbeatIntervalMS int `yaml:"heartbeat_interval_ms"`
}

// StorageConfig selects the Store backend and its connection settings.
// Empty Backend defaults to "sqlite" for back-compat with v0.4 configs
// that pre-date this block. SQLite uses Env.DataDir for its on-disk
// path; Postgres uses the PgDSN below (or LOOMCYCLE_PG_DSN env).
//
// Env precedence: every field below has a corresponding LOOMCYCLE_*
// env var. Env wins over YAML when both are set, so production
// deploys can keep secrets (PG_DSN) out of the version-controlled YAML.
type StorageConfig struct {
	// Backend selects the adapter: "sqlite" (default) or "postgres".
	// Env: LOOMCYCLE_STORAGE_BACKEND.
	Backend string `yaml:"backend"`
	// PgDSN is the Postgres connection string (libpq URL form).
	// Required when Backend="postgres". Env: LOOMCYCLE_PG_DSN.
	// Example: postgres://user:pass@host:5432/loomcycle?sslmode=require
	PgDSN string `yaml:"pg_dsn"`
	// PgMaxOpenConns caps the pgxpool size. Default 32. Env:
	// LOOMCYCLE_PG_MAX_OPEN_CONNS.
	PgMaxOpenConns int32 `yaml:"pg_max_open_conns"`
	// PgMinIdleConns is the floor of warm idle connections. Default 4.
	// Env: LOOMCYCLE_PG_MIN_IDLE_CONNS.
	PgMinIdleConns int32 `yaml:"pg_min_idle_conns"`
	// PgAutoMigrate controls schema bootstrap on startup. When false
	// (default), Open() refuses to start unless the embedded migration
	// set is at or behind the database — the operator must run
	// `loomcycle migrate up` explicitly. When true, Open() runs
	// migrations transparently. Env: LOOMCYCLE_PG_AUTOMIGRATE=1.
	PgAutoMigrate bool `yaml:"pg_automigrate"`

	// --- RFC AA SQL Memory (Phase 1, sqlite-only) ---
	//
	// SQL Memory is OFF by default: even an agent with sql_scopes set sees
	// its SQL ops refuse until the operator enables the subsystem here. The
	// per-scope databases are a SEPARATE store from the main loomcycle DB.

	// SqlMemEnabled turns the SQL Memory subsystem on. Env:
	// LOOMCYCLE_SQLMEM_ENABLED=1.
	SqlMemEnabled bool `yaml:"sqlmem_enabled"`
	// SqlMemRoot is the parent dir for per-scope .db files. Empty defaults
	// to <DataDir>/sqlmem. Env: LOOMCYCLE_SQLMEM_ROOT.
	SqlMemRoot string `yaml:"sqlmem_root"`
	// SqlMemQuotaBytes caps a single scope file's on-disk size (checked
	// before each write). 0 = no quota. Per-agent sql_quota_bytes overrides
	// this. Env: LOOMCYCLE_SQLMEM_QUOTA_BYTES.
	SqlMemQuotaBytes int `yaml:"sqlmem_quota_bytes"`
	// SqlMemStatementTimeoutMS bounds a single sql_query/sql_exec. Default
	// 30000. Env: LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS.
	SqlMemStatementTimeoutMS int `yaml:"sqlmem_statement_timeout_ms"`
	// SqlMemMaxRows caps the rows a sql_query returns (the rest is dropped
	// and the result is flagged truncated). Default 10000. Env:
	// LOOMCYCLE_SQLMEM_MAX_ROWS.
	SqlMemMaxRows int `yaml:"sqlmem_max_rows"`
	// SqlMemAuditMode controls how much of an audited statement is recorded:
	// "full" (the default) records the redacted statement text; "metadata"
	// records only op/scope/row counts, never the statement. Env:
	// LOOMCYCLE_SQLMEM_AUDIT_MODE.
	SqlMemAuditMode string `yaml:"sqlmem_audit_mode"`
	// SqlMemPgDSN is the SEPARATE aux-database DSN for the postgres SQL Memory
	// tier (Phase 2), distinct from the main-store PgDSN. Required when the
	// main Backend is "postgres" and SQL Memory is enabled; ignored on the
	// sqlite backend (file-per-scope). Env: LOOMCYCLE_SQLMEM_PG_DSN.
	SqlMemPgDSN string `yaml:"sqlmem_pg_dsn"`
	// SqlMemTxnTimeoutMS bounds how long an explicit transaction (Phase 3a
	// sql_begin) may stay open before the reaper rolls it back — a held scope
	// connection must never leak past a stuck agent. Default 30000. 0 disables
	// the reaper. Env: LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS.
	SqlMemTxnTimeoutMS int `yaml:"sqlmem_txn_timeout_ms"`
	// SqlMemMaxOpenTxns caps concurrent open explicit transactions process-wide
	// (each pins a scope connection). Default 64. 0 = unbounded. Env:
	// LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS.
	SqlMemMaxOpenTxns int `yaml:"sqlmem_max_open_txns"`
	// SqlMemMaxTxnDepth caps the SAVEPOINT nesting depth of one explicit
	// transaction (Phase 3b): a nested sql_begin past this errors, bounding the
	// in-memory savepoint stack a looping agent can grow. Default 16. 0 =
	// unbounded. Env: LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH.
	SqlMemMaxTxnDepth int `yaml:"sqlmem_max_txn_depth"`
	// SqlMemScopeTTLMS turns on durable-scope GC (Phase 3d): a durable
	// (agent/user) scope idle longer than this is dropped by the sweeper. 0 =
	// OFF (the default — GC DISCARDS DATA, so it is opt-in). Run scopes are never
	// GC'd. Env: LOOMCYCLE_SQLMEM_SCOPE_TTL_MS.
	SqlMemScopeTTLMS int `yaml:"sqlmem_scope_ttl_ms"`
	// SqlMemGCIntervalMS is how often the durable-scope GC sweeper runs. 0 → a
	// sensible default (one hour); meaningful when SqlMemScopeTTLMS > 0 OR
	// SqlMemTotalMaxBytes > 0. Env: LOOMCYCLE_SQLMEM_GC_INTERVAL_MS.
	SqlMemGCIntervalMS int `yaml:"sqlmem_gc_interval_ms"`
	// SqlMemTotalMaxBytes turns on size-based GC (Phase 3f.3): when the AGGREGATE
	// on-disk size of all durable (agent/user) scopes exceeds this, the sweeper
	// evicts the largest idle scopes until back under budget (per-scope size is
	// already capped per-write by the quota; this bounds the total). 0 = OFF (the
	// default — GC DISCARDS DATA, so it is opt-in). Complements the TTL sweep.
	// Env: LOOMCYCLE_SQLMEM_TOTAL_MAX_BYTES.
	SqlMemTotalMaxBytes int64 `yaml:"sqlmem_total_max_bytes"`
	// SqlMemSnapshotMaxScopeBytes caps a single SQL Memory scope's serialized
	// dump in a runtime snapshot (Phase 3f.2). A scope over the cap is EXCLUDED
	// from the snapshot and recorded in the section (so one runaway scope can't
	// sink the whole capture or blow the 512 MB envelope cap); it is not
	// restored. 0 = no per-scope cap. Env: LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES.
	SqlMemSnapshotMaxScopeBytes int64 `yaml:"sqlmem_snapshot_max_scope_bytes"`
	// SqlMemSharedSchemas lists operator-curated READ-ONLY shared schemas (Phase
	// 3g, postgres tier only). Each is baked onto every scope role's search_path so
	// agents can SELECT/JOIN the operator's reference tables; read-only is
	// engine-enforced (the operator grants SELECT only — see docs/SQL_MEMORY.md).
	// A shared schema is GLOBAL (visible to every tenant's scopes) — put only
	// non-sensitive, non-tenant-specific data there. Invalid/missing names are
	// skipped with a boot warning. Ignored on the sqlite tier. Env (comma-
	// separated): LOOMCYCLE_SQLMEM_SHARED_SCHEMAS.
	SqlMemSharedSchemas []string `yaml:"sqlmem_shared_schemas"`
}

// ConfigDir returns the directory the YAML was loaded from. Used by
// callers that need to resolve relative paths declared in the config
// (the local-api spec path, additional resource files).
func (c *Config) ConfigDir() string { return c.configDir }

// HooksConfig is the v0.8.17 top-level config block for the tool-use
// hooks subsystem. Carries operator-side knobs that can't be set via
// the dynamic POST /v1/hooks endpoint — they need a trust boundary
// the registering app can't influence.
type HooksConfig struct {
	// PermitHostWiden lists the (tenant, hook-owner) pairs whose Pre-hook
	// responses are honoured when they include an `allow_hosts` field. A hook
	// registers via POST /v1/hooks with an Owner UID and is stamped with its
	// AUTHORITATIVE tenant; if that (tenant, owner) appears here (exact match on
	// both, no globs), the dispatcher UNIONs the hook's per-call allow_hosts
	// entries into a ctx-scoped extra list that HTTP/WebFetch consult at the
	// host-allowed gate.
	//
	// Each entry is a `[tenant:]owner` string: a bare `owner` (no colon) binds
	// to the shared tenant "" (single-tenant deployments + operator/global
	// hooks); `tenant:owner` confines the grant to that tenant. Keying on
	// (tenant, owner) — not owner alone — stops a second tenant from claiming a
	// permitted owner string and escaping the operator host floor for its own
	// runs (RFC AF follow-up; owner is caller-supplied, only the tenant is
	// authoritative).
	//
	// Without a matching entry, allow_hosts from a hook is silently
	// dropped (with a WARN log + metric increment) — preserving the
	// "operator yaml is the trust-boundary floor" invariant
	// (CLAUDE.md rule #8): the runtime caller and the model both
	// cannot enable widening.
	//
	// Env-var equivalent: LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS
	// (comma-separated, same `[tenant:]owner` syntax). Env appends to yaml.
	PermitHostWiden HostWidenPermitConfig `yaml:"permit_host_widen"`
}

// MemoryConfig is the v0.9.0 top-level Memory tool config block.
// Only sub-field today is `embedder:`. K/v Memory ops have no
// per-block config (the byte caps live on Env). When the entire
// block is unset, vector ops refuse with embedder_not_configured.
type MemoryConfig struct {
	// Embedder picks the provider + model loomcycle uses to embed
	// memory rows (when an agent calls Memory.set with embed=true)
	// and queries (Memory.search). Exactly one embedder is
	// supported in v0.9.0 — per-agent overrides ship in v0.10.x.
	//
	// When Provider is empty (default), vector ops on the Memory
	// tool refuse with embedder_not_configured. K/V Memory is
	// unaffected.
	Embedder EmbedderConfig `yaml:"embedder"`

	// Entries lets the operator pre-seed memory rows at boot from
	// yaml. Added in v0.11.5 for n8n test fixtures + static-
	// deployment use cases — operators declare lookup tables,
	// company policies, default values, etc. in yaml instead of
	// scripting Memory.set calls on every fresh boot.
	//
	// Boot loader semantics (cmd/loomcycle/main.go
	// bootstrapMemoryEntries):
	//   - Idempotent. For each entry, check whether (scope, scope_id,
	//     key) already exists in the store; skip if present. Preserves
	//     any runtime updates the operator made between boots.
	//   - Synchronous embed when entry.Embed=true AND an embedder is
	//     configured. Boot log warns about per-entry embed latency so
	//     operators with many embedded entries aren't surprised by
	//     slow starts.
	//   - Failures are logged but don't fail boot — the operator gets
	//     a degraded substrate they can repair without restart.
	Entries []MemoryEntryDecl `yaml:"entries"`
}

// MemoryEntryDecl is one yaml-declared memory entry, loaded on boot
// by cmd/loomcycle/main.go's bootstrapMemoryEntries helper.
type MemoryEntryDecl struct {
	// Scope is "agent" / "user" / "global". Validated at boot —
	// unknown scopes log a warning + skip the entry.
	Scope string `yaml:"scope"`

	// ScopeID is the per-scope identifier:
	//   - scope=agent  → agent name (e.g. "researcher")
	//   - scope=user   → user id (e.g. "alice")
	//   - scope=global → empty string (loomcycle convention)
	ScopeID string `yaml:"scope_id"`

	// Key is the memory key under (scope, scope_id).
	Key string `yaml:"key"`

	// Value is the entry's body. yaml-supported types (string,
	// number, bool, list, map) round-trip through the substrate as
	// JSON.
	Value interface{} `yaml:"value"`

	// Embed, when true, triggers a synchronous embed via the
	// configured embedder during boot. Slow operations log a warning;
	// boots with many embedded entries take proportionally longer.
	// Default false — k/v-only entries are the common case.
	Embed bool `yaml:"embed"`
}

// EmbedderConfig selects the v0.9.0 embedder. Validated at config
// load: Provider must be in the registered set (providers.NewEmbedder
// catches unknown names); Model is required when Provider is set.
type EmbedderConfig struct {
	// Provider is the registered embedder driver name
	// ("openai" / "gemini" / "anthropic" in v0.9.0).
	Provider string `yaml:"provider"`
	// Model is the wire model id ("text-embedding-3-large" etc.).
	Model string `yaml:"model"`
	// TimeoutMs overrides LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS for
	// this specific embedder. 0 = inherit env (30000 default).
	TimeoutMs int `yaml:"timeout_ms"`
	// BatchSize overrides LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE.
	// 0 = inherit env (100 default).
	BatchSize int `yaml:"batch_size"`
}

// HostWidenPermitConfig is the per-capability slice of HooksConfig
// for the per-call host-widening capability. Kept as its own struct
// so future widening axes (memory scopes, channel ACLs) can hang off
// HooksConfig without flattening the namespace.
type HostWidenPermitConfig struct {
	// Owners is the exact-match list of `[tenant:]owner` entries whose
	// allow_hosts grants are honoured (bare `owner` = the shared tenant "";
	// `tenant:owner` = that tenant only). Empty / nil = no widening permitted
	// (default — the safe stance).
	Owners []string `yaml:"owners"`
}

// Defaults are the fall-throughs for agents that don't specify them.
type Defaults struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// ModelRef points one alias at a (provider, model) pair.
type ModelRef struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// TierCandidate is one entry in a tier's ordered candidate list.
// The resolver walks tier candidates in declaration order, picking
// the first (provider, model) where the provider is reachable, the
// model is listed by the provider, and neither is currently marked
// stalled in the availability matrix.
//
// json: tags MUST be present + lowercase for the v0.9.x content_sha256
// — without them, encoding/json defaults to capitalized field names and
// the substrate's hash of a non-empty `models:` value diverges from the
// CLI's hash of the same source YAML. See internal/agents.TierCandidate
// for the parallel pin + sign_test.go's known-vector test.
type TierCandidate struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model"    yaml:"model"`
}

// UnmarshalYAML accepts a tier candidate written EITHER as a mapping
// ({provider: X, model: Y}) or as a bare scalar string. A bare string is
// taken as the model with an empty provider — the natural way to name a
// models: alias (the alias supplies the provider) without repeating the
// pair, e.g. `- local-qwen`. Without this, a bare scalar fails to unmarshal
// into the struct ("cannot unmarshal !!str into config.TierCandidate"), which
// surprised an operator authoring an all-aliases tier list. Only the YAML
// INPUT shape is affected — the struct still marshals to the same JSON
// object, so content_sha256 (see the json: tags above) is unchanged.
func (tc *TierCandidate) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		tc.Provider = ""
		tc.Model = value.Value
		return nil
	}
	// Mapping form: decode via an alias type so we don't recurse into this
	// method.
	type rawTierCandidate TierCandidate
	var raw rawTierCandidate
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*tc = TierCandidate(raw)
	return nil
}

// UserTier is one named user-facing-tier policy. Operators define
// these in the top-level `user_tiers:` map; clients reference them by
// name via the `user_tier` field on POST /v1/runs. See Config.UserTiers
// for the precedence semantics.
//
// The "default" tier is required (validated at config-load) and covers
// requests that don't carry user_tier in the body — preserves v0.7.x
// behaviour for clients that haven't been bumped to the new wire field.
type UserTier struct {
	// ProviderPriority is the order the resolver walks for runs
	// carrying this user_tier (overlays the library-wide
	// ProviderPriority). Per-agent `providers:` overrides still apply
	// on top — when both are set, the agent's order wins WITHIN the
	// intersection of (agent.Providers, user_tier.ProviderPriority).
	// Empty intersection → resolver refuses with a typed error so the
	// client can render "agent not available for your tier".
	ProviderPriority []string `yaml:"provider_priority"`

	// Tiers is the per-task-tier (low/middle/high) candidate map for
	// this user_tier, overlaying the library-wide Tiers. Same per-
	// agent override semantics: agent.Models[tier] takes precedence
	// when set; otherwise this map; otherwise library.Tiers[tier].
	Tiers map[string][]TierCandidate `yaml:"tiers"`

	// FallbackOnError selects the v0.8.2 PR 2 runtime behaviour when
	// a provider call returns a retryable error (429, 5xx, network
	// timeout, stream-idle deadline). When true, the resolver re-
	// picks the next provider in the candidate list and the loop
	// continues with the new provider (subject to MaxFallbackAttempts).
	// When false, the error propagates to the client — the cost-cap
	// semantic for free tiers, where cascading would defeat the
	// budget guarantee.
	//
	// Defaults to true on the "default" tier so back-compat clients
	// keep the v0.7.x rate-limit retry behaviour they had.
	FallbackOnError bool `yaml:"fallback_on_error"`

	// MaxFallbackAttempts caps cumulative provider switches per run.
	// A run that hits Anthropic → DeepSeek → Gemini under fallback
	// would consume 3 attempts (the original + 2 fallbacks). Default
	// 3. PR 2 consumes this; PR 1 just plumbs it through. Zero falls
	// back to the default.
	MaxFallbackAttempts int `yaml:"max_fallback_attempts"`

	// RetryAttempts caps same-provider retries on retryable errors
	// (v0.12.9) before MarkRateLimited cools the matrix entry and
	// tryProviderFallback escalates to the next provider. Real
	// providers' 429s often clear within seconds (much shorter than
	// the 30s MarkRateLimited cooldown), so retrying the same
	// (provider, model) 1-3 times with exponential backoff often
	// recovers the run cheaper than escalating.
	//
	// 0 (default) preserves v0.12.x behaviour — the FIRST retryable
	// error invokes MarkRateLimited + fallback immediately. 1-3 is
	// the recommended range; backoff is 100ms / 300ms / 900ms so
	// even three attempts add at most 1.3s before fallback engages.
	// Capped internally at 5 to avoid pathological retry storms.
	//
	// Applies to BOTH the Call() error path (driver refused the
	// stream) and the in-stream EventError path (driver opened the
	// stream then surfaced an error mid-flight). Distinct from
	// MaxFallbackAttempts: retries stay on the same (provider,
	// model); fallbacks switch.
	RetryAttempts int `yaml:"retry_attempts"`

	// RateLimitCooldownMs overrides the resolver's hardcoded
	// 30-second MarkRateLimited cooldown per user_tier. The
	// resolver flips a (provider, model) entry to "unavailable"
	// for this many milliseconds after a 429 — Resolve() refuses
	// to pick the pair during the window, letting tryProviderFallback
	// engage cleanly without waiting on the periodic probe.
	//
	// Why per-tier: real providers' Retry-After windows vary widely.
	// Anthropic burst limits clear in 1-10s; Voyage AI per-minute
	// caps may take 30-60s; some self-hosted providers recover
	// instantly. A single hardcoded value (30s) is conservative for
	// some providers and wasteful for others. Operators tune per
	// the tier's actual fleet.
	//
	// 0 (default) keeps the v0.12.x behaviour — 30s default applied
	// inside the resolver. Values in [1_000, 600_000] (1 s to 10
	// min) accepted; positive out-of-range values silently clamp to
	// that window when the resolver overlay is built (see
	// clampRateLimitCooldownMs in internal/api/http/server.go — the
	// single source of truth for the bound), and a negative value is
	// rejected at config-load by validate(). Sub-second cooldowns would
	// defeat the purpose (the cascade would re-fire on the next call);
	// >10 min becomes meaningless because the periodic probe (default
	// 15 min) clears the matrix before the cooldown expires anyway.
	RateLimitCooldownMs int `yaml:"rate_limit_cooldown_ms"`
}

// AgentDef is one agent the API can address by name.
type AgentDef struct {
	Provider string `yaml:"provider"` // optional override of Defaults
	Model    string `yaml:"model"`    // alias or full model ID
	// Code is the inline code-js orchestrator source (RFC J). When set
	// and Provider is "code-js", the provider executes this body instead
	// of reading agent_code/<name>/index.js — the symmetry that lets a
	// code agent be ingested via AgentDef / yaml with no host filesystem
	// bind (containers, pure-cloud). Empty = fall back to the filesystem.
	// Gated by LOOMCYCLE_CODE_AGENTS_ENABLED at create/fork + execution.
	Code string `yaml:"code"`
	// SystemPrompt is the agent's system prompt as an inline YAML
	// string. Mutually exclusive with SystemPromptFile.
	SystemPrompt string `yaml:"system_prompt"`
	// SystemPromptBase is the pre-skill-bake SystemPrompt as it was
	// at config-load (before resolveSkills appended any skill
	// bodies). v0.8.22 SkillDef per-run resolution rebuilds the
	// effective SystemPrompt from this base + each skill's
	// DB-active-or-static body. Not yaml-driven; set by
	// resolveSkills.
	SystemPromptBase string `yaml:"-"`
	// SystemPromptFile points at a file whose contents become
	// SystemPrompt. Resolved relative to the YAML config file's
	// directory (so "agents/qa.md" works regardless of cwd). Useful
	// for prompts that don't fit inline — long .md files with
	// frontmatter, etc. The frontmatter is loaded verbatim; if you
	// want to strip it, use SystemPrompt + a preprocessor.
	SystemPromptFile string   `yaml:"system_prompt_file"`
	AllowedTools     []string `yaml:"allowed_tools"`
	// Skills lists skill names (each = a subdirectory under
	// LOOMCYCLE_SKILLS_ROOT containing SKILL.md) whose bodies are
	// concatenated onto SystemPrompt at config-load. Approach A in
	// doc-internal/skills-design.md: static bundling lets the skill
	// body land inside the cacheable system block, paying the
	// cache-write cost once per 5-min TTL.
	//
	// SECURITY: each named skill's allowed-tools frontmatter must be a
	// SUBSET of this agent's AllowedTools. resolveSkills enforces this
	// at config-load — a skill may only narrow, never widen, the tool
	// set the operator granted to the agent.
	Skills []string `yaml:"skills"`

	// MaxTokens caps per-iteration assistant output. Zero = use the
	// provider driver's default (4096 in anthropic, far below
	// haiku-4-5's 64k ceiling). Set explicitly for agents that emit
	// large structured output (verdict JSON for big batches, long
	// rewrites): without it, output truncates mid-response and the
	// caller's parser fails. Recommended values: 8192 for general
	// use, 16384+ for batch scoring agents.
	MaxTokens int `yaml:"max_tokens"`

	// MaxIterations caps the agent loop at this many provider calls
	// before terminating with stop_reason="max_iterations". Zero =
	// use the loop default (16). Set higher for discovery-style
	// agents whose workflow is intrinsically iterative
	// (search → enumerate → fetch → score across many tool calls).
	// The default 16 is fine for tightly-scoped agents (ats-filter,
	// qa-agent, cv-adapter) but too low for job-searcher /
	// employer-profiler / job-site-searcher which observed
	// max_iterations stop_reason in production at the 16-cap
	// (2026-05-21).
	MaxIterations int `yaml:"max_iterations"`

	// UnboundedIterations lifts the MaxIterations soft-cap for an LLM agent
	// (the 1<<20 hard backstop in the loop still applies as a runaway guard).
	// Cancel is the stop, and LLM runs have no wall-clock timeout — use this
	// for interactive / terminal-driven agents whose turn count is
	// operator-driven, not bounded by a fixed task. Code-js agents are
	// already exempt via their provider Capabilities().UnboundedIterations;
	// this is the LLM-side opt-in.
	UnboundedIterations bool `yaml:"unbounded_iterations"`

	// MaxConcurrentChildren caps how many sub-agents this agent may
	// spawn in parallel via Agent.parallel_spawn (v0.11.8+). Zero =
	// use the runtime default (4 — see builtin.DefaultMaxConcurrentChildren).
	// Sequential Agent.spawn calls are unaffected; the cap only
	// applies to a single parallel_spawn op's `spawns` array. Set
	// higher for orchestrator agents whose workflow legitimately
	// fans out to many specialists at once; keep at default for
	// linear-pipeline agents that don't need parallelism.
	MaxConcurrentChildren int `yaml:"max_concurrent_children"`

	// RunTimeoutSeconds is the per-agent wall-clock budget for a code-js
	// agent (RFC J), overriding the global LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_
	// SECONDS. 0 = use the global. A fan-out orchestrator's budget includes
	// the time it spends blocked in Agent.parallel_spawn awaiting LLM
	// children, so the CPU-oriented global default (120s) is structurally too
	// low for one — set this on the orchestrator agent rather than raising the
	// global for every code agent. Ignored by LLM agents. A /v1/runs request
	// may override this per-call (per-run > per-agent > global).
	RunTimeoutSeconds int `yaml:"run_timeout_seconds"`

	// Tier is the model-tier the resolver should pick from when
	// the agent doesn't declare an explicit Provider+Model pin.
	// One of "low" / "middle" / "high". Empty = no tier-based
	// resolution; the agent must use the explicit pin path.
	// Mutually exclusive with the explicit Provider+Model pin —
	// validation rejects setting both.
	Tier string `yaml:"tier"`

	// Effort is the reasoning-effort hint passed to the resolved
	// model where supported. One of "low" / "medium" / "high" or
	// empty (= no hint, driver default). Anthropic maps this to
	// thinking.budget_tokens; OpenAI to reasoning_effort; DeepSeek
	// V4 to its thinking-mode toggle. Silently ignored on models
	// without a reasoning surface (haiku-4-5, gpt-5.4-mini, etc.).
	// Real per-driver translation lands in PR 3 of the resolve-
	// matrix series; PR 1 plumbs the field through unchanged.
	Effort string `yaml:"effort"`

	// Sampling tunes the per-agent LLM sampling parameters (temperature,
	// top_p, …). nil = use the provider/model defaults. Per-run callers can
	// override individual fields via the /v1/runs `sampling` block (per-run
	// wins per-field; see MergeSampling). Each driver maps the params it
	// supports and drops the rest (the same translate-or-drop contract as
	// Effort). Pointer so a no-sampling agent stays byte-identical pre-feature.
	Sampling *Sampling `yaml:"sampling,omitempty"`

	// Compaction is the per-agent context-compaction block (yaml/JSON
	// `compaction:`). Controls keep-last-N / keep-first, the auto-compact
	// trigger, the summary target size, and an optional cheaper summary model.
	// nil = compaction disabled (auto) with defaults applied where a manual
	// compact runs. Inherited down the spawn tree (parent-set fields win; a
	// child def fills gaps), overridable per-spawn via the Agent tool.
	Compaction *Compaction `yaml:"compaction,omitempty"`

	// Volumes is the RFC AH Phase 1 filesystem-volume binding list — the
	// names of top-level `volumes:` entries the agent's file/exec tools
	// (Read/Write/Edit/Glob/Grep/Bash/NotebookEdit) may resolve paths
	// against. Validated at config-load to reference declared volumes
	// (mirrors how allowed_tools validates against registered tools). An
	// agent that declares NO volumes is implicitly bound to [default]
	// (backward-compatible). An agent that declares volumes is confined
	// to EXACTLY those — it does NOT implicitly also get `default`.
	// Sub-agents inherit a NARROW-ONLY intersection of (child-declared) ∩
	// (parent's active bindings); see server.runSubAgent.
	Volumes []string `yaml:"volumes"`

	// Providers is the per-agent override of the library
	// ProviderPriority for tier resolution. Full replacement
	// semantics: when set, the resolver uses this list verbatim
	// for this agent and ignores the library default. Has no
	// effect on agents using the explicit Provider+Model pin.
	Providers []string `yaml:"providers"`

	// Models is the per-agent override of the library Tiers map
	// (per-tier candidate lists). Same semantics as the library
	// version: keyed by tier name (low/middle/high), each value is
	// an ordered candidate list. Full replacement — when set for a
	// given tier, the resolver uses this list verbatim and ignores
	// the library tier definition. Useful for narrowing an agent
	// to a specific subset of providers (e.g. CV generator that
	// must stay on Anthropic for sensitive paths).
	Models map[string][]TierCandidate `yaml:"models"`

	// MemoryScopes is the v0.8.0 Memory tool scope allowlist. Empty
	// = no Memory access (the default-deny invariant — even if
	// `Memory` is in AllowedTools, agents without an explicit
	// memory_scopes list see refused calls). Currently accepts
	// "agent" and "user"; forward-compatible for "session" / "tenant"
	// when those scopes ship.
	MemoryScopes []string `yaml:"memory_scopes"`

	// MemoryQuotaBytes overrides the global per-(scope, scope_id)
	// byte cap (LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES) for this agent.
	// 0 = use the global default. Set higher for agents that
	// legitimately maintain large state (cv-adapter); set lower for
	// noisy agents you want to keep on a tight leash.
	MemoryQuotaBytes int `yaml:"memory_quota_bytes"`

	// SqlScopes is the RFC AA SQL Memory ACL — the closed set of
	// per-scope sqlite databases the agent may run sql_query / sql_exec
	// against. Empty = NO SQL access (the default-deny invariant: even
	// with Memory in allowed_tools, an agent without sql_scopes sees its
	// SQL ops refused). Closed enum {agent, user, run}:
	//
	//   - "agent" → this agent's durable DB (tenant-keyed, cross-run)
	//   - "user"  → this end-user's durable DB (tenant-keyed, cross-agent)
	//   - "run"   → an ephemeral DB dropped when the top-level run ends
	//
	// Same trust posture as MemoryScopes: the model picks the scope, the
	// operator-resolved config decides which scopes exist at all.
	SqlScopes []string `yaml:"sql_scopes"`

	// SqlQuotaBytes overrides the global per-scope SQL DB byte cap
	// (LOOMCYCLE_SQLMEM_QUOTA_BYTES) for this agent. 0 = use the global
	// default. Checked before each write (approximate; see internal/sqlmem).
	SqlQuotaBytes int `yaml:"sql_quota_bytes"`

	// MemoryBackend names a memory_backends entry / MemoryBackendDef
	// this agent routes its Memory ops through. Empty = the
	// operator-default backend (in-process). RFC I MR-3b. The name is
	// operator-resolved (static yaml or substrate Def); it is NEVER
	// model-supplied — same trust posture as MemoryScopes.
	MemoryBackend string `yaml:"memory_backend"`

	// Channels is the v0.8.4 Channel tool ACL for this agent —
	// per-side (publish / subscribe) allowlists naming channels the
	// agent may post to or read from. Entries may use a trailing
	// "/*" wildcard (`findings/*` matches `findings/alpha` but NOT
	// `findings`). Same trust model as AllowedTools / MemoryScopes —
	// operator-yaml is the floor; the model can never enlarge its
	// own access. Sub-agents inherit the parent's ACL via ctx.
	Channels AgentChannelACL `yaml:"channels"`

	// AgentDefScopes is the v0.8.5 AgentDef tool capability gate.
	// Default-deny when empty. Mirrors MemoryScopes' shape — having
	// AgentDef in allowed_tools is necessary but not sufficient; this
	// list narrows which mutation paths the agent can take. Closed
	// set:
	//
	//   - "self"         → may fork/promote/retire its OWN name
	//                       (== tools.AgentName(ctx))
	//   - "descendants"  → may operate on any def whose lineage chain
	//                       traces back to a def the agent created
	//   - "named:<name>" → may operate on the specified single name
	//                       (multi-entry: "named:foo" + "named:bar")
	//   - "any"          → unrestricted (operator-blessed orchestrator
	//                       privilege)
	//
	// "any" is intentionally a single string ("any") rather than a
	// wildcard pattern so the model never authors mass-mutation
	// access via a templated string.
	AgentDefScopes []string `yaml:"agent_def_scopes"`

	// ScheduleDefScopes is the v1.x RFC E ScheduleDef tool capability
	// gate. Default-deny when empty. Same closed set as
	// AgentDefScopes: "self" / "descendants" / "named:<name>" / "any".
	// Lets operator-blessed orchestrators author + fork schedules at
	// runtime; arbitrary agents have no access.
	ScheduleDefScopes []string `yaml:"schedule_def_scopes"`

	// A2AServerCardDefScopes is the v1.x RFC G A2AServerCardDef tool
	// capability gate. Default-deny when empty. Same closed set as
	// ScheduleDefScopes: "self" / "descendants" / "named:<name>" / "any".
	A2AServerCardDefScopes []string `yaml:"a2a_server_card_def_scopes"`

	// A2AAgentDefScopes is the v1.x RFC G A2AAgentDef tool capability
	// gate. Default-deny when empty. Same closed set as
	// ScheduleDefScopes: "self" / "descendants" / "named:<name>" / "any".
	A2AAgentDefScopes []string `yaml:"a2a_agent_def_scopes"`

	// (WebhookDefScopes removed in the v0.16.0 QA pass — dead config, the
	// identical defect class #316 removed for MemoryBackendDefScopes: the
	// WebhookDef tool is operator-admin-only, kept out of the agent registry,
	// and its callers build the policy with a hardcoded Scopes:["any"], so a
	// per-agent webhook_def_scopes yaml field was parsed but never read. Per
	// CLAUDE.md "no backward-compat shims for unused code", deleted.)

	// SkillDefScopes is the v0.8.22 SkillDef tool capability gate.
	// Default-deny when empty. Mirrors AgentDefScopes minus the
	// "self" scope (skills have no agent identity, so "self" is
	// meaningless for them). Closed set:
	//
	//   - "named:<skill-name>" → may operate on the specified single
	//                            skill name (multi-entry)
	//   - "descendants"        → may operate on any skill def whose
	//                            lineage chain traces back to one
	//                            the agent created (TODO v0.9.x —
	//                            currently equivalent to "any")
	//   - "any"                → unrestricted (operator-blessed
	//                            orchestrator privilege)
	SkillDefScopes []string `yaml:"skill_def_scopes"`

	// VolumeDefScopes is the RFC AH Phase 2a VolumeDef tool capability
	// gate. Default-deny when empty. Mirrors SkillDefScopes (no "self" —
	// a volume has no agent identity). Closed set:
	//
	//   - "named:<volume-name>" → may create/delete/purge the named
	//                             single volume (multi-entry)
	//   - "any"                 → unrestricted (operator-blessed
	//                             dynamic-ensemble launcher privilege)
	//
	// Gates create/delete/purge only; get/list are tenant-scoped reads
	// available to any bound agent (mirrors the other Def families'
	// read posture).
	VolumeDefScopes []string `yaml:"volume_def_scopes"`

	// EvaluationScopes is the v0.8.5 Evaluation tool capability gate.
	// Multi-select; default-deny when empty. Closed set:
	//
	//   - "submit_self"        → may emit evaluations against own runs
	//   - "submit_siblings"    → may emit evaluations against sibling
	//                             runs (same parent_agent_id)
	//   - "submit_descendants" → may emit evaluations against the
	//                             agent's spawn-tree descendants
	//   - "submit_any"         → unrestricted submit (operator
	//                             override; emitter_role = "unrelated"
	//                             when the agent has no kinship)
	//   - "read_any"           → may call list/aggregate ops against
	//                             any def or run
	//
	// Emitter role is derived server-side from the emitter's ctx vs
	// the target run's identity; the model never supplies it. The
	// scope list gates WHICH emitter roles the agent is allowed to
	// produce.
	EvaluationScopes []string `yaml:"evaluation_scopes"`

	// HistoryScope gates the v0.8.7 Context.history op. Closed set:
	//
	//	"self"        — caller may read its own run's transcript
	//	"siblings"    — RESERVED (not yet active in v0.8.7 PR 3)
	//	"descendants" — RESERVED
	//	"named:<n>"   — RESERVED
	//	"any"         — UNRESTRICTED. Caller may read ANY agent's
	//	                transcript INCLUDING transcripts owned by
	//	                OTHER users in multi-tenant deployments. There
	//	                is no user_id check today; `any` is a flat
	//	                operator-trust grant. Use only for admin /
	//	                debugging agents under operator control. For
	//	                "caller may read my own + my children's
	//	                transcripts" wait for siblings/descendants
	//	                scopes to activate (needs RunIdentityValue
	//	                ParentAgentID plumbing).
	//
	// Empty / unset = default-deny. Mirror of the substrate-scope
	// pattern (agent_def_scopes, evaluation_scopes).
	HistoryScope []string `yaml:"history_scope"`

	// DisableContext opts the agent OUT of the v0.8.7 default-add
	// behaviour. Normally every agent's allowed_tools is augmented
	// with "Context" at config-load — introspection is foundational
	// for self-evolving agents and missing it is a footgun. Operators
	// running airgapped or strictly-deterministic agents can set
	// `disable_context: true` to skip the default-add for that agent.
	//
	// Note: this only controls the AUTO-ADD. If an operator explicitly
	// lists "Context" in allowed_tools, that wins regardless of this
	// flag (explicit beats default).
	DisableContext bool `yaml:"disable_context"`

	// Interruption is the v0.8.16 Interruption tool ACL. Default-
	// deny: an absent block means Enabled is false and every
	// Interruption op returns is_error with a clear refusal.
	// Mirror of the substrate-ACL pattern (memory_scopes,
	// agent_def_scopes, evaluation_scopes) — operator-yaml is the
	// floor; the model can never enlarge its own access.
	Interruption AgentInterruptionACL `yaml:"interruption"`

	// RetryAttempts overrides the user_tier's same-provider retry
	// budget (UserTier.RetryAttempts) for this agent. Nullable so
	// "unset" (use tier default) is distinguishable from "0"
	// (explicitly NO retries — high-stakes agents force this even
	// under generous tiers).
	//
	// Why per-agent: a tier sets fleet-wide policy (free tier = 0
	// retries, paid tier = 2 retries), but specific high-stakes
	// agents (cv-adapter, evaluator, anything with side effects)
	// may want to refuse retries regardless of the tier. Adding
	// retries to side-effectful flows is a foot-gun; this gives
	// operators a per-agent escape hatch.
	//
	// Resolution order (in s.retryAttemptsForAgent):
	//   1. agent.RetryAttempts (if non-nil) wins
	//   2. user_tier.RetryAttempts otherwise
	//   3. 0 if neither is set
	//
	// A pointer keeps the yaml-omitted case ("unset") distinct from
	// the yaml-zero case ("explicitly disable retries"). When set,
	// must be >= 0; validator refuses negatives at config-load.
	RetryAttempts *int `yaml:"retry_attempts,omitempty"`
}

// Sampling is the per-agent LLM sampling-parameter block (the yaml/JSON
// `sampling:` object). Every field is a pointer (or, for Stop, a slice) so
// "unset" (nil) is distinct from a meaningful zero value — temperature 0.0 is
// DETERMINISTIC, not "use the default". Each provider driver maps the params
// it supports and silently drops the rest. nil Sampling = full provider/model
// defaults.
type Sampling struct {
	// Temperature: sampling randomness. Provider ranges differ (Anthropic
	// 0–1, OpenAI/Gemini 0–2); validated to 0–2 here, the API is the backstop.
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature"`
	// TopP: nucleus sampling probability mass (0–1).
	TopP *float64 `json:"top_p,omitempty" yaml:"top_p"`
	// TopK: top-k token cutoff (>=1). Anthropic / Gemini / Ollama only.
	TopK *int `json:"top_k,omitempty" yaml:"top_k"`
	// FrequencyPenalty / PresencePenalty (-2..2). OpenAI/DeepSeek/Ollama only.
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty" yaml:"frequency_penalty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty" yaml:"presence_penalty"`
	// Seed: deterministic-sampling seed where the provider supports it
	// (OpenAI/DeepSeek/Gemini/Ollama). Useful for reproducible breeder variants.
	Seed *int `json:"seed,omitempty" yaml:"seed"`
	// Stop: up to a few stop sequences.
	Stop []string `json:"stop,omitempty" yaml:"stop"`
}

// IsZero reports whether no sampling field is set (so callers can collapse an
// empty block to nil — keeps content hashes byte-stable for no-sampling agents).
func (s *Sampling) IsZero() bool {
	return s == nil || (s.Temperature == nil && s.TopP == nil && s.TopK == nil &&
		s.FrequencyPenalty == nil && s.PresencePenalty == nil && s.Seed == nil && len(s.Stop) == 0)
}

// Clone returns a deep copy (pointers + slice) so a merged result never aliases
// either input's fields.
func (s *Sampling) Clone() *Sampling {
	if s == nil {
		return nil
	}
	out := &Sampling{Stop: append([]string(nil), s.Stop...)}
	if s.Temperature != nil {
		v := *s.Temperature
		out.Temperature = &v
	}
	if s.TopP != nil {
		v := *s.TopP
		out.TopP = &v
	}
	if s.TopK != nil {
		v := *s.TopK
		out.TopK = &v
	}
	if s.FrequencyPenalty != nil {
		v := *s.FrequencyPenalty
		out.FrequencyPenalty = &v
	}
	if s.PresencePenalty != nil {
		v := *s.PresencePenalty
		out.PresencePenalty = &v
	}
	if s.Seed != nil {
		v := *s.Seed
		out.Seed = &v
	}
	return out
}

// MergeSampling overlays `over` onto `base` PER FIELD — a field set in `over`
// wins, an unset (nil) field in `over` keeps `base`'s value. Used for both the
// AgentDef fork overlay (a fork that sets only temperature keeps the parent's
// top_p) and the per-run override (a /v1/runs sampling field wins over the
// agent's, field by field). Returns nil only when both inputs contribute
// nothing. Never aliases either input.
func MergeSampling(base, over *Sampling) *Sampling {
	if base.IsZero() && over.IsZero() {
		return nil
	}
	out := base.Clone()
	if out == nil {
		out = &Sampling{}
	}
	if over == nil {
		return out
	}
	if over.Temperature != nil {
		v := *over.Temperature
		out.Temperature = &v
	}
	if over.TopP != nil {
		v := *over.TopP
		out.TopP = &v
	}
	if over.TopK != nil {
		v := *over.TopK
		out.TopK = &v
	}
	if over.FrequencyPenalty != nil {
		v := *over.FrequencyPenalty
		out.FrequencyPenalty = &v
	}
	if over.PresencePenalty != nil {
		v := *over.PresencePenalty
		out.PresencePenalty = &v
	}
	if over.Seed != nil {
		v := *over.Seed
		out.Seed = &v
	}
	if len(over.Stop) > 0 {
		out.Stop = append([]string(nil), over.Stop...)
	}
	return out
}

// Validate checks light per-field bounds (the provider API is the final
// authority). Returns a descriptive error naming the offending field.
func (s *Sampling) Validate() error {
	if s == nil {
		return nil
	}
	if s.Temperature != nil && (*s.Temperature < 0 || *s.Temperature > 2) {
		return fmt.Errorf("sampling.temperature %.3f out of range [0,2]", *s.Temperature)
	}
	if s.TopP != nil && (*s.TopP < 0 || *s.TopP > 1) {
		return fmt.Errorf("sampling.top_p %.3f out of range [0,1]", *s.TopP)
	}
	if s.TopK != nil && *s.TopK < 1 {
		return fmt.Errorf("sampling.top_k %d out of range (>=1)", *s.TopK)
	}
	if s.FrequencyPenalty != nil && (*s.FrequencyPenalty < -2 || *s.FrequencyPenalty > 2) {
		return fmt.Errorf("sampling.frequency_penalty %.3f out of range [-2,2]", *s.FrequencyPenalty)
	}
	if s.PresencePenalty != nil && (*s.PresencePenalty < -2 || *s.PresencePenalty > 2) {
		return fmt.Errorf("sampling.presence_penalty %.3f out of range [-2,2]", *s.PresencePenalty)
	}
	if len(s.Stop) > 8 {
		return fmt.Errorf("sampling.stop has %d sequences (max 8)", len(s.Stop))
	}
	return nil
}

// Compaction is the per-agent context-compaction block (the yaml/JSON
// `compaction:` object). Every field is a pointer so "unset" (nil) is distinct
// from a meaningful value — and so the per-field merge (parent/child/per-run/
// per-spawn) can tell "inherit" from "explicitly set". nil = no compaction
// configured (auto off; a manual compact uses the documented defaults).
type Compaction struct {
	// Enabled turns AUTO-compaction on. Default off (nil/false): the manual
	// Compact button + Context op=compact still work, but the loop never
	// auto-triggers. Opt-in so existing agents are byte-identical.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled"`
	// TargetPercentage shapes the summary: aim for ~N% of the compacted span's
	// length. Range 10..50; default 10 (aggressive).
	TargetPercentage *int `json:"target_percentage,omitempty" yaml:"target_percentage"`
	// KeepLastN keeps the last N messages verbatim (snapped to a clean user-turn
	// boundary so a tool_use/tool_result pair is never split). Default 4. 0 =
	// keep none (summarize the whole conversation).
	KeepLastN *int `json:"keep_last_n,omitempty" yaml:"keep_last_n"`
	// KeepFirst pins the first user message (the task) verbatim — never
	// summarized — so a long autonomous agent never loses its objective.
	// Default true.
	KeepFirst *bool `json:"keep_first,omitempty" yaml:"keep_first"`
	// AutoCompactAtPct is the trigger: auto-compact when used/window >= N%.
	// Range 50..95; default 80. Only consulted when Enabled and the provider
	// reports a context window.
	AutoCompactAtPct *int `json:"autocompact_at_pct,omitempty" yaml:"autocompact_at_pct"`
	// Model optionally runs the summary call on a cheaper/faster model SERVED BY
	// THE SAME PROVIDER (e.g. a haiku-class model). "" / nil = the run's model.
	Model *string `json:"model,omitempty" yaml:"model"`
}

// Compaction defaults — applied at use-time when a field is unset.
const (
	CompactionDefaultTargetPct = 10
	CompactionDefaultKeepLastN = 4
	CompactionDefaultKeepFirst = true
	CompactionDefaultAutoAtPct = 80
)

// IsZero reports whether no compaction field is set (collapse to nil → byte-
// stable content hashes for agents that don't configure compaction).
func (c *Compaction) IsZero() bool {
	return c == nil || (c.Enabled == nil && c.TargetPercentage == nil && c.KeepLastN == nil &&
		c.KeepFirst == nil && c.AutoCompactAtPct == nil && c.Model == nil)
}

// Clone deep-copies (every field is a pointer) so a merge never aliases an input.
func (c *Compaction) Clone() *Compaction {
	if c == nil {
		return nil
	}
	out := &Compaction{}
	if c.Enabled != nil {
		v := *c.Enabled
		out.Enabled = &v
	}
	if c.TargetPercentage != nil {
		v := *c.TargetPercentage
		out.TargetPercentage = &v
	}
	if c.KeepLastN != nil {
		v := *c.KeepLastN
		out.KeepLastN = &v
	}
	if c.KeepFirst != nil {
		v := *c.KeepFirst
		out.KeepFirst = &v
	}
	if c.AutoCompactAtPct != nil {
		v := *c.AutoCompactAtPct
		out.AutoCompactAtPct = &v
	}
	if c.Model != nil {
		v := *c.Model
		out.Model = &v
	}
	return out
}

// MergeCompaction overlays `over` onto `base` PER FIELD — a field set in `over`
// wins, an unset field keeps `base`'s value. Drives the AgentDef fork overlay,
// the per-run override, AND the spawn precedence blend (a child fills the gaps
// its parent left unset: MergeCompaction(childDef, parentSparse)). Returns nil
// only when both inputs are empty; never aliases either input.
func MergeCompaction(base, over *Compaction) *Compaction {
	if base.IsZero() && over.IsZero() {
		return nil
	}
	out := base.Clone()
	if out == nil {
		out = &Compaction{}
	}
	if over == nil {
		return out
	}
	if over.Enabled != nil {
		v := *over.Enabled
		out.Enabled = &v
	}
	if over.TargetPercentage != nil {
		v := *over.TargetPercentage
		out.TargetPercentage = &v
	}
	if over.KeepLastN != nil {
		v := *over.KeepLastN
		out.KeepLastN = &v
	}
	if over.KeepFirst != nil {
		v := *over.KeepFirst
		out.KeepFirst = &v
	}
	if over.AutoCompactAtPct != nil {
		v := *over.AutoCompactAtPct
		out.AutoCompactAtPct = &v
	}
	if over.Model != nil {
		v := *over.Model
		out.Model = &v
	}
	return out
}

// Validate checks per-field bounds. Returns a descriptive error naming the field.
func (c *Compaction) Validate() error {
	if c == nil {
		return nil
	}
	if c.TargetPercentage != nil && (*c.TargetPercentage < 10 || *c.TargetPercentage > 50) {
		return fmt.Errorf("compaction.target_percentage %d out of range [10,50]", *c.TargetPercentage)
	}
	if c.KeepLastN != nil && *c.KeepLastN < 0 {
		return fmt.Errorf("compaction.keep_last_n %d must be >= 0", *c.KeepLastN)
	}
	if c.AutoCompactAtPct != nil && (*c.AutoCompactAtPct < 50 || *c.AutoCompactAtPct > 95) {
		return fmt.Errorf("compaction.autocompact_at_pct %d out of range [50,95]", *c.AutoCompactAtPct)
	}
	return nil
}

// ContextPluginSpec is one entry in the runtime-wide context-transform plugin
// chain (RFC Z / F43). `Name` selects a built-in transformer from the
// contextplugin registry (the only built-in today is "redact"). The chain runs
// in declared order on a COPY of the outbound LLM request. This is plain
// config data (no behaviour) so the config package stays free of a dependency
// on the contextplugin/providers transform layer.
type ContextPluginSpec struct {
	// Name selects the built-in transformer (e.g. "redact"). Required.
	Name string `json:"name" yaml:"name"`
	// Enabled toggles this entry. nil/true = on; an explicit false leaves the
	// entry in the config (documented intent) but skips building it.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// RedactToolInput (redact plugin only): also scrub tool_use inputs in the
	// outbound request, not just text blocks. nil/false = text only.
	RedactToolInput *bool `json:"redact_tool_input,omitempty" yaml:"redact_tool_input,omitempty"`
}

// IsEnabled reports whether this spec should be built (nil Enabled = on).
func (s ContextPluginSpec) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// knownContextPluginNames mirrors the internal/contextplugin registry for
// load-time validation (config can't import contextplugin — cycle). Keep in
// sync when a built-in plugin is added.
var knownContextPluginNames = map[string]bool{"redact": true}

// AgentInterruptionACL is the per-agent v0.8.16 Interruption tool
// gate. Three fields:
//
//   - Enabled gates the tool entirely. False (default) → every op
//     returns is_error with a "not enabled" refusal.
//   - Kinds is the allowlist of interrupt kinds this agent may
//     create. v0.8.16 supports only "question". Empty (when
//     Enabled=true) defaults to ["question"]. Future "pause" /
//     "wait_until" / "approval" kinds land here as additive opt-ins
//     without a yaml shape change.
//   - MaxPending caps simultaneous pending interrupts on a single
//     run. 0 = use the operator's global default
//     (LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN). The lower of
//     the agent and operator caps wins.
type AgentInterruptionACL struct {
	// json tags mirror the yaml so the substrate (agentdef overlay /
	// register_agent) JSON shape is clean snake_case — the yaml-load path is
	// unaffected.
	Enabled    bool     `yaml:"enabled" json:"enabled,omitempty"`
	Kinds      []string `yaml:"kinds" json:"kinds,omitempty"`
	MaxPending int      `yaml:"max_pending" json:"max_pending,omitempty"`
}

// Channel is one operator-declared channel in the top-level
// `channels:` block. Operators must declare a channel explicitly
// before any agent yaml may grant publish/subscribe on it — there
// is no auto-creation. The fields here set per-channel defaults
// (overridable per-publish-call where it makes sense):
//
//   - Scope is the channel's cursor-isolation axis. Re-uses the
//     Memory vocabulary (agent / user) plus a new "global" scope
//     for cross-tenant fan-out (a global channel has ONE cursor
//     for the whole channel, regardless of which agent or user
//     reads from it).
//   - DefaultTTL is the per-message TTL in seconds applied when the
//     publish call doesn't supply one. Zero = no default (message
//     lives until the operator runs a manual purge or hits
//     MaxMessages).
//   - MaxMessages is the bounded-storage cap. When a publish would
//     push the per-(channel, scope, scope_id) count past this
//     value, the OLDEST rows are trimmed inside the same txn
//     (lossy-on-overflow per the v0.8.4 RFC — publishers never
//     block). Zero = unbounded.
//   - Semantic is "queue" or "broadcast" — informational only at
//     the storage level (the wire shape is identical). The tool
//     surface uses it for documentation and to warn at boot when
//     an ACL pattern looks wrong for the semantic (e.g. multiple
//     subscribers on a queue-shaped channel with the same scope
//     will compete for messages).
//
// Volume is one entry in the top-level `volumes:` map (RFC AH Phase 1)
// — a named filesystem root plus its access mode. It is the root the
// file/exec tools resolve paths against; an agent binds to a set of
// these by name via AgentDef.Volumes.
//
// Trust posture: static volumes are OPERATOR-AUTHORED (the same trust
// the legacy jail roots carry) and may map anywhere on disk, but the
// path MUST already exist + be a directory (validated at config-load —
// the runtime never creates a static volume). The dynamic, confined,
// auto-provisioned VolumeDef substrate is Phase 2, not in this struct.
type Volume struct {
	// Path is the absolute-or-relative directory root. Resolved to an
	// absolute path at config-load and validated to exist + be a dir.
	Path string `yaml:"path"`
	// Mode is "rw" (read+write) or "ro" (read-only). Empty defaults to
	// "rw" — validated to one of the two. Write/Edit/NotebookEdit and
	// Bash require "rw"; Read/Glob/Grep operate on either.
	Mode string `yaml:"mode"`
	// Default marks this volume as the one a tool call uses when it omits
	// the `volume` argument. At most one volume may set this (validated).
	Default bool `yaml:"default"`
	// DynamicRoot marks this static volume as the parent under which the
	// RFC AH Phase 2a dynamic VolumeDef substrate provisions per-tenant
	// directories (`<path>/<tenant-segment>/<name>`). At most ONE static
	// volume may set this (validated). When no volume sets it, `VolumeDef
	// create` refuses — there is no operator-blessed root to confine
	// dynamic volumes inside. The dynamic root itself must (already) exist
	// + be a directory, exactly like any static volume; a dynamic volume's
	// mode (ro/rw) is caller-chosen, independent of the root's mode.
	DynamicRoot bool `yaml:"dynamic_root"`
}

// ReadOnly reports whether this volume is read-only. Empty Mode (the
// default) is read+write; only an explicit "ro" is read-only. Mode is
// validated to {rw, ro, ""} at config-load, so a non-empty non-"ro"
// value here is already "rw".
func (v Volume) ReadOnly() bool { return v.Mode == "ro" }

type Channel struct {
	Scope       string `yaml:"scope"`
	DefaultTTL  int    `yaml:"default_ttl"`
	MaxMessages int    `yaml:"max_messages"`
	Semantic    string `yaml:"semantic"`

	// Description is an operator-visible documentation field. Not
	// used by the substrate; pure metadata surfaced in /v1/_channels
	// listings + the Web UI. Empty is fine — existing yaml without
	// a description loads unchanged. Added in v0.11.5 alongside
	// runtime channel CRUD; runtime-declared channels also carry
	// this field.
	Description string `yaml:"description"`

	// v0.8.6 system channels:
	//
	// Publisher restricts who may publish to this channel:
	//   - "" (default) — agents may publish if their ACL allows;
	//     admin endpoint may publish (bearer-authed).
	//   - "system" — agent ACL publishes are REFUSED. Only
	//     loomcycle's internal Go publisher AND the admin endpoint
	//     (POST /v1/_channels/_system/{name}/publish) may publish.
	//     Used for heartbeats, runtime-state, provider-events.
	//
	// Period sets the cadence for system-driven cadence publishes
	// (heartbeats). Required when Publisher == "system" AND the
	// channel name is NOT in the hard-coded event-driven set (see
	// eventDrivenSystemChannels in validate()). Parsed as a Go
	// time.Duration string (e.g. "1m", "5m", "1h") via the
	// PeriodDuration() helper. Empty string when not declared.
	//
	// Channel names starting with `_system/` are reserved — only
	// operator yaml may declare them; agents may subscribe (if their
	// ACL allows) but may not publish regardless of Publisher value.
	Publisher string `yaml:"publisher"`
	Period    string `yaml:"period"`
}

// PeriodDuration parses Period as a Go time.Duration. Returns 0 + nil
// when Period is empty; an error when the string is non-parseable.
func (c Channel) PeriodDuration() (time.Duration, error) {
	if c.Period == "" {
		return 0, nil
	}
	return time.ParseDuration(c.Period)
}

// AgentChannelACL carries the per-agent publish / subscribe
// allowlists for the v0.8.4 Channel tool. Empty Publish / Subscribe
// means no access on that side — the tool surfaces a typed refusal.
type AgentChannelACL struct {
	// json tags mirror the yaml so the substrate (agentdef overlay /
	// register_agent) JSON shape is clean snake_case — yaml-load is unaffected.
	Publish   []string `yaml:"publish" json:"publish,omitempty"`
	Subscribe []string `yaml:"subscribe" json:"subscribe,omitempty"`
}

// ScheduledRun is one entry in the v1.x RFC E `scheduled_runs:` yaml
// block. Two entry styles share this shape; the validator picks the
// path by inspecting `UserID`:
//
//   - TEMPLATE (UserID empty): orchestrators fork per user via the
//     ScheduleDef tool; the template supplies the agent + prompt +
//     per-tier cron defaults + required_credentials manifest.
//   - STANDALONE (UserID set): operator-owned periodic cron with
//     explicit identity. `UserCredentialsFromEnv` resolves
//     per-credential bearers from the env allowlist at boot.
//
// See rfcs/scheduled-agent-runs.md and `Context.help scheduled-runs`.
type ScheduledRun struct {
	// Agent is the agent name to invoke. Must resolve via lookup.Agent
	// (static cfg.Agents or substrate). Required.
	Agent string `yaml:"agent"`

	// Prompt is the input segments. Operators typically use
	// `trusted-text` here. Required.
	Prompt []ScheduledRunSegment `yaml:"prompt"`

	// Schedule is the cron expression (standard 5-field form per
	// `robfig/cron/v3`'s ParseStandard). For STANDALONE entries this
	// is required; for TEMPLATE entries it's optional (replaced by
	// UserTierSchedules per-tier defaults).
	Schedule string `yaml:"schedule"`

	// UserTierSchedules is the per-tier cron map for TEMPLATE entries.
	// Keys are operator-named tiers ("low" / "middle" / "high" /
	// operator-defined); values are cron expressions. Forks pick a
	// tier via the ScheduleDef fork op's `tier` overlay field, OR
	// supply an explicit `schedule` override.
	//
	// Mutually exclusive with `Schedule:` — a template can't fix one
	// cron AND offer per-tier defaults.
	UserTierSchedules map[string]string `yaml:"user_tier_schedules"`

	// RequiredCredentials lists the credential KEYS that forks must
	// populate in `user_credentials`. The fork op refuses with a
	// loud `ErrCredentialsIncomplete` if any required key is missing.
	// Names map to mcp_servers.<name> by convention; see
	// `Context.help per-run-credentials` (RFC F).
	RequiredCredentials []string `yaml:"required_credentials"`

	// Timezone is the cron-interpretation tz (IANA name, e.g.
	// "Europe/Berlin"). Empty defaults to UTC. Per RFC E sharp edge.
	Timezone string `yaml:"timezone"`

	// Enabled is the operator-yaml kill-switch. False = the sweeper
	// skips this schedule entirely without removing the entry. Useful
	// for staged rollouts + emergency disable.
	Enabled bool `yaml:"enabled"`

	// CatchUpMax bounds retroactive runs after a pause/outage. 0
	// (default) = no catch-up (sweeper runs at most ONCE on resume);
	// N > 0 = up to N retroactive fires. Per RFC E sharp edge.
	CatchUpMax int `yaml:"catch_up_max"`

	// MaxFires bounds the schedule's LIFETIME fire count (RFC S / F36).
	// 0 (default) = fire indefinitely until retired; N > 0 = the sweeper
	// auto-retires the def after its Nth fire (1 = one-shot). Fires of any
	// status count (a wedged schedule still retires); catch-up fires count
	// too — it's a hard lifetime cap regardless of cadence.
	MaxFires int `yaml:"max_fires"`

	// UserID is the run's identity anchor for STANDALONE entries.
	// Empty = TEMPLATE entry (orchestrators fork with their per-user
	// identity supplied via the ScheduleDef tool overlay).
	UserID string `yaml:"user_id"`

	// UserCredentialsFromEnv maps credential KEYS to env-var names
	// for STANDALONE entries. The env var must be in the LOOMCYCLE_*
	// allowlist (validated at config-load). Templates set their
	// credentials via fork-time overlay; STANDALONE schedules use
	// this env-indirection escape valve.
	UserCredentialsFromEnv map[string]string `yaml:"user_credentials_from_env"`

	// OnComplete is the closed-set delivery hooks fired after a
	// successful run. Kinds: channel.publish, mcp.call, memory.set.
	// Operator-yaml authoring. Runtime hooks added via the admin HTTP
	// surface live in the substrate state, not here.
	OnComplete []ScheduledRunHook `yaml:"on_complete"`

	// Metadata is NON-SECRET structured metadata passed to the agent as
	// TRUSTED (operator-authored) via RunInput.Metadata. Per-fork metadata
	// (e.g. a different repo per fork) falls out of the overlay naturally.
	// Not for secrets — those use UserCredentials / user_credentials_from_env.
	Metadata map[string]any `yaml:"metadata"`

	// TenantID is the tenant the fired run EXECUTES as (RFC N follow-up).
	// It flows to RunInput.TenantID, so the run resolves that tenant's
	// agents/skills/MCP and isolates its memory/runs. "" = shared/default
	// (no tenant scoping). Operator-authored (def content), never inbound.
	TenantID string `json:"tenant_id,omitempty" yaml:"tenant_id"`
}

// ScheduledRunSegment mirrors the loop.PromptSegment wire shape but
// stays in the config layer to avoid a circular import. The runtime
// converts at sweeper-fire time.
type ScheduledRunSegment struct {
	Role    string                       `yaml:"role"`
	Content []ScheduledRunSegmentContent `yaml:"content"`
}

// ScheduledRunSegmentContent mirrors the loop.PromptSegmentContent wire shape.
type ScheduledRunSegmentContent struct {
	Type string `yaml:"type"`
	Text string `yaml:"text"`
}

// ScheduledRunHook is one entry in OnComplete. Kind is enum-restricted
// at validation time (`channel.publish` / `mcp.call` / `memory.set`).
// The remaining fields are kind-specific; the dispatcher consults Kind
// to know which fields to read. JSON-as-yaml shape (operators can use
// inline yaml shortcuts).
type ScheduledRunHook struct {
	Kind    string                 `yaml:"kind"`
	Channel string                 `yaml:"channel"` // for kind=channel.publish
	Server  string                 `yaml:"server"`  // for kind=mcp.call
	Tool    string                 `yaml:"tool"`    // for kind=mcp.call
	Scope   string                 `yaml:"scope"`   // for kind=memory.set
	Key     string                 `yaml:"key"`     // for kind=memory.set
	Args    map[string]interface{} `yaml:"args"`    // for kind=mcp.call
	Payload map[string]interface{} `yaml:"payload"` // for kind=channel.publish + memory.set value
}

// A2AServerCard is one entry in the v1.x RFC G `a2a_server_cards:`
// yaml block. It declares which loomcycle agents are exposed over the
// A2A protocol + the AgentCard metadata served at the peer-facing
// well-known URI. Field set + yaml tags MUST mirror the tool-layer
// mergedA2AServerCardDef + lookup.SubstrateA2AServerCardDef shapes; a
// 3-way drift test pins parity.
type A2AServerCard struct {
	Name            string                `yaml:"name"`
	Description     string                `yaml:"description"`
	Provider        A2AServerCardProvider `yaml:"provider"`
	Capabilities    A2AServerCardCaps     `yaml:"capabilities"`
	ExposedAgents   []A2AExposedAgent     `yaml:"exposed_agents"`
	SecuritySchemes []A2ASecurityScheme   `yaml:"security_schemes"`
	// SignWithKeyEnv names the env var holding the per-tenant signing
	// key used to sign the served AgentCard. The env-allowlist check
	// is enforced at card-serving time (a later slice), NOT here.
	SignWithKeyEnv string `yaml:"sign_with_key_env"`
}

// A2AServerCardProvider mirrors the AgentCard `provider` block.
type A2AServerCardProvider struct {
	Organization string `yaml:"organization"`
	URL          string `yaml:"url"`
}

// A2AServerCardCaps mirrors the AgentCard `capabilities` block.
type A2AServerCardCaps struct {
	Streaming         bool `yaml:"streaming"`
	PushNotifications bool `yaml:"push_notifications"`
	ExtendedAgentCard bool `yaml:"extended_agent_card"`
}

// A2AExposedAgent declares one loomcycle agent exposed as an A2A skill.
type A2AExposedAgent struct {
	AgentName   string   `yaml:"agent_name"`
	SkillID     string   `yaml:"skill_id"`
	SkillName   string   `yaml:"skill_name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	InputModes  []string `yaml:"input_modes"`
	OutputModes []string `yaml:"output_modes"`
}

// A2ASecurityScheme mirrors one AgentCard security scheme entry. Kind
// is enum-restricted at validation time ("http"/"apiKey"/"oauth2"/"mtls").
type A2ASecurityScheme struct {
	Kind   string `yaml:"kind"`
	Scheme string `yaml:"scheme"`
}

// A2AAgent is one entry in the v1.x RFC G `a2a_agents:` yaml block. It
// declares a REMOTE A2A peer: either its well-known card URL, OR a
// direct endpoint+binding, plus the auth + expected-skills manifest.
// Field set + yaml tags MUST mirror the tool-layer mergedA2AAgentDef +
// lookup.SubstrateA2AAgentDef shapes; a 3-way drift test pins parity.
type A2AAgent struct {
	AgentCardURL     string             `yaml:"agent_card_url"`
	Endpoint         string             `yaml:"endpoint"`
	Binding          string             `yaml:"binding"`
	Auth             A2AAgentAuth       `yaml:"auth"`
	ExpectedSkills   []A2AExpectedSkill `yaml:"expected_skills"`
	VerifySignedCard bool               `yaml:"verify_signed_card"`
}

// A2AAgentAuth mirrors the remote-peer auth block. Scheme is
// enum-restricted ("http"/"apiKey"/"oauth2"/"mtls"). BearerCredentialRef
// names a key in the run's UserCredentials map.
type A2AAgentAuth struct {
	Scheme              string `yaml:"scheme"`
	BearerCredentialRef string `yaml:"bearer_credential_ref"`
}

// A2AExpectedSkill is one skill the remote peer is expected to expose.
type A2AExpectedSkill struct {
	ID       string `yaml:"id"`
	Required bool   `yaml:"required"`
}

// Webhook is one entry in the v1.x RFC H `webhooks:` yaml block. It
// declares an INBOUND HTTP webhook: how an external system reaches
// loomcycle to trigger an agent run (delivery=spawn) or publish to a
// channel (delivery=channel), plus the auth, rate limit, payload
// mapping, and on_complete hooks.
//
// Unlike the A2A config structs (yaml-only), Webhook carries BOTH json
// and yaml tags: the SAME field set backs the tool-layer merged shape
// (WH-2), which persists snake_case JSON into webhook_defs.definition.
// A 3-way drift test (yaml ↔ lookup.SubstrateWebhookDef ↔ json) pins
// parity. on_complete reuses ScheduledRunHook — the same hook shape
// ScheduleDef uses — rather than a parallel type.
type Webhook struct {
	Enabled                bool              `json:"enabled,omitempty" yaml:"enabled"`
	Delivery               string            `json:"delivery,omitempty" yaml:"delivery"`
	Agent                  string            `json:"agent,omitempty" yaml:"agent"`
	Channel                string            `json:"channel,omitempty" yaml:"channel"`
	Auth                   WebhookAuth       `json:"auth,omitempty" yaml:"auth"`
	RateLimit              WebhookRateLimit  `json:"rate_limit,omitempty" yaml:"rate_limit"`
	BodySizeLimitBytes     int               `json:"body_size_limit_bytes,omitempty" yaml:"body_size_limit_bytes"`
	UserCredentialsFromEnv map[string]string `json:"user_credentials_from_env,omitempty" yaml:"user_credentials_from_env"`
	// UserCredentials maps credential KEY → explicit bearer VALUE (RFC F),
	// parity with ScheduleDef forks. Resolved into the spawned run's
	// UserCredentials + substituted at the MCP transport. Receiver
	// precedence: env-resolved < these fork-time values < payload-projected
	// `user_credentials.<name>` (the live per-delivery token wins). NOTE: a
	// literal secret here is baked into the signed/content-hashed/snapshotted
	// def — the weaker posture vs env/payload; prefer those.
	UserCredentials map[string]string `json:"user_credentials,omitempty" yaml:"user_credentials"`
	// Metadata is NON-SECRET structured metadata (repo, review policy,
	// preferred skills, …) passed to the agent as TRUSTED (def-authored) via
	// RunInput.Metadata. Not for secrets — those use UserCredentials*.
	Metadata map[string]any `json:"metadata,omitempty" yaml:"metadata"`
	// TenantID is the tenant the spawned run EXECUTES as (RFC N follow-up).
	// It flows to RunInput.TenantID so the run resolves that tenant's
	// agents/skills/MCP and isolates its memory/runs. "" = shared/default.
	// SECURITY: tenant comes from this STATIC operator-authored def ONLY —
	// NEVER from the inbound payload / payload_mapping (attacker-influenceable).
	TenantID       string              `json:"tenant_id,omitempty" yaml:"tenant_id"`
	PayloadMapping map[string]string   `json:"payload_mapping,omitempty" yaml:"payload_mapping"`
	SyncResponse   WebhookSyncResponse `json:"sync_response,omitempty" yaml:"sync_response"`
	OnComplete     []ScheduledRunHook  `json:"on_complete,omitempty" yaml:"on_complete"`
}

// WebhookAuth declares how inbound webhook requests are authenticated.
// Kind is "hmac" (default) or "bearer". For hmac, the signature header
// carries an HMAC of the body keyed by the secret in SigningSecretEnv.
type WebhookAuth struct {
	Kind             string `json:"kind,omitempty" yaml:"kind"`
	Algorithm        string `json:"algorithm,omitempty" yaml:"algorithm"`
	Header           string `json:"header,omitempty" yaml:"header"`
	SigningSecretEnv string `json:"signing_secret_env,omitempty" yaml:"signing_secret_env"`
	DeliveryIDHeader string `json:"delivery_id_header,omitempty" yaml:"delivery_id_header"`
	BearerTokenEnv   string `json:"bearer_token_env,omitempty" yaml:"bearer_token_env"`
}

// WebhookRateLimit bounds inbound request volume per webhook.
type WebhookRateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty" yaml:"requests_per_minute"`
	Burst             int `json:"burst,omitempty" yaml:"burst"`
}

// WebhookSyncResponse, when enabled, holds the inbound HTTP request
// open until the triggered run completes (or TimeoutMs elapses).
type WebhookSyncResponse struct {
	Enabled   bool `json:"enabled,omitempty" yaml:"enabled"`
	TimeoutMs int  `json:"timeout_ms,omitempty" yaml:"timeout_ms"`
}

// MemoryBackend is one entry in the RFC I MR-3a `memory_backends:` yaml
// block. It declares a named memory backend: the kind (inprocess or
// mem9), connection config, tenancy strategy, and fallback behaviour.
//
// Like Webhook (and unlike the A2A yaml-only structs), MemoryBackend
// carries BOTH json and yaml tags: the SAME field set backs the
// tool-layer merged shape, which persists snake_case JSON into
// memory_backend_defs.definition. A 3-way drift test (yaml ↔
// lookup.SubstrateMemoryBackendDef ↔ json) pins parity. RFC I MR-3a /
// mirrors Webhook. Nothing consumes this yet — the per-agent routing +
// factory land in MR-3b.
type MemoryBackend struct {
	// Name is stamped from the registry key by the MemoryBackendDef tool
	// (like A2AServerCardDef) so the persisted def is self-describing —
	// a MemoryBackendDef is addressed by name. On the yaml side the key
	// is the name, so this is normally left empty in operator config.
	Name                       string               `json:"name,omitempty" yaml:"name"`
	Kind                       string               `json:"kind,omitempty" yaml:"kind"`
	Config                     MemoryBackendConfig  `json:"config,omitempty" yaml:"config"`
	TenancyStrategy            MemoryBackendTenancy `json:"tenancy_strategy,omitempty" yaml:"tenancy_strategy"`
	FallbackOnError            string               `json:"fallback_on_error,omitempty" yaml:"fallback_on_error"`
	HealthCheckIntervalSeconds int                  `json:"health_check_interval_seconds,omitempty" yaml:"health_check_interval_seconds"`
}

// MemoryBackendConfig holds the connection config for a remote memory
// backend (kind=mem9). api_key_env is an env-var NAME — the value is
// resolved (allowlist-gated) at use time in MR-4, never at config load.
type MemoryBackendConfig struct {
	BaseURL    string `json:"base_url,omitempty" yaml:"base_url"`
	APIVersion string `json:"api_version,omitempty" yaml:"api_version"`
	APIKeyEnv  string `json:"api_key_env,omitempty" yaml:"api_key_env"`
}

// MemoryBackendTenancy declares how a shared backend is partitioned per
// tenant. key_per_tenant resolves a distinct API key per tenant via
// EnvPattern; shared_key_with_prefix namespaces a single key's keyspace
// via PrefixPattern. Both patterns interpolate {tenant_id}.
type MemoryBackendTenancy struct {
	Kind          string `json:"kind,omitempty" yaml:"kind"`
	EnvPattern    string `json:"env_pattern,omitempty" yaml:"env_pattern"`
	PrefixPattern string `json:"prefix_pattern,omitempty" yaml:"prefix_pattern"`
}

// MCPServer declares one MCP server. Transport "stdio" or "http".
type MCPServer struct {
	Transport string            `yaml:"transport"`
	Command   string            `yaml:"command"` // stdio
	Args      []string          `yaml:"args"`    // stdio
	Env       map[string]string `yaml:"env"`     // stdio
	URL       string            `yaml:"url"`     // http
	Headers   map[string]string `yaml:"headers"` // http
	PoolSize  int               `yaml:"pool_size"`
	// AllowedTools narrows which of the server's discovered tools are
	// exposed to agents. Empty (default) = expose every tool the server
	// advertises via tools/list. Use this to opt out of expensive or
	// unwanted tools without forking the MCP server itself.
	AllowedTools []string `yaml:"allowed_tools"`
}

// Concurrency caps for the runtime.
type Concurrency struct {
	MaxConcurrentRuns int `yaml:"max_concurrent_runs"`
	MaxQueueDepth     int `yaml:"max_queue_depth"`
	QueueTimeoutMS    int `yaml:"queue_timeout_ms"`
	// MaxConcurrentRunsPerUser caps the in-flight (active+queued)
	// runs per non-empty user_id, inside the global MaxConcurrentRuns
	// cap. Default 0 = unlimited per user (back-compat; existing
	// deployments see no behavior change on upgrade). When >0, a 5th
	// run by a user at cap=4 returns HTTP 429 with
	// `code: "per_user_quota_exhausted"` + `Retry-After: 5`.
	// Env: LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER.
	MaxConcurrentRunsPerUser int `yaml:"max_concurrent_runs_per_user"`
}

// QueueTimeout returns QueueTimeoutMS as a duration.
func (c Concurrency) QueueTimeout() time.Duration {
	return time.Duration(c.QueueTimeoutMS) * time.Millisecond
}

// CacheConfig is the cache layer config; v0.1 only honours .Native.Enabled.
type CacheConfig struct {
	ResponseKV ResponseKVConfig  `yaml:"response_kv"`
	Native     NativeCacheConfig `yaml:"native"`
}

type ResponseKVConfig struct {
	Backend string `yaml:"backend"` // "memory" | "redis"
	TTL     string `yaml:"ttl"`     // duration string, e.g. "5m"
}

type NativeCacheConfig struct {
	Enabled bool `yaml:"enabled"`
}

// Env is the secrets layer, loaded from process environment.
type Env struct {
	AnthropicAPIKey string
	OpenAIAPIKey    string
	// VoyageAPIKey enables the v0.10.2 Voyage AI embedder, registered
	// under the `anthropic` provider slot (Anthropic has no native
	// embeddings API and explicitly recommends Voyage AI). When
	// `memory.embedder.provider: anthropic` is set in yaml, main.go's
	// buildEmbedder uses this key rather than AnthropicAPIKey for the
	// Voyage HTTP calls. Empty = the anthropic embedder driver
	// constructs but every Embed() call surfaces 401. Voyage's
	// canonical env-var name is reused verbatim.
	// Env: VOYAGE_API_KEY.
	VoyageAPIKey string
	// OllamaBaseURL is the local-network Ollama endpoint. Drives the
	// `ollama-local` provider registration. Default
	// `http://localhost:11434` keeps existing deploys unchanged across
	// the v0.8.3 split — operators with this var set keep working with
	// no further action. Setting `OLLAMA_BASE_URL=disabled` opts out
	// of registering `ollama-local`.
	OllamaBaseURL string
	// OllamaAPIKey enables the `ollama` provider (hosted ollama.com,
	// Bearer auth). Empty = provider not registered. Same on/off
	// semantics as the other paid-cloud providers (Anthropic / OpenAI /
	// DeepSeek / Gemini).
	OllamaAPIKey string
	// OllamaCloudBaseURL overrides the hosted ollama.com endpoint
	// (https://ollama.com) for staged rollouts or vendor mirrors.
	// Defaults to the public hosted endpoint; ignored when
	// OllamaAPIKey is empty.
	OllamaCloudBaseURL string
	// OllamaLocalNumCtx sets options.num_ctx on every chat request the
	// `ollama-local` driver sends. Default 0 = omit (Ollama server
	// uses model's Modelfile PARAMETER num_ctx, falling back to 4096).
	// The 4096 default is the load-bearing reason this knob exists:
	// without an explicit value, Ollama silently truncates prompts at
	// 4096 tokens with no error — a long input produces a partial
	// completion that doesn't reach end_turn. See
	// internal/providers/ollama/driver.go Driver.WithNumCtx for the
	// full incident context. Env: LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX.
	OllamaLocalNumCtx int
	// OllamaNumCtx is the hosted-ollama-cloud counterpart. Empty
	// default lets the cloud apply its own per-model context. Env:
	// LOOMCYCLE_OLLAMA_NUM_CTX. Separate from OllamaLocalNumCtx
	// because the relevant model menu (kimi-k2.6, etc.) differs from
	// what runs on a local box, so the right value almost certainly
	// differs too.
	OllamaNumCtx int
	// OllamaLocalNumGpu sets options.num_gpu on every chat request the
	// `ollama-local` driver sends — the number of model layers Ollama
	// offloads to the GPU. Default 0 = omit (Ollama auto-detects). Set
	// it (e.g. 99 = "all layers") to force GPU offload on a box where
	// Ollama otherwise falls back to CPU — common on integrated/APU
	// GPUs that auto-detection underestimates. Global to ollama-local,
	// like num_ctx: num_gpu is a model-LOAD parameter, not a per-request
	// knob. Env: LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU.
	OllamaLocalNumGpu int
	// DeepSeekAPIKey enables the `provider: deepseek` driver. Empty
	// = provider not registered (agents that ask for it fail at
	// resolve time, mirroring OpenAI / Anthropic behaviour).
	DeepSeekAPIKey string
	// DeepSeekBaseURL overrides the public DeepSeek endpoint
	// (https://api.deepseek.com/v1) for self-hosted OpenAI-
	// compatible mirrors (e.g. an internal vLLM serving a DeepSeek
	// model). Empty = use the public endpoint.
	DeepSeekBaseURL string
	// GeminiAPIKey enables the `provider: gemini` driver. Empty =
	// provider not registered. Set to a Google AI Studio key
	// (https://aistudio.google.com/apikey) or a Vertex AI service
	// account credential exchanged for a `gcloud auth print-access-token`
	// when GeminiBaseURL points at a Vertex AI gateway.
	GeminiAPIKey string
	// GeminiBaseURL overrides the public generativelanguage.googleapis.com
	// endpoint. Set to a Vertex AI Gemini endpoint
	// (https://{region}-aiplatform.googleapis.com/v1beta) for
	// production deployments that route through GCP project quotas
	// rather than the public AI Studio API. Empty = public endpoint.
	GeminiBaseURL string
	ListenAddr    string
	// PublicURL is the operator's externally-reachable base URL for THIS
	// loomcycle instance (e.g. behind a tunnel/proxy). Advertised to agents via
	// `Context op=self` (server.url) so an agent — especially one connected over
	// the MCP transport — can identify the server it's talking to. Empty = unset;
	// self then reports only the bind ListenAddr (and the A2A advertise URL if
	// that's configured). LOOMCYCLE_PUBLIC_URL.
	PublicURL string
	// MaxRequestBytes caps the JSON body the run-ingest paths accept
	// (POST /v1/runs and POST /v1/sessions/{id}/messages). Default 16 MiB —
	// raised from the historical 1 MiB so a request can carry inline base64
	// image content (RFC AT). MaxBytesReader still hard-stops at this bound;
	// operators tune it down to shrink the per-request memory ceiling.
	// LOOMCYCLE_MAX_REQUEST_BYTES (bytes; <=0 ignored, default kept).
	MaxRequestBytes int64
	AuthToken       string
	// OperatorTokenPepper is prepended to a bearer before SHA-256 when
	// hashing OperatorTokenDef tokens (RFC L). A stolen DB dump without
	// the pepper yields no usable token lookup. Secret — never logged.
	OperatorTokenPepper string
	// AuditLogPath is the JSONL sink for OperatorTokenDef mutations
	// (RFC L). Empty = no file audit (a NopSink is wired).
	AuditLogPath string
	// AuthVerbose (LOOMCYCLE_AUTH_VERBOSE=1) logs a server-side reason
	// when a bearer is rejected. Off by default; the wire 401 stays
	// opaque regardless (no oracle) — this only affects local logs.
	AuthVerbose bool
	DataDir     string
	// HTTPHostAllowlist is the comma-separated list of hostnames the
	// HTTP and WebFetch tools may reach. Empty = both tools refuse all
	// calls. Suffix-matched: an entry "example.com" matches both
	// "example.com" and "api.example.com". RFC1918, loopback, and
	// link-local addresses are HARD-blocked regardless of allowlist.
	// Loopback aliases (localhost, 127.0.0.1, ::1, *.localhost) are
	// stripped at startup — use HTTPPrivateHostAllowlist below to
	// permit specific loopback hosts at the IP layer.
	HTTPHostAllowlist []string
	// HTTPPrivateHostAllowlist names hosts whose resolved private IPs
	// are allowed at dial time. Suffix-matched. Use to permit agent
	// callbacks to a localhost-bound application API. Default empty
	// (no exception). Example: "localhost,127.0.0.1".
	HTTPPrivateHostAllowlist []string
	// MCPAllowPrivateIPs controls whether the MCP-HTTP client may dial
	// private/loopback/metadata IPs. DEFAULT true (MCP servers are commonly
	// operator-run on localhost / a private network — incl. loomcycle's own
	// jobs-search-agent /api/mcp consumer). Set LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS=0
	// to enable a dial-time DNS-rebinding block; HTTPPrivateHostAllowlist then
	// exempts specific internal MCP hosts.
	MCPAllowPrivateIPs bool
	// HTTPCallerAuthoritative flips the per-request allowed_hosts
	// trust model: when true, caller's list is the sole policy
	// (operator's HTTPHostAllowlist becomes a fallback for runs that
	// don't carry their own list). When false (default), caller can
	// only narrow operator's list, never widen. Operator opts in once
	// via LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1.
	HTTPCallerAuthoritative bool
	// ResumeFanout enables RFC X Phase 3: a fan-out PARENT blocked in
	// Agent.parallel_spawn cooperatively PARKS on pause (so paused_runs_count
	// includes it + the warning clears), and a snapshotted mid-fan-out parent
	// is RESUMABLE on a fresh instance (a spawn ledger is persisted; resume
	// reconciles the children + synthesizes the parallel_spawn tool_result).
	// Default OFF: when unset, pause/snapshot/resume behave exactly as before
	// (no ledger events, no park watcher, no reconcile). Operator opts in via
	// LOOMCYCLE_RESUME_FANOUT=1; should be on at BOTH the capturing and
	// restoring instances for a cross-instance hand-off.
	ResumeFanout bool
	// BraveAPIKey enables the WebSearch tool. Empty = WebSearch refuses
	// every call. Lives at https://api.search.brave.com/.
	BraveAPIKey string
	// BashEnabled gates the Bash tool. Defaults to false. Even when
	// true the tool is NOT a true sandbox — it restricts cwd, scrubs
	// env, bounds output, and times out, but cannot prevent the spawned
	// process from reaching arbitrary files via absolute paths or making
	// network calls. Operators wanting real isolation should run
	// loomcycle inside a container or VM.
	BashEnabled bool
	// BashboxEnabled gates the Bashbox tool — a TRUE in-process sandbox
	// (gbash) that spawns no OS process, roots all paths at the mounted
	// volume, and has no network. Unlike Bash it HONORS read-only volumes
	// (writes hit an in-RAM overlay, never the host). Defaults to false;
	// enable with LOOMCYCLE_BASHBOX_ENABLED=1. gbash is alpha — the per-agent
	// allowed_tools gate is the escape hatch.
	BashboxEnabled bool
	// BashboxFallbackCommands is the operator allowlist of host commands that
	// gbash does NOT implement (e.g. git, gh) and that may ESCAPE the Bashbox
	// sandbox to run on the real host shell (RFC AJ §13). Empty (default) = no
	// fallback. Only these names reach the host; every other command stays
	// sandboxed. Requires a rw volume. Sourced from
	// LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS (comma-separated).
	BashboxFallbackCommands []string
	// BashboxFallbackAllowedEnv names env vars passed into fallback commands
	// (e.g. GH_TOKEN, HOME, SSH_AUTH_SOCK) — injected only into the host child,
	// never the sandbox. PATH always passes. Sourced from
	// LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV (comma-separated).
	BashboxFallbackAllowedEnv []string
	// SkillsRoot points at a directory holding subdirectories of the
	// shape `<name>/SKILL.md`. When unset, agents may not list skills
	// (resolveSkills errors loudly to surface the misconfiguration —
	// silently dropping skill bodies would defeat the prompts that
	// reference them). Sourced from LOOMCYCLE_SKILLS_ROOT.
	SkillsRoot string

	// AgentsRoot points at a directory of flat `<name>.md` files.
	// Each file's YAML frontmatter is parsed as an AgentDef base; the
	// body becomes SystemPrompt. When set, discovered agents seed
	// cfg.Agents BEFORE the yaml `agents:` block; yaml entries with
	// matching names override per-field (mergeAgentDef in this file).
	// Empty AgentsRoot leaves cfg.Agents to the yaml-only path —
	// today's behaviour.
	//
	// Why this exists: synchronising the yaml `agents:` block with
	// the .claude/agents/<name>.md files referenced from
	// system_prompt_file is recurring operational pain (every dev↔main
	// branch divergence breaks loomcycle on the deploy box). The
	// directory becomes the single source of truth in normal
	// operation; yaml entries shrink to per-environment overrides.
	// Sourced from LOOMCYCLE_AGENTS_ROOT.
	AgentsRoot string

	// HelpRoot points at a directory of flat `<name>.md` files holding
	// help topics for the Context.help op. When set, files at this root
	// overlay the binary's bundled topics (filesystem wins per topic
	// name). When unset, only bundled topics are available. Operators
	// use this to publish deployment-specific guidance (e.g. local
	// policy docs, internal MCP server walkthroughs) without rebuilding
	// loomcycle. Sourced from LOOMCYCLE_HELP_ROOT.
	HelpRoot string

	// HeartbeatSweeperEnabled controls the v0.5.0 stale-run sweeper.
	// When true (default), a goroutine periodically marks runs whose
	// heartbeat hasn't advanced in HeartbeatStaleAfter as failed —
	// prevents the active-run lists from accumulating dead rows when
	// the host process crashes mid-loop. Disable with
	// LOOMCYCLE_HEARTBEAT_SWEEPER=0 (e.g. when an external sweeper
	// owns this responsibility in a multi-replica deployment).
	HeartbeatSweeperEnabled bool
	// HeartbeatSweepInterval is the sweep tick rate. Default 60s.
	// Env: LOOMCYCLE_HEARTBEAT_SWEEP_INTERVAL_MS.
	HeartbeatSweepInterval time.Duration
	// HeartbeatStaleAfter is the cutoff: runs with last_heartbeat_at
	// (or started_at, when no heartbeat ever fired) older than this
	// are swept. Default 10 minutes. Should be ≥ 2× the loop's
	// expected per-iteration time so live runs in long tool calls
	// aren't sweeped. Env: LOOMCYCLE_HEARTBEAT_STALE_MS.
	HeartbeatStaleAfter time.Duration
	// ReplicasSweepInterval is the dead-replica reaper's tick rate.
	// Default 60s. Tunable mostly for tests / crash-recovery load
	// experiments — leave at default in production.
	// Env: LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS.
	ReplicasSweepInterval time.Duration
	// ReplicasStaleAfter is the cutoff for marking a replica's heartbeat
	// stale and reaping its in-flight runs (status='failed',
	// stop_reason='owner_replica_dead') + reclaiming its quota.
	// Default 90s — should be > the replica heartbeat interval (30s) by
	// enough margin to absorb a missed beat. Crash-recovery load tests
	// drive it down to ~15s so the reap fires in-window.
	// Env: LOOMCYCLE_REPLICAS_STALE_AFTER_MS.
	ReplicasStaleAfter time.Duration
	// SessionLockGCInterval is how often the v0.5.0 session-lock map
	// GC runs. Default 5 minutes. Env:
	// LOOMCYCLE_SESSION_LOCK_GC_INTERVAL_MS.
	SessionLockGCInterval time.Duration
	// SessionLockMaxIdle is the cutoff for the GC: a session-lock
	// entry whose refcount is 0 AND lastAccessed is older than this
	// is reclaimed. Default 10 minutes. Env:
	// LOOMCYCLE_SESSION_LOCK_MAX_IDLE_MS.
	SessionLockMaxIdle time.Duration

	// GrpcAddr is the gRPC listener address (e.g. ":8788" or
	// "127.0.0.1:8788"). Empty disables the gRPC surface; HTTP+SSE
	// always listens on ListenAddr regardless. Both surfaces share
	// the same Store / cancel registry / config — operators can
	// run with one, the other, or both. Env: LOOMCYCLE_GRPC_ADDR.
	GrpcAddr string

	// ResolveProbeInterval is the cadence at which the resolver
	// re-probes each provider's /v1/models endpoint (or /api/tags
	// for Ollama) to refresh the availability matrix. Default 15
	// minutes; clamped to a 1-hour ceiling so a misconfigured
	// long interval can't hide a recovered provider for a full
	// day. Env: LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS.
	ResolveProbeInterval time.Duration

	// FallbackPinAfterSuccess, when true, suppresses provider
	// fallback on retryable errors AFTER the run has completed at
	// least one successful turn (assistant message appended to
	// the conversation history). The initial turn can still fall
	// back — so a stale-probe initial pick survives — but once a
	// provider has touched the transcript, the run sticks with
	// it. Same-provider rate-limit retry (internal/providers/
	// ratelimit/) still covers transient errors.
	//
	// Why: cross-provider mid-conversation fallback exposes a
	// growing surface of provider-specific transcript translation
	// bugs (the 2026-05-13 DeepSeek "reasoning_content must be
	// passed back" 400 was one instance; thoughtSignature, tool_call
	// shape, etc. are others). Pinning closes the class of bug.
	//
	// Default OFF in v0.8.x; plan to flip default-on in v0.9.x
	// once production-validated. Env:
	// LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS.
	FallbackPinAfterSuccess bool

	// ---- RFC J synthetic code-js provider (opt-in) ----

	// CodeAgentsEnabled gates registration of the synthetic code-js
	// provider (RFC J). Default OFF (operator opts in via
	// LOOMCYCLE_CODE_AGENTS_ENABLED=1). When false the provider is not
	// registered at all, so an AgentDef with `provider: code-js` fails
	// loud at startup with a clear "code agents are disabled" error
	// rather than silently. Operator-provided JS runs in the operator's
	// own trust posture (same as the Bash tool) — hence opt-in.
	CodeAgentsEnabled bool
	// CodeAgentsRoot is the filesystem root holding
	// agent_code/<name>/index.js. Default ./agent_code. Env:
	// LOOMCYCLE_CODE_AGENTS_ROOT.
	CodeAgentsRoot string
	// CodeAgentsDeterministic seeds Date.now()/Math.random() for
	// reproducible runs (Decision 13). Default OFF. Env:
	// LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1.
	CodeAgentsDeterministic bool
	// CodeAgentsRunTimeout bounds a code-agent's wall-clock as a ctx
	// deadline (the universal cancel path — Appendix A; Interrupt cannot
	// break a parked tool call). Default 120s. Env:
	// LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS.
	//
	// This is TOTAL wall-clock from the run's start, and it KEEPS TICKING
	// while the orchestrator is blocked in Agent.parallel_spawn awaiting its
	// children — each child is a full LLM run (often 60–180s), so a fan-out
	// orchestrator's budget must envelope the whole batch, not one child. The
	// CPU-oriented 120s default is structurally too low for one: raise it
	// per-orchestrator-agent via AgentDef.RunTimeoutSeconds (yaml
	// run_timeout_seconds) or per-call via the /v1/runs run_timeout_seconds
	// field rather than bumping this global for every code agent. Exceeding
	// the budget surfaces as code_agent_timeout (not a throw at a JS line).
	CodeAgentsRunTimeout time.Duration

	// ---- v0.8.x process-resource metrics sampler (opt-in) ----

	// MetricsEnabled enables the periodic process_samples
	// recorder. Default OFF in v0.8.x (operator opts in via
	// LOOMCYCLE_METRICS_ENABLED=1). Flip to default-on in v0.9.x
	// once production-validated. When false, the sampler goroutine
	// is not started at all — zero runtime overhead.
	MetricsEnabled bool
	// MetricsSampleInterval is the tick rate. Default 5s.
	// Values below 1s are rejected at config-load (preventing
	// accidental write-storms). Env:
	// LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS.
	MetricsSampleInterval time.Duration
	// MetricsRetentionDays is how many days of process_samples
	// rows the periodic sweeper keeps. Default 7. Cleared rows
	// are gone. 0 means "no automatic cleanup" (operator must
	// prune manually, or the table grows unbounded). Env:
	// LOOMCYCLE_METRICS_RETENTION_DAYS.
	MetricsRetentionDays int
	// MetricsCollectSystem enables /proc/stat + /proc/meminfo
	// reads for system-wide CPU% + memory usage in addition to
	// loomcycle's own RSS. Linux only; silently ignored on other
	// platforms. Env: LOOMCYCLE_METRICS_COLLECT_SYSTEM.
	MetricsCollectSystem bool
	// MetricsSweepInterval is the sweeper cadence for
	// process_samples. Default 15 minutes. 0 disables the
	// sweeper (combine with MetricsRetentionDays=0 if you
	// want unbounded retention). Env:
	// LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS.
	MetricsSweepInterval time.Duration

	// ---- v0.12.0 multi-replica HA (opt-in) ----

	// ReplicaID activates cluster mode. When unset, loomcycle runs
	// in single-replica mode exactly like v0.11.x — no backplane, no
	// replicas table writes, no /healthz cluster-view fields. When
	// set, the operator must use the Postgres store (SQLite refuses
	// to start). Validated against [A-Za-z0-9][A-Za-z0-9_-]{0,63} at
	// config-load; UUID4 is the recommended default but short labels
	// ("replica-a", "lc-1") are accepted for human-friendly cluster
	// admin. Env: LOOMCYCLE_REPLICA_ID.
	ReplicaID string

	// CancelAckTimeoutMs is the v0.12.2 cross-replica cancel ack wait.
	// On a cluster-mode cancel that broadcasts to the owning replica,
	// the originator waits this long for a "cancelled" ack on the
	// backplane before returning {cancelled:false,
	// reason:"owner_replica_unreachable"}. Default 5000.
	// Env: LOOMCYCLE_CANCEL_ACK_TIMEOUT_MS.
	CancelAckTimeoutMs int64

	// PauseDefaultTimeoutMs is the wait-for-non-idempotent-tools cap
	// applied when POST /v1/_pause omits timeout_ms. 0 ⇒ use the
	// internal default (pause.DefaultPauseTimeout = 30s). Capped at
	// pause.MaxPauseTimeout (5 min) regardless of operator value to
	// avoid an operator typo (300000 vs 30000) leaving the runtime
	// in StatePausing for an extended period. Env:
	// LOOMCYCLE_PAUSE_DEFAULT_TIMEOUT_MS.
	PauseDefaultTimeoutMs int64

	// PauseCacheTTLMs is the v0.12.3 Phase 4 TTL for the cluster-
	// mode DB-backed pause-state cache in pause.Manager.State().
	// Default 1000 (1s). Lower values reduce the maximum latency
	// between a pause event and a remote replica seeing the state
	// change; higher values reduce DB load. Only effective when
	// LOOMCYCLE_REPLICA_ID is set. Env:
	// LOOMCYCLE_PAUSE_CACHE_TTL_MS.
	PauseCacheTTLMs int64

	// MCPAllowPrivilegedTools — v0.8.15. When true, dynamically-
	// registered agents may include Bash/Write/Edit in their
	// allowed_tools. Default false: those three are stripped from
	// any register_agent request, matching the v0.8.7/v0.8.8
	// default-deny pattern for new tool surfaces. Env:
	// LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS.
	MCPAllowPrivilegedTools bool

	// MCPAllowDynamicStdio — F31. When true, the MCPServerDef substrate
	// (POST /v1/_mcpserverdef, the `mcpserverdef` tool) may register a
	// `transport: stdio` server at runtime. Default false: dynamic
	// registration is http/streamable-http only, because a stdio server
	// runs an ARBITRARY LOCAL COMMAND (a second local-exec path alongside
	// Bash, with no outbound-host-allowlist mediation). Like BashEnabled,
	// this is operator-gated and off by default; static yaml `mcp_servers:`
	// stdio entries are unaffected (operator-authored = trusted). Env:
	// LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO.
	MCPAllowDynamicStdio bool

	// RedactSecrets — F32. When true (the DEFAULT), tool I/O is scanned for
	// secret-shaped substrings and masked BEFORE it is persisted to the
	// events.payload BLOB (and thus to snapshots + the /v1/_events audit API).
	// This is defense-in-depth for the runtime-inline leak: an agent that
	// inlines a token on a Bash cmdline (`curl -H "Authorization: token …"`) or
	// a tool result that echoes one would otherwise be stored in cleartext at
	// rest. The live SSE stream is NOT redacted (the caller already holds the
	// secret). Opt out with LOOMCYCLE_REDACT_SECRETS=0. See internal/redact.
	RedactSecrets bool

	// DynamicAgentDefaultTTLSeconds — v0.8.15. TTL applied to
	// dynamic agents registered via mcp__loomcycle__register_agent
	// when the caller omits ttl_seconds. Default 86400 (24h).
	// Set to 0 to deny all default-TTL registrations (callers
	// MUST supply an explicit ttl_seconds). Env:
	// LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS.
	DynamicAgentDefaultTTLSeconds int

	// DynamicAgentSweepInterval — v0.8.15. Cadence for the
	// dynamic_agents TTL sweeper. Default 15 minutes. 0 disables
	// the sweeper (expired rows linger; DynamicAgentGet still
	// filters them out at read time so functional correctness is
	// preserved, but the table grows unbounded). Env:
	// LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS.
	DynamicAgentSweepInterval time.Duration

	// EphemeralVolumeSweepInterval — RFC AH Phase 2b. Cadence for the
	// crash-recovery sweep of ephemeral (run-tree-scoped) volumes whose
	// owning run is terminal-and-not-paused: a fenced os.RemoveAll of the
	// <dynamic_root>/_ephemeral/<root_run_id>/ subtree + its rows. The
	// inline run-completion purge is the primary path; this backstops a
	// crashed host (the inline purge never ran). Default 60s. 0 disables
	// (the inline purge still runs; only crash cleanup lapses). Cluster-gated
	// by coord.LockKeyEphemeralVolumeSweeper. Env:
	// LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS.
	EphemeralVolumeSweepInterval time.Duration

	// ToolParallelism caps how many tool_calls from a single
	// assistant turn run concurrently. Default 8. Set to 1 to
	// force serial dispatch (debug / determinism). Field 0
	// (unset) is treated as the default — the loop fills in 8.
	//
	// Bumping this matters most for agents that fan out via the
	// `Agent` built-in tool: each Agent call spawns a sub-agent
	// run, so a parent that emitted 3 Agent tool_calls would
	// pre-2026-05-09 see them serialised back-to-back instead of
	// running concurrently. The HTTP server's MAX_CONCURRENT_RUNS
	// slot still bounds the run tree, so per-tool parallelism is
	// an inner-loop knob that doesn't change the global ceiling.
	//
	// Env: LOOMCYCLE_TOOL_PARALLELISM.
	ToolParallelism int

	// SSEKeepaliveInterval is the cadence at which the SSE writer
	// emits comment-only frames (`:keepalive`) on long-lived agent
	// streams. Required-ignored by SSE clients per WHATWG so they
	// don't surface as events; the point is to keep the underlying
	// TCP/HTTP path from going idle. Agent runs that fan out to
	// sub-agents can sit minutes between real events while a child
	// is mid-WebFetch — networks with idle-connection timeouts
	// (Tailscale, NAT routers, some reverse proxies) drop silent
	// streams and the consumer-side undici reports the drop as
	// `TypeError: terminated` with no diagnostic context.
	//
	// Default 20 s — comfortably under the typical 30-120 s idle
	// timeouts on the affected network paths. Set to 0 to disable.
	// Env: LOOMCYCLE_SSE_KEEPALIVE_MS.
	SSEKeepaliveInterval time.Duration

	// MemoryMaxValueBytes caps a single Memory.set / Memory.incr
	// payload. Default 65536 (64 KB) — generous for a JSON document
	// agents would actually persist; refuses obvious abuse like
	// "stash an entire transcript here." Set to 0 to disable.
	// Env: LOOMCYCLE_MEMORY_MAX_VALUE_BYTES.
	MemoryMaxValueBytes int

	// MemoryMaxScopeBytes is the default per-(scope, scope_id) byte
	// cap. Per-agent yaml `memory_quota_bytes` overrides this.
	// Default 1048576 (1 MB). Set to 0 to disable.
	// Env: LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES.
	MemoryMaxScopeBytes int

	// MemorySweepInterval is how often the TTL reaper goroutine
	// runs MemorySweep on the store. Default 15 minutes. Set to 0
	// to disable (operators with an external reaper, or tests that
	// don't want background work, can opt out).
	// Env: LOOMCYCLE_MEMORY_SWEEP_MS.
	MemorySweepInterval time.Duration

	// PgvectorEnabled opts in to v0.9.0 Vector Memory on the
	// Postgres backend. When true, Open() probes the `vector`
	// extension and refuses to start if it's not loaded; Memory's
	// `search` op + `embed: true` field become available. When
	// false (default), vector ops refuse with ErrVectorUnsupported.
	// SQLite is unaffected — sqlite-vec ships in v0.9.1.
	// Env: LOOMCYCLE_PGVECTOR_ENABLED.
	PgvectorEnabled bool

	// SqliteVecPath is the path to the sqlite-vec shared library.
	// Reserved for v0.9.1 (the build-tag swap to cgosqlite); parsed
	// in v0.9.0 so operator yaml/env doesn't need a v0.9.0→v0.9.1
	// migration. Currently unused — SQLite vector ops always refuse
	// in v0.9.0.
	// Env: LOOMCYCLE_SQLITE_VEC_PATH.
	SqliteVecPath string

	// MemoryEmbedBatchSize is the default batch size embedder
	// drivers use when grouping texts into one provider call.
	// Provider-specific caps (OpenAI's 2048-item limit etc.) still
	// apply on top. Default 100. 0 disables batching (one call per
	// text — useful for debugging cost surprises).
	// Env: LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE.
	MemoryEmbedBatchSize int

	// MemoryEmbedTimeoutMs caps a single embedder HTTP call. Default
	// 30000 (30 s). 0 disables (rely on outer context). Negative
	// treated as 0 (matches MemoryMaxValueBytes convention).
	// Env: LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS.
	MemoryEmbedTimeoutMs int

	// ChannelsMaxValueBytes caps a single Channel.publish payload
	// (v0.8.4). Default 65536 (64 KB) — mirrors MemoryMaxValueBytes.
	// 0 disables. Env: LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES.
	ChannelsMaxValueBytes int

	// ChannelsSweepInterval is the TTL reaper cadence for the
	// channel_messages table (v0.8.4). Default 15 minutes — same as
	// MemorySweepInterval. 0 disables. Read paths filter expired
	// rows regardless of whether the sweeper has run, so this is
	// purely about keeping the table bounded.
	// Env: LOOMCYCLE_CHANNELS_SWEEP_MS.
	ChannelsSweepInterval time.Duration

	// ChannelsLongPollCapMS caps the wait_ms an agent may request
	// on a Channel.subscribe call (v0.8.4). Default 30000 (30 s) —
	// long enough for "wake me when there's new work" semantics,
	// short enough that a hung subscribe doesn't leak goroutines
	// on agent crash. 0 disables long-poll entirely.
	// Env: LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS.
	ChannelsLongPollCapMS int

	// ChannelsMaxPendingDeferred caps the v0.8.6 deferred-publish
	// scheduler's live timer count. Excess publishes still land in
	// storage; the scheduler silently skips the in-process Bus
	// notification (subscribers see deferred messages on their next
	// long-poll wake instead). Default 10000. 0 disables the cap
	// (unbounded timers).
	// Env: LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED.
	ChannelsMaxPendingDeferred int

	// AgentDefMaxDefinitionBytes caps a single AgentDef.create or
	// AgentDef.fork's serialised definition JSON (v0.8.5). Default
	// 131072 (128 KB). 0 disables. Mirrors MemoryMaxValueBytes's
	// negative-as-disable convention.
	// Env: LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES.
	AgentDefMaxDefinitionBytes int

	// AgentDefMaxDescriptionBytes caps the free-text description
	// field on AgentDef.create / fork (v0.8.5). Default 8192 (8 KB).
	// 0 disables.
	// Env: LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES.
	AgentDefMaxDescriptionBytes int

	// AgentDefMaxCodeBytes caps an inline code-js `code_body` overlay on
	// AgentDef.create / fork (RFC J). A dedicated cap (vs the whole-
	// definition AgentDefMaxDefinitionBytes) gives a clearer error and a
	// tighter default for executable source. Default 262144 (256 KB).
	// 0 disables.
	// Env: LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES.
	AgentDefMaxCodeBytes int

	// SkillDefMaxBodyBytes caps the overlay.body field on
	// SkillDef.create / fork (v0.8.22). Default 131072 (128 KB).
	// 0 disables.
	// Env: LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES.
	SkillDefMaxBodyBytes int

	// SkillDefMaxDescriptionBytes caps the free-text description
	// field on SkillDef.create / fork (v0.8.22). Default 8192 (8 KB).
	// 0 disables.
	// Env: LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES.
	SkillDefMaxDescriptionBytes int

	// EvaluationMaxJudgementBytes caps the structured-judgement JSON
	// on Evaluation.submit (v0.8.5). Default 32768 (32 KB). 0 disables.
	// Env: LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES.
	EvaluationMaxJudgementBytes int

	// EvaluationMaxRationaleBytes caps the natural-language rationale
	// text on Evaluation.submit (v0.8.5). Default 8192 (8 KB).
	// 0 disables.
	// Env: LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES.
	EvaluationMaxRationaleBytes int

	// ProviderHeaderTimeout is the per-attempt cap on time-to-first-
	// byte for streaming provider HTTP calls (set on each driver's
	// http.Transport.ResponseHeaderTimeout). Default 60s — generous
	// enough for cold-start cloud endpoints and warming Ollama models
	// without leaving a stalled pre-stream connection open forever.
	// Env: LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS.
	ProviderHeaderTimeout time.Duration

	// ProviderIdleTimeout is the maximum gap allowed between body
	// bytes during a streaming provider response. The driver wraps
	// resp.Body with streamhttp.WrapBody and resets a timer on every
	// Read; if the timer fires (no bytes for this long), the request
	// context is cancelled. Default 90s — long enough to ride out
	// reasoning-model thinking pauses, short enough to drop a truly
	// stalled stream before the agent's heartbeat sweeper notices.
	//
	// Why this exists: the previous implementation used
	// http.Client.Timeout = 5min as a wall-clock cap on the entire
	// request. For long final-turn responses (e.g. job-searcher
	// emitting a 25-position ingest payload) the cap fired mid-stream
	// even when the model was actively producing tokens. The
	// header-timeout + per-byte idle pair lets long *productive*
	// streams complete while still killing genuinely stalled ones.
	// Env: LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS.
	ProviderIdleTimeout time.Duration

	// OllamaLocalHeaderTimeout / OllamaLocalIdleTimeout override the
	// ProviderHeaderTimeout / ProviderIdleTimeout pair for the
	// "ollama-local" registration ONLY. Local generation is inherently
	// slow on first-token — a cold model load from disk plus prompt
	// evaluation over a large num_ctx can take minutes on a consumer
	// GPU/CPU, blowing past the 60s/90s cloud-shaped defaults and
	// surfacing as a spurious header/idle timeout. These default to a
	// generous 300s each so a slow local model isn't cut off, while the
	// cloud providers keep the tighter fast-fail defaults. Hosted
	// ollama.com (the "ollama" registration) uses the global
	// Provider*Timeout, like every other cloud driver.
	// Env: LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS /
	// LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS.
	OllamaLocalHeaderTimeout time.Duration
	OllamaLocalIdleTimeout   time.Duration

	// v0.10.0 OpenTelemetry tracing — default OFF. Setting
	// OTELExporterEndpoint to a non-empty value installs an OTLP/HTTP
	// exporter; loomcycle emits run/iteration/provider.call/tool.call
	// spans for every agent run. When the endpoint is empty, the global
	// tracer is a no-op and there is zero runtime cost. See
	// `internal/help/topics/observability.md` for the Jaeger / Tempo /
	// Honeycomb walkthroughs.

	// OTELExporterEndpoint is the OTLP/HTTP endpoint (no path — the
	// otlptracehttp exporter appends `/v1/traces`). Empty = OTEL
	// disabled. Examples: `http://localhost:4318` (local Jaeger,
	// `docker run jaegertracing/all-in-one:latest`),
	// `https://api.honeycomb.io` (Honeycomb cloud — pair with the
	// `x-honeycomb-team` header). Env: LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT.
	OTELExporterEndpoint string

	// OTELExporterHeaders is the comma-separated key=value list
	// appended to every OTLP/HTTP request. Used for collector auth
	// (e.g. `x-honeycomb-team=$KEY` or
	// `authorization=Bearer $TOKEN`). Empty = no headers. Whitespace
	// around `=` and `,` is trimmed.
	// Env: LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS.
	OTELExporterHeaders map[string]string

	// OTELServiceName populates the `service.name` resource attribute
	// every span carries. Default `"loomcycle"`. Override per replica
	// when running multi-replica HA so Jaeger groups traces by
	// instance.
	// Env: LOOMCYCLE_OTEL_SERVICE_NAME.
	OTELServiceName string

	// OTELTracesSamplerRatio is the head-based sampling ratio applied
	// before spans are exported. 1.0 = every span; 0.1 = ~10%. Always
	// respects parent decisions (a sampled parent's children are
	// always sampled). Default 1.0; reduce in production when storage
	// matters. Env: LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO.
	OTELTracesSamplerRatio float64

	// SchedulerEnabled enables the v1.x RFC E scheduler runtime
	// (sweeper goroutine + due-row firing). Default OFF — operator
	// opts in via LOOMCYCLE_SCHEDULER_ENABLED=1. When false, the
	// sweeper goroutine is not started and substrate-stored
	// schedules sit idle (the ScheduleDef tool still works for
	// authoring + listing, just nothing fires).
	SchedulerEnabled bool

	// SchedulerTickSeconds is the sweeper poll cadence. Default 30.
	// Lower values trade DB load for tighter punctuality. Env:
	// LOOMCYCLE_SCHEDULER_TICK_SECONDS.
	SchedulerTickSeconds int

	// SchedulerFireTimeoutSeconds is the per-fire run timeout cap.
	// Default 600 (10 minutes). The runner's ctx-cancellation
	// cascades into provider + tool calls so timeout cleanly
	// aborts. Env: LOOMCYCLE_SCHEDULER_FIRE_TIMEOUT_SECONDS.
	SchedulerFireTimeoutSeconds int

	// SchedulerEnvAllowlist is the comma-separated env-var name
	// allowlist for `user_credentials_from_env` resolution. Empty
	// (default) disables env-credential lookup entirely — a
	// safe-by-default posture. Operators opt in by setting
	// LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST="VAR1,VAR2" — only those
	// var names will be readable by scheduled runs.
	//
	// This same allowlist gates the v1.x RFC H webhook receiver's
	// signing-secret + bearer-token + user_credentials_from_env reads
	// (RFC F shared trigger-credential gate). The webhook receiver
	// reuses it rather than a parallel list so an operator declares
	// the env-var floor once for all autonomous-run triggers. Webhook
	// operators may also use the better-named LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST
	// (merged with this one at receiver construction); and a webhook's own
	// statically-declared secret + a LOOMCYCLE_*-named verify secret resolve
	// without any allowlist entry. See internal/api/webhook.BuildEnvAllowlist.
	SchedulerEnvAllowlist []string

	// WebhooksEnabled enables the v1.x RFC H inbound-webhook receiver
	// (the POST /v1/_webhooks/{name} mount). Default OFF — operator
	// opts in via LOOMCYCLE_WEBHOOKS_ENABLED=1. When false, no route
	// is mounted and webhook_defs sit idle (the WebhookDef tool still
	// works for authoring + listing, just nothing receives). Mirrors
	// SchedulerEnabled exactly.
	WebhooksEnabled bool

	// WebhooksEnvAllowlist is the webhook-specific, correctly-named twin of
	// SchedulerEnvAllowlist (LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST="VAR1,VAR2").
	// Webhook operators kept reaching for a LOOMCYCLE_*ENV_ALLOWLIST and
	// missing the scheduler-named knob; this names the gate after the
	// subsystem. Merged with SchedulerEnvAllowlist at receiver construction
	// (union, not replacement) — declaring either authorizes the name.
	WebhooksEnvAllowlist []string

	// WebhooksAllowUnauthenticated opts into the trusted-network ingress
	// posture: a webhook with auth.kind="none" skips signature verification
	// entirely. Default OFF — a none-auth webhook 503s
	// "unauthenticated_mode_disabled" until the operator sets
	// LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED=1. For deployments where the
	// receiver is only reachable over an already-authenticated transport
	// (WireGuard/tailnet, mTLS mesh) and HMAC is redundant.
	WebhooksAllowUnauthenticated bool

	// A2AServerEnabled enables the v1.x RFC G A2A server HTTP surface
	// (the well-known AgentCard URI + the three protocol-binding mounts
	// on the existing mux). Default OFF — operator opts in via
	// LOOMCYCLE_A2A_ENABLED=1. When false, the A2AServerCardDef tool
	// still works for authoring; nothing is mounted and no card is
	// served.
	A2AServerEnabled bool

	// A2AServerCardName names the active A2AServerCardDef whose card the
	// server serves and whose exposed_agents drive skill→agent routing.
	// Env: LOOMCYCLE_A2A_SERVER_CARD. Required when A2AServerEnabled.
	A2AServerCardName string

	// A2ATenancyRouting selects how the inbound tenant is derived for
	// the A2A surface: "" / "none" (single-tenant, served at host root),
	// "host" (tenant-{id}.<host> subdomain), or "path"
	// (/{tenant}/.well-known/... + /{tenant}/a2a/*). The tenant is a
	// TRUST BOUNDARY: it comes ONLY from the routed host/path, never
	// from a request body field. Env: LOOMCYCLE_A2A_TENANCY_ROUTING.
	A2ATenancyRouting string

	// A2APublicBaseURL is the externally-reachable base URL advertised
	// in the AgentCard's interface URLs (e.g. https://agents.example.com).
	// Empty falls back to a relative path, which is valid for same-origin
	// clients but not for cross-host discovery. Env:
	// LOOMCYCLE_A2A_PUBLIC_BASE_URL.
	A2APublicBaseURL string
}

// Load reads, env-expands, merges, and validates one or more config FILES.
// With a single path it is byte-identical to the historical single-file load.
// With multiple paths (RFC AN config layering) the files are deep-merged at the
// YAML-tree level left→right, last-layer-wins (so the operator's authoritative
// file goes LAST); every replaced leaf is reported (a startup Warning, or a fatal
// load error under LOOMCYCLE_CONFIG_STRICT=1). An empty/"" path is ignored
// (the MDs-only / no-yaml deployment). It is a thin wrapper over LoadLayers —
// the embedded-preset path (RFC AQ) calls LoadLayers directly with in-memory
// layers prepended as the base.
func Load(paths ...string) (*Config, error) {
	layers := make([]Layer, len(paths))
	for i, p := range paths {
		layers[i] = Layer{Name: p}
	}
	return LoadLayers(layers...)
}

// LoadLayers is Load generalised to in-memory layers (RFC AQ embedded presets +
// bundles). A Layer is either a disk file (Data == nil → read Name) or
// pre-resolved bytes (Data != nil — an embedded preset/bundle). Layers deep-merge
// left→right, last wins, so embedded presets go FIRST (the base) and the
// operator's authoritative file LAST. A single disk-file layer takes the
// historical byte-identical fast path; everything else goes through the RFC AN
// merge. Downstream — AGENTS_ROOT discovery, system_prompt_file resolution, the
// env block, validate() — runs once over the assembled whole, unchanged.
func LoadLayers(layers ...Layer) (*Config, error) {
	cfg := &Config{
		Concurrency: Concurrency{
			MaxConcurrentRuns: 8,
			MaxQueueDepth:     16,
			QueueTimeoutMS:    30000,
		},
		Cache: CacheConfig{
			ResponseKV: ResponseKVConfig{Backend: "memory", TTL: "5m"},
			Native:     NativeCacheConfig{Enabled: true},
		},
	}

	// Drop the no-yaml sentinel (a "" path / empty in-memory layer = the
	// MDs-only path).
	src := make([]Layer, 0, len(layers))
	for _, l := range layers {
		if l.Data == nil && l.Name == "" {
			continue
		}
		src = append(src, l)
	}
	// primaryPath is the LAST disk-file layer — used for configDir + relative
	// system_prompt_file resolution. Embedded layers (Data != nil) have no
	// directory and never set it; a presets-only stack leaves it "" (relative
	// prompts then resolve against cwd, matching the no-yaml path). A relative
	// system_prompt_file in an operator layer resolves against the LAST file's
	// directory; bundles inline the prompt (RFC AN P1 caveat).
	primaryPath := ""
	for _, l := range src {
		if l.Data == nil {
			primaryPath = l.Name
		}
	}

	switch {
	case len(src) == 1 && src[0].Data == nil:
		// Single disk file — the historical path, byte-identical (no merge round-trip).
		raw, err := os.ReadFile(src[0].Name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", src[0].Name, err)
		}
		expanded := expandEnv(string(raw))
		if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", src[0].Name, err)
		}
		if abs, err := filepath.Abs(filepath.Dir(src[0].Name)); err == nil {
			cfg.configDir = abs
		}
	case len(src) >= 1:
		// RFC AN/AQ: deep-merge the YAML trees (≥2 layers, or a single in-memory
		// preset/bundle), then unmarshal the merged whole.
		merged, overrides, err := mergeLayers(src)
		if err != nil {
			return nil, err
		}
		if len(overrides) > 0 {
			if os.Getenv("LOOMCYCLE_CONFIG_STRICT") == "1" {
				return nil, fmt.Errorf("config: %d cross-layer override(s) with LOOMCYCLE_CONFIG_STRICT=1: %s", len(overrides), strings.Join(overrides, "; "))
			}
			for _, o := range overrides {
				cfg.Warnings = append(cfg.Warnings, "config layer override: "+o)
			}
		}
		out, err := yaml.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("re-marshal merged config: %w", err)
		}
		if err := yaml.Unmarshal(out, cfg); err != nil {
			return nil, fmt.Errorf("parse merged config: %w", err)
		}
		if primaryPath != "" {
			if abs, err := filepath.Abs(filepath.Dir(primaryPath)); err == nil {
				cfg.configDir = abs
			}
		}
	}
	// Discover MD-defined agents BEFORE resolveSystemPromptFiles so
	// the file-resolution pass sees a merged map. We read the env var
	// inline here because the full Env struct is populated later in
	// this function — re-shuffling that order would risk subtler
	// regressions for one early reader. The book-keeping copy onto
	// cfg.Env.AgentsRoot lands in the env-block below.
	//
	// Discovery runs OUTSIDE the `if path != ""` guard so the
	// MDs-as-sole-source-of-truth deployment works (operator sets
	// LOOMCYCLE_AGENTS_ROOT and omits the yaml entirely; cfg.Agents
	// is populated purely from MDs).
	if root := os.Getenv("LOOMCYCLE_AGENTS_ROOT"); root != "" {
		if err := discoverAgents(cfg, root); err != nil {
			return nil, err
		}
	}
	// Resolve any agent's system_prompt_file → system_prompt (for both
	// yaml-declared and discovered agents that set the field). Also
	// outside the path-guard for the MDs-only path. With path == "",
	// configDir is empty → relative paths resolve against cwd; absolute
	// paths still work; this matches the documented semantic ("relative
	// paths resolve against the YAML config file's directory" reduces
	// to "the process's cwd" when there is no YAML).
	if err := resolveSystemPromptFiles(cfg, primaryPath); err != nil {
		return nil, err
	}

	cfg.Env = Env{
		AnthropicAPIKey:           os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIAPIKey:              os.Getenv("OPENAI_API_KEY"),
		VoyageAPIKey:              os.Getenv("VOYAGE_API_KEY"),
		OllamaBaseURL:             getenvDefault("OLLAMA_BASE_URL", "http://localhost:11434"),
		OllamaAPIKey:              os.Getenv("OLLAMA_API_KEY"),
		OllamaCloudBaseURL:        getenvDefault("OLLAMA_CLOUD_BASE_URL", "https://ollama.com"),
		DeepSeekAPIKey:            os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekBaseURL:           os.Getenv("DEEPSEEK_BASE_URL"),
		GeminiAPIKey:              os.Getenv("GEMINI_API_KEY"),
		GeminiBaseURL:             os.Getenv("GEMINI_BASE_URL"),
		ListenAddr:                getenvDefault("LOOMCYCLE_LISTEN_ADDR", "127.0.0.1:8787"),
		PublicURL:                 strings.TrimRight(strings.TrimSpace(os.Getenv("LOOMCYCLE_PUBLIC_URL")), "/"),
		AuthToken:                 os.Getenv("LOOMCYCLE_AUTH_TOKEN"),
		OperatorTokenPepper:       os.Getenv("LOOMCYCLE_OPERATOR_TOKEN_PEPPER"),
		AuditLogPath:              os.Getenv("LOOMCYCLE_AUDIT_LOG_PATH"),
		AuthVerbose:               os.Getenv("LOOMCYCLE_AUTH_VERBOSE") == "1",
		DataDir:                   getenvDefault("LOOMCYCLE_DATA_DIR", "./data"),
		HTTPHostAllowlist:         splitCSV(os.Getenv("LOOMCYCLE_HTTP_HOST_ALLOWLIST")),
		HTTPPrivateHostAllowlist:  splitCSV(os.Getenv("LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST")),
		MCPAllowPrivateIPs:        getenvBool("LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS", true),
		HTTPCallerAuthoritative:   os.Getenv("LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE") == "1",
		ResumeFanout:              os.Getenv("LOOMCYCLE_RESUME_FANOUT") == "1",
		BraveAPIKey:               os.Getenv("BRAVE_API_KEY"),
		BashEnabled:               os.Getenv("LOOMCYCLE_BASH_ENABLED") == "1",
		BashboxEnabled:            os.Getenv("LOOMCYCLE_BASHBOX_ENABLED") == "1",
		BashboxFallbackCommands:   splitCSV(os.Getenv("LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS")),
		BashboxFallbackAllowedEnv: splitCSV(os.Getenv("LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV")),
		SkillsRoot:                os.Getenv("LOOMCYCLE_SKILLS_ROOT"),
		AgentsRoot:                os.Getenv("LOOMCYCLE_AGENTS_ROOT"),
		HelpRoot:                  os.Getenv("LOOMCYCLE_HELP_ROOT"),
		// Sweeper / GC defaults — populated above zero only if the
		// env var below was set. The fallbacks are applied in
		// cmd/loomcycle/main.go where the goroutines are started.
		HeartbeatSweeperEnabled: true,
	}

	// RFC AH Phase 3 — the legacy filesystem jail is retired. The three env
	// vars (LOOMCYCLE_READ_ROOT / WRITE_ROOT / BASH_CWD) no longer exist.
	// Fail fast with a migration hint when a stale deploy still sets one,
	// rather than silently denying every file op (sandbox-by-default).
	if err := checkRetiredJailEnv(); err != nil {
		return nil, err
	}

	// Env-overrides for the storage block. Env wins over YAML so prod
	// deploys can keep PG_DSN out of version-controlled config files.
	// Empty env values fall through to whatever YAML provided.
	if v := os.Getenv("LOOMCYCLE_STORAGE_BACKEND"); v != "" {
		cfg.Storage.Backend = v
	}
	if v := os.Getenv("LOOMCYCLE_PG_DSN"); v != "" {
		cfg.Storage.PgDSN = v
	}
	if v := os.Getenv("LOOMCYCLE_PG_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Storage.PgMaxOpenConns = int32(n)
		}
	}
	if v := os.Getenv("LOOMCYCLE_PG_MIN_IDLE_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.PgMinIdleConns = int32(n)
		}
	}
	if v := os.Getenv("LOOMCYCLE_PG_AUTOMIGRATE"); v == "1" {
		cfg.Storage.PgAutoMigrate = true
	}
	// Default backend is sqlite (back-compat with pre-Storage configs).
	if cfg.Storage.Backend == "" {
		cfg.Storage.Backend = "sqlite"
	}

	// RFC AA SQL Memory (Phase 1). Off by default; env overrides yaml.
	if v := os.Getenv("LOOMCYCLE_SQLMEM_ENABLED"); v == "1" {
		cfg.Storage.SqlMemEnabled = true
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_ROOT"); v != "" {
		cfg.Storage.SqlMemRoot = v
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_QUOTA_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemQuotaBytes = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemStatementTimeoutMS = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_MAX_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemMaxRows = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_AUDIT_MODE"); v != "" {
		cfg.Storage.SqlMemAuditMode = v
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_PG_DSN"); v != "" {
		cfg.Storage.SqlMemPgDSN = v
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemTxnTimeoutMS = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemMaxOpenTxns = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemMaxTxnDepth = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_SCOPE_TTL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemScopeTTLMS = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_GC_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.SqlMemGCIntervalMS = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_TOTAL_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.Storage.SqlMemTotalMaxBytes = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.Storage.SqlMemSnapshotMaxScopeBytes = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SQLMEM_SHARED_SCHEMAS"); v != "" {
		var names []string
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		cfg.Storage.SqlMemSharedSchemas = names
	}
	// Defaults for the bounds the operator did not set. quota stays 0 (off);
	// timeout/rows get sensible ceilings; audit defaults to full.
	if cfg.Storage.SqlMemStatementTimeoutMS == 0 {
		cfg.Storage.SqlMemStatementTimeoutMS = 30000
	}
	if cfg.Storage.SqlMemMaxRows == 0 {
		cfg.Storage.SqlMemMaxRows = 10000
	}
	if cfg.Storage.SqlMemAuditMode == "" {
		cfg.Storage.SqlMemAuditMode = "full"
	}
	// Explicit-transaction bounds (Phase 3a). A txn TTL ensures a held scope
	// connection is reclaimed if an agent abandons a transaction; the
	// MaxOpenTxns cap bounds total pinned connections. Both apply only when the
	// agent uses sql_begin. A negative env (unparseable) leaves the default.
	if cfg.Storage.SqlMemTxnTimeoutMS == 0 {
		cfg.Storage.SqlMemTxnTimeoutMS = 30000
	}
	if cfg.Storage.SqlMemMaxOpenTxns == 0 {
		cfg.Storage.SqlMemMaxOpenTxns = 64
	}
	// SAVEPOINT nesting depth cap (Phase 3b). Bounds the in-memory savepoint
	// stack per transaction; a nested sql_begin past it errors.
	if cfg.Storage.SqlMemMaxTxnDepth == 0 {
		cfg.Storage.SqlMemMaxTxnDepth = 16
	}

	// Hooks block (v0.8.17). The env-var override APPENDS to whatever
	// yaml already declared rather than replacing, so an operator can
	// keep their static list in yaml and add an ops-only entry via env
	// without rewriting the config file. Duplicates are tolerated — the
	// registry's IsHostWidenPermitted() does set membership.
	if v := os.Getenv("LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS"); v != "" {
		for _, owner := range splitCSV(v) {
			cfg.Hooks.PermitHostWiden.Owners = append(cfg.Hooks.PermitHostWiden.Owners, owner)
		}
	}

	// gRPC server (v0.5.5+). Disabled by default; operator opts in
	// by setting LOOMCYCLE_GRPC_ADDR. Coexists with HTTP+SSE (which
	// remains the default and is on a separate port).
	cfg.Env.GrpcAddr = os.Getenv("LOOMCYCLE_GRPC_ADDR")

	// Heartbeat sweeper + session-lock GC env. All optional; defaults
	// are sensible for a single-replica deployment.
	cfg.Env.HeartbeatSweeperEnabled = os.Getenv("LOOMCYCLE_HEARTBEAT_SWEEPER") != "0"
	if v := os.Getenv("LOOMCYCLE_HEARTBEAT_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.HeartbeatSweepInterval = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_HEARTBEAT_STALE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.HeartbeatStaleAfter = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ReplicasSweepInterval = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_REPLICAS_STALE_AFTER_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ReplicasStaleAfter = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_SESSION_LOCK_GC_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.SessionLockGCInterval = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_SESSION_LOCK_MAX_IDLE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.SessionLockMaxIdle = time.Duration(n) * time.Millisecond
		}
	}
	// LOOMCYCLE_TOOL_PARALLELISM overrides the per-iteration
	// tool_call concurrency cap (default 8). Floor 1, ceiling 64
	// — anything beyond 64 would spawn a goroutine storm that
	// outweighs realistic fan-out. Zero / negative values fall
	// through to the default.
	if v := os.Getenv("LOOMCYCLE_TOOL_PARALLELISM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 64 {
				n = 64
			}
			cfg.Env.ToolParallelism = n
		}
	}
	// LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX overrides the ollama-local
	// driver's options.num_ctx. Required when running prompts above
	// ~4k tokens against a local Ollama whose Modelfile doesn't pin
	// num_ctx — see the field doc on Env.OllamaLocalNumCtx.
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalNumCtx = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_OLLAMA_NUM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaNumCtx = n
		}
	}
	// LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU forces the number of model layers
	// Ollama offloads to the GPU on every ollama-local request — see the
	// field doc on Env.OllamaLocalNumGpu. 0/unset = omit (Ollama
	// auto-detects); a literal 0 must NOT be sent (it would force CPU).
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalNumGpu = n
		}
	}
	// LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS overrides the default 15-min
	// probe cadence. Clamped to a 1-hour ceiling so a typo or
	// misunderstanding can't stretch the recovery window beyond what
	// the design assumed. Sub-minute intervals are also clamped up to
	// 60s — anything tighter risks hammering provider /v1/models
	// endpoints for negligible recovery-time benefit.
	if v := os.Getenv("LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d := time.Duration(n) * time.Millisecond
			const minProbe = 60 * time.Second
			const maxProbe = 60 * time.Minute
			if d < minProbe {
				d = minProbe
			}
			if d > maxProbe {
				d = maxProbe
			}
			cfg.Env.ResolveProbeInterval = d
		}
	}
	// LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS: when set to "1",
	// suppress provider fallback after the first successful turn.
	// Opt-in for v0.8.x; default-on planned for v0.9.x.
	cfg.Env.FallbackPinAfterSuccess = os.Getenv("LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS") == "1"
	// LOOMCYCLE_SSE_KEEPALIVE_MS sets the SSE keepalive cadence.
	// Default 20 s; 0 (or any value ≤ 0) disables. Floor 1 s for
	// positive values so a misconfigured tiny value can't busy-loop
	// the writer; ceiling 5 min so a misread (e.g. seconds vs ms)
	// can't effectively disable keepalive in practice.
	//
	// Treating negative values as "disable" (same as 0) matches
	// operator intent for the typical typo case and is consistent
	// with the disable contract on `sse.startKeepalive` itself
	// (interval <= 0 → no goroutine).
	cfg.Env.SSEKeepaliveInterval = 20 * time.Second
	if v := os.Getenv("LOOMCYCLE_SSE_KEEPALIVE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.SSEKeepaliveInterval = 0
			} else {
				d := time.Duration(n) * time.Millisecond
				const minSSE = 1 * time.Second
				const maxSSE = 5 * time.Minute
				if d < minSSE {
					d = minSSE
				}
				if d > maxSSE {
					d = maxSSE
				}
				cfg.Env.SSEKeepaliveInterval = d
			}
		}
	}

	// Memory tool defaults. Per-write 64 KB, per-scope 1 MB,
	// 15-minute TTL sweep cadence. Negative values are treated as
	// "disable" (matches the SSE keepalive convention above).
	cfg.Env.MemoryMaxValueBytes = 64 * 1024
	if v := os.Getenv("LOOMCYCLE_MEMORY_MAX_VALUE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryMaxValueBytes = 0
			} else {
				cfg.Env.MemoryMaxValueBytes = n
			}
		}
	}
	cfg.Env.MemoryMaxScopeBytes = 1024 * 1024
	if v := os.Getenv("LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryMaxScopeBytes = 0
			} else {
				cfg.Env.MemoryMaxScopeBytes = n
			}
		}
	}
	cfg.Env.MemorySweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_MEMORY_SWEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemorySweepInterval = 0
			} else {
				cfg.Env.MemorySweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}

	// v0.9.0 Vector Memory env vars.
	cfg.Env.PgvectorEnabled = os.Getenv("LOOMCYCLE_PGVECTOR_ENABLED") == "1"
	cfg.Env.SqliteVecPath = os.Getenv("LOOMCYCLE_SQLITE_VEC_PATH")
	cfg.Env.MemoryEmbedBatchSize = 100
	if v := os.Getenv("LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryEmbedBatchSize = 0
			} else {
				cfg.Env.MemoryEmbedBatchSize = n
			}
		}
	}
	cfg.Env.MemoryEmbedTimeoutMs = 30000
	if v := os.Getenv("LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryEmbedTimeoutMs = 0
			} else {
				cfg.Env.MemoryEmbedTimeoutMs = n
			}
		}
	}

	// v0.10.0 OpenTelemetry env vars. All default to OFF (empty endpoint =
	// no-op tracer; zero runtime cost). When the operator sets an endpoint,
	// the bootstrap in cmd/loomcycle/main.go installs the OTLP exporter and
	// loomcycle emits per-run + per-iteration + per-provider/tool spans.
	cfg.Env.OTELExporterEndpoint = strings.TrimSpace(os.Getenv("LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT"))
	cfg.Env.OTELExporterHeaders = parseHeaderList(os.Getenv("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS"))
	cfg.Env.OTELServiceName = strings.TrimSpace(os.Getenv("LOOMCYCLE_OTEL_SERVICE_NAME"))
	if cfg.Env.OTELServiceName == "" {
		cfg.Env.OTELServiceName = "loomcycle"
	}
	cfg.Env.OTELTracesSamplerRatio = 1.0
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO")); v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil {
			// Clamp to [0, 1] so an operator's "100" or "-0.5" doesn't
			// silently produce a broken sampler.
			switch {
			case r < 0:
				cfg.Env.OTELTracesSamplerRatio = 0
			case r > 1:
				cfg.Env.OTELTracesSamplerRatio = 1
			default:
				cfg.Env.OTELTracesSamplerRatio = r
			}
		}
	}

	// v0.10.1 per-tenant fairness env var. Yaml is the canonical source
	// (cfg.Concurrency.MaxConcurrentRunsPerUser); the env override lets
	// containerized operators set it without editing the mounted yaml.
	// Negative values clamp to 0 (= disabled, back-compat).
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				cfg.Concurrency.MaxConcurrentRunsPerUser = 0
			} else {
				cfg.Concurrency.MaxConcurrentRunsPerUser = n
			}
		}
	}

	// Channel tool defaults (v0.8.4). Per-write 64 KB, 15-minute
	// TTL sweep cadence, 30 s long-poll cap — same shape as Memory's
	// negative-as-disable convention.
	cfg.Env.ChannelsMaxValueBytes = 64 * 1024
	if v := os.Getenv("LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsMaxValueBytes = 0
			} else {
				cfg.Env.ChannelsMaxValueBytes = n
			}
		}
	}
	// Run-ingest body cap (RFC AT). Default 16 MiB so a request can carry an
	// inline base64 image; a positive override tunes it, a non-positive value
	// is ignored (keep the default — the cap is a security floor).
	cfg.Env.MaxRequestBytes = 16 << 20
	if v := os.Getenv("LOOMCYCLE_MAX_REQUEST_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.MaxRequestBytes = n
		}
	}
	cfg.Env.ChannelsSweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_CHANNELS_SWEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsSweepInterval = 0
			} else {
				cfg.Env.ChannelsSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}
	cfg.Env.ChannelsLongPollCapMS = 30000
	if v := os.Getenv("LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsLongPollCapMS = 0
			} else {
				cfg.Env.ChannelsLongPollCapMS = n
			}
		}
	}
	cfg.Env.ChannelsMaxPendingDeferred = 10000
	if v := os.Getenv("LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsMaxPendingDeferred = 0
			} else {
				cfg.Env.ChannelsMaxPendingDeferred = n
			}
		}
	}

	// v0.8.x process-resource metrics sampler. Default OFF; operator
	// RFC J code-js provider. Default OFF; the provider is registered in
	// main.go only when enabled. Root defaults to ./agent_code (mirrors
	// the skills bundling convention). Timeout floored at 1s.
	cfg.Env.CodeAgentsEnabled = os.Getenv("LOOMCYCLE_CODE_AGENTS_ENABLED") == "1"
	cfg.Env.CodeAgentsRoot = "./agent_code"
	if v := os.Getenv("LOOMCYCLE_CODE_AGENTS_ROOT"); v != "" {
		cfg.Env.CodeAgentsRoot = v
	}
	cfg.Env.CodeAgentsDeterministic = os.Getenv("LOOMCYCLE_CODE_AGENTS_DETERMINISTIC") == "1"
	cfg.Env.CodeAgentsRunTimeout = 120 * time.Second
	if v := os.Getenv("LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d := time.Duration(n) * time.Second
			if d < time.Second {
				d = time.Second
			}
			cfg.Env.CodeAgentsRunTimeout = d
		}
	}

	// opts in via LOOMCYCLE_METRICS_ENABLED=1.
	cfg.Env.MetricsEnabled = os.Getenv("LOOMCYCLE_METRICS_ENABLED") == "1"
	cfg.Env.MetricsSampleInterval = 5 * time.Second
	if v := os.Getenv("LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d := time.Duration(n) * time.Millisecond
			// Floor 1s to prevent accidental write-storms from a
			// typo'd value like 50 (interpreted as 50ms not 5s).
			const minInterval = 1 * time.Second
			if d < minInterval {
				d = minInterval
			}
			cfg.Env.MetricsSampleInterval = d
		}
	}
	cfg.Env.MetricsRetentionDays = 7
	if v := os.Getenv("LOOMCYCLE_METRICS_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Env.MetricsRetentionDays = n
		}
	}
	cfg.Env.MetricsCollectSystem = os.Getenv("LOOMCYCLE_METRICS_COLLECT_SYSTEM") == "1"

	// v1.x RFC E scheduler runtime. Default OFF; operator opts in via
	// LOOMCYCLE_SCHEDULER_ENABLED=1. When false the sweeper goroutine
	// is not started — ScheduleDef tool still works for authoring +
	// listing, but nothing fires.
	cfg.Env.SchedulerEnabled = os.Getenv("LOOMCYCLE_SCHEDULER_ENABLED") == "1"
	cfg.Env.SchedulerTickSeconds = 30
	if v := os.Getenv("LOOMCYCLE_SCHEDULER_TICK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.SchedulerTickSeconds = n
		}
	}
	cfg.Env.SchedulerFireTimeoutSeconds = 600
	if v := os.Getenv("LOOMCYCLE_SCHEDULER_FIRE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.SchedulerFireTimeoutSeconds = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST"); v != "" {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Env.SchedulerEnvAllowlist = append(cfg.Env.SchedulerEnvAllowlist, name)
			}
		}
	}

	// v1.x RFC H inbound-webhook receiver. Default OFF; operator opts in
	// via LOOMCYCLE_WEBHOOKS_ENABLED=1. Mirrors SchedulerEnabled — when
	// false the receiver route is not mounted; the WebhookDef tool still
	// works for authoring + listing.
	cfg.Env.WebhooksEnabled = os.Getenv("LOOMCYCLE_WEBHOOKS_ENABLED") == "1"
	if v := os.Getenv("LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST"); v != "" {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Env.WebhooksEnvAllowlist = append(cfg.Env.WebhooksEnvAllowlist, name)
			}
		}
	}
	cfg.Env.WebhooksAllowUnauthenticated = os.Getenv("LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED") == "1"

	// v1.x RFC G A2A server surface. Default OFF; operator opts in via
	// LOOMCYCLE_A2A_ENABLED=1 + names the active card to serve. Tenancy
	// routing is host/path-authoritative (trust boundary), never from a
	// request body.
	cfg.Env.A2AServerEnabled = os.Getenv("LOOMCYCLE_A2A_ENABLED") == "1"
	cfg.Env.A2AServerCardName = strings.TrimSpace(os.Getenv("LOOMCYCLE_A2A_SERVER_CARD"))
	cfg.Env.A2ATenancyRouting = strings.TrimSpace(os.Getenv("LOOMCYCLE_A2A_TENANCY_ROUTING"))
	cfg.Env.A2APublicBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("LOOMCYCLE_A2A_PUBLIC_BASE_URL")), "/")
	if cfg.Env.A2AServerEnabled {
		switch cfg.Env.A2ATenancyRouting {
		case "", "none", "host", "path":
		default:
			return nil, fmt.Errorf("LOOMCYCLE_A2A_TENANCY_ROUTING: must be one of none|host|path, got %q", cfg.Env.A2ATenancyRouting)
		}
		if cfg.Env.A2AServerCardName == "" {
			return nil, fmt.Errorf("LOOMCYCLE_A2A_ENABLED=1 requires LOOMCYCLE_A2A_SERVER_CARD to name the active server card")
		}
	}

	// v0.12.0 multi-replica HA: cluster mode activates when REPLICA_ID
	// is set. Validation is by coord.ValidateReplicaID; we re-implement
	// the regex here to avoid an import cycle (coord depends on config
	// via Env propagation in main.go, not the other way around).
	cfg.Env.ReplicaID = os.Getenv("LOOMCYCLE_REPLICA_ID")
	if cfg.Env.ReplicaID != "" {
		if err := validateReplicaID(cfg.Env.ReplicaID); err != nil {
			return nil, fmt.Errorf("LOOMCYCLE_REPLICA_ID: %w", err)
		}
	}
	cfg.Env.CancelAckTimeoutMs = 5000
	if v := os.Getenv("LOOMCYCLE_CANCEL_ACK_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.CancelAckTimeoutMs = n
		}
	}

	cfg.Env.MetricsSweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MetricsSweepInterval = 0
			} else {
				cfg.Env.MetricsSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}

	// v0.12.3 Phase 4 pause-state cache TTL. Effective only when
	// LOOMCYCLE_REPLICA_ID is set (cluster mode); single-replica
	// pause.Manager skips the DB cache entirely.
	cfg.Env.PauseCacheTTLMs = 1000 // default 1s
	if v := os.Getenv("LOOMCYCLE_PAUSE_CACHE_TTL_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.PauseCacheTTLMs = n
		}
	}

	// v0.8.17 pause manager default timeout. 0 ⇒ pause package
	// default (30s). The manager itself clamps at pause.MaxPauseTimeout
	// regardless of what we set here.
	if v := os.Getenv("LOOMCYCLE_PAUSE_DEFAULT_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.PauseDefaultTimeoutMs = n
		}
	}

	// v0.8.15 LoomCycle MCP: dynamic agent registration policy.
	cfg.Env.MCPAllowPrivilegedTools = os.Getenv("LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS") == "1"
	cfg.Env.MCPAllowDynamicStdio = os.Getenv("LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO") == "1"
	// F32: default-ON. Only an explicit "0" disables redaction; an unset var
	// keeps the secure posture (secrets masked before persistence).
	cfg.Env.RedactSecrets = os.Getenv("LOOMCYCLE_REDACT_SECRETS") != "0"
	cfg.Env.DynamicAgentDefaultTTLSeconds = 86400 // 24h
	if v := os.Getenv("LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Env.DynamicAgentDefaultTTLSeconds = n
		}
	}
	cfg.Env.DynamicAgentSweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.DynamicAgentSweepInterval = 0
			} else {
				cfg.Env.DynamicAgentSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}

	// RFC AH Phase 2b ephemeral-volume crash-recovery sweeper. Default 60s;
	// 0 disables (the inline run-completion purge still runs).
	cfg.Env.EphemeralVolumeSweepInterval = 60 * time.Second
	if v := os.Getenv("LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.EphemeralVolumeSweepInterval = 0
			} else {
				cfg.Env.EphemeralVolumeSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}

	// v0.8.5 substrate caps. Same negative-as-disable convention as
	// Memory + Channel sibling caps.
	cfg.Env.AgentDefMaxDefinitionBytes = 131072
	if v := os.Getenv("LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.AgentDefMaxDefinitionBytes = 0
			} else {
				cfg.Env.AgentDefMaxDefinitionBytes = n
			}
		}
	}
	cfg.Env.AgentDefMaxDescriptionBytes = 8192
	if v := os.Getenv("LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.AgentDefMaxDescriptionBytes = 0
			} else {
				cfg.Env.AgentDefMaxDescriptionBytes = n
			}
		}
	}
	cfg.Env.AgentDefMaxCodeBytes = 262144
	if v := os.Getenv("LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.AgentDefMaxCodeBytes = 0
			} else {
				cfg.Env.AgentDefMaxCodeBytes = n
			}
		}
	}
	// v0.8.22 SkillDef caps. Mirror of AgentDef caps.
	cfg.Env.SkillDefMaxBodyBytes = 131072
	if v := os.Getenv("LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.SkillDefMaxBodyBytes = 0
			} else {
				cfg.Env.SkillDefMaxBodyBytes = n
			}
		}
	}
	cfg.Env.SkillDefMaxDescriptionBytes = 8192
	if v := os.Getenv("LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.SkillDefMaxDescriptionBytes = 0
			} else {
				cfg.Env.SkillDefMaxDescriptionBytes = n
			}
		}
	}
	cfg.Env.EvaluationMaxJudgementBytes = 32768
	if v := os.Getenv("LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.EvaluationMaxJudgementBytes = 0
			} else {
				cfg.Env.EvaluationMaxJudgementBytes = n
			}
		}
	}
	cfg.Env.EvaluationMaxRationaleBytes = 8192
	if v := os.Getenv("LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.EvaluationMaxRationaleBytes = 0
			} else {
				cfg.Env.EvaluationMaxRationaleBytes = n
			}
		}
	}

	// Provider streaming timeouts. Defaults match streamhttp.Default*.
	// Negative or zero values are NOT treated as "disable" — there's no
	// safe interpretation of "stream forever" given the agent loop's
	// liveness assumptions. Operators bumping these should pick a real
	// number; bad input falls through to the default.
	cfg.Env.ProviderHeaderTimeout = 60 * time.Second
	if v := os.Getenv("LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ProviderHeaderTimeout = time.Duration(n) * time.Millisecond
		}
	}
	cfg.Env.ProviderIdleTimeout = 90 * time.Second
	if v := os.Getenv("LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ProviderIdleTimeout = time.Duration(n) * time.Millisecond
		}
	}

	// Local Ollama gets a more generous default than the cloud providers:
	// a cold model load + large-context prompt eval can take minutes to
	// first token. Scoped to the ollama-local registration; see the field
	// docs on Env.OllamaLocalHeaderTimeout.
	cfg.Env.OllamaLocalHeaderTimeout = 300 * time.Second
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalHeaderTimeout = time.Duration(n) * time.Millisecond
		}
	}
	cfg.Env.OllamaLocalIdleTimeout = 300 * time.Second
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalIdleTimeout = time.Duration(n) * time.Millisecond
		}
	}

	// resolveSkills MUST come after env loading (it needs SkillsRoot)
	// AND after resolveSystemPromptFiles (skill bodies append onto
	// SystemPrompt — file-loaded prompts have to land first).
	if err := resolveSkills(cfg); err != nil {
		return nil, err
	}

	// v0.8.7 default-add: every agent gets Context auto-appended to
	// its allowed_tools unless `disable_context: true` is set. Runs
	// after resolveSkills so skill-driven AllowedTools widening has
	// already taken effect.
	addContextToolDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// addContextToolDefaults appends "Context" to every agent's
// AllowedTools unless DisableContext is set (or "Context" is already
// listed). v0.8.7 introspection is foundational for self-evolving
// agents; missing it is a footgun. Operators with airgapped agents
// opt out per-agent via `disable_context: true`.
func addContextToolDefaults(cfg *Config) {
	for name, def := range cfg.Agents {
		if def.DisableContext {
			continue
		}
		alreadyHas := false
		for _, t := range def.AllowedTools {
			// Case-insensitive match. An operator's lowercase
			// `allowed_tools: [context]` is a typo, not an explicit
			// listing — but case-sensitive eq would let the typo
			// double-add (yielding [context, Context]) and confuse
			// the per-run dispatcher's case-sensitive registry
			// lookup. PR 3 review fix.
			if strings.EqualFold(t, "Context") {
				alreadyHas = true
				break
			}
		}
		if alreadyHas {
			continue
		}
		def.AllowedTools = append(def.AllowedTools, "Context")
		cfg.Agents[name] = def
	}
}

// checkRetiredJailEnv fails config load when a deploy still sets one of the
// retired legacy-jail env vars (RFC AH Phase 3). Volumes are now the sole
// filesystem mechanism; honoring these silently would no-op (an unbound agent
// is denied all disk access), so we surface a clear migration hint instead.
func checkRetiredJailEnv() error {
	var set []string
	for _, name := range []string{"LOOMCYCLE_READ_ROOT", "LOOMCYCLE_WRITE_ROOT", "LOOMCYCLE_BASH_CWD"} {
		if os.Getenv(name) != "" {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf("%s retired in RFC AH Phase 3 — declare a `volumes:` block instead, e.g. `default: {path: …, mode: rw, default: true}` (see docs/CONFIGURATION.md)", strings.Join(set, ", "))
}

// validateVolumes checks the top-level `volumes:` map (RFC AH Phase 1)
// and normalises each path to absolute in place. Rules:
//   - Mode is one of "ro" / "rw" / "" (empty defaults to rw).
//   - Path is required, must already exist, and must be a directory.
//     Static volumes map EXISTING infrastructure — the runtime never
//     creates one — so a missing/non-dir path is a config-load error
//     (surfaced here rather than as a baffling call-time failure).
//   - At most one volume may be `default: true`.
//
// Paths are resolved to absolute so the run-start binding resolution +
// the tools' resolveInsideRoot get a stable root independent of process
// cwd. Symlinks are left intact here; resolveInsideRoot EvalSymlinks at
// call time (the TOCTOU-safe containment check is unchanged).
func validateVolumes(c *Config) error {
	defaultSeen := ""
	dynamicRootSeen := ""
	// Deterministic iteration order so the "two defaults" error names a
	// stable pair across runs (Go map order is randomised).
	names := make([]string, 0, len(c.Volumes))
	for name := range c.Volumes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := c.Volumes[name]
		if name == "" {
			return fmt.Errorf("volumes: empty volume name")
		}
		switch v.Mode {
		case "", "rw", "ro":
		default:
			return fmt.Errorf("volumes.%s: invalid mode %q (want \"rw\", \"ro\", or empty for rw)", name, v.Mode)
		}
		if v.Path == "" {
			return fmt.Errorf("volumes.%s: path is required", name)
		}
		abs, err := filepath.Abs(v.Path)
		if err != nil {
			return fmt.Errorf("volumes.%s: resolve path %q: %w", name, v.Path, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("volumes.%s: path %q must already exist (static volumes map existing infrastructure; the runtime never creates them): %w", name, abs, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("volumes.%s: path %q is not a directory", name, abs)
		}
		v.Path = abs
		c.Volumes[name] = v
		if v.Default {
			if defaultSeen != "" {
				return fmt.Errorf("volumes: at most one volume may be default:true (found %q and %q)", defaultSeen, name)
			}
			defaultSeen = name
		}
		// RFC AH Phase 2a: at most one static volume may be the dynamic
		// root — the single operator-blessed parent the VolumeDef substrate
		// provisions (and confines) dynamic volumes inside. Two roots would
		// make "which parent does a create land in" ambiguous.
		if v.DynamicRoot {
			if dynamicRootSeen != "" {
				return fmt.Errorf("volumes: at most one volume may be dynamic_root:true (found %q and %q)", dynamicRootSeen, name)
			}
			dynamicRootSeen = name
		}
	}
	return nil
}

// ResolveAgentModel returns (provider, model) for the named agent, walking
// model aliases and provider defaults.
func (c *Config) ResolveAgentModel(agent string) (provider string, model string, err error) {
	def, ok := c.Agents[agent]
	if !ok {
		return "", "", fmt.Errorf("unknown agent %q", agent)
	}
	return c.ResolveAgentDefModel(agent, def)
}

// ExpandModelAlias resolves a model-alias (a key in the top-level models:
// map) to its concrete provider/model. The alias only fills an EMPTY
// provider — an explicit provider on the pin/candidate always wins — so the
// tier path and the pin path expand aliases identically. A nil/absent map or
// a non-alias model is a no-op (the model is treated as a literal). This is
// the single source of truth for alias expansion, shared by the pin path
// (ResolveAgentDefModel) and the tier path (the resolver-boundary converters
// in internal/api/http and cmd/loomcycle); keeping it in one place stops the
// two paths from drifting.
func ExpandModelAlias(models map[string]ModelRef, provider, model string) (string, string) {
	if ref, ok := models[model]; ok {
		model = ref.Model
		if provider == "" {
			provider = ref.Provider
		}
	}
	return provider, model
}

// ResolveAgentDefModel mirrors ResolveAgentModel but resolves against
// a caller-supplied AgentDef instead of looking it up in c.Agents.
// Used by the sub-agent path when an overlay has already produced an
// effective def whose Provider/Model differ from the static yaml.
// Same alias-expansion + defaults-fallback rules as ResolveAgentModel.
func (c *Config) ResolveAgentDefModel(agent string, def AgentDef) (provider string, model string, err error) {
	model = def.Model
	provider = def.Provider

	// code-js agents (RFC J) have no LLM model: the synthetic provider
	// resolves code by agent name (agent_code/<name>/index.js), not by
	// model. A model value is therefore cosmetic here — default it to the
	// agent name when unset so resolution succeeds and run records carry a
	// meaningful identifier. Usage/OTEL report loomcycle/code-js regardless
	// (see internal/providers/codejs). An explicit model still wins.
	if provider == "code-js" && model == "" {
		return provider, agent, nil
	}

	// If model is an alias in models:, expand it.
	provider, model = ExpandModelAlias(c.Models, provider, model)
	if provider == "" {
		provider = c.Defaults.Provider
	}
	if model == "" {
		model = c.Defaults.Model
	}
	if provider == "" {
		return "", "", fmt.Errorf("agent %q: no provider resolved", agent)
	}
	if model == "" {
		return "", "", fmt.Errorf("agent %q: no model resolved", agent)
	}
	return provider, model, nil
}

// envVarRe matches ${VAR} interpolation tokens in the YAML source.
var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the value of VAR, but only for VARs whose
// names match ExpandEnvAllowed. Other ${VAR} tokens pass through verbatim.
//
// Why an allowlist: a malicious or compromised YAML in a GitOps / shared-
// config setup could otherwise inject `${ANTHROPIC_API_KEY}` into outbound
// fields (MCP server URL, args, system prompt) and exfiltrate the secret.
// We restrict expansion to a known-safe set of names that the project
// explicitly publishes for this purpose.
//
// To add a new var that needs to be referenceable from YAML, add it here.
// Provider keys (ANTHROPIC_API_KEY, OPENAI_API_KEY) are intentionally NOT
// in this list — they reach providers through the Env struct, not via the
// YAML interpolation path.
func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if !ExpandEnvAllowed(name) || expandDenyNames[name] {
			return m // leave verbatim — caller sees the literal ${...}
		}
		v := os.Getenv(name)
		// exp7 I6: env values are interpolated into raw YAML bytes BEFORE
		// yaml.Unmarshal, so a value carrying a newline could inject new
		// keys/structure into the document. A legitimate scalar is never
		// multi-line, so refuse to expand it — leaving the literal ${name}
		// is a visible "didn't expand" signal and cannot corrupt the
		// document structure (vs. an error path that would change this
		// helper's signature, which the runtime ExpandEnv path also uses).
		if strings.ContainsAny(v, "\r\n") {
			return m
		}
		return v
	})
}

// ExpandEnv is the exported entry point for substrate paths that register
// servers OUTSIDE yaml-load and must mirror its ${LOOMCYCLE_*} expansion.
// A yaml-configured MCP server gets expansion for free in Load() (the whole
// document passes through expandEnv at line ~1852); a server registered at
// runtime via MCPServerDef never passes through Load, so it calls this on its
// operator-authored string fields to get the identical, same-allowlist
// behaviour. Without it the inner ${LOOMCYCLE_TOKEN} in a header like
// `Bearer ${run.credentials.x:-${LOOMCYCLE_TOKEN}}` survives verbatim and the
// request-time substituter truncates on the nested brace.
func ExpandEnv(s string) string { return expandEnv(s) }

// parseHeaderList parses a comma-separated `key=value,key2=value2` string
// into a map. Whitespace around keys, values, and separators is trimmed.
// Entries without `=` are skipped. Returns nil for an empty input so the
// caller doesn't need to nil-check before iterating. Used by the v0.10.0
// OTEL exporter for collector auth headers (e.g. `x-honeycomb-team=KEY`).
func parseHeaderList(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ExpandEnvAllowed reports whether the given env-var name may be expanded
// inside YAML. Allowlist:
//   - any LOOMCYCLE_-prefixed variable (the project's own namespace)
//   - well-known third-party keys MCP servers commonly need
//
// Exported because the v1.x RFC H webhook receiver reuses this exact
// predicate as the VERIFICATION-secret namespace auto-allow: a webhook
// whose signing_secret_env / bearer_token_env is LOOMCYCLE_*-prefixed (or
// one of the known third-party names) resolves without an explicit
// allowlist entry, mirroring the YAML ${LOOMCYCLE_*} posture. The verify
// secret is consumed by the receiver and never reaches the agent, so the
// auto-allow carries no exfiltration risk. (The agent-reachable
// user_credentials_from_env path does NOT use this predicate — see
// internal/api/webhook/runinput.go — so a runtime-authored webhook cannot
// inject an arbitrary LOOMCYCLE_* value into a run.)
//
// Note on the v0.8.x ${run.user_bearer} tokens: these are intentionally
// NOT handled here. envVarRe above requires var names matching
// [A-Za-z_][A-Za-z0-9_]*; the "." in "run.user_bearer" structurally
// cannot match, so those tokens survive yaml-load verbatim. Per-run
// substitution happens at MCP outbound request time in
// internal/tools/mcp/http/client.go Client.do(). This means a yaml
// header value like `Bearer ${run.user_bearer:-${LOOMCYCLE_STATIC}}`
// has its inner ${LOOMCYCLE_STATIC} resolved here, while the outer
// ${run.user_bearer:-...} flows through to the request-time substitution.
func ExpandEnvAllowed(name string) bool {
	if strings.HasPrefix(name, "LOOMCYCLE_") {
		return true
	}
	switch name {
	case "BRAVE_API_KEY",
		"GITHUB_TOKEN",
		"SLACK_BOT_TOKEN",
		"REDIS_URL":
		return true
	}
	return false
}

// expandDenyNames is the authoritative set of loomcycle's OWN infrastructure /
// admin secrets that must NEVER be interpolated into a YAML/MCP field, even
// though the LOOMCYCLE_ prefix (or a bare third-party name) would otherwise
// allow it (exp7 C2 + the v0.34.0 security review S1). These are loomcycle's
// own credentials — the DB DSN, the operator bearer, the operator-token hashing
// pepper, the thin-client upstream MCP token, and the OTEL exporter headers
// (which carry collector auth like x-honeycomb-team). Interpolating any of them
// into an attacker-controlled outbound MCP URL/header/arg (a runtime-authored
// MCPServerDef → config.ExpandEnv at dial time) would exfiltrate the secret to
// a third party. They reach the system via the Env struct, never via YAML
// interpolation, so denying them here breaks no legitimate use.
//
// Deliberately a tight NAMED set, NOT the broad IsSecretEnvName suffix match —
// a suffix deny would also block the legitimate operator pattern of referencing
// a per-MCP auth token via ${LOOMCYCLE_*_TOKEN} / ${LOOMCYCLE_STATIC_BEARER} in
// THAT server's own header (documented in docs/ARCHITECTURE.md). The
// completeness this named set needs — every loomcycle-OWNED infra secret denied
// — is enforced by TestExpandDenyNames_CoversInfraSecretReads, which scans this
// package's own os.Getenv("LOOMCYCLE_…") secret-bearing reads and fails CI if a
// new one isn't listed here (closing the "incomplete blocklist" gap S1 named).
var expandDenyNames = map[string]bool{
	"PG_DSN":                               true,
	"LOOMCYCLE_PG_DSN":                     true,
	"LOOMCYCLE_SQLMEM_PG_DSN":              true, // RFC AA aux-DB DSN (carries the aux credentials)
	"LOOMCYCLE_AUTH_TOKEN":                 true,
	"LOOMCYCLE_OPERATOR_TOKEN_PEPPER":      true, // RFC L token-hash pepper (S1: the high-value miss)
	"LOOMCYCLE_MCP_UPSTREAM_TOKEN":         true, // thin-client upstream MCP bearer
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS": true, // collector auth headers
}

// secretEnvSuffixes are the env-var NAME patterns this project classifies as
// secret (CLAUDE.md §security). A name ending in one of these denotes a
// credential whose VALUE must be kept out of persisted transcripts (F32).
var secretEnvSuffixes = []string{
	"_KEY", "_TOKEN", "_SECRET", "_AUTH", "_PASSWORD", "_CREDENTIAL", "_CREDENTIALS",
}

// IsSecretEnvName reports whether an env-var name denotes a secret VALUE, by the
// documented suffix classification (case-insensitive). Used by the secret
// redactor (internal/redact) to decide which env values to mask from persisted
// tool transcripts. Deliberately NOT keyed off ExpandEnvAllowed: that allows
// every LOOMCYCLE_*-prefixed var (incl. non-secrets like LOOMCYCLE_LISTEN_ADDR),
// so reusing it would collect non-secret values and risk masking benign
// substrings. The suffix list catches both LOOMCYCLE_* secrets and provider keys
// (ANTHROPIC_API_KEY, OPENAI_API_KEY, …) without a hard-coded name list.
func IsSecretEnvName(name string) bool {
	up := strings.ToUpper(name)
	for _, suf := range secretEnvSuffixes {
		if strings.HasSuffix(up, suf) {
			return true
		}
	}
	return false
}

func getenvDefault(name, dflt string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return dflt
}

// getenvBool reads a boolean env var with an explicit default. Unset → dflt;
// "1"/"true"/"yes"/"on" → true; "0"/"false"/"no"/"off" → false (case-
// insensitive). An unrecognised non-empty value → dflt (fail-safe to the
// documented default rather than silently flipping).
func getenvBool(name string, dflt bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return dflt
	}
}

// discoverAgents walks LOOMCYCLE_AGENTS_ROOT, parses each `<name>.md`
// frontmatter as an AgentDef base, and merges the result into
// cfg.Agents — yaml entries override per-field on conflict.
//
// Resolution-order constraint: this runs AFTER yaml.Unmarshal but
// BEFORE resolveSystemPromptFiles. The latter needs to see the merged
// map so a yaml `system_prompt_file` for a same-named MD agent works
// (yaml's pointer wins over MD's body — see mergeAgentDef's special
// case). resolveSkills runs later and also operates on the merged map.
//
// Discovered AgentDefs use the same field set as yaml-defined ones; no
// new validation rules are introduced here. validate() runs unchanged
// over the merged set, so a merge that produces both Pin and Tier
// (e.g. MD `model: haiku` + yaml override `tier: low`) gets caught by
// the existing post-load check.
func discoverAgents(cfg *Config, root string) error {
	set, err := agents.LoadSet(root)
	if err != nil {
		return fmt.Errorf("agents discovery: %w", err)
	}
	if cfg.Agents == nil {
		cfg.Agents = make(map[string]AgentDef)
	}
	for _, name := range set.Names() {
		discovered, _ := set.Get(name)
		base := agentFromDiscovered(discovered)
		// yaml-as-override: if cfg.Agents[name] already exists from
		// the yaml unmarshal, the yaml fields beat the discovered
		// ones for any non-zero override slot.
		yamlEntry, hasYaml := cfg.Agents[name]
		if hasYaml {
			cfg.Agents[name] = mergeAgentDef(base, yamlEntry)
		} else {
			cfg.Agents[name] = base
		}
	}
	return nil
}

// agentFromDiscovered converts the agents-package shape (which can't
// import config without creating a cycle) to AgentDef. Field-for-field
// passthrough except Models, where the local TierCandidate type is
// converted to config.TierCandidate.
func agentFromDiscovered(d *agents.Agent) AgentDef {
	def := AgentDef{
		Provider:         d.Provider,
		Model:            d.Model,
		Code:             d.Code,
		SystemPrompt:     d.SystemPrompt,
		SystemPromptFile: d.SystemPromptFile,
		AllowedTools:     d.AllowedTools,
		Skills:           d.Skills,
		MaxTokens:        d.MaxTokens,
		MaxIterations:    d.MaxIterations,
		// MaxConcurrentChildren rounds out the loop-budget trio (with
		// MaxTokens/MaxIterations) — it lives on agents.Agent + the MD
		// frontmatter, so dropping it here silently capped an MD-declared
		// agent at the runtime default (4) instead of its declared value.
		MaxConcurrentChildren: d.MaxConcurrentChildren,
		Tier:                  d.Tier,
		Effort:                d.Effort,
		Providers:             d.Providers,
		MemoryScopes:          d.MemoryScopes,
		MemoryQuotaBytes:      d.MemoryQuotaBytes,
		MemoryBackend:         d.MemoryBackend,
		Channels: AgentChannelACL{
			Publish:   d.Channels.Publish,
			Subscribe: d.Channels.Subscribe,
		},
		AgentDefScopes:   d.AgentDefScopes,
		SkillDefScopes:   d.SkillDefScopes,
		VolumeDefScopes:  d.VolumeDefScopes,
		EvaluationScopes: d.EvaluationScopes,
		// F14: an MD-declared `interruption:` block now flows to config
		// (parity with channels) so it takes effect at runtime AND the
		// content hash matches the substrate.
		Interruption: AgentInterruptionACL{
			Enabled:    d.Interruption.Enabled,
			Kinds:      d.Interruption.Kinds,
			MaxPending: d.Interruption.MaxPending,
		},
	}
	if len(d.Models) > 0 {
		def.Models = make(map[string][]TierCandidate, len(d.Models))
		for tier, cands := range d.Models {
			out := make([]TierCandidate, 0, len(cands))
			for _, c := range cands {
				out = append(out, TierCandidate{Provider: c.Provider, Model: c.Model})
			}
			def.Models[tier] = out
		}
	}
	return def
}

// mergeAgentDef returns base with override's non-zero fields applied
// on top. Per-field shallow merge: each AgentDef field is replaced
// independently when the override's value is non-zero, otherwise
// base's value carries through.
//
// Slices/maps: yaml.Unmarshal produces nil for absent keys and
// non-nil-empty for explicit empty entries (`allowed_tools: []`).
// We treat nil as "absent in yaml — keep discovered" and non-nil as
// "explicit override — take yaml". This lets ops zero-out a list by
// writing the empty form in yaml.
//
// Special case: SystemPromptFile in the yaml override clears the
// discovered SystemPrompt. Otherwise resolveSystemPromptFiles would
// load the file's contents and concatenate alongside the MD body,
// confusing the prompt with two sources. The yaml SystemPromptFile
// is the explicit "use this file instead of the MD body" signal.
func mergeAgentDef(base, override AgentDef) AgentDef {
	out := base
	if override.Provider != "" {
		out.Provider = override.Provider
	}
	if override.Model != "" {
		out.Model = override.Model
	}
	if override.Code != "" {
		out.Code = override.Code
	}
	// Either prompt-source override clears the OTHER source on the
	// merged struct. Without this, a discovered MD that sets
	// system_prompt_file in its frontmatter merging with a yaml
	// override that sets inline system_prompt (or vice versa) would
	// produce both fields populated and trip resolveSystemPromptFiles'
	// mutual-exclusion check downstream — making yaml overrides for
	// the prompt source unusable. The yaml override is the explicit
	// "use this prompt source instead" signal, regardless of which
	// shape it takes.
	if override.SystemPrompt != "" {
		out.SystemPrompt = override.SystemPrompt
		out.SystemPromptFile = ""
	}
	if override.SystemPromptFile != "" {
		out.SystemPromptFile = override.SystemPromptFile
		out.SystemPrompt = ""
	}
	if override.AllowedTools != nil {
		out.AllowedTools = override.AllowedTools
	}
	if override.Skills != nil {
		out.Skills = override.Skills
	}
	if override.MaxTokens != 0 {
		out.MaxTokens = override.MaxTokens
	}
	if override.MaxIterations != 0 {
		out.MaxIterations = override.MaxIterations
	}
	if override.MaxConcurrentChildren != 0 {
		out.MaxConcurrentChildren = override.MaxConcurrentChildren
	}
	if override.Tier != "" {
		out.Tier = override.Tier
	}
	if override.Effort != "" {
		out.Effort = override.Effort
	}
	if override.Providers != nil {
		out.Providers = override.Providers
	}
	if override.Models != nil {
		out.Models = override.Models
	}
	if override.MemoryScopes != nil {
		out.MemoryScopes = override.MemoryScopes
	}
	if override.MemoryQuotaBytes != 0 {
		out.MemoryQuotaBytes = override.MemoryQuotaBytes
	}
	if override.SqlScopes != nil {
		out.SqlScopes = override.SqlScopes
	}
	if override.SqlQuotaBytes != 0 {
		out.SqlQuotaBytes = override.SqlQuotaBytes
	}
	if override.MemoryBackend != "" {
		out.MemoryBackend = override.MemoryBackend
	}
	if override.Channels.Publish != nil {
		out.Channels.Publish = override.Channels.Publish
	}
	if override.Channels.Subscribe != nil {
		out.Channels.Subscribe = override.Channels.Subscribe
	}
	if override.AgentDefScopes != nil {
		out.AgentDefScopes = override.AgentDefScopes
	}
	if override.SkillDefScopes != nil {
		out.SkillDefScopes = override.SkillDefScopes
	}
	if override.VolumeDefScopes != nil {
		out.VolumeDefScopes = override.VolumeDefScopes
	}
	if override.EvaluationScopes != nil {
		out.EvaluationScopes = override.EvaluationScopes
	}
	// F14: yaml interruption override replaces the MD-discovered block when
	// the operator set any field. Mirrors the substrate applyOverlay's
	// "any non-zero field signals intent" rule (and shares its known limit:
	// flipping enabled true→false via override isn't expressible — the
	// all-zero block reads as "not set").
	if override.Interruption.Enabled || len(override.Interruption.Kinds) > 0 || override.Interruption.MaxPending != 0 {
		out.Interruption = override.Interruption
	}
	return out
}

// resolveSystemPromptFiles populates each agent's SystemPrompt from
// SystemPromptFile when set. Relative paths are resolved against the
// YAML config file's directory so the operator's "agents/qa.md" works
// regardless of the process's cwd.
//
// Errors:
//   - both SystemPrompt and SystemPromptFile set on the same agent
//   - SystemPromptFile points at a missing or unreadable file
//
// SECURITY: the YAML config is treated as fully trusted operator
// input. SystemPromptFile values may use "../" relative paths that
// escape configDir, and os.ReadFile follows symlinks — both are
// intentional. This is fine when the operator owns the YAML (typical
// deployment: a sysadmin checks the file in alongside the binary).
//
// If you ever load YAML from a less-trusted source — multi-tenant
// control plane, GitOps from PR branches, shared file shares — you
// MUST clamp paths to configDir (reject relative segments containing
// ".." after Clean) and open with O_NOFOLLOW. The current code makes
// neither check; an attacker who can write YAML can read any file
// the loomcycle process can.
func resolveSystemPromptFiles(cfg *Config, configPath string) error {
	configDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	for name, def := range cfg.Agents {
		if def.SystemPromptFile == "" {
			continue
		}
		if def.SystemPrompt != "" {
			return fmt.Errorf("agent %q: system_prompt and system_prompt_file are mutually exclusive", name)
		}
		p := def.SystemPromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(configDir, p)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("agent %q: read system_prompt_file %s: %w", name, p, err)
		}
		def.SystemPrompt = string(body)
		// SystemPromptFile is preserved on the struct — no harm, and
		// surfaces the source path for anyone debugging the config.
		cfg.Agents[name] = def
	}
	return nil
}

// SkillSpec is an INLINE skill definition — the value type of the top-level
// `skills:` map. It mirrors a SKILL.md loaded from LOOMCYCLE_SKILLS_ROOT but is
// defined in YAML: the map key is the skill name an agent's `skills: [name]`
// references; Body is the markdown bundled onto the agent's system_prompt at
// config-load; AllowedTools is the skill's tool requirement, which resolveSkills
// enforces is a SUBSET of the bundling agent's allowed_tools (a skill may never
// widen the agent's tool set). Uses the underscore `allowed_tools` key to match
// the rest of the loomcycle YAML (the SKILL.md frontmatter uses hyphenated
// `allowed-tools`).
type SkillSpec struct {
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed_tools"`
	Body         string   `yaml:"body"`
}

// resolveSkills bundles skill bodies into agent system prompts and
// validates each skill's allowed-tools is a subset of the bundling
// agent's allowed_tools. Static bundling — see Approach A in
// doc-internal/skills-design.md.
//
// Errors:
//   - an agent references a skill defined in NEITHER the top-level `skills:`
//     map NOR LOOMCYCLE_SKILLS_ROOT (silent drop would produce agents whose
//     prompts reference skills the runtime never loaded; loud failure forces
//     the operator to fix the config). An unset/empty skills root is NOT an
//     error on its own — inline `skills:` can satisfy an agent entirely.
//   - skills root set but unreadable / not a directory
//   - skill demands a tool the agent doesn't grant (security invariant)
//
// SECURITY: the subset check uses internal/tools/policy.matches so
// glob rules on either side compose correctly. Examples:
//   - skill `[Read]`, agent `[Read, HTTP]` → OK (literal match)
//   - skill `[mcp__brave__search]`, agent `[mcp__brave__*]` → OK
//     (skill literal matched by agent glob, narrowing is fine)
//   - skill `[mcp__brave__*]`, agent `[mcp__brave__search]` → ERROR
//     (skill demands broader access than agent grants)
//   - skill `[Edit]`, agent `[Read]` → ERROR (skill widens)
func resolveSkills(cfg *Config) error {
	// Build the skill registry from two sources: the LOOMCYCLE_SKILLS_ROOT
	// directory (file-backed SKILL.md; an empty root yields an empty set, no
	// error) and the top-level `skills:` map (inline, merged across config
	// layers by key like `agents:`). Inline definitions OVERLAY the root on a
	// name collision — config is authoritative. An agent's skills may thus be
	// satisfied entirely from inline defs with no skills root set.
	set, err := skills.LoadSet(cfg.Env.SkillsRoot)
	if err != nil {
		return fmt.Errorf("load skills: %w", err)
	}
	for skillName, spec := range cfg.Skills {
		set.Add(&skills.Skill{
			Name:         skillName,
			Description:  spec.Description,
			AllowedTools: spec.AllowedTools,
			Body:         spec.Body,
		})
	}
	for name, def := range cfg.Agents {
		if len(def.Skills) == 0 {
			continue
		}
		// Capture the base (pre-skill-bake) prompt so v0.8.22 SkillDef
		// per-run resolution can rebuild from it when a DB-active row
		// shadows the static body.
		def.SystemPromptBase = def.SystemPrompt
		// Build agent rule set once per agent.
		agentSet := make(map[string]bool, len(def.AllowedTools))
		for _, t := range def.AllowedTools {
			agentSet[t] = true
		}
		for _, skillName := range def.Skills {
			sk, ok := set.Get(skillName)
			if !ok {
				return fmt.Errorf("agent %q: unknown skill %q — define it under the top-level `skills:` map or as a SKILL.md under LOOMCYCLE_SKILLS_ROOT (%q)", name, skillName, cfg.Env.SkillsRoot)
			}
			// SECURITY: enforce skill.allowed-tools ⊆ agent.allowed_tools.
			var widening []string
			for _, t := range sk.AllowedTools {
				if !policy.Matches(t, agentSet) {
					widening = append(widening, t)
				}
			}
			if len(widening) > 0 {
				return fmt.Errorf(
					"agent %q: skill %q requires tools %v not granted by the agent's allowed_tools — skills may not widen the agent's tool set",
					name, skillName, widening,
				)
			}
			// Append. Use a separator only if there is already content
			// in the system prompt; first skill on a prompt-less agent
			// becomes the prompt without a leading "---".
			if def.SystemPrompt != "" {
				def.SystemPrompt += "\n\n---\n\n"
			}
			def.SystemPrompt += sk.Body
		}
		cfg.Agents[name] = def
	}
	return nil
}

// splitCSV trims whitespace and drops empties from a comma-separated env value.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// validProviderIDs is the set of provider names the resolver knows
// how to dispatch to. Adding a new driver requires extending this set
// AND wiring the driver into cmd/loomcycle/main.go's resolver.
var validProviderIDs = map[string]bool{
	"anthropic":    true,
	"openai":       true,
	"deepseek":     true,
	"ollama":       true, // hosted ollama.com (Bearer auth)
	"ollama-local": true, // local-network Ollama (no auth)
	"gemini":       true,
	// v0.11.9 — opt-in OAuth-dev provider. config-load accepts the ID;
	// the resolver returns a clear "not registered" error at request
	// time if LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 isn't set or no
	// token file exists. See docs/PROVIDERS.md.
	"anthropic-oauth-dev": true,
	// v0.12.8 — synthetic mock provider for cost-free stress testing.
	// config-load accepts the ID; the resolver returns a clear "not
	// configured" error at request time if LOOMCYCLE_MOCK_ENABLED=1
	// isn't set. See internal/providers/mock/driver.go.
	"mock": true,
	// v0.12.9 — companion stable variant; same gate as `mock`.
	// Lets operators configure tier candidate lists like
	// `[{provider: mock, ...}, {provider: mock-stable, ...}]` for
	// fallback-recovery testing without a real second provider.
	"mock-stable": true,
}

// validTierNames is the closed set of tier names. Operators choose
// per-agent which tier they want; the names are not user-extensible
// today (would require coordinating with the library matrix shape).
var validTierNames = map[string]bool{
	"low":    true,
	"middle": true,
	"high":   true,
}

// validEffortLevels mirrors the Claude / OpenAI reasoning-effort
// vocabulary. Empty string means "no hint" (driver default).
var validEffortLevels = map[string]bool{
	"":       true,
	"low":    true,
	"medium": true,
	"high":   true,
}

// validMemoryScopes is the closed set of Memory tool scope names
// accepted in agent yaml. v0.8.0 ships agent + user; future versions
// may add session / tenant.
var validMemoryScopes = map[string]bool{
	"agent": true,
	"user":  true,
}

// validSqlScopes is the closed set of RFC AA SQL Memory scope names
// accepted in agent yaml. Unlike Memory's k/v scopes it also includes
// `run` — an ephemeral per-run database dropped at run completion.
var validSqlScopes = map[string]bool{
	"agent": true,
	"user":  true,
	"run":   true,
}

// validChannelScopes is the closed set of Channel tool scope names
// accepted on a top-level `channels:` entry. agent + user mirror
// Memory's vocabulary; global is the cross-tenant fan-out shape.
var validChannelScopes = map[string]bool{
	"agent":  true,
	"user":   true,
	"global": true,
}

// validChannelSemantics is informational at the storage level (the
// wire shape is identical for queue and broadcast) — the tool layer
// uses it for documentation + boot warnings.
var validChannelSemantics = map[string]bool{
	"queue":     true,
	"broadcast": true,
}

// eventDrivenSystemChannels is the closed set of `publisher: system`
// channel names that publish on internal state transitions rather
// than on a fixed cadence. These channels do NOT require `period:`
// in operator yaml because loomcycle wires them via event hooks
// (v0.8.6 PR 3 + downstream feature PRs).
//
// New entries here document the convention; the runtime hooks live
// in the respective subsystems (heartbeat goroutine for cadence
// channels; loop / runner / pause-state handlers for event-driven
// channels).
var eventDrivenSystemChannels = map[string]bool{
	"_system/runtime-state":       true, // v0.8.9 pause/resume/restore
	"_system/provider-events":     true, // provider fallback / cache-invalidated
	"_system/interrupts/pending":  true, // v0.8.16 Interruption.ask publishes
	"_system/interrupts/resolved": true, // v0.8.16 resolve endpoint + sweeper publishes
}

// eventDrivenSystemChannelNames returns the deterministic list for
// error messages.
func eventDrivenSystemChannelNames() []string {
	out := make([]string, 0, len(eventDrivenSystemChannels))
	for n := range eventDrivenSystemChannels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// validEvaluationScopes is the closed set of Evaluation-tool scope
// strings. See AgentDef.EvaluationScopes docstring for the meaning
// of each.
var validEvaluationScopes = map[string]bool{
	"submit_self":        true,
	"submit_siblings":    true,
	"submit_descendants": true,
	"submit_any":         true,
	"read_any":           true,
}

// validHistoryScopes is the closed set of Context.history scope
// strings. "self" / "siblings" / "descendants" / "named:<n>" /
// "any". The non-"self"/"any" entries are RESERVED in v0.8.7 PR 3
// — granting them is well-formed but the runtime falls through to
// default-deny until the plumbing PR lands.
var validHistoryScopes = map[string]bool{
	"self":        true,
	"siblings":    true,
	"descendants": true,
	"any":         true,
	// "named:<n>" handled by prefix check at validation time.
}

// validateAgentDefScope checks one entry in an agent's
// agent_def_scopes list. Closed set:
//
//   - "self"
//   - "descendants"
//   - "any"
//   - "named:<name>" where <name> is non-empty
//
// The "named:" prefix is the only stringly-typed exception — keeps
// the yaml authoring ergonomic. Empty name in "named:" is rejected
// at config-load.
func validateAgentDefScope(sc string) error {
	switch sc {
	case "self", "descendants", "any":
		return nil
	}
	if strings.HasPrefix(sc, "named:") {
		ref := strings.TrimPrefix(sc, "named:")
		if ref == "" {
			return fmt.Errorf("agent_def_scopes: \"named:\" requires a non-empty name (e.g. \"named:coder\")")
		}
		return nil
	}
	return fmt.Errorf("unknown scope %q (want one of: self, descendants, any, or \"named:<name>\")", sc)
}

// validateSkillDefScope mirrors validateAgentDefScope minus "self"
// (skills have no agent identity).
func validateSkillDefScope(sc string) error {
	switch sc {
	case "descendants", "any":
		return nil
	}
	if strings.HasPrefix(sc, "named:") {
		ref := strings.TrimPrefix(sc, "named:")
		if ref == "" {
			return fmt.Errorf("skill_def_scopes: \"named:\" requires a non-empty skill name (e.g. \"named:karpathy-guidelines\")")
		}
		return nil
	}
	return fmt.Errorf("unknown scope %q (want one of: descendants, any, or \"named:<skill-name>\")", sc)
}

// validateVolumeDefScope checks one entry in an agent's
// volume_def_scopes list (RFC AH Phase 2a). Closed set, mirroring
// skill_def_scopes minus "descendants" (volumes have no lineage):
//
//   - "any"
//   - "named:<volume-name>" where <volume-name> is non-empty
func validateVolumeDefScope(sc string) error {
	if sc == "any" {
		return nil
	}
	if strings.HasPrefix(sc, "named:") {
		ref := strings.TrimPrefix(sc, "named:")
		if ref == "" {
			return fmt.Errorf("volume_def_scopes: \"named:\" requires a non-empty volume name (e.g. \"named:repo-a\")")
		}
		return nil
	}
	return fmt.Errorf("unknown scope %q (want \"any\" or \"named:<volume-name>\")", sc)
}

// validateAgentChannelEntry checks one publish/subscribe entry on
// an AgentDef.Channels list. Exact match → must reference a declared
// channel. Trailing "/*" wildcard → must match at least one declared
// channel's prefix. Wildcards mid-string are rejected so operators
// can't accidentally grant "*" access.
func validateAgentChannelEntry(declared map[string]Channel, entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("empty channel reference")
	}
	if strings.Contains(entry, "*") && !strings.HasSuffix(entry, "/*") {
		return fmt.Errorf("channel %q: wildcards must be a trailing \"/*\" suffix (no mid-string globs)", entry)
	}
	if strings.HasSuffix(entry, "/*") {
		prefix := strings.TrimSuffix(entry, "*") // keep trailing /
		for name := range declared {
			if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
				return nil
			}
		}
		return fmt.Errorf("channel %q: no declared channel matches the prefix", entry)
	}
	if _, ok := declared[entry]; !ok {
		return fmt.Errorf("channel %q: not in operator-declared channels:", entry)
	}
	return nil
}

// agentGateWarnings returns the non-fatal "tool present but its capability gate
// is unset" advisories for one agent (F21). Each named tool DEFAULT-DENIES every
// call when its gate is empty, so the agent runs but the tool silently refuses —
// easy to miss (it looks like the agent "chose" not to use the tool). Pure +
// deterministic order (Memory, Evaluation, Channel, Interruption) so it is
// unit-testable; the caller (validate) accumulates these onto Config.Warnings.
func agentGateWarnings(name string, a AgentDef) []string {
	has := func(tool string) bool {
		for _, t := range a.AllowedTools {
			if t == tool {
				return true
			}
		}
		return false
	}
	var w []string
	if has("Memory") && len(a.MemoryScopes) == 0 {
		w = append(w, fmt.Sprintf("agent %q: allowed_tools includes Memory but memory_scopes is empty — every Memory op will default-deny; add memory_scopes: [agent] and/or [user]", name))
	}
	if has("Evaluation") && len(a.EvaluationScopes) == 0 {
		w = append(w, fmt.Sprintf("agent %q: allowed_tools includes Evaluation but evaluation_scopes is empty — every Evaluation op will default-deny; add evaluation_scopes", name))
	}
	if has("Channel") && len(a.Channels.Publish) == 0 && len(a.Channels.Subscribe) == 0 {
		w = append(w, fmt.Sprintf("agent %q: allowed_tools includes Channel but channels.publish and channels.subscribe are both empty — every Channel op will default-deny", name))
	}
	if has("Interruption") && !a.Interruption.Enabled {
		w = append(w, fmt.Sprintf("agent %q: allowed_tools includes Interruption but interruption.enabled is false — every Interruption op will refuse; set interruption.enabled: true", name))
	}
	return w
}

// sqlMemConfigWarnings returns the non-fatal advisory when an agent is
// configured for RFC AA SQL Memory (Memory in allowed_tools + a non-empty
// sql_scopes) but the subsystem is disabled at the storage layer — the agent
// boots, but every sql_query/sql_exec refuses with "not enabled". Kept pure +
// separate from agentGateWarnings because it needs the storage flag, not just
// the AgentDef. The inverse (sql_scopes empty → default-deny) is enforced at
// runtime, not flagged here.
func sqlMemConfigWarnings(name string, a AgentDef, sqlMemEnabled bool) []string {
	has := func(tool string) bool {
		for _, t := range a.AllowedTools {
			if t == tool {
				return true
			}
		}
		return false
	}
	if has("Memory") && len(a.SqlScopes) > 0 && !sqlMemEnabled {
		return []string{fmt.Sprintf("agent %q: sql_scopes is set but storage.sqlmem_enabled is false — every sql_query/sql_exec will refuse; set LOOMCYCLE_SQLMEM_ENABLED=1 (or storage.sqlmem_enabled: true)", name)}
	}
	return nil
}

// validateStaticWebhook checks a static `webhooks:` entry's delivery target +
// auth.kind at config-load (F24). A mismatched delivery target (spawn with no
// agent, channel with no channel) means the webhook can NEVER fire — failing
// loud at boot is a better operator signal than a 404/500 at request time. The
// receiver normalizes an empty `delivery` to spawn and an empty `auth.kind` to
// hmac, so the empty cases validate as those. Secret RESOLVABILITY is a
// separate, non-fatal boot WARNING (the receiver's UnresolvableStaticSecrets).
func validateStaticWebhook(name string, w Webhook) error {
	switch w.Delivery {
	case "", "spawn":
		if w.Agent == "" {
			return fmt.Errorf("webhooks.%s: delivery=spawn requires `agent`", name)
		}
		if w.Channel != "" {
			return fmt.Errorf("webhooks.%s: delivery=spawn forbids `channel` (set agent, not channel)", name)
		}
	case "channel":
		if w.Channel == "" {
			return fmt.Errorf("webhooks.%s: delivery=channel requires `channel`", name)
		}
		if w.Agent != "" {
			return fmt.Errorf("webhooks.%s: delivery=channel forbids `agent` (set channel, not agent)", name)
		}
	default:
		return fmt.Errorf("webhooks.%s: unknown delivery %q (want spawn or channel)", name, w.Delivery)
	}
	switch strings.ToLower(strings.TrimSpace(w.Auth.Kind)) {
	case "", "hmac", "bearer", "none":
	default:
		return fmt.Errorf("webhooks.%s: unknown auth.kind %q (want hmac, bearer, or none)", name, w.Auth.Kind)
	}
	return nil
}

// validateTierCandidate checks one tier candidate at config-load. A candidate
// may name a models: alias as a bare model with an empty provider — the alias
// supplies the provider at resolve time (ExpandModelAlias) — so an empty
// provider is valid IFF the model is a defined alias. Otherwise the provider
// must be a known ID and the model non-empty. Without the alias carve-out an
// all-aliases tier list fails load with `unknown provider ""`.
func validateTierCandidate(cand TierCandidate, models map[string]ModelRef) error {
	if cand.Provider == "" {
		if _, ok := models[cand.Model]; !ok {
			return fmt.Errorf("empty provider and %q is not a model alias (define it under models: or set an explicit provider)", cand.Model)
		}
		return nil
	}
	if !validProviderIDs[cand.Provider] {
		return fmt.Errorf("unknown provider %q", cand.Provider)
	}
	if cand.Model == "" {
		return fmt.Errorf("model is required")
	}
	return nil
}

func validate(c *Config) error {
	if c.Concurrency.MaxConcurrentRuns < 1 {
		return fmt.Errorf("concurrency.max_concurrent_runs must be >= 1")
	}
	// Runtime-wide context-transform plugin chain (RFC Z). Validate names
	// loudly at load — a typo'd plugin name must fail startup, not silently
	// drop a (possibly security-critical) transform like redaction. The
	// authoritative registry lives in internal/contextplugin (config can't
	// import it — that would cycle); knownContextPluginNames mirrors it and
	// must be kept in sync as built-in plugins are added.
	for i, p := range c.ContextPlugins {
		if p.Name == "" {
			return fmt.Errorf("context_plugins[%d]: name is required", i)
		}
		if !knownContextPluginNames[p.Name] {
			return fmt.Errorf("context_plugins[%d]: unknown plugin %q (want one of: redact)", i, p.Name)
		}
	}
	if c.Concurrency.MaxQueueDepth < 0 {
		return fmt.Errorf("concurrency.max_queue_depth must be >= 0")
	}
	// Library-level provider priority — validate every entry is a
	// known provider name. Empty list is fine (resolver falls back
	// to its hardcoded default order).
	for i, p := range c.ProviderPriority {
		if !validProviderIDs[p] {
			return fmt.Errorf("provider_priority[%d]: unknown provider %q (want one of anthropic/openai/deepseek/gemini/ollama)", i, p)
		}
	}
	// Library-level tier definitions.
	for tierName, candidates := range c.Tiers {
		if !validTierNames[tierName] {
			return fmt.Errorf("tiers.%s: unknown tier (want one of low/middle/high)", tierName)
		}
		for i, cand := range candidates {
			if err := validateTierCandidate(cand, c.Models); err != nil {
				return fmt.Errorf("tiers.%s[%d]: %v", tierName, i, err)
			}
		}
	}
	// User-tier definitions (v0.8.2). When the map is populated, a
	// "default" entry is required so requests without a user_tier
	// field have a defined policy to fall through to. Each tier's
	// internal shape is validated with the same rules as the library
	// ProviderPriority + Tiers above — duplication is intentional so
	// the errors point at the offending user_tiers.<name> path rather
	// than a generic message.
	if len(c.UserTiers) > 0 {
		if _, ok := c.UserTiers["default"]; !ok {
			return fmt.Errorf(`user_tiers: a "default" entry is required when the user_tiers block is populated (covers requests without user_tier and back-compat with v0.7.x clients)`)
		}
		for tierName, ut := range c.UserTiers {
			if tierName == "" {
				return fmt.Errorf("user_tiers: empty tier name")
			}
			for i, p := range ut.ProviderPriority {
				if !validProviderIDs[p] {
					return fmt.Errorf("user_tiers.%s.provider_priority[%d]: unknown provider %q", tierName, i, p)
				}
			}
			for taskTier, candidates := range ut.Tiers {
				if !validTierNames[taskTier] {
					return fmt.Errorf("user_tiers.%s.tiers.%s: unknown tier (want one of low/middle/high)", tierName, taskTier)
				}
				for i, cand := range candidates {
					if err := validateTierCandidate(cand, c.Models); err != nil {
						return fmt.Errorf("user_tiers.%s.tiers.%s[%d]: %v", tierName, taskTier, i, err)
					}
				}
			}
			if ut.MaxFallbackAttempts < 0 {
				return fmt.Errorf("user_tiers.%s.max_fallback_attempts: must be >= 0 (0 = use default of 3)", tierName)
			}
			if ut.RateLimitCooldownMs < 0 {
				return fmt.Errorf("user_tiers.%s.rate_limit_cooldown_ms: must be >= 0 (0 = use resolver default of 30000ms)", tierName)
			}
		}
	}
	// Static memory_backends entries skip the MemoryBackendDef tool's
	// validateWebhookDef-equivalent, so structurally validate them at load.
	// Most important: a shared_key_with_prefix backend MUST carry a
	// {tenant_id} token in its prefix_pattern — an empty/token-less prefix
	// resolves to an empty key prefix and collapses every tenant into one
	// keyspace (cross-tenant read+write leak). resolveTenancy is the runtime
	// backstop, but failing loudly at boot is the better operator signal.
	for bname, mb := range c.MemoryBackends {
		if mb.TenancyStrategy.Kind == "shared_key_with_prefix" &&
			!strings.Contains(mb.TenancyStrategy.PrefixPattern, "{tenant_id}") {
			return fmt.Errorf("memory_backends.%s: tenancy_strategy.prefix_pattern %q must contain {tenant_id} for shared_key_with_prefix (an empty or token-less prefix collapses all tenants into one keyspace)", bname, mb.TenancyStrategy.PrefixPattern)
		}
	}
	// Static webhooks: a misconfigured delivery target can never fire (F24).
	for wname, wh := range c.Webhooks {
		if err := validateStaticWebhook(wname, wh); err != nil {
			return err
		}
	}
	// RFC AH: validate the top-level `volumes:` map. Each path must
	// already exist + be a directory (static volumes map existing
	// infrastructure; the runtime never creates them — a missing/non-dir
	// path is a config-load error surfaced here rather than a baffling
	// call-time failure).
	// At most one volume may be `default: true`. Mode must be ro/rw/empty.
	// Paths are resolved to absolute IN PLACE so the run-start binding
	// resolution gets a stable absolute Root regardless of process cwd.
	if err := validateVolumes(c); err != nil {
		return err
	}
	// RFC AO: validate the principals: block + build the resolved token→Principal
	// table (reads each token_env from the environment). A bad scope/env-name
	// fails load; an empty token_env makes that principal inert + warns.
	if err := resolvePrincipals(c); err != nil {
		return err
	}
	for name, agent := range c.Agents {
		// Tier-based resolution and explicit pin are mutually
		// exclusive — pinning a model and asking for a tier is
		// ambiguous, and silently picking one would surprise the
		// next reader of the yaml.
		hasPin := agent.Provider != "" || agent.Model != ""
		hasTier := agent.Tier != ""
		if hasPin && hasTier {
			return fmt.Errorf("agent %q: cannot set both explicit provider/model pin and tier (pick one)", name)
		}
		if !hasPin && !hasTier {
			// Back-compat path: agents without either fall back
			// to defaults.model — same as v0.5.x behaviour.
			if c.Defaults.Model == "" {
				return fmt.Errorf("agent %q: no model, no tier, and no defaults.model", name)
			}
		}
		if !validEffortLevels[agent.Effort] {
			return fmt.Errorf("agent %q: invalid effort %q (want one of low/medium/high or empty)", name, agent.Effort)
		}
		if err := agent.Sampling.Validate(); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
		if err := agent.Compaction.Validate(); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
		if hasTier && !validTierNames[agent.Tier] {
			return fmt.Errorf("agent %q: invalid tier %q (want one of low/middle/high)", name, agent.Tier)
		}
		// Per-agent provider override.
		for i, p := range agent.Providers {
			if !validProviderIDs[p] {
				return fmt.Errorf("agent %q: providers[%d]: unknown provider %q", name, i, p)
			}
		}
		// Per-agent tier-candidate override.
		for tierName, candidates := range agent.Models {
			if !validTierNames[tierName] {
				return fmt.Errorf("agent %q: models.%s: unknown tier", name, tierName)
			}
			for i, cand := range candidates {
				if err := validateTierCandidate(cand, c.Models); err != nil {
					return fmt.Errorf("agent %q: models.%s[%d]: %v", name, tierName, i, err)
				}
			}
		}
		// Memory tool: validate memory_scopes are known scope strings.
		// Empty memory_scopes is not an ERROR (it just means no Memory
		// access), but if the agent ALSO lists Memory in allowed_tools the
		// tool default-denies every call — a silent-ish footgun surfaced as a
		// boot warning below (F21). Non-empty must be a subset of {agent, user}.
		for i, sc := range agent.MemoryScopes {
			if !validMemoryScopes[sc] {
				return fmt.Errorf("agent %q: memory_scopes[%d]: unknown scope %q (want one of: agent, user)", name, i, sc)
			}
		}
		// RFC AA SQL Memory: validate sql_scopes are known scope strings.
		// Empty = no SQL access (default-deny, enforced at runtime, not here).
		// Non-empty must be a subset of {agent, user, run}.
		for i, sc := range agent.SqlScopes {
			if !validSqlScopes[sc] {
				return fmt.Errorf("agent %q: sql_scopes[%d]: unknown scope %q (want one of: agent, user, run)", name, i, sc)
			}
		}
		// RFC AH Phase 1: a per-agent `volumes` binding must reference a
		// declared top-level `volumes:` entry (mirrors how allowed_tools
		// validates against registered tools — operator-yaml is the floor,
		// the model can never enlarge its own filesystem access). An agent
		// that declares NO volumes is implicitly bound to [default] and so
		// needs no validation here.
		for i, vn := range agent.Volumes {
			if _, ok := c.Volumes[vn]; !ok {
				return fmt.Errorf("agent %q: volumes[%d]: unknown volume %q (declare it in the top-level volumes: map)", name, i, vn)
			}
		}
		// Non-fatal: "tool in allowed_tools but its capability gate is unset"
		// advisories (Memory/memory_scopes, Evaluation/evaluation_scopes,
		// Channel/channels, Interruption/interruption.enabled). Accumulated and
		// logged once at boot, never fatal — an operator may legitimately list a
		// tool they haven't gated yet.
		c.Warnings = append(c.Warnings, agentGateWarnings(name, agent)...)
		c.Warnings = append(c.Warnings, sqlMemConfigWarnings(name, agent, c.Storage.SqlMemEnabled)...)
		if agent.MemoryQuotaBytes < 0 {
			return fmt.Errorf("agent %q: memory_quota_bytes must be >= 0", name)
		}
		if agent.SqlQuotaBytes < 0 {
			return fmt.Errorf("agent %q: sql_quota_bytes must be >= 0", name)
		}
		// Memory backend routing (RFC I MR-3b): a static agent that names
		// a memory_backend must reference a declared memory_backends key
		// OR the built-in "inprocess"/"default" literals. We only validate
		// static-yaml→static-yaml references: an agent may legitimately
		// name a backend that exists only as a dynamic MemoryBackendDef
		// (created at runtime, absent at config load), so an unresolved
		// name is NOT a load error — the Memory tool degrades to the
		// operator-default backend at runtime (see memory.go backend()).
		if agent.MemoryBackend != "" &&
			agent.MemoryBackend != "inprocess" &&
			agent.MemoryBackend != "default" {
			if _, ok := c.MemoryBackends[agent.MemoryBackend]; !ok {
				// Lenient: only fail when the static map is non-empty and
				// the name is clearly a typo against it. When the operator
				// declares no static backends at all, assume the name
				// targets a dynamic Def and let the runtime fallback cover
				// a miss.
				if len(c.MemoryBackends) > 0 {
					return fmt.Errorf("agent %q: memory_backend %q is not a declared memory_backends entry (or \"inprocess\"/\"default\")", name, agent.MemoryBackend)
				}
			}
		}
		if agent.RetryAttempts != nil && *agent.RetryAttempts < 0 {
			return fmt.Errorf("agent %q: retry_attempts must be >= 0 (0 = explicitly disable retries; omit to use user_tier default)", name)
		}
		// Channel tool (v0.8.4): every entry in publish/subscribe must
		// reference a declared channel (exact match) OR be a "<prefix>/*"
		// wildcard whose prefix matches at least one declared channel.
		// Wildcard with no matches at config-load is rejected so an
		// operator typo doesn't silently disable an ACL.
		for i, ch := range agent.Channels.Publish {
			if err := validateAgentChannelEntry(c.Channels, ch); err != nil {
				return fmt.Errorf("agent %q: channels.publish[%d]: %w", name, i, err)
			}
		}
		for i, ch := range agent.Channels.Subscribe {
			if err := validateAgentChannelEntry(c.Channels, ch); err != nil {
				return fmt.Errorf("agent %q: channels.subscribe[%d]: %w", name, i, err)
			}
		}
		// AgentDef tool (v0.8.5): validate agent_def_scopes entries.
		// Closed set: "self" / "descendants" / "named:<name>" / "any".
		for i, sc := range agent.AgentDefScopes {
			if err := validateAgentDefScope(sc); err != nil {
				return fmt.Errorf("agent %q: agent_def_scopes[%d]: %w", name, i, err)
			}
		}
		// SkillDef tool (v0.8.22): validate skill_def_scopes entries.
		// Closed set: "descendants" / "named:<skill-name>" / "any".
		// No "self" — skills have no agent identity.
		for i, sc := range agent.SkillDefScopes {
			if err := validateSkillDefScope(sc); err != nil {
				return fmt.Errorf("agent %q: skill_def_scopes[%d]: %w", name, i, err)
			}
		}
		// VolumeDef tool (RFC AH Phase 2a): validate volume_def_scopes
		// entries. Closed set: "named:<volume-name>" / "any".
		for i, sc := range agent.VolumeDefScopes {
			if err := validateVolumeDefScope(sc); err != nil {
				return fmt.Errorf("agent %q: volume_def_scopes[%d]: %w", name, i, err)
			}
		}
		// Evaluation tool (v0.8.5): validate evaluation_scopes entries.
		// Closed set as documented on AgentDef.EvaluationScopes.
		for i, sc := range agent.EvaluationScopes {
			if !validEvaluationScopes[sc] {
				return fmt.Errorf("agent %q: evaluation_scopes[%d]: unknown scope %q (want one of: submit_self, submit_siblings, submit_descendants, submit_any, read_any)", name, i, sc)
			}
		}
		// Context.history (v0.8.7): validate history_scope entries.
		// Closed set as documented on AgentDef.HistoryScope. The
		// `named:<n>` prefix follows the same shape as agent_def_scopes.
		for i, sc := range agent.HistoryScope {
			if validHistoryScopes[sc] {
				continue
			}
			if strings.HasPrefix(sc, "named:") {
				if strings.TrimPrefix(sc, "named:") == "" {
					return fmt.Errorf("agent %q: history_scope[%d]: empty name after `named:` prefix", name, i)
				}
				continue
			}
			return fmt.Errorf("agent %q: history_scope[%d]: unknown scope %q (want one of: self, siblings, descendants, any, named:<n>)", name, i, sc)
		}
	}
	// Channel tool: validate the top-level `channels:` block.
	for name, ch := range c.Channels {
		if name == "" {
			return fmt.Errorf("channels: empty channel name")
		}
		if !validChannelScopes[ch.Scope] {
			return fmt.Errorf("channels.%s: unknown scope %q (want one of: agent, user, global)", name, ch.Scope)
		}
		if ch.DefaultTTL < 0 {
			return fmt.Errorf("channels.%s: default_ttl must be >= 0", name)
		}
		if ch.MaxMessages < 0 {
			return fmt.Errorf("channels.%s: max_messages must be >= 0", name)
		}
		if ch.Semantic != "" && !validChannelSemantics[ch.Semantic] {
			return fmt.Errorf("channels.%s: unknown semantic %q (want one of: queue, broadcast)", name, ch.Semantic)
		}
		// v0.8.6 system-channels validation.
		if ch.Publisher != "" && ch.Publisher != "system" {
			return fmt.Errorf("channels.%s: unknown publisher %q (want: \"\" or \"system\")", name, ch.Publisher)
		}
		periodDur, err := ch.PeriodDuration()
		if err != nil {
			return fmt.Errorf("channels.%s: invalid period %q: %w", name, ch.Period, err)
		}
		if ch.Period != "" && ch.Publisher != "system" {
			return fmt.Errorf("channels.%s: period is only valid on `publisher: system` channels", name)
		}
		if ch.Publisher == "system" && periodDur == 0 && !eventDrivenSystemChannels[name] {
			return fmt.Errorf("channels.%s: publisher: system requires a `period:` (cadence) or the channel name must be in the event-driven set (%v)", name, eventDrivenSystemChannelNames())
		}
		// `_system/` prefix is reserved — channels with this prefix
		// can only be operator-declared (we're inside the iteration
		// over the operator yaml, so any channel here IS declared);
		// agents still cannot publish to them regardless of Publisher
		// (enforced at tool layer).
	}
	for name, srv := range c.MCPServers {
		switch srv.Transport {
		case "stdio":
			if srv.Command == "" {
				return fmt.Errorf("mcp_servers.%s: stdio transport requires command", name)
			}
		case "http":
			if srv.URL == "" {
				return fmt.Errorf("mcp_servers.%s: http transport requires url", name)
			}
		default:
			return fmt.Errorf("mcp_servers.%s: unknown transport %q", name, srv.Transport)
		}
	}
	// v1.x RFC E scheduled_runs validation. Cron syntax parses,
	// agent name resolves (statically — substrate-active checks are
	// deferred to runtime because the substrate isn't loaded at
	// config-load time), on_complete kinds in closed set, env-allowlist
	// for from_env references.
	for name, sr := range c.ScheduledRuns {
		if name == "" {
			return fmt.Errorf("scheduled_runs: empty schedule name")
		}
		if sr.Agent == "" {
			return fmt.Errorf("scheduled_runs.%s: agent is required", name)
		}
		if _, ok := c.Agents[sr.Agent]; !ok {
			return fmt.Errorf("scheduled_runs.%s: agent %q not declared in cfg.Agents (substrate-only agents are resolved at runtime; declare a yaml stub if you want compile-time validation)", name, sr.Agent)
		}
		// Mutual exclusion: standalone-schedule vs template-with-tier-defaults.
		if sr.Schedule != "" && len(sr.UserTierSchedules) > 0 {
			return fmt.Errorf("scheduled_runs.%s: cannot set both schedule: and user_tier_schedules: (pick one — schedule for standalone, user_tier_schedules for template)", name)
		}
		// Validate cron expressions where present. robfig/cron/v3
		// ParseStandard is the canonical 5-field parser the sweeper
		// uses; we validate against the same.
		if sr.Schedule != "" {
			if _, err := cron.ParseStandard(sr.Schedule); err != nil {
				return fmt.Errorf("scheduled_runs.%s: invalid cron expression %q: %w", name, sr.Schedule, err)
			}
		}
		for tier, cronExpr := range sr.UserTierSchedules {
			if _, err := cron.ParseStandard(cronExpr); err != nil {
				return fmt.Errorf("scheduled_runs.%s: user_tier_schedules.%s: invalid cron expression %q: %w", name, tier, cronExpr, err)
			}
		}
		if sr.CatchUpMax < 0 {
			return fmt.Errorf("scheduled_runs.%s: catch_up_max must be >= 0", name)
		}
		// Validate on_complete kinds.
		for i, hook := range sr.OnComplete {
			switch hook.Kind {
			case "channel.publish":
				if hook.Channel == "" {
					return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: channel required for channel.publish", name, i)
				}
			case "mcp.call":
				if hook.Server == "" || hook.Tool == "" {
					return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: server + tool required for mcp.call", name, i)
				}
			case "memory.set":
				if hook.Scope == "" || hook.Key == "" {
					return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: scope + key required for memory.set", name, i)
				}
			default:
				return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: unknown kind %q (want: channel.publish | mcp.call | memory.set)", name, i, hook.Kind)
			}
		}
	}
	// v0.9.0 Vector Memory: validate the memory.embedder block when
	// set. Empty block = vector ops refuse with embedder_not_configured
	// at the tool layer (caught at first use, not boot). Set block
	// must have a known provider AND a model.
	if c.Memory.Embedder.Provider != "" || c.Memory.Embedder.Model != "" {
		if c.Memory.Embedder.Provider == "" {
			return fmt.Errorf("memory.embedder: provider is required when embedder block is set")
		}
		if c.Memory.Embedder.Model == "" {
			return fmt.Errorf("memory.embedder: model is required when embedder block is set")
		}
		known := providers.RegisteredEmbedders()
		seen := false
		for _, p := range known {
			if p == c.Memory.Embedder.Provider {
				seen = true
				break
			}
		}
		if !seen {
			return fmt.Errorf("memory.embedder.provider: unknown provider %q (known: %v)", c.Memory.Embedder.Provider, known)
		}
		if c.Memory.Embedder.TimeoutMs < 0 {
			return fmt.Errorf("memory.embedder.timeout_ms must be >= 0")
		}
		if c.Memory.Embedder.BatchSize < 0 {
			return fmt.Errorf("memory.embedder.batch_size must be >= 0")
		}
	}
	return nil
}

// replicaIDPattern duplicates internal/coord.replicaIDPattern. We
// can't import coord here because main.go composes config.Load + the
// coord backplane wiring — config has to validate independently. The
// two patterns must stay in sync; TestReplicaIDPatternsAreInSync in
// internal/coord/replica_store_test.go cross-checks them on a shared
// input corpus so any drift fails CI.
var replicaIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateReplicaID is the exported config-side validator (mirrors
// coord.ValidateReplicaID with the same accept/reject decisions but
// different error text). Exported so the drift-checking test in the
// coord package can call it without re-implementing the regex.
func ValidateReplicaID(id string) error {
	return validateReplicaID(id)
}

func validateReplicaID(id string) error {
	if !replicaIDPattern.MatchString(id) {
		return fmt.Errorf("must match [A-Za-z0-9][A-Za-z0-9_-]{0,63} (got %q)", id)
	}
	return nil
}
