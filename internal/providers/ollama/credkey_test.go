package ollama

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Hosted ollama.com honors an OLLAMA_API_KEY override; ollama-local is
// unauthenticated local-network, so no override ever applies there (RFC AR).
func TestCallKey_HostedOverride_LocalNever(t *testing.T) {
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-ollama"}, name == "OLLAMA_API_KEY"
	})

	hosted := &Driver{providerID: "ollama", apiKey: "host-key"}
	if got, _, _ := hosted.resolveKey(ctx); got != "tenant-ollama" {
		t.Errorf("hosted with override: resolveKey = %q, want tenant-ollama", got)
	}
	if got, _, _ := hosted.resolveKey(context.Background()); got != "host-key" {
		t.Errorf("hosted no resolver: resolveKey = %q, want host-key", got)
	}

	local := &Driver{providerID: "ollama-local", apiKey: ""}
	if got, _, _ := local.resolveKey(ctx); got != "" {
		t.Errorf("ollama-local must never take an override: resolveKey = %q, want empty", got)
	}
}
