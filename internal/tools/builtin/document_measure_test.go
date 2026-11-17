package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// measureCorpus is a concept fixture whose documents the test dates itself (root
// valid_at), so the periods are known. Concepts: topics/risk-management (rm) with
// topics/risk-management/outsourcing (out) under it, topics/reporting (rep), and
// rank/law (law).
type measureCorpus struct {
	f                 *conceptFixture
	d                 *Document
	rm, out, rep, law string
	roots             map[string]string // document → root chunk
	links             map[string]string // document → its subject statement
	n                 int
}

func newMeasureCorpus(t *testing.T, d *Document, base context.Context, md string) *measureCorpus {
	t.Helper()
	f := newConceptFixtureMD(t, d, base, md)
	k := &measureCorpus{f: f, d: d, roots: map[string]string{}, links: map[string]string{},
		rm: f.concepts["topics/risk-management"], out: f.concepts["topics/risk-management/outsourcing"],
		rep: f.concepts["topics/reporting"], law: f.concepts["rank/law"]}
	if k.rm == "" || k.out == "" || k.rep == "" || k.law == "" {
		t.Fatalf("concepts = %v", f.concepts)
	}
	return k
}

// doc makes a Document dated at (root valid_at; zero leaves it undated) and places
// it under concept ("" places it nowhere).
func (k *measureCorpus) doc(t *testing.T, at time.Time, concept string) string {
	t.Helper()
	k.n++
	out := k.f.c.exec(map[string]any{"op": "create_document", "scope": "tenant", "type": "Document",
		"title": fmt.Sprintf("Doc %d", k.n), "path": fmt.Sprintf("/kb/m%d", k.n)})
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)
	k.roots[doc] = root
	if !at.IsZero() {
		k.date(t, doc, at)
	}
	if concept != "" {
		k.links[doc] = k.f.link("link_chunks", k.f.chunk("tenant", doc), concept, "subject")
	}
	return doc
}

func (k *measureCorpus) date(t *testing.T, doc string, at time.Time) {
	t.Helper()
	g := k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": k.roots[doc]})
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": k.roots[doc],
		"valid_at": at.UnixNano(), "revision": g["revision"]})
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

// compute runs node_stats mode=compute as the operator.
func (k *measureCorpus) compute(t *testing.T) NodeStatsReport {
	t.Helper()
	ans := nodeStatsRun(t, k.d, k.f.operator, computeAll)
	if ans.Series == nil {
		t.Fatalf("node_stats answered without a series part: %+v", ans)
	}
	return ans
}

