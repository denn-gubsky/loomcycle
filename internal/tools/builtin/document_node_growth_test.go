package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestGrowth_GonePointIsStaleAndNotRanked — a stored point the documents no longer
// yield (its only member is now dated in another period) is counted stale and is
// not ranked from: the old period's member no longer counts as "previous".
func TestGrowth_GonePointIsStaleAndNotRanked(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	moved := k.doc(t, monthsAgo(3), k.rep)
	k.doc(t, monthsAgo(0), k.rep)
	k.compute(t)
	if a := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":2}`); a.candidate(t, k.rep).Previous != 1 || a.SeriesStale != 0 {
		t.Fatalf("precondition = %+v", a.candidate(t, k.rep))
	}
	// Clear its date: it falls back to when it was stored, this month.
	g := k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": k.roots[moved]})
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": k.roots[moved], "valid_at": nil, "revision": g["revision"]})
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":2,"scheme":"topics"}`)
	if c := ans.candidate(t, k.rep); c.Previous != 0 || c.Recent != 1 {
		t.Errorf("reporting = recent %d previous %d; want 1 and 0 (the gone point not ranked, this month's still the stored 1)", c.Recent, c.Previous)
	}
	// Within topics. Stale: this month's count and unplaced basis (changed), the old month's count
	// and unplaced basis (gone).
	if ans.SeriesStale != 4 || ans.SeriesMissing != 0 || ans.Note == "" {
		t.Errorf("series stale %d missing %d note %q; want 4, 0 and a note", ans.SeriesStale, ans.SeriesMissing, ans.Note)
	}
}

// TestGrowth_FuturePeriodIsNotRecent — a member dated after the period holding now
// counts in no window and is no last period: a node whose other members are old is
// quiet, not a candidate.
func TestGrowth_FuturePeriodIsNotRecent(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, monthsAgo(5), k.rep)
	k.doc(t, monthsAgo(-2), k.rep)
	k.compute(t)
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":2}`)
	for _, c := range ans.Candidates {
		if c.NodeID == k.rep {
			t.Errorf("a future-dated member made reporting a candidate: %+v", c)
		}
	}
	var q *quietNode
	for i := range ans.Quiet {
		if ans.Quiet[i].NodeID == k.rep {
			q = &ans.Quiet[i]
		}
	}
	if q == nil || q.LastPeriod != monthsAgo(5).Format("2006-01") || q.PeriodsSince != 5 {
		t.Errorf("reporting quiet = %+v; want last period five months ago", q)
	}
}

// vectorListRefusingStore fails every vector read: a pass that needs no vector
// must not read one.
type vectorListRefusingStore struct{ *ftsStore }

func (s *vectorListRefusingStore) MemoryEmbedListVectors(context.Context, string, store.MemoryScope, string, string, string, int) ([]store.MemoryEmbedRow, error) {
	return nil, errors.New("a vector was read")
}

// TestGrowth_ReadsNoVector — growth uses counts only, so with an embedder set it
// still reads no member vector (node_stats report, which does, fails here).
func TestGrowth_ReadsNoVector(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	growthCorpus(t, k)
	k.compute(t)
	d.Store = &vectorListRefusingStore{d.Store.(*ftsStore)}
	if _, res := docExec(t, d, k.f.operator, `{"op":"node_stats"}`); !res.IsError {
		t.Fatal("precondition: a report reads vectors and fails on this store")
	}
	if ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":2}`); len(ans.Candidates) == 0 {
		t.Errorf("growth = %+v; want a ranking", ans)
	}
}

// monthsAgo is the 10th of the month n months before now, at noon UTC: inside that
// month whatever today's date.
func monthsAgo(n int) time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 10, 12, 0, 0, 0, time.UTC).AddDate(0, -n, 0)
}

