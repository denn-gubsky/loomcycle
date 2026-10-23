package wiring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

// openAIEmbedSrv is an OpenAI-shaped /embeddings stub that records the
// Authorization header of every request it receives.
func openAIEmbedSrv(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}],"model":"m"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

// TestEmbedder_CustomIDOpenAIProviderEmbedsAtItsOwnEndpoint — RFC EV's side
// finding: a provider declared under its own id with driver: openai can be the
// embedder, and it embeds at that provider's endpoint with that provider's key.
// It failed before: the factory looked the constructor up by provider id only.
func TestEmbedder_CustomIDOpenAIProviderEmbedsAtItsOwnEndpoint(t *testing.T) {
	srv, auths := openAIEmbedSrv(t)
	t.Setenv("LOOMCYCLE_TEST_GATEWAY_KEY", "gateway-key")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"my-gateway": {Driver: "openai", BaseURL: srv.URL, APIKeyEnv: "LOOMCYCLE_TEST_GATEWAY_KEY"},
		},
		Models: map[string]config.ModelRef{"gw-embed": {Provider: "my-gateway", Model: "gateway-embed", Kind: config.ModelKindEmbedder}},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Model: "gw-embed"}},
	}
	e, provider, err := buildEmbedder(cfg)
	if err != nil || e == nil {
		t.Fatalf("buildEmbedder = %v, %v", e, err)
	}
	// The lane is the provider's, whatever constructor serves it.
	if provider != "my-gateway" {
		t.Errorf("resolved provider = %q, want my-gateway", provider)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := auths(); len(got) != 1 || got[0] != "Bearer gateway-key" {
		t.Fatalf("requests at the provider's endpoint = %q, want one carrying its own key", got)
	}
}

// TestEmbedder_TheVoyageEmbedderNeverInheritsAnotherProvidersEndpointOrKey — the
// Voyage exception follows the embedder chosen, not the provider id: with
// api: anthropic over another provider, neither that provider's base_url nor
// its key may travel to Voyage.
func TestEmbedder_TheVoyageEmbedderNeverInheritsAnotherProvidersEndpointOrKey(t *testing.T) {
	providerSrv, providerAuths := openAIEmbedSrv(t)
	voyageSrv, voyageAuths := openAIEmbedSrv(t)
	t.Setenv("LOOMCYCLE_TEST_GATEWAY_KEY", "gateway-key-must-not-reach-voyage")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"my-gateway": {Driver: "openai", BaseURL: providerSrv.URL, APIKeyEnv: "LOOMCYCLE_TEST_GATEWAY_KEY"},
		},
		// The embedder's own base_url is the only endpoint Voyage may take
		// besides its default; it stands in for Voyage here.
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{
			Provider: "my-gateway", Model: "voyage-3", API: "anthropic", BaseURL: voyageSrv.URL,
		}},
	}
	cfg.Env.VoyageAPIKey = "voyage-key"
	e, err := Embedder(cfg)
	if err != nil || e == nil {
		t.Fatalf("Embedder = %v, %v", e, err)
	}
	_, _ = e.Embed(context.Background(), []string{"x"})
	if got := providerAuths(); len(got) != 0 {
		t.Errorf("the Voyage embedder sent %d requests to the provider's base_url, want 0", len(got))
	}
	if got := voyageAuths(); len(got) != 1 || got[0] != "Bearer voyage-key" {
		t.Errorf("requests reaching Voyage = %q, want one carrying VOYAGE_API_KEY only", got)
	}
}

// TestEmbedder_RefusesWhatConfigRefuses — the factory resolves the constructor
// with the same function config validation does, so a config that could not
// load cannot build either.
func TestEmbedder_RefusesWhatConfigRefuses(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"my-claude": {Driver: "anthropic"}},
		Memory:    config.MemoryConfig{Embedder: config.EmbedderConfig{Provider: "my-claude", Model: "voyage-3"}},
	}
	want, wantErr := cfg.EmbedderKey("my-claude")
	if wantErr == nil {
		t.Fatalf("EmbedderKey(my-claude) = %q, want a refusal", want)
	}
	if _, err := Embedder(cfg); err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("Embedder = %v, want config's refusal %q", err, wantErr)
	}
}
