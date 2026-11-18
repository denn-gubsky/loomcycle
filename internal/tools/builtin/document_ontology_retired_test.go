package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type retiredFixture struct {
	t   *testing.T
	d   *Document
	ctx context.Context // the operator plane, tenant and user scopes granted
}

func newRetiredFixture(t *testing.T) *retiredFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	return &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}
}

func (f *retiredFixture) do(ctx context.Context, v map[string]any) (map[string]any, tools.Result) {
	f.t.Helper()
	b, _ := json.Marshal(v)
	return docExec(f.t, f.d, ctx, string(b))
}

func (f *retiredFixture) must(v map[string]any) map[string]any {
	f.t.Helper()
	out, r := f.do(f.ctx, v)
	if r.IsError {
		f.t.Fatalf("%v: %s", v["op"], r.Text)
	}
	return out
}

func (f *retiredFixture) read() OntologyRead {
	f.t.Helper()
	read, ok := f.d.tenantOntologyRead(f.ctx)
	if !ok {
		f.t.Fatal("the ontology could not be read")
	}
	return read
}

// chunk finds an ontology chunk by its title.
func (f *retiredFixture) chunk(title string) string {
	f.t.Helper()
	read := f.read()
	res, err := f.d.query(f.ctx, mustTenantKey(f.t, f.d, f.ctx),
		`SELECT id FROM chunks WHERE document_id = ? AND title = ?`, read.DocumentID, title)
	if err != nil || len(res.Rows) != 1 {
		f.t.Fatalf("chunk %q: %v %v", title, err, res)
	}
	return asStr(res.Rows[0][0])
}

// setStatus is the operator's update_chunk of a chunk's status; it returns the refusal
// text, "" when applied.
func (f *retiredFixture) setStatus(ctx context.Context, id, status string) string {
	f.t.Helper()
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	_, r := f.do(ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": id,
		"revision": int(g["revision"].(float64)), "status": status})
	if r.IsError {
		return r.Text
	}
	return ""
}

func (f *retiredFixture) retire(title string) {
	f.t.Helper()
	if text := f.setStatus(f.ctx, f.chunk(title), memrank.OntologyStatusRetired); text != "" {
		f.t.Fatalf("retire %s: %s", title, text)
	}
}

func (f *retiredFixture) status(id string) string {
	f.t.Helper()
	s, _ := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["status"].(string)
	return s
}

func conceptOfTag(read OntologyRead, tag string) memrank.OntologyConcept {
	return schemeConcepts(read)[tag]
}

func termByName(read OntologyRead, name string) (memrank.OntologyTerm, bool) {
	for _, t := range read.Terms {
		if t.Name == name {
			return t, true
		}
	}
	return memrank.OntologyTerm{}, false
}

// TestOntology_RetiringAnEntryRetiresItsSubtree: the operator's update_chunk retires the
// entry and every live descendant in the same change; each reads back retired and stays
// in the read. A child an operator brings back while its parent stays retired is
// reported; clearing the parent's status un-retires it.
func TestOntology_RetiringAnEntryRetiresItsSubtree(t *testing.T) {
	f := newRetiredFixture(t)
	f.retire("Risk Management")
	f.retire("Policy")
	f.retire("supervises")

	read := f.read()
	for _, tag := range []string{"topics/risk-management", "topics/risk-management/outsourcing"} {
		c := conceptOfTag(read, tag)
		if c.ChunkID == "" || c.EntryStatus != memrank.OntologyStatusRetired {
			t.Errorf("%s = %+v, want it in the read and retired", tag, c)
		}
	}
	if c := conceptOfTag(read, "topics/reporting"); c.EntryStatus != "" {
		t.Errorf("a sibling was retired: %+v", c)
	}
	for _, name := range []string{"Policy", "Internal Policy"} {
		if tm, ok := termByName(read, name); !ok || tm.EntryStatus != memrank.OntologyStatusRetired {
			t.Errorf("class %s = %+v (in read %v), want retired", name, tm, ok)
		}
	}
	for _, r := range read.Relations {
		if (r.Name == "supervises" || r.Name == "co_supervises") != (r.EntryStatus == memrank.OntologyStatusRetired) {
			t.Errorf("relation %s entry_status = %q", r.Name, r.EntryStatus)
		}
	}
	if len(read.Issues) != 0 {
		t.Errorf("a consistent retirement raised issues: %v", read.Issues)
	}

	// The operator brings the child back alone: it is live under a retired parent.
	if text := f.setStatus(f.ctx, f.chunk("outsourcing"), ""); text != "" {
		t.Fatal(text)
	}
	if got := strings.Join(f.read().Issues, "\n"); !strings.Contains(got, `"outsourcing" is live under the retired "Risk Management"`) {
		t.Errorf("issues = %q, want the live child under a retired parent reported", got)
	}
	// Un-retire: the operator clears the parent's status.
	if text := f.setStatus(f.ctx, f.chunk("Risk Management"), ""); text != "" {
		t.Fatal(text)
	}
	read = f.read()
	if c := conceptOfTag(read, "topics/risk-management"); c.EntryStatus != "" || len(read.Issues) != 0 {
		t.Errorf("after un-retiring: %+v, issues %v", c, read.Issues)
	}
}

