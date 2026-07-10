// Package teamrun is the runtime orchestrator for RFC AP agent teams: it walks a
// TeamDef's state graph for one unit of work, running each non-terminal state's
// handler and following the transition the handler selects, until it reaches a
// terminal state (done) or a state's per-state cycle cap (ErrIterationCap).
//
// The engine is a pure graph-walker. It does NOT spawn agents, read the store,
// or touch a Document chunk — a Runner does the execution and a Task carries the
// mutable walk position. This keeps the state-machine logic deterministic and
// unit-testable with a fake runner; the production wiring (spawning agents via
// the connector, persisting the position onto a Document chunk, firing an
// Interruption on the cap, and a trigger surface) is layered on top (a
// follow-up), not baked into the walk.
//
// Phase 1 (RFC AP) exercises single-`agent` handlers + `success`/`pushback`
// transitions + loops. The engine itself is handler-kind-agnostic beyond
// stopping at `terminal`: which state runs, how, and which edge it picks are all
// the Runner's concern, so `parallel`/`consolidator` execution (Phase 3) lands
// entirely in a richer Runner without changing this walk.
package teamrun

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// Runner executes one non-terminal state's handler and reports the outcome. The
// production runner spawns the state's AgentDef(s) via the connector; a test
// runner returns canned outcomes. A returned error aborts the walk.
type Runner interface {
	RunHandler(ctx context.Context, state teamgraph.State, input string) (Outcome, error)
}

// Outcome is what a handler produced: the text output (which becomes the next
// state's input) and the transition label it selected. An empty Edge defaults to
// teamgraph.OnSuccess, so a linear handler needn't name its single edge.
type Outcome struct {
	Output string
	Edge   string
}

// Task is the mutable walk position for one unit of work (a Document chunk, or an
// ephemeral run). The caller persists it between steps — State + IterationCounts
// ride the chunk's status + fields in production; Input threads handler output to
// the next state. A zero State starts the walk at the definition's entry.
type Task struct {
	State           string
	Input           string
	IterationCounts map[string]int
}

// StepRecord is one executed state, for the caller's trace/audit.
type StepRecord struct {
	State   string // the state that ran
	Handler string // its handler kind
	Agent   string // the handler's agent (for kind=agent/consolidator)
	Edge    string // the transition label taken
	Next    string // the destination state
	Output  string // the handler's output
}

// ErrIterationCap is returned when a state is entered more than the definition's
// per-state cap allows — the caller raises an Interruption. It is a distinct type
// (not a sentinel) so the caller can read which state and the counts.
type ErrIterationCap struct {
	State string
	Count int
	Max   int
}

func (e *ErrIterationCap) Error() string {
	return fmt.Sprintf("teamrun: state %q exceeded max_iterations (%d > %d)", e.State, e.Count, e.Max)
}

// Walk drives task from its current state (or the definition's entry when
// task.State is empty) to a terminal state, mutating task in place as it goes:
// each non-terminal state's handler runs via r, its selected edge picks the next
// state, and its output becomes the next input. It returns the ordered trace of
// executed states.
//
// Termination is guaranteed: every non-terminal state increments its own entry
// count, and exceeding the per-state cap returns *ErrIterationCap. So a
// pushback loop that never converges is bounded, not infinite.
func Walk(ctx context.Context, d teamgraph.Definition, task *Task, r Runner) ([]StepRecord, error) {
	if task == nil {
		return nil, fmt.Errorf("teamrun: nil task")
	}
	if task.State == "" {
		task.State = d.Entry
	}
	if task.IterationCounts == nil {
		task.IterationCounts = map[string]int{}
	}
	max := teamgraph.EffectiveMaxIterations(d)

	var trace []StepRecord
	for {
		if err := ctx.Err(); err != nil {
			return trace, err
		}
		st, ok := teamgraph.StateByID(d, task.State)
		if !ok {
			return trace, fmt.Errorf("teamrun: current state %q is not in the team definition", task.State)
		}
		if st.Handler.Kind == teamgraph.HandlerTerminal {
			return trace, nil // reached an end state — done
		}

		// Count this entry before running, and refuse to run past the cap so a
		// non-converging loop can't spend an extra handler run on overflow.
		task.IterationCounts[st.ID]++
		if n := task.IterationCounts[st.ID]; n > max {
			return trace, &ErrIterationCap{State: st.ID, Count: n, Max: max}
		}

		out, err := r.RunHandler(ctx, st, task.Input)
		if err != nil {
			return trace, fmt.Errorf("teamrun: state %q handler: %w", st.ID, err)
		}
		edge := out.Edge
		if edge == "" {
			edge = teamgraph.OnSuccess
		}
		next, ok := teamgraph.NextState(d, st.ID, edge)
		if !ok {
			return trace, fmt.Errorf("teamrun: state %q handler selected edge %q with no matching transition", st.ID, edge)
		}

		trace = append(trace, StepRecord{
			State:   st.ID,
			Handler: st.Handler.Kind,
			Agent:   st.Handler.Agent,
			Edge:    edge,
			Next:    next,
			Output:  out.Output,
		})
		task.Input = out.Output
		task.State = next
	}
}
