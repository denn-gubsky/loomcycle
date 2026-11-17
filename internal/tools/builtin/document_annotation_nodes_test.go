package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Node annotations (RFC GM §4, D33; plan P4.2), against statsCorpus:
//
//	topics/risk-management             a1 a2 a3 b1   written
//	topics/risk-management/outsourcing a1 a2 a3      written
//	topics/reporting                   r1 r2 r3      written
//	file-plan/compliance(/reporting)   —             declared
//	rank/law                           law1          thin

// nodeInputs reads a node's annotation inputs as ctx.
func nodeInputs(t *testing.T, d *Document, ctx context.Context, level, id string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"annotation_inputs","scope":"tenant","level":%q,"id":%q}`, level, id))
	if r.IsError {
		t.Fatalf("annotation_inputs %s %s: %s", level, id, r.Text)
	}
	return out
}

// nodeWrite writes a node's annotation from its current inputs: the composed text
// for a declared node, else brief.
func nodeWrite(t *testing.T, d *Document, ctx context.Context, level, id, brief string) map[string]any {
	t.Helper()
	in := nodeInputs(t, d, ctx, level, id)
	abstract := "Abstract of " + brief + "."
	if c, ok := in["composed"].(map[string]any); ok {
		brief, abstract = c["brief"].(string), c["abstract"].(string)
	}
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":%q,"id":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q}`,
		level, id, brief, abstract, in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation %s %s: %s", level, id, r.Text)
	}
	return out["annotation"].(map[string]any)
}

func nodeRecords(t *testing.T, d *Document, ctx context.Context, level, id string) []any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_annotation","scope":"tenant","level":%q,"id":%q}`, level, id))
	if r.IsError {
		t.Fatalf("get_annotation %s %s: %s", level, id, r.Text)
	}
	return out["annotations"].([]any)
}

func schemeChunk(t *testing.T, d *Document, ctx context.Context, name string) string {
	t.Helper()
	read, _ := d.tenantOntologyRead(ctx)
	for _, s := range read.Schemes {
		if s.Name == name {
			return s.ChunkID
		}
	}
	t.Fatalf("no scheme %s", name)
	return ""
}

// TestNodeAnnotationInputs_ThreeCases — basis follows the member count; a declared
// node carries the composed text and nothing computed from members.
func TestNodeAnnotationInputs_ThreeCases(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	op := k.f.operator
	for _, tc := range []struct {
		id, basis string
		members   int
	}{{k.rm, "written", 4}, {k.law, "thin", 1}, {k.f.concepts["file-plan/compliance"], "declared", 0}} {
		in := nodeInputs(t, d, op, "concept", tc.id)
		if in["basis"] != tc.basis || in["members"] != float64(tc.members) {
			t.Errorf("%s: basis %v members %v, want %s %d", tc.id, in["basis"], in["members"], tc.basis, tc.members)
		}
		_, composed := in["composed"]
		if composed != (tc.basis == "declared") {
			t.Errorf("%s: composed present = %v, basis %s", tc.id, composed, tc.basis)
		}
	}
	thin := nodeInputs(t, d, op, "concept", k.law)
	if ex := anyList(thin["exemplars"]); thin["exemplars_from"] != "members" || len(ex) != 1 || ex[0].(map[string]any)["brief"] != "statute text" {
		t.Fatalf("thin request carries %v from %v, want law1's own brief", thin["exemplars"], thin["exemplars_from"])
	}
	declared := nodeInputs(t, d, op, "concept", k.f.concepts["file-plan/compliance"])
	c := declared["composed"].(map[string]any)
	if !strings.Contains(c["abstract"].(string), "Nothing is held under it yet") || declared["exemplars"] != nil {
		t.Fatalf("declared = %v", declared)
	}
	scheme := nodeInputs(t, d, op, "scheme", schemeChunk(t, d, op, "topics"))
	if scheme["members"] != float64(7) || len(anyList(scheme["narrower"])) != 2 {
		t.Fatalf("scheme topics: members %v narrower %v, want 7 and its two top-level concepts", scheme["members"], scheme["narrower"])
	}
}

