package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// RFC EV §5.1 — which embedder constructor memory.embedder resolves to. The
// same function serves config validation and the embedder factory, so they
// cannot disagree. (The real embedders are registered by the driver blank
// imports in drivers_registry_test.go.)

const bedrockRuntimeAuth = "base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic, auth: {type: aws_sigv4, region: eu-central-1}"

func embedderKeyCfg(api string) *Config {
	c := &Config{Providers: map[string]ProviderConfig{
		"anthropic":    {Driver: "anthropic"},
		"openai":       {Driver: "openai"},
		"gemini":       {Driver: "gemini"},
		"ollama":       {Driver: "ollama"},
		"ollama-local": {Driver: "ollama"},
		"my-gateway":   {Driver: "openai", BaseURL: "https://gateway.example/v1"},
		"my-gemini":    {Driver: "gemini"},
		"my-ollama":    {Driver: "ollama"},
		"my-claude":    {Driver: "anthropic"},
		"my-deepseek":  {Driver: "deepseek"},
		"bedrock-claude": {Driver: "anthropic", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic",
			Auth: &ProviderAuth{Type: ProviderAuthSigV4, Region: "eu-central-1"}},
		"bedrock-openai": {Driver: "openai", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/openai/v1",
			Auth: &ProviderAuth{Type: ProviderAuthSigV4, Region: "eu-central-1"}},
		"bedrock-token": {Driver: "anthropic", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic",
			Auth: &ProviderAuth{Type: ProviderAuthBedrockToken, Region: "eu-central-1"}},
		"bedrock-mantle": {Driver: "openai", BaseURL: "https://bedrock-mantle.eu-central-1.api.aws/v1",
			Auth: &ProviderAuth{Type: ProviderAuthSigV4, Region: "eu-central-1"}},
		"signed-gateway": {Driver: "openai", BaseURL: "https://gateway.eu-central-1.example/v1",
			Auth: &ProviderAuth{Type: ProviderAuthSigV4, Region: "eu-central-1", Service: "execute-api"}},
		"bedrock-odd-service": {Driver: "anthropic", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic",
			Auth: &ProviderAuth{Type: ProviderAuthSigV4, Region: "eu-central-1", Service: "bedrock-mantle"}},
	}}
	c.Memory.Embedder.API = api
	return c
}

func TestEmbedderKey_ResolvesEachProviderToItsConstructor(t *testing.T) {
	for _, tc := range []struct {
		name, api, provider, want string
	}{
		// Every built-in id keeps the key it has always had — the id itself.
		{"anthropic stays Voyage", "", "anthropic", "anthropic"},
		{"openai", "", "openai", "openai"},
		{"gemini", "", "gemini", "gemini"},
		{"ollama", "", "ollama", "ollama"},
		{"ollama-local stays the local flavour, not its driver", "", "ollama-local", "ollama-local"},
		// The fix: a custom id embeds through its driver's embedder.
		{"custom id, driver openai", "", "my-gateway", "openai"},
		{"custom id, driver gemini", "", "my-gemini", "gemini"},
		{"custom id, driver ollama", "", "my-ollama", "ollama"},
		// An explicit api wins over the id and the driver.
		{"api over a custom driver", "openai", "my-gemini", "openai"},
		{"api over a built-in id", "ollama", "ollama-local", "ollama"},
		// RFC EV P2: Bedrock InvokeModel over either AWS-authenticated
		// runtime provider, whatever its chat route.
		{"bedrock over the anthropic route", "bedrock", "bedrock-claude", "bedrock"},
		{"bedrock over the openai route", "bedrock", "bedrock-openai", "bedrock"},
		{"bedrock over a minted-key provider", "bedrock", "bedrock-token", "bedrock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := embedderKeyCfg(tc.api).EmbedderKey(tc.provider)
			if err != nil {
				t.Fatalf("EmbedderKey(%q) with api %q: %v", tc.provider, tc.api, err)
			}
			if got != tc.want {
				t.Fatalf("EmbedderKey(%q) with api %q = %q, want %q", tc.provider, tc.api, got, tc.want)
			}
		})
	}
	// No providers: block at all (LOOMCYCLE_NO_DEFAULT_PROVIDERS=1) — the id
	// alone decides, as before.
	if got, err := (&Config{}).EmbedderKey("openai"); err != nil || got != "openai" {
		t.Fatalf("EmbedderKey(openai) with no providers block = %q, %v; want openai", got, err)
	}
}

