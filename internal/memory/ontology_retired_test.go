package memory

import (
	"strings"
	"testing"
)

// TestOntology_RetiredIsAReservedStatusWord: `retired` is the third reserved status word
// (RFC FO 3.7 B1), matched in any case like the other two, and it is NOT inert. Any
// other word still leaves an entry live (RFC CA's fail-open rule).
func TestOntology_RetiredIsAReservedStatusWord(t *testing.T) {
	for _, s := range []string{"retired", " Retired ", "RETIRED"} {
		if !IsRetiredEntityStatus(s) || EntryStatusOf(s) != OntologyStatusRetired {
			t.Errorf("%q is not read as retired", s)
		}
		if IsInertEntityStatus(s) {
			t.Errorf("%q is inert; a retired entry must stay in the read", s)
		}
	}
	for _, s := range []string{"", "confirmed", "archived", "retire", "proposed", "rejected"} {
		if IsRetiredEntityStatus(s) || EntryStatusOf(s) != "" {
			t.Errorf("%q is read as retired", s)
		}
	}
}

// TestOntologyRender_RetiredClassIsLeftOutOfThePrompt: the extraction prompt offers no
// retired class, relation kind or concept, and a retired tenant class keeps the seed
// class of its name out (RFC FO 3.7 B4).
func TestOntologyRender_RetiredClassIsLeftOutOfThePrompt(t *testing.T) {
	terms := []OntologyTerm{
		{Name: "incident", Fields: []string{"severity"}},
		{Name: "event", Fields: []string{"name"}, EntryStatus: OntologyStatusRetired},
		{Name: "outage", Parent: "incident", EntryStatus: OntologyStatusRetired},
	}
	p := RenderOntology(EffectiveOntology(terms, true), true)
	if !strings.Contains(p, "**incident**") {
		t.Fatalf("a live class is missing:\n%s", p)
	}
	for _, gone := range []string{"event", "outage"} {
		if strings.Contains(p, gone) {
			t.Errorf("%s is in the prompt (a retired class, or the seed it replaces):\n%s", gone, p)
		}
	}

	rels := RenderRelations([]OntologyRelation{
		{Name: "supervises", Source: RelationSourceTenant},
		{Name: "oversees", Source: RelationSourceTenant, EntryStatus: OntologyStatusRetired},
	})
	if !strings.Contains(rels, "supervises") || strings.Contains(rels, "oversees") {
		t.Errorf("relation prompt:\n%s", rels)
	}

	schemes := RenderSchemes([]OntologyScheme{
		{Name: "topics", Render: true, Concepts: []OntologyConcept{
			{Name: "Risk", Path: []string{"Risk"}},
			{Name: "Legacy", Path: []string{"Legacy"}, EntryStatus: OntologyStatusRetired},
		}},
		{Name: "old", Render: true, EntryStatus: OntologyStatusRetired, Concepts: []OntologyConcept{{Name: "Gone", Path: []string{"Gone"}}}},
	})
	if !strings.Contains(schemes, "- Risk") || strings.Contains(schemes, "Legacy") || strings.Contains(schemes, "Gone") {
		t.Errorf("scheme prompt:\n%s", schemes)
	}
}
