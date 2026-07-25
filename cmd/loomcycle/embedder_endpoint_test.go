package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestEmbedderConfig_BaseURLAndKeyEnvThreadThrough — the composition-root
// contract for the self-hostable embedder. buildEmbedder's per-provider switch
// pins `openai` to the vendor endpoint and the OPENAI_API_KEY-derived host
// key; an operator's explicit yaml must WIN over both, otherwise an
// OpenAI-compatible self-hosted embedder is unreachable no matter what the
// driver supports.
//
// Asserted end-to-end (config → buildEmbedder → driver → wire) because that is
// the only place the override is observable: the driver keeps its endpoint and
// key unexported.
func TestEmbedderConfig_BaseURLAndKeyEnvThreadThrough(t *testing.T) {
	var hits int
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		seenAuth = r.Header.Get("Authorization")
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, 0, len(req.Input))
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, 2, 3, 4}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "bge-m3", "data": data})
	}))
	defer srv.Close()

	t.Setenv("LOOMCYCLE_TEST_EMBED_KEY", "operator-key-placeholder")

	cfg := &config.Config{}
	// The host key the switch would otherwise hand to the openai driver.
	cfg.Env.OpenAIAPIKey = "host-key-must-lose"
	cfg.Memory.Embedder = config.EmbedderConfig{
		Provider:  "openai",
		Model:     "bge-m3",
		BaseURL:   srv.URL,
		APIKeyEnv: "LOOMCYCLE_TEST_EMBED_KEY",
	}

	e, err := buildEmbedder(cfg)
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if e == nil {
		t.Fatal("buildEmbedder returned nil for a configured embedder")
	}
	if _, err := e.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if hits != 1 {
		t.Fatalf("operator base_url saw %d requests, want 1 (the switch default won)", hits)
	}
	if seenAuth != "Bearer operator-key-placeholder" {
		t.Errorf("Authorization = %q, want the api_key_env-resolved key (the host key won)", seenAuth)
	}
}

// TestEmbedderConfig_OllamaLocalIsKeylessAndSendsDimensions — the self-hosted
// route end-to-end: provider + model + base_url, no key. Also proves
// `dimensions` reaches the wire, the lever an operator uses to keep a 4096-dim
// model under pgvector's 2000-dim index ceiling.
func TestEmbedderConfig_OllamaLocalIsKeylessAndSendsDimensions(t *testing.T) {
	var body struct {
		Model      string   `json:"model"`
		Input      []string `json:"input"`
		Dimensions *int     `json:"dimensions"`
	}
	var seenAuth, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3-embedding","embeddings":[[1,2,3]]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Memory.Embedder = config.EmbedderConfig{
		Provider:   "ollama-local",
		Model:      "qwen3-embedding",
		BaseURL:    srv.URL,
		Dimensions: 1024,
	}

	e, err := buildEmbedder(cfg)
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.HasSuffix(seenPath, "/api/embed") {
		t.Errorf("path = %q, want /api/embed", seenPath)
	}
	if seenAuth != "" {
		t.Errorf("Authorization = %q, want none (a local Ollama is keyless)", seenAuth)
	}
	if body.Model != "qwen3-embedding" {
		t.Errorf("model = %q, want qwen3-embedding", body.Model)
	}
	if body.Dimensions == nil || *body.Dimensions != 1024 {
		t.Errorf("dimensions = %v, want 1024 threaded from yaml", body.Dimensions)
	}
	if e.Provider() != "ollama-local" {
		t.Errorf("Provider() = %q, want ollama-local", e.Provider())
	}
	if e.Dimension() != 3 {
		t.Errorf("Dimension() = %d, want the 3 observed on the wire", e.Dimension())
	}
}

// TestEmbedderConfig_OllamaLocalInheritsTheChatBaseURLEnv — an operator who
// already set OLLAMA_BASE_URL for chat must not have to restate it for the
// embedder. buildEmbedder's `ollama-local` case reads the SAME resolved env
// value the chat driver does.
func TestEmbedderConfig_OllamaLocalInheritsTheChatBaseURLEnv(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"nomic-embed-text","embeddings":[[1,2,3]]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Env.OllamaBaseURL = srv.URL // what OLLAMA_BASE_URL resolves to
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "ollama-local", Model: "nomic-embed-text"}

	e, err := buildEmbedder(cfg)
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if hits != 1 {
		t.Errorf("OLLAMA_BASE_URL endpoint saw %d requests, want 1", hits)
	}
}

// TestEmbedderConfig_YamlBaseURLBeatsTheEnv — the precedence an operator will
// otherwise spend an hour debugging: explicit yaml `base_url` > the inherited
// env var > the driver default. A silently-ignored yaml override is the whole
// failure mode this asserts against.
func TestEmbedderConfig_YamlBaseURLBeatsTheEnv(t *testing.T) {
	var envHits, yamlHits int
	envSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","embeddings":[[1]]}`))
	}))
	defer envSrv.Close()
	yamlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		yamlHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","embeddings":[[1]]}`))
	}))
	defer yamlSrv.Close()

	cfg := &config.Config{}
	cfg.Env.OllamaBaseURL = envSrv.URL
	cfg.Memory.Embedder = config.EmbedderConfig{
		Provider: "ollama-local",
		Model:    "m",
		BaseURL:  yamlSrv.URL,
	}

	e, err := buildEmbedder(cfg)
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if yamlHits != 1 || envHits != 0 {
		t.Errorf("yaml base_url hits=%d env hits=%d; want the yaml override to win (1/0)", yamlHits, envHits)
	}
}

// TestEmbedderConfig_OllamaLocalIgnoresTheDisabledSentinel —
// OLLAMA_BASE_URL=disabled is the chat side's opt-out marker, not an endpoint.
// Passing it through would make the embedder dial a host named "disabled";
// it must fall back to the driver default instead.
func TestEmbedderConfig_OllamaLocalIgnoresTheDisabledSentinel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.OllamaBaseURL = "disabled"
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "ollama-local", Model: "m"}

	e, err := buildEmbedder(cfg)
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	// The endpoint is unexported; observe it through the dial error, which
	// must name the driver default host, never "disabled".
	_, err = e.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Skip("something is listening on the default Ollama port; dial target not observable here")
	}
	if strings.Contains(err.Error(), "disabled") {
		t.Errorf("the opt-out sentinel leaked into the endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "localhost:11434") {
		t.Errorf("error should name the driver default endpoint, got: %v", err)
	}
}

// TestEmbedderConfig_UnsetKeyEnvKeepsTheHostKey — the compatibility guarantee
// in the other direction: with api_key_env absent, the per-provider switch's
// host key is still what authenticates. (base_url is set purely to make the
// header observable without dialling api.openai.com.)
func TestEmbedderConfig_UnsetKeyEnvKeepsTheHostKey(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"bge-m3","data":[{"index":0,"embedding":[1,2]}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Env.OpenAIAPIKey = "host-key-placeholder"
	cfg.Memory.Embedder = config.EmbedderConfig{Provider: "openai", Model: "bge-m3", BaseURL: srv.URL}

	e, err := buildEmbedder(cfg)
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if seenAuth != "Bearer host-key-placeholder" {
		t.Errorf("Authorization = %q, want the per-provider host key when api_key_env is unset", seenAuth)
	}
}
