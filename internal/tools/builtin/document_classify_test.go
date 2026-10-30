package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// classifyOntologyMD: NormativeAct declares the identifier and the identity field; Law
// and Code each declare their own title pattern and inherit the rest; Order matches by
// its own title pattern. One instance, and one concept to vote on.
const classifyOntologyMD = "# Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### NormativeAct\n@identifier_pattern (?P<number>\\d+-[IVX]+)\n- `number` text @identity\n\n" +
	"##### Law\n@title_pattern ^Закон України\n\n" +
	"##### Code\n@title_pattern ^Кодекс України\n\n" +
	"#### Order\n@title_pattern ^Наказ\n\n" +
	"## Instances\n@section instances\n\n### Verkhovna Rada\n@a organization\n@alt Верховна Рада\n\n" +
	"## Schemes\n@section schemes\n\n### topics\n\n#### Risk Management\n"

type classifyFixture struct {
	t   *testing.T
	d   *Document
	ctx context.Context // the operator plane, tenant and user scopes granted
}

func newClassifyFixture(t *testing.T, status string, withEmbedder bool) *classifyFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	if withEmbedder {
		d.Store = &ftsStore{newVectorStore(d.Store)}
		d.Embedder = newFakeEmbedder("fake", "m1", "risk", "note", "budget")
	}
	ctx := operatorCtx(installTenantOntology(t, d, base, classifyOntologyMD, status))
	return &classifyFixture{t: t, d: d, ctx: ctx}
}

func (f *classifyFixture) exec(ctx context.Context, v map[string]any) (map[string]any, string, bool) {
	f.t.Helper()
	b, _ := json.Marshal(v)
	out, r := docExec(f.t, f.d, ctx, string(b))
	return out, r.Text, r.IsError
}

func (f *classifyFixture) must(v map[string]any) map[string]any {
	f.t.Helper()
	out, text, isErr := f.exec(f.ctx, v)
	if isErr {
		f.t.Fatalf("%v: %s", v["op"], text)
	}
	return out
}

// doc creates a document with one part per body; typ "" leaves it untyped.
func (f *classifyFixture) doc(scope, title, path, typ string, parts ...[2]string) (docID, rootID string) {
	f.t.Helper()
	v := map[string]any{"op": "create_document", "scope": scope, "title": title, "path": path}
	if typ != "" {
		v["type"] = typ
	}
	out := f.must(v)
	docID, rootID = out["document_id"].(string), out["root_chunk_id"].(string)
	for _, p := range parts {
		f.must(map[string]any{"op": "create_chunk", "scope": scope, "document_id": docID, "title": p[0], "body": p[1]})
	}
	return docID, rootID
}

// tiedDoc makes a document whose title matches Law and whose import source title
// matches Code: two title-level matches of unrelated classes, a real tie. (A section
// title alone no longer ties with the title's class.)
func (f *classifyFixture) tiedDoc(scope, number, path string) string {
	f.t.Helper()
	docID, root := f.doc(scope, "Закон України № "+number, path, "")
	rev := int(f.root(scope, root)["revision"].(float64))
	f.must(map[string]any{"op": "update_chunk", "scope": scope, "id": root, "revision": rev,
		"fields": map[string]any{importFieldSourceTitle: "Кодекс України про працю"}})
	return docID
}

