package config

import (
	"reflect"
	"testing"
)

// The models the metrics may name are the ones the operator configured,
// wherever the config names one; nothing else.
func TestConfig_MetricModelNamesCollectsTheOperatorsModels(t *testing.T) {
	c := &Config{
		Defaults: Defaults{Model: "default-m"},
		Models: map[string]ModelRef{
			"fast":  {Provider: "anthropic", Model: "claude-x"},
			"haiku": {Provider: "anthropic", ModelPattern: "claude-haiku-*"},
		},
		Tiers:     map[string][]TierCandidate{"middle": {{Provider: "openai", Model: "gpt-tier"}}},
		UserTiers: map[string]UserTier{"pro": {Tiers: map[string][]TierCandidate{"high": {{Model: "user-tier-m"}}}}},
		Agents: map[string]AgentDef{
			"a": {Model: "agent-m", Models: map[string][]TierCandidate{"low": {{Model: "agent-tier-m"}}}},
		},
		Pricing: PricingConfig{Models: map[string]ModelPrice{"openrouter/meta/llama": {}}},
	}
	names, patterns := c.MetricModelNames()
	want := []string{"agent-m", "agent-tier-m", "claude-x", "default-m", "fast", "gpt-tier", "haiku", "meta/llama", "user-tier-m"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if !reflect.DeepEqual(patterns, []string{"claude-haiku-*"}) {
		t.Errorf("patterns = %v", patterns)
	}
	if n, p := (*Config)(nil).MetricModelNames(); n != nil || p != nil {
		t.Errorf("nil config = %v %v", n, p)
	}
}
