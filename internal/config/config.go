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

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// Config is the top-level YAML structure plus env-derived fields.
type Config struct {
	Defaults    Defaults             `yaml:"defaults"`
	Models      map[string]ModelRef  `yaml:"models"`
	Agents      map[string]AgentDef  `yaml:"agents"`
	MCPServers  map[string]MCPServer `yaml:"mcp_servers"`
	Concurrency Concurrency          `yaml:"concurrency"`
	Cache       CacheConfig          `yaml:"cache"`
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

	// Env-derived; not in YAML.
	Env Env `yaml:"-"`

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
}

// ConfigDir returns the directory the YAML was loaded from. Used by
// callers that need to resolve relative paths declared in the config
// (the local-api spec path, additional resource files).
func (c *Config) ConfigDir() string { return c.configDir }

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
type TierCandidate struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
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
}

// AgentDef is one agent the API can address by name.
type AgentDef struct {
	Provider string `yaml:"provider"` // optional override of Defaults
	Model    string `yaml:"model"`    // alias or full model ID
	// SystemPrompt is the agent's system prompt as an inline YAML
	// string. Mutually exclusive with SystemPromptFile.
	SystemPrompt string `yaml:"system_prompt"`
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
type Channel struct {
	Scope       string `yaml:"scope"`
	DefaultTTL  int    `yaml:"default_ttl"`
	MaxMessages int    `yaml:"max_messages"`
	Semantic    string `yaml:"semantic"`

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
	Publish   []string `yaml:"publish"`
	Subscribe []string `yaml:"subscribe"`
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
	AuthToken     string
	DataDir       string
	// ReadRoot is the sandbox root for the built-in Read tool. Empty by
	// default — the tool is registered but rejects every call until set.
	ReadRoot string
	// WriteRoot is the sandbox root for both Write and Edit. Empty by
	// default — both tools refuse every call until set. Same TOCTOU
	// guarantees as ReadRoot.
	WriteRoot string
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
	// HTTPCallerAuthoritative flips the per-request allowed_hosts
	// trust model: when true, caller's list is the sole policy
	// (operator's HTTPHostAllowlist becomes a fallback for runs that
	// don't carry their own list). When false (default), caller can
	// only narrow operator's list, never widen. Operator opts in once
	// via LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1.
	HTTPCallerAuthoritative bool
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
	// BashCwd is the working directory for spawned bash commands. Required
	// when BashEnabled is true; if unset the tool refuses every call.
	BashCwd string
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
}

// Load reads a YAML file and the process env. Empty path returns defaults +
// env only. Returns error on YAML parse failure or missing-required-field.
func Load(path string) (*Config, error) {
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

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		expanded := expandEnv(string(raw))
		if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		// Stash the config directory so callers (e.g. localapi.Build)
		// can resolve relative paths declared inside the YAML.
		if abs, err := filepath.Abs(filepath.Dir(path)); err == nil {
			cfg.configDir = abs
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
	if err := resolveSystemPromptFiles(cfg, path); err != nil {
		return nil, err
	}

	cfg.Env = Env{
		AnthropicAPIKey:          os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIAPIKey:             os.Getenv("OPENAI_API_KEY"),
		OllamaBaseURL:            getenvDefault("OLLAMA_BASE_URL", "http://localhost:11434"),
		OllamaAPIKey:             os.Getenv("OLLAMA_API_KEY"),
		OllamaCloudBaseURL:       getenvDefault("OLLAMA_CLOUD_BASE_URL", "https://ollama.com"),
		DeepSeekAPIKey:           os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekBaseURL:          os.Getenv("DEEPSEEK_BASE_URL"),
		GeminiAPIKey:             os.Getenv("GEMINI_API_KEY"),
		GeminiBaseURL:            os.Getenv("GEMINI_BASE_URL"),
		ListenAddr:               getenvDefault("LOOMCYCLE_LISTEN_ADDR", "127.0.0.1:8787"),
		AuthToken:                os.Getenv("LOOMCYCLE_AUTH_TOKEN"),
		DataDir:                  getenvDefault("LOOMCYCLE_DATA_DIR", "./data"),
		ReadRoot:                 os.Getenv("LOOMCYCLE_READ_ROOT"),
		WriteRoot:                os.Getenv("LOOMCYCLE_WRITE_ROOT"),
		HTTPHostAllowlist:        splitCSV(os.Getenv("LOOMCYCLE_HTTP_HOST_ALLOWLIST")),
		HTTPPrivateHostAllowlist: splitCSV(os.Getenv("LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST")),
		HTTPCallerAuthoritative:  os.Getenv("LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE") == "1",
		BraveAPIKey:              os.Getenv("BRAVE_API_KEY"),
		BashEnabled:              os.Getenv("LOOMCYCLE_BASH_ENABLED") == "1",
		BashCwd:                  os.Getenv("LOOMCYCLE_BASH_CWD"),
		SkillsRoot:               os.Getenv("LOOMCYCLE_SKILLS_ROOT"),
		AgentsRoot:               os.Getenv("LOOMCYCLE_AGENTS_ROOT"),
		// Sweeper / GC defaults — populated above zero only if the
		// env var below was set. The fallbacks are applied in
		// cmd/loomcycle/main.go where the goroutines are started.
		HeartbeatSweeperEnabled: true,
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

	// resolveSkills MUST come after env loading (it needs SkillsRoot)
	// AND after resolveSystemPromptFiles (skill bodies append onto
	// SystemPrompt — file-loaded prompts have to land first).
	if err := resolveSkills(cfg); err != nil {
		return nil, err
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
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

// ResolveAgentDefModel mirrors ResolveAgentModel but resolves against
// a caller-supplied AgentDef instead of looking it up in c.Agents.
// Used by the sub-agent path when an overlay has already produced an
// effective def whose Provider/Model differ from the static yaml.
// Same alias-expansion + defaults-fallback rules as ResolveAgentModel.
func (c *Config) ResolveAgentDefModel(agent string, def AgentDef) (provider string, model string, err error) {
	model = def.Model
	provider = def.Provider

	// If model is an alias in models:, expand it.
	if ref, ok := c.Models[model]; ok {
		model = ref.Model
		if provider == "" {
			provider = ref.Provider
		}
	}
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
// names match expandEnvAllowed. Other ${VAR} tokens pass through verbatim.
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
		if !expandEnvAllowed(name) {
			return m // leave verbatim — caller sees the literal ${...}
		}
		return os.Getenv(name)
	})
}

// expandEnvAllowed reports whether the given env-var name may be expanded
// inside YAML. Allowlist:
//   - any LOOMCYCLE_-prefixed variable (the project's own namespace)
//   - well-known third-party keys MCP servers commonly need
func expandEnvAllowed(name string) bool {
	if strings.HasPrefix(name, "LOOMCYCLE_") {
		return true
	}
	switch name {
	case "BRAVE_API_KEY",
		"GITHUB_TOKEN",
		"SLACK_BOT_TOKEN",
		"PG_DSN",
		"REDIS_URL":
		return true
	}
	return false
}

func getenvDefault(name, dflt string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return dflt
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
		SystemPrompt:     d.SystemPrompt,
		SystemPromptFile: d.SystemPromptFile,
		AllowedTools:     d.AllowedTools,
		Skills:           d.Skills,
		MaxTokens:        d.MaxTokens,
		Tier:             d.Tier,
		Effort:           d.Effort,
		Providers:        d.Providers,
		MemoryScopes:     d.MemoryScopes,
		MemoryQuotaBytes: d.MemoryQuotaBytes,
		Channels: AgentChannelACL{
			Publish:   d.Channels.Publish,
			Subscribe: d.Channels.Subscribe,
		},
		AgentDefScopes:   d.AgentDefScopes,
		EvaluationScopes: d.EvaluationScopes,
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
	if override.Channels.Publish != nil {
		out.Channels.Publish = override.Channels.Publish
	}
	if override.Channels.Subscribe != nil {
		out.Channels.Subscribe = override.Channels.Subscribe
	}
	if override.AgentDefScopes != nil {
		out.AgentDefScopes = override.AgentDefScopes
	}
	if override.EvaluationScopes != nil {
		out.EvaluationScopes = override.EvaluationScopes
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

// resolveSkills bundles skill bodies into agent system prompts and
// validates each skill's allowed-tools is a subset of the bundling
// agent's allowed_tools. Static bundling — see Approach A in
// doc-internal/skills-design.md.
//
// Errors:
//   - SkillsRoot unset but an agent lists `skills:` (silent drop would
//     produce agents whose prompts reference skills the runtime never
//     loaded; loud failure forces the operator to fix the config)
//   - skills root unreadable / not a directory
//   - agent references an unknown skill name
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
	// Fast-fail when skills root is unset but agents list skills. We
	// could no-op silently, but that produces agents whose prompts
	// reference skills the runtime never bundled — exactly the failure
	// mode this whole feature was added to fix.
	if cfg.Env.SkillsRoot == "" {
		for name, def := range cfg.Agents {
			if len(def.Skills) > 0 {
				return fmt.Errorf("agent %q: lists skills %v but LOOMCYCLE_SKILLS_ROOT is not set", name, def.Skills)
			}
		}
		return nil
	}
	set, err := skills.LoadSet(cfg.Env.SkillsRoot)
	if err != nil {
		return fmt.Errorf("load skills: %w", err)
	}
	for name, def := range cfg.Agents {
		if len(def.Skills) == 0 {
			continue
		}
		// Build agent rule set once per agent.
		agentSet := make(map[string]bool, len(def.AllowedTools))
		for _, t := range def.AllowedTools {
			agentSet[t] = true
		}
		for _, skillName := range def.Skills {
			sk, ok := set.Get(skillName)
			if !ok {
				return fmt.Errorf("agent %q: unknown skill %q (skills root: %s)", name, skillName, cfg.Env.SkillsRoot)
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
	"_system/runtime-state":   true, // v0.8.9 pause/resume/restore
	"_system/provider-events": true, // provider fallback / cache-invalidated
	// v0.8.8 Question tool channels are agent-published (not
	// publisher: system) so they don't appear here.
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

func validate(c *Config) error {
	if c.Concurrency.MaxConcurrentRuns < 1 {
		return fmt.Errorf("concurrency.max_concurrent_runs must be >= 1")
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
			if !validProviderIDs[cand.Provider] {
				return fmt.Errorf("tiers.%s[%d]: unknown provider %q", tierName, i, cand.Provider)
			}
			if cand.Model == "" {
				return fmt.Errorf("tiers.%s[%d]: model is required", tierName, i)
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
					if !validProviderIDs[cand.Provider] {
						return fmt.Errorf("user_tiers.%s.tiers.%s[%d]: unknown provider %q", tierName, taskTier, i, cand.Provider)
					}
					if cand.Model == "" {
						return fmt.Errorf("user_tiers.%s.tiers.%s[%d]: model is required", tierName, taskTier, i)
					}
				}
			}
			if ut.MaxFallbackAttempts < 0 {
				return fmt.Errorf("user_tiers.%s.max_fallback_attempts: must be >= 0 (0 = use default of 3)", tierName)
			}
		}
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
				if !validProviderIDs[cand.Provider] {
					return fmt.Errorf("agent %q: models.%s[%d]: unknown provider %q", name, tierName, i, cand.Provider)
				}
				if cand.Model == "" {
					return fmt.Errorf("agent %q: models.%s[%d]: model is required", name, tierName, i)
				}
			}
		}
		// Memory tool: validate memory_scopes are known scope strings.
		// Empty memory_scopes is fine (it just means no Memory access);
		// non-empty must be a subset of {agent, user} for v0.8.0.
		for i, sc := range agent.MemoryScopes {
			if !validMemoryScopes[sc] {
				return fmt.Errorf("agent %q: memory_scopes[%d]: unknown scope %q (want one of: agent, user)", name, i, sc)
			}
		}
		if agent.MemoryQuotaBytes < 0 {
			return fmt.Errorf("agent %q: memory_quota_bytes must be >= 0", name)
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
		// Evaluation tool (v0.8.5): validate evaluation_scopes entries.
		// Closed set as documented on AgentDef.EvaluationScopes.
		for i, sc := range agent.EvaluationScopes {
			if !validEvaluationScopes[sc] {
				return fmt.Errorf("agent %q: evaluation_scopes[%d]: unknown scope %q (want one of: submit_self, submit_siblings, submit_descendants, submit_any, read_any)", name, i, sc)
			}
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
	return nil
}
