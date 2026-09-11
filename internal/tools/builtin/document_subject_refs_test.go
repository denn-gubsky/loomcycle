package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// The reader contract: a relation-fact is reachable from EVERY subject it names.
//
// Subject-homing files a fact under one subject because containment is
// single-parent. "Dave works at the shop" therefore lives in Dave's document and
// references the shop. Read Dave and it is there; read the shop by its tree and it
// is not — so every reader of a subject's facts has to add the inbound references,
// or the second subject silently loses every relation it is named in.

// subjectFixture builds two subject documents and the three facts that make the
// contract testable: one about Dave alone, one about the shop alone, and one about
// BOTH — filed under Dave, referencing the shop.
type subjectFixture struct {
	t                          *testing.T
	d                          *Document
	ctx                        contextT
	daveDoc, daveRoot          string
	shopDoc, shopRoot          string
	daveOnly, shopOnly, shared string
}

func newSubjectFixture(t *testing.T) *subjectFixture {
	t.Helper()
	d, ctx, _ := documentFixture(t)
	f := &subjectFixture{t: t, d: d, ctx: ctx}
	f.daveDoc, f.daveRoot = f.subjectDoc("Dave", "person:dave", "/facts/dave")
	f.shopDoc, f.shopRoot = f.subjectDoc("Shop", "organization:shop", "/facts/shop")

	f.daveOnly = f.fact(f.daveDoc, f.daveRoot, "memory/fact/dave-writes-go", "Dave writes Go.", nil)
	f.shopOnly = f.fact(f.shopDoc, f.shopRoot, "memory/fact/shop-opens-nine", "The shop opens at nine.", nil)
	// THE RELATION FACT. Filed under Dave — the tree gives it one home — and
	// referencing the shop, which is the only trace of the second subject.
	f.shared = f.fact(f.daveDoc, f.daveRoot, "memory/fact/dave-works-at-shop",
		"Dave works at the shop.", []string{f.shopRoot})
	return f
}

// subjectDoc creates a subject's document the way subject-homing does: the ROOT
// chunk is the entity node.
func (f *subjectFixture) subjectDoc(name, key, path string) (docID, rootID string) {
	f.t.Helper()
	out, r := docExec(f.t, f.d, f.ctx, `{"op":"create_document","scope":"user","title":"`+name+
		`","path":"`+path+`","type":"person","subject":"`+name+`","natural_key":"`+key+`"}`)
	if r.IsError {
		f.t.Fatalf("create %s: %s", path, r.Text)
	}
	docID, _ = out["document_id"].(string)
	rootID, _ = out["root_chunk_id"].(string)
	if docID == "" || rootID == "" {
		f.t.Fatalf("create %s returned %v", path, out)
	}
	return docID, rootID
}

// fact files a fact under its home subject and links it to every subject it is
// about — including its home, which is what the consolidation pass does.
func (f *subjectFixture) fact(docID, homeRoot, key, text string, alsoAbout []string) string {
	f.t.Helper()
	out, r := docExec(f.t, f.d, f.ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+homeRoot+`","natural_key":"`+key+`","title":"`+text+
		`","body":"`+text+`","type":"fact"}`)
	if r.IsError {
		f.t.Fatalf("fact %s: %s", key, r.Text)
	}
	id, _ := out["id"].(string)
	for _, to := range append([]string{homeRoot}, alsoAbout...) {
		if _, r := docExec(f.t, f.d, f.ctx, `{"op":"link_chunks","scope":"user","from_id":"`+id+
			`","to_id":"`+to+`","kind":"about"}`); r.IsError {
			f.t.Fatalf("about edge %s->%s: %s", id, to, r.Text)
		}
	}
	return id
}

// factIDs runs list_facts and returns the ids it reported.
func (f *subjectFixture) factIDs(input string) map[string]bool {
	f.t.Helper()
	out, r := docExec(f.t, f.d, f.ctx, input)
	if r.IsError {
		f.t.Fatalf("list_facts: %s", r.Text)
	}
	raw, _ := json.Marshal(out["facts"])
	var rows []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &rows)
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	return got
}

