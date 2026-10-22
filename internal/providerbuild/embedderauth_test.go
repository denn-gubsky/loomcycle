package providerbuild

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/awsauth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC EV §5.2 — the embedder of an AWS-authenticated provider signs with that
// provider's credentials, built the way every other construction path builds
// them: lazily, with every secret handed to the redactor.
func TestEmbedderAuth_IsTheProvidersAWSAuthAndFeedsTheRedactor(t *testing.T) {
	awsEnv(t)
	var sunk []string
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"bedrock-claude": authProvider("https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic"),
			"openai":         {Driver: "openai"},
		},
		SecretSink: func(v string) { sunk = append(sunk, v) },
	}
	ra := EmbedderAuth(cfg, "bedrock-claude")
	a, ok := ra.(*awsauth.Auth)
	if !ok {
		t.Fatalf("EmbedderAuth = %T, want *awsauth.Auth", ra)
	}
	if a.Region() != "eu-central-1" || a.Service() != "bedrock" || a.Mode() != awsauth.ModeSigV4 {
		t.Errorf("auth = %s/%s/%s", a.Mode(), a.Region(), a.Service())
	}
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.eu-central-1.amazonaws.com/model/m/invoke", nil)
	if err := ra.Apply(context.Background(), req, nil, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(sunk, "providerbuild-secret-0123456789") {
		t.Errorf("the secret access key never reached the sink (%d values did)", len(sunk))
	}
	for _, id := range []string{"openai", "undeclared"} {
		if got := EmbedderAuth(cfg, id); got != nil {
			t.Errorf("EmbedderAuth(%s) = %T, want nil for a provider without auth:", id, got)
		}
	}
}

// A chain that cannot even be configured (a named profile that does not exist)
// must not fail the build — boot treats an embedder construction error as fatal
// — but every call fails with the reason.
func TestEmbedderAuth_UnconfigurableChainFailsEachCallNotTheBuild(t *testing.T) {
	awsEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "no-such-profile")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"bedrock-claude": authProvider("https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic")}}
	ra := EmbedderAuth(cfg, "bedrock-claude")
	if ra == nil {
		t.Fatal("EmbedderAuth = nil, want an auth that fails each call")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.eu-central-1.amazonaws.com/model/m/invoke", nil)
	if err := ra.Apply(context.Background(), req, nil, nil); !errors.Is(err, awsauth.ErrNoCredentials) {
		t.Fatalf("Apply = %v, want ErrNoCredentials", err)
	}
}
