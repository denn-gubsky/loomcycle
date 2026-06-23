package config

import (
	"testing"
)

// seqStrings decodes a merged map's sequence value to []string for assertions.
func seqStrings(t *testing.T, v any) []string {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("not a sequence: %#v", v)
	}
	out := make([]string, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			t.Fatalf("sequence item %d not a string: %#v", i, it)
		}
		out[i] = s
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMerge_PrependComposesNotReplaces (RFC AQ §3): an overlay sequence tagged
// !prepend merges its items in front of the base sequence instead of replacing —
// and is NOT recorded as a cross-layer override (it's a deliberate compose).
func TestMerge_PrependComposesNotReplaces(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", "provider_priority: [deepseek, openai, anthropic]\n")
	over := writeYAML(t, dir, "over.yaml", "provider_priority: !prepend [anthropic-oauth-dev]\n")
	merged, overrides, err := mergeConfigFiles([]string{base, over})
	if err != nil {
		t.Fatalf("mergeConfigFiles: %v", err)
	}
	got := seqStrings(t, merged["provider_priority"])
	want := []string{"anthropic-oauth-dev", "deepseek", "openai", "anthropic"}
	if !eqStrings(got, want) {
		t.Errorf("provider_priority = %v, want %v (prepend in front, no restatement)", got, want)
	}
	if len(overrides) != 0 {
		t.Errorf("a !prepend merge must NOT record a conflict; got %v", overrides)
	}
}

// TestMerge_AppendAndDedupKeepFirst: !append puts items after; a duplicate (by
// deep equality) is dropped keeping the FIRST occurrence.
func TestMerge_AppendAndDedupKeepFirst(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", "provider_priority: [deepseek, openai]\n")
	over := writeYAML(t, dir, "over.yaml", "provider_priority: !append [openai, ollama]\n")
	merged, _, err := mergeConfigFiles([]string{base, over})
	if err != nil {
		t.Fatalf("mergeConfigFiles: %v", err)
	}
	got := seqStrings(t, merged["provider_priority"])
	// [deepseek, openai] ++ [openai, ollama] → dedup keep-first → [deepseek, openai, ollama]
	want := []string{"deepseek", "openai", "ollama"}
	if !eqStrings(got, want) {
		t.Errorf("provider_priority = %v, want %v (append + dedup keep-first)", got, want)
	}
}

// TestMerge_PrependPromotesDuplicate: !prepend a provider already present pulls it
// to the FRONT and drops the lower copy (keep-first across the composed order).
func TestMerge_PrependPromotesDuplicate(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", "provider_priority: [deepseek, openai, anthropic]\n")
	over := writeYAML(t, dir, "over.yaml", "provider_priority: !prepend [anthropic]\n")
	merged, _, err := mergeConfigFiles([]string{base, over})
	if err != nil {
		t.Fatalf("mergeConfigFiles: %v", err)
	}
	got := seqStrings(t, merged["provider_priority"])
	want := []string{"anthropic", "deepseek", "openai"}
	if !eqStrings(got, want) {
		t.Errorf("provider_priority = %v, want %v (anthropic promoted, lower copy dropped)", got, want)
	}
}

// TestMerge_UntaggedSequenceStillReplaces: the RFC AN default is unchanged — an
// untagged overlay sequence replaces wholesale (and IS a recorded conflict).
func TestMerge_UntaggedSequenceStillReplaces(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", "provider_priority: [deepseek, openai]\n")
	over := writeYAML(t, dir, "over.yaml", "provider_priority: [anthropic]\n")
	merged, overrides, err := mergeConfigFiles([]string{base, over})
	if err != nil {
		t.Fatalf("mergeConfigFiles: %v", err)
	}
	got := seqStrings(t, merged["provider_priority"])
	if !eqStrings(got, []string{"anthropic"}) {
		t.Errorf("provider_priority = %v, want [anthropic] (untagged replaces)", got)
	}
	if !containsSub(overrides, "provider_priority") {
		t.Errorf("an untagged replace must record an override; got %v", overrides)
	}
}

// TestLoad_PrependNotAConflictUnderStrict: a !prepend compose must NOT trip
// LOOMCYCLE_CONFIG_STRICT (it's not a clobber) — while an untagged replace still
// would. The strict half is the fail-before for the gate.
func TestLoad_PrependNotAConflictUnderStrict(t *testing.T) {
	t.Setenv("LOOMCYCLE_CONFIG_STRICT", "1")
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", "provider_priority: [deepseek]\n")
	over := writeYAML(t, dir, "over.yaml", "provider_priority: !prepend [anthropic]\n")
	cfg, err := Load(base, over)
	if err != nil {
		t.Fatalf("strict Load with a !prepend merge should succeed: %v", err)
	}
	if len(cfg.ProviderPriority) != 2 || cfg.ProviderPriority[0] != "anthropic" {
		t.Errorf("provider_priority = %v, want [anthropic, deepseek]", cfg.ProviderPriority)
	}
}

// TestLoad_TierCandidatePrepend: the tag works on a nested tier candidate list
// (a sequence inside the tiers map), composing into cfg.Tiers.
func TestLoad_TierCandidatePrepend(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", `
models:
  deepseek-pro: { provider: deepseek, model: deepseek-v4-pro }
  oauth-sonnet: { provider: anthropic-oauth-dev, model: claude-sonnet-4-6 }
tiers:
  middle: [deepseek-pro]
`)
	over := writeYAML(t, dir, "over.yaml", `
tiers:
  middle: !prepend [oauth-sonnet]
`)
	cfg, err := Load(base, over)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mid := cfg.Tiers["middle"]
	if len(mid) != 2 {
		t.Fatalf("tiers.middle has %d candidates, want 2 (composed): %+v", len(mid), mid)
	}
	// The OAuth alias is first (prepended), the base candidate second. A bare
	// alias parses to TierCandidate{Model: <alias>} (provider resolved later).
	if mid[0].Model != "oauth-sonnet" {
		t.Errorf("tiers.middle[0] = %+v, want the prepended oauth-sonnet first", mid[0])
	}
	if mid[1].Model != "deepseek-pro" {
		t.Errorf("tiers.middle[1] = %+v, want the base deepseek-pro second", mid[1])
	}
}
