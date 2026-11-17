package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// countsCorpus is a concept fixture with documents about its concepts, and the
// chunk → document map a search hit is resolved through.
type countsCorpus struct {
	f        *conceptFixture
	docOf    map[string]string // chunk id → document id
	rm, out  string            // topics/risk-management, its narrower outsourcing
	law      string            // rank/law
	a, b, c  string            // documents
	cOnlyLaw string            // a chunk of c about rank/law
}

// newCountsCorpus writes:
//
//	/kb/a  Provision  one chunk about Risk Management, one about outsourcing (narrower)
//	/kb/b  Provision  one chunk about outsourcing
//	/kb/c  untyped    one chunk about rank/law, one primary_topic (a sub-kind) of Risk Management
//	/kb/x  Provision  about nothing
func newCountsCorpus(t *testing.T, d *Document, base context.Context) *countsCorpus {
	t.Helper()
	f := newConceptFixture(t, d, base)
	k := &countsCorpus{f: f, docOf: map[string]string{},
		rm: f.concepts["topics/risk-management"], out: f.concepts["topics/risk-management/outsourcing"], law: f.concepts["rank/law"]}
	if k.rm == "" || k.out == "" || k.law == "" {
		t.Fatalf("concepts = %v", f.concepts)
	}
	k.a = k.typedDoc("/kb/a", "Provision")
	k.link(k.a, k.rm, "subject")
	k.link(k.a, k.out, "subject")
	k.b = k.typedDoc("/kb/b", "Provision")
	k.link(k.b, k.out, "subject")
	k.c = k.typedDoc("/kb/c", "")
	k.cOnlyLaw = k.link(k.c, k.law, "subject")
	k.link(k.c, k.rm, "primary_topic")
	k.typedDoc("/kb/x", "Provision")
	return k
}

func (k *countsCorpus) typedDoc(path, typ string) string {
	k.f.c.t.Helper()
	v := map[string]any{"op": "create_document", "scope": "tenant", "title": "Doc", "path": path}
	if typ != "" {
		v["type"] = typ
	}
	return k.f.c.exec(v)["document_id"].(string)
}

// link writes a chunk of doc and an asserted statement from it to concept.
func (k *countsCorpus) link(doc, concept, kind string) string {
	k.f.c.t.Helper()
	ch := k.f.chunk("tenant", doc)
	k.docOf[ch] = doc
	k.f.link("link_chunks", ch, concept, kind)
	return ch
}

// describeCounted runs describe_ontology with_counts as ctx and decodes it.
func describeCounted(t *testing.T, d *Document, ctx context.Context) ontologyDescription {
	t.Helper()
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology","with_counts":true}`)
	if res.IsError {
		t.Fatalf("describe_ontology with_counts: %s", res.Text)
	}
	var out ontologyDescription
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

// conceptMembers is each described concept's members by tag; a concept with no
// members field fails the test (with_counts gives every concept one).
func conceptMembers(t *testing.T, desc ontologyDescription) (map[string]int, map[string]map[string]int) {
	t.Helper()
	n, byClass := map[string]int{}, map[string]map[string]int{}
	for _, s := range desc.Schemes {
		for _, c := range s.Concepts {
			if c.Members == nil {
				t.Fatalf("concept %s has no members field", c.Tag)
			}
			n[c.Tag], byClass[c.Tag] = *c.Members, c.MembersByClass
		}
	}
	if len(n) == 0 {
		t.Fatal("no concepts described")
	}
	return n, byClass
}

func schemeMembers(t *testing.T, desc ontologyDescription) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, s := range desc.Schemes {
		if s.Members == nil {
			t.Fatalf("scheme %s has no members field", s.Name)
		}
		out[s.Name] = *s.Members
	}
	return out
}

func classDocuments(desc ontologyDescription, name string) (int, bool) {
	for _, c := range desc.Classes {
		if c.Name == name && c.Documents != nil {
			return *c.Documents, true
		}
	}
	return 0, false
}

// TestDescribeOntology_ConceptCountEqualsConceptSearch — on both SQL Memory tiers,
// each concept's members is the number of distinct documents search concept:<tag>
// returns, and the corpus makes those numbers differ from concept to concept.
func TestDescribeOntology_ConceptCountEqualsConceptSearch(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertConceptCountEqualsSearch(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertConceptCountEqualsSearch(t, d, ctx)
	})
}

