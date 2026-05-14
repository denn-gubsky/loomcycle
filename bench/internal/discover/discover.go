// Package discover queries each candidate provider's "what models do
// you serve?" endpoint via the existing provider drivers in
// internal/providers/. The bench imports those drivers in-process and
// calls ListModels directly — no second loomcycle round-trip.
//
// Provider keys MUST mirror the IDs loomcycle uses in its yaml — the
// bench registers dynamic agents with `provider: "<key>"` and the
// loomcycle resolver looks the key up in its registered providers.
// A mismatch fails register_agent with "unknown provider".
//
//	anthropic     — Anthropic (api.anthropic.com). The baseline; not
//	                in the default --providers list (would be redundant
//	                with the judge), but available for diagnostic
//	                "is the MCP integration broken?" sweeps that
//	                isolate loomcycle/wire issues from third-party
//	                model weaknesses.
//	deepseek      — DeepSeek public API (api.deepseek.com)
//	gemini        — Google Gemini (generativelanguage.googleapis.com)
//	ollama        — Ollama Cloud (ollama.com, Bearer auth via OLLAMA_API_KEY)
//	ollama-local  — local Ollama (uses OLLAMA_BASE_URL env var; same
//	                env var loomcycle's main.go reads, so the bench
//	                and loomcycle stay in agreement on which host)
package discover

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	"github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Discovery is one provider's discovered model list.
type Discovery struct {
	Provider string   // bench provider key (e.g. "deepseek", "ollama-desktop")
	Models   []string // wire model IDs returned by ListModels
	Err      error    // non-nil if discovery failed for this provider
}

// Discover runs ListModels for each requested provider. Providers
// missing credentials are returned with Err set (so the report can
// surface them) rather than silently skipped. Optional filter regexp
// further narrows the returned models per provider (applied after
// ListModels).
func Discover(ctx context.Context, providerKeys []string, filter *regexp.Regexp) []Discovery {
	out := make([]Discovery, 0, len(providerKeys))
	for _, key := range providerKeys {
		drv, err := newDriver(key)
		if err != nil {
			out = append(out, Discovery{Provider: key, Err: err})
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		models, err := drv.ListModels(probeCtx)
		cancel()
		if err != nil {
			out = append(out, Discovery{Provider: key, Err: err})
			continue
		}
		if filter != nil {
			filtered := models[:0]
			for _, m := range models {
				if filter.MatchString(m) {
					filtered = append(filtered, m)
				}
			}
			models = filtered
		}
		sort.Strings(models)
		out = append(out, Discovery{Provider: key, Models: models})
	}
	return out
}

// newDriver constructs a provider driver for ListModels. Credentials
// come from env vars matching loomcycle's standard names — keeps the
// bench config-free.
func newDriver(key string) (providers.Provider, error) {
	httpc := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
	opts := streamhttp.Options{HeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}

	switch key {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
		}
		return anthropic.New(apiKey, "", opts, httpc), nil

	case "deepseek":
		apiKey := os.Getenv("DEEPSEEK_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("DEEPSEEK_API_KEY not set")
		}
		return deepseek.New(apiKey, "", opts, httpc), nil

	case "gemini":
		apiKey := os.Getenv("GEMINI_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY not set")
		}
		return gemini.New(apiKey, "", opts, httpc), nil

	case "ollama":
		token := os.Getenv("OLLAMA_API_KEY")
		if token == "" {
			return nil, fmt.Errorf("OLLAMA_API_KEY not set (Ollama Cloud Bearer)")
		}
		baseURL := envOrDefault("OLLAMA_CLOUD_URL", "https://ollama.com")
		return ollama.New("ollama", token, baseURL, opts, httpc), nil

	case "ollama-local":
		baseURL := os.Getenv("OLLAMA_BASE_URL")
		if baseURL == "" || baseURL == "disabled" {
			return nil, fmt.Errorf("OLLAMA_BASE_URL not set (or =disabled); ollama-local unavailable")
		}
		return ollama.New("ollama-local", "", baseURL, opts, httpc), nil

	default:
		return nil, fmt.Errorf("unknown provider key %q", key)
	}
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