func (f *classifyFixture) classify(ctx context.Context, scope, docID string, extra map[string]any) classifyAnswer {
	f.t.Helper()
	v := map[string]any{"op": "classify", "scope": scope, "document_id": docID}
	for k, x := range extra {
		v[k] = x
	}
	b, _ := json.Marshal(v)
	res, err := f.d.Execute(ctx, b)
	if err != nil || res.IsError {
		f.t.Fatalf("classify: %v %s", err, res.Text)
	}
	var a classifyAnswer
	if err := json.Unmarshal([]byte(res.Text), &a); err != nil {
		f.t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return a
}

func (f *classifyFixture) root(scope, rootID string) map[string]any {
	f.t.Helper()
	return f.must(map[string]any{"op": "get_chunk", "scope": scope, "id": rootID})
}

func (f *classifyFixture) docType(scope, docID string) string {
	f.t.Helper()
	t, _ := f.must(map[string]any{"op": "get_document", "scope": scope, "id": docID})["type"].(string)
	return t
}

func pendingOf(t *testing.T, root map[string]any) classification {
	t.Helper()
	fields, _ := root["fields"].(map[string]any)
	b, _ := json.Marshal(fields[classifyField])
	var rec classification
	_ = json.Unmarshal(b, &rec)
	return rec
}

// TestClassify_AUniqueRuleMatchIsApplied: one class matching by its own title pattern is
// applied live — the type, the identity field its identifier pattern filled (so the work
// key), and issued_by to the adopted instance the text names.
func TestClassify_AUniqueRuleMatchIsApplied(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	if _, err := f.d.AdoptInstances(f.ctx, false); err != nil {
		t.Fatal(err)
	}
	docID, rootID := f.doc("tenant", "Закон України № 2297-VI про захист даних", "/kb/laws/2297",
		"", [2]string{"Преамбула", "Цей Закон прийняла Верховна Рада України."})

	a := f.classify(f.ctx, "tenant", docID, nil)
	if a.Result != "classified" || !a.Live || a.Class != "Law" || a.WorkKey != "law:2297-vi" {
		t.Fatalf("answer = %+v", a)
	}
	if got := f.docType("tenant", docID); got != "Law" {
		t.Errorf("documents.type = %q, want Law", got)
	}
	root := f.root("tenant", rootID)
	if root["identity_key"] != nil && root["identity_key"] != "law:2297-vi" {
		t.Errorf("identity_key = %v", root["identity_key"])
	}
	if fields, _ := root["fields"].(map[string]any); fields["number"] != "2297-VI" {
		t.Errorf("the identity field was not set: %v", fields)
	}
	if rec := pendingOf(t, root); rec.Applied != "Law" || rec.Proposed != "" {
		t.Errorf("record = %+v", rec)
	}
	if len(a.IssuedBy) != 1 || a.IssuedBy[0].Instance != "Verkhovna Rada" || a.IssuedBy[0].SubjectChunkID == "" {
		t.Fatalf("issued_by = %+v", a.IssuedBy)
	}
	var issued *edgeWriteResult
	for i := range a.Statements {
		issued = &a.Statements[i]
	}
	if issued == nil || issued.Status != edgeStatusAsserted {
		t.Errorf("issued_by statement = %+v", a.Statements)
	}

	// Classifying again is idempotent: same class, no second statement.
	again := f.classify(f.ctx, "tenant", docID, nil)
	if again.Result != "classified" || len(again.Statements) != 1 || again.Statements[0].Created {
		t.Errorf("a re-run = %+v", again)
	}
}

// TestClassify_ReportWritesNothing.
func TestClassify_ReportWritesNothing(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	docID, rootID := f.doc("user", "Закон України № 1-I", "/kb/a", "")
	before := f.root("user", rootID)
	a := f.classify(f.ctx, "user", docID, map[string]any{"write": "report"})
	if a.Class != "Law" || !a.Live || a.Write != "report" {
		t.Errorf("a report still says what propose would do: %+v", a)
	}
	after := f.root("user", rootID)
	if after["revision"] != before["revision"] || f.docType("user", docID) != "" {
		t.Errorf("report wrote: before %v after %v", before, after)
	}
}

// TestClassify_ADraftOntologyClassifiesNothing.
func TestClassify_ADraftOntologyClassifiesNothing(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyDraftStatus, false)
	docID, rootID := f.doc("user", "Закон України № 1-I", "/kb/a", "")
	a := f.classify(f.ctx, "user", docID, nil)
	if a.Result != "unclassified" || a.Class != "" || f.docType("user", docID) != "" || pendingOf(t, f.root("user", rootID)).By != "" {
		t.Errorf("a draft classified: %+v", a)
	}
}

// TestClassify_AHandSetTypeIsKept: classify never replaces a type it did not set.
func TestClassify_AHandSetTypeIsKept(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	docID, _ := f.doc("user", "Закон України № 1-I", "/kb/a", "Order")
	a := f.classify(f.ctx, "user", docID, nil)
	if a.Result != "kept" || a.Live || a.Class != "Law" || f.docType("user", docID) != "Order" {
		t.Errorf("a hand-set type was not kept: %+v, type %q", a, f.docType("user", docID))
	}
}