// TestOntology_SectionHeaderAndRootCannotBeRetired: neither the root, a section header
// nor a proposal is an entry, so none of them can be retired; outside the ontology the
// word is an ordinary status.
func TestOntology_SectionHeaderAndRootCannotBeRetired(t *testing.T) {
	f := newRetiredFixture(t)
	read := f.read()
	if text := f.setStatus(f.ctx, read.RootChunkID, memrank.OntologyStatusRetired); !strings.Contains(text, "root cannot be retired") {
		t.Errorf("root: %q", text)
	}
	if text := f.setStatus(f.ctx, read.SectionChunks[memrank.SectionSchemes], memrank.OntologyStatusRetired); !strings.Contains(text, "section header cannot be retired") {
		t.Errorf("section header: %q", text)
	}
	prop := f.must(map[string]any{"op": "propose_entity", "name": "Contract", "parent": "Document"})
	if text := f.setStatus(f.ctx, prop["chunk_id"].(string), memrank.OntologyStatusRetired); !strings.Contains(text, "proposal is not an entry") {
		t.Errorf("proposal: %q", text)
	}
	out := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Elsewhere"})
	if text := f.setStatus(f.ctx, out["root_chunk_id"].(string), "retired"); text != "" {
		t.Errorf("a document outside the ontology may use the word: %s", text)
	}
}

// TestOntologyGuard_AgentCannotRetireAnEntry: retiring is the operator's write; an
// agent's update_chunk on the ontology is refused by the guard.
func TestOntologyGuard_AgentCannotRetireAnEntry(t *testing.T) {
	f := newRetiredFixture(t)
	agent := tools.WithCallerTier(inRunCtx(f.ctx), tools.TierMember)
	if text := f.setStatus(agent, f.chunk("Provision"), memrank.OntologyStatusRetired); !strings.Contains(text, "may not modify the ontology") {
		t.Errorf("an agent retired an entry: %q", text)
	}
	if s := f.status(f.chunk("Provision")); s != "" {
		t.Errorf("status = %q after a refused retire", s)
	}
}

// TestOntology_RetiredConceptKeepsItsPolicy: what is already placed under a retired
// concept keeps the concept's retention (RFC GQ 7).
func TestOntology_RetiredConceptKeepsItsPolicy(t *testing.T) {
	f := newRetiredFixture(t)
	read := f.read()
	out := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Vendor risk", "path": "/kb/vendor"})
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": out["root_chunk_id"],
		"to_id": conceptOfTag(read, "topics/risk-management/outsourcing").ChunkID, "kind": "subject"})
	f.retire("Risk Management")
	_, r := f.do(f.ctx, map[string]any{"op": "effective_policy", "scope": "tenant", "document_id": out["document_id"]})
	if r.IsError {
		t.Fatal(r.Text)
	}
	var a effectivePolicyAnswer
	_ = json.Unmarshal([]byte(r.Text), &a)
	if a.Retention != "10y" || len(a.Concepts) != 1 || a.Concepts[0].RetentionFrom != "topics/risk-management" {
		t.Errorf("policy after retiring = %+v, want 10y inherited from topics/risk-management", a)
	}
}

