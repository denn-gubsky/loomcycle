package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeStore is a store.Store that ONLY implements the metrics
// methods. Unused methods inherit from the embedded interface; the
// interface is nil at construction, so any non-metrics call panics
// with a nil-pointer dereference. That's the desired test fence —
// if the sampler accidentally starts calling MemoryGet (etc.), the
// test will fail loudly rather than silently succeed.
//
// This pattern is robust against future Store interface additions:
// new methods that the sampler doesn't call require no test
// changes at all.
type fakeStore struct {
	store.Store // intentionally nil — unused methods nil-panic

	writes     []store.ProcessSample
	writeError error
}

func (f *fakeStore) MetricsWriteSample(_ context.Context, s store.ProcessSample) error {
	if f.writeError != nil {
		return f.writeError
	}
	f.writes = append(f.writes, s)
	return nil
}

func (f *fakeStore) MetricsSampleWindow(context.Context, time.Time, time.Time, int, string) ([]store.ProcessSample, string, error) {
	return nil, "", nil
}

func (f *fakeStore) MetricsRunSummary(context.Context, string) (store.MetricsRunWindow, error) {
	return store.MetricsRunWindow{}, nil
}

func (f *fakeStore) MetricsSweep(context.Context, time.Time) (int, error) {
	return 0, nil
}

// TestSampler_SkipsWhenIdle: no active runs → no write.
func TestSampler_SkipsWhenIdle(t *testing.T) {
	fs := &fakeStore{}
	sem := concurrency.New(8, 16, time.Second)
	s := New(fs, sem, Config{Interval: time.Second})
	if err := s.sampleOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("sampleOnce: %v", err)
	}
	if len(fs.writes) != 0 {
		t.Errorf("got %d writes, want 0 (idle)", len(fs.writes))
	}
}

// TestSampler_WritesWhenActive: at least one active run → one
// write with the correct active_runs/queued_runs fields.
func TestSampler_WritesWhenActive(t *testing.T) {
	fs := &fakeStore{}
	sem := concurrency.New(8, 16, time.Second)
	// Acquire 2 slots to drive active=2.
	ctx := context.Background()
	rel1, err := sem.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()
	rel2, err := sem.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	s := New(fs, sem, Config{Interval: time.Second})
	now := time.Now()
	if err := s.sampleOnce(ctx, now); err != nil {
		t.Fatalf("sampleOnce: %v", err)
	}
	if len(fs.writes) != 1 {
		t.Fatalf("got %d writes, want 1", len(fs.writes))
	}
	got := fs.writes[0]
	if got.ActiveRuns != 2 {
		t.Errorf("active_runs = %d, want 2", got.ActiveRuns)
	}
	if got.QueuedRuns != 0 {
		t.Errorf("queued_runs = %d, want 0", got.QueuedRuns)
	}
	if got.SampleID == "" {
		t.Error("SampleID empty")
	}
	if got.LoomcycleGoroutines <= 0 {
		t.Errorf("goroutines = %d, want > 0", got.LoomcycleGoroutines)
	}
	if got.LoomcycleHeapAlloc <= 0 {
		t.Errorf("heap_alloc = %d, want > 0", got.LoomcycleHeapAlloc)
	}
}

// TestSampler_GracefulStoreError: store returns error → sampler
// logs + counts the failure, doesn't panic. Subsequent ticks
// still attempt.
func TestSampler_GracefulStoreError(t *testing.T) {
	fs := &fakeStore{writeError: errors.New("simulated DB outage")}
	sem := concurrency.New(8, 16, time.Second)
	ctx := context.Background()
	rel, _ := sem.Acquire(ctx)
	defer rel()
	logs := 0
	s := New(fs, sem, Config{
		Interval: time.Second,
		Logf:     func(string, ...any) { logs++ },
	})
	if err := s.sampleOnce(ctx, time.Now()); err == nil {
		t.Fatal("expected error from sampleOnce, got nil")
	}
	if s.failures != 1 {
		t.Errorf("failures = %d after first error, want 1", s.failures)
	}
	if logs != 1 {
		t.Errorf("log count = %d after first error, want 1 (loud-on-first)", logs)
	}
	// Subsequent failures up to N=9 should NOT log (rate-limit).
	for i := 0; i < 8; i++ {
		_ = s.sampleOnce(ctx, time.Now())
	}
	if logs != 1 {
		t.Errorf("log count = %d after 9 errors, want still 1 (rate-limited)", logs)
	}
	// 10th failure logs again.
	_ = s.sampleOnce(ctx, time.Now())
	if logs != 2 {
		t.Errorf("log count = %d after 10 errors, want 2", logs)
	}
}

// TestSampler_NilStore: sampler with no store wired (e.g., metrics
// enabled but DB initialisation deferred). Must not panic; no
// writes.
func TestSampler_NilStore(t *testing.T) {
	sem := concurrency.New(8, 16, time.Second)
	ctx := context.Background()
	rel, _ := sem.Acquire(ctx)
	defer rel()
	s := New(nil, sem, Config{Interval: time.Second})
	if err := s.sampleOnce(ctx, time.Now()); err != nil {
		t.Errorf("nil-store sampleOnce returned err: %v", err)
	}
}

// TestSampler_RecoveryResetsCounter: a successful write after a
// failure run resets the failure counter.
func TestSampler_RecoveryResetsCounter(t *testing.T) {
	fs := &fakeStore{writeError: errors.New("transient")}
	sem := concurrency.New(8, 16, time.Second)
	ctx := context.Background()
	rel, _ := sem.Acquire(ctx)
	defer rel()
	s := New(fs, sem, Config{
		Interval: time.Second,
		Logf:     func(string, ...any) {},
	})
	// Two failures.
	_ = s.sampleOnce(ctx, time.Now())
	_ = s.sampleOnce(ctx, time.Now())
	if s.failures != 2 {
		t.Errorf("failures = %d, want 2", s.failures)
	}
	// Clear the simulated outage.
	fs.writeError = nil
	if err := s.sampleOnce(ctx, time.Now()); err != nil {
		t.Fatalf("post-recovery sampleOnce returned err: %v", err)
	}
	if s.failures != 0 {
		t.Errorf("failures = %d after recovery, want 0", s.failures)
	}
}
