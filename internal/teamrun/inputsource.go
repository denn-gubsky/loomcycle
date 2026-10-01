package teamrun

import (
	"bytes"
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
// element, in order; any other JSON value is one item, the value itself; text
// that is not JSON (including the empty string) is one item {"text": input},
// so a member always receives a JSON value in its data slot.
//
// An empty array is an error, the same posture as an empty channel or
// document: a walk that proceeded on an empty wave would hand the next state
// an answer nobody produced.
func inputItems(input string) ([]json.RawMessage, error) {
	raw := bytes.TrimSpace([]byte(input))
	if len(raw) == 0 || !json.Valid(raw) {
		item, err := textItem(input)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{item}, nil
	}
	if raw[0] != '[' {
		return []json.RawMessage{json.RawMessage(raw)}, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, fmt.Errorf("the walk's input array: %w", err)
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("the walk's input is an empty array — its items are the array's elements, so there is nothing to dispatch")
	}
	return elems, nil
}

// textItem wraps non-JSON input without HTML escaping: the text is read by a
// model, and `<`, `>` and `&` arriving escaped make it harder to read for no
// gain — the payload lands in a data slot, not in HTML.
func textItem(input string) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{"text": input}); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
