package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// runDoctorWith runs doctor over stub and returns its output and exit code.
func runDoctorWith(t *testing.T, stub *stubConfig) (string, int) {
	t.Helper()
	withTempHome(t)
	stub.backend, stub.dataDir, stub.listen = "sqlite", t.TempDir(), freePort(t)
	withStubLoader(t, stub)
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)
	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	return stdout.String(), code
}

func withListModels(t *testing.T, ids []string, err error) *[]AWSAuthProvider {
	t.Helper()
	var asked []AWSAuthProvider
	orig := awsListModelsForDoctor
	t.Cleanup(func() { awsListModelsForDoctor = orig })
	awsListModelsForDoctor = func(_ context.Context, p AWSAuthProvider) ([]string, error) {
		asked = append(asked, p)
		return ids, err
	}
	return &asked
}

var bedrockAWS = AWSAuthProvider{
	BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic",
	Auth:    config.ProviderAuth{Type: config.ProviderAuthSigV4, Region: "eu-central-1"},
}

// TestDoctor_BedrockEmbedderIsReportedAndItsModelListed — RFC EV §5.5: doctor
// names the embedder (provider, key, model, dimension), says whose AWS
// credentials it uses, and passes when the region lists the model.
func TestDoctor_BedrockEmbedderIsReportedAndItsModelListed(t *testing.T) {
	asked := withListModels(t, []string{"cohere.embed-multilingual-v3", "eu.anthropic.claude-sonnet-5-5", "eu.cohere.embed-v4:0"}, nil)
	out, code := runDoctorWith(t, &stubConfig{embedder: &DoctorEmbedder{
		Provider: "bedrock-claude", Key: "bedrock", Model: "eu.cohere.embed-v4:0", Dimension: 1024, AWS: &bedrockAWS}})
	if code != 0 {
		t.Errorf("exit = %d:\n%s", code, out)
	}
	for _, want := range []string{"[PASS]  Embedder", "bedrock-claude", "bedrock embedder", "eu.cohere.embed-v4:0", "1024", "provider bedrock-claude's AWS credentials", "listed in eu-central-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(*asked) != 1 || (*asked)[0].BaseURL != bedrockAWS.BaseURL {
		t.Errorf("ListModels asked of %+v, want the provider's auth once", *asked)
	}
}

// TestDoctor_BedrockEmbedderModelNotListedWarns — a model the region does not
// list (wrong id, wrong region, no access) is a WARN, as is a listing that
// failed; neither fails doctor.
func TestDoctor_BedrockEmbedderModelNotListedWarns(t *testing.T) {
	for name, tc := range map[string]struct {
		ids  []string
		err  error
		want string
	}{
		"not listed":     {[]string{"cohere.embed-multilingual-v3"}, nil, "not listed in eu-central-1"},
		"listing failed": {nil, errors.New("bedrock model listing: status 403 (AccessDeniedException)"), "could not list"},
	} {
		t.Run(name, func(t *testing.T) {
			withListModels(t, tc.ids, tc.err)
			out, code := runDoctorWith(t, &stubConfig{embedder: &DoctorEmbedder{
				Provider: "bedrock-claude", Key: "bedrock", Model: "eu.cohere.embed-v4:0", Dimension: 1536, AWS: &bedrockAWS}})
			if code != 0 {
				t.Errorf("exit = %d, a warning must not fail doctor:\n%s", code, out)
			}
			if !strings.Contains(out, "[WARN]  Embedder") || !strings.Contains(out, tc.want) {
				t.Errorf("want a WARN mentioning %q in:\n%s", tc.want, out)
			}
		})
	}
}

// TestDoctor_OtherEmbedderIsReportedWithoutListing — any configured embedder
// gets its line; only a Bedrock one is listed against AWS.
func TestDoctor_OtherEmbedderIsReportedWithoutListing(t *testing.T) {
	asked := withListModels(t, nil, nil)
	out, _ := runDoctorWith(t, &stubConfig{embedder: &DoctorEmbedder{
		Provider: "ollama-local", Key: "ollama-local", Model: "bge-m3", Dimension: 0}})
	if !strings.Contains(out, "[PASS]  Embedder") || !strings.Contains(out, "bge-m3") || !strings.Contains(out, "learned on the first call") {
		t.Errorf("missing the embedder line in:\n%s", out)
	}
	if len(*asked) != 0 {
		t.Errorf("listed models for a non-Bedrock embedder")
	}
}

// TestDoctor_EmbedderThatCannotBuildFails — the server exits at boot when the
// embedder cannot be built, so doctor fails it; no embedder prints nothing.
func TestDoctor_EmbedderThatCannotBuildFails(t *testing.T) {
	out, code := runDoctorWith(t, &stubConfig{embedderErr: errors.New("bedrock embedder: model \"x\" is not a Cohere Embed text model")})
	if code == 0 || !strings.Contains(out, "[FAIL]  Embedder") {
		t.Errorf("exit = %d, want a FAIL:\n%s", code, out)
	}
	out, _ = runDoctorWith(t, &stubConfig{})
	if strings.Contains(out, "]  Embedder") {
		t.Errorf("an embedder line with none configured:\n%s", out)
	}
}

// TestDoctorConfig_EmbedderReadsTheBuiltBedrockEmbedder — the real adapter
// reports what the server would build: the bedrock key, the configured width
// and the provider's AWS auth.
func TestDoctorConfig_EmbedderReadsTheBuiltBedrockEmbedder(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "")
	// Nothing is fetched while building, but the chain still reads its files:
	// keep it off this machine's.
	awsDir := t.TempDir()
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(awsDir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(awsDir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	path := filepath.Join(t.TempDir(), "loomcycle.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  bedrock-claude:
    driver: anthropic
    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic
    auth: { type: aws_sigv4, region: eu-central-1 }
models:
  cohere-embed: { provider: bedrock-claude, model: eu.cohere.embed-v4:0, kind: embedder }
memory:
  embedder: { model: cohere-embed, api: bedrock, dimensions: 1024 }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigRealAdapter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e, err := cfg.Embedder()
	if err != nil || e == nil {
		t.Fatalf("Embedder = %+v, %v", e, err)
	}
	if e.Provider != "bedrock-claude" || e.Key != "bedrock" || e.Model != "eu.cohere.embed-v4:0" || e.Dimension != 1024 {
		t.Errorf("Embedder = %+v", e)
	}
	if e.AWS == nil || e.AWS.BaseURL != "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic" || e.AWS.Auth.Region != "eu-central-1" {
		t.Errorf("AWS = %+v, want the provider's auth", e.AWS)
	}
}
