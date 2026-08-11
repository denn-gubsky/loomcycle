package memory

import (
	"strings"
	"testing"
	"time"
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

// TestResolveInheritance_SubclassCarriesItsParentsFields is the phase-2 claim: a
// subclass adds fields rather than restating them.
func TestResolveInheritance_SubclassCarriesItsParentsFields(t *testing.T) {
	got := ResolveInheritance([]OntologyTerm{
		{Name: "event", Fields: []string{"name", "occurred_at"}},
		{Name: "incident", Parent: "event", Fields: []string{"severity"}},
		{Name: "outage", Parent: "incident", Fields: []string{"minutes_down"}},
	})
	byName := map[string]OntologyTerm{}
	for _, t := range got {
		byName[t.Name] = t
	}
	if got := strings.Join(byName["event"].Inherited, ","); got != "" {
		t.Errorf("a root inherits nothing, got %q", got)
	}
	if got := strings.Join(byName["incident"].Inherited, ","); got != "name,occurred_at" {
		t.Errorf("incident.Inherited = %q, want name,occurred_at", got)
	}
	// TRANSITIVE, ancestor-first: the general fields read ahead of the specific ones,
	// which is also the order a model fills them in.
	if got := strings.Join(byName["outage"].Inherited, ","); got != "name,occurred_at,severity" {
		t.Errorf("outage.Inherited = %q, want name,occurred_at,severity", got)
	}
	// Declared fields are untouched — the panel has to be able to show what the
	// operator actually wrote, so inheritance must not be merged into Fields.
	if got := strings.Join(byName["outage"].Fields, ","); got != "minutes_down" {
		t.Errorf("outage.Fields = %q, want only its own declaration", got)
	}
	if got := strings.Join(AllFields(byName["outage"]), ","); got != "name,occurred_at,severity,minutes_down" {
		t.Errorf("AllFields(outage) = %q", got)
	}
}

// TestResolveInheritance_ChildRedeclarationWins: most specific wins, the same rule as
// the tenant-over-seed layer. The field must appear ONCE, in the child's own list.
func TestResolveInheritance_ChildRedeclarationWins(t *testing.T) {
	got := ResolveInheritance([]OntologyTerm{
		{Name: "event", Fields: []string{"name", "occurred_at"}},
		{Name: "incident", Parent: "event", Fields: []string{"occurred_at", "severity"}},
	})
	for _, term := range got {
		if term.Name != "incident" {
			continue
		}
		if strings.Join(term.Inherited, ",") != "name" {
			t.Errorf("a redeclared field must not also be inherited: %v", term.Inherited)
		}
		if got := strings.Join(AllFields(term), ","); got != "name,occurred_at,severity" {
			t.Errorf("AllFields = %q, want each field once", got)
		}
	}
}

// TestResolveInheritance_CycleDoesNotHang. The chunk tree cannot produce a cycle, but
// this runs on every prompt render and accepts caller-supplied terms — an unbounded
// walk would turn a malformed input into a hung request instead of a wrong answer.
func TestResolveInheritance_CycleDoesNotHang(t *testing.T) {
	done := make(chan []OntologyTerm, 1)
	go func() {
		done <- ResolveInheritance([]OntologyTerm{
			{Name: "a", Parent: "b", Fields: []string{"fa"}},
			{Name: "b", Parent: "a", Fields: []string{"fb"}},
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveInheritance hung on a cycle")
	}
}

// TestResolveInheritance_DanglingParentCostsOnlyInheritance: a typo'd parent name
// should cost the operator their inheritance, not the whole ontology.
func TestResolveInheritance_DanglingParentCostsOnlyInheritance(t *testing.T) {
	got := ResolveInheritance([]OntologyTerm{
		{Name: "incident", Parent: "evnet", Fields: []string{"severity"}},
	})
	if len(got) != 1 {
		t.Fatalf("the term must survive, got %v", got)
	}
	if len(got[0].Inherited) != 0 {
		t.Errorf("nothing to inherit from a missing parent, got %v", got[0].Inherited)
	}
	if strings.Join(AllFields(got[0]), ",") != "severity" {
		t.Errorf("its own fields must remain: %v", got[0].Fields)
	}
}

// TestRenderOntology_FlatOntologyIsByteIdentical is the compatibility guarantee, and
// the reason it is asserted rather than assumed.
//
// This string goes into the system prompt of every extracting agent. A whitespace
// change would invalidate provider prompt caches and shift extraction results for every
// deployment that never nests anything — a cost paid by people who did not opt in. So
// the tree rendering engages ONLY when a hierarchy is actually present.
func TestRenderOntology_FlatOntologyIsByteIdentical(t *testing.T) {
	flat := []OntologyTerm{
		{Name: "person", Fields: []string{"name", "aliases"}},
		{Name: "event", Fields: []string{"occurred_at"}},
	}
	want := "# Entity types\n\n" +
		"When you record an entity or a fact, use one of these types and fill the fields it lists.\n\n" +
		"- **person** — name, aliases\n" +
		"- **event** — occurred_at\n"
	if got := RenderOntology(flat, true); got != want {
		t.Errorf("flat rendering drifted.\n got: %q\nwant: %q", got, want)
	}
	// A DANGLING parent must not flip the rendering either — one typo'd chunk title
	// would otherwise switch every deployment to the tree form and bolt on a
	// specificity instruction with no hierarchy to apply it to.
	dangling := append([]OntologyTerm{}, flat...)
	dangling[1].Parent = "nonexistent"
	if got := RenderOntology(dangling, true); got != want {
		t.Errorf("a dangling parent changed the rendering:\n%s", got)
	}
}

// TestRenderOntology_HierarchyIndentsAndDemandsSpecificity.
//
// The indentation carries the hierarchy; the per-line field list carries what to fill
// in, duplicated on purpose because a model asked to infer "incident also has
// occurred_at" from indentation gets it wrong often enough to matter. And the
// specificity instruction is load-bearing: handed a ladder with no instruction, a model
// sits at the top of it and the subclasses go unused — a capability with no caller.
func TestRenderOntology_HierarchyIndentsAndDemandsSpecificity(t *testing.T) {
	got := RenderOntology(ResolveInheritance([]OntologyTerm{
		{Name: "event", Fields: []string{"occurred_at"}},
		{Name: "incident", Parent: "event", Fields: []string{"severity"}},
		{Name: "person", Fields: []string{"name"}},
	}), true)

	if !strings.Contains(got, "\n  - **incident** — occurred_at, severity\n") {
		t.Errorf("subclass must be indented and carry inherited fields:\n%s", got)
	}
	if !strings.Contains(got, "- **event** — occurred_at\n") {
		t.Errorf("parent line missing:\n%s", got)
	}
	if !strings.Contains(got, "MOST SPECIFIC") {
		t.Errorf("the specificity instruction is missing, so the ladder has no caller:\n%s", got)
	}
	// A root stays flush left even when a sibling root has children.
	if !strings.Contains(got, "\n- **person** — name\n") {
		t.Errorf("unrelated root got indented:\n%s", got)
	}
}

// TestEffectiveOntology_InheritanceUsesTheOVERRIDDENParent: resolution runs AFTER
// layering, so a subclass of a type the tenant redefined gets the tenant's fields, not
// the seed's. Resolving first would hand the child fields its parent no longer has.
func TestEffectiveOntology_InheritanceUsesTheOverriddenParent(t *testing.T) {
	got := EffectiveOntology([]OntologyTerm{
		// Override the standard `event` wholesale — the documented way to subclass a
		// standard type is to declare it as your own root and nest beneath it.
		{Name: "event", Fields: []string{"when", "where"}},
		{Name: "incident", Parent: "event", Fields: []string{"severity"}},
	}, true)

	for _, term := range got {
		if term.Name != "incident" {
			continue
		}
		if strings.Join(term.Inherited, ",") != "when,where" {
			t.Errorf("incident inherited %v — want the tenant's override, not the seed's fields", term.Inherited)
		}
		return
	}
	t.Fatal("incident missing from the effective ontology")
}

// TestEnforcePinnedRoots_TierTypesCannotBeNestedButCanBeSubclassed.
//
// The asymmetry is the whole rule. Subclassing `preference` is a good idea and stays
// legal; giving it a parent inverts the tier, and with subtype-expanded retrieval a query
// for that parent would start sweeping in every preference the user ever expressed.
func TestEnforcePinnedRoots_TierTypesCannotBeNestedButCanBeSubclassed(t *testing.T) {
	got, rerooted := EnforcePinnedRoots([]OntologyTerm{
		{Name: "signal", Fields: []string{"kind"}},
		{Name: "preference", Parent: "signal"},
		{Name: "dietary-preference", Parent: "preference", Fields: []string{"restriction"}},
		{Name: "fact", Parent: "signal"},
		{Name: "project", Parent: "signal", Fields: []string{"status"}},
	})
	byName := map[string]OntologyTerm{}
	for _, tm := range got {
		byName[tm.Name] = tm
	}
	if byName["preference"].Parent != "" || byName["fact"].Parent != "" {
		t.Errorf("a tier type kept a parent: preference=%q fact=%q",
			byName["preference"].Parent, byName["fact"].Parent)
	}
	// SUBCLASSING one is untouched.
	if byName["dietary-preference"].Parent != "preference" {
		t.Errorf("subclassing a tier type must stay legal, got parent %q",
			byName["dietary-preference"].Parent)
	}
	// And an ordinary type is not swept up by the rule.
	if byName["project"].Parent != "signal" {
		t.Errorf("an unrelated type was re-rooted: %q", byName["project"].Parent)
	}
	if len(rerooted) != 2 {
		t.Errorf("both re-rootings must be reported so the operator is told, got %v", rerooted)
	}
	// Nothing is DROPPED — the operator keeps their ontology, they just lose the nesting.
	if len(got) != 5 {
		t.Errorf("want all five terms, got %d", len(got))
	}
}

// TestEffectiveOntology_PinnedRootIsEnforcedBeforeInheritance: a wrongly-nested tier type
// must not inherit its would-be parent's fields on the way through.
func TestEffectiveOntology_PinnedRootIsEnforcedBeforeInheritance(t *testing.T) {
	for _, term := range EffectiveOntology([]OntologyTerm{
		{Name: "signal", Fields: []string{"kind", "weight"}},
		{Name: "preference", Parent: "signal"},
	}, true) {
		if term.Name != "preference" {
			continue
		}
		if len(term.Inherited) != 0 {
			t.Errorf("preference inherited %v — the pin must apply before inheritance", term.Inherited)
		}
		if term.Parent != "" {
			t.Errorf("preference kept parent %q in the effective ontology", term.Parent)
		}
	}
}

// TestOntologyNameIssue_WarnsWithoutRewriting (RFC BZ §10.2).
//
// Warn-only is the decision, not a first step toward normalising. A name is part of a
// stored fact's key, so rewriting `Notes on naming` into `notes-on-naming` after facts
// exist under the old spelling splits the type in half — the cost of a bad name is
// friction in a prompt, not corruption.
func TestOntologyNameIssue_WarnsWithoutRewriting(t *testing.T) {
	fine := []string{"event", "security-incident", "work_item", "p", "x9"}
	for _, n := range fine {
		if got := OntologyNameIssue(n); got != "" {
			t.Errorf("%q should be accepted quietly, got %q", n, got)
		}
	}
	awkward := []string{"Notes on naming", "Event", "type:thing", "trailing-"}
	for _, n := range awkward {
		if OntologyNameIssue(n) == "" {
			t.Errorf("%q should have produced an advisory", n)
		}
	}
	// The SEED must be quiet — a warning on a name the operator cannot edit is noise.
	for _, term := range BaseSeedOntology() {
		if got := OntologyNameIssue(term.Name); got != "" {
			t.Errorf("seed term %q warns: %q", term.Name, got)
		}
	}
	// An empty name is not an advisory case: it is dropped elsewhere as nameless.
	if OntologyNameIssue("  ") != "" {
		t.Error("an empty name should not produce a name advisory")
	}
}
