package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// assignOntologyMD: one scheme, topics, with Alpha (Alpha One, Alpha Two) and Beta
// at the top level, so level 1 compares Alpha with Beta and level 2 compares Alpha's
// two children within Alpha.
const assignOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\n\n" +
	"## Relations\n@section relations\n\n### filed_under\n@domain Provision\n@range concept\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n\n#### Alpha\n\n##### Alpha One\n\n##### Alpha Two\n\n#### Beta\n"

// assignCorpus: three members each of Alpha One (along x), Alpha Two (along y) and
// Beta (along z), with Alpha holding the six of its children. Unplaced and
// placed-at-Alpha documents are added by each test.
type assignCorpus struct {
	*statsCorpus
	alpha, one, two, beta string
	ones, twos, betas     []string
}

func newAssignCorpus(t *testing.T, d *Document, base context.Context, md string) *assignCorpus {
	t.Helper()
	f := newConceptFixtureMD(t, d, base, md)
	k := &statsCorpus{f: f, d: d, vecs: map[string][]float32{}, briefs: map[string]string{}, docs: map[string]string{}}
	a := &assignCorpus{statsCorpus: k, alpha: f.concepts["topics/alpha"], one: f.concepts["topics/alpha/alpha-one"],
		two: f.concepts["topics/alpha/alpha-two"], beta: f.concepts["topics/beta"]}
	if a.alpha == "" || a.one == "" || a.two == "" || a.beta == "" {
		t.Fatalf("concepts = %v", f.concepts)
	}
	for i, v := range [][]float32{{1, 0, 0, 0}, {0.95, 0.05, 0, 0}, {0.9, 0, 0.05, 0}} {
		a.ones = append(a.ones, a.doc(t, fmt.Sprintf("/kb/one/%d", i), v, a.one))
	}
	for i, v := range [][]float32{{0, 1, 0, 0}, {0.05, 0.95, 0, 0}, {0, 0.9, 0, 0.05}} {
		a.twos = append(a.twos, a.doc(t, fmt.Sprintf("/kb/two/%d", i), v, a.two))
	}
	for i, v := range [][]float32{{0, 0, 1, 0}, {0, 0.05, 0.95, 0}, {0, 0, 0.9, 0.05}} {
		a.betas = append(a.betas, a.doc(t, fmt.Sprintf("/kb/beta/%d", i), v, a.beta))
	}
	return a
}

// doc adds a Document with annotation vector vec, placed live under concepts.
func (a *assignCorpus) doc(t *testing.T, path string, vec []float32, concepts ...string) string {
	t.Helper()
	out := a.f.c.exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "Doc " + path, "path": path, "type": "Document"})
	doc := out["document_id"].(string)
	a.docs[doc] = out["root_chunk_id"].(string)
	for _, c := range concepts {
		a.f.link("link_chunks", a.f.chunk("tenant", doc), c, "subject")
	}
	a.annotate(t, doc, "brief of "+path, vec)
	return doc
}

