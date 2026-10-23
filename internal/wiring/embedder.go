package wiring

import (
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Embedder turns cfg.Memory.Embedder into a constructed providers.Embedder,
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
func Embedder(cfg *config.Config) (providers.Embedder, error) {
	e, _, err := buildEmbedder(cfg)
	return e, err
}

// GatedEmbedder is Embedder for the running server: the embedder's calls take the
// embedder lane of the provider the block resolved to (sched, the server's call
// scheduler), so providers.<id>.lanes.embedder caps the embeddings in flight
// and the provider's max_models_in_flight counts the embedding model beside
// the chat models. Embedder stays ungated for a caller with no server scheduler
// (the CLI's memory commands).
//
// The lane is the PROVIDER's, whatever endpoint the block's own base_url points
// the embedder at: an operator who moved the embedder to a host of its own and
// wants it capped apart from chat sets the embedder lane, which no chat call
// takes.
func GatedEmbedder(cfg *config.Config, sched *concurrency.CallScheduler) (providers.Embedder, error) {
	e, provider, err := buildEmbedder(cfg)
	if err != nil {
		return nil, err
	}
	return lanegate.WrapEmbedder(provider, e, sched), nil
}

// buildEmbedder is Embedder plus the provider id the block resolved to ("" with no embedder).
func buildEmbedder(cfg *config.Config) (providers.Embedder, string, error) {
	if cfg.Memory.Embedder.Provider == "" && cfg.Memory.Embedder.Model == "" {
		return nil, "", nil
	}
	// The model may be a models: alias; resolve it (and the provider it carries,
	// when the block names none) the way an agent's model resolves.
	provider, model, err := cfg.ExpandServiceModel("memory.embedder", cfg.Memory.Embedder.Provider, cfg.Memory.Embedder.Model)
	if err != nil {
		return nil, "", err
	}
	if provider == "" {
		return nil, "", nil
	}
	// Which constructor serves the provider: the same function config
	// validation used, so the embedder built is the one the config was checked
	// against.
	key, err := cfg.EmbedderKey(provider)
	if err != nil {
		return nil, "", err
	}

	var apiKey, baseURL string
	var auth providers.RequestAuth

	if key == config.EmbedderAPIBedrock {
		// Bedrock InvokeModel, signed with the provider's own AWS credentials
		// (RFC EV). They may go to the provider's host only: the scheme and
		// host of its base_url, never its chat route (/anthropic,
		// /openai/v1), and never the block's own base_url or key (config
		// refuses both with api: bedrock, in EmbedderKey above).
		base, _, _ := providerbuild.ProviderEndpoint(cfg, provider)
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
			baseURL = u.Scheme + "://" + u.Host
		}
		auth = providerbuild.EmbedderAuth(cfg, provider)
	} else if key == "anthropic" {
		// EXCEPTION — the only provider that does NOT share its chat account.
		// The `anthropic` embedder slot is a Voyage AI proxy (v0.10.2):
		// Anthropic has no native embeddings API and points users at Voyage.
		// The operator yaml stays `provider: anthropic` for ergonomics, but the
		// service is a different vendor, so it must inherit NEITHER half of the
		// `providers: anthropic:` entry — an Anthropic proxy base_url would
		// receive Voyage requests, and ANTHROPIC_API_KEY would be sent to
		// Voyage. Auth is the separate VOYAGE_API_KEY. Keyed on the embedder
		// chosen, not the provider id, so `api: anthropic` over another provider
		// inherits nothing from that provider either.
		apiKey = cfg.Env.VoyageAPIKey
		if apiKey == "" {
			slog.Warn("memory.embedder: provider=anthropic uses Voyage AI; set VOYAGE_API_KEY or Embed() calls will fail at 401")
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
	if cfg.Memory.Embedder.BaseURL != "" && key != config.EmbedderAPIBedrock {
		baseURL = cfg.Memory.Embedder.BaseURL
	}
	if cfg.Memory.Embedder.APIKeyEnv != "" && key != config.EmbedderAPIBedrock {
		apiKey = os.Getenv(cfg.Memory.Embedder.APIKeyEnv)
		if apiKey == "" {
			slog.Warn("memory.embedder: api_key_env is set but empty — the embedder will call unauthenticated",
				"api_key_env", cfg.Memory.Embedder.APIKeyEnv, "provider", provider)
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

	e, err := providers.NewEmbedder(key, providers.EmbedderOptions{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		Model:      model,
		Dimensions: cfg.Memory.Embedder.Dimensions,
		Timeout:    time.Duration(timeoutMs) * time.Millisecond,
		BatchSize:  batchSize,
		Auth:       auth,
	})
	return e, provider, err
}
