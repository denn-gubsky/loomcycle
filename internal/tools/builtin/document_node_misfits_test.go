package builtin

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestMisfit_ProposingNeedsTheRuleGrant — filing stamps rule:centroid, so only a
// caller who could stamp a rule: method itself may file: an operator, or a run
// granted rule. A run node_stats admits on an extractor grant alone still reads
// the misfits, and is refused propose with nothing filed.
func TestMisfit_ProposingNeedsTheRuleGrant(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	x1, _ := k.withStrays(t)
	run := func(sm ...string) context.Context {
		return tools.WithRunID(tools.WithRunPolicy(base, tools.RunPolicy{
			Tools:            []string{"*"},
			Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
			SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
			StatementMethods: sm,
		}), "run-misfits")
	}
	extractor := run("extractor")
	if rep := misfitsRun(t, d, extractor, `{"op":"node_stats","mode":"misfits"}`); rep.Count == 0 {
		t.Fatalf("an extractor-granted run reads no misfit: %+v", rep)
	}
	_, res := docExec(t, d, extractor, `{"op":"node_stats","mode":"misfits","propose":true}`)
	if !res.IsError || !strings.Contains(res.Text, "method_not_granted") {
		t.Errorf("an extractor-granted run filed: %s", res.Text)
	}
	if n := len(centroidProposals(t, d, k.f.operator)); n != 0 {
		t.Fatalf("the refused call filed %d proposals", n)
	}
	ruled := misfitsRun(t, d, run("rule"), `{"op":"node_stats","mode":"misfits","propose":true,"limit":1}`)
	if ruled.Proposed != 1 || ruled.of(t, x1).Status != edgeStatusProposed {
		t.Errorf("a rule-granted run = %+v; want x1 filed", ruled)
	}
}

// subjectAutoAssertMD is conceptOntologyMD with subject declared @auto_assert and
// no Review section: a rule-made subject proposal with chunk evidence would be
// written asserted by the legacy rule.
var subjectAutoAssertMD = strings.Replace(conceptOntologyMD,
	"## Relations\n@section relations\n\n", "## Relations\n@section relations\n\n### subject\n@auto_assert\n\n", 1)

// TestMisfit_FiledPlacementStaysProposedWithoutAReviewRule — the pass never makes a
// placement live itself: with subject @auto_assert and no Review section (where a
// rule: proposal with title evidence is otherwise written asserted), what it files
// is proposed.
func TestMisfit_FiledPlacementStaysProposedWithoutAReviewRule(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpusMD(t, d, base, subjectAutoAssertMD)
	if pf, ok := d.reviewPolicyInForce(k.f.operator); !ok || pf.section || !d.autoAssertKind(k.f.operator, "subject") {
		t.Fatalf("precondition: subject @auto_assert with no Review section: %+v %v", pf, ok)
	}
	x1, _ := k.withStrays(t)
	ans := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
	if m := ans.of(t, x1); m.StatementID == "" || m.Status != edgeStatusProposed {
		t.Errorf("x1 = %+v; want filed proposed", m)
	}
	for _, s := range centroidProposals(t, d, k.f.operator) {
		if s.Status != edgeStatusProposed {
			t.Errorf("statement %s filed %s", s.ID, s.Status)
		}
	}
}

// misfitDoc adds a Document under concept with an annotation vector vec, as
// newStatsCorpus's documents are made, and returns its id.
func (k *statsCorpus) misfitDoc(t *testing.T, path, title string, vec []float32, concept string) string {
	t.Helper()
	out := k.f.c.exec(map[string]any{"op": "create_document", "scope": "tenant", "title": title, "path": path, "type": "Document"})
	doc := out["document_id"].(string)
	k.docs[doc] = out["root_chunk_id"].(string)
	k.f.link("link_chunks", k.f.chunk("tenant", doc), concept, "subject")
	k.annotate(t, doc, title, vec)
	return doc
}

// withStrays adds to reporting two documents whose vectors point where Risk
// Management's members do: both are misfits of reporting, nearer Risk Management.
// x1 is the farther from reporting, so it sorts first.
func (k *statsCorpus) withStrays(t *testing.T) (x1, x2 string) {
	t.Helper()
	x1 = k.misfitDoc(t, "/kb/x1", "Vendor exit plan", []float32{1, 0.15, 0, 0}, k.rep)
	x2 = k.misfitDoc(t, "/kb/x2", "Vendor oversight memo", []float32{0.9, 0.2, 0.2, 0}, k.rep)
	return x1, x2
}

