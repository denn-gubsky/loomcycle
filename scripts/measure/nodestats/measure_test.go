package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
)

func u(t *testing.T, v ...float32) []float64 {
	t.Helper()
	x, err := unitVec(v)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestAgreement_Rates(t *testing.T) {
	cases := []struct {
		name                  string
		in                    func(t *testing.T) AgreementInput
		evaluable, top1, top3 int
	}{
		{
			name: "two separated classes: every unit nearest its own",
			in: func(t *testing.T) AgreementInput {
				return AgreementInput{
					Classes: []string{"A", "B"},
					Units: []Unit{
						{"a1", u(t, 1, 0.1, 0)}, {"a2", u(t, 1, -0.1, 0)}, {"a3", u(t, 1, 0, 0.1)},
						{"b1", u(t, 0, 1, 0.1)}, {"b2", u(t, 0.1, 1, 0)}, {"b3", u(t, 0, 1, -0.1)},
					},
					Weights: map[string]map[string]float64{
						"a1": {"A": 1}, "a2": {"A": 1}, "a3": {"A": 1},
						"b1": {"B": 1}, "b2": {"B": 1}, "b3": {"B": 1},
					},
				}
			},
			evaluable: 6, top1: 6, top3: 6,
		},
		{
			name: "a sole member is not evaluable: its class empties when it is left out",
			in: func(t *testing.T) AgreementInput {
				return AgreementInput{
					Classes: []string{"A", "B"},
					Units:   []Unit{{"a1", u(t, 1, 0)}, {"a2", u(t, 1, 0.1)}, {"b1", u(t, 0, 1)}},
					Weights: map[string]map[string]float64{"a1": {"A": 1}, "a2": {"A": 1}, "b1": {"B": 1}},
				}
			},
			evaluable: 2, top1: 2, top3: 2,
		},
		{
			name: "a mislabelled unit misses top-1 but a two-class level always hits top-3",
			in: func(t *testing.T) AgreementInput {
				return AgreementInput{
					Classes: []string{"A", "B"},
					Units: []Unit{
						{"a1", u(t, 1, 0)}, {"a2", u(t, 1, 0.1)}, {"x", u(t, 1, -0.1)},
						{"b1", u(t, 0, 1)}, {"b2", u(t, 0.1, 1)},
					},
					Weights: map[string]map[string]float64{
						"a1": {"A": 1}, "a2": {"A": 1}, "x": {"B": 1}, "b1": {"B": 1}, "b2": {"B": 1},
					},
				}
			},
			evaluable: 5, top1: 4, top3: 5,
		},
		{
			name: "sections leave their whole document out",
			in: func(t *testing.T) AgreementInput {
				// Document d's two sections alone define A apart from one far
				// member; left out together, A points away from them.
				return AgreementInput{
					Classes: []string{"A", "B"},
					Units: []Unit{
						{"d", u(t, 0, 1, 0)}, {"d", u(t, 0, 1, 0.1)},
						{"a1", u(t, 0, 0, 1)},
						{"b1", u(t, 0.1, 1, 0)}, {"b2", u(t, 0, 1, -0.1)},
					},
					Weights: map[string]map[string]float64{"d": {"A": 1}, "a1": {"A": 1}, "b1": {"B": 1}, "b2": {"B": 1}},
				}
			},
			// d's sections are nearer B (miss); a1 is nearer d's A-sections than
			// B (hit); b1, b2 nearest B (hit).
			evaluable: 5, top1: 3, top3: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r, err := Agreement(tc.in(t))
			if err != nil {
				t.Fatal(err)
			}
			if r.Evaluable != tc.evaluable || r.Top1 != tc.top1 || r.Top3 != tc.top3 {
				t.Fatalf("got evaluable=%d top1=%d top3=%d, want %d %d %d", r.Evaluable, r.Top1, r.Top3,
					tc.evaluable, tc.top1, tc.top3)
			}
		})
	}
}

