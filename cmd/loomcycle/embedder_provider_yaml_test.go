package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory/embedders"
)

// embedSrv is an Ollama-shaped /api/embed stub that counts what reached it.
// The endpoint is unexported on every driver, so which server receives the
// request IS the assertion — the same technique as embedder_endpoint_test.go.
func embedSrv(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"bge-m3","embeddings":[[1,2,3]]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmbedderConfig_OllamaLocalResolvesTheProvidersMapBaseURL — the regression.
//
// An operator repoints ollama-local at a new host the documented way, in
// `providers:` yaml. Chat followed (providerbuild prefers pc.BaseURL) but the
// embedder read cfg.Env.OllamaBaseURL ONLY, so it kept dialing the stale
// default. Nothing surfaced it: a k/v memory write returns 200 whether or not
// the embedding lands, so memory silently degraded to non-semantic.
//
// FAILS on the unfixed code — the request lands on the env server.
func TestEmbedderConfig_OllamaLocalResolvesTheProvidersMapBaseURL(t *testing.T) {
	var envHits, yamlHits int
	envSrv := embedSrv(t, &envHits)   // the stale host the env var still names
	yamlSrv := embedSrv(t, &yamlHits) // the new host declared in yaml

	cfg := &config.Config{}
	cfg.Env.OllamaBaseURL = envSrv.URL
	cfg.Providers = map[string]config.ProviderConfig{
		"ollama-local": {Driver: "ollama", BaseURL: yamlSrv.URL},
	}
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "ollama-local", Model: "bge-m3"}

	e, err := embedders.Build(cfg)
	if err != nil {
		t.Fatalf("embedders.Build: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if yamlHits != 1 || envHits != 0 {
		t.Errorf("providers: yaml base_url got %d requests and the env default got %d; want 1 and 0 "+
			"(the embedder ignored the yaml the chat driver honours)", yamlHits, envHits)
	}
}

// TestEmbedderConfig_EmbedderYamlBeatsTheProvidersMap — the embedder's own block
// stays the top of the precedence chain, so an operator can point embeddings at
// a dedicated host while chat keeps the shared one.
func TestEmbedderConfig_EmbedderYamlBeatsTheProvidersMap(t *testing.T) {
	var providerHits, embedderHits int
	providerSrv := embedSrv(t, &providerHits)
	embedderSrv := embedSrv(t, &embedderHits)

	cfg := &config.Config{}
	cfg.Providers = map[string]config.ProviderConfig{
		"ollama-local": {Driver: "ollama", BaseURL: providerSrv.URL},
	}
	cfg.Memory.Embedder = config.EmbedderConfig{
		Provider: "ollama-local", Model: "bge-m3", BaseURL: embedderSrv.URL,
	}

	e, err := embedders.Build(cfg)
	if err != nil {
		t.Fatalf("embedders.Build: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if embedderHits != 1 || providerHits != 0 {
		t.Errorf("memory.embedder.base_url got %d and providers: got %d; want 1 and 0",
			embedderHits, providerHits)
	}
}

// TestEmbedderConfig_ProvidersMapApiKeyEnvIsHonoured — api_key_env is a NAME, and
// the embedder must resolve the one the operator declared rather than assuming
// the vendor default. Guards the RFC AR renamed-credential case.
func TestEmbedderConfig_ProvidersMapApiKeyEnvIsHonoured(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","data":[{"index":0,"embedding":[1,2]}]}`))
	}))
	defer srv.Close()

	t.Setenv("LOOMCYCLE_TEST_DECLARED_KEY", "declared-key-placeholder")

	cfg := &config.Config{}
	cfg.Env.OpenAIAPIKey = "vendor-default-must-lose"
	cfg.Providers = map[string]config.ProviderConfig{
		"openai": {Driver: "openai", BaseURL: srv.URL, APIKeyEnv: "LOOMCYCLE_TEST_DECLARED_KEY"},
	}
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "openai", Model: "m"}

	e, err := embedders.Build(cfg)
	if err != nil {
		t.Fatalf("embedders.Build: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if seenAuth != "Bearer declared-key-placeholder" {
		t.Errorf("Authorization did not come from the declared api_key_env (got the vendor default)")
	}
}

// TestEmbedderConfig_NoProvidersBlockStillUsesTheEnvKey — a deployment running
// with LOOMCYCLE_NO_DEFAULT_PROVIDERS=1 and no `providers:` block declares no
// api_key_env at all. The per-id env key must remain the last resort: the openai
// embedder accepts an empty key at construction and only fails later at 401, so
// dropping this fallback would turn a working embedder into a runtime auth error.
func TestEmbedderConfig_NoProvidersBlockStillUsesTheEnvKey(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","data":[{"index":0,"embedding":[1,2]}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{} // no cfg.Providers at all
	cfg.Env.OpenAIAPIKey = "env-key-placeholder"
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "openai", Model: "m", BaseURL: srv.URL}

	e, err := embedders.Build(cfg)
	if err != nil {
		t.Fatalf("embedders.Build: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if seenAuth != "Bearer env-key-placeholder" {
		t.Errorf("Authorization = %q, want the per-id env key fallback", seenAuth)
	}
}

// TestEmbedderConfig_AnthropicDoesNotInheritTheChatProviderEntry — the one
// provider that must NOT share its chat account. The `anthropic` embedder slot is
// a Voyage AI proxy, a different vendor, so inheriting the anthropic entry would
// POST Voyage requests at an Anthropic proxy and send ANTHROPIC_API_KEY to
// Voyage. Both halves must be ignored.
func TestEmbedderConfig_AnthropicDoesNotInheritTheChatProviderEntry(t *testing.T) {
	var anthropicHits int
	anthropicSrv := embedSrv(t, &anthropicHits)

	t.Setenv("LOOMCYCLE_TEST_ANTHROPIC_KEY", "anthropic-key-must-not-reach-voyage")

	cfg := &config.Config{}
	cfg.Env.VoyageAPIKey = "voyage-key-placeholder"
	cfg.Providers = map[string]config.ProviderConfig{
		"anthropic": {
			Driver:    "anthropic",
			BaseURL:   anthropicSrv.URL,
			APIKeyEnv: "LOOMCYCLE_TEST_ANTHROPIC_KEY",
		},
	}
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "anthropic", Model: "voyage-3"}

	e, err := embedders.Build(cfg)
	if err != nil {
		t.Fatalf("embedders.Build: %v", err)
	}
	// Not asserting on a successful Embed: the point is that the Voyage driver
	// never adopted the Anthropic endpoint, so the anthropic stub sees nothing.
	_, _ = e.Embed(context.Background(), []string{"x"})
	if anthropicHits != 0 {
		t.Errorf("the Voyage embedder sent %d requests to the anthropic chat base_url, want 0", anthropicHits)
	}
}
