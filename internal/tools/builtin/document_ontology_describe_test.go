package builtin

import (
	"context"
	"encoding/json"
	"fmt"
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
	if law.Directives["citation_grammar"] != "law-ua" || law.Directives["identifier_pattern"] == "" ||
		law.Directives["definitions"] != `(?i)^Стаття 1\b|визначення термінів` {
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

// describedInstancesRaw runs describe_ontology and returns its instances as raw JSON
// objects, so a test can tell an absent field from an empty one — and the list itself
// as written, so [] can be told from null.
func describedInstancesRaw(t *testing.T, d *Document, ctx context.Context) (string, []map[string]json.RawMessage) {
	t.Helper()
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	if res.IsError {
		t.Fatalf("describe_ontology: %s", res.Text)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Text), &top); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	raw := string(top["instances"])
	var list []map[string]json.RawMessage
	_ = json.Unmarshal(top["instances"], &list)
	return raw, list
}

func rawInstance(list []map[string]json.RawMessage, name string) map[string]json.RawMessage {
	for _, in := range list {
		var n string
		if json.Unmarshal(in["name"], &n) == nil && n == name {
			return in
		}
	}
	return nil
}

func describedInstanceByName(desc ontologyDescription, name string) (describedInstance, bool) {
	for _, in := range desc.Instances {
		if in.Name == name {
			return in, true
		}
	}
	return describedInstance{}, false
}

// ontologyChunkIDByTitle reads an ontology chunk's id from the store by its title.
func ontologyChunkIDByTitle(t *testing.T, d *Document, ctx context.Context, title string) string {
	t.Helper()
	read, _ := d.tenantOntologyRead(ctx)
	res, err := d.query(ctx, mustTenantKey(t, d, ctx), `SELECT id FROM chunks WHERE document_id = ? AND title = ?`, read.DocumentID, title)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("ontology chunk %q: %v (%d rows)", title, err, len(res.Rows))
	}
	return asStr(res.Rows[0][0])
}

// TestDescribeOntology_AnAdoptedInstanceNamesItsSubject (RFC FP §13 R4): a confirmed
// instance is described with its chunk, and once adopted with the key it was adopted
// under and the subject node that key names — what the store holds, not a re-derivation.
// A renamed instance keeps both: its recorded key is no longer the derived one, and is
// trusted because the subject records it was adopted from this chunk.
func TestDescribeOntology_AnAdoptedInstanceNamesItsSubject(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)

	before := describeOntologyVia(t, d, ctx)
	var names []string
	for _, in := range before.Instances {
		names = append(names, in.Name)
		if in.RegistryKey != "" || in.SubjectChunkID != "" {
			t.Errorf("%s is not adopted yet but was described with %q / %q", in.Name, in.RegistryKey, in.SubjectChunkID)
		}
	}
	if want := []string{"Acme Bank", "Compliance Department", "Head of Compliance", "Compliance Officer", "Ghost"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("instances = %v, want document order %v", names, want)
	}
	acme, _ := describedInstanceByName(before, "Acme Bank")
	if acme.Class != "organization" || !reflect.DeepEqual(acme.Alt, []string{"Acme"}) ||
		acme.ChunkID != ontologyChunkIDByTitle(t, d, ctx, "Acme Bank") {
		t.Errorf("Acme Bank = %#v", acme)
	}
	if ghost, _ := describedInstanceByName(before, "Ghost"); ghost.Class != "" || len(ghost.Issues) == 0 {
		t.Errorf("Ghost should carry no class and an issue: %#v", ghost)
	}

	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatal(err)
	}
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+acme.ChunkID+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"title":"Acme Bank PLC"}`,
		acme.ChunkID, int(g["revision"].(float64)))); r.IsError {
		t.Fatal(r.Text)
	}

	desc := describeOntologyVia(t, d, ctx)
	key := mustTenantKey(t, d, ctx)
	for _, name := range []string{"Acme Bank PLC", "Head of Compliance", "Compliance Department"} {
		in, ok := describedInstanceByName(desc, name)
		if !ok {
			t.Fatalf("%s not described", name)
		}
		// The key is the one recorded on the instance chunk.
		ic, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+in.ChunkID+`"}`)
		recorded, _ := ic["fields"].(map[string]any)["registry_key"].(string)
		if recorded == "" || in.RegistryKey != recorded {
			t.Errorf("%s: registry_key %q, the instance chunk records %q", name, in.RegistryKey, recorded)
		}
		// The subject is the registry's node for that key, and records this instance.
		res, err := d.query(ctx, key, `SELECT natural_key FROM chunk_memory_meta WHERE chunk_id = ?`, in.SubjectChunkID)
		if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != in.RegistryKey {
			t.Errorf("%s: subject_chunk_id %q is not the registry node for %q", name, in.SubjectChunkID, in.RegistryKey)
		}
		sc, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+in.SubjectChunkID+`"}`)
		if f, _ := sc["fields"].(map[string]any); f["instance_chunk"] != in.ChunkID {
			t.Errorf("%s: the subject records instance_chunk %v, want %s", name, f["instance_chunk"], in.ChunkID)
		}
	}
	if ghost, _ := describedInstanceByName(desc, "Ghost"); ghost.RegistryKey != "" || ghost.SubjectChunkID != "" {
		t.Errorf("Ghost was never adopted: %#v", ghost)
	}
}

