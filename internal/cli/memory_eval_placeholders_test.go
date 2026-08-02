package cli

import (
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestExpandEvalPlaceholders_RendersTheOntology is the defect this exists for.
//
// The runtime turns {{memory:ontology}} into ~520 characters of entity types before
// the model sees the prompt; this harness sent the nineteen literal characters. So
// every score taken since the ontology landed in the extractor prompt measured a
// prompt telling the model to "use one of the types above" where above was a
// placeholder — a complete explanation for the eval's own finding that half the
// emitted types fell outside the ontology.
//
// The bundle drift test could never catch it: the harness's copy and the bundle's
// were identical throughout. What differed was the bundle's prompt versus what the
// RUNTIME sends.
func TestExpandEvalPlaceholders_RendersTheOntology(t *testing.T) {
	got, err := expandEvalPlaceholders("You extract facts.\n\n{{memory:ontology}}\n\nRules.", "")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if strings.Contains(got, "{{memory:") {
		t.Fatalf("the placeholder survived:\n%s", got)
	}
	// The seed types must actually be named — a rendering that produced an empty
	// string would also "expand" and would be just as meaningless.
	for _, want := range []string{"person", "organization", "Entity types"} {
		if !strings.Contains(got, want) {
			t.Errorf("expansion is missing %q:\n%s", want, got)
		}
	}
	if len(got) < len("You extract facts.\n\n{{memory:ontology}}\n\nRules.")+300 {
		t.Errorf("expansion added almost nothing — the model sees a fraction of what the runtime sends:\n%s", got)
	}
}

// TestExpandEvalPlaceholders_ConfirmedTenantTermsChangeThePrompt. The whole point of
// the flag: an unconfirmed deployment gets the seed alone, a confirmed one gets its
// own types too, and the two produce materially different prompts. Scoring only the
// first would answer a question nobody asked about a deployment that has confirmed.
func TestExpandEvalPlaceholders_ConfirmedTenantTermsChangeThePrompt(t *testing.T) {
	seed, err := expandEvalPlaceholders("{{memory:ontology}}", "")
	if err != nil {
		t.Fatal(err)
	}
	withTenant, err := expandEvalPlaceholders("{{memory:ontology}}", "project, incident ,constraint")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"project", "incident", "constraint"} {
		if !strings.Contains(withTenant, want) {
			t.Errorf("tenant term %q missing:\n%s", want, withTenant)
		}
		if strings.Contains(seed, want) {
			t.Errorf("seed-only expansion should NOT carry tenant term %q", want)
		}
	}
	// And the unconfirmed wording must not appear once terms ARE supplied, or the
	// prompt would tell the model to ignore the very types it just listed.
	if strings.Contains(withTenant, "has not confirmed") {
		t.Errorf("a confirmed expansion still says the deployment has not confirmed:\n%s", withTenant)
	}
	if !strings.Contains(seed, "has not confirmed") {
		t.Errorf("the seed-only expansion should say so:\n%s", seed)
	}
}

// TestExpandEvalPlaceholders_RefusesWhatItCannotRender. user_info and tenant_info
// read a live store this CLI does not have. Sending them verbatim is what already
// happened to `ontology` and cost a baseline's worth of meaning, so an unrenderable
// variant fails loudly instead.
func TestExpandEvalPlaceholders_RefusesWhatItCannotRender(t *testing.T) {
	for _, v := range []meminject.Variant{
		meminject.VariantUserInfo, meminject.VariantTenantInfo, meminject.VariantCoreBlocks,
	} {
		_, err := expandEvalPlaceholders("prompt {{memory:"+string(v)+"}}", "")
		if err == nil {
			t.Errorf("%s must be refused, not sent literally", v)
			continue
		}
		if !strings.Contains(err.Error(), string(v)) {
			t.Errorf("the refusal should name the variant; got %v", err)
		}
	}
	// The tool family too — same hazard, same silence.
	if _, err := expandEvalPlaceholders("prompt {{tool:Context.tools}}", ""); err == nil {
		t.Error("a {{tool:...}} placeholder must be refused")
	}
}

// TestExpandEvalPlaceholders_LeavesAPlainPromptAlone: no placeholder, no change.
func TestExpandEvalPlaceholders_LeavesAPlainPromptAlone(t *testing.T) {
	const plain = "You extract durable facts from ONE chat transcript."
	got, err := expandEvalPlaceholders(plain, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Errorf("a prompt with no placeholder must be byte-identical:\n%q", got)
	}
}
