package ollama

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Hosted ollama.com honors an OLLAMA_API_KEY override; ollama-local is
// unauthenticated local-network, so no override ever applies there (RFC AR).
func TestCallKey_HostedOverride_LocalNever(t *testing.T) {
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-ollama"}, name == "OLLAMA_API_KEY"
	})

	hosted := &Driver{providerID: "ollama", apiKey: "host-key", keyEnvName: "OLLAMA_API_KEY"}
	if got, _, _, err := hosted.resolveKey(ctx); err != nil || got != "tenant-ollama" {
		t.Errorf("hosted with override: resolveKey = (%q, %v), want (tenant-ollama, nil)", got, err)
	}
	if got, _, _, err := hosted.resolveKey(context.Background()); err != nil || got != "host-key" {
		t.Errorf("hosted no resolver: resolveKey = (%q, %v), want (host-key, nil)", got, err)
	}

	local := &Driver{providerID: "ollama-local", apiKey: ""}
	if got, _, _, err := local.resolveKey(ctx); err != nil || got != "" {
		t.Errorf("ollama-local must never take an override: resolveKey = (%q, %v), want (\"\", nil)", got, err)
	}
}

// RFC AX: a RESTRICTED run refuses on hosted ollama (never the host key) but
// ollama-local is NEVER restricted — it has no operator key to protect, so it
// returns its (empty) key directly even when operator-key access is withheld.
func TestResolveKey_RestrictedHostedRefuses_LocalUnaffected(t *testing.T) {
	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)

	hosted := &Driver{providerID: "ollama", apiKey: "host-key", keyEnvName: "OLLAMA_API_KEY"}
	if got, _, _, err := hosted.resolveKey(restricted); !errors.Is(err, providers.ErrOperatorKeyForbidden) || got != "" {
		t.Errorf("hosted restricted: (%q, %v), want (\"\", ErrOperatorKeyForbidden)", got, err)
	}

	local := &Driver{providerID: "ollama-local", apiKey: ""}
	if got, _, _, err := local.resolveKey(restricted); err != nil || got != "" {
		t.Errorf("ollama-local restricted must NOT refuse: (%q, %v), want (\"\", nil)", got, err)
	}
}