// TestNodeAnnotation_FirstMemberReplacesDeclared — a declared record goes stale when
// its node gets a member, and the rewrite is thin.
func TestNodeAnnotation_FirstMemberReplacesDeclared(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	op := k.f.operator
	comp := k.f.concepts["file-plan/compliance"]
	if rec := nodeWrite(t, d, op, "concept", comp, ""); rec["basis"] != "declared" {
		t.Fatalf("record = %v, want basis declared", rec)
	}
	if in := nodeInputs(t, d, op, "concept", comp); in["stale"] != false {
		t.Fatalf("a fresh declared record reads stale: %v", in["stale"])
	}
	k.f.link("link_chunks", k.f.chunk("tenant", k.r1), comp, "subject")
	in := nodeInputs(t, d, op, "concept", comp)
	if in["stale"] != true || in["basis"] != "thin" {
		t.Fatalf("after the first member: stale %v basis %v, want stale and thin", in["stale"], in["basis"])
	}
	if rec := nodeWrite(t, d, op, "concept", comp, "Compliance filings"); rec["basis"] != "thin" {
		t.Fatalf("rewrite = %v, want basis thin", rec)
	}
}

// TestNodeAnnotation_ReaderLackingAMemberGetsNone — a written record is served only
// to a caller who may read every member; a declared one to anyone who may read the
// ontology.
func TestNodeAnnotation_ReaderLackingAMemberGetsNone(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		k := newStatsCorpus(t, d, base)
		op := k.f.operator
		comp := k.f.concepts["file-plan/compliance"]
		nodeWrite(t, d, op, "concept", k.rm, "Risk management")
		nodeWrite(t, d, op, "concept", k.rep, "Reporting")
		nodeWrite(t, d, op, "concept", comp, "")
		deny := aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
			Resources: []string{"document:tenant:/kb/b1"}})
		restricted := tenantDocCtx(base, deny)
		if got := nodeRecords(t, d, restricted, "concept", k.rm); len(got) != 0 {
			t.Fatalf("a reader who may not read b1 got Risk Management's record: %v", got)
		}
		if got := nodeRecords(t, d, restricted, "concept", k.rep); len(got) != 1 {
			t.Fatalf("Reporting (every member readable) = %v, want its record", got)
		}
		if got := nodeRecords(t, d, restricted, "concept", comp); len(got) != 1 {
			t.Fatalf("declared compliance = %v, want its record", got)
		}
		if got := nodeRecords(t, d, k.f.reader, "concept", k.rm); len(got) != 1 {
			t.Fatalf("an unrestricted reader got %v, want Risk Management's record", got)
		}
		list, _ := docExec(t, d, restricted, `{"op":"list_annotations","scope":"tenant","level":"concept"}`)
		if n := len(anyList(list["annotations"])); n != 2 {
			t.Fatalf("restricted list_annotations level concept = %d records, want 2 (reporting, compliance)", n)
		}
		sum, _ := docExec(t, d, restricted, `{"op":"list_annotations","scope":"tenant","summarize":true}`)
		if c, _ := sum["counts"].(map[string]any)["concept"].(map[string]any); c["total"] != float64(2) {
			t.Fatalf("restricted summarize = %v, want 2 concept records", sum["counts"])
		}
		// A restricted reader may not ask for a node's inputs: they are computed from
		// every member.
		driver := tools.WithRunID(tools.WithRunPolicy(base, tools.RunPolicy{
			Tools:            []string{"*"},
			Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
			SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
			StatementMethods: []string{annotationGrant},
			ACL:              deny,
		}), "run-restricted-annotator")
		if _, r := docExec(t, d, driver, fmt.Sprintf(`{"op":"annotation_inputs","scope":"tenant","level":"concept","id":%q}`, k.rm)); !r.IsError {
			t.Fatal("annotation_inputs answered a caller whose rules hide a member")
		}
	})
}

