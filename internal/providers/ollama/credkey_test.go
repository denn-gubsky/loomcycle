package ollama

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Hosted ollama.com honors an OLLAMA_API_KEY override; ollama-local is
// unauthenticated local-network, so no override ever applies there (RFC AR).
func TestCallKey_HostedOverride_LocalNever(t *testing.T) {
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-ollama", name == "OLLAMA_API_KEY"
	})

	hosted := &Driver{providerID: "ollama", apiKey: "host-key"}
	if got := hosted.callKey(ctx); got != "tenant-ollama" {
		t.Errorf("hosted with override: callKey = %q, want tenant-ollama", got)
	}
	if got := hosted.callKey(context.Background()); got != "host-key" {
		t.Errorf("hosted no resolver: callKey = %q, want host-key", got)
	}

	local := &Driver{providerID: "ollama-local", apiKey: ""}
	if got := local.callKey(ctx); got != "" {
		t.Errorf("ollama-local must never take an override: callKey = %q, want empty", got)
	}
}
