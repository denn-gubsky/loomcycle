package http

import (
	"context"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
)

// Throughput exposes the per-(provider, model) speed estimate (RFC DT) so the
// Context tool can report the current model's slowdown. nil when
// timeout_scaling.mode is off; the estimator's methods are nil-safe.
func (s *Server) Throughput() *throughput.Estimator { return s.throughput }

// SeedThroughput replays the ledger's latest timed calls into the estimate at
// boot, so a restart does not forget each model's measured speed. Non-fatal: a
// seeding error leaves the estimate empty, and it learns again from live calls.
func (s *Server) SeedThroughput(ctx context.Context) error {
	if s.throughput == nil || s.store == nil {
		return nil
	}
	n, err := s.throughput.Seed(ctx, s.store, time.Now())
	if err != nil {
		return err
	}
	log.Printf("throughput: seeded %d timed call(s) from the usage ledger (mode=%s)", n, s.throughput.Mode())
	return nil
}

// ObserveCallTiming feeds one call made outside any run to the estimate —
// a Document unit-generator or image-describe call. It records nothing in the
// ledger: those calls are not billed, and that is unchanged.
func (s *Server) ObserveCallTiming(u *providers.Usage) { s.observeCall("", u) }

// callObserver is the RunOptions / ctx observer for a run's calls that emit no
// usage event (its summaries), attributed to runID for the per-run sample cap.
// nil when the estimate is off, so the loop skips the work entirely.
func (s *Server) callObserver(runID string) providers.CallObserver {
	if s.throughput == nil {
		return nil
	}
	return func(u *providers.Usage) { s.observeCall(runID, u) }
}

func (s *Server) observeCall(runID string, u *providers.Usage) {
	if s.throughput == nil {
		return
	}
	if smp, ok := throughput.SampleFromUsage(runID, time.Now(), u); ok {
		s.throughput.Observe(smp)
	}
}

// isLocalProvider reports whether a provider id is a local-inference backend
// (Capabilities().Local), whose unmeasured models start from local_prior.
func (s *Server) isLocalProvider(id string) bool {
	if s.providers == nil {
		return false
	}
	p, err := s.providers.Get(id)
	return err == nil && p != nil && p.Capabilities().Local
}
