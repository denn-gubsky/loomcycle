package main

// metrics.go — retrieval metrics over the LoCoMo evidence answer key.
//
// precision@k and recall@k use the same definitions as the in-tree
// memory-eval harness (precision = hits/k, recall = hits/|expected|) so the
// two are directly comparable. mrr and hit-rate are added because they are
// what a memory system's consumer actually feels: whether the supporting turn
// came back at all, and how far down.

import (
	"math"
	"sort"
	"time"
)

// QueryResult is one probe's outcome.
type QueryResult struct {
	Question  string   `json:"question"`
	Category  int      `json:"category"`
	Expected  []string `json:"expected"`
	Retrieved []string `json:"retrieved"`
	Hits      int      `json:"hits"`
	// FirstHitRank is the 1-based rank of the first expected key, 0 if none.
	FirstHitRank int           `json:"first_hit_rank"`
	Latency      time.Duration `json:"-"`
	LatencyMs    float64       `json:"latency_ms"`
}

// Score computes one query's outcome against its answer key.
func Score(q Query, retrieved []string, topK int, latency time.Duration) QueryResult {
	// GROUND TRUTH IS TURN-LEVEL, ALWAYS. LoCoMo's qa.evidence names turn ids, so a
	// coarser row (RFC CM-1's session unit) is credited when it CONTAINS an expected
	// turn — its key maps to the same session. Without this a session-level arm would
	// score zero against every question and the comparison would measure nothing.
	//
	// Recall stays denominated in EXPECTED TURNS, not in rows: one session row that
	// covers three expected turns is three hits, exactly as three turn rows would be.
	// Counting it as one would make the coarse arm look worse purely for being
	// coarse, and counting the row once as a full hit would make it look better.
	want := make(map[string]bool, len(q.Expected))
	bySession := make(map[string][]string, len(q.Expected))
	for _, e := range q.Expected {
		want[e] = true
		if sk := SessionOfDiaID(e); sk != "" {
			bySession[sk] = append(bySession[sk], e)
		}
	}
	res := QueryResult{
		Question: q.Question, Category: q.Category,
		Expected: q.Expected, Retrieved: retrieved,
		Latency: latency, LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}
	if topK > 0 && len(retrieved) > topK {
		retrieved = retrieved[:topK]
		res.Retrieved = retrieved
	}
	credited := make(map[string]bool, len(q.Expected))
	for i, key := range retrieved {
		// A session row credits every expected turn it contains, each only once.
		if covered, ok := bySession[key]; ok && !want[key] {
			fresh := 0
			for _, e := range covered {
				if !credited[e] {
					credited[e] = true
					fresh++
				}
			}
			if fresh > 0 {
				res.Hits += fresh
				if res.FirstHitRank == 0 {
					res.FirstHitRank = i + 1
				}
			}
			continue
		}
		if want[key] && !credited[key] {
			credited[key] = true
			res.Hits++
			if res.FirstHitRank == 0 {
				res.FirstHitRank = i + 1
			}
		}
	}
	return res
}

// Stats aggregates a set of query results.
type Stats struct {
	Label        string  `json:"label"`
	Queries      int     `json:"queries"`
	RecallAtK    float64 `json:"recall_at_k"`
	PrecisionAtK float64 `json:"precision_at_k"`
	MRR          float64 `json:"mrr"`
	HitRate      float64 `json:"hit_rate"`
	LatencyP50Ms float64 `json:"latency_p50_ms"`
	LatencyP99Ms float64 `json:"latency_p99_ms"`
}

// Aggregate computes the metric set over results at retrieval depth k.
func Aggregate(label string, results []QueryResult, k int) Stats {
	st := Stats{Label: label, Queries: len(results)}
	if len(results) == 0 {
		return st
	}
	var sumRecall, sumPrecision, sumRR float64
	var hit int
	lat := make([]time.Duration, 0, len(results))
	for _, r := range results {
		if len(r.Expected) > 0 {
			sumRecall += float64(r.Hits) / float64(len(r.Expected))
		}
		if k > 0 {
			sumPrecision += float64(r.Hits) / float64(k)
		}
		if r.FirstHitRank > 0 {
			sumRR += 1 / float64(r.FirstHitRank)
			hit++
		}
		lat = append(lat, r.Latency)
	}
	n := float64(len(results))
	st.RecallAtK = sumRecall / n
	st.PrecisionAtK = sumPrecision / n
	st.MRR = sumRR / n
	st.HitRate = float64(hit) / n
	st.LatencyP50Ms = percentileMs(lat, 0.50)
	st.LatencyP99Ms = percentileMs(lat, 0.99)
	return st
}

// ByCategory aggregates per LoCoMo category, ordered by category id so the
// report is stable run to run.
func ByCategory(results []QueryResult, k int) []Stats {
	buckets := map[int][]QueryResult{}
	for _, r := range results {
		buckets[r.Category] = append(buckets[r.Category], r)
	}
	cats := make([]int, 0, len(buckets))
	for c := range buckets {
		cats = append(cats, c)
	}
	sort.Ints(cats)
	out := make([]Stats, 0, len(cats))
	for _, c := range cats {
		out = append(out, Aggregate(CategoryName(c), buckets[c], k))
	}
	return out
}

// percentileMs mirrors the memory-eval harness's percentile so latency numbers
// from the two are read the same way.
func percentileMs(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000.0
}
