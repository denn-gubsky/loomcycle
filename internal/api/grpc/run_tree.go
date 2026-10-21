package grpc

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runTree is the agent-allowlist walker for the principal on ctx
// (auth.RunTree), reading runs from the store — the HTTP server's twin, for
// the RPCs that read runs and sessions here rather than through the
// connector. Without a store it reads nothing, so a token limited to certain
// agents is refused and every other caller is unaffected.
func (s *Server) runTree(ctx context.Context) *auth.RunTree {
	p, ok := auth.PrincipalFromContext(ctx)
	if s.store == nil {
		return auth.NewRunTree(p, ok, nil)
	}
	return auth.NewRunTree(p, ok, func(runID string) (auth.RunLink, error) {
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return auth.RunLink{}, err
		}
		return runLink(run), nil
	})
}

// runLink is what an allowlist walk reads of a run row.
func runLink(run store.Run) auth.RunLink {
	return auth.RunLink{ID: run.ID, Agent: run.Agent, ParentRunID: run.ParentRunID}
}

// sessionReachable reports whether the caller's agent allowlist reaches every
// run in sess (auth.RunTree.ReachesSession): a session read hands over their
// history.
func (s *Server) sessionReachable(ctx context.Context, sess store.Session) bool {
	tree := s.runTree(ctx)
	if !tree.Restricted() {
		return true
	}
	if s.store == nil {
		return false
	}
	return tree.ReachesSession(sess.Agent, func() ([]auth.RunLink, error) {
		runs, err := s.store.RunsForSession(ctx, sess.ID)
		if err != nil {
			return nil, err
		}
		links := make([]auth.RunLink, 0, len(runs))
		for _, r := range runs {
			links = append(links, runLink(r))
		}
		return links, nil
	})
}
