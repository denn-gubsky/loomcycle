package gemini

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestNewDriver_ForwardsKeyEnvName is the RFC BF review-finding regression: a
// config-declared api_key_env must reach KeyEnvName() so a custom-id gemini
// provider resolves RFC AR/AX tenant credential overrides under its OWN var.
// Before the fix the gemini driver had no SetKeyEnvName and KeyEnvName() was
// hardcoded, dropping the declared api_key_env for tenant-override resolution.
func TestNewDriver_ForwardsKeyEnvName(t *testing.T) {
	p, err := providers.NewDriver("gemini", providers.DriverOptions{APIKey: "host", KeyEnvName: "MY_GEMINI_KEY"})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	kp, ok := p.(providers.KeyedProvider)
	if !ok {
		t.Fatal("gemini driver must implement KeyedProvider")
	}
	if got := kp.KeyEnvName(); got != "MY_GEMINI_KEY" {
		t.Errorf("KeyEnvName() = %q, want MY_GEMINI_KEY (config api_key_env must be forwarded)", got)
	}
}

// TestNewDriver_DefaultKeyEnvName confirms back-compat: with no declared
// api_key_env the driver keeps the canonical default.
func TestNewDriver_DefaultKeyEnvName(t *testing.T) {
	p, err := providers.NewDriver("gemini", providers.DriverOptions{APIKey: "host"})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if got := p.(providers.KeyedProvider).KeyEnvName(); got != "GEMINI_API_KEY" {
		t.Errorf("default KeyEnvName() = %q, want GEMINI_API_KEY", got)
	}
}