// The closed form must rank exactly as recomputing every centroid without the
// group does (weights 1; nodestats computes the reference centroids).
func TestAgreement_MatchesRecomputedLeaveOneOut(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	classes := []string{"A", "B", "C", "D"}
	var units []Unit
	weights := map[string]map[string]float64{}
	vecs := map[string]nodestats.Vector{}
	for i := 0; i < 30; i++ {
		g := string(rune('a'+i%26)) + string(rune('0'+i/26))
		v := make(nodestats.Vector, 8)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		vecs[g] = v
		units = append(units, Unit{Group: g, Vec: u(t, v...)})
		w := map[string]float64{classes[rng.IntN(4)]: 1}
		if rng.IntN(3) == 0 {
			w[classes[rng.IntN(4)]] = 1
		}
		weights[g] = w
	}
	ranks, _, err := Agreement(AgreementInput{Classes: classes, Units: units, Weights: weights})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ranks {
		var want []scored
		for _, c := range classes {
			var vs []nodestats.Vector
			for g, w := range weights {
				if g != r.Group && w[c] > 0 {
					vs = append(vs, vecs[g])
				}
			}
			if len(vs) == 0 {
				continue
			}
			cen, err := nodestats.CentroidOf(vs)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, scored{c, dotf(u(t, vecs[r.Group]...), cen.Unit)})
		}
		if got := rankIDs(want); !reflect.DeepEqual(got, r.Ranked) {
			t.Fatalf("group %s: ranked %v, recomputed %v", r.Group, r.Ranked, got)
		}
		if len(want) >= 2 {
			if m := want[0].s - want[1].s; !r.HasMargin || math.Abs(r.Margin-m) > 1e-9 {
				t.Fatalf("group %s: margin %v (has %v), recomputed %v", r.Group, r.Margin, r.HasMargin, m)
			}
		}
		if hit := r.Evaluable && weights[r.Group][r.Ranked[0]] > 0; hit != r.Top1 {
			t.Fatalf("group %s: top1 %v, want %v", r.Group, r.Top1, hit)
		}
	}
}

func TestMarginAgreement_HitRateRisesWithMargin(t *testing.T) {
	// 20 points, margins 0.005 .. 0.100; the nearest is wrong for every point
	// below 0.03 and right above it.
	var ps []MarginPoint
	for i := 1; i <= 20; i++ {
		m := float64(i) * 0.005
		ps = append(ps, MarginPoint{Margin: m, Hit: m > 0.0275})
	}
	r := MarginAgreement(ps, 5, []float64{0, 0.03, 0.08})
	if r.Units != 20 || r.Hits != 15 {
		t.Fatalf("units %d hits %d, want 20 / 15", r.Units, r.Hits)
	}
	wantBins := []MarginBin{
		{Lo: 0.005, Hi: 0.020, Units: 4, Hits: 0, HitRate: 0},
		{Lo: 0.025, Hi: 0.040, Units: 4, Hits: 3, HitRate: 0.75},
		{Lo: 0.045, Hi: 0.060, Units: 4, Hits: 4, HitRate: 1},
		{Lo: 0.065, Hi: 0.080, Units: 4, Hits: 4, HitRate: 1},
		{Lo: 0.085, Hi: 0.100, Units: 4, Hits: 4, HitRate: 1},
	}
	if len(r.Bins) != len(wantBins) {
		t.Fatalf("bins %+v", r.Bins)
	}
	for i, b := range r.Bins {
		w := wantBins[i]
		if math.Abs(b.Lo-w.Lo) > 1e-12 || math.Abs(b.Hi-w.Hi) > 1e-12 || b.Units != w.Units || b.Hits != w.Hits || b.HitRate != w.HitRate {
			t.Fatalf("bin %d = %+v, want %+v", i, b, w)
		}
	}
	wantTh := []MarginThreshold{
		{Min: 0, Units: 20, Coverage: 1, Hits: 15, HitRate: 0.75},
		{Min: 0.03, Units: 15, Coverage: 0.75, Hits: 15, HitRate: 1},
		{Min: 0.08, Units: 5, Coverage: 0.25, Hits: 5, HitRate: 1},
	}
	if !reflect.DeepEqual(r.Thresholds, wantTh) {
		t.Fatalf("thresholds %+v, want %+v", r.Thresholds, wantTh)
	}
}

