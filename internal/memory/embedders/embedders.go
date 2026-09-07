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
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Build turns cfg.Memory.Embedder into a constructed providers.Embedder,
// resolving the endpoint + credential through the SAME path the chat-completion
// drivers use (providerbuild.ProviderEndpoint). Returns (nil, nil) when no
// embedder is configured — the Memory tool refuses vector ops with
// embedder_not_configured in that case.
//
// base_url precedence, highest first:
//
//	memory.embedder.base_url  >  providers.<id>.base_url  >  the per-id env default
//
// The middle level is the fix for a real outage: the embedder used to read
// cfg.Env.OllamaBaseURL ONLY, so an operator who repointed
// `providers: ollama-local: base_url:` at a new Ollama host moved chat and left
// the embedder on the env var. An embed failure warns rather than failing the
// write (the row is kept, with embedded:false + embed_warning), so a caller that
// does not read that field accumulates unembedded rows: memory stopped being
// semantic (108 fact rows, ~0 embeddings) while every write still reported 200,
// and the benchmark that surfaced it read as a bad extractor. YAML is now the
// config home for both.
//
// Per-embedder yaml knobs (timeout_ms, batch_size) override the env-var
// defaults when set; the env-var fallback gives operators a single-place
// override for many embedders without touching yaml.
func Build(cfg *config.Config) (providers.Embedder, error) {
	provider := cfg.Memory.Embedder.Provider
	if provider == "" {
		return nil, nil
	}

	var apiKey, baseURL string

	if provider == "anthropic" {
		// EXCEPTION — the only provider that does NOT share its chat account.
		// The `anthropic` embedder slot is a Voyage AI proxy (v0.10.2):
		// Anthropic has no native embeddings API and points users at Voyage.
		// The operator yaml stays `provider: anthropic` for ergonomics, but the
		// service is a different vendor, so it must inherit NEITHER half of the
		// `providers: anthropic:` entry — an Anthropic proxy base_url would
		// receive Voyage requests, and ANTHROPIC_API_KEY would be sent to
		// Voyage. Auth is the separate VOYAGE_API_KEY.
		apiKey = cfg.Env.VoyageAPIKey
		if apiKey == "" {
			log.Printf("memory.embedder: provider=anthropic uses Voyage AI; set VOYAGE_API_KEY or Embed() calls will fail at 401")
		}
	} else {
		// Every other embedder hits the same account and endpoint as its chat
		// twin, so it resolves through the chat path's own resolver rather than
		// a second copy of the switch that drifted from it once already.
		var keyEnvName string
		baseURL, apiKey, keyEnvName = providerbuild.ProviderEndpoint(cfg, provider)

		// "disabled" is the chat side's opt-out sentinel (providerEnabled), not
		// an endpoint — treat it as unset so the embedder falls back to the
		// driver default rather than dialing a host literally named "disabled".
		if baseURL == "disabled" {
			baseURL = ""
		}

		// The per-id env key stays the LAST resort, for a deployment that runs
		// with no `providers:` block (LOOMCYCLE_NO_DEFAULT_PROVIDERS=1) and so
		// declares no api_key_env. It cannot be dropped as dead code: the openai
		// embedder accepts an empty key at construction and only fails later at
		// 401, so losing this fallback would trade a working embedder for a
		// runtime auth error rather than a startup one.
		if apiKey == "" && keyEnvName == "" {
			switch provider {
			case "openai":
				apiKey = cfg.Env.OpenAIAPIKey
			case "gemini":
				apiKey = cfg.Env.GeminiAPIKey
			case "ollama":
				apiKey = cfg.Env.OllamaAPIKey
			}
		}
	}

	// The embedder's OWN yaml block wins over everything resolved above —
	// that is the whole point of the two knobs, and it is what lets the
	// embedder diverge from its chat twin on purpose (a dedicated embedding
	// host, or a different key on the same vendor). base_url points any driver
	// at a self-hosted endpoint (Ollama, vLLM, LocalAI, an OpenAI-compatible
	// gateway); api_key_env names the operator's own env var (a NAME, resolved
	// here; the value never appears in yaml). An empty-valued var is not a
	// silent fallback to the provider key: naming it is an explicit choice, so
	// it wins either way.
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
