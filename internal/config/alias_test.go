package config

import "testing"

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