func assignRun(t *testing.T, d *Document, ctx context.Context, body string) NodeAssignReport {
	t.Helper()
	_, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out NodeAssignReport
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func (r NodeAssignReport) docOf(t *testing.T, doc string) assignDoc {
	t.Helper()
	for _, d := range r.Page {
		if d.DocumentID == doc {
			return d
		}
	}
	t.Fatalf("no page entry for %s", doc)
	return assignDoc{}
}

func (d assignDoc) at(t *testing.T, level int) assignRanking {
	t.Helper()
	for _, r := range d.Rankings {
		if r.Level == level {
			return r
		}
	}
	t.Fatalf("%s has no level-%d ranking: %+v", d.DocumentID, level, d.Rankings)
	return assignRanking{}
}

// assignedStatements is every rule:assign statement filed, by from → to.
func assignedStatements(t *testing.T, d *Document, ctx context.Context) map[string]statementRow {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT id FROM chunk_edges WHERE method = ?`, assignMethod)
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

// TestAssign_ReportRanksEachLevelAndWritesNothing — the report form compares a
// placed document at level 1 (agreeing with its own placement, measured without
// itself) and within its parent at level 2; an unplaced document is compared at
// level 1 only (level 1 has no threshold, so the walk does not descend); nothing is
// written.
func TestAssign_ReportRanksEachLevelAndWritesNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	p1 := a.doc(t, "/kb/p1", []float32{1, 0.02, 0, 0}, a.alpha)
	u1 := a.doc(t, "/kb/u1", []float32{1, 0.05, 0, 0})

	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200}`)
	if !rep.Vectors || rep.Documents != 11 || rep.WithoutVector != 0 || rep.Proposals != nil {
		t.Fatalf("report = vectors %v documents %d without %d proposals %v", rep.Vectors, rep.Documents, rep.WithoutVector, rep.Proposals)
	}
	if !reflect.DeepEqual(rep.Margins, map[string]float64{"2": 0.03}) {
		t.Errorf("margins = %v; want level 2 at 0.03 only", rep.Margins)
	}
	pd := rep.docOf(t, p1)
	if len(pd.Placements) != 1 || pd.Placements[0].NodeID != a.alpha {
		t.Errorf("p1 placements = %+v", pd.Placements)
	}
	l1 := pd.at(t, 1)
	if l1.Nearest.NodeID != a.alpha || l1.Second.NodeID != a.beta || l1.Agrees == nil || !*l1.Agrees || l1.Threshold != nil || l1.Eligible {
		t.Errorf("p1 level 1 = %+v; want Alpha nearest, agreeing, no threshold", l1)
	}
	l2 := pd.at(t, 2)
	if l2.Parent != "topics/alpha" || l2.Nearest.NodeID != a.one || l2.Agrees != nil || l2.Threshold == nil || !l2.Eligible || l2.Margin < 0.03 {
		t.Errorf("p1 level 2 = %+v; want Alpha One nearest within Alpha, nothing to agree with, eligible", l2)
	}
	ud := rep.docOf(t, u1)
	if len(ud.Rankings) != 1 || ud.Rankings[0].Level != 1 || ud.Rankings[0].Agrees != nil || len(ud.Placements) != 0 {
		t.Errorf("u1 = %+v; want one unjudged level-1 ranking", ud)
	}
	// A member of Alpha One agrees at level 2 too, measured without itself.
	if r := rep.docOf(t, a.ones[0]).at(t, 2); r.Nearest.NodeID != a.one || r.Agrees == nil || !*r.Agrees || r.Eligible {
		t.Errorf("a member of Alpha One at level 2 = %+v", r)
	}
	if n := len(assignedStatements(t, d, a.f.operator)); n != 0 {
		t.Fatalf("the report filed %d statements", n)
	}
}

// TestAssign_NoReviewRuleMeansEveryPlacementIsAProposal — with propose and no
// review rule (subject even declared @auto_assert, which asserts other rule: proposals
// with chunk evidence), every rule:assign placement filed is proposed, with the
// margin as its confidence.
func TestAssign_NoReviewRuleMeansEveryPlacementIsAProposal(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(assignOntologyMD, "## Relations\n@section relations\n\n", "## Relations\n@section relations\n\n### subject\n@auto_assert\n\n", 1)
	a := newAssignCorpus(t, d, base, md)
	p1 := a.doc(t, "/kb/p1", []float32{1, 0.02, 0, 0}, a.alpha)
	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`)
	if rep.Proposed != 1 || len(rep.Proposals) != 1 {
		t.Fatalf("propose = %+v; want p1's level-2 placement alone", rep)
	}
	pr := rep.Proposals[0]
	if pr.DocumentID != p1 || pr.Nearest.NodeID != a.one || pr.Status != edgeStatusProposed {
		t.Fatalf("proposal = %+v", pr)
	}
	got := assignedStatements(t, d, a.f.operator)
	s, ok := got[a.docs[p1]+"->"+a.one]
	if !ok || s.Status != edgeStatusProposed || s.Method != assignMethod || s.Kind != "subject" || s.Confidence == nil || math.Abs(*s.Confidence-pr.Margin) > 1e-6 {
		t.Errorf("filed = %+v; want a proposed rule:assign subject statement with the margin as confidence", got)
	}
}

// TestAssign_RuleOnMethodAndConfidencePutsItLive — the mechanism D17 reserves for
// the tenant: a Review rule accepting subject statements of method rule:assign
// above a confidence makes such a placement live through the existing review
// policy, at filing; one below the bound stays a proposal. Nothing in the pass
// adds the rule.
func TestAssign_RuleOnMethodAndConfidencePutsItLive(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := assignOntologyMD + "\n## Review\n@section review\n- accept kind:subject method:rule:assign when confidence>=0.3\n- human *\n"
	a := newAssignCorpus(t, d, base, md)
	far := a.doc(t, "/kb/far", []float32{1, 0.02, 0, 0}, a.alpha)  // a large margin within Alpha
	near := a.doc(t, "/kb/near", []float32{1, 0.8, 0, 0}, a.alpha) // a small one
	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true,"margins":{"2":0}}`)
	status := map[string]string{}
	for _, pr := range rep.Proposals {
		status[pr.DocumentID] = pr.Status
		if pr.DocumentID == far && pr.Margin < 0.3 || pr.DocumentID == near && pr.Margin >= 0.3 {
			t.Fatalf("fixture: %s margin %v", pr.DocumentID, pr.Margin)
		}
	}
	if status[far] != edgeStatusAsserted || status[near] != edgeStatusProposed {
		t.Errorf("statuses = %v; want far asserted by the rule, near proposed", status)
	}
}

