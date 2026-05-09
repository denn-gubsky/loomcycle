// Package config loads loomcycle.yaml + env vars and validates them.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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
	OllamaBaseURL   string
	// DeepSeekAPIKey enables the `provider: deepseek` driver. Empty
	// = provider not registered (agents that ask for it fail at
	// resolve time, mirroring OpenAI / Anthropic behaviour).
	DeepSeekAPIKey string
	// DeepSeekBaseURL overrides the public DeepSeek endpoint
	// (https://api.deepseek.com/v1) for self-hosted OpenAI-
	// compatible mirrors (e.g. an internal vLLM serving a DeepSeek
	// model). Empty = use the public endpoint.
	DeepSeekBaseURL string
	ListenAddr      string
	AuthToken       string
	DataDir         string
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
		// Resolve any agent's system_prompt_file → system_prompt. Done
		// here so the rest of the runtime sees a uniform AgentDef
		// regardless of which form the operator wrote.
		if err := resolveSystemPromptFiles(cfg, path); err != nil {
			return nil, err
		}
	}

	cfg.Env = Env{
		AnthropicAPIKey:          os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIAPIKey:             os.Getenv("OPENAI_API_KEY"),
		OllamaBaseURL:            getenvDefault("OLLAMA_BASE_URL", "http://localhost:11434"),
		DeepSeekAPIKey:           os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekBaseURL:          os.Getenv("DEEPSEEK_BASE_URL"),
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
	"anthropic": true,
	"openai":    true,
	"deepseek":  true,
	"ollama":    true,
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
			return fmt.Errorf("provider_priority[%d]: unknown provider %q (want one of anthropic/openai/deepseek/ollama)", i, p)
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
