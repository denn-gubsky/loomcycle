package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTierCandidate_UnmarshalYAML_BareStringIsModel locks the bare-string
// authoring form: `- local-qwen` parses to {Provider:"", Model:"local-qwen"}
// (the alias supplies the provider downstream via ExpandModelAlias), while the
// mapping form still works. Fail-before: without the custom UnmarshalYAML, a
// bare scalar fails with "cannot unmarshal !!str into config.TierCandidate".
func TestTierCandidate_UnmarshalYAML_BareStringIsModel(t *testing.T) {
	var doc struct {
		Tiers map[string][]TierCandidate `yaml:"tiers"`
	}
	src := "tiers:\n" +
		"  middle:\n" +
		"    - local-qwen\n" +
		"    - { provider: deepseek, model: deepseek-v4-pro }\n"
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mid := doc.Tiers["middle"]
	if len(mid) != 2 {
		t.Fatalf("got %d candidates, want 2 (%+v)", len(mid), mid)
	}
	if mid[0] != (TierCandidate{Provider: "", Model: "local-qwen"}) {
		t.Errorf("bare candidate = %+v, want {Provider: Model:local-qwen}", mid[0])
	}
	if mid[1] != (TierCandidate{Provider: "deepseek", Model: "deepseek-v4-pro"}) {
		t.Errorf("mapping candidate = %+v", mid[1])
	}
}

// TestValidateTierCandidate_AliasAware locks the config-load carve-out: a bare
// alias (empty provider, model is a defined models: alias) passes validation
// because the alias supplies the provider at resolve time. Fail-before:
// without the carve-out, an empty-provider candidate fails `unknown provider
// ""` and an all-aliases tier list won't load.
func TestValidateTierCandidate_AliasAware(t *testing.T) {
	models := map[string]ModelRef{
		"local-qwen": {Provider: "ollama-local", Model: "qwen3.6:max"},
	}
	if err := validateTierCandidate(TierCandidate{Model: "local-qwen"}, models); err != nil {
		t.Errorf("bare alias rejected: %v", err)
	}
	if err := validateTierCandidate(TierCandidate{Model: "not-an-alias"}, models); err == nil {
		t.Error("empty provider + non-alias model accepted, want error")
	}
	if err := validateTierCandidate(TierCandidate{Provider: "deepseek", Model: "deepseek-v4-pro"}, models); err != nil {
		t.Errorf("explicit literal candidate rejected: %v", err)
	}
	if err := validateTierCandidate(TierCandidate{Provider: "bogus", Model: "x"}, models); err == nil {
		t.Error("unknown provider accepted, want error")
	}
	if err := validateTierCandidate(TierCandidate{Provider: "deepseek"}, models); err == nil {
		t.Error("explicit provider + empty model accepted, want error")
	}
}

// TestExpandModelAlias_* lock the single source of truth for model-alias
// expansion shared by the pin path (ResolveAgentDefModel) and the tier path
// (the resolver-boundary converters). Each behaviour is the same rule the pin
// path always used; the regression these guard is the tier path NOT honouring
// aliases, which produced a 503 "no provider available for requested tier".
func TestExpandModelAlias_ExpandsModelAndFillsEmptyProvider(t *testing.T) {
	models := map[string]ModelRef{
		"local-gemma": {Provider: "ollama-local", Model: "gemma4:max"},
	}
	prov, mdl := ExpandModelAlias(models, "", "local-gemma")
	if prov != "ollama-local" || mdl != "gemma4:max" {
		t.Fatalf("got (%q, %q), want (ollama-local, gemma4:max)", prov, mdl)
	}
}

func TestExpandModelAlias_ExplicitProviderWins(t *testing.T) {
	// A candidate that names both a provider and an alias keeps its explicit
	// provider — the alias only fills an EMPTY provider, mirroring the pin
	// path. The model is still expanded.
	models := map[string]ModelRef{
		"local-gemma": {Provider: "ollama-local", Model: "gemma4:max"},
	}
	prov, mdl := ExpandModelAlias(models, "openai", "local-gemma")
	if prov != "openai" || mdl != "gemma4:max" {
		t.Fatalf("got (%q, %q), want (openai, gemma4:max)", prov, mdl)
	}
}

func TestExpandModelAlias_NonAliasIsLiteralNoop(t *testing.T) {
	models := map[string]ModelRef{
		"local-gemma": {Provider: "ollama-local", Model: "gemma4:max"},
	}
	prov, mdl := ExpandModelAlias(models, "anthropic", "claude-sonnet-4-6")
	if prov != "anthropic" || mdl != "claude-sonnet-4-6" {
		t.Fatalf("got (%q, %q), want (anthropic, claude-sonnet-4-6)", prov, mdl)
	}
}

func TestExpandModelAlias_NilMapNoop(t *testing.T) {
	prov, mdl := ExpandModelAlias(nil, "ollama-local", "local-gemma")
	if prov != "ollama-local" || mdl != "local-gemma" {
		t.Fatalf("got (%q, %q), want (ollama-local, local-gemma)", prov, mdl)
	}
}