// TestProposeEntity_RetiredNameIsRefusedWithUnretirePointer: a retired entry is brought
// back by an operator, never proposed again — in every section.
func TestProposeEntity_RetiredNameIsRefusedWithUnretirePointer(t *testing.T) {
	f := newRetiredFixture(t)
	for _, title := range []string{"Provision", "filed_under", "Reporting", "Globex"} {
		f.retire(title)
	}
	for _, in := range []map[string]any{
		{"name": "Provision"},
		{"name": "filed_under", "section": "relations"},
		{"name": "Reporting", "section": "schemes", "parent": "topics"},
		{"name": "Globex", "section": "instances", "parent": "organization"},
	} {
		in["op"] = "propose_entity"
		_, r := f.do(inRunCtx(f.ctx), in)
		if !r.IsError || !strings.Contains(r.Text, "is retired") || !strings.Contains(r.Text, "clearing its status") {
			t.Errorf("%v: %q, want a refusal pointing at un-retiring", in, r.Text)
		}
	}
}

// TestProposeEntity_RetiredParentIsRefused: nothing new is filed under a retired entry.
func TestProposeEntity_RetiredParentIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	for _, title := range []string{"Policy", "supervises", "Risk Management", "event"} {
		f.retire(title)
	}
	for _, in := range []map[string]any{
		{"name": "Board Policy", "parent": "Policy"},
		{"name": "oversees", "section": "relations", "parent": "supervises"},
		{"name": "cyber", "section": "schemes", "parent": "Risk Management"},
		{"name": "Launch", "section": "instances", "parent": "event"},
		{"name": "Sectors", "section": "schemes", "concepts": []any{map[string]any{"name": "Energy"}},
			"header": map[string]any{"projects": []any{"Policy"}}},
	} {
		in["op"] = "propose_entity"
		_, r := f.do(inRunCtx(f.ctx), in)
		if !r.IsError || !strings.Contains(r.Text, "entry_retired") {
			t.Errorf("%v: %q, want entry_retired", in, r.Text)
		}
	}
}

// TestOntology_RetiredClassIsDescribedAndMarked: describe_ontology keeps every retired
// entry, marked in entry_status — not `retired`, which names a superseded fact.
func TestOntology_RetiredClassIsDescribedAndMarked(t *testing.T) {
	f := newRetiredFixture(t)
	for _, title := range []string{"Memo", "co_supervises", "outsourcing", "Acme"} {
		f.retire(title)
	}
	_, r := f.do(f.ctx, map[string]any{"op": "describe_ontology"})
	if r.IsError {
		t.Fatal(r.Text)
	}
	if strings.Contains(r.Text, `"retired"`+":") {
		t.Errorf("an entry was marked with the field `retired`")
	}
	var desc ontologyDescription
	_ = json.Unmarshal([]byte(r.Text), &desc)
	marked := map[string]string{}
	for _, c := range desc.Classes {
		marked["class "+c.Name] = c.EntryStatus
	}
	for _, rel := range desc.Relations {
		marked["relation "+rel.Name] = rel.EntryStatus
	}
	for _, s := range desc.Schemes {
		for _, c := range s.Concepts {
			marked["concept "+c.Tag] = c.EntryStatus
		}
	}
	for _, in := range desc.Instances {
		marked["instance "+in.Name] = in.EntryStatus
	}
	for k, want := range map[string]string{
		"class Memo": "retired", "class Policy": "", "relation co_supervises": "retired", "relation supervises": "",
		"concept topics/risk-management/outsourcing": "retired", "concept topics/risk-management": "",
		"instance Acme": "retired", "instance Globex": "",
	} {
		got, ok := marked[k]
		if !ok || got != want {
			t.Errorf("%s: entry_status %q (described %v), want %q", k, got, ok, want)
		}
	}
}

