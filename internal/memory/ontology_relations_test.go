package memory

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestParseRelationBody_ReadsEveryDirectiveInBothSpellings.
func TestParseRelationBody_ReadsEveryDirectiveInBothSpellings(t *testing.T) {
	r := ParseRelationBody("amends", "Changes the text of a provision.\n"+
		"@inverse amended_by\n"+
		"@domain Provision|Document\n"+
		"- `@range` Provision, Document\n"+
		"@qualifiers mod_type(substitution|insertion|repeal), effective_from\n"+
		"@discriminator mod_type\n"+
		"@dated\n@walk\n@auto_assert\n@functional\n@irreflexive\n"+
		"@renamed_from modifies\n")
	want := OntologyRelation{
		Name: "amends", Inverse: "amended_by",
		Domain: []string{"Provision", "Document"}, Range: []string{"Provision", "Document"},
		Qualifiers: []RelationQualifier{
			{Name: "mod_type", Values: []string{"substitution", "insertion", "repeal"}},
			{Name: "effective_from"},
		},
		Discriminator: "mod_type", Dated: true, Walk: true, AutoAssert: true, Functional: true, Irreflexive: true,
		Meaning: "Changes the text of a provision.", RenamedFrom: "modifies", Source: RelationSourceTenant,
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("parsed:\n got %#v\nwant %#v", r, want)
	}
}

// TestParseRelationBody_ContradictionsAreReportedAndDropped.
//
// A kind with contradictory declarations stays in force; only the contradiction is
// dropped, and the operator is told which.
func TestParseRelationBody_ContradictionsAreReportedAndDropped(t *testing.T) {
	r := ParseRelationBody("x", "@symmetric\n@asymmetric\n@discriminator kind\n@transitive_within aspect\n@identity\n@bogus\n")
	if r.Symmetric || r.Asymmetric || r.Discriminator != "" || r.TransitiveWithin != "" {
		t.Errorf("contradictions should be dropped: %#v", r)
	}
	joined := strings.Join(r.Issues, "\n")
	for _, want := range []string{"@symmetric and @asymmetric", "@discriminator kind", "@transitive_within aspect",
		"@identity is not a relation directive", "unknown directive @bogus"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing issue %q in:\n%s", want, joined)
		}
	}
	sym := ParseRelationBody("conflicts_with", "@symmetric\n@inverse conflicted_by\n")
	if sym.Inverse != "" || len(sym.Issues) == 0 {
		t.Errorf("a symmetric kind's declared inverse should be ignored and reported: %#v", sym)
	}
}

// TestSeedRelations_IsAWellFormedVocabulary checks the seed's shape. Every parent is a
// kind that exists (seed or reserved), no name or inverse collides with another kind's,
// the walk set is decision D9's, and — load-bearing — no seed kind constrains domain or
// range, which would refuse writes against classes a tenant never declared.
func TestSeedRelations_IsAWellFormedVocabulary(t *testing.T) {
	seed := SeedRelations()
	names := map[string]bool{}
	for _, r := range reservedRelations() {
		names[r.Name] = true
	}
	for _, r := range seed {
		if names[r.Name] {
			t.Errorf("seed kind %q duplicates another kind", r.Name)
		}
		names[r.Name] = true
		if len(r.Domain) > 0 || len(r.Range) > 0 {
			t.Errorf("seed kind %q constrains domain/range; seed constraints would refuse writes", r.Name)
		}
		if r.Source != RelationSourceBase || r.Meaning == "" {
			t.Errorf("seed kind %q: source %q, meaning %q", r.Name, r.Source, r.Meaning)
		}
		if r.Symmetric && r.Inverse != "" {
			t.Errorf("symmetric seed kind %q declares an inverse", r.Name)
		}
		if RelationNameIssue(r.Name) != "" {
			t.Errorf("seed kind %q has a name advisory", r.Name)
		}
	}
	inverses := map[string]string{}
	for _, r := range append(seed, reservedRelations()...) {
		if r.Parent != "" && !names[r.Parent] {
			t.Errorf("seed kind %q names a missing parent %q", r.Name, r.Parent)
		}
		if r.Inverse == "" {
			continue
		}
		if names[r.Inverse] {
			t.Errorf("inverse %q of %q is also a kind name", r.Inverse, r.Name)
		}
		if prev, dup := inverses[r.Inverse]; dup {
			t.Errorf("inverse %q is shared by %q and %q", r.Inverse, prev, r.Name)
		}
		inverses[r.Inverse] = r.Name
	}
	var walk []string
	for _, r := range EffectiveRelations(nil, true, true) {
		if r.Walk {
			walk = append(walk, r.Name)
		}
	}
	want := []string{"based_on", "cites", "defines", "derogates_from", "implements", "overrides", "supersedes"}
	if !reflect.DeepEqual(walk, want) {
		t.Errorf("default walk set = %v, want D9's %v", walk, want)
	}
}

