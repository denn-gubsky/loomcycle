package memory

import (
	"strings"
	"testing"
)

// TestOntology_OriginOutsideTheThreeIsAnIssue (RFC GM D14): @origin takes adopted,
// declared or generated; anything else is reported and not kept.
func TestOntology_OriginOutsideTheThreeIsAnIssue(t *testing.T) {
	for _, origin := range []string{"adopted", "Declared", "generated"} {
		s := ParseSchemeBody("topics", "@origin "+origin+"\n")
		if s.Origin != strings.ToLower(origin) || len(s.Issues) != 0 {
			t.Errorf("@origin %s: origin %q, issues %v", origin, s.Origin, s.Issues)
		}
	}
	s := ParseSchemeBody("topics", "@origin imported\n")
	if s.Origin != "" || len(s.Issues) != 1 || !strings.Contains(s.Issues[0], `@origin "imported" is not an origin`) {
		t.Errorf("origin %q, issues %v", s.Origin, s.Issues)
	}
}

// TestOntology_SchemeHeaderDirectivesBelongToTheirEntry: a header directive on a
// concept, or @focus on a scheme, is reported as misplaced and ignored; a repeated
// one-valued directive keeps the last value and says so; provenance on a scheme that
// is not adopted is kept and reported.
func TestOntology_SchemeHeaderDirectivesBelongToTheirEntry(t *testing.T) {
	c := ParseConceptBody("topics", "security", []string{"security"}, "@projects Document\n@origin adopted\n")
	if len(c.Issues) != 2 || !strings.Contains(c.Issues[0], "@projects is not a concept directive") {
		t.Errorf("concept issues = %v", c.Issues)
	}
	s := ParseSchemeBody("topics", "@focus Provider\n")
	if len(s.Issues) != 1 || !strings.Contains(s.Issues[0], "@focus is not a scheme directive") {
		t.Errorf("scheme issues = %v", s.Issues)
	}
	s = ParseSchemeBody("topics", "@domain A\n@domain B\n@projects  , \n@origin declared\n@source x\n")
	if s.Domain != "B" || len(s.Projects) != 0 || s.Source != "x" {
		t.Errorf("scheme = %#v", s)
	}
	want := []string{"@domain given more than once", "@projects names no class", "@source describes an adopted scheme"}
	if len(s.Issues) != len(want) {
		t.Fatalf("issues = %v", s.Issues)
	}
	for i, w := range want {
		if !strings.Contains(s.Issues[i], w) {
			t.Errorf("issue %d = %q, want %q", i, s.Issues[i], w)
		}
	}
}

// TestResolveFocus_InstanceWinsOverAClassOfTheSameName.
func TestResolveFocus_InstanceWinsOverAClassOfTheSameName(t *testing.T) {
	classes := []OntologyTerm{{Name: "Provider"}, {Name: "organization"}}
	instances := []OntologyInstance{{Name: "Provider", Class: "organization"}, {Name: "Acme Bank", Class: "organization"}}
	if got, ok, both := ResolveFocus("provider", classes, instances); !ok || !both || got.Kind != FocusInstance {
		t.Errorf("provider = %#v ok %v both %v", got, ok, both)
	}
	if got, ok, _ := ResolveFocus("acme bank", classes, instances); !ok || got.Name != "Acme Bank" || got.Class != "organization" {
		t.Errorf("acme bank = %#v", got)
	}
	if got, ok, _ := ResolveFocus("Organization", classes, nil); !ok || got.Kind != FocusClass || got.Name != "organization" {
		t.Errorf("organization = %#v", got)
	}
	if _, ok, _ := ResolveFocus("nobody", classes, instances); ok {
		t.Error("an unknown name resolved")
	}
}
