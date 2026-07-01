package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestListModels_ReturnsConfiguredAliases: GET /v1/_models surfaces the
// top-level `models:` map (alias -> provider/model) so a UI can offer aliases in
// a model picker and store the alias on a fork (tracking the operator's local
// override) instead of a concrete model.
func TestListModels_ReturnsConfiguredAliases(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelRef{
			"local-medium": {Provider: "ollama-local", Model: "qwen3.6:latest"},
			"deepseek-pro": {Provider: "deepseek", Model: "deepseek-v4-pro"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = ""
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/_models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	var got modelAliasesResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if lm := got.Aliases["local-medium"]; lm.Provider != "ollama-local" || lm.Model != "qwen3.6:latest" {
		t.Errorf("local-medium = %+v, want ollama-local/qwen3.6:latest", lm)
	}
	if dp := got.Aliases["deepseek-pro"]; dp.Provider != "deepseek" || dp.Model != "deepseek-v4-pro" {
		t.Errorf("deepseek-pro = %+v, want deepseek/deepseek-v4-pro", dp)
	}
}