func TestEmbedderKey_RefusesWhatNoEmbedderServes(t *testing.T) {
	for _, tc := range []struct {
		name, api, provider string
		want                []string
	}{
		// The anthropic driver has no embeddings API; Voyage is reachable only
		// under the id `anthropic`, so a custom anthropic id must not reach it.
		{"custom id, driver anthropic", "", "my-claude", []string{`"my-claude"`, `driver "anthropic"`, "openai"}},
		{"custom id, driver without an embedder", "", "my-deepseek", []string{`"my-deepseek"`, `driver "deepseek"`}},
		{"undeclared, unregistered id", "", "nowhere", []string{`"nowhere"`}},
		// AWS-authenticated providers: no derived embedder speaks their auth.
		{"auth provider, anthropic driver", "", "bedrock-claude", []string{"api: bedrock"}},
		{"auth provider, openai driver", "", "bedrock-openai", []string{"api: bedrock"}},
		{"auth provider, another api", "openai", "bedrock-openai", []string{"api: bedrock"}},
		// api must name a registered embedder.
		{"unknown api", "cohere", "openai", []string{"memory.embedder.api", `"cohere"`, "ollama-local"}},
		// api: bedrock speaks only to an AWS-authenticated runtime host.
		{"bedrock over a provider without auth", "bedrock", "openai", []string{"api: bedrock", `"openai"`, "auth:"}},
		{"bedrock over an undeclared provider", "bedrock", "nowhere", []string{"api: bedrock", `"nowhere"`}},
		{"bedrock over mantle", "bedrock", "bedrock-mantle", []string{"bedrock-mantle", "bedrock-runtime"}},
		{"bedrock over another signed host", "bedrock", "signed-gateway", []string{"bedrock-runtime.eu-central-1.amazonaws.com"}},
		{"bedrock with another signing service", "bedrock", "bedrock-odd-service", []string{"auth.service", `"bedrock"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := embedderKeyCfg(tc.api).EmbedderKey(tc.provider)
			if err == nil {
				t.Fatalf("EmbedderKey(%q) with api %q = %q, want an error", tc.provider, tc.api, got)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %s", err, w)
				}
			}
		})
	}
}

// TestEmbedderConfig_CustomIDOpenAIProviderLoads is the side finding: a
// provider declared under its own id with driver: openai failed config load as
// an "unknown provider" embedder.
func TestEmbedderConfig_CustomIDOpenAIProviderLoads(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  my-gateway: { driver: openai, base_url: "https://gateway.example/v1", api_key_env: LOOMCYCLE_GATEWAY_KEY }
models:
  gw-embed: { provider: my-gateway, model: text-embedding-3-small, kind: embedder }
memory:
  embedder:
    model: gw-embed
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, err := cfg.EmbedderKey("my-gateway"); err != nil || got != "openai" {
		t.Fatalf("EmbedderKey = %q, %v; want openai", got, err)
	}
}

func TestEmbedderConfig_AuthProviderWithoutAPIIsRefusedAtLoad(t *testing.T) {
	_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  bedrock-claude: { driver: anthropic, `+bedrockRuntimeAuth+` }
models:
  cohere-embed: { provider: bedrock-claude, model: eu.cohere.embed-v4:0, kind: embedder }
memory:
  embedder:
    model: cohere-embed
`))
	if err == nil || !strings.Contains(err.Error(), "api: bedrock") {
		t.Fatalf("Load = %v, want a refusal that says to set api: bedrock", err)
	}
}

// TestEmbedderConfig_RunsTheRegisteredValidator — a driver whose model and
// dimension rules config cannot know registers a validator, and config load
// runs it on the resolved key.
func TestEmbedderConfig_RunsTheRegisteredValidator(t *testing.T) {
	const key = "ev-test-validated"
	providers.RegisterEmbedder(key, func(providers.EmbedderOptions) (providers.Embedder, error) {
		return nil, errors.New("not built in this test")
	})
	providers.RegisterEmbedderValidator(key, func(model string, dimensions int) error {
		if dimensions == 7 {
			return errors.New("dimensions 7 is not one of 0, 8")
		}
		return nil
	})
	body := func(dims string) string {
		return `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  my-gateway: { driver: openai, base_url: "https://gateway.example/v1" }
memory:
  embedder:
    provider: my-gateway
    model: some-model
    api: ` + key + `
    dimensions: ` + dims + "\n"
	}
	if _, err := Load(writeCfg(t, body("0"))); err != nil {
		t.Fatalf("a config the validator accepts: %v", err)
	}
	_, err := Load(writeCfg(t, body("7")))
	if err == nil || !strings.Contains(err.Error(), "dimensions 7 is not one of 0, 8") || !strings.Contains(err.Error(), "memory.embedder") {
		t.Fatalf("Load = %v, want the validator's refusal under memory.embedder", err)
	}
}

// rfcEVConfig is RFC EV §4's example, on a provider the way the bedrock preset
// declares it, with extra lines appended to the embedder block.
func rfcEVConfig(model string, embedderLines string) string {
	return `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  bedrock-claude: { driver: anthropic, ` + bedrockRuntimeAuth + ` }
  bedrock-mantle: { driver: openai, base_url: "https://bedrock-mantle.eu-central-1.api.aws/v1", auth: {type: aws_sigv4, region: eu-central-1} }
models:
  cohere-embed: { provider: bedrock-claude, model: ` + model + `, kind: embedder }
  mantle-embed: { provider: bedrock-mantle, model: cohere.embed-v4:0, kind: embedder }
memory:
  embedder:
    api: bedrock
` + embedderLines
}

// TestEmbedderConfig_BedrockExampleLoads — RFC EV §4's block loads, v4 through
// the EU profile and v3 in-region, at each width the model serves.
func TestEmbedderConfig_BedrockExampleLoads(t *testing.T) {
	for _, tc := range []struct{ model, dims string }{
		{"eu.cohere.embed-v4:0", "1024"},
		{"eu.cohere.embed-v4:0", "0"},
		{"cohere.embed-v4:0", "256"},
		{"global.cohere.embed-v4:0", "1536"},
		{"cohere.embed-multilingual-v3", "1024"},
		{"cohere.embed-multilingual-v3", "0"},
	} {
		cfg, err := Load(writeCfg(t, rfcEVConfig(tc.model, "    model: cohere-embed\n    dimensions: "+tc.dims+"\n")))
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.model, tc.dims, err)
		}
		if got, err := cfg.EmbedderKey("bedrock-claude"); err != nil || got != EmbedderAPIBedrock {
			t.Fatalf("EmbedderKey = %q, %v; want bedrock", got, err)
		}
	}
}