// TestNodeAnnotation_NonExemplarEditMarksNothingStale — a member that is not an
// exemplar can be rewritten without making the node stale; an exemplar's rewrite does.
func TestNodeAnnotation_NonExemplarEditMarksNothingStale(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	op := k.f.operator
	nodeStatsRun(t, d, op, computeAll)
	in := nodeInputs(t, d, op, "concept", k.rm)
	if in["exemplars_from"] != "statistics" {
		t.Fatalf("exemplars from %v, want statistics", in["exemplars_from"])
	}
	nodeWrite(t, d, op, "concept", k.rm, "Risk management")
	exemplar := map[string]bool{}
	for _, e := range anyList(in["exemplars"]) {
		exemplar[e.(map[string]any)["document_id"].(string)] = true
	}
	var other, ex string
	for _, id := range []string{k.a1, k.a2, k.a3, k.b1} {
		if exemplar[id] {
			ex = id
		} else {
			other = id
		}
	}
	if other == "" || ex == "" {
		t.Fatalf("exemplars %v leave no member outside them", exemplar)
	}
	k.annotate(t, other, "rewritten brief of a member", k.vecs[other])
	if in := nodeInputs(t, d, op, "concept", k.rm); in["stale"] != false {
		t.Fatalf("a non-exemplar's rewrite marked the node stale")
	}
	k.annotate(t, ex, "rewritten brief of an exemplar", k.vecs[ex])
	if in := nodeInputs(t, d, op, "concept", k.rm); in["stale"] != true {
		t.Fatalf("an exemplar's rewrite did not mark the node stale")
	}
}

// TestNodeAnnotation_AgentStillCannotEditTheNodeChunk — the annotate grant writes a
// node's annotation; the node's chunk stays the operator's.
func TestNodeAnnotation_AgentStillCannotEditTheNodeChunk(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	driver := annDriver(base)
	if rec := nodeWrite(t, d, driver, "concept", k.rep, "Reporting"); rec["brief"] != "Reporting" {
		t.Fatalf("driver's write = %v", rec)
	}
	g, _ := docExec(t, d, k.f.operator, `{"op":"get_chunk","scope":"tenant","id":"`+k.rep+`"}`)
	_, r := docExec(t, d, driver, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"rewritten","revision":%v}`, k.rep, g["revision"]))
	if !r.IsError || !strings.Contains(strings.ToLower(r.Text), "ontology") {
		t.Fatalf("driver's update_chunk on the concept = %v %s, want an ontology refusal", r.IsError, r.Text)
	}
}

// TestNodeAnnotationInputs_StatusCountsAreNotHashed — the request counts the members'
// stored statuses, and a status change marks nothing stale (the member's own
// annotation carries it).
func TestNodeAnnotationInputs_StatusCountsAreNotHashed(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	op := k.f.operator
	setStatus := func(doc, status string) {
		g, _ := docExec(t, d, op, `{"op":"get_chunk","scope":"tenant","id":"`+k.docs[doc]+`"}`)
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%v}`, k.docs[doc], status, g["revision"])); r.IsError {
			t.Fatal(r.Text)
		}
	}
	setStatus(k.r1, "done")
	setStatus(k.r2, "done")
	before := nodeInputs(t, d, op, "concept", k.rep)
	if got := fmt.Sprint(before["member_statuses"]); got != "[map[count:2 name:done] map[count:1 name:none]]" {
		t.Fatalf("member_statuses = %s", got)
	}
	setStatus(k.r3, "draft")
	after := nodeInputs(t, d, op, "concept", k.rep)
	if after["inputs_sha256"] != before["inputs_sha256"] {
		t.Fatal("a member's status change moved the node's inputs hash")
	}
}

// TestNodeAnnotation_ClassIsAddressedByNameAndAStandardClassIsRefused — a declared
// document class is annotated on its chunk; a seed class has no chunk.
func TestNodeAnnotation_ClassIsAddressedByNameAndAStandardClassIsRefused(t *testing.T) {
	d, ctx := annOntologyFixture(t, annOntologyMD)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Widget guide","path":"/kb/w","type":"QuickGuide"}`); r.IsError {
		t.Fatal(r.Text)
	}
	in, r := docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant","level":"class","name":"Guide"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if in["members"] != float64(1) || in["basis"] != "thin" || fmt.Sprint(in["place"]) != "[Document]" {
		t.Fatalf("Guide inputs = %v", in)
	}
	nodeWrite(t, d, ctx, "class", in["chunk_id"].(string), "How-to guides")

	d2, base2, _ := documentFixture(t)
	op2 := annOperator(installTenantOntology(t, d2, base2, conceptOntologyMD, memrank.OntologyConfirmedStatus))
	_, r = docExec(t, d2, op2, `{"op":"annotation_inputs","scope":"tenant","level":"class","name":"Document"}`)
	if !r.IsError || !strings.Contains(r.Text, "standard_class") {
		t.Fatalf("seed Document = %s, want standard_class", r.Text)
	}
}