// TestDescribeOntology_ADraftDescribesNoInstances: a draft governs nothing, so an
// ontology taken back to draft describes no instances — even ones already adopted.
func TestDescribeOntology_ADraftDescribesNoInstances(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, list := describedInstancesRaw(t, d, ctx); len(list) == 0 {
		t.Fatal("the confirmed ontology described no instances; the draft check below would be vacuous")
	}
	read, _ := d.tenantOntologyRead(ctx)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+read.RootChunkID+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%d}`,
		read.RootChunkID, memrank.OntologyDraftStatus, int(g["revision"].(float64)))); r.IsError {
		t.Fatal(r.Text)
	}
	if raw, _ := describedInstancesRaw(t, d, ctx); raw != "[]" {
		t.Errorf("a draft described instances %s, want []", raw)
	}
}

// TestDescribeOntology_NoInstancesSectionDescribesNone: [] — never null — without an
// Instances section.
func TestDescribeOntology_NoInstancesSectionDescribesNone(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, relationsOntologyMD, memrank.OntologyConfirmedStatus)
	if raw, _ := describedInstancesRaw(t, d, ctx); raw != "[]" {
		t.Errorf("instances = %s, want []", raw)
	}
}

// TestDescribeOntology_AnUntrustedRegistryKeyIsNotReported (security review): a key
// recorded on an instance chunk is described only when adoption would trust it — the
// derived key, or a subject that records this instance chunk. A key copied from another
// instance, or naming a subject the ontology never adopted, would point the links agent
// at someone else's node.
func TestDescribeOntology_AnUntrustedRegistryKeyIsNotReported(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatal(err)
	}
	victimKey := memrank.ConsolidatorEntityKey("organization", "Victim Corp")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":"Victim Corp","path":"/facts/victim-corp","type":"organization","subject":"Victim Corp","natural_key":%q}`, victimKey)); r.IsError {
		t.Fatal(r.Text)
	}
	read, _ := d.tenantOntologyRead(ctx)
	sec := read.SectionChunks[memrank.SectionInstances]
	acmeKey := memrank.ConsolidatorEntityKey("organization", "Acme Bank")
	for name, forged := range map[string]string{"Mallory Ltd": victimKey, "Eve Ltd": acmeKey} {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":%q,"body":"@a organization\n","fields":{"registry_key":%q}}`,
			read.DocumentID, sec, name, forged)); r.IsError {
			t.Fatal(r.Text)
		}
	}

	_, list := describedInstancesRaw(t, d, ctx)
	if acme := rawInstance(list, "Acme Bank"); acme["registry_key"] == nil || acme["subject_chunk_id"] == nil {
		t.Fatalf("the adopted Acme Bank should name its subject: %v", acme)
	}
	for _, name := range []string{"Mallory Ltd", "Eve Ltd"} {
		in := rawInstance(list, name)
		if in == nil {
			t.Fatalf("%s not described", name)
		}
		if _, has := in["registry_key"]; has {
			t.Errorf("%s: an untrusted registry_key was described: %s", name, in["registry_key"])
		}
		if _, has := in["subject_chunk_id"]; has {
			t.Errorf("%s: an untrusted subject was described: %s", name, in["subject_chunk_id"])
		}
	}
}

// TestDescribeOntology_ASubjectTheCallerCannotReadIsNotNamed: the ontology is config and
// needs neither the tenant grant nor read beyond its own document, but a subject node is
// tenant data. A caller that may not read it — no tenant grant, or read denied where it
// is filed — gets the instance without its subject, which neither leaks the node nor
// claims the instance is unadopted in a way it could act on.
func TestDescribeOntology_ASubjectTheCallerCannotReadIsNotNamed(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, list := describedInstancesRaw(t, d, ctx); rawInstance(list, "Acme Bank")["subject_chunk_id"] == nil {
		t.Fatal("with access the subject should be named; the checks below would be vacuous")
	}

	denied := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/facts/**"}})
	})
	userOnly := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}}
	})
	for what, c := range map[string]context.Context{"read denied on /facts": denied, "no tenant grant": userOnly} {
		_, list := describedInstancesRaw(t, d, c)
		acme := rawInstance(list, "Acme Bank")
		if acme == nil || acme["chunk_id"] == nil {
			t.Fatalf("%s: the instance itself should still be described: %v", what, acme)
		}
		if _, has := acme["subject_chunk_id"]; has {
			t.Errorf("%s: subject_chunk_id described", what)
		}
		if _, has := acme["registry_key"]; has {
			t.Errorf("%s: registry_key described", what)
		}
	}
}
