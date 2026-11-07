package http

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// callerOwnsRun reports whether the ctx principal may act on the run's
// questions — read them or answer them: the run is in its tenant and, for a
// member (isolated or not), its own run in one of its own sessions; for a
// token limited to certain agents, in a tree it could have started
// (allowlistReachesRun). The run's author is the main actor, so its own
// questions are always its to answer, a hook's included; a tenant operator
// answers any run's of its tenant.
// Same rule as the review verb.
func (s *Server) callerOwnsRun(ctx context.Context, runID string) (bool, error) {
	run, err := s.tenantStore(ctx).GetRun(ctx, runID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	// The run's own owner, not only its session's: a run with no session
	// would otherwise pass on the tenant fold alone.
	if !runOwnershipOK(ctx, run) {
		return false, nil
	}
	if run.SessionID != "" {
		sess, err := s.store.GetSession(ctx, run.SessionID)
		if err != nil || !sessionOwnershipOK(ctx, sess) {
			return false, nil
		}
	}
	return s.allowlistReachesRun(ctx, runID), nil
}