// measureRun runs op=measure as ctx and decodes it.
func measureRun(t *testing.T, d *Document, ctx context.Context, body string) measureAnswer {
	t.Helper()
	_, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out measureAnswer
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func (a measureAnswer) node(t *testing.T, id string) measureNode {
	t.Helper()
	for _, n := range a.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	t.Fatalf("no node %s in %+v", id, a.Nodes)
	return measureNode{}
}

// storedPointRow is one measure_points row, read directly.
type storedPointRow struct {
	Measure, Node, Period string
	Start                 int64
	Value, Fallback       float64
	Members, Basis        int
	ComputedAt            int64
}

func measureRows(t *testing.T, d *Document, ctx context.Context) []storedPointRow {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT measure, node_id, period, period_start, value_sum, members, basis,
		fallback_share, computed_at FROM measure_points ORDER BY measure, node_id, period_start, computed_at`)
	if err != nil {
		t.Fatal(err)
	}
	var out []storedPointRow
	for _, r := range res.Rows {
		out = append(out, storedPointRow{Measure: asStr(r[0]), Node: asStr(r[1]), Period: asStr(r[2]), Start: asInt64Val(r[3]),
			Value: *asFloat64Ptr(r[4]), Members: asInt(r[5]), Basis: asInt(r[6]), Fallback: *asFloat64Ptr(r[7]), ComputedAt: asInt64Val(r[8])})
	}
	return out
}

func rowsOf(rows []storedPointRow, measure, node string, start int64) []storedPointRow {
	var out []storedPointRow
	for _, r := range rows {
		if r.Measure == measure && r.Node == node && r.Start == start {
			out = append(out, r)
		}
	}
	return out
}

func monthStart(y int, m time.Month) int64 {
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).UnixNano()
}

// TestMeasure_RecomputeOfUnchangedPeriodAddsNoRow — a second compute over the same
// state appends no row; a change to one period appends rows for that period only.
func TestMeasure_RecomputeOfUnchangedPeriodAddsNoRow(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	for _, at := range []time.Time{day(2026, 6, 3), day(2026, 6, 9), day(2026, 7, 2)} {
		k.doc(t, at, k.rep)
	}
	first := k.compute(t)
	if first.Series.Computed == 0 || first.Series.Missing != first.Series.Computed {
		t.Fatalf("first compute: %+v; want every point missing and computed", first.Series)
	}
	before := measureRows(t, d, k.f.operator)
	again := k.compute(t)
	if again.Series.Computed != 0 || again.Series.Stale != 0 || again.Series.Missing != 0 {
		t.Errorf("second compute = %+v; want nothing computed", again.Series)
	}
	if after := measureRows(t, d, k.f.operator); !reflect.DeepEqual(before, after) {
		t.Errorf("rows changed on a compute with nothing stale:\n%v\n%v", before, after)
	}
	// Not vacuous: a member dated into July changes July's points and not June's.
	k.doc(t, day(2026, 7, 20), k.rep)
	k.compute(t)
	rows := measureRows(t, d, k.f.operator)
	if n := len(rowsOf(rows, measureCount, k.rep, monthStart(2026, 6))); n != 1 {
		t.Errorf("June's count of reporting has %d rows, want 1 (June did not change)", n)
	}
	if n := len(rowsOf(rows, measureCount, k.rep, monthStart(2026, 7))); n != 2 {
		t.Errorf("July's count of reporting has %d rows, want 2 (a member was added)", n)
	}
}

// TestMeasure_LateMemberAddsARowAndKeepsTheOld — a member dated into a period
// already computed appends a row with a later computed_at; the earlier row is kept
// unchanged. A period that loses every member gets an empty row, and a read leaves
// the period out.
func TestMeasure_LateMemberAddsARowAndKeepsTheOld(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, day(2026, 6, 3), k.rep)
	k.doc(t, day(2026, 6, 4), k.rep)
	k.compute(t)
	late := k.doc(t, day(2026, 6, 28), k.rep)
	k.compute(t)
	june := rowsOf(measureRows(t, d, k.f.operator), measureCount, k.rep, monthStart(2026, 6))
	if len(june) != 2 {
		t.Fatalf("June rows = %+v, want 2", june)
	}
	if june[0].Value != 2 || june[1].Value != 3 || june[1].ComputedAt <= june[0].ComputedAt {
		t.Errorf("June rows = %+v; want the old row (2) kept and a later one (3)", june)
	}

	// The late member is removed again: its period goes back to two, and when the
	// last member leaves, an empty row says the period holds nothing now.
	k.f.c.exec(map[string]any{"op": "unlink_chunks", "scope": "tenant", "id": k.links[late]})
	k.compute(t)
	if june = rowsOf(measureRows(t, d, k.f.operator), measureCount, k.rep, monthStart(2026, 6)); len(june) != 3 || june[2].Value != 2 {
		t.Errorf("after the unlink: June rows = %+v, want a third row of 2", june)
	}
	jul := k.doc(t, day(2026, 7, 2), k.rep)
	k.compute(t)
	k.f.c.exec(map[string]any{"op": "unlink_chunks", "scope": "tenant", "id": k.links[jul]})
	k.compute(t)
	july := rowsOf(measureRows(t, d, k.f.operator), measureCount, k.rep, monthStart(2026, 7))
	if len(july) != 2 || july[1].Members != 0 || july[1].Value != 0 {
		t.Errorf("July rows = %+v; want the empty row appended", july)
	}
	got := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep))
	if pts := got.node(t, k.rep).Points; len(pts) != 1 || pts[0].Label != "2026-06" {
		t.Errorf("read points = %+v; want June only", pts)
	}
}

// TestMeasure_ShownAtReturnsWhatWasStoredThen — shown_at reads the rows in force at
// that instant; without it, the latest.
func TestMeasure_ShownAtReturnsWhatWasStoredThen(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, day(2026, 6, 3), k.rep)
	k.doc(t, day(2026, 6, 4), k.rep)
	k.compute(t)
	between := time.Now().UnixNano()
	time.Sleep(2 * time.Millisecond)
	k.doc(t, day(2026, 6, 28), k.rep)
	k.compute(t)

	now := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep))
	then := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q,"shown_at":%d}`, k.rep, between))
	if p := now.node(t, k.rep).Points; len(p) != 1 || p[0].Value != 3 {
		t.Errorf("latest = %+v, want 3", p)
	}
	if p := then.node(t, k.rep).Points; len(p) != 1 || p[0].Value != 2 || p[0].ComputedAt > between {
		t.Errorf("shown_at = %+v, want the row of 2 computed before %d", p, between)
	}
	before := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q,"shown_at":1}`, k.rep))
	if p := before.node(t, k.rep).Points; len(p) != 0 {
		t.Errorf("shown_at before any compute = %+v, want none", p)
	}
}

// TestMeasure_DateFallbackShareIsReported — a thing is dated by its root valid_at,
// else its import's source_updated_at, else its creation; each row says what share
// of its members only the last dated.
func TestMeasure_DateFallbackShareIsReported(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	now := time.Now().UTC()
	cur := time.Unix(0, periodStart(now.UnixNano(), "month")).UTC()
	// valid_at wins over everything: dated into June although created now.
	k.doc(t, day(2026, 6, 3), k.rep)
	// valid_at in this month, the import's source date, and no date at all.
	k.doc(t, cur.Add(time.Minute), k.rep)
	out := k.f.c.exec(map[string]any{"op": "import_md", "scope": "tenant", "path": "/kb/imported",
		"markdown": "# Imported\n\nText.\n", "source_url": "https://example.org/a",
		"source_updated_at": cur.Add(time.Hour).Format(time.RFC3339)})
	imported := out["document_id"].(string)
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": out["root_chunk_id"], "type": "Document",
		"revision": k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": out["root_chunk_id"]})["revision"]})
	k.f.link("link_chunks", k.f.chunk("tenant", imported), k.rep, "subject")
	k.doc(t, time.Time{}, k.rep)
	k.compute(t)

	got := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep)).node(t, k.rep)
	if len(got.Points) != 2 {
		t.Fatalf("points = %+v; want June and this month", got.Points)
	}
	if p := got.Points[0]; p.Label != "2026-06" || p.Value != 1 || p.FallbackShare != 0 {
		t.Errorf("June = %+v; want 1 member dated by valid_at (fallback 0)", p)
	}
	if p := got.Points[1]; p.Label != periodLabel(cur.UnixNano(), "month") || p.Value != 3 || math.Abs(p.FallbackShare-1.0/3) > 1e-9 {
		t.Errorf("this month = %+v; want 3 members, a third dated by creation", p)
	}
}

// TestMeasure_ReaderLackingAMemberGetsNoSeries — a reader whose rules hide one
// member of a node gets that node's series withheld, and the series of a node
// whose members it all reads; a full reader gets both.
func TestMeasure_ReaderLackingAMemberGetsNoSeries(t *testing.T) {
	d, base, _ := documentFixture(t)
	checkReaderLackingAMemberGetsNoSeries(t, d, base)
}

func checkReaderLackingAMemberGetsNoSeries(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	k := newMeasureCorpus(t, d, ctx, conceptOntologyMD)
	k.doc(t, day(2026, 6, 3), k.rep)
	k.doc(t, day(2026, 6, 4), k.out)
	k.doc(t, day(2026, 6, 5), k.law)
	k.compute(t)
	if n := measureRun(t, d, k.f.reader, `{"op":"measure","measure":"count"}`).node(t, k.out); n.Withheld != "" || len(n.Points) != 1 {
		t.Fatalf("precondition: a full reader sees outsourcing: %+v", n)
	}
	deny := tools.UpdateRunPolicy(k.f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/m2"}})
	})
	got := measureRun(t, d, deny, `{"op":"measure","measure":"count"}`)
	for _, id := range []string{k.out, k.rm} {
		if n := got.node(t, id); n.Withheld == "" || len(n.Points) != 0 {
			t.Errorf("node %s: %+v; want withheld, no point", id, n)
		}
	}
	if n := got.node(t, k.law); n.Withheld != "" || len(n.Points) != 1 {
		t.Errorf("law (every member readable): %+v; want shown", n)
	}
	// share is a share of every projected thing of the period, the hidden one too.
	if n := measureRun(t, d, deny, fmt.Sprintf(`{"op":"measure","measure":"share","node":%q}`, k.rep)).node(t, k.rep); n.Withheld == "" {
		t.Errorf("share of reporting: %+v; want withheld (its basis holds the hidden document)", n)
	}
	desc := describeStats(t, d, deny)
	for _, s := range desc.Schemes {
		for _, c := range s.Concepts {
			if c.ChunkID == k.out && (c.Series == nil || c.Series.Withheld == "" || c.Series.Count != nil) {
				t.Errorf("with_stats outsourcing series = %+v; want withheld", c.Series)
			}
		}
	}
}

// TestMeasure_CompareOrdersNodesByShareChange — compare sums each concept's share
// over the baseline and the main range and orders the concepts by how far it moved.
func TestMeasure_CompareOrdersNodesByShareChange(t *testing.T) {
	d, base, _ := documentFixture(t)
	checkCompareOrdersNodesByShareChange(t, d, base)
}

func checkCompareOrdersNodesByShareChange(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	k := newMeasureCorpus(t, d, ctx, conceptOntologyMD)
	// June: reporting 3, outsourcing 1; July: reporting 1, outsourcing 3, risk
	// management itself 2. Both months also hold one unplaced Document.
	place := map[time.Month]map[string]int{6: {k.rep: 3, k.out: 1}, 7: {k.rep: 1, k.out: 3, k.rm: 2}}
	for m, byConcept := range place {
		for c, n := range byConcept {
			for i := 0; i < n; i++ {
				k.doc(t, day(2026, m, 2+i), c)
			}
		}
		k.doc(t, day(2026, m, 20), "")
	}
	k.compute(t)
	ans := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"share","scheme":"topics","from":%d,"to":%d,"compare":{"from":%d,"to":%d}}`,
		monthStart(2026, 7), monthStart(2026, 8), monthStart(2026, 6), monthStart(2026, 7)))
	if ans.Compare == nil {
		t.Fatal("no compare in the answer")
	}
	// Basis: June 5 (4 placed + 1 unplaced), July 7.
	want := map[string][4]int{k.rep: {1, 7, 3, 5}, k.out: {3, 7, 1, 5}, k.rm: {5, 7, 1, 5}}
	var order []string
	for _, n := range ans.Compare.Nodes {
		order = append(order, n.NodeID)
		w, ok := want[n.NodeID]
		if !ok {
			continue
		}
		if n.Count != w[0] || n.Basis != w[1] || n.BaselineCount != w[2] || n.BaselineBasis != w[3] {
			t.Errorf("%s: %+v; want counts %v", n.Name, n, w)
		}
		if lr := cappedLogRatio(w[0], w[1], w[2], w[3]); math.Abs(n.LogRatio-lr) > 1e-12 {
			t.Errorf("%s: log_ratio %v, want %v", n.Name, n.LogRatio, lr)
		}
	}
	// Smoothed shares, July over June: reporting 1.5/8 over 3.5/6 (ln -1.14), risk
	// management 5.5/8 over 1.5/6 (ln 1.01), outsourcing 3.5/8 over 1.5/6 (ln 0.56).
	if wantOrder := []string{k.rep, k.rm, k.out}; !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("order = %v, want %v (reporting, risk management, outsourcing)", order, wantOrder)
	}
}

