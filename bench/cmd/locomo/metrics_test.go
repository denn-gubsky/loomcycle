package main

import (
	"math"
	"testing"
	"time"
)

func nearly(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func TestScore_CountsHitsAndTheRankOfTheFirstOne(t *testing.T) {
	q := Query{Question: "q", Category: 4, Expected: []string{"D1:2", "D1:5"}}
	res := Score(q, []string{"D1:9", "D1:2", "D1:7", "D1:5"}, 10, 3*time.Millisecond)
	if res.Hits != 2 {
		t.Errorf("Hits = %d, want 2", res.Hits)
	}
	if res.FirstHitRank != 2 {
		t.Errorf("FirstHitRank = %d, want 2 (1-based)", res.FirstHitRank)
	}
	if res.LatencyMs != 3 {
		t.Errorf("LatencyMs = %v, want 3", res.LatencyMs)
	}
}

// TestScore_IgnoresHitsBelowTopK — a store that returns more than top_k rows
// must not be credited for the overflow, or the depth the metric claims and the
// depth it measures diverge.
func TestScore_IgnoresHitsBelowTopK(t *testing.T) {
	q := Query{Expected: []string{"D1:5"}}
	res := Score(q, []string{"a", "b", "c", "D1:5"}, 3, 0)
	if res.Hits != 0 {
		t.Errorf("Hits = %d, want 0 — D1:5 sits at rank 4 with top_k=3", res.Hits)
	}
	if len(res.Retrieved) != 3 {
		t.Errorf("Retrieved kept %d rows, want it truncated to 3", len(res.Retrieved))
	}
}

func TestScore_MissIsZeroNotAnError(t *testing.T) {
	res := Score(Query{Expected: []string{"D1:1"}}, []string{"x", "y"}, 5, 0)
	if res.Hits != 0 || res.FirstHitRank != 0 {
		t.Errorf("miss scored Hits=%d FirstHitRank=%d, want 0/0", res.Hits, res.FirstHitRank)
	}
}

// TestAggregate_RecallDividesByExpectedAndPrecisionByK mirrors the in-tree
// memory-eval definitions so the two harnesses' numbers mean the same thing.
func TestAggregate_RecallDividesByExpectedAndPrecisionByK(t *testing.T) {
	results := []QueryResult{
		// 1 of 2 expected found, at rank 1.
		{Category: 4, Expected: []string{"a", "b"}, Retrieved: []string{"a"}, Hits: 1, FirstHitRank: 1},
		// 0 of 1 found.
		{Category: 4, Expected: []string{"c"}, Retrieved: []string{"z"}, Hits: 0},
	}
	st := Aggregate("t", results, 10)
	nearly(t, "recall@k", st.RecallAtK, (0.5+0.0)/2)
	nearly(t, "precision@k", st.PrecisionAtK, (1.0/10+0.0)/2)
	nearly(t, "mrr", st.MRR, (1.0+0.0)/2)
	nearly(t, "hit_rate", st.HitRate, 0.5)
	if st.Queries != 2 {
		t.Errorf("Queries = %d, want 2", st.Queries)
	}
}

func TestAggregate_MRRRewardsTheHigherRank(t *testing.T) {
	high := Aggregate("high", []QueryResult{{Expected: []string{"a"}, Hits: 1, FirstHitRank: 1}}, 10)
	low := Aggregate("low", []QueryResult{{Expected: []string{"a"}, Hits: 1, FirstHitRank: 5}}, 10)
	if !(high.MRR > low.MRR) {
		t.Errorf("MRR did not reward rank: rank1=%v rank5=%v", high.MRR, low.MRR)
	}
	// Both found the row, so hit-rate cannot tell them apart — which is why MRR
	// is reported alongside it.
	nearly(t, "hit_rate parity", high.HitRate, low.HitRate)
}

func TestAggregate_EmptyIsZeroNotNaN(t *testing.T) {
	st := Aggregate("empty", nil, 10)
	for label, v := range map[string]float64{
		"recall": st.RecallAtK, "precision": st.PrecisionAtK, "mrr": st.MRR, "hit": st.HitRate,
	} {
		if math.IsNaN(v) || v != 0 {
			t.Errorf("%s = %v, want 0", label, v)
		}
	}
}

func TestByCategory_OrdersByCategoryIDForAStableReport(t *testing.T) {
	results := []QueryResult{
		{Category: 4, Expected: []string{"a"}, Hits: 1, FirstHitRank: 1},
		{Category: 1, Expected: []string{"a"}, Hits: 1, FirstHitRank: 1},
		{Category: 2, Expected: []string{"a"}, Hits: 0},
	}
	got := ByCategory(results, 10)
	want := []string{"multi-hop", "temporal", "single-hop"}
	if len(got) != len(want) {
		t.Fatalf("got %d buckets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Label != want[i] {
			t.Errorf("bucket %d = %q, want %q", i, got[i].Label, want[i])
		}
	}
}

func TestCategoryName_UnknownRendersNumericallyRatherThanMislabelling(t *testing.T) {
	if got := CategoryName(9); got != "category-9" {
		t.Errorf("CategoryName(9) = %q, want %q", got, "category-9")
	}
}

func TestPercentileMs_P50AndP99(t *testing.T) {
	ds := []time.Duration{}
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	nearly(t, "p50", percentileMs(ds, 0.50), 50)
	nearly(t, "p99", percentileMs(ds, 0.99), 99)
}