// TestClassify_RetiredConceptIsNotOffered: classify offers the model no retired concept
// and no retired class.
func TestClassify_RetiredConceptIsNotOffered(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	stub := &stubClassifier{verdict: memrank.ClassifyVerdict{Method: memrank.ClassifyMethodChat, Concepts: []string{"Risk Management"}}}
	f.d.Classifier = stub
	rf := &retiredFixture{t: t, d: f.d, ctx: f.ctx}
	rf.retire("Risk Management")
	rf.retire("Order")
	docID, _ := f.doc("tenant", "Розпорядження 5", "/kb/x/5", "", [2]string{"Пункт 1", "управління ризиками"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if len(stub.asked) != 1 {
		t.Fatalf("asked %d times", len(stub.asked))
	}
	if n := len(stub.asked[0].Concepts); n != 0 {
		t.Errorf("a retired concept was offered: %+v", stub.asked[0].Concepts)
	}
	for _, c := range stub.asked[0].Classes {
		if c.Name == "Order" {
			t.Errorf("the retired class Order was offered: %+v", stub.asked[0].Classes)
		}
	}
	if len(a.Concepts) != 0 {
		t.Errorf("a retired concept was proposed: %+v", a.Concepts)
	}
}

// TestRelationGate_NewStatementOfRetiredKindIsRefused: a new statement of a retired
// kind, or to a retired concept, is refused with entry_retired.
func TestRelationGate_NewStatementOfRetiredKindIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	a := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A"})["root_chunk_id"]
	b := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "B"})["root_chunk_id"]
	reporting := conceptOfTag(f.read(), "topics/reporting").ChunkID
	f.retire("supervises")
	f.retire("Reporting")
	for _, in := range []map[string]any{
		{"op": "link_chunks", "from_id": a, "to_id": b, "kind": "supervises"},
		{"op": "link_chunks", "from_id": a, "to_id": b, "kind": "co_supervises"},
		{"op": "propose_statement", "from_id": a, "to_id": b, "kind": "supervises"},
		{"op": "link_chunks", "from_id": a, "to_id": reporting, "kind": "filed_under"},
	} {
		in["scope"] = "tenant"
		_, r := f.do(f.ctx, in)
		if !r.IsError || !strings.Contains(r.Text, "entry_retired") {
			t.Errorf("%v: %q, want entry_retired", in, r.Text)
		}
	}
}