func growthRun(t *testing.T, d *Document, ctx context.Context, body string) NodeGrowthReport {
	t.Helper()
	_, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out NodeGrowthReport
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func (a NodeGrowthReport) candidate(t *testing.T, id string) growthNode {
	t.Helper()
	for _, c := range a.Candidates {
		if c.NodeID == id {
			return c
		}
	}
	t.Fatalf("no candidate %s in %+v", id, a.Candidates)
	return growthNode{}
}

// growthCorpus dates documents so that, with a window of two months, growth and
// recent size disagree: reporting gains 2 recent members from none (+2); Law 1
// from none (+1); outsourcing (and Risk Management through it) has the most recent
// members, 3, but had 4 before (-1).
func growthCorpus(t *testing.T, k *measureCorpus) {
	t.Helper()
	for _, m := range []int{0, 1} {
		k.doc(t, monthsAgo(m), k.rep)
	}
	for _, m := range []int{0, 0, 1, 2, 2, 3, 3} {
		k.doc(t, monthsAgo(m), k.out)
	}
	k.doc(t, monthsAgo(1), k.law)
}

// checkGrowthRanking — candidates are ranked by recent growth, then recent members,
// then size, then tag, with their windows' counts.
func checkGrowthRanking(t *testing.T, d *Document, ctx context.Context) {
	k := newMeasureCorpus(t, d, ctx, conceptOntologyMD)
	growthCorpus(t, k)
	k.compute(t)
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":2}`)
	if ans.QuietPeriods != 2 || ans.SeriesStale != 0 || ans.SeriesMissing != 0 || ans.Note != "" {
		t.Errorf("header = %+v; want a fresh series", ans)
	}
	var order []string
	for _, c := range ans.Candidates {
		order = append(order, c.NodeID)
	}
	want := []string{k.rep, k.law, k.rm, k.out}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want reporting, Law, Risk Management, outsourcing (%v)", order, want)
	}
	for id, w := range map[string][3]int{k.rep: {2, 0, 2}, k.law: {1, 0, 1}, k.rm: {3, 4, -1}, k.out: {3, 4, -1}} {
		c := ans.candidate(t, id)
		if c.Recent != w[0] || c.Previous != w[1] || c.Growth != w[2] {
			t.Errorf("%s: recent %d previous %d growth %d, want %v", c.Name, c.Recent, c.Previous, c.Growth, w)
		}
	}
	if c := ans.candidate(t, k.rep); c.LastPeriod != monthsAgo(0).Format("2006-01") || c.Scheme != "topics" || c.Fixed != "" {
		t.Errorf("reporting = %+v", c)
	}
}

// TestGrowth_CandidatesAreRankedByRecentGrowth — see checkGrowthRanking.
func TestGrowth_CandidatesAreRankedByRecentGrowth(t *testing.T) {
	d, base, _ := documentFixture(t)
	checkGrowthRanking(t, d, base)
}

// TestOntologySchemes_GrowthOnBothTiers — the ranking on both SQL Memory tiers (the
// postgres half runs where LOOMCYCLE_TEST_SQLMEM_PG_DSN is set; the CI filter takes
// this prefix).
func TestOntologySchemes_GrowthOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		checkGrowthRanking(t, d, ctx)
	})
}

// TestGrowth_QuietNodeIsReportedAndNeverRetired — a node with members and none in
// the recent window is quiet, with the periods since its last member; the pass
// writes nothing, so the concept stays in force with its placements.
func TestGrowth_QuietNodeIsReportedAndNeverRetired(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, monthsAgo(8), k.rep)
	k.doc(t, monthsAgo(9), k.rep)
	k.doc(t, monthsAgo(0), k.out)
	k.compute(t)
	before := ontologySnapshot(t, d, k.f.operator)
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":3}`)
	var quiet *quietNode
	for i := range ans.Quiet {
		if ans.Quiet[i].NodeID == k.rep {
			quiet = &ans.Quiet[i]
		}
		if ans.Quiet[i].NodeID == k.out || ans.Quiet[i].NodeID == k.rm {
			t.Errorf("a node with a recent member is quiet: %+v", ans.Quiet[i])
		}
	}
	if quiet == nil || quiet.Members != 2 || quiet.PeriodsSince != 8 || quiet.LastPeriod != monthsAgo(8).Format("2006-01") || quiet.Kind != nodeKindConcept {
		t.Fatalf("reporting quiet = %+v (all %+v); want 2 members, 8 periods since", quiet, ans.Quiet)
	}
	for _, c := range ans.Candidates {
		if c.NodeID == k.rep {
			t.Errorf("a quiet node is a candidate: %+v", c)
		}
	}
	if after := ontologySnapshot(t, d, k.f.operator); !reflect.DeepEqual(before, after) {
		t.Errorf("growth changed the ontology:\n%v\n%v", before, after)
	}
	if n := growthCount(t, d, k.f.operator, `SELECT count(*) FROM chunk_edges WHERE to_id = ? AND status = 'asserted' AND expired_at IS NULL`, k.rep); n != 2 {
		t.Errorf("reporting's live placements = %d, want 2", n)
	}
}