// TestMeasure_PeriodDirectiveWeekBucketsByISOWeek — a scheme's @period week keeps
// its series by ISO week (Monday to Sunday); another scheme keeps months.
func TestMeasure_PeriodDirectiveWeekBucketsByISOWeek(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(conceptOntologyMD, "### topics\n\n", "### topics\n@period week\n\n", 1)
	if md == conceptOntologyMD {
		t.Fatal("precondition: the fixture ontology changed shape")
	}
	k := newMeasureCorpus(t, d, base, md)
	k.doc(t, day(2026, 7, 6), k.rep)  // Monday, 2026-W28
	k.doc(t, day(2026, 7, 12), k.rep) // Sunday, 2026-W28
	k.doc(t, day(2026, 7, 13), k.rep) // Monday, 2026-W29
	k.doc(t, day(2026, 7, 6), k.law)
	k.doc(t, day(2026, 7, 13), k.law)
	k.compute(t)
	rep := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep)).node(t, k.rep)
	var got []string
	for _, p := range rep.Points {
		got = append(got, fmt.Sprintf("%s=%v", p.Label, p.Value))
	}
	if rep.Period != "week" || !reflect.DeepEqual(got, []string{"2026-W28=2", "2026-W29=1"}) {
		t.Errorf("reporting (topics, @period week): period %q points %v", rep.Period, got)
	}
	law := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.law)).node(t, k.law)
	if law.Period != "month" || len(law.Points) != 1 || law.Points[0].Value != 2 {
		t.Errorf("law (rank, no @period): %+v; want one month of 2", law)
	}
}