// TestRelationGate_ExistingStatementsOfRetiredKindStayReadable: what was written before
// the kind was retired stays, unmarked, and writing the same statement again is an edit.
func TestRelationGate_ExistingStatementsOfRetiredKindStayReadable(t *testing.T) {
	f := newRetiredFixture(t)
	a := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A"})["root_chunk_id"]
	b := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "B"})["root_chunk_id"]
	id := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"})["id"]
	f.retire("supervises")
	edges := edgesOf(t, f.d, f.ctx, fmt.Sprintf(`{"op":"get_edges","scope":"tenant","document_id":%q,"kind":"supervises"}`,
		f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": a})["document_id"]))
	if len(edges) != 1 || edges[0]["id"] != id || edges[0]["status"] != edgeStatusAsserted {
		t.Fatalf("edges after retiring = %v", edges)
	}
	if attrs, _ := edges[0]["attributes"].(map[string]any); attrs["ontology_valid"] == false {
		t.Errorf("an existing statement was marked invalid: %v", attrs)
	}
	again := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"})
	if again["id"] != id {
		t.Errorf("re-writing the existing statement gave %v, want %v", again["id"], id)
	}
}

// TestSearch_ConceptFilterStillFindsMembersOfARetiredConcept: the concept filter keeps
// working for what is already placed under a retired concept and its narrower ones.
func TestSearch_ConceptFilterStillFindsMembersOfARetiredConcept(t *testing.T) {
	d, base, _ := documentFixture(t)
	cf := newConceptFixture(t, d, base)
	kb := cf.doc("tenant", "/kb/a")
	direct := cf.chunk("tenant", kb)
	cf.link("link_chunks", direct, cf.concepts["topics/risk-management"], "subject")
	narrower := cf.chunk("tenant", kb)
	cf.link("link_chunks", narrower, cf.concepts["topics/risk-management/outsourcing"], "subject")
	rf := &retiredFixture{t: t, d: d, ctx: cf.operator}
	rf.retire("Risk Management")
	ids, text := cf.search(cf.reader, map[string]any{"scope": "tenant", "concept": "topics/risk-management"})
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[direct] || !got[narrower] {
		t.Errorf("concept filter after retiring = %v (%s), want %s and %s", ids, text, direct, narrower)
	}
}

// TestOntologyConfirm_RetiredInstanceIsNotAdopted: a retired instance keeps the subject
// it was adopted as; adoption neither adopts it again nor reports it orphaned. One
// retired before it was ever adopted is not adopted.
func TestOntologyConfirm_RetiredInstanceIsNotAdopted(t *testing.T) {
	f := newRetiredFixture(t)
	f.retire("Globex")
	rep, err := f.d.AdoptInstances(f.ctx, false)
	if err != nil || rep.Adopted != 1 {
		t.Fatalf("first adoption = %+v, %v; want Acme alone", rep, err)
	}
	f.retire("Acme")
	rep, err = f.d.AdoptInstances(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Adopted != 0 || len(rep.Orphaned) != 0 {
		t.Errorf("after retiring = %+v, want nothing adopted and nothing orphaned", rep)
	}
	states := map[string]string{}
	for _, st := range rep.Instances {
		states[st.Name] = st.State
	}
	if states["Acme"] != "retired" || states["Globex"] != "retired" {
		t.Errorf("states = %v", states)
	}
	key := mustTenantKey(t, f.d, f.ctx)
	if id, _ := f.d.chunkIDByNaturalKey(f.ctx, key, "organization:acme"); id == "" {
		t.Error("the retired instance lost its subject")
	}
	if id, _ := f.d.chunkIDByNaturalKey(f.ctx, key, "organization:globex"); id != "" {
		t.Error("an instance retired before adoption was adopted")
	}
}

// TestOntology_SeedEntryMustBeAdoptedBeforeRetiring: a seed class has no chunk, so it is
// retired by retiring the tenant entry that replaces it; the retired entry keeps
// replacing the seed — the seed does not come back, in the prompt or for a new write.
func TestOntology_SeedEntryMustBeAdoptedBeforeRetiring(t *testing.T) {
	f := newRetiredFixture(t)
	f.retire("event")
	read := f.read()
	var event *memrank.OntologyTerm
	for _, tm := range memrank.EffectiveClasses(read.Terms, true) {
		if strings.EqualFold(tm.Name, "event") {
			tm := tm
			event = &tm
		}
	}
	if event == nil || event.Source != "tenant" || event.EntryStatus != memrank.OntologyStatusRetired {
		t.Fatalf("event = %+v, want the retired tenant entry, not the seed", event)
	}
	if p := memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true); strings.Contains(p, "**event**") {
		t.Errorf("the seed event came back in the prompt:\n%s", p)
	}
	_, r := f.do(f.ctx, map[string]any{"op": "upsert_chunk", "scope": "user", "natural_key": "event:launch",
		"type": "event", "subject": "Launch", "title": "Launch", "body": "the launch"})
	if !r.IsError || !strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a new fact of the retired event: %q", r.Text)
	}
}

