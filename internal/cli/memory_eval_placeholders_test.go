package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
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

// TestExpandEvalPlaceholders_SeedOnlyPromptStillMatchesTheRecordedBaseline answers a
// question the ontology-hierarchy work raised, with a fact instead of a belief.
//
// Adding subclasses changed how the ontology renders — but only when a hierarchy is
// actually present. The eval expands the SEED alone, whose types are all roots, so it
// takes the flat path and the prompt should be byte-identical to the one the committed
// numbers were measured against. "Should be" is exactly the kind of claim that turns
// out to be wrong, and the cost of being wrong is silent: the baseline keys on the
// prompt digest, so a changed prompt does not fail — it un-gates every model at once
// and the next run reports a clean pass with nothing behind it.
//
// So: recompute the digest from the SHIPPED bundle prompt and require it to still match
// a recorded entry. If someone edits the seed, the renderer, or the extractor prompt,
// this fails and names re-measurement as the cost, rather than letting the gate expire
// unnoticed.
func TestExpandEvalPlaceholders_SeedOnlyPromptStillMatchesTheRecordedBaseline(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "base,memory")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "")
	cfg, err := loadLayeredConfig("")
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	agent, ok := cfg.Agents[extractorAgentName]
	if !ok {
		t.Fatalf("the %s bundle did not provide %q", "memory", extractorAgentName)
	}
	prompt, err := expandEvalPlaceholders(agent.SystemPrompt, "")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	sum := sha256.Sum256([]byte(prompt))
	got := hex.EncodeToString(sum[:])

	base, err := eval.LoadBaseline(filepath.Join("..", "memory", "eval", "extraction-baseline.json"))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if len(base.Entries) == 0 {
		t.Skip("no recorded baseline to compare against")
	}
	recorded := map[string]bool{}
	for _, e := range base.Entries {
		recorded[e.SystemPromptSHA256] = true
	}
	if !recorded[got] {
		have := make([]string, 0, len(recorded))
		for k := range recorded {
			have = append(have, k[:12])
		}
		sort.Strings(have)
		t.Errorf("the seed-only extractor prompt now digests to %s, which no recorded baseline "+
			"entry was measured against (recorded: %v).\n\nEvery stored score is now ungated: "+
			"EntryFor keys on this digest, so a mismatch reports as \"no baseline\" rather than as "+
			"a regression. Re-measure with `make memory-eval-live` and commit the new entries.",
			got[:12], have)
	}
}

// TestExpandEvalPlaceholders_SlashDeclaresASubclass — the hierarchy corpus is
// unscoreable without this, and a silently-flat expansion would score the model on
// subtypes it was never shown.
func TestExpandEvalPlaceholders_SlashDeclaresASubclass(t *testing.T) {
	got, err := expandEvalPlaceholders("{{memory:ontology}}", "event/incident/outage,person")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	// The rendered form must be the TREE, not a flat list — indentation is how the
	// prompt conveys specificity, and the "most specific type" instruction is what
	// makes the ladder get used at all.
	if !strings.Contains(got, "  - **incident**") {
		t.Errorf("incident is not nested under event:\n%s", got)
	}
	if !strings.Contains(got, "    - **outage**") {
		t.Errorf("outage is not nested under incident:\n%s", got)
	}
	if !strings.Contains(got, "MOST SPECIFIC") {
		t.Errorf("the specificity instruction is missing, so the ladder has no caller:\n%s", got)
	}
	// An INTERMEDIATE ancestor must be declared by the path alone. If it were not, its
	// child's parent would dangle, the render would fall back to flat, and the run would
	// measure nothing it claims to — while looking completely normal.
	if !strings.Contains(got, "- **event**") {
		t.Errorf("the path did not declare its ancestor:\n%s", got)
	}
	// And a term outside the chain stays a root.
	if !strings.Contains(got, "\n- **person**") {
		t.Errorf("an unrelated term was pulled into the tree:\n%s", got)
	}
}

// TestExpandEvalPlaceholders_RefusesAMalformedPath: a trailing slash names nothing, and
// silently dropping it would flatten the hierarchy the run depends on.
func TestExpandEvalPlaceholders_RefusesAMalformedPath(t *testing.T) {
	for _, bad := range []string{"event/", "event//incident"} {
		if _, err := expandEvalPlaceholders("{{memory:ontology}}", bad); err == nil {
			t.Errorf("%q should have been refused", bad)
		}
	}
}
