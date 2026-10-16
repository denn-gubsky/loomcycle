package http

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// callerOwnsRun reports whether the ctx principal may act on the run's
// questions — read them or answer them: the run is in its tenant and, for an
// isolated member, in one of its own sessions; for a token limited to certain
// agents, in a tree it could have started (allowlistReachesRun). The run's
// author is the main actor, so its own questions are always its to answer, a
// hook's included; a colleague in a shared tenant keeps the tenant's
// collaboration model.
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
	if run.SessionID != "" {
		sess, err := s.store.GetSession(ctx, run.SessionID)
		if err != nil || !sessionOwnershipOK(ctx, sess) {
			return false, nil
		}
	}
	return s.allowlistReachesRun(ctx, runID), nil
}