// TestAssign_MarginThresholdIsRespected — a level-2 ranking under the threshold is
// reported, not eligible, and not filed; a lower threshold makes it eligible; a
// negative margin turns level 2 off; a level-1 threshold makes an unplaced document
// eligible at level 1 and walks it down to level 2.
func TestAssign_MarginThresholdIsRespected(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	near := a.doc(t, "/kb/near", []float32{1, 0.8, 0, 0}, a.alpha)
	u1 := a.doc(t, "/kb/u1", []float32{1, 0.05, 0, 0})
	m := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200}`).docOf(t, near).at(t, 2).Margin
	if m <= 0 || m >= 0.3 {
		t.Fatalf("fixture: near's level-2 margin %v", m)
	}
	high := assignRun(t, d, a.f.operator, fmt.Sprintf(`{"op":"node_stats","mode":"assign","limit":200,"margins":{"2":%v}}`, m+0.001))
	if r := high.docOf(t, near).at(t, 2); r.Eligible || *r.Threshold != m+0.001 {
		t.Errorf("above the margin: %+v; want not eligible", r)
	}
	if filed := assignRun(t, d, a.f.operator, fmt.Sprintf(`{"op":"node_stats","mode":"assign","propose":true,"margins":{"2":%v}}`, m+0.001)); filed.Proposed != 0 {
		t.Errorf("filed %d under a threshold above the margin", filed.Proposed)
	}
	low := assignRun(t, d, a.f.operator, fmt.Sprintf(`{"op":"node_stats","mode":"assign","limit":200,"margins":{"2":%v}}`, m-0.001))
	if r := low.docOf(t, near).at(t, 2); !r.Eligible {
		t.Errorf("below the margin: %+v; want eligible", r)
	}
	off := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200,"margins":{"2":-1}}`)
	if r := off.docOf(t, near).at(t, 2); r.Threshold != nil || r.Eligible || len(off.Margins) != 0 {
		t.Errorf("level 2 off: %+v margins %v", r, off.Margins)
	}
	l1 := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200,"margins":{"1":0.05}}`)
	ud := l1.docOf(t, u1)
	if r := ud.at(t, 1); !r.Eligible || r.Nearest.NodeID != a.alpha {
		t.Errorf("u1 level 1 with a threshold = %+v", r)
	}
	if r := ud.at(t, 2); r.Nearest.NodeID != a.one || !r.Eligible {
		t.Errorf("u1 walked to level 2 = %+v; want Alpha One eligible", r)
	}
}

// TestAssign_RejectedPlacementIsNeverFiledAgain, and the cap holds per call.
func TestAssign_RejectedPlacementIsNeverFiledAgainAndTheCapHolds(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	p1 := a.doc(t, "/kb/p1", []float32{1, 0.02, 0, 0}, a.alpha)
	p2 := a.doc(t, "/kb/p2", []float32{0.02, 1, 0, 0}, a.alpha)
	first := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true,"limit":1}`)
	if first.Proposed != 1 || first.Skipped["cap"] != 1 || first.Cap != 1 {
		t.Fatalf("first = %+v; want one filed, one capped", first)
	}
	var filedDoc, filedID string
	for _, pr := range first.Proposals {
		if pr.StatementID != "" {
			filedDoc, filedID = pr.DocumentID, pr.StatementID
		}
	}
	rejectAs(t, d, a.statsCorpus, "judge", filedID)
	second := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`)
	for _, pr := range second.Proposals {
		switch pr.DocumentID {
		case filedDoc:
			if pr.Skipped != "rejected" {
				t.Errorf("the rejected placement: %+v", pr)
			}
		case p1, p2:
			if pr.StatementID == "" {
				t.Errorf("the capped placement was not filed: %+v", pr)
			}
		}
	}
	third := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`)
	if third.Proposed != 0 || third.Skipped["rejected"] != 1 || third.Skipped["exists"] != 1 {
		t.Errorf("third = %+v; want nothing filed: one rejected, one already proposed", third.Skipped)
	}
}

