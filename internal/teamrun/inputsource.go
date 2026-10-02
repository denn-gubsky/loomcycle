package teamrun

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// A Starter whose source is the walk's INPUT: the items are read from the
// value the walk was started with, once, when the wave dispatches. Validation
// confines it to the entry state, so task.Input here is the walk's own input
// (or, on a loop back to the entry, what the state before it threaded on —
// the same thing an input state sees there).

// runInputStarter turns the walk's input into wave items and dispatches the
// wave exactly as a document Starter does. Nothing is read from a store and
// nothing is acked: there is no cursor.
func (r *agentRunner) runInputStarter(ctx context.Context, st teamgraph.State, task *Task) (Outcome, error) {
	h := st.Handler
	// The sink is still a channel. Checked before anything is dispatched so a
	// walk that cannot publish its results never spawns the runs that produce
	// them.
	if h.Sink != nil && r.channels == nil {
		return Outcome{}, fmt.Errorf("state %q publishes to a sink but no channel executor is wired", st.ID)
	}
	width := 0
	if h.Fanout.Per != teamgraph.FanoutPerOnce {
		var err error
		if width, err = r.waveWidth(st); err != nil {
			return Outcome{}, err
		}
	}

	items, err := inputItems(task.Input)
	if err != nil {
		return Outcome{}, fmt.Errorf("state %q: %w", st.ID, err)
	}
	// MORE than the ceiling fails the walk rather than dispatching the first
	// `max`: the input has no cursor, so the items past the ceiling would be
	// dropped with nothing to say so.
	if width > 0 && len(items) > width {
		return Outcome{}, fmt.Errorf("state %q: the walk's input has %d items, more than fanout.max=%d — "+
			"raise max or send fewer; the input has no cursor, so dispatching the first %d would silently drop the rest",
			st.ID, len(items), width, width)
	}

	msgs := make([]ChannelMessage, 0, len(items))
	for i, it := range items {
		msgs = append(msgs, ChannelMessage{ID: fmt.Sprintf("input[%d]", i), Payload: it})
	}

	results, waveErr := r.runWave(ctx, st, task, msgs)
	if waveErr != nil {
		return Outcome{}, fmt.Errorf("state %q wave: %w", st.ID, waveErr)
	}
	envelope, err := resultsEnvelope(results)
	if err != nil {
		return Outcome{}, err
	}
	return r.captured(st, task, Outcome{Output: envelope})
}

// inputItems is the walk's input as wave items: a JSON array is one item per
// element, in order; anything else is one item, teamgraph.InputValue — the
// value itself, or {"text": input} for text that is not JSON (including the
// empty string), so a member always receives a JSON value in its data slot.
//
// An empty array is an error, the same posture as an empty channel or
// document: a walk that proceeded on an empty wave would hand the next state
// an answer nobody produced.
func inputItems(input string) ([]json.RawMessage, error) {
	v := teamgraph.InputValue(input)
	if v[0] != '[' {
		return []json.RawMessage{v}, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(v, &elems); err != nil {
		return nil, fmt.Errorf("the walk's input array: %w", err)
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("the walk's input is an empty array — its items are the array's elements, so there is nothing to dispatch")
	}
	return elems, nil
}

// refuseInputReentry refuses a cap reroute into an input-sourced Starter. The
// definition can name no edge into one (validation refuses it), but a reroute
// target is chosen at run time — by a person answering a cap interruption —
// and by then the walk's input slot holds the capped state's input, not the
// input the walk was started with. Refused rather than degraded to abort so
// the person sees why their answer was not followed.
func refuseInputReentry(d teamgraph.Definition, from, target string) error {
	if to, ok := teamgraph.StateByID(d, target); ok && teamgraph.IsInputStarter(to) {
		return fmt.Errorf("teamrun: state %q: reroute to %q refused — it is a starter that reads the walk's input, "+
			"and its items are the input the walk was started with; route a retry to a later state", from, target)
	}
	return nil
}