func TestMarginAgreement_UnevenAndEmpty(t *testing.T) {
	r := MarginAgreement([]MarginPoint{{0.3, true}, {0.1, false}, {0.2, true}}, 5, []float64{0.15})
	if len(r.Bins) != 3 || r.Bins[0].Lo != 0.1 || r.Bins[2].Hi != 0.3 {
		t.Fatalf("fewer points than bins: one point per bin in margin order, got %+v", r.Bins)
	}
	if th := r.Thresholds[0]; th.Units != 2 || th.HitRate != 1 {
		t.Fatalf("threshold %+v", th)
	}
	sizes := []int{}
	for _, b := range MarginAgreement(make([]MarginPoint, 7), 5, nil).Bins {
		sizes = append(sizes, b.Units)
	}
	if !reflect.DeepEqual(sizes, []int{2, 2, 1, 1, 1}) {
		t.Fatalf("7 points in 5 bins: sizes %v", sizes)
	}
	if e := MarginAgreement(nil, 5, []float64{0}); len(e.Bins) != 0 || e.Thresholds[0].Units != 0 {
		t.Fatalf("empty input: %+v", e)
	}
}

func TestMarginPoints_SkipNotEvaluableAndCountNoMargin(t *testing.T) {
	ps, none := marginPoints([]UnitRank{
		{Evaluable: true, Top1: true, Margin: 0.04, HasMargin: true},
		{Evaluable: true},
		{Evaluable: false, Margin: 0.5, HasMargin: true},
	})
	if none != 1 || len(ps) != 1 || ps[0] != (MarginPoint{0.04, true}) {
		t.Fatalf("points %+v none %d", ps, none)
	}
}

func TestPermute_KeepsEveryClassSize(t *testing.T) {
	w := map[string]map[string]float64{
		"d1": {"A": 1}, "d2": {"A": 1}, "d3": {"B": 1, "C": 1}, "d4": {"C": 1}, "d5": {"B": 1},
	}
	size := func(m map[string]map[string]float64) map[string]int {
		out := map[string]int{}
		for _, ws := range m {
			for c := range ws {
				out[c]++
			}
		}
		return out
	}
	moved := false
	for seed := uint64(1); seed <= 20; seed++ {
		p := Permute(w, seed)
		if !reflect.DeepEqual(size(p), size(w)) {
			t.Fatalf("seed %d: sizes %v, want %v", seed, size(p), size(w))
		}
		moved = moved || !reflect.DeepEqual(p, w)
	}
	if !moved {
		t.Fatal("no permutation moved a label")
	}
	if !reflect.DeepEqual(Permute(w, 3), Permute(w, 3)) {
		t.Fatal("same seed, different permutation")
	}
}

func TestFixedAgreement_RanksAgainstGivenVectors(t *testing.T) {
	in := AgreementInput{
		Classes: []string{"A", "B", "C"},
		Units:   []Unit{{"x", u(t, 1, 0)}, {"y", u(t, 0, 1)}},
		Weights: map[string]map[string]float64{"x": {"A": 1}, "y": {"A": 1}},
	}
	// C has no vector: not a candidate.
	r := FixedAgreement(in, map[string][]float64{"A": u(t, 1, 0.1), "B": u(t, 0.1, 1)})
	if r.Evaluable != 2 || r.Top1 != 1 || r.Top3 != 2 {
		t.Fatalf("got %+v", r)
	}
}

