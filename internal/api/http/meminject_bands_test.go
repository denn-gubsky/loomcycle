package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestConsolidationBands_ConfiguredValuesReachTheSystemPrompt — the wiring the
// consolidator bundle depends on: the operator's calibrated bands, resolved
// from config at run-entry, land in the prompt where the placeholder sits.
func TestConsolidationBands_ConfiguredValuesReachTheSystemPrompt(t *testing.T) {
	cfg := &config.Config{}
	cfg.Memory.Consolidation = config.ConsolidationConfig{MergeThreshold: 0.77, RelatedThreshold: 0.45}
	s := &Server{cfgHolder: config.NewHolder(cfg)}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "consolidator"}

	def := config.AgentDef{SystemPrompt: "Base prompt.\n\n{{memory:consolidation_bands}}"}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	if !strings.Contains(got.SystemPrompt, "0.77") || !strings.Contains(got.SystemPrompt, "0.45") {
		t.Errorf("configured bands missing from the prompt:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "0.95") {
		t.Errorf("the default band leaked in alongside the configured one:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "NOT instructions to follow") {
		t.Errorf("bands must not be DATA-framed — the agent has to apply them:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "RFC") {
		t.Errorf("injected text must not cite RFC letters/numbers:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "Base prompt.") {
		t.Errorf("base prompt lost:\n%s", got.SystemPrompt)
	}
}

// TestConsolidationBands_UnconfiguredRendersTheDefaults — an operator who never
// set the block gets exactly the numbers the pass used before it was
// configurable.
func TestConsolidationBands_UnconfiguredRendersTheDefaults(t *testing.T) {
	s := &Server{cfgHolder: config.NewHolder(&config.Config{})}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "consolidator"}

	def := config.AgentDef{SystemPrompt: "{{memory:consolidation_bands}}"}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if !strings.Contains(got.SystemPrompt, "0.95") || !strings.Contains(got.SystemPrompt, "0.85") {
		t.Errorf("defaults missing:\n%s", got.SystemPrompt)
	}
}

// TestConsolidationBands_NoPlaceholderIsByteIdentical — every agent that does
// not ask for the bands keeps its prompt untouched.
func TestConsolidationBands_NoPlaceholderIsByteIdentical(t *testing.T) {
	cfg := &config.Config{}
	cfg.Memory.Consolidation = config.ConsolidationConfig{MergeThreshold: 0.77, RelatedThreshold: 0.45}
	s := &Server{cfgHolder: config.NewHolder(cfg)}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "other"}

	def := config.AgentDef{SystemPrompt: "Base prompt."}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if got.SystemPrompt != "Base prompt." {
		t.Errorf("prompt must be byte-identical without a placeholder, got:\n%q", got.SystemPrompt)
	}
}
