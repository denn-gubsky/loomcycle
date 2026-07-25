package config

import (
	"strings"
	"testing"
)

// TestConsolidationBands_DefaultsUnchangedWhenUnset — an existing deployment
// that never heard of memory.consolidation keeps the exact bands the pass used
// before they were configurable.
func TestConsolidationBands_DefaultsUnchangedWhenUnset(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Memory.Consolidation
	if c.MergeThreshold != 0 || c.RelatedThreshold != 0 {
		t.Errorf("unset block must stay zero, got %v/%v", c.MergeThreshold, c.RelatedThreshold)
	}
	if got := c.EffectiveMergeThreshold(); got != 0.95 {
		t.Errorf("EffectiveMergeThreshold() = %v, want 0.95", got)
	}
	if got := c.EffectiveRelatedThreshold(); got != 0.85 {
		t.Errorf("EffectiveRelatedThreshold() = %v, want 0.85", got)
	}
}

// TestConsolidationBands_ConfiguredPairLoads — the operator's calibrated pair
// (measured against their own embedding model) round-trips.
func TestConsolidationBands_ConfiguredPairLoads(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  consolidation:
    merge_threshold: 0.75
    related_threshold: 0.5
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Memory.Consolidation
	if c.EffectiveMergeThreshold() != 0.75 || c.EffectiveRelatedThreshold() != 0.5 {
		t.Errorf("got %v/%v, want 0.75/0.5", c.EffectiveMergeThreshold(), c.EffectiveRelatedThreshold())
	}
}

// TestConsolidationBands_OneSidedSetIsCheckedAgainstTheDefault — lowering only
// merge_threshold below the DEFAULT related band inverts the two. That is the
// realistic operator mistake (they calibrate the number they measured and leave
// the other alone), so it must fail at load, not at classification time.
func TestConsolidationBands_OneSidedSetIsCheckedAgainstTheDefault(t *testing.T) {
	_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  consolidation:
    merge_threshold: 0.80
`))
	if err == nil {
		t.Fatal("expected an inverted-band error")
	}
	// Assert the DEFAULT is what it was compared against. Checking only for
	// "related_threshold" would also pass if validation read the raw 0 and
	// tripped the range check instead — a different bug with the same word in
	// its message.
	if !strings.Contains(err.Error(), "0.85") {
		t.Errorf("error must show the default related band (0.85) it was checked against, got: %v", err)
	}
	if !strings.Contains(err.Error(), "must be <") {
		t.Errorf("expected the inversion message, got: %v", err)
	}
}

// TestConsolidationBands_RejectsInvertedAndOutOfRange.
func TestConsolidationBands_RejectsInvertedAndOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"inverted", "merge_threshold: 0.5\n    related_threshold: 0.9", "related_threshold"},
		{"equal", "merge_threshold: 0.9\n    related_threshold: 0.9", "related_threshold"},
		{"merge above one", "merge_threshold: 1.5\n    related_threshold: 0.5", "merge_threshold"},
		{"merge negative", "merge_threshold: -0.5\n    related_threshold: 0.2", "merge_threshold"},
		{"related negative", "merge_threshold: 0.9\n    related_threshold: -0.2", "related_threshold"},
		{"related above one", "merge_threshold: 0.9\n    related_threshold: 1.2", "related_threshold"},
	}
	for _, tc := range cases {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  consolidation:
    `+tc.yaml+`
`))
		if err == nil {
			t.Errorf("%s: expected a load error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error should name %s, got: %v", tc.name, tc.want, err)
		}
	}
}

// TestConsolidationBands_BoundaryOneIsAllowed — 1.0 is a legitimate merge band
// (only an exact-match merges), so the range check must be inclusive at the top.
func TestConsolidationBands_BoundaryOneIsAllowed(t *testing.T) {
	if _, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  consolidation:
    merge_threshold: 1.0
    related_threshold: 0.9
`)); err != nil {
		t.Errorf("merge_threshold 1.0 should load, got: %v", err)
	}
}