func TestPercentile_NearestRank(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3}
	for _, tc := range []struct{ p, want float64 }{{95, 5}, {50, 3}, {20, 1}, {100, 5}} {
		if got := Percentile(xs, tc.p); got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func testTaxonomy(t *testing.T) *Taxonomy {
	t.Helper()
	tax, err := NewTaxonomy([]Concept{
		{ID: "t1", Name: "one", Position: 0},
		{ID: "t2", Name: "two", Position: 1},
		{ID: "c11", Name: "one-a", Parent: "t1", Position: 0},
		{ID: "c12", Name: "one-b", Parent: "t1", Position: 1},
		{ID: "c21", Name: "two-a", Parent: "t2", Position: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tax
}

func TestTaxonomy_MembersAreSubtreeUnions(t *testing.T) {
	tax := testTaxonomy(t)
	placed := map[string][]string{"d1": {"c11", "c12"}, "d2": {"t1"}, "d3": {"c21", "c11"}}
	got := tax.Members(placed)
	want := map[string][]string{
		"t1": {"d1", "d2", "d3"}, "c11": {"d1", "d3"}, "c12": {"d1"}, "t2": {"d3"}, "c21": {"d3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("members %v, want %v", got, want)
	}
	if !reflect.DeepEqual(tax.Order(), []string{"t1", "c11", "c12", "t2", "c21"}) {
		t.Fatalf("order %v", tax.Order())
	}
}

func TestTaxonomy_LevelsLabelOnlyDocumentsBelowTheParent(t *testing.T) {
	tax := testTaxonomy(t)
	placed := map[string][]string{"d1": {"c11", "c12"}, "d2": {"t1"}, "d3": {"c21", "c11"}}
	lvs := tax.Levels(placed)
	// L1, then L2 under t1 only: t2 has a single child.
	if len(lvs) != 2 {
		t.Fatalf("got %d levels", len(lvs))
	}
	if want := map[string][]string{"d1": {"t1"}, "d2": {"t1"}, "d3": {"t1", "t2"}}; !reflect.DeepEqual(lvs[0].Labels, want) {
		t.Fatalf("L1 labels %v, want %v", lvs[0].Labels, want)
	}
	// d2 sits on t1 itself, not below it: absent from L2.
	if want := map[string][]string{"d1": {"c11", "c12"}, "d3": {"c11"}}; !reflect.DeepEqual(lvs[1].Labels, want) {
		t.Fatalf("L2 labels %v, want %v", lvs[1].Labels, want)
	}
}

func TestNewTaxonomy_RefusesBrokenTrees(t *testing.T) {
	for name, cs := range map[string][]Concept{
		"unknown parent": {{ID: "a", Parent: "zz"}},
		"duplicate":      {{ID: "a"}, {ID: "a"}},
		"cycle":          {{ID: "a", Parent: "b"}, {ID: "b", Parent: "a"}},
	} {
		if _, err := NewTaxonomy(cs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDocDate_FollowsTheRFCOrder(t *testing.T) {
	va := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	ca := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		doc  Doc
		src  string
		at   time.Time
	}{
		{"valid_at wins", Doc{RootValidAt: ptr(va.UnixNano()), SourceUpdatedAt: "2024-01-01T00:00:00Z", CreatedAt: ca.UnixNano()}, DateValidAt, va},
		{"source next", Doc{SourceUpdatedAt: "2024-01-02T03:04:05Z", CreatedAt: ca.UnixNano()}, DateSourceUpd, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"unparsable source falls to created_at", Doc{SourceUpdatedAt: "yesterday", CreatedAt: ca.UnixNano()}, DateCreatedAt, ca},
		{"created_at last", Doc{CreatedAt: ca.UnixNano()}, DateCreatedAt, ca},
		// valid_at = 0 is a real date (1970-01-01), not unset.
		{"valid_at zero is a date", Doc{RootValidAt: ptr(int64(0)), SourceUpdatedAt: "2024-01-01T00:00:00Z", CreatedAt: ca.UnixNano()}, DateValidAt, time.Unix(0, 0).UTC()},
	}
	for _, tc := range cases {
		at, src := DocDate(&tc.doc, time.Time{})
		if src != tc.src || !at.Equal(tc.at) {
			t.Errorf("%s: got %s %v, want %s %v", tc.name, src, at, tc.src, tc.at)
		}
	}
}

func TestDocDate_CreatedOnOrBeforeTheImportDayIsImportDated(t *testing.T) {
	day := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		src  string
	}{
		{"before the day", time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC), DateImport},
		{"late on the day", time.Date(2026, 7, 21, 23, 59, 0, 0, time.UTC), DateImport},
		{"the next day", time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC), DateCreatedAt},
	}
	for _, tc := range cases {
		_, src := DocDate(&Doc{CreatedAt: tc.at.UnixNano()}, day)
		if src != tc.src {
			t.Errorf("%s: got %s, want %s", tc.name, src, tc.src)
		}
	}
	va := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, src := DocDate(&Doc{RootValidAt: ptr(va.UnixNano()), CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).UnixNano()}, day); src != DateValidAt {
		t.Errorf("a valid_at is never import-dated, got %s", src)
	}
}

func TestDates_MonthsCountOnlyMembersNotDatedByTheImport(t *testing.T) {
	day := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	tax := testTaxonomy(t)
	docs := map[string]*Doc{}
	var ids []string
	for i, at := range []time.Time{
		time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC), // import
		time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC), // import
		time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC), // import
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
	} {
		id := fmt.Sprintf("d%d", i)
		docs[id] = &Doc{ID: id, CreatedAt: at.UnixNano()}
		ids = append(ids, id)
	}
	node := tax.Order()[0]
	r := Dates(tax, docs, map[string][]string{}, map[string][]string{node: ids}, day)
	n := r.Nodes[0]
	if n.Dated != 3 || n.Periods != 1 || n.Usable != 1 {
		t.Fatalf("got dated=%d months=%d usable=%d, want 3, 1, 1", n.Dated, n.Periods, n.Usable)
	}
	if r.All[DateImport] != 3 || r.All[DateCreatedAt] != 3 {
		t.Fatalf("sources = %v", r.All)
	}
}

