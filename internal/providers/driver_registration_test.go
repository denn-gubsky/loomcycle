package providers_test

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"

	// Blank imports trigger each driver package's init(), which registers it
	// with the RFC BF driver registry. main.go imports all of these on the
	// hardcoded resolver path too — the registration is a pure side effect, so
	// importing them here exercises the real init()s without any wiring.
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

// TestRegisteredDrivers_TheSeven locks that exactly the seven LLM drivers
// self-register — anthropic-oauth-dev (residual, keeps its own path per RFC BF)
// and mock-stable (a resolver-side wrapper, not a config-declared driver) do
// NOT.
func TestRegisteredDrivers_TheSeven(t *testing.T) {
	want := []string{"anthropic", "code-js", "deepseek", "gemini", "mock", "ollama", "openai"}
	got := providers.RegisteredDrivers()
	set := map[string]bool{}
	for _, d := range got {
		set[d] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("driver %q not registered (got %v)", w, got)
		}
	}
	for _, absent := range []string{"anthropic-oauth-dev", "mock-stable"} {
		if set[absent] {
			t.Errorf("driver %q should NOT be registered (residual/wrapper), got %v", absent, got)
		}
	}
}

// TestNewDriver_OpenAI_IDAndCapsPatchAndInterfaces proves the seam end to end
// for the openai driver: the factory-built instance reports the configured
// DriverOptions.ID, reflects a capability override inside Capabilities(), and —
// critically — still satisfies the optional KeyedProvider interface (the
// in-driver capsPatch approach preserves it; an external wrapper would not).
func TestNewDriver_OpenAI_IDAndCapsPatchAndInterfaces(t *testing.T) {
	tru := true
	p, err := providers.NewDriver("openai", providers.DriverOptions{
		ID:           "openai-eu",
		APIKey:       "unused-in-this-test",
		KeyEnvName:   "OPENAI_EU_API_KEY",
		Capabilities: &providers.CapabilityPatch{SupportsThinking: &tru},
	})
	if err != nil {
		t.Fatalf("NewDriver(openai): %v", err)
	}
	if p.ID() != "openai-eu" {
		t.Errorf("ID() = %q, want the configured %q", p.ID(), "openai-eu")
	}
	// Base openai SupportsThinking is false; the override must flip it.
	if !p.Capabilities().SupportsThinking {
		t.Error("capsPatch override not reflected in Capabilities().SupportsThinking")
	}
	kp, ok := p.(providers.KeyedProvider)
	if !ok {
		t.Fatal("factory-built openai driver must still satisfy providers.KeyedProvider")
	}
	if kp.KeyEnvName() != "OPENAI_EU_API_KEY" {
		t.Errorf("KeyEnvName() = %q, want the forwarded %q", kp.KeyEnvName(), "OPENAI_EU_API_KEY")
	}
}

// TestNewDriver_DeepSeek_PreservesThinkingDowngrader proves the deepseek factory
// preserves the optional ThinkingDowngrader interface (an external wrapper type
// would break the loop's x.(ThinkingDowngrader) assertion).
func TestNewDriver_DeepSeek_PreservesThinkingDowngrader(t *testing.T) {
	p, err := providers.NewDriver("deepseek", providers.DriverOptions{ID: "deepseek"})
	if err != nil {
		t.Fatalf("NewDriver(deepseek): %v", err)
	}
	td, ok := p.(providers.ThinkingDowngrader)
	if !ok {
		t.Fatal("factory-built deepseek driver must still satisfy providers.ThinkingDowngrader")
	}
	if sib, down := td.NonThinkingSibling("deepseek-reasoner"); !down || sib != "deepseek-chat" {
		t.Errorf("NonThinkingSibling(deepseek-reasoner) = (%q,%v), want (deepseek-chat,true)", sib, down)
	}
	// deepseek must also stay a KeyedProvider (its key env is DEEPSEEK_API_KEY).
	if kp, ok := p.(providers.KeyedProvider); !ok || kp.KeyEnvName() != "DEEPSEEK_API_KEY" {
		t.Errorf("deepseek KeyedProvider = %v / %q, want true / DEEPSEEK_API_KEY", ok, keyEnv(p))
	}
}

// TestNewDriver_Ollama_IDDrivesKeyEnv proves the ONE ollama driver serves BOTH
// provider ids: built as "ollama-local" it is keyless (a restricted run may
// always route to it); the canonical "ollama" registration keys off
// OLLAMA_API_KEY.
func TestNewDriver_Ollama_IDDrivesKeyEnv(t *testing.T) {
	local, err := providers.NewDriver("ollama", providers.DriverOptions{ID: "ollama-local"})
	if err != nil {
		t.Fatalf("NewDriver(ollama, ollama-local): %v", err)
	}
	if local.ID() != "ollama-local" {
		t.Errorf("ID() = %q, want ollama-local", local.ID())
	}
	if kp, ok := local.(providers.KeyedProvider); ok && kp.KeyEnvName() != "" {
		t.Errorf("ollama-local KeyEnvName() = %q, want \"\" (keyless)", kp.KeyEnvName())
	}

	hosted, err := providers.NewDriver("ollama", providers.DriverOptions{ID: "ollama"})
	if err != nil {
		t.Fatalf("NewDriver(ollama, ollama): %v", err)
	}
	if kp, ok := hosted.(providers.KeyedProvider); !ok || kp.KeyEnvName() != "OLLAMA_API_KEY" {
		t.Errorf("hosted ollama KeyEnvName mismatch: ok=%v key=%q", ok, keyEnv(hosted))
	}
}

// TestNewDriver_Mock_IDAndCaps proves the mock factory honours DriverOptions.ID
// and a capability override (the mock is the cheapest full driver to build).
func TestNewDriver_Mock_IDAndCaps(t *testing.T) {
	no := false
	p, err := providers.NewDriver("mock", providers.DriverOptions{
		ID:           "mock",
		Capabilities: &providers.CapabilityPatch{SupportsVision: &no},
	})
	if err != nil {
		t.Fatalf("NewDriver(mock): %v", err)
	}
	if p.ID() != "mock" {
		t.Errorf("ID() = %q, want mock", p.ID())
	}
	// Base mock SupportsVision is true; the override must flip it to false.
	if p.Capabilities().SupportsVision {
		t.Error("capsPatch override not reflected: SupportsVision should be false")
	}
}

// keyEnv is a test helper: the KeyEnvName if p is a KeyedProvider, else "".
func keyEnv(p providers.Provider) string {
	if kp, ok := p.(providers.KeyedProvider); ok {
		return kp.KeyEnvName()
	}
	return ""
}