// TestAssign_AgreementBucketsCountEveryRanking — the table counts each ranking once
// in the bucket of its margin and in every at_least row up to it, recounted here from
// the page (which holds every document).
func TestAssign_AgreementBucketsCountEveryRanking(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	a.doc(t, "/kb/p1", []float32{1, 0.02, 0, 0}, a.alpha)
	a.doc(t, "/kb/p2", []float32{1, 0.8, 0, 0}, a.one) // placed where it barely leads
	a.doc(t, "/kb/u1", []float32{0.7, 0, 0.7, 0})
	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200}`)
	type row struct{ rankings, placed, agree int }
	want := map[int][assignBucketCount]row{}
	wantAt := map[int][assignBucketCount]row{}
	for _, doc := range rep.Page {
		for _, r := range doc.Rankings {
			i := min(int(math.Floor(r.Margin/assignBucketWidth)), assignBucketCount-1)
			b, at := want[r.Level], wantAt[r.Level]
			inc := func(x *row) {
				x.rankings++
				if r.Agrees != nil {
					x.placed++
					if *r.Agrees {
						x.agree++
					}
				}
			}
			inc(&b[i])
			for j := 0; j <= i; j++ {
				inc(&at[j])
			}
			want[r.Level], wantAt[r.Level] = b, at
		}
	}
	if len(rep.Levels) != 2 {
		t.Fatalf("levels = %+v", rep.Levels)
	}
	for _, lv := range rep.Levels {
		total := 0
		for i, b := range lv.Buckets {
			total += b.Rankings
			if got := (row{b.Rankings, b.Placed, b.Agree}); got != want[lv.Level][i] {
				t.Errorf("level %d bucket %d = %+v; want %+v", lv.Level, i, got, want[lv.Level][i])
			}
			at := lv.AtLeast[i]
			if got := (row{at.Rankings, at.Placed, at.Agree}); got != wantAt[lv.Level][i] {
				t.Errorf("level %d at_least %d = %+v; want %+v", lv.Level, i, got, wantAt[lv.Level][i])
			}
			if b.Placed > 0 && (b.Agreement == nil || math.Abs(*b.Agreement-float64(b.Agree)/float64(b.Placed)) > 1e-9) {
				t.Errorf("level %d bucket %d agreement = %v", lv.Level, i, b.Agreement)
			}
		}
		if total != lv.Rankings || lv.AtLeast[0].Rankings != lv.Rankings {
			t.Errorf("level %d: buckets sum %d, at_least[0] %d, rankings %d", lv.Level, total, lv.AtLeast[0].Rankings, lv.Rankings)
		}
	}
}

// TestAssignLevel_BucketsByMargin — the bucket edges: 0.03 computed a hair short
// still falls in [0.03, 0.04); a margin past the last edge falls in the open bucket.
func TestAssignLevel_BucketsByMargin(t *testing.T) {
	lv := newAssignLevel(2, assignDefaultMargins)
	yes, no := true, false
	for _, r := range []assignRanking{
		{Margin: 0}, {Margin: 0.005, Agrees: &yes}, {Margin: 0.03 - 1e-12, Agrees: &no}, {Margin: 0.03, Agrees: &yes}, {Margin: 0.15},
	} {
		lv.count(r)
	}
	got := []int{}
	for _, b := range lv.Buckets {
		got = append(got, b.Rankings)
	}
	if want := []int{2, 0, 0, 2, 0, 0, 0, 0, 0, 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("buckets = %v; want %v", got, want)
	}
	if b := lv.AtLeast[3]; b.Rankings != 3 || b.Placed != 2 || b.Agree != 1 || *b.Agreement != 0.5 {
		t.Errorf("at_least 0.03 = %+v", b)
	}
	if lv.Buckets[9].To != nil || *lv.Buckets[0].To != 0.01 {
		t.Errorf("edges: %+v", lv.Buckets)
	}
}

// TestAssign_PagesAndHeldOutBound — the page follows next_cursor to the end, and
// created_after keeps only the documents created after the bound.
func TestAssign_PagesAndHeldOutBound(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	seen := map[string]bool{}
	after := ""
	for pages := 0; ; pages++ {
		body := `{"op":"node_stats","mode":"assign","limit":4`
		if after != "" {
			body += `,"after":"` + after + `"`
		}
		rep := assignRun(t, d, a.f.operator, body+`}`)
		for _, doc := range rep.Page {
			if seen[doc.DocumentID] {
				t.Fatalf("%s listed twice", doc.DocumentID)
			}
			seen[doc.DocumentID] = true
		}
		if rep.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		after = rep.NextCursor
	}
	if len(seen) != 9 {
		t.Errorf("paged %d documents, want 9", len(seen))
	}
	key, _, _ := d.ontologyTenantKey(a.f.operator)
	res, err := d.query(a.f.operator, key, `SELECT max(created_at) FROM documents`)
	if err != nil {
		t.Fatal(err)
	}
	bound := asInt64Val(res.Rows[0][0])
	late := a.doc(t, "/kb/late", []float32{1, 0.02, 0, 0}, a.alpha)
	held := assignRun(t, d, a.f.operator, fmt.Sprintf(`{"op":"node_stats","mode":"assign","created_after":%d}`, bound))
	if held.Documents != 1 || len(held.Page) != 1 || held.Page[0].DocumentID != late || held.CreatedAfter == nil {
		t.Errorf("held-out = %+v; want the late document alone", held)
	}
}

// TestAssign_StoreWithoutVectorsReportsItCannotRun
func TestAssign_StoreWithoutVectorsReportsItCannotRun(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	d.Store = a.f.c.vs.Store
	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`)
	if rep.Vectors || !strings.Contains(rep.Note, "keeps no vectors") || rep.Documents != 0 || rep.Proposed != 0 {
		t.Errorf("answer = %+v; want no comparison and why", rep)
	}
}

