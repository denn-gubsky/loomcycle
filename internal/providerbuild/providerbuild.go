// Package providerbuild turns a loaded config's `providers:` block into
// constructed LLM providers.Provider instances.
//
// WHY IT IS NOT IN cmd/loomcycle: the server is no longer the only caller. The
// memory extraction eval (`loomcycle memory-eval-live`) has to score a model
// through the SAME construction the running server uses, and a second copy of
// the per-provider base-URL / auth / options switch would drift. The embedder
// side already learned that lesson twice — see internal/memory/embedders, and
// internal/cli.loadLayeredConfig before it.
//
// For this harness the drift would not be cosmetic. `ollama-local` takes its
// context window from LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX via Options["num_ctx"], and
// Ollama SILENTLY truncates the prompt to 4096 tokens when it is unset. A harness
// that built its own driver and forgot that knob would score a model on a
// transcript it never received — the exact class of failure the eval exists to
// catch, in a new costume.
//
// NOTE: the driver registry is populated by the blank imports in
// cmd/loomcycle/main.go, so Provider only resolves in a binary that links them.
// A test in another package must import the driver it needs (or register a fake).
package providerbuild

import (
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Provider constructs the single provider registered under id. Returns an error
// when id is not declared in cfg.Providers, so a caller naming a provider the
// deployment does not have is told which ids exist rather than getting a nil.
//
// It does NOT apply the server's providerEnabled gate: that gate is about which
// providers a RUNNING deployment routes to, and a caller here has named one
// explicitly. A provider whose key is absent still fails at construction (the
// driver factories reject a missing API key), so the operator gets a clear error
// either way rather than a silent nil.
func Provider(cfg *config.Config, id string) (providers.Provider, error) {
	pc, ok := cfg.Providers[id]
	if !ok {
		known := make([]string, 0, len(cfg.Providers))
		for k := range cfg.Providers {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("provider %q is not declared in this config (declared: %v)", id, known)
	}
	p, err := providers.NewDriver(pc.Driver, DriverOptions(id, pc, cfg))
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", id, err)
	}
	return p, nil
}

// DriverOptions ports the pre-P2a per-provider construction 1:1 into the driver
// factory input so an absent `providers:` block is byte-identical. BaseURL:
// explicit config wins, else the per-id env/driver default (incl. the
// OLLAMA_*_BASE_URL / DEEPSEEK_BASE_URL / GEMINI_BASE_URL defaults). APIKey +
// KeyEnvName come from api_key_env (RFC AR per-tenant override). StreamOpts +
// Options carry the per-provider timeouts and ollama num_ctx/num_gpu.
func DriverOptions(id string, pc config.ProviderConfig, cfg *config.Config) providers.DriverOptions {
	baseURL := pc.BaseURL
	stream := streamhttp.Options{
		HeaderTimeout: cfg.Env.ProviderHeaderTimeout,
		IdleTimeout:   cfg.Env.ProviderIdleTimeout,
	}
	opts := map[string]any{}
	switch id {
	case "ollama": // hosted ollama.com
		if baseURL == "" {
			baseURL = cfg.Env.OllamaCloudBaseURL
		}
		if cfg.Env.OllamaNumCtx > 0 {
			opts["num_ctx"] = cfg.Env.OllamaNumCtx
		}
	case "ollama-local":
		if baseURL == "" {
			baseURL = cfg.Env.OllamaBaseURL
		}
		// Local Ollama is slow on first-token (cold model load + large-context
		// eval), so it gets its own, more generous timeout pair.
		stream = streamhttp.Options{
			HeaderTimeout: cfg.Env.OllamaLocalHeaderTimeout,
			IdleTimeout:   cfg.Env.OllamaLocalIdleTimeout,
		}
		if cfg.Env.OllamaLocalNumCtx > 0 {
			opts["num_ctx"] = cfg.Env.OllamaLocalNumCtx
		}
		if cfg.Env.OllamaLocalNumGpu > 0 {
			opts["num_gpu"] = cfg.Env.OllamaLocalNumGpu
		}
	case "deepseek":
		if baseURL == "" {
			baseURL = cfg.Env.DeepSeekBaseURL
		}
	case "gemini":
		if baseURL == "" {
			baseURL = cfg.Env.GeminiBaseURL
		}
	case "code-js":
		// RFC BF P2a regression fix (b8d3f42d line): the code-js driver factory
		// (codejs.newFromOptions) sources code_root/deterministic/run_timeout_seconds
		// from this options map — but P2a's flip to the registry never ported the
		// pre-P2a codejs.New(Config{CodeRoot: cfg.Env.CodeAgentsRoot, ...}) mapping,
		// so with no `providers: code-js: options:` block the compiler root was empty
		// and EVERY static `provider: code-js` agent failed to load ("no index.js at
		// <name>/index.js" — a relative path, the empty-root tell). CodeAgentsRoot
		// defaults to ./agent_code (never empty), so this is byte-identical to
		// pre-P2a; an explicit options entry still wins via the pc.Options merge below.
		opts["code_root"] = cfg.Env.CodeAgentsRoot
		opts["deterministic"] = cfg.Env.CodeAgentsDeterministic
		opts["run_timeout_seconds"] = int(cfg.Env.CodeAgentsRunTimeout / time.Second)
	}
	// Operator-declared options override the env-derived defaults (e.g. mock-stable's
	// `stable: true`, or a per-provider num_ctx).
	for k, v := range pc.Options {
		opts[k] = v
	}
	// RFC CK: per-provider stream timeouts are settable via `options:`
	// (header_timeout_ms / idle_timeout_ms) so a `local` preset/bundle can tune a
	// slow local backend's cold-load window in YAML — previously env-only. These
	// are CONSUMED here (folded into StreamOpts + deleted) so they never reach the
	// driver's Options map, which would otherwise WarnUnknownOptions on them. A
	// YAML value overrides the env/global default; an absent/invalid entry leaves
	// the env-derived timeout in place.
	if ms, ok := optionMillis(opts, "header_timeout_ms"); ok {
		stream.HeaderTimeout = ms
		delete(opts, "header_timeout_ms")
	}
	if ms, ok := optionMillis(opts, "idle_timeout_ms"); ok {
		stream.IdleTimeout = ms
		delete(opts, "idle_timeout_ms")
	}
	return providers.DriverOptions{
		ID:           id,
		Dialect:      pc.Dialect,
		BaseURL:      baseURL,
		APIKey:       os.Getenv(pc.APIKeyEnv),
		KeyEnvName:   pc.APIKeyEnv,
		StreamOpts:   stream,
		Options:      opts,
		Capabilities: CapabilityPatch(pc.Capabilities),
		Logf:         log.Printf,
	}
}

// optionMillis coerces an `options:` value expressed in milliseconds (a YAML/JSON
// number, which round-trips through config layering as int / int64 / float64) into
// a time.Duration. Returns false for a missing key, a non-numeric value, or a
// non-positive one — so a bad or absent entry falls back to the env/global default
// rather than silently installing a zero (i.e. immediate-timeout) client.
func optionMillis(opts map[string]any, key string) (time.Duration, bool) {
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	var ms int64
	switch n := v.(type) {
	case int:
		ms = int64(n)
	case int64:
		ms = n
	case float64:
		ms = int64(n)
	default:
		return 0, false
	}
	if ms <= 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// CapabilityPatch translates a config.CapabilityOverride (the `capabilities:`
// block on an RFC BF `providers:` entry) into the providers-side
// providers.CapabilityPatch the driver registry applies inside Capabilities().
// The providers package can't import config (it would need config.CapabilityOverride,
// and providers is a dependency of config), so this boundary translation lives
// here at the composition root and DriverOptions feeds the result to every
// driver factory. Nil in → nil out (no override → advertise driver defaults).
func CapabilityPatch(o *config.CapabilityOverride) *providers.CapabilityPatch {
	if o == nil {
		return nil
	}
	return &providers.CapabilityPatch{
		SupportsThinking:  o.SupportsThinking,
		SupportsVision:    o.SupportsVision,
		SupportsEffort:    o.SupportsEffort,
		NativePromptCache: o.NativePromptCache,
		ParallelToolCalls: o.ParallelToolCalls,
		MaxContextTokens:  o.MaxContextTokens,
	}
}
