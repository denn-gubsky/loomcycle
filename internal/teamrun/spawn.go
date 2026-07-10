package teamrun

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// SpawnFunc runs one named agent with an input prompt and returns its final text
// output. It mirrors builtin.SubAgentRunner exactly, so the orchestrator reuses
// the existing sub-agent machinery (tenant/identity inheritance, the recursion
// depth cap, the cancel registry) rather than re-implementing run dispatch.
type SpawnFunc func(ctx context.Context, agent, input, defID string) (string, error)

// NewAgentRunner returns the Phase-1 production Runner: a single-`agent` state's
// AgentDef is run via spawn, and its output flows to the next state along the
// `success` edge.
//
// A `parallel`/`consolidator` handler, or an `agent` handler with a consolidator
// (consolidator-driven edge selection — i.e. pushback), is Phase 3 and returns
// an error here. The walk surfaces that error and stops, so a team authored with
// routing it can't yet execute fails loudly instead of silently taking success.
func NewAgentRunner(spawn SpawnFunc) Runner {
	return &agentRunner{spawn: spawn}
}

type agentRunner struct {
	spawn SpawnFunc
}

func (r *agentRunner) RunHandler(ctx context.Context, st teamgraph.State, input string) (Outcome, error) {
	switch st.Handler.Kind {
	case teamgraph.HandlerAgent:
		if st.Handler.Consolidator != "" {
			return Outcome{}, fmt.Errorf("state %q has a consolidator — consolidator-driven edge selection is not supported yet (Phase 3)", st.ID)
		}
		out, err := r.spawn(ctx, st.Handler.Agent, input, "")
		if err != nil {
			return Outcome{}, err
		}
		// No consolidator → a single-agent state advances on success.
		return Outcome{Output: out}, nil
	case teamgraph.HandlerParallel, teamgraph.HandlerConsolidator:
		return Outcome{}, fmt.Errorf("handler kind %q is not supported yet (Phase 3: parallel fan-out + consolidator)", st.Handler.Kind)
	default:
		// terminal is handled by the walk; anything else is a validation gap.
		return Outcome{}, fmt.Errorf("unexpected handler kind %q", st.Handler.Kind)
	}
}
