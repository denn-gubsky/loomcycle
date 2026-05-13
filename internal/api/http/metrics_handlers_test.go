package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// metricsFixture wires a Server with a real sqlite store + an
// optionally-attached metrics sampler. Returns the server, the
// store (so tests can pre-seed rows), and a cleanup func.
func metricsFixture(t *testing.T, attachSampler bool) (*Server, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Server only needs the store + sampler for the metrics
	// endpoints; the rest of the Server fields are unused. We set
	// `store` directly via the struct rather than calling New(),
	// because New() requires a fully-wired provider resolver etc.
	srv := &Server{store: st}
	if attachSampler {
		srv.metricsSampler = metrics.New(st, concurrency.New(8, 16, time.Second), metrics.Config{
			Interval: time.Second,
		})
	}
	return srv, st
}

// TestMetricsEndpoints_DisabledReturns503 — no sampler wired, all
// three endpoints return 503 with the standard envelope.
func TestMetricsEndpoints_DisabledReturns503(t *testing.T) {
	srv, _ := metricsFixture(t, false)
	cases := []struct {
		name string
		path string
		fn   http.HandlerFunc
		// some handlers need PathValue
		pathValues map[string]string
	}{
		{"samples", "/v1/_metrics/samples?since=2026-05-13T00:00:00Z", srv.handleMetricsSamples, nil},
		{"run", "/v1/_metrics/runs/r_nope", srv.handleMetricsRunSummary, map[string]string{"run_id": "r_nope"}},
		{"summary", "/v1/_metrics/summary?period=1h", srv.handleMetricsSummary, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			for k, v := range tc.pathValues {
				req.SetPathValue(k, v)
			}
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleMetricsSamples_RoundTrip — sampler attached, store
// pre-loaded with 3 samples. GET returns them in time order with
// next_cursor empty.
func TestHandleMetricsSamples_RoundTrip(t *testing.T) {
	srv, st := metricsFixture(t, true)
	ctx := t.Context()
	base := time.Now().UTC().Add(-1 * time.Minute).Truncate(time.Millisecond)
	for i := 0; i < 3; i++ {
		sa := store.ProcessSample{
			SampleID:           store.MintSampleID(base.Add(time.Duration(i) * time.Second)),
			SampledAt:          base.Add(time.Duration(i) * time.Second),
			ActiveRuns:         1,
			QueuedRuns:         0,
			LoomcycleRSSBytes:  int64(100+i) << 20,
			LoomcycleHeapAlloc: 50 << 20,
		}
		if err := st.MetricsWriteSample(ctx, sa); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("GET", "/v1/_metrics/samples?since="+base.Add(-1*time.Second).Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	srv.handleMetricsSamples(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Samples    []store.ProcessSample `json:"samples"`
		NextCursor string                `json:"next_cursor"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Samples) != 3 {
		t.Errorf("got %d samples, want 3", len(resp.Samples))
	}
	if resp.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty", resp.NextCursor)
	}
}

// TestHandleMetricsSamples_MissingSince — 400 with helpful error.
func TestHandleMetricsSamples_MissingSince(t *testing.T) {
	srv, _ := metricsFixture(t, true)
	req := httptest.NewRequest("GET", "/v1/_metrics/samples", nil)
	rec := httptest.NewRecorder()
	srv.handleMetricsSamples(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandleMetricsRunSummary_NotFound — unknown run_id → 404.
func TestHandleMetricsRunSummary_NotFound(t *testing.T) {
	srv, _ := metricsFixture(t, true)
	req := httptest.NewRequest("GET", "/v1/_metrics/runs/r_nope", nil)
	req.SetPathValue("run_id", "r_nope")
	rec := httptest.NewRecorder()
	srv.handleMetricsRunSummary(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleMetricsRunSummary_HappyPath — a run + 2 overlapping
// samples → 200 with non-zero peak + count.
func TestHandleMetricsRunSummary_HappyPath(t *testing.T) {
	srv, st := metricsFixture(t, true)
	ctx := t.Context()
	sess, _ := st.CreateSession(ctx, "t", "default", "")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_test"})
	time.Sleep(2 * time.Millisecond)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i, rss := range []int64{100 << 20, 150 << 20} {
		sa := store.ProcessSample{
			SampleID:          store.MintSampleID(now.Add(time.Duration(i) * time.Millisecond)),
			SampledAt:         now.Add(time.Duration(i) * time.Millisecond),
			ActiveRuns:        1,
			LoomcycleRSSBytes: rss,
		}
		if err := st.MetricsWriteSample(ctx, sa); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/_metrics/runs/"+run.ID, nil)
	req.SetPathValue("run_id", run.ID)
	rec := httptest.NewRecorder()
	srv.handleMetricsRunSummary(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got store.MetricsRunWindow
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.SampleCount != 2 {
		t.Errorf("SampleCount = %d, want 2", got.SampleCount)
	}
	if got.PeakRSSBytes != 150<<20 {
		t.Errorf("PeakRSSBytes = %d, want %d", got.PeakRSSBytes, int64(150<<20))
	}
}

// TestHandleMetricsSummary_Period1h — 12 samples spread over an
// hour at 5-min intervals → 12 buckets (one per 5-min span)
// with sample_count=1 each.
func TestHandleMetricsSummary_Period1h(t *testing.T) {
	srv, st := metricsFixture(t, true)
	ctx := t.Context()
	now := time.Now().UTC()
	for i := 0; i < 12; i++ {
		sampleTime := now.Add(-time.Duration(i+1) * 5 * time.Minute)
		sa := store.ProcessSample{
			SampleID:            store.MintSampleID(sampleTime),
			SampledAt:           sampleTime,
			ActiveRuns:          1 + i%3,
			LoomcycleRSSBytes:   int64(100+i) << 20,
			LoomcycleCPUPctX100: 1000 + i*100,
		}
		if err := st.MetricsWriteSample(ctx, sa); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("GET", "/v1/_metrics/summary?period=1h", nil)
	rec := httptest.NewRecorder()
	srv.handleMetricsSummary(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Period  string                 `json:"period"`
		Buckets []metricsSummaryBucket `json:"buckets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Period != "1h" {
		t.Errorf("period = %q, want 1h", resp.Period)
	}
	if len(resp.Buckets) != 12 {
		t.Errorf("got %d buckets, want 12 (5-min bucket × 1h)", len(resp.Buckets))
	}
	// At least one bucket should have a non-zero sample count.
	total := 0
	for _, b := range resp.Buckets {
		total += b.SampleCount
	}
	if total == 0 {
		t.Error("no bucket recorded any samples")
	}
}

// TestHandleMetricsSummary_InvalidPeriod — 400 on unknown period.
func TestHandleMetricsSummary_InvalidPeriod(t *testing.T) {
	srv, _ := metricsFixture(t, true)
	req := httptest.NewRequest("GET", "/v1/_metrics/summary?period=42h", nil)
	rec := httptest.NewRecorder()
	srv.handleMetricsSummary(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPercentile — sanity-check the helper used by summary.
func TestPercentile_Nearest(t *testing.T) {
	cases := []struct {
		in   []int
		p    int
		want int
	}{
		{nil, 95, 0},
		{[]int{100}, 95, 100},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 95, 10},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 5},
	}
	for _, tc := range cases {
		got := percentile(tc.in, tc.p)
		if got != tc.want {
			t.Errorf("percentile(%v, %d) = %d, want %d", tc.in, tc.p, got, tc.want)
		}
	}
}
