package wiring

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/awsauth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// These tests stand a capturing constructor in for the bedrock embedder, so
// they read exactly what buildEmbedder hands it, and nothing reaches AWS. The real
// package is not imported in this test binary.

var (
	capturedMu sync.Mutex
	captured   = map[string]providers.EmbedderOptions{}
)

type capturedEmbedder struct{}

func (capturedEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (capturedEmbedder) Model() string                                        { return "m" }
func (capturedEmbedder) Provider() string                                     { return "captured" }
func (capturedEmbedder) Dimension() int                                       { return 0 }

func registerCapture(key string) {
	providers.RegisterEmbedder(key, func(o providers.EmbedderOptions) (providers.Embedder, error) {
		capturedMu.Lock()
		captured[key] = o
		capturedMu.Unlock()
		return capturedEmbedder{}, nil
	})
}

func capturedOpts(t *testing.T, key string) providers.EmbedderOptions {
	t.Helper()
	capturedMu.Lock()
	defer capturedMu.Unlock()
	o, ok := captured[key]
	if !ok {
		t.Fatalf("the %s constructor was never called", key)
	}
	delete(captured, key)
	return o
}

// isolatedAWSEnv gives the credential chain fixed env keys and nothing else.
func isolatedAWSEnv(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"AWS_PROFILE", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEMBEDDERS")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "embedders-secret-0123456789")
}

func bedrockCfg(route string) *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"bedrock-claude": {Driver: "anthropic", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com" + route,
				Auth: &config.ProviderAuth{Type: config.ProviderAuthSigV4, Region: "eu-central-1"}},
		},
		Models: map[string]config.ModelRef{"cohere-embed": {Provider: "bedrock-claude", Model: "eu.cohere.embed-v4:0", Kind: config.ModelKindEmbedder}},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Model: "cohere-embed", API: config.EmbedderAPIBedrock, Dimensions: 1024}},
	}
}

// TestEmbedder_BedrockGetsTheProvidersAWSAuthAtItsHostOnly — RFC EV §5.2: the
// bedrock embedder gets the provider's AWS auth, the scheme and host of the
// provider's base_url (its chat route dropped), and no key.
func TestEmbedder_BedrockGetsTheProvidersAWSAuthAtItsHostOnly(t *testing.T) {
	isolatedAWSEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key-must-not-reach-aws")
	registerCapture(config.EmbedderAPIBedrock)
	for _, route := range []string{"/anthropic", "/openai/v1", ""} {
		cfg := bedrockCfg(route)
		var sunk []string
		cfg.SecretSink = func(v string) { sunk = append(sunk, v) }
		_, provider, err := buildEmbedder(cfg)
		if err != nil {
			t.Fatalf("%q: buildEmbedder: %v", route, err)
		}
		if provider != "bedrock-claude" {
			t.Errorf("lane provider = %q, want bedrock-claude", provider)
		}
		o := capturedOpts(t, config.EmbedderAPIBedrock)
		if o.BaseURL != "https://bedrock-runtime.eu-central-1.amazonaws.com" {
			t.Errorf("%q: BaseURL = %q, want the provider's scheme and host", route, o.BaseURL)
		}
		if o.APIKey != "" {
			t.Errorf("%q: an API key was handed to the bedrock embedder", route)
		}
		if o.Model != "eu.cohere.embed-v4:0" || o.Dimensions != 1024 {
			t.Errorf("%q: model %q dims %d", route, o.Model, o.Dimensions)
		}
		a, ok := o.Auth.(*awsauth.Auth)
		if !ok || a.Region() != "eu-central-1" || a.Service() != "bedrock" {
			t.Fatalf("%q: Auth = %T, want the provider's AWS auth", route, o.Auth)
		}
		if _, err := a.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
		found := false
		for _, v := range sunk {
			found = found || v == "embedders-secret-0123456789"
		}
		if !found {
			t.Errorf("%q: the embedder's credentials bypass the redactor", route)
		}
	}
}

// TestEmbedder_OnlyTheBedrockEmbedderGetsAnAuth — every other embedder keeps
// getting a key and never an Auth.
func TestEmbedder_OnlyTheBedrockEmbedderGetsAnAuth(t *testing.T) {
	const key = "ev-capture-plain"
	registerCapture(key)
	t.Setenv("LOOMCYCLE_TEST_GATEWAY_KEY", "gateway-key")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"my-gateway": {Driver: "openai", BaseURL: "https://gateway.example/v1", APIKeyEnv: "LOOMCYCLE_TEST_GATEWAY_KEY"},
		},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Provider: "my-gateway", Model: "m", API: key}},
	}
	if _, _, err := buildEmbedder(cfg); err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	o := capturedOpts(t, key)
	if o.Auth != nil || o.APIKey != "gateway-key" || o.BaseURL != "https://gateway.example/v1" {
		t.Errorf("opts = auth %T, key set %v, base %q; want the provider's key and endpoint, no auth", o.Auth, o.APIKey != "", o.BaseURL)
	}
}

// TestEmbedder_BedrockRefusesAnEndpointOrKeyOfItsOwn — the embedder block cannot
// send the provider's AWS credentials anywhere but the provider's own host.
func TestEmbedder_BedrockRefusesAnEndpointOrKeyOfItsOwn(t *testing.T) {
	isolatedAWSEnv(t)
	registerCapture(config.EmbedderAPIBedrock)
	for name, mod := range map[string]func(*config.EmbedderConfig){
		"base_url":    func(e *config.EmbedderConfig) { e.BaseURL = "https://elsewhere.example" },
		"api_key_env": func(e *config.EmbedderConfig) { e.APIKeyEnv = "LOOMCYCLE_SOME_KEY" },
	} {
		cfg := bedrockCfg("/anthropic")
		mod(&cfg.Memory.Embedder)
		if _, _, err := buildEmbedder(cfg); err == nil {
			t.Errorf("%s: buildEmbedder accepted it", name)
		}
	}
}

// TestEmbedder_BedrockWithNoUsableCredentialsStillBuilds — boot treats an
// embedder construction error as fatal, so missing credentials must surface
// on the first call, not at build.
func TestEmbedder_BedrockWithNoUsableCredentialsStillBuilds(t *testing.T) {
	isolatedAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "no-such-profile")
	registerCapture(config.EmbedderAPIBedrock)
	if _, _, err := buildEmbedder(bedrockCfg("/anthropic")); err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	o := capturedOpts(t, config.EmbedderAPIBedrock)
	if o.Auth == nil {
		t.Fatal("no Auth: the embedder would refuse construction and fail boot")
	}
	if err := o.Auth.Apply(context.Background(), nil, nil, nil); !errors.Is(err, awsauth.ErrNoCredentials) {
		t.Fatalf("Apply = %v, want ErrNoCredentials", err)
	}
}
