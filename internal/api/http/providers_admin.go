// providers_admin.go — operator-facing read-only endpoints for
// inspecting the live provider state. Complements /v1/_resolver
// (which reads the CACHED availability matrix refreshed every ~15
// min) by doing a fresh round-trip to the provider's own /models
// endpoint NOW. Useful when an operator adds a model to the
// Anthropic / OpenAI console and wants to confirm loomcycle's
// driver sees it without waiting for the next probe cycle.
//
// All routes are bearer-authed (operator-only). The matrix and the
// model list expose config-shape — provider configuration, model
// names, reachability — that external consumers shouldn't see.
package http

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// providerModelsResponse is the wire shape returned by
// GET /v1/_providers/{id}/models.
type providerModelsResponse struct {
	// Provider is the id the URL named (anthropic / openai /
	// deepseek / gemini / ollama / ollama-local / etc.) — echoed
	// back so the response is self-describing.
	Provider string `json:"provider"`

	// FetchedAt is when this round-trip completed (server time).
	// Distinct from /v1/_resolver's GeneratedAt: that one cites the
	// snapshot assembly time against the cached matrix; this one
	// names the live wire round-trip's completion.
	FetchedAt time.Time `json:"fetched_at"`

	// DurationMs is how long the ListModels round-trip took. Helpful
	// when triaging slow-provider symptoms — a 4-second response
	// here points at the provider, not loomcycle.
	DurationMs int64 `json:"duration_ms"`

	// Models is the wire-listed model ids the provider currently
	// serves. Format is provider-native (Anthropic's `data[].id`
	// values, Ollama's `models[].name`, etc.) — the resolver matrix
	// uses these strings verbatim.
	//
	// Empty slice (not nil) when the provider responds successfully
	// with zero models — same convention as the Provider interface
	// contract. Distinguishes "reachable but empty" from "probe
	// failed".
	Models []string `json:"models"`
}

// handleProviderModels serves GET /v1/_providers/{id}/models.
// Bearer-authed. Looks up the named provider via the ProviderResolver,
// then does a live ListModels round-trip and returns the result.
//
// Error mapping:
//   - 404 provider_unknown:    no driver registered for the URL id
//   - 503 provider_unavailable: known driver but not configured (no
//                               API key, OLLAMA_BASE_URL=disabled, etc.)
//   - 502 provider_list_failed: ListModels round-trip failed
//                               (network, auth rejection, 5xx upstream)
//
// We distinguish 404 vs 503 by string-matching the resolver's error
// message — the only signal the existing ProviderResolver interface
// gives us. Fragile but adequate for an operator-only admin surface;
// adding a typed-error API to ProviderResolver would touch every
// driver implementation for marginal gain.
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_path", "provider id is required in path /v1/_providers/{id}/models")
		return
	}

	p, err := s.providers.Get(id)
	if err != nil {
		// "unknown provider X" is the resolver's default-branch
		// signal that the URL named a driver we don't ship.
		// Anything else means "we know about this driver but
		// the operator didn't wire it" — a different operator
		// fix and a different status code.
		if strings.Contains(err.Error(), "unknown provider") {
			writeJSONError(w, http.StatusNotFound, "provider_unknown", err.Error())
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "provider_unavailable", err.Error())
		return
	}

	start := time.Now()
	models, listErr := p.ListModels(r.Context())
	duration := time.Since(start)

	if listErr != nil {
		writeJSONError(w, http.StatusBadGateway, "provider_list_failed", listErr.Error())
		return
	}

	// Preserve "empty slice not nil" — the Provider interface
	// contract: a successful round-trip with zero models is
	// distinct from a failed probe.
	if models == nil {
		models = []string{}
	}

	resp := providerModelsResponse{
		Provider:   id,
		FetchedAt:  time.Now().UTC(),
		DurationMs: duration.Milliseconds(),
		Models:     models,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}
