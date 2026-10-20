package providerbuild

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/awsauth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic" // registers the driver
)

// awsEnv gives the credential chain fixed env keys and nothing else.
func awsEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"AWS_PROFILE", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDPROVIDERBUILD")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "providerbuild-secret-0123456789")
}

func authProvider(baseURL string) config.ProviderConfig {
	return config.ProviderConfig{Driver: "anthropic", BaseURL: baseURL,
		Auth: &config.ProviderAuth{Type: config.ProviderAuthSigV4, Region: "eu-central-1", Service: "bedrock"}}
}

// An auth provider gets AWS authentication, and a tenant override only under
// the Bedrock key's own name: never ANTHROPIC_API_KEY, which would send a
// tenant's Anthropic key to AWS.
func TestDriverOptions_AuthProviderUsesAWSAndTheBedrockKeyName(t *testing.T) {
	awsEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "operator-anthropic-key")
	o := DriverOptions("bedrock-claude", authProvider("https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic"), &config.Config{})
	if _, ok := o.RequestAuth.(*awsauth.Auth); !ok {
		t.Fatalf("RequestAuth = %T", o.RequestAuth)
	}
	if o.KeyEnvName != BedrockKeyEnvName || o.APIKey != "" {
		t.Errorf("KeyEnvName=%q APIKey set=%v", o.KeyEnvName, o.APIKey != "")
	}
}

// End to end through the registry factory: the driver signs with the chain's
// credentials, as region and service say, and sends no key.
func TestProvider_AuthProviderSignsItsCalls(t *testing.T) {
	awsEnv(t)
	var auth, apiKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, apiKey = r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"bedrock-claude": authProvider(srv.URL)}}
	p, err := Provider(cfg, "bedrock-claude")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Call(context.Background(), providers.Request{Model: "eu.anthropic.claude-sonnet-5-5",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDPROVIDERBUILD/") || !strings.Contains(auth, "/eu-central-1/bedrock/aws4_request") {
		t.Errorf("Authorization = %q", auth)
	}
	if apiKey != "" {
		t.Errorf("x-api-key sent: %q", apiKey)
	}
}

// A service block pointed at another endpoint, or naming its own key, does
// not carry the provider's AWS authentication there.
func TestServiceDriverOptions_OwnEndpointOrKeyDropsAWSAuth(t *testing.T) {
	awsEnv(t)
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"bedrock-claude": authProvider("https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic")}}
	o, _, _, _, err := ServiceDriverOptions(cfg, "memory.reranker", ServiceEndpoint{Provider: "bedrock-claude", Model: "m"}, "OWN")
	if err != nil || o.RequestAuth == nil {
		t.Fatalf("same endpoint: auth=%v err=%v", o.RequestAuth, err)
	}
	for name, ep := range map[string]ServiceEndpoint{
		"own base_url": {Provider: "bedrock-claude", Model: "m", BaseURL: "https://rerank.internal/v1"},
		"own key":      {Provider: "bedrock-claude", Model: "m", APIKeyEnv: "RERANK_KEY"},
	} {
		o, _, _, _, err := ServiceDriverOptions(cfg, "memory.reranker", ep, "OWN")
		if err != nil || o.RequestAuth != nil {
			t.Errorf("%s: auth=%v err=%v", name, o.RequestAuth, err)
		}
	}
}

// Both ways a server builds a provider (the provider set, and a service block
// such as the reranker) hand the AWS secrets they obtain to cfg.SecretSink,
// so the redactor masks them whichever path made the call.
func TestEveryConstructionPath_RegistersAWSSecrets(t *testing.T) {
	awsEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()
	build := map[string]func(cfg *config.Config) (providers.Provider, error){
		"provider set": func(cfg *config.Config) (providers.Provider, error) { return Provider(cfg, "bedrock-claude") },
		"service block": func(cfg *config.Config) (providers.Provider, error) {
			p, _, _, err := ServiceProvider(cfg, "memory.reranker", ServiceEndpoint{Provider: "bedrock-claude", Model: "m"}, "OWN")
			return p, err
		},
	}
	for name, mk := range build {
		t.Run(name, func(t *testing.T) {
			var got []string
			cfg := &config.Config{
				Providers:  map[string]config.ProviderConfig{"bedrock-claude": authProvider(srv.URL)},
				SecretSink: func(v string) { got = append(got, v) },
			}
			p, err := mk(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := p.Call(context.Background(), providers.Request{Model: "m",
				Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			found := false
			for _, v := range got {
				found = found || v == "providerbuild-secret-0123456789"
			}
			if !found {
				t.Errorf("the secret access key never reached the sink (%d values did)", len(got))
			}
		})
	}
}