// TestListFacts_ARelationFactIsListedUnderBothItsSubjects is the contract itself.
func TestListFacts_ARelationFactIsListedUnderBothItsSubjects(t *testing.T) {
	f := newSubjectFixture(t)

	dave := f.factIDs(`{"op":"list_facts","scope":"user","about":"` + f.daveRoot + `","claims_only":true}`)
	if !dave[f.shared] {
		t.Error("the relation fact is missing from its HOME subject's listing")
	}
	if !dave[f.daveOnly] {
		t.Error("Dave's own fact is missing from his listing")
	}
	if dave[f.shopOnly] {
		t.Error("a fact about the shop alone was listed under Dave")
	}

	shop := f.factIDs(`{"op":"list_facts","scope":"user","about":"` + f.shopRoot + `","claims_only":true}`)
	if !shop[f.shared] {
		t.Error("THE CONTRACT: the relation fact is unreachable from the subject it references " +
			"but is not filed under — filing is single-parent, so a document filter loses it")
	}
	if !shop[f.shopOnly] {
		t.Error("the shop's own fact is missing from its listing")
	}
	if shop[f.daveOnly] {
		t.Error("a fact about Dave alone was listed under the shop")
	}
}

// TestListFacts_ASubjectsOwnFactsAreListedWithoutAnEdge. The `about` edge is
// written best-effort — "an unreachable fact is a retrieval gap, a missing fact is
// data loss, and only the second may fail a write" — and a `remember`-written fact
// carries its subject on the chunk and has no edge at all. Containment is what
// catches both, so the union is not redundancy.
func TestListFacts_ASubjectsOwnFactsAreListedWithoutAnEdge(t *testing.T) {
	f := newSubjectFixture(t)
	// A fact filed under Dave whose edge write never happened.
	out, r := docExec(t, f.d, f.ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+f.daveDoc+
		`","parent_id":"`+f.daveRoot+`","natural_key":"memory/operator/dave-likes-tea",`+
		`"title":"Dave likes tea.","body":"Dave likes tea.","type":"fact"}`)
	if r.IsError {
		t.Fatalf("edgeless fact: %s", r.Text)
	}
	edgeless, _ := out["id"].(string)

	if got := f.factIDs(`{"op":"list_facts","scope":"user","about":"` + f.daveRoot + `"}`); !got[edgeless] {
		t.Error("a fact filed under the subject with no `about` edge was dropped — the edge is " +
			"best-effort, so containment has to carry it")
	}
}

// TestGetDocument_ADossierReportsWhatItsTreeCannotReach. export_md must NOT carry
// these (it round-trips through import_md, and an edge whose far end is in another
// document would be dropped or re-created pointing at nothing), so the document read
// is where a reader learns the tree is not the whole answer.
func TestGetDocument_ADossierReportsWhatItsTreeCannotReach(t *testing.T) {
	f := newSubjectFixture(t)

	out, r := docExec(t, f.d, f.ctx, `{"op":"get_document","scope":"user","path":"/facts/shop"}`)
	if r.IsError {
		t.Fatalf("get_document: %s", r.Text)
	}
	raw, _ := json.Marshal(out["references"])
	var refs []subjectRef
	_ = json.Unmarshal(raw, &refs)
	if len(refs) != 1 {
		t.Fatalf("references = %v, want the one relation fact filed in Dave's document", refs)
	}
	if refs[0].ID != f.shared {
		t.Errorf("reference = %q, want the relation fact %q", refs[0].ID, f.shared)
	}
	if refs[0].DocumentID != f.daveDoc {
		t.Errorf("reference lives in %q, want Dave's document %q — a same-document fact is a "+
			"child and is already in the tree", refs[0].DocumentID, f.daveDoc)
	}
	if refs[0].Kind != "about" {
		t.Errorf("reference kind = %q, want about", refs[0].Kind)
	}

	// Dave's dossier has NO references block: both his facts are his own children,
	// so a block there would report the tree back to itself as though it were extra.
	out, r = docExec(t, f.d, f.ctx, `{"op":"get_document","scope":"user","path":"/facts/dave"}`)
	if r.IsError {
		t.Fatalf("get_document dave: %s", r.Text)
	}
	if _, present := out["references"]; present {
		t.Errorf("Dave's dossier carries a references block for facts already in its tree: %v", out["references"])
	}
}

