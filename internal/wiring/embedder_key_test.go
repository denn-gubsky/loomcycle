package wiring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestEmbedder_HostVendorKeyStaysAtTheVendorsEndpoint — the per-id env key the
// embedder falls back to when the provider names no api_key_env (OPENAI_API_KEY
// for the "openai" id) is the vendor's key: it goes only to the endpoint that
// id calls by default, never to a base_url the operator pointed elsewhere,
// whether on the provider or on the embedder block.
func TestEmbedder_HostVendorKeyStaysAtTheVendorsEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		embedder string
	}{
		{name: "provider base_url", provider: "srv"},
		{name: "embedder base_url", embedder: "srv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, auths := openAIEmbedSrv(t)
			pc := config.ProviderConfig{Driver: "openai"}
			ec := config.EmbedderConfig{Provider: "openai", Model: "text-embedding-3-small"}
			if tc.provider != "" {
				pc.BaseURL = srv.URL
			}
			if tc.embedder != "" {
				ec.BaseURL = srv.URL
			}
			cfg := &config.Config{
				Env:       config.Env{OpenAIAPIKey: "host-openai-key"},
				Providers: map[string]config.ProviderConfig{"openai": pc},
				Memory:    config.MemoryConfig{Embedder: ec},
			}
			e, _, err := buildEmbedder(cfg)
			if err != nil || e == nil {
				t.Fatalf("buildEmbedder = %v, %v", e, err)
			}
			_, _ = e.Embed(context.Background(), []string{"x"})
			// The openai embedder sends its (empty) key as a bare "Bearer"
			// header; what matters is that no key follows it.
			if got := auths(); len(got) != 1 || strings.TrimSpace(strings.TrimPrefix(got[0], "Bearer")) != "" {
				t.Fatalf("requests at the operator's endpoint = %q, want one carrying no key", got)
			}
		})
	}
}

// TestEmbedder_HostVendorKeyStillAuthenticatesAtTheVendor — the other half:
// with no providers: entry and no base_url anywhere, the per-id env key is
// still what the embedder authenticates with at the vendor's own endpoint, and
// a base_url on the provider takes it away. A capturing constructor stands in
// for the gemini embedder, so nothing dials Google.
func TestEmbedder_HostVendorKeyStillAuthenticatesAtTheVendor(t *testing.T) {
	registerCapture("gemini")
	cfg := &config.Config{
		Env:    config.Env{GeminiAPIKey: "host-gemini-key"},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Provider: "gemini", Model: "text-embedding-004"}},
	}
	if _, _, err := buildEmbedder(cfg); err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if o := capturedOpts(t, "gemini"); o.APIKey != "host-gemini-key" || o.BaseURL != "" {
		t.Errorf("at the vendor: APIKey set = %v, BaseURL = %q; want the host key and the driver default", o.APIKey != "", o.BaseURL)
	}

	cfg.Providers = map[string]config.ProviderConfig{"gemini": {Driver: "gemini", BaseURL: "http://127.0.0.1:9/v1beta"}}
	if _, _, err := buildEmbedder(cfg); err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if o := capturedOpts(t, "gemini"); o.APIKey != "" {
		t.Error("a provider base_url off the vendor still received the vendor's host key")
	}
}