func assertConceptCountEqualsSearch(t *testing.T, d *Document, base context.Context) {
	k := newCountsCorpus(t, d, base)
	// A proposal and a closed chunk: the search filter's rule leaves out the first
	// and keeps the second (it does not read chunk dates without as_of), and so
	// must the count.
	proposed := k.typedDoc("/kb/proposed", "Provision")
	ch := k.f.chunk("tenant", proposed)
	k.f.link("propose_statement", ch, k.rm, "subject")
	closed := k.typedDoc("/kb/closed", "Provision")
	cc := k.link(closed, k.rm, "subject")
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": cc, "valid_at": 1000, "invalid_at": 2000,
		"revision": k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": cc})["revision"]})

	members, byClass := conceptMembers(t, describeCounted(t, d, k.f.reader))
	for tag, n := range members {
		ids, text := k.f.search(k.f.reader, map[string]any{"scope": "tenant", "concept": tag})
		docs := map[string]bool{}
		for _, id := range ids {
			doc, ok := k.docOf[id]
			if !ok {
				t.Fatalf("concept %s: search returned %s, a chunk the corpus did not write: %s", tag, id, text)
			}
			docs[doc] = true
		}
		if n != len(docs) {
			t.Errorf("concept %s: members = %d, search concept: returns %d documents: %s", tag, n, len(docs), text)
		}
	}
	// Not vacuous: the numbers the corpus was written to give.
	want := map[string]int{"topics/risk-management": 4, "topics/risk-management/outsourcing": 2, "rank/law": 1, "topics/reporting": 0}
	for tag, n := range want {
		if members[tag] != n {
			t.Errorf("concept %s: members = %d, want %d (all: %v)", tag, members[tag], n, members)
		}
	}
	if got := byClass["topics/risk-management"]; !reflect.DeepEqual(got, map[string]int{"Provision": 3}) {
		t.Errorf("risk-management members_by_class = %v, want Provision:3 (a, b, closed; c is untyped)", got)
	}
	if got := schemeMembers(t, describeCounted(t, d, k.f.reader)); got["topics"] != 4 || got["rank"] != 1 || got["file-plan"] != 0 {
		t.Errorf("scheme members = %v, want topics 4, rank 1, file-plan 0", got)
	}
	// Provision: a, b, x, proposed, closed — a class counts its documents whether
	// or not any concept reaches them.
	if n, ok := classDocuments(describeCounted(t, d, k.f.reader), "Provision"); !ok || n != 5 {
		t.Errorf("Provision documents = %d (present %v), want 5", n, ok)
	}
}

// TestDescribeOntology_CountsRollUpEachDocumentOnce — a document about a concept and
// about a narrower one, or about two narrower ones, counts once at the broader.
func TestDescribeOntology_CountsRollUpEachDocumentOnce(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	// One document reaching Risk Management three ways (itself, and outsourcing from
	// two chunks), and the scheme through two top-level concepts.
	doc := f.doc("tenant", "/kb/twice")
	f.link("link_chunks", f.chunk("tenant", doc), f.concepts["topics/risk-management/outsourcing"], "subject")
	f.link("link_chunks", f.chunk("tenant", doc), f.concepts["topics/risk-management/outsourcing"], "subject")
	f.link("link_chunks", f.chunk("tenant", doc), f.concepts["topics/risk-management"], "subject")
	f.link("link_chunks", f.chunk("tenant", doc), f.concepts["topics/reporting"], "subject")

	desc := describeCounted(t, d, f.reader)
	members, _ := conceptMembers(t, desc)
	for tag, want := range map[string]int{"topics/risk-management": 1, "topics/risk-management/outsourcing": 1, "topics/reporting": 1} {
		if members[tag] != want {
			t.Errorf("%s: members = %d, want %d", tag, members[tag], want)
		}
	}
	if got := schemeMembers(t, desc)["topics"]; got != 1 {
		t.Errorf("topics scheme: members = %d, want 1 — one document reaching three of its concepts", got)
	}
}

