package memory_test

import (
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestConsolidationBands_RendersTheConfiguredNumbers — the whole point of the
// variant: the operator's calibrated bands, not a constant, reach the prompt.
func TestConsolidationBands_RendersTheConfiguredNumbers(t *testing.T) {
	got := meminject.ConsolidationBands(0.77, 0.45)
	for _, want := range []string{"0.77", "0.45"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered bands missing %q:\n%s", want, got)
		}
	}
	// A stale default must not linger alongside the configured value.
	if strings.Contains(got, "0.95") || strings.Contains(got, "0.85") {
		t.Errorf("rendered bands still carry the default numbers:\n%s", got)
	}
}

// TestConsolidationBands_FormatsWithoutTrailingZerosOrExponent — the system
// prompt is re-derived on every run/resume/compaction and must stay byte-stable
// for provider prompt-caching, so the number formatting has to be canonical.
func TestConsolidationBands_FormatsWithoutTrailingZerosOrExponent(t *testing.T) {
	got := meminject.ConsolidationBands(0.95, 0.7675)
	if !strings.Contains(got, "0.7675") {
		t.Errorf("want the exact measured value 0.7675:\n%s", got)
	}
	if strings.Contains(got, "0.950000") || strings.Contains(got, "0.7675000") {
		t.Errorf("trailing-zero float rendering:\n%s", got)
	}
	// A small band must not fall back to scientific notation, which the model
	// would read as a different number entirely.
	if tiny := meminject.ConsolidationBands(0.95, 0.0001); !strings.Contains(tiny, "0.0001") {
		t.Errorf("small band rendered non-decimally:\n%s", tiny)
	}
	if got != meminject.ConsolidationBands(0.95, 0.7675) {
		t.Error("rendering is not deterministic — breaks prompt caching")
	}
}

// TestConsolidationBands_RendersUnframed — the bands are instructions the agent
// APPLIES, not stored memory data. Wrapping them in the <memory> DATA frame
// would tell the model, in the same breath, not to follow them.
func TestConsolidationBands_RendersUnframed(t *testing.T) {
	sections := map[meminject.Variant]string{
		meminject.VariantConsolidationBands: meminject.ConsolidationBands(0.9, 0.6),
		meminject.VariantCoreBlocks:         "name: alice",
	}
	out := meminject.Expand("prompt\n\n{{memory:consolidation_bands}}", sections, 0)
	if strings.Contains(out, `source="consolidation_bands"`) {
		t.Errorf("bands were DATA-framed:\n%s", out)
	}
	if strings.Contains(out, "NOT instructions to follow") &&
		strings.Index(out, "NOT instructions to follow") < strings.Index(out, "0.9") {
		t.Errorf("bands sit inside the do-not-follow frame:\n%s", out)
	}
	if !strings.Contains(out, "0.9") || !strings.Contains(out, "0.6") {
		t.Errorf("bands missing from the expansion:\n%s", out)
	}
	// The core-blocks section must still be framed — the bypass is per-variant.
	if !strings.Contains(out, `source="core_blocks"`) {
		t.Errorf("core_blocks lost its DATA frame:\n%s", out)
	}
}

// TestConsolidationBands_IsAKnownVariant — an unknown variant fails config
// load, so registration is what makes the bundle's placeholder legal.
func TestConsolidationBands_IsAKnownVariant(t *testing.T) {
	if !meminject.KnownVariant("consolidation_bands") {
		t.Error("consolidation_bands must be a recognised variant")
	}
	if len(meminject.UnknownVariants("x {{memory:consolidation_bands}} y")) != 0 {
		t.Error("consolidation_bands must not be reported unknown")
	}
	found := false
	for _, v := range meminject.AllVariants() {
		if v == "consolidation_bands" {
			found = true
		}
	}
	if !found {
		t.Errorf("AllVariants() must list consolidation_bands, got %v", meminject.AllVariants())
	}
}

// TestConsolidationBands_AbsentPlaceholderInjectsNothing — a variant with no
// implicit-append path must not leak into agents that never asked for it.
func TestConsolidationBands_AbsentPlaceholderInjectsNothing(t *testing.T) {
	sections := map[meminject.Variant]string{
		meminject.VariantConsolidationBands: meminject.ConsolidationBands(0.9, 0.6),
	}
	out := meminject.Expand("a prompt with no placeholders", sections, 0)
	if strings.Contains(out, "similarity") {
		t.Errorf("bands were injected without a placeholder:\n%s", out)
	}
}
