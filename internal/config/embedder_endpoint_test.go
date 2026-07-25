package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	// Blank import so the `ollama` embedder ctor is registered — the
	// memory.embedder validation path checks the provider name against
	// providers.RegisteredEmbedders().
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
)

// writeCfg writes a throwaway yaml and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestEmbedderConfig_SelfHostedFieldsLoadFromYaml — the self-hostable-embedder
// surface: base_url / api_key_env / dimensions round-trip off the yaml so the
// composition root can thread them into providers.EmbedderOptions.
func TestEmbedderConfig_SelfHostedFieldsLoadFromYaml(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    provider: ollama
    model: nomic-embed-text
    base_url: http://ollama.internal:11434
    api_key_env: MY_OLLAMA_PROXY_TOKEN
    dimensions: 1024
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := cfg.Memory.Embedder
	if e.Provider != "ollama" || e.Model != "nomic-embed-text" {
		t.Errorf("provider/model = %q/%q, want ollama/nomic-embed-text", e.Provider, e.Model)
	}
	if e.BaseURL != "http://ollama.internal:11434" {
		t.Errorf("base_url = %q, want http://ollama.internal:11434", e.BaseURL)
	}
	if e.APIKeyEnv != "MY_OLLAMA_PROXY_TOKEN" {
		t.Errorf("api_key_env = %q, want MY_OLLAMA_PROXY_TOKEN", e.APIKeyEnv)
	}
	if e.Dimensions != 1024 {
		t.Errorf("dimensions = %d, want 1024", e.Dimensions)
	}
}

// TestEmbedderConfig_OmittedFieldsStayEmpty — the byte-identical-behaviour
// guarantee: a pre-existing embedder block that names none of the new knobs
// loads with all three at their zero value, so the composition root's
// per-provider defaults are what apply.
func TestEmbedderConfig_OmittedFieldsStayEmpty(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    provider: openai
    model: text-embedding-3-large
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := cfg.Memory.Embedder
	if e.BaseURL != "" || e.APIKeyEnv != "" || e.Dimensions != 0 {
		t.Errorf("unset knobs must stay zero, got base_url=%q api_key_env=%q dimensions=%d",
			e.BaseURL, e.APIKeyEnv, e.Dimensions)
	}
}

// TestEmbedderConfig_RejectsNonHTTPBaseURL — a base_url that is not an
// absolute http(s) URL with a host fails LOAD. The bare "host:port" form is
// the one operators actually typo (url.Parse reads "localhost" as the scheme),
// and it would otherwise surface as a confusing dial error on the first
// Memory.search rather than at boot.
func TestEmbedderConfig_RejectsNonHTTPBaseURL(t *testing.T) {
	for _, bad := range []string{
		"localhost:11434",
		"ollama.internal",
		"file:///etc/passwd",
		"ftp://ollama.internal:11434",
		"http://",
	} {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    provider: ollama
    model: nomic-embed-text
    base_url: "`+bad+`"
`))
		if err == nil {
			t.Errorf("base_url %q: expected a load error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "memory.embedder.base_url") {
			t.Errorf("base_url %q: error should name the field, got: %v", bad, err)
		}
	}
}

// TestEmbedderConfig_AcceptsHTTPAndHTTPSBaseURL — the validator must not be
// so tight it rejects the shapes operators legitimately use (bare host, IP,
// port, a proxied path prefix, https).
func TestEmbedderConfig_AcceptsHTTPAndHTTPSBaseURL(t *testing.T) {
	for _, ok := range []string{
		"http://localhost:11434",
		"http://127.0.0.1:11434",
		"http://ollama",
		"https://ollama.example.com",
		"https://gw.example.com/ollama",
	} {
		if _, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    provider: ollama
    model: nomic-embed-text
    base_url: "`+ok+`"
`)); err != nil {
			t.Errorf("base_url %q should load, got: %v", ok, err)
		}
	}
}

// TestEmbedderConfig_NewKnobsRequireAProvider — setting only base_url (or
// api_key_env / dimensions) with no provider must be refused rather than
// silently ignored: the block would otherwise look configured while vector ops
// kept refusing with embedder_not_configured.
func TestEmbedderConfig_NewKnobsRequireAProvider(t *testing.T) {
	for _, knob := range []string{
		"base_url: http://localhost:11434",
		"api_key_env: MY_EMBED_KEY",
		"dimensions: 1024",
	} {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    `+knob+`
`))
		if err == nil || !strings.Contains(err.Error(), "provider is required") {
			t.Errorf("%s alone: expected a provider-required error, got %v", knob, err)
		}
	}
}

// TestEmbedderConfig_NegativeDimensionsRefused — a negative width is a config
// error, not something to pass to a provider.
func TestEmbedderConfig_NegativeDimensionsRefused(t *testing.T) {
	_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    provider: ollama
    model: nomic-embed-text
    dimensions: -1
`))
	if err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("expected a dimensions validation error, got %v", err)
	}
}

// TestEmbedderConfig_OllamaIsASelectableProvider — the driver registers under
// the name an operator writes in yaml. Guards against the registry name and
// the documented yaml value drifting apart.
func TestEmbedderConfig_OllamaIsASelectableProvider(t *testing.T) {
	if _, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  embedder:
    provider: ollama
    model: nomic-embed-text
`)); err != nil {
		t.Fatalf("provider: ollama should be accepted, got: %v", err)
	}
}