// TestDescribeOntology_CountsExcludeUnreadableDocuments — a document the caller's
// access-control set hides is counted nowhere: not on a concept, not in a class, not
// on a scheme. Both list shapes: a denied subtree (the bare scope readable) and an
// allow on part of the scope (the bare scope denied).
func TestDescribeOntology_CountsExcludeUnreadableDocuments(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newCountsCorpus(t, d, base)
	withACL := func(rules ...authz.Rule) context.Context {
		return tools.UpdateRunPolicy(k.f.reader, func(rp *tools.RunPolicy) { rp.ACL = aclSet(t, rules...) })
	}

	full := describeCounted(t, d, k.f.reader)
	if m, _ := conceptMembers(t, full); m["topics/risk-management/outsourcing"] != 2 {
		t.Fatalf("precondition: outsourcing has a and b: %v", m)
	}

	denyB := withACL(authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/b"}})
	desc := describeCounted(t, d, denyB)
	members, byClass := conceptMembers(t, desc)
	if members["topics/risk-management/outsourcing"] != 1 || members["topics/risk-management"] != 2 {
		t.Errorf("deny /kb/b: members = %v, want outsourcing 1, risk-management 2", members)
	}
	if !reflect.DeepEqual(byClass["topics/risk-management"], map[string]int{"Provision": 1}) {
		t.Errorf("deny /kb/b: risk-management members_by_class = %v, want Provision:1", byClass["topics/risk-management"])
	}
	if n, _ := classDocuments(desc, "Provision"); n != 2 {
		t.Errorf("deny /kb/b: Provision documents = %d, want 2 (a, x)", n)
	}
	if got := schemeMembers(t, desc)["topics"]; got != 2 {
		t.Errorf("deny /kb/b: topics members = %d, want 2", got)
	}

	// Enforce mode with allows only: the ontology and /kb/a are readable, nothing else.
	allowA := authz.WithMode(withACL(
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:" + memrank.OntologyPath}},
		// installTenantOntology files the ontology at its default /documents/<title> name.
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:/documents/**"}},
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/a"}}), authz.ModeEnforce)
	desc = describeCounted(t, d, allowA)
	members, _ = conceptMembers(t, desc)
	if members["topics/risk-management"] != 1 || members["topics/risk-management/outsourcing"] != 1 || members["rank/law"] != 0 {
		t.Errorf("allow /kb/a only: members = %v, want risk-management 1, outsourcing 1, law 0", members)
	}
	if n, _ := classDocuments(desc, "Provision"); n != 1 {
		t.Errorf("allow /kb/a only: Provision documents = %d, want 1", n)
	}
}

// TestDescribeOntology_ProposedAndRetiredStatementsAreNotCounted — membership is the
// live statements only: a proposal, a rejected proposal, a refuted statement and a
// retired one make no document a member.
func TestDescribeOntology_ProposedAndRetiredStatementsAreNotCounted(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	rm := f.concepts["topics/risk-management"]
	op := operatorCtx(f.operator)
	statement := func(path, how string) string {
		t.Helper()
		return f.link(how, f.chunk("tenant", f.doc("tenant", path)), rm, "subject")
	}
	statement("/kb/live", "link_chunks")
	statement("/kb/proposed", "propose_statement")
	rejected := statement("/kb/rejected", "propose_statement")
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"wrong"}`, rejected)); r.IsError {
		t.Fatalf("reject_statement: %s", r.Text)
	}
	refuted := statement("/kb/refuted", "link_chunks")
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"judge_fact","scope":"tenant","id":%q,"verdict":"unsupported","reason":"not about it"}`, refuted)); r.IsError {
		t.Fatalf("judge_fact: %s", r.Text)
	}
	retired := statement("/kb/retired", "link_chunks")
	tkey, _, err := d.resolveScope(f.reader, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.exec(f.reader, tkey, `UPDATE chunk_edges SET expired_at = ? WHERE id = ?`, int64(500), retired); err != nil {
		t.Fatalf("retire: %v", err)
	}

	members, _ := conceptMembers(t, describeCounted(t, d, f.reader))
	if members["topics/risk-management"] != 1 {
		t.Errorf("risk-management members = %d, want 1 (the live statement only)", members["topics/risk-management"])
	}
}

// TestDescribeOntology_WithoutCountsIsUnchanged — without with_counts (absent or
// false) the answer carries no count field at all, and with it the answer is the
// same answer plus the count fields alone.
func TestDescribeOntology_WithoutCountsIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newCountsCorpus(t, d, base)
	text := func(input string) string {
		t.Helper()
		_, res := docExec(t, d, k.f.reader, input)
		if res.IsError {
			t.Fatalf("%s: %s", input, res.Text)
		}
		return res.Text
	}
	plain := text(`{"op":"describe_ontology"}`)
	if off := text(`{"op":"describe_ontology","with_counts":false}`); off != plain {
		t.Errorf("with_counts:false differs from no argument:\n%s\n%s", off, plain)
	}
	for _, key := range []string{`"members"`, `"members_by_class"`, `"documents"`} {
		if strings.Contains(plain, key) {
			t.Errorf("without with_counts the answer carries %s: %s", key, plain)
		}
	}
	// Golden: the answer is exactly the pure derivation plus the steps the handler
	// always ran — re-marshalled here from the same read.
	var a, b map[string]any
	if err := json.Unmarshal([]byte(plain), &a); err != nil {
		t.Fatal(err)
	}
	counted := text(`{"op":"describe_ontology","with_counts":true}`)
	if err := json.Unmarshal([]byte(counted), &b); err != nil {
		t.Fatal(err)
	}
	stripCountFields(b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("with_counts changed more than the count fields:\n without %s\n with    %s", plain, counted)
	}
}

// stripCountFields removes the with_counts fields from a decoded description.
func stripCountFields(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "members")
		delete(x, "members_by_class")
		delete(x, "documents")
		for _, e := range x {
			stripCountFields(e)
		}
	case []any:
		for _, e := range x {
			stripCountFields(e)
		}
	}
}

// TestDescribeOntology_CountsNeedTheTenantScope — a caller without the tenant grant
// gets the ontology without counts and an issue saying why.
func TestDescribeOntology_CountsNeedTheTenantScope(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newCountsCorpus(t, d, base)
	userOnly := tools.UpdateRunPolicy(k.f.reader, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}}
	})
	_, res := docExec(t, d, userOnly, `{"op":"describe_ontology","with_counts":true}`)
	if res.IsError {
		t.Fatalf("describe_ontology: %s", res.Text)
	}
	if strings.Contains(res.Text, `"members"`) || strings.Contains(res.Text, `"documents"`) {
		t.Errorf("a caller without the tenant scope got counts: %s", res.Text)
	}
	if !strings.Contains(res.Text, "with_counts: member counts need the tenant scope") {
		t.Errorf("no issue explains the missing counts: %s", res.Text)
	}
}