// TestGrowth_UnplacedRisingPeriodsComeFirst — a scheme's unplaced periods are listed
// rising first; a rising period carries its unplaced documents.
func TestGrowth_UnplacedRisingPeriodsComeFirst(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, monthsAgo(3), k.rep)
	k.doc(t, monthsAgo(3), "") // 1 of 2 unplaced
	k.doc(t, monthsAgo(2), k.rep)
	k.doc(t, monthsAgo(2), k.out) // 0 of 2
	u1 := k.doc(t, monthsAgo(1), "")
	u2 := k.doc(t, monthsAgo(1), "") // 2 of 2: rising
	k.doc(t, monthsAgo(0), k.rep)
	k.doc(t, monthsAgo(0), "") // 1 of 2: falling, and the most recent
	k.compute(t)
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","scheme":"topics"}`)
	if len(ans.Unplaced) != 1 || ans.Unplaced[0].Scheme != "topics" {
		t.Fatalf("unplaced = %+v", ans.Unplaced)
	}
	ps := ans.Unplaced[0].Periods
	labels := []string{}
	for _, p := range ps {
		labels = append(labels, p.Label)
	}
	want := []string{monthsAgo(1).Format("2006-01"), monthsAgo(0).Format("2006-01"), monthsAgo(2).Format("2006-01"), monthsAgo(3).Format("2006-01")}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("periods = %v, want %v (rising first, then most recent)", labels, want)
	}
	if !ps[0].Rising || ps[0].Share != 1 || ps[0].PreviousShare == nil || *ps[0].PreviousShare != 0 {
		t.Errorf("rising period = %+v", ps[0])
	}
	docs := append([]string{}, ps[0].Documents...)
	sort.Strings(docs)
	wantDocs := []string{u1, u2}
	sort.Strings(wantDocs)
	if !reflect.DeepEqual(docs, wantDocs) {
		t.Errorf("rising period documents = %v, want %v", docs, wantDocs)
	}
	for _, p := range ps[1:] {
		if p.Rising || p.Documents != nil {
			t.Errorf("period %+v; want not rising, no documents", p)
		}
	}
}

// adoptedTopicsMD is conceptOntologyMD with topics an adopted scheme.
var adoptedTopicsMD = strings.Replace(conceptOntologyMD, "### topics\n\n",
	"### topics\n@origin adopted\n@source https://example.org/topics\n@version 2026.1\n\n", 1)