func misfitsRun(t *testing.T, d *Document, ctx context.Context, body string) NodeMisfitsReport {
	t.Helper()
	_, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out NodeMisfitsReport
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func (a NodeMisfitsReport) of(t *testing.T, doc string) nodeMisfit {
	t.Helper()
	for _, m := range a.Misfits {
		if m.DocumentID == doc {
			return m
		}
	}
	t.Fatalf("no misfit for %s in %+v", doc, a.Misfits)
	return nodeMisfit{}
}

// centroidProposals is every statement the misfit pass filed, by from_id → to_id.
func centroidProposals(t *testing.T, d *Document, ctx context.Context) map[string]statementRow {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT id FROM chunk_edges WHERE method = ?`, misfitMethod)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]statementRow{}
	for _, r := range res.Rows {
		s, err := d.statementByID(ctx, key, asStr(r[0]))
		if err != nil || s == nil {
			t.Fatalf("statement %v: %v", r[0], err)
		}
		out[s.FromID+"->"+s.ToID] = *s
	}
	return out
}

// TestMisfit_ReportByDefaultWritesNothing — mode misfits lists a member nearer a
// sibling's centroid than its own (with the margin and both similarities) and,
// without propose, files nothing and stores nothing.
func TestMisfit_ReportByDefaultWritesNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	x1, x2 := k.withStrays(t)
	ans := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits"}`)
	if !ans.Vectors || ans.Propose || ans.Margin != nodestats.MisfitMargin || ans.Proposed != 0 || ans.Skipped != nil {
		t.Errorf("answer header = %+v", ans)
	}
	m := ans.of(t, x1)
	if m.NodeID != k.rep || m.NearestNodeID != k.rm || m.Nearest != "Risk Management" || m.Title != "Vendor exit plan" {
		t.Errorf("x1 = %+v; want a misfit of reporting nearer Risk Management", m)
	}
	if math.Abs(m.Margin-(m.NearestSimilarity-m.OwnSimilarity)) > 1e-12 || m.Margin <= nodestats.MisfitMargin {
		t.Errorf("x1 margin %v, similarities %v / %v", m.Margin, m.OwnSimilarity, m.NearestSimilarity)
	}
	if m.StatementID != "" || m.Skipped != "" {
		t.Errorf("a report filed or skipped a proposal: %+v", m)
	}
	ans.of(t, x2)
	if !sort.SliceIsSorted(ans.Misfits, func(i, j int) bool { return ans.Misfits[i].Margin > ans.Misfits[j].Margin }) {
		t.Errorf("misfits not largest margin first: %+v", ans.Misfits)
	}
	for _, m := range ans.Misfits {
		if m.DocumentID == k.a1 || m.DocumentID == k.b1 || m.DocumentID == k.r1 {
			t.Errorf("a member at home is reported: %+v", m)
		}
	}
	if got := centroidProposals(t, d, k.f.operator); len(got) != 0 {
		t.Errorf("a report filed %d proposals", len(got))
	}
	if rows := storedRows(t, d, k.f.operator); len(rows) != 0 {
		t.Errorf("a misfit report stored %d node_stats rows", len(rows))
	}
}