// shiftCorpus places reporting's members in two months: three in June pointing
// one way, three in July pointing another, each with a document vector.
func shiftCorpus(t *testing.T, k *measureCorpus) {
	t.Helper()
	vecs := map[time.Month][]float32{6: {1, 0, 0, 0}, 7: {0, 1, 0, 0}}
	for _, m := range []time.Month{6, 7} {
		for i := 0; i < 3; i++ {
			doc := k.doc(t, day(2026, m, 2+i), k.rep)
			sc := &statsCorpus{f: k.f, d: k.d, vecs: map[string][]float32{}, briefs: map[string]string{}, docs: k.roots}
			sc.annotate(t, doc, fmt.Sprintf("report %d %d", m, i), vecs[m])
		}
	}
}

// TestMeasure_ShiftIsTheCentroidMoveFromEarlierPeriods — where vectors are kept,
// a period with enough members after enough earlier ones has a shift: 1 − cos of
// the two centroids.
func TestMeasure_ShiftIsTheCentroidMoveFromEarlierPeriods(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	shiftCorpus(t, k)
	ans := k.compute(t)
	if ans.Series.Shift != "" {
		t.Fatalf("shift off: %s", ans.Series.Shift)
	}
	got := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"shift","node":%q}`, k.rep)).node(t, k.rep)
	if len(got.Points) != 1 || got.Points[0].Label != "2026-07" || math.Abs(got.Points[0].Value-1) > 1e-9 || got.Points[0].Members != 3 {
		t.Errorf("shift = %+v; want July only, 1 (orthogonal), 3 members", got.Points)
	}
}

// TestMeasure_StoreWithoutVectorsHasNoShift — on a store that keeps no vectors the
// pass says shift is not kept, stores no shift row, and the read says why; the
// counts are kept as everywhere.
func TestMeasure_StoreWithoutVectorsHasNoShift(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	shiftCorpus(t, k)
	d.Store = k.f.c.vs.Store // the plain sqlite store: SupportsVectors is false
	ans := k.compute(t)
	if !strings.Contains(ans.Series.Shift, "keeps no vectors") {
		t.Errorf("series.shift = %q; want why shift is not kept", ans.Series.Shift)
	}
	for _, r := range measureRows(t, d, k.f.operator) {
		if r.Measure == measureShift {
			t.Errorf("a shift row was stored: %+v", r)
		}
	}
	got := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"shift","node":%q}`, k.rep))
	if !strings.Contains(got.Note, "keeps no vectors") || len(got.node(t, k.rep).Points) != 0 {
		t.Errorf("read = %+v; want no point and a note", got)
	}
	if c := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep)).node(t, k.rep); len(c.Points) != 2 {
		t.Errorf("count = %+v; want June and July", c.Points)
	}
}

