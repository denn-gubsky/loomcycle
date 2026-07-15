package anthropic

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestNewDriver_ForwardsKeyEnvName is the RFC BF review-finding regression: a
// config-declared api_key_env must reach the driver's KeyEnvName() so a custom-id
// anthropic provider resolves RFC AR/AX tenant credential overrides under its OWN
// var — matching the var toDriverOptions read the host key from. Before the fix
// the anthropic driver had no SetKeyEnvName and KeyEnvName() was hardcoded, so
// the declared api_key_env was dropped for tenant-override resolution.
func TestNewDriver_ForwardsKeyEnvName(t *testing.T) {
	p, err := providers.NewDriver("anthropic", providers.DriverOptions{APIKey: "host", KeyEnvName: "MY_CLAUDE_KEY"})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	kp, ok := p.(providers.KeyedProvider)
	if !ok {
		t.Fatal("anthropic driver must implement KeyedProvider")
	}
	if got := kp.KeyEnvName(); got != "MY_CLAUDE_KEY" {
		t.Errorf("KeyEnvName() = %q, want MY_CLAUDE_KEY (config api_key_env must be forwarded)", got)
	}
}

// TestNewDriver_DefaultKeyEnvName confirms back-compat: with no declared
// api_key_env the driver keeps the canonical default.
func TestNewDriver_DefaultKeyEnvName(t *testing.T) {
	p, err := providers.NewDriver("anthropic", providers.DriverOptions{APIKey: "host"})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if got := p.(providers.KeyedProvider).KeyEnvName(); got != "ANTHROPIC_API_KEY" {
		t.Errorf("default KeyEnvName() = %q, want ANTHROPIC_API_KEY", got)
	}
}
