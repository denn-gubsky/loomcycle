package http

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// withRunHooks resolves the hooks a run carries and attaches them to the run's
// ctx, where every hook the run fires is taken from: its AgentDef's, verbatim,
// then what the run adds — what a parent run passed down (on ctx) followed by
// what this run's request named. Additions are only ever appended; nothing a
// run is given can remove a hook its agent carries. The run's additions are put
// back on ctx for the sub-agents it starts.
//
// The set is also kept by run id for the hooks fired outside the loop (a manual
// compaction, run_end). A set that fails to resolve still attaches: the loop
// refuses to start a run whose hooks it could not resolve, because a gate a
// definition or a caller named must not quietly be missing.
func (s *Server) withRunHooks(ctx context.Context, runID, agentName string, def config.AgentDef, requested hooks.Additions) context.Context {
	added := hooks.AdditionsFrom(ctx).Merge(requested)
	ctx = hooks.WithAdditions(ctx, added)
	if len(def.Hooks) == 0 && len(def.ToolHooks) == 0 && added.Empty() {
		// No hooks. A sub-agent's ctx still carries its parent's set, which is
		// the parent's own gates, not the child's: replace it with an empty one.
		if hooks.SetFrom(ctx) != nil {
			return hooks.WithSet(ctx, hooks.NewSet())
		}
		return ctx
	}
	set := s.resolveRunHooks(ctx, agentName, def, requested, added)
	if runID != "" {
		s.runHookSets.Store(runID, set)
	}
	return hooks.WithSet(ctx, set)
}

func (s *Server) resolveRunHooks(ctx context.Context, agentName string, def config.AgentDef, requested, added hooks.Additions) *hooks.Set {
	// A caller's own additions must name tools this agent has; inherited ones
	// were checked against the parent's, and a child without the tool simply
	// never fires them.
	if err := requested.Validate(def.Tools); err != nil {
		return hooks.FailedSet(fmt.Errorf("the run's hooks: %w", err))
	}
	set := hooks.NewSet()
	lookup := builtin.HookDefLookup(s.store)
	agent := hooks.Source{Owner: "agent:" + agentName, Tenant: def.OwnerTenant, OperatorAuthored: def.OperatorAuthored}
	if err := hooks.Resolve(ctx, agent, def.Hooks, def.ToolHooks, lookup, s.hookPermits, set); err != nil {
		return hooks.FailedSet(fmt.Errorf("agent %s: %w", agentName, err))
	}
	// A run's additions are the caller's, not an operator's definition: they
	// resolve in the run's tenant and never widen hosts.
	run := hooks.Source{Owner: "run", Tenant: tools.RunIdentity(ctx).TenantID}
	if err := hooks.Resolve(ctx, run, added.Hooks, added.ToolHooks, lookup, s.hookPermits, set); err != nil {
		return hooks.FailedSet(fmt.Errorf("the run's hooks: %w", err))
	}
	return set
}

// additionsRecord is what a run's record keeps of its additions (nil = none).
func additionsRecord(a hooks.Additions) *hooks.Additions {
	if a.Empty() {
		return nil
	}
	return &a
}

// runHookSet is the resolved set of a live run, or nil.
func (s *Server) runHookSet(runID string) *hooks.Set {
	if v, ok := s.runHookSets.Load(runID); ok {
		return v.(*hooks.Set)
	}
	return nil
}
