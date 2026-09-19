package memory

import (
	"strings"
	"testing"
)

// TestRecalledContext_IsARecognisedVariant. An unrecognised variant is rejected at
// boot, so a renderer with no registry entry is a prompt that will not load.
func TestRecalledContext_IsARecognisedVariant(t *testing.T) {
	if !KnownVariant(string(VariantRecalledContext)) {
		t.Fatal("recalled_context is not recognised — a prompt using it is rejected at boot")
	}
	// Case and whitespace are normalised for every other variant; this one too.
	for _, spelling := range []string{"RECALLED_CONTEXT", "  recalled_context  "} {
		if !KnownVariant(spelling) {
			t.Errorf("KnownVariant(%q) = false", spelling)
		}
	}
	var listed bool
	for _, v := range AllVariants() {
		if v == string(VariantRecalledContext) {
			listed = true
		}
	}
	if !listed {
		t.Error("recalled_context is absent from AllVariants — it cannot be discovered, " +
			"and the refusal message for a typo will not suggest it")
	}
}

// TestRecalledContext_IsNotTrusted.
//
// ⚠️ THE TRUST BOUNDARY, and the one thing about this variant that must not drift.
// A trusted variant renders WITHOUT the <memory> data frame, and belongs there only
// if its body is a pure function of operator config. This body is retrieved store
// content — facts an agent wrote and raw conversation turns a user typed. Rendering
// it unframed would present user-authored text to the model as instruction, which is
// the injection this frame exists to prevent.
func TestRecalledContext_IsNotTrusted(t *testing.T) {
	if trustedVariants[VariantRecalledContext] {
		t.Fatal("recalled_context is marked TRUSTED — it carries retrieved store content, " +
			"including raw user turns, and unframed that is prompt injection with extra steps")
	}
}

// TestRecalledContext_IsReferenceGated. Rendering costs two retrievals and an
// embedding of the run's initial input. Every other variant with a cost is gated on
// an actual placeholder reference, and this asserts the renderer is wired that way
// rather than run for every prompt.
func TestRecalledContext_IsReferenceGated(t *testing.T) {
	if ReferencesVariant("no placeholders here", VariantRecalledContext) {
		t.Error("ReferencesVariant matched a prompt with no placeholder")
	}
	if !ReferencesVariant("context:\n{{memory:recalled_context}}\n", VariantRecalledContext) {
		t.Error("ReferencesVariant did not match the placeholder — the renderer would " +
			"never fire, and the block would silently never appear")
	}
	// The sibling must not be matched by it: two variants firing off one placeholder
	// would double the retrieval cost invisibly.
	if ReferencesVariant("{{memory:search_request}}", VariantRecalledContext) {
		t.Error("the search_request placeholder also triggers recalled_context")
	}
}

// TestRecalledContext_DoesNotDisturbSearchRequest. The sibling is lexical-only and
// agents depend on exactly that; this variant exists BECAUSE changing it in place
// would change what every existing prompt renders.
func TestRecalledContext_DoesNotDisturbSearchRequest(t *testing.T) {
	if VariantRecalledContext == VariantSearchRequest {
		t.Fatal("the two variants collapsed to one name")
	}
	if !KnownVariant(string(VariantSearchRequest)) {
		t.Error("search_request stopped being recognised")
	}
	if strings.Contains(string(VariantRecalledContext), string(VariantSearchRequest)) {
		t.Error("one variant name is a substring of the other — a prefix match anywhere " +
			"in the expansion path would confuse them")
	}
}
