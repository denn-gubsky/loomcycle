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

// CapAction is what a cap observer (OnCap) decides when a state trips its
// per-state iteration cap. The zero value is CapAbort so a nil/incomplete
// decision fails safe — the termination guarantee holds no matter what the
// observer returns.
type CapAction int

const (
	// CapAbort terminates the walk with *ErrIterationCap (the same outcome as
	// having no observer). It is the zero value — an unanswered / timed-out /
	// declined human interruption maps here, so the walk always terminates.
	CapAbort CapAction = iota
	// CapContinue grants the capped state one more full max-iteration window
	// (the tripping entry becomes #1 of the new window) and runs its handler.
	CapContinue
	// CapReroute abandons the capped state's handler this cycle and jumps the
	// walk to Reroute instead, clearing the capped state's counter so a later
	// revisit gets a fresh window. An empty/unknown Reroute target degrades to
	// CapAbort at the next state lookup (fail safe).
	CapReroute
)

// CapDecision is an OnCap observer's ruling.
type CapDecision struct {
	Action  CapAction
	Reroute string // target state id when Action == CapReroute
}

// Option customizes a Walk. Zero options → the walk behaves exactly as the
// bare four-argument form always has (nothing observed; a cap returns
// *ErrIterationCap), so existing callers and the default op=run path are
// byte-identical.
type Option func(*walkConfig)

type walkConfig struct {
	// onEnterState fires once for each state the walk enters (including the
	// terminal state), with that state's id. A board-bound run persists the
	// position here (chunk.status = state). A returned error aborts the walk.
	onEnterState func(ctx context.Context, state string) error
	// onCap fires when a state trips its iteration cap. Its CapDecision steers
	// the walk (abort / continue / reroute). A returned error aborts the walk
	// with that error. nil = a cap returns *ErrIterationCap as before.
	onCap func(ctx context.Context, cap *ErrIterationCap) (CapDecision, error)
}

// OnEnterState registers a hook fired as the walk enters each state (terminal
// included) — the seam a durable board-bound run uses to persist chunk.status.
func OnEnterState(f func(ctx context.Context, state string) error) Option {
	return func(c *walkConfig) { c.onEnterState = f }
}

// OnCap registers a hook that decides what happens when a state trips its
// iteration cap — the seam for human-in-the-loop escalation (Interruption).
// Without it a cap terminates the walk with *ErrIterationCap.
func OnCap(f func(ctx context.Context, cap *ErrIterationCap) (CapDecision, error)) Option {
	return func(c *walkConfig) { c.onCap = f }
}

// Walk drives task from its current state (or the definition's entry when
// task.State is empty) to a terminal state, mutating task in place as it goes:
// each non-terminal state's handler runs via r, its selected edge picks the next
// state, and its output becomes the next input. It returns the ordered trace of
// executed states.
//
// Termination is guaranteed: every non-terminal state increments its own entry
// count, and exceeding the per-state cap returns *ErrIterationCap — unless an
// OnCap observer rules CapContinue/CapReroute, in which case the loop is bounded
// instead by how long a human keeps answering (an unanswered/aborted decision
// still returns *ErrIterationCap). So a pushback loop that never converges is
// always bounded, never infinite.
func Walk(ctx context.Context, d teamgraph.Definition, task *Task, r Runner, opts ...Option) ([]StepRecord, error) {
	if task == nil {
		return nil, fmt.Errorf("teamrun: nil task")
	}
	var cfg walkConfig
	for _, o := range opts {
		o(&cfg)
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
		// Observe the entered (validated) state before dispatch, so a board-bound
		// run persists the position it is ABOUT to work — a crash resumes at this
		// state (at-least-once for its handler). Fires for the terminal state too,
		// so a completed board lands on the end state.
		if cfg.onEnterState != nil {
			if err := cfg.onEnterState(ctx, st.ID); err != nil {
				return trace, err
			}
		}
		if st.Handler.Kind == teamgraph.HandlerTerminal {
			return trace, nil // reached an end state — done
		}

		// Count this entry before running, and refuse to run past the cap so a
		// non-converging loop can't spend an extra handler run on overflow.
		task.IterationCounts[st.ID]++
		if n := task.IterationCounts[st.ID]; n > max {
			capErr := &ErrIterationCap{State: st.ID, Count: n, Max: max}
			if cfg.onCap == nil {
				return trace, capErr
			}
			dec, err := cfg.onCap(ctx, capErr)
			if err != nil {
				return trace, err
			}
			switch dec.Action {
			case CapContinue:
				// Fresh window; this tripping entry becomes iteration #1.
				task.IterationCounts[st.ID] = 1
			case CapReroute:
				// Unstick the capped state (fresh window on a future revisit) and
				// jump. An unknown target is caught by StateByID next iteration.
				task.IterationCounts[st.ID] = 0
				task.State = dec.Reroute
				continue
			default: // CapAbort (and the zero value) — terminate, fail safe.
				return trace, capErr
			}
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