// TestMisfit_ProposalsAreCappedPerPass — with propose the pass files at most limit
// proposals, largest margin first, each a subject proposal from the document's root
// to the nearer concept with method rule:centroid and the margin as confidence; the
// rest are reported as skipped for the cap, and a second pass files them without
// filing the first again.
func TestMisfit_ProposalsAreCappedPerPass(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	x1, x2 := k.withStrays(t)
	first := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true,"limit":1}`)
	if first.Cap != 1 || first.Proposed != 1 || first.Skipped["cap"] != 1 {
		t.Fatalf("first pass = proposed %d skipped %v cap %d; want 1, {cap:1}, 1", first.Proposed, first.Skipped, first.Cap)
	}
	m1 := first.of(t, x1)
	if m1.StatementID == "" || m1.Status != edgeStatusProposed {
		t.Fatalf("x1 = %+v; want a proposal filed", m1)
	}
	if s := first.of(t, x2); s.Skipped != "cap" || s.StatementID != "" {
		t.Errorf("x2 = %+v; want skipped for the cap", s)
	}
	got := centroidProposals(t, d, k.f.operator)
	s, ok := got[k.docs[x1]+"->"+k.rm]
	if len(got) != 1 || !ok {
		t.Fatalf("filed = %v; want only x1's root -> Risk Management", got)
	}
	if s.Kind != "subject" || s.Status != edgeStatusProposed || s.Method != misfitMethod {
		t.Errorf("statement = kind %s status %s method %s", s.Kind, s.Status, s.Method)
	}
	if s.Confidence == nil || math.Abs(*s.Confidence-m1.Margin) > 1e-9 {
		t.Errorf("confidence = %v, want the margin %v", s.Confidence, m1.Margin)
	}

	second := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
	if second.Cap != misfitProposalCap || second.Proposed != 1 || second.Skipped["exists"] != 1 {
		t.Errorf("second pass = proposed %d skipped %v cap %d; want x2 filed, x1 exists", second.Proposed, second.Skipped, second.Cap)
	}
	if n := len(centroidProposals(t, d, k.f.operator)); n != 2 {
		t.Errorf("after two passes %d proposals, want 2", n)
	}
	// limit is bounded.
	if a := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true,"limit":100000}`); a.Cap != misfitProposalMax {
		t.Errorf("cap for a huge limit = %d, want %d", a.Cap, misfitProposalMax)
	}
}

// TestMisfit_RejectedPlacementIsNotProposedAgain — a placement a reviewer rejected
// for the document is never proposed again by the pass: not after a person's
// rejection, and not after a judge's, which propose_statement alone would let
// through (a judge's rejection is a verdict, not a tombstone).
func TestMisfit_RejectedPlacementIsNotProposedAgain(t *testing.T) {
	for _, by := range []string{"person", "judge"} {
		t.Run(by, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			k := newStatsCorpus(t, d, base)
			x1, _ := k.withStrays(t)
			first := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
			id := first.of(t, x1).StatementID
			if id == "" {
				t.Fatalf("precondition: x1 proposed: %+v", first.of(t, x1))
			}
			rejectAs(t, d, k, by, id)
			again := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
			if m := again.of(t, x1); m.Skipped != "rejected" || m.StatementID != "" {
				t.Errorf("x1 after a %s's rejection = %+v; want skipped rejected", by, m)
			}
			for key, s := range centroidProposals(t, d, k.f.operator) {
				if key == k.docs[x1]+"->"+k.rm && s.ID != id {
					t.Errorf("a second statement %s re-files the rejected placement", s.ID)
				}
			}
		})
	}
}

