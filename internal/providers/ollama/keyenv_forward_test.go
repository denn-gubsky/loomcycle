package ollama

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestNewDriver_ForwardsKeyEnvName is the RFC BF review-finding regression: a
// config-declared api_key_env must reach KeyEnvName() so a custom-id
// ollama-driver provider is keyable under its OWN var. Before the fix ollama's
// KeyEnvName was derived solely from providerID, so a custom id with a declared
// api_key_env was treated as keyless and its tenant-override resolution was lost.
func TestNewDriver_ForwardsKeyEnvName(t *testing.T) {
	p, err := providers.NewDriver("ollama", providers.DriverOptions{ID: "ollama-remote", APIKey: "host", KeyEnvName: "MY_OLLAMA_KEY"})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	kp, ok := p.(providers.KeyedProvider)
	if !ok {
		t.Fatal("ollama driver must implement KeyedProvider")
	}
	if got := kp.KeyEnvName(); got != "MY_OLLAMA_KEY" {
		t.Errorf("KeyEnvName() = %q, want MY_OLLAMA_KEY (config api_key_env must be forwarded)", got)
	}
}

// TestNewDriver_DefaultKeyEnvName_ByID confirms back-compat: the hosted "ollama"
// id defaults to OLLAMA_API_KEY and keyless "ollama-local" stays "" (never
// keyable) when no api_key_env is declared.
func TestNewDriver_DefaultKeyEnvName_ByID(t *testing.T) {
	hosted, err := providers.NewDriver("ollama", providers.DriverOptions{ID: "ollama"})
	if err != nil {
		t.Fatalf("NewDriver(ollama): %v", err)
	}
	if got := hosted.(providers.KeyedProvider).KeyEnvName(); got != "OLLAMA_API_KEY" {
		t.Errorf("hosted default KeyEnvName() = %q, want OLLAMA_API_KEY", got)
	}
	local, err := providers.NewDriver("ollama", providers.DriverOptions{ID: "ollama-local"})
	if err != nil {
		t.Fatalf("NewDriver(ollama-local): %v", err)
	}
	if got := local.(providers.KeyedProvider).KeyEnvName(); got != "" {
		t.Errorf("ollama-local KeyEnvName() = %q, want \"\" (keyless)", got)
	}
}
