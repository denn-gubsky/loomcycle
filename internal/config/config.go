// Package config loads loomcycle.yaml + env vars and validates them.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML structure plus env-derived fields.
type Config struct {
	Defaults    Defaults             `yaml:"defaults"`
	Models      map[string]ModelRef  `yaml:"models"`
	Agents      map[string]AgentDef  `yaml:"agents"`
	MCPServers  map[string]MCPServer `yaml:"mcp_servers"`
	Concurrency Concurrency          `yaml:"concurrency"`
	Cache       CacheConfig          `yaml:"cache"`

	// Env-derived; not in YAML.
	Env Env `yaml:"-"`
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

// AgentDef is one agent the API can address by name.
type AgentDef struct {
	Provider     string   `yaml:"provider"` // optional override of Defaults
	Model        string   `yaml:"model"`    // alias or full model ID
	SystemPrompt string   `yaml:"system_prompt"`
	AllowedTools []string `yaml:"allowed_tools"`
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
	HTTPHostAllowlist []string
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
	}

	cfg.Env = Env{
		AnthropicAPIKey:   os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIAPIKey:      os.Getenv("OPENAI_API_KEY"),
		OllamaBaseURL:     getenvDefault("OLLAMA_BASE_URL", "http://localhost:11434"),
		ListenAddr:        getenvDefault("LOOMCYCLE_LISTEN_ADDR", "127.0.0.1:8787"),
		AuthToken:         os.Getenv("LOOMCYCLE_AUTH_TOKEN"),
		DataDir:           getenvDefault("LOOMCYCLE_DATA_DIR", "./data"),
		ReadRoot:          os.Getenv("LOOMCYCLE_READ_ROOT"),
		WriteRoot:         os.Getenv("LOOMCYCLE_WRITE_ROOT"),
		HTTPHostAllowlist: splitCSV(os.Getenv("LOOMCYCLE_HTTP_HOST_ALLOWLIST")),
		BraveAPIKey:       os.Getenv("BRAVE_API_KEY"),
		BashEnabled:       os.Getenv("LOOMCYCLE_BASH_ENABLED") == "1",
		BashCwd:           os.Getenv("LOOMCYCLE_BASH_CWD"),
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

func validate(c *Config) error {
	if c.Concurrency.MaxConcurrentRuns < 1 {
		return fmt.Errorf("concurrency.max_concurrent_runs must be >= 1")
	}
	if c.Concurrency.MaxQueueDepth < 0 {
		return fmt.Errorf("concurrency.max_queue_depth must be >= 0")
	}
	for name, agent := range c.Agents {
		if agent.Model == "" && c.Defaults.Model == "" {
			return fmt.Errorf("agent %q: no model and no defaults.model", name)
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
