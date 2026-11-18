package builtin

// Regression tests for the class lookup the @focus mirror and propose_entity share:
// only a class the walk put in force counts (RFC GM P1.1 review).

import (
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestClassChunkID_PassesOverARelationOfTheSameName: a relation entry titled like a
// class is not the class's chunk. organization is a standard class with no entry, so
// a @focus on it has no chunk to point at — never the relation's.
func TestClassChunkID_PassesOverARelationOfTheSameName(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(focusOntologyMD, "## Relations\n@section relations\n\n",
		"## Relations\n@section relations\n\n### Organization\nA relation that shares a class's name.\n\n", 1)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("ontology read failed")
	}
	if id := read.classChunkID("organization"); id != "" {
		t.Errorf("classChunkID(organization) = %q; want none (a relation entry is not a class)", id)
	}
	if id := read.classChunkID("provider"); id == "" {
		t.Error("the declared Provider class was not found by its name")
	}
}

// TestProposeEntity_ClassParentMustBeAnInForceClass: a class proposal can be filed
// only under a class in force — not under a concept, an instance or a relation that
// shares a name — and its body carries no live directive.
func TestProposeEntity_ClassParentMustBeAnInForceClass(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	for _, parent := range []string{"Misc", "Acme work", "Acme Bank", "components"} {
		body := fmt.Sprintf(`{"op":"propose_entity","scope":"user","name":"Widget %s","parent":%q,"body":"@focus Acme Bank"}`, parent, parent)
		if _, r := docExec(t, d, ctx, body); !r.IsError {
			t.Errorf("a class proposal was filed under %q, which is not a class", parent)
		}
	}
	out, r := docExec(t, d, ctx, `{"op":"propose_entity","scope":"user","name":"Gateway","parent":"Provider",`+
		`"body":"A provider behind a gateway.\n@focus Acme Bank\n@retention 1d\n- `+"`@acl`"+` admins"}`)
	if r.IsError {
		t.Fatalf("a class proposal under a class: %s", refusalText(r))
	}
	ids := focusIDs(t, d, ctx)
	if out["parent_id"] != ids.providerClass {
		t.Errorf("parent_id = %v, want the Provider class %s", out["parent_id"], ids.providerClass)
	}
	read, _ := d.tenantOntologyRead(ctx)
	var body string
	for _, p := range read.Proposals {
		if p.Name == "Gateway" {
			body = p.Body
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if t2 := strings.TrimSpace(line); strings.HasPrefix(t2, "@") || strings.HasPrefix(t2, "- `@") {
			t.Errorf("a live directive survived in the class proposal: %q (body %q)", line, body)
		}
	}
	if !strings.Contains(body, "A provider behind a gateway.") {
		t.Errorf("the evidence was lost: %q", body)
	}
}
