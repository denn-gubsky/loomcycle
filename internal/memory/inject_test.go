package memory

import (
	"strings"
	"testing"
)

// TestPlaceholder_ExpandsKnownVariantAndEscapes pins the two core expander
// behaviours: a known {{memory:...}} placeholder is replaced with its framed
// section, and a backslash-escaped placeholder renders as the LITERAL text
// (backslash stripped) rather than being expanded.
func TestPlaceholder_ExpandsKnownVariantAndEscapes(t *testing.T) {
	prompt := `Intro. {{memory:core_blocks}} Middle. \{{memory:user_info}} End.`
	sections := map[Variant]string{
		VariantCoreBlocks: "persona=helpful",
		VariantUserInfo:   "SHOULD-NOT-APPEAR",
	}

	got := Expand(prompt, sections, 1024)

	// Known variant expanded, framed as data (not instructions).
	if !strings.Contains(got, "persona=helpful") {
		t.Errorf("core_blocks not expanded: %q", got)
	}
	if !strings.Contains(got, `<memory source="core_blocks">`) {
		t.Errorf("expanded content is not framed as memory data: %q", got)
	}
	if !strings.Contains(got, "NOT instructions") {
		t.Errorf("data frame missing the not-instructions note: %q", got)
	}

	// Escaped placeholder → literal, backslash stripped, NOT expanded.
	if !strings.Contains(got, "{{memory:user_info}}") {
		t.Errorf("escaped placeholder should render literally: %q", got)
	}
	if strings.Contains(got, `\{{memory:user_info}}`) {
		t.Errorf("escape backslash should be stripped: %q", got)
	}
	if strings.Contains(got, "SHOULD-NOT-APPEAR") {
		t.Errorf("escaped placeholder must not expand its section: %q", got)
	}
}

// TestPlaceholder_ImplicitAppendWhenNoPlaceholder verifies core_blocks are
// appended in their own framed section when configured but the prompt carries
// no explicit placeholder.
func TestPlaceholder_ImplicitAppendWhenNoPlaceholder(t *testing.T) {
	got := Expand("Just a prompt, no placeholder.", map[Variant]string{
		VariantCoreBlocks: "persona=helpful",
	}, 1024)
	if !strings.Contains(got, "Just a prompt") {
		t.Errorf("base prompt lost: %q", got)
	}
	if !strings.Contains(got, `<memory source="core_blocks">`) || !strings.Contains(got, "persona=helpful") {
		t.Errorf("core_blocks not implicitly appended: %q", got)
	}
}

// TestPlaceholder_NoDoubleAppendWhenPlaceholderPresent guards against the
// implicit append firing when the operator already placed the placeholder.
func TestPlaceholder_NoDoubleAppendWhenPlaceholderPresent(t *testing.T) {
	got := Expand("A {{memory:core_blocks}} B", map[Variant]string{
		VariantCoreBlocks: "persona=helpful",
	}, 1024)
	if n := strings.Count(got, `<memory source="core_blocks">`); n != 1 {
		t.Errorf("core_blocks framed section count = %d, want 1 (no implicit double-append): %q", n, got)
	}
}

// TestInject_RespectsMaxTokensBudget verifies the injected memory content is
// capped by memory_inject_max_tokens (chars/4) and truncated with a marker.
func TestInject_RespectsMaxTokensBudget(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1000 tokens of content
	const maxTokens = 10             // budget = 40 chars

	got := Expand("Prompt {{memory:core_blocks}}", map[Variant]string{
		VariantCoreBlocks: big,
	}, maxTokens)

	if !strings.Contains(got, "[memory truncated]") {
		t.Fatalf("expected truncation marker, got: %q", got)
	}
	// The injected body (the run of x's) must not exceed the char budget.
	xs := strings.Count(got, "x")
	if xs > maxTokens*4 {
		t.Errorf("injected body = %d chars, exceeds budget %d", xs, maxTokens*4)
	}
	if xs == 0 {
		t.Errorf("budget truncated everything; expected a bounded prefix: %q", got)
	}
}

// TestUnknownVariants_DetectsTypoIgnoresEscaped backs the boot-validation path:
// an unknown variant is reported, a known one is not, and an escaped
// placeholder is ignored (it is a literal, not a reference).
func TestUnknownVariants_DetectsTypoIgnoresEscaped(t *testing.T) {
	got := UnknownVariants(`{{memory:core_blocks}} {{memory:core_block}} \{{memory:bogus}}`)
	if len(got) != 1 || got[0] != "core_block" {
		t.Errorf("UnknownVariants = %v, want [core_block] (known kept out, escaped ignored)", got)
	}
	if len(UnknownVariants(`{{ memory : user_info }}`)) != 0 {
		t.Errorf("whitespace-tolerant known variant should not be flagged unknown")
	}
}