// TestGrowth_AdoptedConceptIsNeverRenamedOrMoved — growth over an adopted scheme
// marks its concepts fixed (growth beneath them may only add local concepts) and
// changes nothing in the ontology: no concept renamed, moved, retired or proposed.
func TestGrowth_AdoptedConceptIsNeverRenamedOrMoved(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, adoptedTopicsMD)
	read, _ := d.tenantOntologyRead(k.f.operator)
	adopted := false
	for _, s := range read.Schemes {
		adopted = adopted || (s.Name == "topics" && s.Origin == "adopted")
	}
	if !adopted {
		t.Fatalf("precondition: topics adopted: %+v", read.Schemes)
	}
	for _, m := range []int{0, 0, 1} {
		k.doc(t, monthsAgo(m), k.rep)
	}
	k.doc(t, monthsAgo(9), k.out)
	k.doc(t, monthsAgo(0), k.law)
	k.compute(t)
	before := ontologySnapshot(t, d, k.f.operator)
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":3}`)
	if c := ans.candidate(t, k.rep); c.Fixed != "adopted" {
		t.Errorf("reporting = %+v; want fixed adopted", c)
	}
	if c := ans.candidate(t, k.law); c.Fixed != "" {
		t.Errorf("Law (rank, declared) = %+v; want not fixed", c)
	}
	for _, q := range ans.Quiet {
		if (q.NodeID == k.out || q.NodeID == k.rm) && q.Fixed != "adopted" {
			t.Errorf("quiet adopted concept = %+v; want fixed adopted", q)
		}
	}
	if after := ontologySnapshot(t, d, k.f.operator); !reflect.DeepEqual(before, after) {
		t.Errorf("growth changed the ontology:\n%v\n%v", before, after)
	}
}

// TestGrowth_RunsWithoutVectorsAndSaysWhenTheSeriesIsBehind — growth reads counts,
// so it runs on a store without vectors; before a compute it says the series is
// behind and judges nothing.
func TestGrowth_RunsWithoutVectorsAndSaysWhenTheSeriesIsBehind(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	growthCorpus(t, k)
	d.Store = k.f.c.vs.Store // the plain sqlite store: SupportsVectors is false
	behind := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth"}`)
	if behind.SeriesMissing == 0 || behind.Note == "" || len(behind.Candidates) != 0 || len(behind.Quiet) != 0 {
		t.Errorf("before a compute = %+v; want missing points, a note, nothing judged", behind)
	}
	k.compute(t)
	ans := growthRun(t, d, k.f.operator, `{"op":"node_stats","mode":"growth","quiet_periods":2}`)
	if ans.SeriesMissing != 0 || ans.Note != "" || len(ans.Candidates) == 0 || ans.Candidates[0].NodeID != k.rep {
		t.Errorf("after a compute = %+v", ans)
	}
}

// TestGrowth_InputAndAccessAreChecked — quiet_periods only with mode growth and in
// range; an unknown mode names growth; a plain reader may not run it.
func TestGrowth_InputAndAccessAreChecked(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	for body, want := range map[string]string{
		`{"op":"node_stats","quiet_periods":3}`:                   "quiet_periods is taken only with mode growth",
		`{"op":"node_stats","mode":"growth","quiet_periods":-1}`:  "quiet_periods must be 1 to 120",
		`{"op":"node_stats","mode":"growth","quiet_periods":121}`: "quiet_periods must be 1 to 120",
		`{"op":"node_stats","mode":"nope"}`:                       "is not report, compute, misfits or growth",
	} {
		_, res := docExec(t, d, k.f.operator, body)
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %s; want %q", body, res.Text, want)
		}
	}
	if _, res := docExec(t, d, k.f.reader, `{"op":"node_stats","mode":"growth"}`); !res.IsError || !strings.Contains(res.Text, "permission") {
		t.Errorf("a plain reader ran growth: %s", res.Text)
	}
}

// TestNodeStats_AnswerIsUnchangedWithoutGrowthOptions — plan §2, byte-identical when
// unused: report and compute carry exactly the fields they did before mode growth.
func TestNodeStats_AnswerIsUnchangedWithoutGrowthOptions(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	growthCorpus(t, k)
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
}

// ontologySnapshot is every chunk of the ontology document: id, parent, title,
// status and body — what a rename, a move, a retirement or a proposal would change.
func ontologySnapshot(t *testing.T, d *Document, ctx context.Context) []string {
	t.Helper()
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("ontology unreadable")
	}
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT id, COALESCE(parent_id, ''), title, COALESCE(status, '') FROM chunks
		WHERE document_id = ? ORDER BY id`, read.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, fmt.Sprintf("%v|%v|%v|%v", r[0], r[1], r[2], r[3]))
	}
	return out
}

func growthCount(t *testing.T, d *Document, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, sql, args...)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("%s: %v", sql, err)
	}
	return asInt(res.Rows[0][0])
}