// TestSearch_DefaultLevelsReturnNoNodeAnnotations — a node annotation is never a
// search hit by default: the leg's default levels are unchanged and node records get
// no index row in this phase.
func TestSearch_DefaultLevelsReturnNoNodeAnnotations(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	op := annOperator(f.operator)
	rm := f.concepts["topics/risk-management"]
	doc := f.doc("tenant", "/kb/risk")
	f.link("link_chunks", f.chunk("tenant", doc), rm, "subject")
	// A document record, indexed as always, so the leg has something to return.
	annWrite(t, d, op, `"document_id":"`+doc+`"`, "Risk note on outsourcing")
	nodeWrite(t, d, op, "concept", rm, "Risk management outsourcing vendor")
	nodeWrite(t, d, op, "scheme", schemeChunk(t, d, op, "topics"), "Topics of risk and reporting")
	if got := nodeRecords(t, d, op, "concept", rm); len(got) != 1 {
		t.Fatalf("the node record is not stored: %v", got)
	}
	read, _ := d.tenantOntologyRead(op)
	for _, mode := range []string{"include", "only"} {
		out, r := docExec(t, d, op, `{"op":"search","scope":"tenant","query":"risk note","annotations":"`+mode+`","limit":50}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		found := false
		for _, h := range anyList(out["chunks"]) {
			m := h.(map[string]any)
			ma, ok := m["matched_annotation"].(map[string]any)
			if !ok {
				continue // a passage hit; the ontology's own chunks are text like any other
			}
			if m["document_id"] == read.DocumentID || isNodeAnnotationLevel(fmt.Sprint(ma["level"])) {
				t.Fatalf("annotations %s ranked a node annotation: %v", mode, m)
			}
			found = true
		}
		if !found {
			t.Fatalf("annotations %s ranked no annotation at all, so it checks nothing: %v", mode, out)
		}
	}
}

// TestNodeAnnotation_OtherLevelsReadAsBefore — a section or document record carries
// no basis field: existing responses are unchanged.
func TestNodeAnnotation_OtherLevelsReadAsBefore(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	out, _ := docExec(t, d, op, `{"op":"create_document","scope":"tenant","title":"Plain","path":"/kb/plain"}`)
	rec := annWrite(t, d, op, `"document_id":"`+out["document_id"].(string)+`"`, "A plain document")
	if _, has := rec["basis"]; has {
		t.Fatalf("a document record carries basis: %v", rec)
	}
}

// TestNodeAnnotation_LimitedReaderLearnsNothingFromDeclaredStaleness — a reader with
// a read limit polls a declared record; a document it may not read is then filed
// under the node. What the reader sees (the record, the stale filter, summarize's
// stale count) must not change: the staleness would tell it the hidden document
// exists (RFC GM D8 (i)).
func TestNodeAnnotation_LimitedReaderLearnsNothingFromDeclaredStaleness(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	op := k.f.operator
	comp := k.f.concepts["file-plan/compliance"]
	nodeWrite(t, d, op, "concept", comp, "")
	restricted := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/b1"}}))
	poll := func() string {
		get, r := docExec(t, d, restricted, fmt.Sprintf(`{"op":"get_annotation","scope":"tenant","level":"concept","id":%q}`, comp))
		if r.IsError {
			t.Fatal(r.Text)
		}
		staleList, _ := docExec(t, d, restricted, `{"op":"list_annotations","scope":"tenant","level":"concept","stale":true}`)
		sum, _ := docExec(t, d, restricted, `{"op":"list_annotations","scope":"tenant","summarize":true}`)
		return fmt.Sprint(get["annotations"], " | ", staleList["annotations"], " | ", sum["counts"])
	}
	before := poll()
	if !strings.Contains(before, "declared") {
		t.Fatalf("the restricted reader does not see the declared record: %s", before)
	}
	k.f.link("link_chunks", k.f.chunk("tenant", k.b1), comp, "subject")
	if in := nodeInputs(t, d, op, "concept", comp); in["stale"] != true {
		t.Fatalf("the operator does not see the record stale after b1 was filed: %v", in["stale"])
	}
	if after := poll(); after != before {
		t.Fatalf("the restricted reader's view changed when a document it may not read was filed under the node:\nbefore %s\nafter  %s", before, after)
	}
}