// rejectAs rejects statement id as a person (reject_statement) or as a judge (the
// ruling a judge_statements run writes: status rejected, ruled_by judge:<run>).
func rejectAs(t *testing.T, d *Document, k *statsCorpus, by, id string) {
	t.Helper()
	if by == "person" {
		k.f.c.exec(map[string]any{"op": "reject_statement", "scope": "tenant", "id": id, "reason": "not about risk"})
		return
	}
	key, _, err := d.ontologyTenantKey(k.f.operator)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.withSqlTxn(k.f.operator, key, func(txnID string) error {
		return d.execTxn(k.f.operator, txnID, `UPDATE chunk_edges SET status = ?, ruled_by = ?, judge_reason = ? WHERE id = ?`,
			edgeStatusRejected, "judge:run-judge", "not about risk", id)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMisfit_UnusableVectorIsSkippedNotFatal — a stored vector the comparison
// cannot use (all zero, or of another dimension) leaves that member out and is
// counted; the rest of the answer, and its proposals, still come.
func TestMisfit_UnusableVectorIsSkippedNotFatal(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	x1, _ := k.withStrays(t)
	k.setVector(t, k.b1, []float32{0, 0, 0, 0}, "m1")
	k.setVector(t, k.r1, []float32{0, 0, 1}, "m1")
	ans := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
	if ans.MembersSkipped != 2 || len(ans.Issues) != 0 {
		t.Errorf("members_skipped %d issues %v; want 2 and none", ans.MembersSkipped, ans.Issues)
	}
	if m := ans.of(t, x1); m.NearestNodeID != k.rm || m.StatementID == "" {
		t.Errorf("x1 = %+v; want still a misfit, filed", m)
	}
	for _, m := range ans.Misfits {
		if m.DocumentID == k.b1 || m.DocumentID == k.r1 {
			t.Errorf("a skipped member is listed: %+v", m)
		}
	}
}

// TestMisfit_StoreWithoutVectorsReportsItCannotRun — on a store that keeps no
// vectors the mode says why and lists nothing; with propose it files nothing.
func TestMisfit_StoreWithoutVectorsReportsItCannotRun(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	k.withStrays(t)
	d.Store = k.f.c.vs.Store // the plain sqlite store: SupportsVectors is false
	ans := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
	if ans.Vectors || !strings.Contains(ans.Note, "keeps no vectors") || len(ans.Misfits) != 0 || ans.Proposed != 0 {
		t.Errorf("answer = %+v; want no vectors, why, and nothing listed", ans)
	}
}

// TestMisfit_InputAndAccessAreChecked — propose is refused outside mode misfits,
// an unknown mode names misfits, and a plain run may not run the pass.
func TestMisfit_InputAndAccessAreChecked(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	for body, want := range map[string]string{
		`{"op":"node_stats","propose":true}`:                   "propose is taken only with mode misfits",
		`{"op":"node_stats","mode":"compute","propose":true}`:  "propose is taken only with mode misfits",
		`{"op":"node_stats","mode":"nope"}`:                    "is not report, compute, misfits, growth or split",
		`{"op":"node_stats","mode":"misfits","scheme":"nope"}`: `no scheme "nope" is in force`,
	} {
		_, res := docExec(t, d, k.f.operator, body)
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %s; want %q", body, res.Text, want)
		}
	}
	if _, res := docExec(t, d, k.f.reader, `{"op":"node_stats","mode":"misfits"}`); !res.IsError || !strings.Contains(res.Text, "permission") {
		t.Errorf("a plain reader ran misfits: %s", res.Text)
	}
}

// TestNodeStats_AnswerIsUnchangedWithoutMisfitOptions — plan §2, byte-identical when
// unused: a report and a compute carry exactly the fields they carried before mode
// misfits existed, and no misfit field.
func TestNodeStats_AnswerIsUnchangedWithoutMisfitOptions(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	k.withStrays(t)
	want := "computed,embed_model,fresh,min_members,missing,mode,nodes,removed,series,stale,vectors"
	for _, body := range []string{`{"op":"node_stats"}`, computeAll} {
		_, res := docExec(t, d, k.f.operator, body)
		if res.IsError {
			t.Fatalf("%s: %s", body, res.Text)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(res.Text), &top); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(top))
		for key := range top {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != want {
			t.Errorf("%s: fields %s, want %s", body, got, want)
		}
	}
	if n := len(centroidProposals(t, d, k.f.operator)); n != 0 {
		t.Errorf("report and compute filed %d proposals", n)
	}
}

// TestOntologySchemes_MisfitProposalsOnBothTiers — the misfit pass on both SQL
// Memory tiers (the postgres half runs where LOOMCYCLE_TEST_SQLMEM_PG_DSN is set;
// the CI filter takes this prefix): the cap, the stamped method, and a rejected
// placement never filed again.
func TestOntologySchemes_MisfitProposalsOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		x1, x2 := k.withStrays(t)
		first := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true,"limit":1}`)
		if first.Proposed != 1 || first.of(t, x2).Skipped != "cap" {
			t.Fatalf("first pass = %+v", first)
		}
		id := first.of(t, x1).StatementID
		rejectAs(t, d, k, "judge", id)
		second := misfitsRun(t, d, k.f.operator, `{"op":"node_stats","mode":"misfits","propose":true}`)
		if second.of(t, x1).Skipped != "rejected" || second.of(t, x2).StatementID == "" {
			t.Errorf("second pass = %+v; want x1 rejected, x2 filed", second.Misfits)
		}
		got := centroidProposals(t, d, k.f.operator)
		if len(got) != 2 {
			t.Errorf("filed %d statements, want 2: %v", len(got), got)
		}
		for _, s := range got {
			if s.Method != misfitMethod || s.Kind != "subject" {
				t.Errorf("statement %s: method %s kind %s", s.ID, s.Method, s.Kind)
			}
		}
	})
}