// TestEffectiveRelations_InertUntilAConfirmedSection.
func TestEffectiveRelations_InertUntilAConfirmedSection(t *testing.T) {
	if EffectiveRelations(nil, false, true) != nil || EffectiveRelations(nil, true, false) != nil {
		t.Error("the vocabulary must be inert without a confirmed Relations section")
	}
	if len(EffectiveRelations(nil, true, true)) != len(SeedRelations())+len(reservedRelations()) {
		t.Error("an empty declared section should bring the seed and reserved kinds into force")
	}
}

// TestEffectiveRelations_TenantOverridesSeedButNeverAReservedKind (RFC FO 10d.1).
func TestEffectiveRelations_TenantOverridesSeedButNeverAReservedKind(t *testing.T) {
	eff := EffectiveRelations([]OntologyRelation{
		{Name: "cites", Parent: "references", Domain: []string{"Provision"}, Source: RelationSourceTenant},
		{Name: "references", Inverse: "hijacked", Domain: []string{"Provision"}, Source: RelationSourceTenant},
	}, true, true)
	by := map[string]OntologyRelation{}
	for _, r := range eff {
		by[r.Name] = r
	}
	if by["cites"].Source != RelationSourceTenant || len(by["cites"].Domain) != 1 || by["cites"].Inverse != "" {
		t.Errorf("tenant cites should replace the seed wholesale: %#v", by["cites"])
	}
	if by["references"].Source != RelationSourceReserved || by["references"].Inverse != "referenced_by" || len(by["references"].Domain) != 0 {
		t.Errorf("a reserved kind's definition must not change: %#v", by["references"])
	}
	marked := MarkReservedEntries([]OntologyRelation{{Name: "references"}, {Name: "cites"}})
	if len(marked[0].Issues) == 0 || len(marked[1].Issues) != 0 {
		t.Errorf("only the reserved-named entry should be marked: %#v", marked)
	}
}

// TestExpandDescendants_StableAndBounded.
func TestExpandDescendants_StableAndBounded(t *testing.T) {
	parentOf := map[string]string{"cites": "references", "based_on": "references", "amends": "changes", "x": "cites"}
	got, added := ExpandDescendants([]string{"references", "zz"}, parentOf, 4)
	if strings.Join(got, ",") != "references,zz,based_on,cites,x" || strings.Join(added, ",") != "based_on,cites,x" {
		t.Errorf("got %v added %v", got, added)
	}
	shallow, _ := ExpandDescendants([]string{"references"}, parentOf, 1)
	if len(shallow) != 3 {
		t.Errorf("depth 1 should stop at direct children: %v", shallow)
	}
	same, none := ExpandDescendants([]string{"related"}, parentOf, 4)
	if len(same) != 1 || none != nil {
		t.Errorf("nothing to expand: %v %v", same, none)
	}
}

// TestRenderRelations_ListsOnlyTheTenantsOwnKinds (RFC FO 10d.2): seed and reserved
// kinds are in force but never listed, and with no tenant kinds the block is empty, so
// the prompt does not change.
func TestRenderRelations_ListsOnlyTheTenantsOwnKinds(t *testing.T) {
	if RenderRelations(EffectiveRelations(nil, true, true)) != "" {
		t.Error("seed-only relations must render nothing")
	}
	if RenderRelations(nil) != "" {
		t.Error("no relations must render nothing")
	}
	got := RenderRelations(EffectiveRelations([]OntologyRelation{
		ParseRelationBody("stricter_than", "Sets a stricter version of the same requirement.\n@domain Provision\n@range Provision\n@inverse looser_than\n@qualifiers aspect\n"),
	}, true, true))
	for _, want := range []string{"# Relation kinds", "**stricter_than** (seen from the other end: looser_than)",
		"Provision → Provision", "record aspect", "describe the relation in the sentence"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, seedName := range []string{"**cites**", "**about**", "**amends**"} {
		if strings.Contains(got, seedName) {
			t.Errorf("seed/reserved kind %s reached the prompt", seedName)
		}
	}
}

func TestReservedRelationNames_Sorted(t *testing.T) {
	got := ReservedRelationNames()
	if !sort.StringsAreSorted(got) || len(got) != 6 {
		t.Errorf("reserved names = %v", got)
	}
}
