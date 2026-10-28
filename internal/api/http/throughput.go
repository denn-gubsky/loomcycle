package http

import (
	"context"
	"log/slog"
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
	slog.Info("throughput: seeded timed calls from the usage ledger", "calls", n, "mode", s.throughput.Mode())
	return nil
}

func (s *Server) observeCall(runID string, u *providers.Usage) {
	if s.throughput == nil {
		return
	}
	if smp, ok := throughput.SampleFromUsage(runID, time.Now(), u); ok {
		s.throughput.Observe(smp)
	}
}

// concreteModel names the concrete model behind a (provider, model) pair through
// the LIVE `models:` map, which a config reload can change: a sample that arrives
// after an alias is repointed is filed under the model it now names.
func (s *Server) concreteModel(provider, model string) string {
	if c := s.cfg(); c != nil {
		return c.ConcreteModel(provider, model)
	}
	return model
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
