package http

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// SetStrictLimits turns strict token-budget enforcement on
// (LOOMCYCLE_LIMITS_ENFORCE=strict).
//
// Without it a hard budget is checked where a run is admitted and nowhere
// else: a run already going finishes, and so does every sub-agent it spawns.
// With it the same check also runs before each model call of a run and before
// each sub-agent is started, so a (tenant, user) over its budget stops
// spending within one model call per run in flight.
//
// It is the same counter either way, so the same limits apply: it is exact on
// one replica and best effort across several, and a counter that could not be
// loaded refuses nothing.
func (s *Server) SetStrictLimits(on bool) { s.strictLimits = on }

// strictBudgetCheck is the strict-mode budget check for (tenant, user): nil
// while they are under every hard budget, or when strict mode is off.
func (s *Server) strictBudgetCheck(tenantID, userID string) error {
	if !s.strictLimits {
		return nil
	}
	if dec := s.limits.Check(tenantID, userID); !dec.Allowed {
		return fmt.Errorf("%w: %s", runner.ErrTokenLimitExceeded, dec.Refusal.Message)
	}
	return nil
}

// budgetGate is the loop's per-call budget hook for a run of (tenant, user).
// Nil when strict mode is off, so the loop does not consult it at all.
func (s *Server) budgetGate(tenantID, userID string) func() error {
	if !s.strictLimits {
		return nil
	}
	return func() error { return s.strictBudgetCheck(tenantID, userID) }
}
