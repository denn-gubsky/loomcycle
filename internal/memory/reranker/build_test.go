package reranker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

func openAIConfig(rc config.RerankerConfig) *config.Config {
	rc.Provider, rc.Model = "openai", "gpt-test"
	return &config.Config{
		Providers: map[string]config.ProviderConfig{"openai": {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"}},
		Memory:    config.MemoryConfig{Reranker: rc},
	}
}

// TestBuild_AnOverriddenEndpointDoesNotTakeATenantsProviderKey — pointed at the
// operator's own endpoint without a named key, the reranker must not answer to
// the PROVIDER's credential name: a tenant's stored OPENAI_API_KEY would be sent
// to a host OpenAI does not run. Only a credential stored for the reranker may
// override. With the provider's own endpoint, the provider's name is right: a
// tenant bringing its own key pays for its own reranks.
func TestBuild_AnOverriddenEndpointDoesNotTakeATenantsProviderKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	t.Setenv("MY_RERANK_KEY", "test-rerank-key")
	for _, c := range []struct {
		name string
		rc   config.RerankerConfig
		want string
	}{
		{"provider endpoint", config.RerankerConfig{}, "OPENAI_API_KEY"},
		{"own endpoint, no key named", config.RerankerConfig{BaseURL: "http://gpu.internal:8000/v1"}, rerankerKeyEnvName},
		{"own endpoint, key named", config.RerankerConfig{BaseURL: "http://gpu.internal:8000/v1", APIKeyEnv: "MY_RERANK_KEY"}, "MY_RERANK_KEY"},
	} {
		m, err := Build(openAIConfig(c.rc))
		if err != nil {
			t.Fatalf("%s: Build: %v", c.name, err)
		}
		keyed, ok := m.provider.(interface{ KeyEnvName() string })
		if !ok {
			t.Fatalf("%s: the openai driver no longer reports its credential name", c.name)
		}
		if got := keyed.KeyEnvName(); got != c.want {
			t.Errorf("%s: the reranker answers to credential %q, want %q", c.name, got, c.want)
		}
	}
}

// TestModel_ARestrictedRunCannotSpendTheOperatorsKey — the operator-key
// restriction holds for a rerank as it does for the run: the call reaches the
// driver on the run's context, and a restricted run with no key of its own is
// refused BEFORE any request leaves — the search then keeps its own order.
func TestModel_ARestrictedRunCannotSpendTheOperatorsKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer srv.Close()
	m, err := Build(openAIConfig(config.RerankerConfig{BaseURL: srv.URL + "/v1"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := providers.WithOperatorKeyAllowed(context.Background(), false)
	_, err = m.Complete(ctx, "prompt")
	if !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		t.Errorf("restricted run: err = %v, want ErrOperatorKeyForbidden", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the endpoint was called %d times on the operator's key for a restricted run", n)
	}
	// Control: the same reranker on an unrestricted run does reach the endpoint,
	// so the zero above is the restriction and not a dead endpoint.
	_, _ = m.Complete(context.Background(), "prompt")
	if hits.Load() == 0 {
		t.Error("control: an unrestricted rerank never reached the endpoint — the check above proves nothing")
	}
}

// TestBuild_ResolvesAModelsAlias — `memory.reranker.model: local-medium` asks the
// provider for the model the alias names, not for "local-medium".
func TestBuild_ResolvesAModelsAlias(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	cfg := openAIConfig(config.RerankerConfig{})
	cfg.Memory.Reranker.Provider = ""
	cfg.Memory.Reranker.Model = "fast-rerank"
	cfg.Models = map[string]config.ModelRef{"fast-rerank": {Provider: "openai", Model: "gpt-5.4-mini"}}
	m, err := Build(cfg)
	if err != nil || m == nil {
		t.Fatalf("Build = %v, %v", m, err)
	}
	if m.ModelID() != "gpt-5.4-mini" || m.ProviderID() != "openai" {
		t.Errorf("reranker = %s/%s, want openai/gpt-5.4-mini", m.ProviderID(), m.ModelID())
	}
}
