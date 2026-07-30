package memory

import (
	"strings"
	"testing"
)

// TestEffectiveOntology_DraftIsInactive is the operator's gate, and the whole
// reason the ontology ships as a document rather than a config block.
//
// An unconfirmed document must change NOTHING. A half-written ontology sitting on
// disk must not steer extraction just because it exists — before the flip the
// tenant runs on the seed alone, after it everything the operator wrote applies.
func TestEffectiveOntology_DraftIsInactive(t *testing.T) {
	tenant := []OntologyTerm{
		{Name: "incident", Fields: []string{"summary", "cause"}},
		{Name: "project", Fields: []string{"name"}},
	}

	draft := EffectiveOntology(tenant, false)
	if len(draft) != len(BaseSeedOntology()) {
		t.Fatalf("an unconfirmed tenant layer must be discarded: got %d terms, want the %d seed terms",
			len(draft), len(BaseSeedOntology()))
	}
	for _, term := range draft {
		if term.Name == "incident" || term.Name == "project" {
			t.Errorf("tenant term %q applied while the document was still draft", term.Name)
		}
	}

	confirmed := EffectiveOntology(tenant, true)
	if len(confirmed) != len(BaseSeedOntology())+2 {
		t.Errorf("a confirmed layer must add its terms: got %d, want %d",
			len(confirmed), len(BaseSeedOntology())+2)
	}
}

// TestEffectiveOntology_TenantOverridesSeedRatherThanMerging: a tenant term with a
// seed term's name REPLACES it. Merging field lists would produce a type the
// operator never wrote and give them no way to remove a seed field — what the
// document says has to be what applies.
func TestEffectiveOntology_TenantOverridesSeedRatherThanMerging(t *testing.T) {
	got := EffectiveOntology([]OntologyTerm{
		{Name: "person", Fields: []string{"full_name", "employee_id"}},
	}, true)

	var person OntologyTerm
	for _, term := range got {
		if term.Name == "person" {
			person = term
		}
	}
	if len(person.Fields) != 2 {
		t.Fatalf("person should carry exactly the tenant's 2 fields, got %v", person.Fields)
	}
	for _, f := range person.Fields {
		if f == "role" || f == "aliases" {
			t.Errorf("seed field %q survived an override — the fields were merged, not replaced", f)
		}
	}
	if person.Source != "tenant" {
		t.Errorf("an overridden term should report source=tenant, got %q", person.Source)
	}
	// And the override must not change the term COUNT.
	if len(got) != len(BaseSeedOntology()) {
		t.Errorf("overriding a seed term changed the term count: %d vs %d", len(got), len(BaseSeedOntology()))
	}
}

// TestEffectiveOntology_IsStablyOrdered: the ontology is injected into a system
// prompt, which must stay byte-stable across runs or every call misses the
// provider prompt-cache on the whole prefix. Map iteration is not ordered, so the
// sort is load-bearing rather than cosmetic.
func TestEffectiveOntology_IsStablyOrdered(t *testing.T) {
	tenant := []OntologyTerm{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}}
	first := RenderOntology(EffectiveOntology(tenant, true), true)
	for i := 0; i < 20; i++ {
		if got := RenderOntology(EffectiveOntology(tenant, true), true); got != first {
			t.Fatalf("ontology rendering is not byte-stable across calls:\n%q\nvs\n%q", first, got)
		}
	}
}

// TestBaseSeedOntology_IsNotSharedState: a package-level slice would let one
// caller's sort or append reorder every other caller's ontology.
func TestBaseSeedOntology_IsNotSharedState(t *testing.T) {
	a := BaseSeedOntology()
	a[0].Name = "clobbered"
	a[0].Fields = append(a[0].Fields, "injected")
	b := BaseSeedOntology()
	if b[0].Name == "clobbered" {
		t.Error("BaseSeedOntology returns shared state — one caller mutated another's seed")
	}
	for _, f := range b[0].Fields {
		if f == "injected" {
			t.Error("BaseSeedOntology's field slices are shared")
		}
	}
}

// TestBaseSeedOntology_CoversPOLEPlusTheMemoryTypes: the seed is what a tenant
// that never edits the document gets, so its coverage is the floor.
func TestBaseSeedOntology_CoversPOLEPlusTheMemoryTypes(t *testing.T) {
	have := map[string][]string{}
	for _, term := range BaseSeedOntology() {
		have[term.Name] = term.Fields
	}
	for _, want := range []string{"person", "object", "location", "event", "organization", "preference", "fact"} {
		if _, ok := have[want]; !ok {
			t.Errorf("the base seed is missing %q", want)
		}
	}
	// `fact`'s three fields are what a fact's natural key is built from, so their
	// names are not decoration.
	for _, want := range []string{"subject", "predicate", "object"} {
		found := false
		for _, f := range have["fact"] {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("fact is missing the %q field its natural key is built from", want)
		}
	}
}

// TestOntologyTemplate_IsUsableAndSaysItIsInert. Two properties an operator's
// first screen has to have: worked examples rather than a blank page, and a plain
// statement that nothing happens until they confirm it — otherwise an operator
// edits the document, sees no change, and concludes the feature is broken.
func TestOntologyTemplate_IsUsableAndSaysItIsInert(t *testing.T) {
	tpl := OntologyTemplate()
	if !strings.HasPrefix(strings.TrimSpace(tpl), "# "+OntologyTitle) {
		t.Errorf("the template's first heading must match OntologyTitle (%s) — import_md makes it the root chunk", OntologyTitle)
	}
	// Three worked examples.
	for _, sample := range []string{"## project", "## incident", "## constraint"} {
		if !strings.Contains(tpl, sample) {
			t.Errorf("the template should ship a worked %q example", sample)
		}
	}
	// And it must say, in the operator's words, that it is inert until confirmed.
	if !strings.Contains(tpl, OntologyConfirmedStatus) || !strings.Contains(tpl, "draft") {
		t.Error("the template must tell the operator it does nothing until confirmed, naming both states")
	}
	// House rule: operator-visible seed content cites no internal RFC letters.
	if strings.Contains(tpl, "RFC") {
		t.Error("the template must not cite internal RFC letters")
	}
}

// TestRenderOntology_ExplainsAnUnconfirmedDeployment: without this line an
// operator who edited the document and forgot to confirm it would see their terms
// simply absent, with nothing to explain why.
func TestRenderOntology_ExplainsAnUnconfirmedDeployment(t *testing.T) {
	unconfirmed := RenderOntology(EffectiveOntology(nil, false), false)
	if !strings.Contains(unconfirmed, "has not confirmed") {
		t.Errorf("an unconfirmed render should say so:\n%s", unconfirmed)
	}
	confirmed := RenderOntology(EffectiveOntology([]OntologyTerm{{Name: "project"}}, true), true)
	if strings.Contains(confirmed, "has not confirmed") {
		t.Errorf("a confirmed render must not claim otherwise:\n%s", confirmed)
	}
	if !strings.Contains(confirmed, "project") {
		t.Errorf("a confirmed render should list the tenant's terms:\n%s", confirmed)
	}
	// Model-visible text.
	if strings.Contains(confirmed, "RFC") {
		t.Error("rendered ontology must not cite internal RFC letters")
	}
}
