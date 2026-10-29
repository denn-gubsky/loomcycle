package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// describeOntologyVia runs op=describe_ontology through Execute, the way an agent calls
// it, and decodes the answer.
func describeOntologyVia(t *testing.T, d *Document, ctx context.Context) ontologyDescription {
	t.Helper()
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	if res.IsError {
		t.Fatalf("describe_ontology: %s", res.Text)
	}
	var out ontologyDescription
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func describedClassByName(desc ontologyDescription) map[string]describedClass {
	out := map[string]describedClass{}
	for _, c := range desc.Classes {
		out[c.Name] = c
	}
	return out
}

// TestDescribeOntology_MatchesTheGates (RFC FP §13 R4): the description, the reader and
// the gates agree.
//
// Three sides: what describe_ontology answers over the wire, the internal read it is
// derived from, and what the gates actually enforce (the relation view, the anchor
// grammar a document of the class gets, the schemes in force). An agent acting on a
// description that drifts from the gates would write what the gates then refuse.
func TestDescribeOntology_MatchesTheGates(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("tenant ontology read failed")
	}

	if !desc.Exists || desc.Status != "confirmed" || !desc.Confirmed || desc.DocumentID != read.DocumentID ||
		desc.Version != read.RootRevision || desc.Version == 0 || !reflect.DeepEqual(desc.Sections, read.Sections) {
		t.Fatalf("header = exists %v status %q confirmed %v doc %q version %d sections %v; read doc %q revision %d sections %v",
			desc.Exists, desc.Status, desc.Confirmed, desc.DocumentID, desc.Version, desc.Sections, read.DocumentID, read.RootRevision, read.Sections)
	}

	// Classes: the same set the gates resolve, with inheritance applied.
	var gateClasses, described []string
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		gateClasses = append(gateClasses, c.Name)
	}
	byName := describedClassByName(desc)
	for name := range byName {
		described = append(described, name)
	}
	sort.Strings(gateClasses)
	sort.Strings(described)
	if !reflect.DeepEqual(gateClasses, described) {
		t.Errorf("classes = %v, gates resolve %v", described, gateClasses)
	}
	law := byName["Law"]
	if law.Kind != "document" || law.Parent != "NormativeAct" {
		t.Errorf("Law = %#v", law)
	}
	// Inherited from NormativeAct, which declares them; Law declares neither.
	if law.Directives["citation_grammar"] != "law-ua" || law.Directives["identifier_pattern"] == "" {
		t.Errorf("Law's directives should be the effective (inherited) ones: %v", law.Directives)
	}
	if len(law.Fields) == 0 || law.Fields[0].Name != "title" || !law.Fields[0].Required {
		t.Errorf("Law's fields should start with Document's inherited title: %#v", law.Fields)
	}
	if byName["incident"].Kind != "chunk" || byName["Provision"].Kind != "chunk" {
		t.Errorf("chunk classes: incident %#v, Provision %#v", byName["incident"], byName["Provision"])
	}

	// The anchor grammar a document of a class gets is the class's described one.
	_, rootID := newUserDoc(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+rootID+`","type":"Provision","revision":1}`); r.IsError {
		t.Fatalf("type the root: %s", r.Text)
	}
	userKey, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.anchorGrammarFor(ctx, userKey, rootID); got != "" {
		// akn-ua is named but never declared, so the gate disables it; the description
		// must not list it as a usable grammar either.
		t.Errorf("anchor grammar for Provision = %q, want none (akn-ua is undeclared)", got)
	}
	for _, g := range desc.Grammars {
		if g.Name == "akn-ua" {
			t.Error("an undeclared grammar was described")
		}
	}
	if len(desc.Grammars) != 1 || desc.Grammars[0].Name != "law-ua" || len(desc.Grammars[0].Patterns) != 1 ||
		!reflect.DeepEqual(desc.Grammars[0].UsedAs, []string{"citation"}) {
		t.Errorf("grammars = %#v", desc.Grammars)
	}

	// Relations: exactly the gate's vocabulary, entry for entry.
	gate := sortedRelations(d.relationView(ctx))
	if len(gate) == 0 || !reflect.DeepEqual(desc.Relations, gate) {
		t.Errorf("relations differ from the gate's view:\n described %#v\n gate      %#v", desc.Relations, gate)
	}

	// Schemes: exactly those in force.
	if schemes := d.EffectiveSchemesFor(ctx); len(schemes) == 0 || !reflect.DeepEqual(desc.Schemes, schemes) {
		t.Errorf("schemes = %#v, in force %#v", desc.Schemes, schemes)
	}
}

