// Package metrics implements the v0.8.x process-resource sampler:
// a background goroutine that periodically captures loomcycle's
// own CPU + memory usage (and optionally system-wide CPU/mem)
// WHILE at least one agent run is active, and writes a row to the
// process_samples table for later correlation with run records.
//
// When no agent runs are active, the sampler skips the tick — no
// /proc reads, no DB writes — so the idle-runtime cost is the
// per-interval call to sem.Stats().
//
// The HTTP API at /v1/_metrics/* reads the persisted samples.
package metrics

import (
	"context"
	"log"
	"runtime"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config tunes the sampler. Operators set via env vars; the
// Run() loop reads from the Sampler.cfg snapshot.
type Config struct {
	// Interval is the tick rate. Default 5s when zero; the
	// config-load layer rejects values below 1s to prevent
	// accidental write-storms.
	Interval time.Duration
	// CollectSystem enables /proc/stat + /proc/meminfo reads
	// (Linux only; ignored silently on other platforms).
	CollectSystem bool
	// ReplicaID stamps each sample so a shared (cluster-mode)
	// process_samples table can be split per replica. Empty in
	// single-replica mode. Sourced from LOOMCYCLE_REPLICA_ID.
	ReplicaID string
	// Logf is the log sink. nil → log.Printf.
	Logf func(string, ...any)
}

// Sampler is the background-worker handle. Construct via New;
// drive via Run(ctx).
type Sampler struct {
	cfg     Config
	store   store.Store
	sem     *concurrency.Semaphore
	prevCPU cpuSnapshot
	// failures is the consecutive-STORE-WRITE-error counter.
	// Sampler logs loudly on the first failure, then every 10th to
	// avoid log flood on a wedged disk / Postgres pool. /proc-read
	// errors are environmental (hardened-container shapes), NOT
	// store-write errors — they're tracked separately by
	// procReadFailureLogged so they don't pollute this signal.
	failures int
	// procReadFailureLogged is set the first time readProcMetrics
	// returns an error. On hardened containers (gVisor, etc.) or
	// CI runners where /proc/self/status lacks VmRSS, the proc read
	// fails every tick — we log ONCE at first occurrence and then
	// silently zero the affected fields. Matches proc_linux.go's
	// "log once, continue" contract.
	procReadFailureLogged bool
}

// New constructs a Sampler. The store may be nil (writes will be
// skipped with a single warning), but the semaphore must not be —
// it's the idle gate.
func New(st store.Store, sem *concurrency.Semaphore, cfg Config) *Sampler {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Sampler{cfg: cfg, store: st, sem: sem}
}

// Run blocks until ctx is done, sampling once per cfg.Interval
// while at least one agent run is active.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			_ = s.sampleOnce(ctx, now)
		}
	}
}

// sampleOnce performs one tick of the sampler loop. Extracted
// from Run for deterministic unit tests. Returns an error from
// the store write path (if any); the caller logs + continues
// (errors are NOT fatal — a transient store outage shouldn't
// crash loomcycle).
func (s *Sampler) sampleOnce(ctx context.Context, now time.Time) error {
	// 1. Idle gate. If nothing is running AND nothing is queued,
	//    skip — no /proc read, no DB write.
	active, queued := 0, 0
	if s.sem != nil {
		st := s.sem.Stats()
		active, queued = st.Active, st.Queued
	}
	if active == 0 && queued == 0 {
		return nil
	}

	// 2. Always-cheap measurements (Go heap + goroutines).
	var rt runtime.MemStats
	runtime.ReadMemStats(&rt)
	sample := store.ProcessSample{
		SampleID:            store.MintSampleID(now),
		ReplicaID:           s.cfg.ReplicaID,
		SampledAt:           now.UTC(),
		ActiveRuns:          active,
		QueuedRuns:          queued,
		LoomcycleHeapAlloc:  int64(rt.HeapAlloc),
		LoomcycleHeapInuse:  int64(rt.HeapInuse),
		LoomcycleGoroutines: runtime.NumGoroutine(),
	}

	// 3. Linux-only /proc reads (RSS + per-process CPU + optional
	//    system-wide). Errors here are SOFT — we still write the
	//    row with the cheap fields, leaving RSS/CPU as zero. We log
	//    ONCE per program lifetime on the first failure
	//    (hardened-container /proc shapes fail every tick; logging
	//    each one would flood). Goes through log.Printf directly,
	//    NOT cfg.Logf, so this environmental warning is decoupled
	//    from the actionable store-write backoff signal that tests
	//    drive via cfg.Logf.
	if ProcMetricsAvailable {
		pm, nextSnap, err := readProcMetrics(s.cfg.CollectSystem, s.prevCPU)
		if err != nil {
			if !s.procReadFailureLogged {
				log.Printf("metrics: /proc read failed: %v (continuing with zero RSS/CPU; further proc errors suppressed)", err)
				s.procReadFailureLogged = true
			}
		} else {
			sample.LoomcycleRSSBytes = pm.rssBytes
			sample.LoomcycleCPUPctX100 = pm.cpuPctX100
			sample.SystemCPUPctX100 = pm.systemCPUPctX100
			sample.SystemMemUsedMB = pm.systemMemUsedMB
			sample.SystemMemAvailableMB = pm.systemMemAvailableMB
			s.prevCPU = nextSnap
		}
	}

	// 4. Persist. Soft-failure on store outage.
	if s.store == nil {
		return nil
	}
	if err := s.store.MetricsWriteSample(ctx, sample); err != nil {
		s.recordFailure(err)
		return err
	}
	// Successful write resets the failure counter — the sampler
	// is back to a healthy state.
	if s.failures > 0 {
		s.cfg.Logf("metrics: write recovered after %d consecutive failures", s.failures)
		s.failures = 0
	}
	return nil
}

// recordFailure increments the failure counter and emits a log
// line at first failure and every 10th thereafter — preventing
// log floods on a wedged disk or disconnected Postgres pool while
// still surfacing the problem at startup.
func (s *Sampler) recordFailure(err error) {
	s.failures++
	if s.failures == 1 || s.failures%10 == 0 {
		s.cfg.Logf("metrics: write failed (consecutive=%d): %v", s.failures, err)
	}
}