// TestAssign_AccessAndInputAreChecked — a restricted document (one some member of
// the tenant may not read, C8) is never compared nor listed; a caller whose rules
// hide part of the scope is refused, as every node_stats mode; filing needs the rule
// grant; margins and propose are refused where they do not belong.
func TestAssign_AccessAndInputAreChecked(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	secret := a.doc(t, "/kb/secret/p", []float32{1, 0.02, 0, 0}, a.alpha)
	restrictForSome(t, d, "/kb/secret/p")
	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200}`)
	for _, doc := range rep.Page {
		if doc.DocumentID == secret {
			t.Errorf("a restricted document was listed: %+v", doc)
		}
	}
	if rep.Documents != 9 {
		t.Errorf("documents = %d; want the 9 unrestricted", rep.Documents)
	}
	if _, res := docExec(t, d, denying(t, a.f.operator, "/kb/one/0"), `{"op":"node_stats","mode":"assign"}`); !res.IsError || !strings.Contains(res.Text, "hide some") {
		t.Errorf("a limited reader: %s", res.Text)
	}
	extractor := tools.WithRunID(tools.WithRunPolicy(base, tools.RunPolicy{
		Tools:            []string{"*"},
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		StatementMethods: []string{"extractor"},
	}), "run-assign")
	if rep := assignRun(t, d, extractor, `{"op":"node_stats","mode":"assign"}`); rep.Documents == 0 {
		t.Errorf("an extractor-granted run reads no report: %+v", rep)
	}
	if _, res := docExec(t, d, extractor, `{"op":"node_stats","mode":"assign","propose":true}`); !res.IsError || !strings.Contains(res.Text, "method_not_granted") {
		t.Errorf("an extractor-granted run filed: %s", res.Text)
	}
	for body, want := range map[string]string{
		`{"op":"node_stats","mode":"misfits","margins":{"2":0.1}}`: "margins is taken only with mode assign",
		`{"op":"node_stats","mode":"assign","margins":{"0":0.1}}`:  "not a level",
		`{"op":"node_stats","mode":"assign","margins":{"2":1.5}}`:  "at most 1",
		`{"op":"node_stats","mode":"growth","propose":true}`:       "propose is taken only with mode misfits, split or assign",
	} {
		if _, res := docExec(t, d, a.f.operator, body); !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %s; want %q", body, res.Text, want)
		}
	}
}

// TestNodeStats_AnswersAreUnchangedWithoutAssign — the other modes answer exactly as
// before when assign's options are not used: report, misfits and growth carry no new
// field (compute gains its definitions block only where the vector stage runs, see
// TestNodeDefinitions_*).
func TestNodeStats_AnswersAreUnchangedWithoutAssign(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	for _, body := range []string{`{"op":"node_stats"}`, `{"op":"node_stats","mode":"misfits"}`, `{"op":"node_stats","mode":"growth"}`} {
		_, res := docExec(t, d, a.f.operator, body)
		if res.IsError {
			t.Fatalf("%s: %s", body, res.Text)
		}
		for _, field := range []string{`"definitions"`, `"margins"`, `"levels"`, `"page"`} {
			if strings.Contains(res.Text, field) {
				t.Errorf("%s answers with %s: %s", body, field, res.Text)
			}
		}
	}
}

// TestOntologySchemes_AssignProposalsOnBothTiers — the propose form on both SQL
// Memory tiers (postgres where LOOMCYCLE_TEST_SQLMEM_PG_DSN is set): filed proposed,
// stamped rule:assign, a rejected placement never filed again.
func TestOntologySchemes_AssignProposalsOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		a := newAssignCorpus(t, d, ctx, assignOntologyMD)
		p1 := a.doc(t, "/kb/p1", []float32{1, 0.02, 0, 0}, a.alpha)
		first := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`)
		if first.Proposed != 1 || first.Proposals[0].DocumentID != p1 || first.Proposals[0].Status != edgeStatusProposed {
			t.Fatalf("first = %+v", first.Proposals)
		}
		rejectAs(t, d, a.statsCorpus, "judge", first.Proposals[0].StatementID)
		if again := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`); again.Proposed != 0 || again.Skipped["rejected"] != 1 {
			t.Errorf("again = %+v", again.Skipped)
		}
		for _, s := range assignedStatements(t, d, a.f.operator) {
			if s.Method != assignMethod || s.Kind != "subject" {
				t.Errorf("statement %s: method %s kind %s", s.ID, s.Method, s.Kind)
			}
		}
	})
}

// TestAssign_PlacedUnderAnUncomparedSiblingIsNeverFiled — a document placed live
// under a concept too small for a centroid (Alpha Three, one member) is placed at
// that level: the ranker compares only Alpha One and Two, but neither is filed for
// it, however clear its margin (conditional-qa 2026-11-27: 11 such filings, all
// wrong). An unplaced document in the same spot is still filed.
func TestAssign_PlacedUnderAnUncomparedSiblingIsNeverFiled(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(assignOntologyMD, "##### Alpha Two\n", "##### Alpha Two\n\n##### Alpha Three\n", 1)
	a := newAssignCorpus(t, d, base, md)
	three := a.f.concepts["topics/alpha/alpha-three"]
	if three == "" {
		t.Fatalf("concepts = %v", a.f.concepts)
	}
	small := a.doc(t, "/kb/small", []float32{1, 0.02, 0, 0}, three)
	loose := a.doc(t, "/kb/loose", []float32{1, 0.03, 0, 0}, a.alpha)
	body := `{"op":"node_stats","mode":"assign","limit":200,"margins":{"2":0.01}}`
	rep := assignRun(t, d, a.f.operator, body)
	if r := rep.docOf(t, small).at(t, 2); r.Compared != 2 || r.Nearest.NodeID != a.one || r.Margin < 0.01 || r.Eligible || r.Agrees != nil {
		t.Errorf("small (placed under Alpha Three) level 2 = %+v; want Alpha One nearest, compared 2, not eligible, no agreement", r)
	}
	if r := rep.docOf(t, loose).at(t, 2); !r.Eligible || r.Nearest.NodeID != a.one {
		t.Errorf("loose (placed at Alpha only) level 2 = %+v; want Alpha One eligible", r)
	}
	assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true,"margins":{"2":0.01}}`)
	got := assignedStatements(t, d, a.f.operator)
	if _, ok := got[a.docs[small]+"->"+a.one]; ok {
		t.Errorf("filed Alpha One for a document placed under Alpha Three: %v", got)
	}
	if _, ok := got[a.docs[loose]+"->"+a.one]; !ok {
		t.Errorf("did not file Alpha One for the document placed at Alpha only: %v", got)
	}
}
