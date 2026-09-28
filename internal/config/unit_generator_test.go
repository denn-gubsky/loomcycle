package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestUnitGeneratorConfig_LoadsAndResolvesAnAlias(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  local-medium: { provider: ollama, model: qwen3.6:latest }
memory:
  unit_generator:
    model: local-medium
    effort: low
    subtrees:
      - { path: /docs/policies }
      - { path: /docs/policies/faq, kinds: [questions] }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := cfg.Memory.UnitGenerator
	if !g.Configured() {
		t.Fatal("a block naming only an alias is declared")
	}
	for path, want := range map[string][]string{
		"/docs/policies":            UnitKinds,
		"/docs/policies/leave":      UnitKinds,
		"/docs/policies/faq/hr":     {"questions"}, // the deepest subtree wins
		"/docs/policiesx/unrelated": nil,
	} {
		got, ok := g.SubtreeKinds(path)
		if ok != (want != nil) || !reflect.DeepEqual(got, want) {
			t.Errorf("SubtreeKinds(%q) = %v,%v, want %v", path, got, ok, want)
		}
	}
}

// TestUnitGeneratorConfig_NeverCoversTheMemoryTrees — /facts and /memory hold
// memory's own derived data; marking them would feed memory's output back into its
// input, so it fails LOAD rather than being silently skipped.
func TestUnitGeneratorConfig_NeverCoversTheMemoryTrees(t *testing.T) {
	for _, bad := range []string{
		"subtrees: [{ path: /facts }]",
		"subtrees: [{ path: /memory/ontology }]",
		"subtrees: [{ path: / }]",
		"subtrees: [{ path: docs }]",
		"subtrees: [{ path: /docs, kinds: [summaries] }]",
		"effort: max",
	} {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  unit_generator:
    provider: ollama
    model: m
    `+bad+`
`))
		if err == nil || !strings.Contains(err.Error(), "memory.unit_generator") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	for _, p := range []string{"/facts", "/facts/ada", "/memory", "/memory/tenant-root"} {
		if !UnitsExcludedPath(p) {
			t.Errorf("%s must be excluded", p)
		}
	}
	for _, p := range []string{"/factsheet", "/docs/memory", "/documents/facts"} {
		if UnitsExcludedPath(p) {
			t.Errorf("%s is not in a memory tree", p)
		}
	}
}