// TestClassify_SiblingsMatchingIsAmbiguous: two unrelated classes match at title level
// (the title and the import's source title), so nothing is applied; both are
// reported. (A class inheriting a sibling's pattern does not count: Law and Code each
// match only by the pattern they declare.)
func TestClassify_SiblingsMatchingIsAmbiguous(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	docID := f.tiedDoc("user", "1-I", "/kb/a")
	a := f.classify(f.ctx, "user", docID, nil)
	if a.Live || a.Class != "" || f.docType("user", docID) != "" {
		t.Errorf("an ambiguous match was applied: %+v", a)
	}
	if len(a.Steps) == 0 || a.Steps[0].Step != "rules" || len(a.Steps[0].Candidates) != 2 {
		t.Errorf("rule candidates = %+v", a.Steps)
	}
}

// TestClassify_AHeldIdentityIsProposedInstead: a second document claiming a work key
// another holds is not given the class; it is proposed, with the reason.
func TestClassify_AHeldIdentityIsProposedInstead(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	first, _ := f.doc("user", "Закон України № 7-I", "/kb/a", "")
	if a := f.classify(f.ctx, "user", first, nil); !a.Live {
		t.Fatalf("first = %+v", a)
	}
	second, rootID := f.doc("user", "Закон України № 7-I (копія)", "/kb/b", "")
	a := f.classify(f.ctx, "user", second, nil)
	if a.Live || a.Result != "proposed" || f.docType("user", second) != "" || !strings.Contains(strings.Join(a.Warnings, " "), "identity_collision") {
		t.Errorf("a held identity was applied: %+v", a)
	}
	if rec := pendingOf(t, f.root("user", rootID)); rec.Proposed != "Law" {
		t.Errorf("record = %+v", rec)
	}
	// Accepting it while the identity is still held leaves it pending, and says so.
	out := f.must(map[string]any{"op": "accept_classification", "scope": "user", "document_id": second})
	if out["type_pending"] != true || out["type"] != nil || f.docType("user", second) != "" {
		t.Errorf("accept with the identity held = %v", out)
	}
	if rec := pendingOf(t, f.root("user", rootID)); rec.Proposed != "Law" {
		t.Errorf("record after a refused accept = %+v", rec)
	}
}

// voteCorpus makes two Order documents about Risk Management in /kb/orders, one Law in
// another folder, and an untyped document in /kb/orders no pattern matches.
func voteCorpus(f *classifyFixture, scope string) (target, targetRoot string) {
	f.t.Helper()
	read, _ := f.d.tenantOntologyRead(f.ctx)
	var rm memrank.OntologyConcept
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			rm = c
		}
	}
	for i := range 2 {
		_, root := f.doc(scope, fmt.Sprintf("Наказ %d", i+1), fmt.Sprintf("/kb/orders/o%d", i), "Order", [2]string{"Пункт 1", "risk note"})
		if scope == "tenant" {
			f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": root, "to_id": rm.ChunkID, "kind": "subject"})
		} else {
			f.must(map[string]any{"op": "add_tags", "scope": scope, "id": root, "tags": []string{rm.Tag}})
		}
	}
	f.doc(scope, "Закон України № 9-I", "/kb/laws/9", "Law", [2]string{"Стаття 1", "risk note"})
	return f.doc(scope, "Розпорядження 5", "/kb/orders/new", "", [2]string{"Пункт 1", "risk note budget"})
}