// TestEmbedderConfig_BedrockRefusals — what RFC EV §4 refuses at load, each
// with a message that says what is allowed.
func TestEmbedderConfig_BedrockRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, model, lines string
		want               []string
	}{
		{"v4 width", "eu.cohere.embed-v4:0", "    model: cohere-embed\n    dimensions: 1000\n", []string{"memory.embedder", "1000", "256, 512, 1024, 1536"}},
		{"v3 width", "cohere.embed-multilingual-v3", "    model: cohere-embed\n    dimensions: 512\n", []string{"memory.embedder", "512", "0 or 1024"}},
		{"not cohere", "amazon.titan-embed-text-v2:0", "    model: cohere-embed\n", []string{"memory.embedder", "cohere.embed-v4:0"}},
		{"application profile ARN", "\"arn:aws:bedrock:eu-central-1:123456789012:application-inference-profile/abc\"", "    model: cohere-embed\n", []string{"ARN"}},
		{"mantle", "eu.cohere.embed-v4:0", "    model: mantle-embed\n", []string{"bedrock-mantle", "no embeddings"}},
		{"base_url override", "eu.cohere.embed-v4:0", "    model: cohere-embed\n    base_url: https://elsewhere.example\n", []string{"memory.embedder.base_url", "api: bedrock"}},
		{"api_key_env override", "eu.cohere.embed-v4:0", "    model: cohere-embed\n    api_key_env: LOOMCYCLE_SOME_KEY\n", []string{"memory.embedder.api_key_env", "api: bedrock"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, rfcEVConfig(tc.model, tc.lines)))
			if err == nil {
				t.Fatal("Load accepted it")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %s", err, w)
				}
			}
		})
	}
}