// TestMeasure_UnplacedCountsProjectedThingsUnderNoConcept — unplaced is per scheme
// and period: the projected things dated in it with no placement in the scheme.
func TestMeasure_UnplacedCountsProjectedThingsUnderNoConcept(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, day(2026, 6, 2), k.rep)
	k.doc(t, day(2026, 6, 3), k.law) // placed in rank, unplaced in topics
	k.doc(t, day(2026, 6, 4), "")
	k.compute(t)
	ans := measureRun(t, d, k.f.reader, `{"op":"measure","measure":"unplaced","scheme":"topics"}`)
	if len(ans.Nodes) != 1 || len(ans.Nodes[0].Points) != 1 {
		t.Fatalf("unplaced = %+v", ans.Nodes)
	}
	p := ans.Nodes[0].Points[0]
	if p.Value != 2 || p.Basis == nil || *p.Basis != 3 || p.Share == nil || math.Abs(*p.Share-2.0/3) > 1e-9 {
		t.Errorf("June = %+v; want 2 of 3 unplaced", p)
	}
}

// TestMeasure_InputIsChecked — what measure cannot answer is refused before any read.
func TestMeasure_InputIsChecked(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	for body, want := range map[string]string{
		`{"op":"measure"}`:                                                    "measure is required",
		`{"op":"measure","measure":"rating"}`:                                 "not a built-in series",
		`{"op":"measure","measure":"unplaced","node":"x"}`:                    "kept per scheme",
		`{"op":"measure","measure":"count","compare":{"from":1,"to":2}}`:      "takes measure share",
		`{"op":"measure","measure":"share","compare":{"from":1}}`:             "compare needs from and to",
		`{"op":"measure","measure":"count","from":5,"to":5}`:                  "to must be after",
		`{"op":"measure","measure":"count","node":"nowhere"}`:                 `no node "nowhere"`,
		`{"op":"measure","measure":"count","scheme":"nowhere"}`:               `no scheme "nowhere"`,
		`{"op":"measure","measure":"share","node":"Document","compare":null}`: `no node "Document"`,
	} {
		_, res := docExec(t, d, k.f.reader, body)
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %s; want an error saying %q", body, res.Text, want)
		}
	}
}