// TestDescribeOntology_ADraftDescribesOnlyTheSeed: a draft governs nothing, so neither
// its classes nor its relations, grammars or schemes are described as in force.
func TestDescribeOntology_ADraftDescribesOnlyTheSeed(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyDraftStatus)
	desc := describeOntologyVia(t, d, ctx)
	if !desc.Exists || desc.Status != "draft" || desc.Confirmed {
		t.Fatalf("header = %#v", desc)
	}
	byName := describedClassByName(desc)
	if _, ok := byName["Law"]; ok {
		t.Error("a draft's class was described as in force")
	}
	if _, ok := byName[memrank.DocumentClassRoot]; !ok {
		t.Error("the seed Document class is missing")
	}
	if len(desc.Relations) != 0 || len(desc.Grammars) != 0 || len(desc.Schemes) != 0 {
		t.Errorf("a draft described relations %v, grammars %v, schemes %v", desc.Relations, desc.Grammars, desc.Schemes)
	}
	if len(desc.Sections) == 0 {
		t.Error("the declared sections should still be listed")
	}
}

// TestDescribeOntology_NoDocumentDescribesTheSeed, and needs no tenant grant: the op
// reads the tenant ontology whatever the run's own scopes are.
func TestDescribeOntology_NoDocumentDescribesTheSeed(t *testing.T) {
	d, base, _ := documentFixture(t)
	desc := describeOntologyVia(t, d, base)
	if desc.Exists || desc.Status != "" || desc.Confirmed {
		t.Fatalf("header = %#v", desc)
	}
	byName := describedClassByName(desc)
	for _, seed := range []string{"person", "organization", memrank.DocumentClassRoot} {
		if _, ok := byName[seed]; !ok {
			t.Errorf("seed class %s missing", seed)
		}
	}
	if desc.Relations == nil || desc.Grammars == nil || desc.Schemes == nil {
		t.Error("empty lists should be [], not null")
	}

	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyConfirmedStatus)
	userOnly := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}}
	})
	if got := describeOntologyVia(t, d, userOnly); !got.Exists || !got.Confirmed {
		t.Errorf("a run without the tenant grant got %#v", got)
	}
}

// TestDescribeOntology_ReadDeniedOnTheOntologyIsRefused: the op asks read on the
// ontology document itself, by its Path name.
func TestDescribeOntology_ReadDeniedOnTheOntologyIsRefused(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyConfirmedStatus)
	withACL := func(rules ...authz.Rule) context.Context {
		eff := aclSet(t, rules...)
		return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.ACL = eff })
	}

	denied := withACL(authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:" + memrank.OntologyPath}})
	_, res := docExec(t, d, denied, `{"op":"describe_ontology"}`)
	refusedByACL(t, "describe_ontology with read denied on the ontology", res)

	elsewhere := withACL(authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/**"}})
	_, res = docExec(t, d, elsewhere, `{"op":"describe_ontology"}`)
	allowed(t, "describe_ontology with read denied elsewhere", res)
}

// TestDescribeOntologyRead_ReportsAPartialRead: a read that could not see every chunk
// says so, so a pass does not treat what it could not read as removed.
func TestDescribeOntologyRead_ReportsAPartialRead(t *testing.T) {
	if describeOntologyRead(OntologyRead{partial: true}).Partial != true {
		t.Error("a partial read was described as complete")
	}
	if describeOntologyRead(OntologyRead{}).Partial {
		t.Error("a complete read was described as partial")
	}
}

// TestDescribeOntology_AnchorGrammarIsTheOneTheGateUses: a declared anchor grammar is
// described on its class and as used for anchors, and is the grammar the anchor gate
// gives a document of that class.
func TestDescribeOntology_AnchorGrammarIsTheOneTheGateUses(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, relationsOntologyMD, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	_, rootID := newUserDoc(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+rootID+`","type":"Provision","revision":1}`); r.IsError {
		t.Fatalf("type the root: %s", r.Text)
	}
	userKey, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	gate := d.anchorGrammarFor(ctx, userKey, rootID)
	if got := describedClassByName(desc)["Provision"].Directives["anchor_grammar"]; gate != "art" || got != gate {
		t.Errorf("described anchor grammar %q, the gate uses %q", got, gate)
	}
	if len(desc.Grammars) != 1 || desc.Grammars[0].Name != "art" || !reflect.DeepEqual(desc.Grammars[0].UsedAs, []string{"anchor"}) {
		t.Errorf("grammars = %#v", desc.Grammars)
	}
}
