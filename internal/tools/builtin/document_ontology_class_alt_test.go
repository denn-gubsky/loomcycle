package builtin

import (
	"encoding/json"
	"reflect"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// classAltOntologyMD declares a document class with two other names, a subclass of it
// and a sibling with none.
const classAltOntologyMD = "# Tenant Ontology\n@version 1\n\n## Classes\n@section classes\n\n" +
	"### Document\n- `title` text @required\n\n" +
	"#### Runbook\n@alt playbook\n@alt operations manual\n\n" +
	"##### OnCallRunbook\n\n" +
	"#### Guide\n"

// TestDescribeOntology_AClassReportsItsAltNames: a class's @alt lines round-trip into
// describe_ontology as its own alt, not inherited by a subclass, and a class without
// one carries no alt key at all, so an ontology that declares none describes exactly
// as before.
func TestDescribeOntology_AClassReportsItsAltNames(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, classAltOntologyMD, memrank.OntologyConfirmedStatus)
	byName := describedClassByName(describeOntologyVia(t, d, ctx))
	if got := byName["Runbook"].Alt; !reflect.DeepEqual(got, []string{"playbook", "operations manual"}) {
		t.Errorf("Runbook alt = %q", got)
	}
	if got := byName["OnCallRunbook"].Alt; got != nil {
		t.Errorf("a subclass inherited alt %q", got)
	}
	if issues := byName["Runbook"].Issues; len(issues) != 0 {
		t.Errorf("Runbook issues = %q, want none", issues)
	}
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	var raw struct {
		Classes []map[string]json.RawMessage `json:"classes"`
	}
	if err := json.Unmarshal([]byte(res.Text), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range raw.Classes {
		var name string
		_ = json.Unmarshal(c["name"], &name)
		if _, has := c["alt"]; has != (name == "Runbook") {
			t.Errorf("class %s: alt key present = %v", name, has)
		}
	}
}