// TestClassify_TheVoteProposesAndAnOperatorAccepts (tenant scope): no rule matches, the
// two Orders in the folder agree, so Order and their concept are proposed — nothing is
// applied — and accept_classification applies them, from the operator plane only.
func TestClassify_TheVoteProposesAndAnOperatorAccepts(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, true)
	target, root := voteCorpus(f, "tenant")
	a := f.classify(f.ctx, "tenant", target, nil)
	if a.Result != "proposed" || a.Live || a.Class != "Order" || f.docType("tenant", target) != "" {
		t.Fatalf("answer = %+v", a)
	}
	if vote := a.Steps[1]; vote.Chosen != "Order" || vote.Neighbours != 2 {
		t.Errorf("the vote counted outside the folder or missed a neighbour: %+v", vote)
	}
	if len(a.Concepts) != 1 || a.Concepts[0].Concept != "Risk Management" || a.Concepts[0].Kind != "subject" {
		t.Fatalf("concepts = %+v", a.Concepts)
	}
	if len(a.Statements) != 1 || a.Statements[0].Status != edgeStatusProposed {
		t.Fatalf("statements = %+v", a.Statements)
	}
	if rec := pendingOf(t, f.root("tenant", root)); rec.Proposed != "Order" || len(rec.Statements) != 1 {
		t.Errorf("record = %+v", rec)
	}

	_, text, isErr := f.exec(inRunCtx(f.ctx), map[string]any{"op": "accept_classification", "scope": "tenant", "document_id": target})
	if !isErr || !strings.Contains(text, "only an operator") {
		t.Errorf("accept_classification inside a run = %s", text)
	}
	out := f.must(map[string]any{"op": "accept_classification", "scope": "tenant", "document_id": target})
	if out["type"] != "Order" || len(out["accepted_statements"].([]any)) != 1 {
		t.Errorf("accept = %v", out)
	}
	if f.docType("tenant", target) != "Order" {
		t.Error("the accepted class is not the type")
	}
	edges := edgesOf(t, f.d, f.ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+target+`"}`)
	found := false
	for _, e := range edges {
		if e["from_id"] == root && e["kind"] == "subject" && e["status"] == edgeStatusAsserted {
			found = true
		}
	}
	if !found {
		t.Errorf("the subject statement was not accepted: %v", edges)
	}
	if rec := pendingOf(t, f.root("tenant", root)); rec.Applied != "Order" || rec.Proposed != "" || len(rec.Statements) != 0 {
		t.Errorf("record after accept = %+v", rec)
	}
	if _, text, isErr := f.exec(f.ctx, map[string]any{"op": "accept_classification", "scope": "tenant", "document_id": target}); !isErr || !strings.Contains(text, "nothing_pending") {
		t.Errorf("a second accept = %s", text)
	}
}

// TestClassify_OutsideTheTenantAConceptWaitsAsItsTag: in a user scope no statement can
// reach a concept, so the proposed concept is kept as its tag and becomes the root's
// tag on accept.
func TestClassify_OutsideTheTenantAConceptWaitsAsItsTag(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, true)
	target, root := voteCorpus(f, "user")
	a := f.classify(f.ctx, "user", target, nil)
	if a.Class != "Order" || len(a.Concepts) != 1 || len(a.Statements) != 0 {
		t.Fatalf("answer = %+v", a)
	}
	rec := pendingOf(t, f.root("user", root))
	if len(rec.ProposedTags) != 1 || rec.ProposedTags[0] != a.Concepts[0].Tag {
		t.Fatalf("record = %+v", rec)
	}
	f.must(map[string]any{"op": "accept_classification", "scope": "user", "document_id": target})
	tags, _ := f.root("user", root)["tags"].([]any)
	if len(tags) != 1 || tags[0] != a.Concepts[0].Tag || f.docType("user", target) != "Order" {
		t.Errorf("after accept: tags %v, type %q", tags, f.docType("user", target))
	}
}

// TestClassify_AnUnreadableNeighbourIsNotCounted: the vote only counts documents the
// caller may read, so a confined caller learns nothing about one it cannot see.
func TestClassify_AnUnreadableNeighbourIsNotCounted(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, true)
	target, _ := voteCorpus(f, "tenant")
	confined := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/orders/o0"}})
	})
	a := f.classify(confined, "tenant", target, map[string]any{"write": "report"})
	if vote := a.Steps[1]; vote.Neighbours != 1 || vote.Chosen != "" || len(a.Concepts) != 0 {
		t.Errorf("an unreadable neighbour was counted: %+v, concepts %+v", vote, a.Concepts)
	}
}

