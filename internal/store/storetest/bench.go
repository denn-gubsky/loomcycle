package storetest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// BenchmarkConfig parameterises the concurrent-runs benchmark below.
// Defaults match the v0.5.0 acceptance criterion (100 concurrent runs
// × 50 events each); operators tuning their deployment can override
// for higher counts.
type BenchmarkConfig struct {
	// Concurrency is the number of in-flight runs. Default 100.
	Concurrency int
	// EventsPerRun is the number of AppendEvent calls per run. Default
	// 50 — roughly mirrors a cv-adapter run's transcript size.
	EventsPerRun int
	// PayloadBytes is the size of each event's JSON payload. Default
	// 256.
	PayloadBytes int
}

// BenchmarkResult captures per-op latency stats + total throughput.
// Operators reading this from a doc want a quick read of "is this
// adapter fast enough for my workload" — we keep the schema small and
// human-readable rather than building a full histogram library.
type BenchmarkResult struct {
	Concurrency  int
	EventsPerRun int
	PayloadBytes int
	TotalRuns    int
	TotalEvents  int
	Wall         time.Duration
	RunsPerSec   float64
	EventsPerSec float64
	// Per-op latencies (across the union of CreateSession + CreateRun
	// + AppendEvent + FinishRun). We don't break out per-op stats
	// here — the storetest contract suite already proves correctness
	// of each op individually; the benchmark cares about aggregate
	// throughput at concurrency.
	LatencyP50 time.Duration
	LatencyP95 time.Duration
	LatencyP99 time.Duration
}

// RunConcurrencyBench drives `Concurrency` parallel goroutines against
// the supplied Store, each running one full session/run/event-loop/
// finish lifecycle. Returns a BenchmarkResult ready to plug into a
// doc table.
//
// The Store is supplied directly (vs through the storetest.Factory
// wrapper) so the same helper works from both `*testing.T` callers
// (regression / acceptance tests) and `*testing.B` callers (Go
// benchmarks). Caller is responsible for opening + closing the Store.
func RunConcurrencyBench(tb testing.TB, s store.Store, cfg BenchmarkConfig) BenchmarkResult {
	tb.Helper()
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 100
	}
	if cfg.EventsPerRun <= 0 {
		cfg.EventsPerRun = 50
	}
	if cfg.PayloadBytes <= 0 {
		cfg.PayloadBytes = 256
	}

	payload := make([]byte, cfg.PayloadBytes)
	for i := range payload {
		payload[i] = byte('a' + (i % 26))
	}

	// Pre-create one session for every goroutine to take CreateSession
	// out of the hot loop's contended path. A real workload mixes the
	// two; for the throughput measurement we want store ops, not
	// goroutine startup amortisation.
	ctx := context.Background()
	sessionIDs := make([]string, cfg.Concurrency)
	for i := 0; i < cfg.Concurrency; i++ {
		sess, err := s.CreateSession(ctx, "bench", "default", fmt.Sprintf("u_%d", i))
		if err != nil {
			tb.Fatalf("CreateSession: %v", err)
		}
		sessionIDs[i] = sess.ID
	}

	// Latency samples: pre-allocate to the expected count to avoid
	// realloc-while-appending under contention.
	totalOps := cfg.Concurrency * (cfg.EventsPerRun + 2) // +1 CreateRun, +1 FinishRun
	latencies := make([]time.Duration, 0, totalOps)
	var latMu sync.Mutex

	var (
		wg            sync.WaitGroup
		errCount      atomic.Uint64
		eventsTotal   atomic.Uint64
	)
	wg.Add(cfg.Concurrency)
	wallStart := time.Now()
	for i := 0; i < cfg.Concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			localLat := make([]time.Duration, 0, cfg.EventsPerRun+2)

			t0 := time.Now()
			run, err := s.CreateRun(ctx, sessionIDs[i], store.RunIdentity{
				AgentID: fmt.Sprintf("a_%d", i),
				UserID:  fmt.Sprintf("u_%d", i),
			})
			localLat = append(localLat, time.Since(t0))
			if err != nil {
				errCount.Add(1)
				return
			}

			for e := 0; e < cfg.EventsPerRun; e++ {
				t0 := time.Now()
				if err := s.AppendEvent(ctx, run.ID, "text", payload); err != nil {
					errCount.Add(1)
					return
				}
				localLat = append(localLat, time.Since(t0))
			}

			t0 = time.Now()
			if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
				store.Usage{InputTokens: 100, OutputTokens: 50, Model: "bench-model"}, ""); err != nil {
				errCount.Add(1)
				return
			}
			localLat = append(localLat, time.Since(t0))

			eventsTotal.Add(uint64(cfg.EventsPerRun))

			latMu.Lock()
			latencies = append(latencies, localLat...)
			latMu.Unlock()
		}(i)
	}
	wg.Wait()
	wall := time.Since(wallStart)

	if errCount.Load() > 0 {
		tb.Fatalf("%d operations errored under load", errCount.Load())
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}

	return BenchmarkResult{
		Concurrency:  cfg.Concurrency,
		EventsPerRun: cfg.EventsPerRun,
		PayloadBytes: cfg.PayloadBytes,
		TotalRuns:    cfg.Concurrency,
		TotalEvents:  int(eventsTotal.Load()),
		Wall:         wall,
		RunsPerSec:   float64(cfg.Concurrency) / wall.Seconds(),
		EventsPerSec: float64(eventsTotal.Load()) / wall.Seconds(),
		LatencyP50:   pct(0.50),
		LatencyP95:   pct(0.95),
		LatencyP99:   pct(0.99),
	}
}

// FormatResult renders the benchmark result as a one-line summary
// suitable for a doc table or stdout. The format is deliberately
// stable — operators copying numbers into change-management docs
// shouldn't have to deal with cosmetic drift release-to-release.
func FormatResult(r BenchmarkResult) string {
	return fmt.Sprintf(
		"runs=%d events/run=%d payload=%dB | wall=%s | %.0f runs/s, %.0f events/s | latency p50=%s p95=%s p99=%s",
		r.Concurrency, r.EventsPerRun, r.PayloadBytes,
		r.Wall.Round(time.Millisecond),
		r.RunsPerSec, r.EventsPerSec,
		r.LatencyP50.Round(time.Microsecond),
		r.LatencyP95.Round(time.Microsecond),
		r.LatencyP99.Round(time.Microsecond),
	)
}
