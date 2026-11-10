package builtin

import (
	"reflect"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// normOntologyMD marks one document class a norm. Law inherits it through NormativeAct;
// Guide is a document class that is not; Provision is an entity type, where @norm means
// nothing; Memo carries a malformed @norm with a value.
const normOntologyMD = "# Tenant Ontology\n@version 2\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### NormativeAct\n@norm\n\n" +
	"##### Law\n\n" +
	"#### Guide\n\n" +
	"#### Memo\n@norm false\n\n" +
	"### Provision\n@norm\n\n" +
	"## Relations\n@section relations\n\n### cites\n@norm\n"

// TestDescribeOntology_ReportsNormClassesWithInheritance: @norm on a document class marks
// it and its subclasses as norms; describe_ontology reports declared and inherited, and
// lists every norm class in force.
func TestDescribeOntology_ReportsNormClassesWithInheritance(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, normOntologyMD, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	by := describedClassByName(desc)

	if c := by["NormativeAct"]; !c.Norm || c.InheritedNorm {
		t.Errorf("NormativeAct norm=%v inherited=%v, want declared norm", c.Norm, c.InheritedNorm)
	}
	if c := by["Law"]; !c.Norm || !c.InheritedNorm {
		t.Errorf("Law norm=%v inherited=%v, want inherited norm", c.Norm, c.InheritedNorm)
	}
	for _, name := range []string{"Guide", "Document", "Memo", "Provision"} {
		if by[name].Norm || by[name].InheritedNorm {
			t.Errorf("%s reported as a norm", name)
		}
	}
	if want := []string{"Law", "NormativeAct"}; !reflect.DeepEqual(desc.NormClasses, want) {
		t.Errorf("norm_classes = %v, want %v", desc.NormClasses, want)
	}
	if !strings.Contains(strings.Join(by["Memo"].Issues, " "), "@norm takes no value") {
		t.Errorf("Memo's malformed @norm was not reported: %v", by["Memo"].Issues)
	}
	if !strings.Contains(strings.Join(by["Provision"].Issues, " "), "@norm marks a document class") {
		t.Errorf("Provision's @norm on an entity type was not reported: %v", by["Provision"].Issues)
	}
	for _, r := range desc.Relations {
		if r.Name == "cites" && !strings.Contains(strings.Join(r.Issues, " "), "@norm is not a relation directive") {
			t.Errorf("@norm on a relation was not reported: %v", r.Issues)
		}
	}
}

// TestDescribeOntology_ADraftDeclaresNoNorms: a draft governs nothing, so its @norm
// marks no class — norm_classes is empty, not absent.
func TestDescribeOntology_ADraftDeclaresNoNorms(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, normOntologyMD, memrank.OntologyDraftStatus)
	desc := describeOntologyVia(t, d, ctx)
	if desc.NormClasses == nil || len(desc.NormClasses) != 0 {
		t.Errorf("norm_classes = %#v, want [] for a draft", desc.NormClasses)
	}
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	if !strings.Contains(res.Text, `"norm_classes":[]`) {
		t.Errorf("norm_classes should be on the wire as [] so a consumer can tell no markers from an older runtime:\n%s", res.Text)
	}
}