// TestClassify_ProposeNeedsWriteOnTheDocument: report reads; propose writes the root, so
// it asks write on the document.
func TestClassify_ProposeNeedsWriteOnTheDocument(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	docID, _ := f.doc("tenant", "Закон України № 3-I", "/kb/ro/3", "")
	readOnly := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/kb/ro/**"}})
	})
	if a := f.classify(readOnly, "tenant", docID, map[string]any{"write": "report"}); a.Class != "Law" {
		t.Errorf("report = %+v", a)
	}
	_, res := docExec(t, f.d, readOnly, `{"op":"classify","scope":"tenant","document_id":"`+docID+`"}`)
	refusedByACL(t, "classify write:propose without write", res)
	if f.docType("tenant", docID) != "" {
		t.Error("a refused classify wrote the type")
	}
}

// TestClassifyRules_OwnPatternsOnlyAndTheMostSpecificWins: a subclass that only inherits
// its parent's pattern is not a candidate (else siblings would always tie), and a class
// matching beside its own ancestor replaces it.
func TestClassifyRules_OwnPatternsOnlyAndTheMostSpecificWins(t *testing.T) {
	terms := memrank.ResolveInheritance([]memrank.OntologyTerm{
		{Name: "Document", IsDocumentClass: true},
		{Name: "Act", Parent: "Document", IsDocumentClass: true, TitlePattern: `^Акт`},
		{Name: "Law", Parent: "Act", IsDocumentClass: true},
		{Name: "Code", Parent: "Act", IsDocumentClass: true},
		{Name: "Decree", Parent: "Act", IsDocumentClass: true, TitlePattern: `^Акт про`},
	})
	classes := map[string]memrank.OntologyTerm{}
	for _, c := range terms {
		classes[strings.ToLower(c.Name)] = c
	}
	names := func(cs []classifyCandidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Class)
		}
		return out
	}
	if got := names(classifyRules(classes, classifyDoc{title: "Акт 12"})); len(got) != 1 || got[0] != "Act" {
		t.Errorf("inherited patterns counted: %v, want [Act]", got)
	}
	if got := names(classifyRules(classes, classifyDoc{title: "Акт про реформу"})); len(got) != 1 || got[0] != "Decree" {
		t.Errorf("the ancestor was kept beside its descendant: %v, want [Decree]", got)
	}
}

// TestAcceptClassification_AForgedRecordAcceptsNothing: the record lives in the root's
// fields, which any writer of the document may set. One that names another document's
// proposal, a rejected statement, a tag that is no concept and an undeclared class
// must not let an operator's accept act on any of them.
func TestAcceptClassification_AForgedRecordAcceptsNothing(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	_, otherRoot := f.doc("tenant", "Other", "/kb/other", "")
	_, a2 := f.doc("tenant", "Another", "/kb/another", "")
	proposed := f.must(map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": otherRoot, "to_id": a2, "kind": "cites"})["id"].(string)
	rejected := f.must(map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": a2, "to_id": otherRoot, "kind": "cites"})["id"].(string)
	f.must(map[string]any{"op": "reject_statement", "scope": "tenant", "id": rejected, "reason": "wrong"})

	docID, root := f.doc("tenant", "Target", "/kb/target", "")
	rev := int(f.root("tenant", root)["revision"].(float64))
	forged := map[string]any{classifyField: map[string]any{"by": edgeMethodClassify, "proposed_type": "Treaty",
		"statements": []string{proposed, rejected}, "proposed_tags": []string{"anything/goes"}}}
	f.must(map[string]any{"op": "update_chunk", "scope": "tenant", "id": root, "revision": rev, "fields": forged})

	out := f.must(map[string]any{"op": "accept_classification", "scope": "tenant", "document_id": docID})
	if acc, _ := out["accepted_statements"].([]any); len(acc) != 0 {
		t.Errorf("a forged record accepted statements: %v", out)
	}
	if out["type"] != nil || f.docType("tenant", docID) != "" || out["tags"] != nil {
		t.Errorf("a forged record applied a type or tags: %v", out)
	}
	for id, want := range map[string]string{proposed: edgeStatusProposed, rejected: edgeStatusRejected} {
		s, err := f.d.statementByID(f.ctx, mustTenantKey(t, f.d, f.ctx), id)
		if err != nil || s == nil || s.Status != want {
			t.Errorf("statement %s = %+v, want %s", id, s, want)
		}
	}
}

type stubClassifier struct {
	verdict memrank.ClassifyVerdict
	asked   []memrank.ClassifyRequest
}

func (s *stubClassifier) Classify(_ context.Context, req memrank.ClassifyRequest) memrank.ClassifyVerdict {
	s.asked = append(s.asked, req)
	return s.verdict
}

func modelStep(a classifyAnswer) *classifyStep {
	for i := range a.Steps {
		if a.Steps[i].Step == "model" {
			return &a.Steps[i]
		}
	}
	return nil
}

// TestClassify_TheModelSettlesWhatTheStepsLeaveOpen: no rule matched and nothing voted,
// so the model is asked about every document class but the root, and its answer — and
// the concept it names — is a PROPOSAL, never applied.
func TestClassify_TheModelSettlesWhatTheStepsLeaveOpen(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	stub := &stubClassifier{verdict: memrank.ClassifyVerdict{Method: memrank.ClassifyMethodChat, Class: "Order", Quote: "Розпорядження", Concepts: []string{"Risk Management"}}}
	f.d.Classifier = stub
	docID, root := f.doc("tenant", "Розпорядження 5", "/kb/x/5", "", [2]string{"Пункт 1", "управління ризиками"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if a.Result != "proposed" || a.Live || a.Class != "Order" || f.docType("tenant", docID) != "" {
		t.Fatalf("a model verdict was applied, or lost: %+v", a)
	}
	step := modelStep(a)
	if step == nil || step.Method != "chat" || step.Chosen != "Order" {
		t.Fatalf("model step = %+v", step)
	}
	if len(stub.asked) != 1 {
		t.Fatalf("asked %d times", len(stub.asked))
	}
	var offered []string
	for _, c := range stub.asked[0].Classes {
		offered = append(offered, c.Name)
	}
	if strings.Join(offered, ",") != "Code,Law,NormativeAct,Order" {
		t.Errorf("offered classes = %v (the root class must not be one)", offered)
	}
	if len(stub.asked[0].Concepts) != 1 || !strings.Contains(stub.asked[0].Text, "управління ризиками") {
		t.Errorf("request = %+v", stub.asked[0])
	}
	if len(a.Concepts) != 1 || len(a.Statements) != 1 || a.Statements[0].Status != edgeStatusProposed {
		t.Errorf("the model's concept was not proposed: concepts %+v, statements %+v", a.Concepts, a.Statements)
	}
	if rec := pendingOf(t, f.root("tenant", root)); rec.Proposed != "Order" {
		t.Errorf("record = %+v", rec)
	}
}

// TestClassify_TheModelIsAskedOnlyWhereTheStepsLeaveItOpen: a unique rule match and a
// hand-set type ask nothing; an ambiguous match offers the model only the tied classes,
// and the chosen one keeps the identity fields its pattern filled.
func TestClassify_TheModelIsAskedOnlyWhereTheStepsLeaveItOpen(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	stub := &stubClassifier{verdict: memrank.ClassifyVerdict{Method: memrank.ClassifyMethodDecision, Class: "Code", Probability: 0.9}}
	f.d.Classifier = stub
	unique, _ := f.doc("user", "Закон України № 11-I", "/kb/u", "")
	if a := f.classify(f.ctx, "user", unique, nil); !a.Live || a.Class != "Law" || (modelStep(a) != nil && modelStep(a).Chosen != "") {
		t.Errorf("a unique rule match asked the model for a class: %+v", a)
	}
	handSet, _ := f.doc("user", "Розпорядження 9", "/kb/h", "Order")
	if a := f.classify(f.ctx, "user", handSet, nil); a.Result != "kept" || (modelStep(a) != nil && modelStep(a).Chosen != "") {
		t.Errorf("a hand-set type asked the model for a class: %+v", a)
	}
	// Both were asked for their concepts only (the class is decided), never for a class.
	for _, q := range stub.asked {
		if !q.ConceptsOnly {
			t.Fatalf("a decided class was asked about: %+v", q)
		}
	}
	stub.asked = nil
	tied := f.tiedDoc("user", "12-I", "/kb/t")
	a := f.classify(f.ctx, "user", tied, nil)
	if len(stub.asked) != 1 || len(stub.asked[0].Classes) != 2 {
		t.Fatalf("an ambiguous match offered %+v", stub.asked)
	}
	if a.Class != "Code" || a.Live || a.Result != "proposed" || a.Fields["number"] != "12-I" || a.WorkKey != "code:12-i" {
		t.Errorf("answer = %+v", a)
	}
}

// TestClassify_WithoutAClassifierTheModelStepSaysSo.
func TestClassify_WithoutAClassifierTheModelStepSaysSo(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	docID, _ := f.doc("user", "Розпорядження 5", "/kb/x", "")
	a := f.classify(f.ctx, "user", docID, nil)
	if step := modelStep(a); step == nil || !strings.Contains(step.Reason, "no classifier") || a.Result != "unclassified" {
		t.Errorf("answer = %+v", a)
	}
}

// TestResolveAnchor_AWorkKeyMatchesItsIdentityWhateverItsCase: a citation names a law as
// the text writes it ("2297-VI"); the class rules store its identity lower-cased.
func TestResolveAnchor_AWorkKeyMatchesItsIdentityWhateverItsCase(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	docID, _ := f.doc("tenant", "Закон України № 2297-VI", "/kb/laws/2297", "")
	if a := f.classify(f.ctx, "tenant", docID, nil); a.WorkKey != "law:2297-vi" {
		t.Fatalf("work key = %q", a.WorkKey)
	}
	out := f.must(map[string]any{"op": "resolve_anchor", "scope": "tenant", "work_key": "law:2297-VI"})
	if out["found"] != true || out["document_id"] != docID {
		t.Errorf("resolve_anchor law:2297-VI = %v", out)
	}
}

// TestClassifyRules_ATitleMatchOutranksASection: a section headed by another kind's
// name makes no tie with the title's class; with a title that says nothing, the
// sections still speak.
func TestClassifyRules_ATitleMatchOutranksASection(t *testing.T) {
	terms := memrank.ResolveInheritance([]memrank.OntologyTerm{
		{Name: "Document", IsDocumentClass: true},
		{Name: "Regulation", Parent: "Document", IsDocumentClass: true, TitlePattern: `^Полож`},
		{Name: "Procedure", Parent: "Document", IsDocumentClass: true, TitlePattern: `^Порядок`},
	})
	classes := map[string]memrank.OntologyTerm{}
	for _, c := range terms {
		classes[strings.ToLower(c.Name)] = c
	}
	got := classifyRules(classes, classifyDoc{title: "Полож_Значн_правочини", sectionTitles: []string{"Порядок погодження"}})
	if len(got) != 1 || got[0].Class != "Regulation" {
		t.Errorf("title and section = %#v, want only Regulation", got)
	}
	got = classifyRules(classes, classifyDoc{title: "B001385OM000091", sectionTitles: []string{"Порядок погодження"}})
	if len(got) != 1 || got[0].Class != "Procedure" || got[0].From != "section" {
		t.Errorf("section only = %#v, want Procedure from the section", got)
	}
}

// TestClassify_AClassDecidedByRuleStillAsksForConcepts: the rules settle the class,
// but nothing says what the document is about until the model is asked for the
// concepts alone; they are proposals, and the class stays applied.
func TestClassify_AClassDecidedByRuleStillAsksForConcepts(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	stub := &stubClassifier{verdict: memrank.ClassifyVerdict{Method: memrank.ClassifyMethodChat, Concepts: []string{"Risk Management"}, Quote: "ризиками"}}
	f.d.Classifier = stub
	docID, _ := f.doc("tenant", "Закон України № 5-I", "/kb/c/5", "", [2]string{"Стаття 1", "управління ризиками"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if !a.Live || a.Class != "Law" {
		t.Fatalf("the rule's class was not applied: %+v", a)
	}
	if len(stub.asked) != 1 || !stub.asked[0].ConceptsOnly || len(stub.asked[0].Concepts) != 1 {
		t.Fatalf("asked = %+v", stub.asked)
	}
	if len(a.Concepts) != 1 || len(a.Statements) != 1 || a.Statements[0].Status != edgeStatusProposed {
		t.Errorf("concepts %+v, statements %+v", a.Concepts, a.Statements)
	}
	if step := modelStep(a); step == nil || step.Chosen != "" {
		t.Errorf("model step = %+v", step)
	}
}
