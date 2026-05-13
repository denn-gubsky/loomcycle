package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// metricsEndpointsEnabled returns true when the metrics sampler is
// wired AND the store backend is non-nil. False sends a 503 with
// a one-line operator-facing explanation.
func (s *Server) metricsEndpointsEnabled() bool {
	return s.metricsSampler != nil && s.store != nil
}

// writeMetricsDisabled emits the standard 503 envelope when the
// metrics endpoints are queried in a deployment where they're not
// enabled. The body's `enable_hint` points operators at the env var
// so the failure mode is self-explanatory.
func writeMetricsDisabled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "metrics sampler not enabled",
		"enable_hint": "set LOOMCYCLE_METRICS_ENABLED=1 and restart loomcycle",
	})
}

// handleMetricsSamples implements `GET /v1/_metrics/samples` —
// raw process_samples rows within a time window.
//
// Query params:
//   - since   RFC3339, required
//   - until   RFC3339, optional (defaults to now)
//   - limit   int 1..1000, optional (default 200)
//   - cursor  opaque, optional (from a previous response's next_cursor)
func (s *Server) handleMetricsSamples(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEndpointsEnabled() {
		writeMetricsDisabled(w)
		return
	}
	q := r.URL.Query()
	sinceStr := q.Get("since")
	if sinceStr == "" {
		http.Error(w, `{"error":"missing required query param: since (RFC3339)"}`, http.StatusBadRequest)
		return
	}
	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		http.Error(w, `{"error":"invalid since: must be RFC3339"}`, http.StatusBadRequest)
		return
	}
	until := time.Now().UTC()
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, `{"error":"invalid until: must be RFC3339"}`, http.StatusBadRequest)
			return
		}
		until = t
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, `{"error":"invalid limit: must be positive integer (1..1000)"}`, http.StatusBadRequest)
			return
		}
		limit = n
	}
	cursor := q.Get("cursor")

	samples, nextCursor, err := s.store.MetricsSampleWindow(r.Context(), since, until, limit, cursor)
	if err != nil {
		http.Error(w, `{"error":"failed to query samples"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"samples":     samples,
		"next_cursor": nextCursor,
	})
}

// handleMetricsRunSummary implements `GET /v1/_metrics/runs/{run_id}` —
// peak / mean RSS + max CPU% from samples overlapping the run's
// [started_at, COALESCE(completed_at, now)] window.
//
// 404 when the run_id doesn't exist. 200 with SampleCount=0 when
// the run exists but had no overlapping samples (in-flight run
// that hasn't ticked yet, or metrics disabled during the run).
func (s *Server) handleMetricsRunSummary(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEndpointsEnabled() {
		writeMetricsDisabled(w)
		return
	}
	runID := r.PathValue("run_id")
	if runID == "" {
		http.Error(w, `{"error":"missing run_id"}`, http.StatusBadRequest)
		return
	}
	summary, err := s.store.MetricsRunSummary(r.Context(), runID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to compute run summary"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
}

// metricsSummaryBucket is one aggregated time bucket in the
// /v1/_metrics/summary response.
type metricsSummaryBucket struct {
	At            time.Time `json:"at"`
	MeanRSSBytes  int64     `json:"mean_rss_bytes"`
	MaxRSSBytes   int64     `json:"max_rss_bytes"`
	P95CPUPctX100 int       `json:"p95_cpu_pct_x100"`
	ActiveRunsMax int       `json:"active_runs_max"`
	SampleCount   int       `json:"sample_count"`
}

// handleMetricsSummary implements `GET /v1/_metrics/summary` —
// aggregated buckets over a fixed period.
//
// Query params:
//   - period: "1h" | "24h" | "7d" (default "1h")
//
// The handler fetches samples from MetricsSampleWindow and
// aggregates in-process. For v0.8.x scale (≤2016 rows / 7-day
// period at default 5s interval) this is sub-millisecond. A
// dedicated SQL GROUP BY path can replace this in v0.9.x when
// load justifies it.
func (s *Server) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEndpointsEnabled() {
		writeMetricsDisabled(w)
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "1h"
	}
	var (
		windowDur  time.Duration
		bucketSize time.Duration
	)
	switch period {
	case "1h":
		windowDur, bucketSize = 1*time.Hour, 5*time.Minute
	case "24h":
		windowDur, bucketSize = 24*time.Hour, 1*time.Hour
	case "7d":
		windowDur, bucketSize = 7*24*time.Hour, 6*time.Hour
	default:
		http.Error(w, `{"error":"invalid period: must be 1h | 24h | 7d"}`, http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	since := now.Add(-windowDur)
	// Fetch ALL samples in the window. Cap of 1000 per page is
	// applied by the store; loop to drain if more.
	var all []store.ProcessSample
	cursor := ""
	for {
		batch, next, err := s.store.MetricsSampleWindow(r.Context(), since, now, 1000, cursor)
		if err != nil {
			http.Error(w, `{"error":"failed to query samples"}`, http.StatusInternalServerError)
			return
		}
		all = append(all, batch...)
		if next == "" {
			break
		}
		cursor = next
		// Safety: cap total rows fetched at 100k to prevent a
		// runaway query from consuming all memory.
		if len(all) >= 100000 {
			break
		}
	}

	buckets := summariseIntoBuckets(all, since, now, bucketSize)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"period":  period,
		"buckets": buckets,
	})
}

// summariseIntoBuckets groups samples into fixed-size time buckets
// and computes per-bucket aggregates: mean/max RSS, max active_runs,
// p95 CPU%, and sample count.
//
// Empty buckets (no samples in their window) are still emitted with
// zero-valued fields so a consumer plotting a timeline sees the
// gaps explicitly.
func summariseIntoBuckets(samples []store.ProcessSample, since, until time.Time, bucketSize time.Duration) []metricsSummaryBucket {
	numBuckets := int(until.Sub(since) / bucketSize)
	if numBuckets <= 0 {
		numBuckets = 1
	}
	type bucketAgg struct {
		rssSum  int64
		rssMax  int64
		runsMax int
		cpuList []int
		count   int
	}
	aggs := make([]bucketAgg, numBuckets)
	for _, s := range samples {
		idx := int(s.SampledAt.Sub(since) / bucketSize)
		if idx < 0 || idx >= numBuckets {
			continue
		}
		aggs[idx].rssSum += s.LoomcycleRSSBytes
		if s.LoomcycleRSSBytes > aggs[idx].rssMax {
			aggs[idx].rssMax = s.LoomcycleRSSBytes
		}
		if s.ActiveRuns > aggs[idx].runsMax {
			aggs[idx].runsMax = s.ActiveRuns
		}
		aggs[idx].cpuList = append(aggs[idx].cpuList, s.LoomcycleCPUPctX100)
		aggs[idx].count++
	}
	out := make([]metricsSummaryBucket, numBuckets)
	for i := 0; i < numBuckets; i++ {
		bucket := metricsSummaryBucket{
			At:            since.Add(time.Duration(i) * bucketSize),
			SampleCount:   aggs[i].count,
			MaxRSSBytes:   aggs[i].rssMax,
			ActiveRunsMax: aggs[i].runsMax,
		}
		if aggs[i].count > 0 {
			bucket.MeanRSSBytes = aggs[i].rssSum / int64(aggs[i].count)
			bucket.P95CPUPctX100 = percentile(aggs[i].cpuList, 95)
		}
		out[i] = bucket
	}
	return out
}

// percentile returns the p-th percentile of a slice of ints
// (rounded to the nearest sample). Used for p95 CPU% in the
// summary buckets. Empty slice → 0.
func percentile(values []int, p int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	sort.Ints(sorted)
	// Nearest-rank percentile: index = ceil(p/100 * n) - 1.
	idx := (p*len(sorted) + 99) / 100
	if idx > 0 {
		idx--
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
