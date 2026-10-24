package providers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Every keyed LLM driver tags an auth refusal with the key it sent, so the
// loop can tell the operator's dead key (provider down for everyone) from a
// tenant's (that run only).
func TestDrivers_AuthRefusalNamesTheKeyThatWasRefused(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"bad key"}`, status)
		}))
		for _, driver := range []string{"anthropic", "openai", "deepseek", "gemini", "ollama"} {
			p, err := providers.NewDriver(driver, providers.DriverOptions{
				ID: driver, BaseURL: srv.URL, APIKey: "host-key", KeyEnvName: "TEST_KEYSOURCE_API_KEY",
			})
			if err != nil {
				t.Fatalf("NewDriver(%s): %v", driver, err)
			}
			req := providers.Request{Model: "m", MaxTokens: 16, Messages: []providers.Message{
				{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			}}

			_, opErr := p.Call(context.Background(), req)
			if !providers.OperatorKeyRefused(opErr) {
				t.Errorf("%s %d on the operator key: OperatorKeyRefused = false (err %v)", driver, status, opErr)
			}

			tenantCtx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
				return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant"}, name == "TEST_KEYSOURCE_API_KEY", nil
			})
			_, tErr := p.Call(tenantCtx, req)
			if tErr == nil || providers.OperatorKeyRefused(tErr) {
				t.Errorf("%s %d on a tenant key: err %v, OperatorKeyRefused = %v, want an error that is not an operator refusal",
					driver, status, tErr, providers.OperatorKeyRefused(tErr))
			}
		}
		srv.Close()
	}
}