// TestOntology_RetiredClassRefusesNewTypedWrites: a new document of a retired class, a
// change of type to it and a concept field set to a retired concept are refused; editing
// a document that already has the type, or already holds the concept, is not.
func TestOntology_RetiredClassRefusesNewTypedWrites(t *testing.T) {
	f := newRetiredFixture(t)
	old := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Old memo", "type": "Memo"})
	pol := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Risk policy", "type": "Policy"})
	prev := int(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": pol["root_chunk_id"]})["revision"].(float64))
	f.must(map[string]any{"op": "update_chunk", "scope": "tenant", "id": pol["root_chunk_id"], "revision": prev,
		"fields": map[string]any{"topic": "Reporting"}})
	f.retire("Memo")
	f.retire("Reporting")

	if _, r := f.do(f.ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "New memo", "type": "Memo"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a new document of a retired class: %q", r.Text)
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "New policy", "type": "Policy",
		"fields": map[string]any{"topic": "Reporting"}}); !r.IsError || !strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a concept field set to a retired concept: %q", r.Text)
	}
	root := old["root_chunk_id"].(string)
	rev := int(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": root})["revision"].(float64))
	if _, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": root, "revision": rev, "title": "Old memo, edited"}); r.IsError {
		t.Errorf("editing a document that already has the retired type: %s", r.Text)
	}
	proot := pol["root_chunk_id"].(string)
	rev = int(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": proot})["revision"].(float64))
	if _, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": proot, "revision": rev,
		"fields": map[string]any{"topic": "Reporting", "note": "kept"}}); r.IsError {
		t.Errorf("editing a document that already holds the retired concept: %s", r.Text)
	}
	other := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Provision 1", "type": "Provision"})["root_chunk_id"].(string)
	rev = int(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": other})["revision"].(float64))
	if _, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": other, "revision": rev, "type": "Memo"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a change of type to a retired class: %q", r.Text)
	}
}

// TestOntology_RetiredInAnUnknownSectionRaisesNoIssue: an unknown section's entries are
// not in force, so a live one under a retired one there is not reported.
func TestOntology_RetiredInAnUnknownSectionRaisesNoIssue(t *testing.T) {
	md := retiredOntologyMD + "\n## Questions\n@section questions\n\n### Who owns it\n\n#### Follow-up\n"
	d, base, _ := documentFixture(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	id := f.chunk("Who owns it")
	if text := f.setStatus(f.ctx, id, memrank.OntologyStatusRetired); text != "" {
		t.Fatal(text)
	}
	if text := f.setStatus(f.ctx, f.chunk("Follow-up"), ""); text != "" {
		t.Fatal(text)
	}
	for _, iss := range f.read().Issues {
		if strings.Contains(iss, "live under the retired") {
			t.Errorf("an out-of-force entry was reported: %q", iss)
		}
	}
}

// TestOntologySchemes_RetiredEntryOnPostgres runs the retire cascade, the read and the
// write refusals on the postgres tier.
func TestOntologySchemes_RetiredEntryOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}
	reporting := conceptOfTag(f.read(), "topics/reporting").ChunkID
	f.retire("Risk Management")
	f.retire("Reporting")
	f.retire("Memo")
	if s := f.status(f.chunk("outsourcing")); s != memrank.OntologyStatusRetired {
		t.Errorf("the narrower concept was not retired with its parent on postgres: %q", s)
	}
	if c := conceptOfTag(f.read(), "topics/risk-management/outsourcing"); c.EntryStatus != memrank.OntologyStatusRetired {
		t.Errorf("outsourcing = %+v", c)
	}
	root := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A"})["root_chunk_id"]
	if _, r := f.do(f.ctx, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": root, "to_id": reporting, "kind": "filed_under"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a new placement under a retired concept on postgres: %q", r.Text)
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "M", "type": "Memo"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a new document of a retired class on postgres: %q", r.Text)
	}
}

// TestMirrorSchemeFocus_RetiredEntryKeepsItsStatementAndWritesNone: a retired concept's
// focus statement is kept, and none is written to a retired class.
func TestMirrorSchemeFocus_RetiredEntryKeepsItsStatementAndWritesNone(t *testing.T) {
	md := strings.Replace(retiredOntologyMD, "#### Reporting\n", "#### Reporting\n@focus Provision\n\n#### Duties\n@focus Memo\n", 1)
	d, base, _ := documentFixture(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	f.retire("Memo")
	rep, err := f.d.MirrorSchemeFocus(f.ctx, false)
	if err != nil || rep.Written != 1 {
		t.Fatalf("first focus = %+v, %v; want Reporting's alone", rep, err)
	}
	f.retire("Reporting")
	rep, err = f.d.MirrorSchemeFocus(f.ctx, false)
	if err != nil || rep.Written != 0 || rep.Retired != 0 {
		t.Errorf("after retiring = %+v, %v; want nothing written and nothing retired", rep, err)
	}
	if !reflect.DeepEqual(rep.Errors, []string(nil)) {
		t.Errorf("errors = %v", rep.Errors)
	}
}