// TestDescribeOntology_WithStatsCarriesTheSeries — with_stats serves each node's
// latest count, share and trend beside its statistics.
func TestDescribeOntology_WithStatsCarriesTheSeries(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, day(2026, 6, 2), k.rep)
	k.doc(t, day(2026, 6, 3), k.out)
	k.doc(t, day(2026, 7, 2), k.rep)
	k.doc(t, day(2026, 7, 3), k.rep)
	k.compute(t)
	desc := describeStats(t, d, k.f.reader)
	var series *nodeSeriesView
	for _, s := range desc.Schemes {
		for _, c := range s.Concepts {
			if c.ChunkID == k.rep {
				series = c.Series
			}
		}
	}
	if series == nil || series.Latest != "2026-07" || series.Count == nil || *series.Count != 2 || series.Share == nil || *series.Share != 1 {
		t.Fatalf("reporting series = %+v; want July, 2 members, share 1", series)
	}
	if tr := series.Trend; tr == nil || tr.Previous != "2026-06" || tr.PreviousCount != 1 || tr.PreviousShare == nil ||
		*tr.PreviousShare != 0.5 || tr.Change != 0.5 {
		t.Errorf("trend = %+v; want from June's share 0.5, change 0.5", series.Trend)
	}
	for _, c := range desc.Classes {
		if c.Name == "Document" && (c.Series == nil || c.Series.Count == nil || *c.Series.Count != 2 || c.Series.Share != nil) {
			t.Errorf("class Document series = %+v; want July's 2, no share", c.Series)
		}
	}
}

// TestOntologySchemes_MeasureSeriesOnBothTiers — the append-only store, shown_at
// and the late member on both SQL Memory tiers (the postgres half runs where
// LOOMCYCLE_TEST_SQLMEM_PG_DSN is set; the CI filter takes this prefix).
func TestOntologySchemes_MeasureSeriesOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newMeasureCorpus(t, d, ctx, conceptOntologyMD)
		k.doc(t, day(2026, 6, 3), k.rep)
		k.doc(t, day(2026, 6, 4), k.rep)
		k.compute(t)
		between := time.Now().UnixNano()
		time.Sleep(2 * time.Millisecond)
		if again := k.compute(t); again.Series.Computed != 0 {
			t.Errorf("unchanged recompute appended %d rows", again.Series.Computed)
		}
		k.doc(t, day(2026, 6, 28), k.rep)
		k.compute(t)
		june := rowsOf(measureRows(t, d, k.f.operator), measureCount, k.rep, monthStart(2026, 6))
		if len(june) != 2 || june[0].Value != 2 || june[1].Value != 3 {
			t.Errorf("June rows = %+v; want 2 then 3", june)
		}
		then := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q,"shown_at":%d}`, k.rep, between))
		if p := then.node(t, k.rep).Points; len(p) != 1 || p[0].Value != 2 {
			t.Errorf("shown_at = %+v, want 2", p)
		}
	})
}

// TestOntologySchemes_MeasureAccessAndCompareOnBothTiers — the withholding rule and
// compare on both tiers.
func TestOntologySchemes_MeasureAccessAndCompareOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		checkReaderLackingAMemberGetsNoSeries(t, d, ctx)
	})
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		checkCompareOrdersNodesByShareChange(t, d, ctx)
	})
}
