// Package embedders turns a loaded config's `memory.embedder` block into a
// constructed providers.Embedder.
//
// WHY IT IS NOT IN cmd/loomcycle: the server is no longer the only caller.
// Operator CLI subcommands (memory-calibrate) need the SAME embedder the
// running server builds, and a second copy of the per-provider auth/base-URL
// switch would drift — the CLI already learned that lesson once, when it
// loaded a single config file while the server layered presets and reported a
// false "no provider resolved" (see internal/cli.loadLayeredConfig).
//
// NOTE: the driver registry is populated by the blank imports in
// cmd/loomcycle/main.go, so Build only resolves a provider in a binary that
// links them. A test in another package must import the driver it needs (or
// register a fake) exactly as before.
package embedders

import (
	"log"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Build turns cfg.Memory.Embedder into a constructed providers.Embedder,
// sourcing the API key + base URL from the same env vars the chat-completion
// drivers use. Returns (nil, nil) when no embedder is configured — the Memory
// tool refuses vector ops with embedder_not_configured in that case.
//
// Per-embedder yaml knobs (timeout_ms, batch_size) override the env-var
// defaults when set; the env-var fallback gives operators a single-place
// override for many embedders without touching yaml.
func Build(cfg *config.Config) (providers.Embedder, error) {
	provider := cfg.Memory.Embedder.Provider
	if provider == "" {
		return nil, nil
	}

	// Reuse the chat-completion driver's auth + base URL. Embedders
	// hit the same provider account, so a separate set of env vars
	// would be operator friction without benefit.
	//
	// EXCEPTION: the `anthropic` embedder slot is a Voyage AI proxy
	// (v0.10.2) — Anthropic has no native embeddings API and points
	// users at Voyage. The operator yaml stays `provider: anthropic`
	// for ergonomics, but the underlying auth is the separate
	// VOYAGE_API_KEY env var routed to cfg.Env.VoyageAPIKey.
	//
	// Providers with no case here (`stub`) fall through with both empty:
	// the driver's own default endpoint, keyless.
	var apiKey, baseURL string
	switch provider {
	case "openai":
		apiKey, baseURL = cfg.Env.OpenAIAPIKey, ""
	case "gemini":
		apiKey, baseURL = cfg.Env.GeminiAPIKey, cfg.Env.GeminiBaseURL
	case "anthropic":
		apiKey, baseURL = cfg.Env.VoyageAPIKey, ""
		if apiKey == "" {
			log.Printf("memory.embedder: provider=anthropic uses Voyage AI; set VOYAGE_API_KEY or Embed() calls will fail at 401")
		}
	case "ollama-local":
		// Inherits the SAME OLLAMA_BASE_URL the chat driver uses, so one
		// setting serves both. "disabled" is the chat side's opt-out
		// sentinel (providerEnabled), not an endpoint — treat it as unset
		// so the embedder falls back to the driver default rather than
		// trying to dial a host literally named "disabled". Keyless.
		if cfg.Env.OllamaBaseURL != "disabled" {
			baseURL = cfg.Env.OllamaBaseURL
		}
	case "ollama":
		// Hosted ollama.com — same pair the chat provider reads.
		apiKey, baseURL = cfg.Env.OllamaAPIKey, cfg.Env.OllamaCloudBaseURL
	}

	// The operator's explicit yaml WINS over the per-provider defaults
	// above — that is the whole point of the two knobs. base_url points
	// any driver at a self-hosted endpoint (Ollama, vLLM, LocalAI, an
	// OpenAI-compatible gateway); api_key_env re-points the credential
	// at the operator's own env var, mirroring the `providers:` map
	// convention (a NAME, resolved here; the value never appears in
	// yaml). An empty-valued var is not a silent fallback to the host
	// key: naming it is an explicit choice, so it wins either way.
	if cfg.Memory.Embedder.BaseURL != "" {
		baseURL = cfg.Memory.Embedder.BaseURL
	}
	if cfg.Memory.Embedder.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Memory.Embedder.APIKeyEnv)
		if apiKey == "" {
			log.Printf("memory.embedder: api_key_env=%s is set but empty — the embedder will call %s unauthenticated",
				cfg.Memory.Embedder.APIKeyEnv, provider)
		}
	}

	timeoutMs := cfg.Memory.Embedder.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = cfg.Env.MemoryEmbedTimeoutMs
	}
	batchSize := cfg.Memory.Embedder.BatchSize
	if batchSize == 0 {
		batchSize = cfg.Env.MemoryEmbedBatchSize
	}

	return providers.NewEmbedder(provider, providers.EmbedderOptions{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		Model:      cfg.Memory.Embedder.Model,
		Dimensions: cfg.Memory.Embedder.Dimensions,
		Timeout:    time.Duration(timeoutMs) * time.Millisecond,
		BatchSize:  batchSize,
	})
}