// TestGetDocument_AnOrdinaryDocumentCarriesNoReferences. The probe is unconditional,
// so the guarantee that it costs an ordinary document nothing visible is a test.
func TestGetDocument_AnOrdinaryDocumentCarriesNoReferences(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","path":"/docs/notes"}`); r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"/docs/notes"}`)
	if r.IsError {
		t.Fatalf("get_document: %s", r.Text)
	}
	if _, present := out["references"]; present {
		t.Errorf("an ordinary document reported references: %v", out["references"])
	}
}

// TestListFacts_AboutAnUnknownChunkIsReportedNotAnsweredEmpty. "No facts about it"
// and "that subject does not exist" are different answers, and returning the first
// for the second hides a caller's mistake behind a plausible result.
func TestListFacts_AboutAnUnknownChunkIsReportedNotAnsweredEmpty(t *testing.T) {
	f := newSubjectFixture(t)
	_, r := docExec(t, f.d, f.ctx, `{"op":"list_facts","scope":"user","about":"no-such-chunk"}`)
	if !r.IsError {
		t.Fatal("list_facts about an unknown chunk succeeded — an empty list reads as 'nothing known'")
	}
}

// TestMarkdownRoundTrip_ASubjectDossier is the other half of the same decision.
//
// References are reported by get_document and NOT by export_md, and the reason is
// this: the Markdown round-trips. A subject's dossier carries an edge whose far end
// is a chunk in ANOTHER document — the relation fact pointing at the other subject —
// and on re-import those ids name nothing. Re-creating them would fabricate edges to
// chunks that are not there; emitting inbound ones too would make it worse.
//
// So the contract is: the chunk TREE comes back identical, the same-document edges
// come back, and the cross-document one is dropped rather than invented.
func TestMarkdownRoundTrip_ASubjectDossier(t *testing.T) {
	f := newSubjectFixture(t)

	md, r := docExec(t, f.d, f.ctx, `{"op":"export_md","scope":"user","id":"`+f.daveDoc+`"}`)
	if r.IsError {
		t.Fatalf("export: %s", r.Text)
	}
	exported := asStr(md["markdown"])
	// The trailer must actually contain the cross-document edge, or this test proves
	// nothing about how import handles one.
	if !strings.Contains(exported, f.shared+" -> "+f.shopRoot+" [about]") {
		t.Fatalf("the dossier's trailer does not carry the cross-document reference, so the "+
			"round-trip below is vacuous:\n%s", exported)
	}

	ij, _ := json.Marshal(exported)
	out, r2 := docExec(t, f.d, f.ctx, `{"op":"import_md","scope":"user","markdown":`+string(ij)+`}`)
	if r2.IsError {
		t.Fatalf("import: %s", r2.Text)
	}
	// Dave's root + his two facts.
	if n := asInt(out["chunks_created"]); n != 3 {
		t.Errorf("re-import created %d chunks, want 3 (the entity root and its two facts)", n)
	}
	newDoc, _ := out["document_id"].(string)

	// The same-document edges came back — so the trailer is being read, not ignored.
	edges, r3 := docExec(t, f.d, f.ctx, `{"op":"get_edges","scope":"user","document_id":"`+newDoc+`"}`)
	if r3.IsError {
		t.Fatalf("get_edges: %s", r3.Text)
	}
	raw, _ := json.Marshal(edges["edges"])
	var got []struct {
		FromID string `json:"from_id"`
		ToID   string `json:"to_id"`
		Kind   string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &got)
	if len(got) != 2 {
		t.Errorf("the imported dossier has %d edges, want 2 — its two facts' `about` edges to "+
			"their own root, and NOT the one whose far end is in another document: %v", len(got), got)
	}
	for _, e := range got {
		if e.ToID == f.shopRoot {
			t.Errorf("import re-created the cross-document reference %v — on import those ids "+
				"name nothing in the new document, so the edge would point outside it", e)
		}
	}
}
