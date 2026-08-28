package config

import (
	"reflect"
	"testing"
)

// TestChangedSections is the RFC CK section-diff: two configs are compared by
// top-level YAML section, and only the sections that actually differ are
// reported (sorted). Env-derived fields are identical between two loads of the
// same process env, so they never surface.
func TestChangedSections(t *testing.T) {
	base := &Config{
		Providers:        map[string]ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://a:11434"}},
		ProviderPriority: []string{"ollama-local"},
		Tiers:            map[string][]TierCandidate{"low": {{Provider: "ollama-local", Model: "m1"}}},
	}

	// (1) identical → no changes.
	same := &Config{
		Providers:        map[string]ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://a:11434"}},
		ProviderPriority: []string{"ollama-local"},
		Tiers:            map[string][]TierCandidate{"low": {{Provider: "ollama-local", Model: "m1"}}},
	}
	got, err := ChangedSections(base, same)
	if err != nil {
		t.Fatalf("ChangedSections: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("identical configs reported changed sections %v, want none", got)
	}

	// (2) one section (providers.base_url) changed → only "providers".
	newBase := &Config{
		Providers:        map[string]ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://b:11434"}},
		ProviderPriority: []string{"ollama-local"},
		Tiers:            map[string][]TierCandidate{"low": {{Provider: "ollama-local", Model: "m1"}}},
	}
	got, err = ChangedSections(base, newBase)
	if err != nil {
		t.Fatalf("ChangedSections: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"providers"}) {
		t.Errorf("changed sections = %v, want [providers]", got)
	}

	// (3) two sections changed → both, sorted.
	twoChanged := &Config{
		Providers:        map[string]ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://b:11434"}},
		ProviderPriority: []string{"ollama-local", "openai"},
		Tiers:            map[string][]TierCandidate{"low": {{Provider: "ollama-local", Model: "m1"}}},
	}
	got, err = ChangedSections(base, twoChanged)
	if err != nil {
		t.Fatalf("ChangedSections: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"provider_priority", "providers"}) {
		t.Errorf("changed sections = %v, want [provider_priority providers]", got)
	}
}
