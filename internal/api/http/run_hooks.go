package http

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// withRunHooks resolves the hooks a run carries — its AgentDef's, verbatim —
// and attaches them to the run's ctx, where every hook the run fires is taken
// from. The set is also kept by run id for the hooks fired outside the loop (a
// manual compaction, run_end). A set that fails to resolve still attaches: the
// loop refuses to start a run whose hooks it could not resolve, because a gate
// a definition names must not quietly be missing.
func (s *Server) withRunHooks(ctx context.Context, runID, agentName string, def config.AgentDef) context.Context {
	if len(def.Hooks) == 0 && len(def.ToolHooks) == 0 {
		// No hooks. A sub-agent's ctx still carries its parent's set, which is
		// the parent's own gates, not the child's: replace it with an empty one.
		if hooks.SetFrom(ctx) != nil {
			return hooks.WithSet(ctx, hooks.NewSet())
		}
		return ctx
	}
	set := hooks.NewSet()
	src := hooks.Source{Owner: "agent:" + agentName, Tenant: def.OwnerTenant, OperatorAuthored: def.OperatorAuthored}
	if err := hooks.Resolve(ctx, src, def.Hooks, def.ToolHooks, builtin.HookDefLookup(s.store), s.hookPermits, set); err != nil {
		set = hooks.FailedSet(fmt.Errorf("agent %s: %w", agentName, err))
	}
	if runID != "" {
		s.runHookSets.Store(runID, set)
	}
	return hooks.WithSet(ctx, set)
}

// runHookSet is the resolved set of a live run, or nil.
func (s *Server) runHookSet(runID string) *hooks.Set {
	if v, ok := s.runHookSets.Load(runID); ok {
		return v.(*hooks.Set)
	}
	return nil
}