func TestProvenance_NamesMethodClassifierAndVerdict(t *testing.T) {
	cases := []struct{ method, attrs, want string }{
		{"classify", `{"classified_by":"model","judge":{"verdict":"accept"}}`, "classify/model/judge:accept"},
		{"classify", `{"classified_by":"model","concept_fallback":true,"judge":{"verdict":"unsure"}}`, "classify/model+fallback/judge:unsure"},
		{"classify", `{"classified_by":"vote"}`, "classify/vote/judge:-"},
		{"manual", ``, "manual/-/judge:-"},
		{"manual", `not json`, "manual/-/judge:-"},
	}
	for _, tc := range cases {
		if got := provenance(tc.method, tc.attrs); got != tc.want {
			t.Errorf("provenance(%q, %q) = %q, want %q", tc.method, tc.attrs, got, tc.want)
		}
	}
}

func TestParsePgvector(t *testing.T) {
	v, err := parsePgvector("[1,2.5,-3e-05]")
	if err != nil || !reflect.DeepEqual(v, nodestats.Vector{1, 2.5, -3e-05}) {
		t.Fatalf("got %v %v", v, err)
	}
	for _, bad := range []string{"", "1,2", "[1,x]", "[]"} {
		if _, err := parsePgvector(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitAnnotationKey(t *testing.T) {
	for _, tc := range []struct {
		key, chunk, level string
		ok                bool
	}{
		{"doc.annotation:abc:document", "abc", "document", true},
		{"doc.annotation:abc:branch", "abc", "branch", true},
		{"doc.annotation:abc", "", "", false},
		{"doc.annotation:abc:", "", "", false},
		{"doc.chunk:abc:branch", "", "", false},
	} {
		c, l, ok := splitAnnotationKey(tc.key)
		if c != tc.chunk || l != tc.level || ok != tc.ok {
			t.Errorf("%q: got %q %q %v", tc.key, c, l, ok)
		}
	}
}

func TestQuality_MarksANodeWithManyOldAnnotationsUngradable(t *testing.T) {
	tax := testTaxonomy(t)
	docs := map[string]*Doc{
		"d1": {ID: "d1", DocVec: nodestats.Vector{1}, HasAnnotation: true, PromptVersion: 2},
		"d2": {ID: "d2", DocVec: nodestats.Vector{1}, HasAnnotation: true, PromptVersion: 1},
		"d3": {ID: "d3"},
	}
	q := Quality(tax, docs, map[string][]string{"c11": {"d1"}, "c12": {"d1", "d2"}, "t2": {"d3"}},
		Options{CurrentPromptVersion: 2, NonCurrentLimit: 0.10})
	got := map[string]NodeQuality{}
	for _, n := range q.Nodes {
		got[n.ID] = n
	}
	if !got["c11"].Gradable || got["c12"].Gradable || got["t2"].Gradable || got["c21"].Gradable {
		t.Fatalf("gradable flags wrong: %+v", got)
	}
	if n := got["t2"]; n.WithoutDocVec != 1 || n.NoAnnotation != 1 || n.NonCurrentShare != 1 {
		t.Fatalf("t2: %+v", n)
	}
	if q.Overall.Members != 3 || math.Abs(q.Overall.NonCurrentShare-2.0/3) > 1e-12 {
		t.Fatalf("overall: %+v", q.Overall)
	}
}

func TestSizeHistogram_BinsEveryConcept(t *testing.T) {
	tax := testTaxonomy(t)
	h := SizeHistogram(tax, map[string][]string{"t1": make([]string, 25), "c11": {"x"}, "c12": {"x", "y", "z"}})
	total := 0
	got := map[string]int{}
	for _, b := range h {
		total += b.Concepts
		got[b.Range] = b.Concepts
	}
	if total != 5 || got["0-0"] != 2 || got["1-2"] != 1 || got["3-4"] != 1 || got["20-49"] != 1 {
		t.Fatalf("histogram %v", h)
	}
}

func TestDefinitionText(t *testing.T) {
	got := DefinitionText(Concept{Name: "memory", Meaning: "What an agent remembers.", Alt: []string{"recall", "facts"}})
	if want := "memory: What an agent remembers. (also: recall, facts)"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestUnplaced_ExcludesOntologyAndVectorless(t *testing.T) {
	docs := map[string]*Doc{
		"p": {DocVec: nodestats.Vector{1}}, "o": {DocVec: nodestats.Vector{1}, Ontology: true},
		"n": {}, "u2": {DocVec: nodestats.Vector{1}}, "u1": {DocVec: nodestats.Vector{1}},
	}
	got := Unplaced(docs, map[string][]string{"p": {"c"}})
	if !sort.StringsAreSorted(got) || !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Fatalf("got %v", got)
	}
}

// A concept whose members cancel out has no centroid: it is reported without
// spread, exemplars or siblings, and the other concepts are still measured.
func TestGradingMaterial_SkipsAConceptWhoseMembersCancel(t *testing.T) {
	tax := testTaxonomy(t)
	docs := map[string]*Doc{
		"d1": {ID: "d1", DocVec: nodestats.Vector{1, 0}},
		"d2": {ID: "d2", DocVec: nodestats.Vector{-1, 0}},
		"d3": {ID: "d3", DocVec: nodestats.Vector{0, 1}},
	}
	members := tax.Members(map[string][]string{"d1": {"c12"}, "d2": {"c12"}, "d3": {"c11"}})
	got, err := GradingMaterial(tax, docs, members)
	if err != nil {
		t.Fatalf("GradingMaterial: %v", err)
	}
	byID := map[string]NodeMaterial{}
	for _, nm := range got {
		byID[nm.ID] = nm
	}
	c12 := byID["c12"]
	if c12.Count != 2 || c12.Spread != nil || c12.Exemplars != nil || c12.Siblings != nil {
		t.Errorf("cancelling concept: %+v, want count 2 and no spread, exemplars or siblings", c12)
	}
	c11 := byID["c11"]
	if c11.Spread == nil || len(c11.Exemplars) != 1 || c11.Siblings != nil {
		t.Errorf("sibling concept: %+v, want a spread, one exemplar and no centroid-less sibling", c11)
	}
	if byID["t1"].Spread == nil {
		t.Errorf("parent concept has no spread: %+v", byID["t1"])
	}
}
