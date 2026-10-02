package teamgraph

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A Starter whose source is the walk's INPUT (source.kind=input): the value the
// walk was started with becomes the wave's items, so a team that dispatches
// from a channel or a document can also be started from its own form, in one
// call, and process exactly the input it was given.

// validateInputSource checks an input source. Like a document it has no cursor
// and nothing to wait for, and it reads no store at all, so every field that
// names a store or shapes a read is refused rather than ignored: each would
// read as configured and do nothing.
func validateInputSource(stateID string, h Handler) error {
	src := h.Source
	switch {
	case strings.TrimSpace(src.Channel) != "":
		return fmt.Errorf("team definition: state %q starter reads the walk's input, so `source.channel` means nothing — remove it", stateID)
	case src.Path != "" || src.Scope != "" || src.Select != "":
		return fmt.Errorf("team definition: state %q starter reads the walk's input, which is no document — remove source.path/scope/select", stateID)
	case src.Wait != "" || src.N != 0 || src.WaitMS != 0:
		return fmt.Errorf("team definition: state %q starter reads the walk's input, which is there when the walk starts — "+
			"there is nothing to wait for, so remove source.wait/n/wait_ms", stateID)
	case src.Batch != 0:
		return fmt.Errorf("team definition: state %q starter reads the walk's input, which is read whole — remove source.batch "+
			"(fanout.max bounds the wave)", stateID)
	case h.Ack != "":
		return fmt.Errorf("team definition: state %q starter reads the walk's input, which has no cursor to acknowledge — remove `ack`", stateID)
	case h.Fanout != nil && h.Fanout.Per == FanoutPerChunk:
		return fmt.Errorf("team definition: state %q starter fanout per=chunk needs a document source (source.kind: document); "+
			"an input source fans out per=message (one run per item) or per=once", stateID)
	case len(h.Schema) > 0 && !json.Valid(h.Schema):
		return fmt.Errorf("team definition: state %q starter `schema` is not valid JSON", stateID)
	}
	return nil
}

// validateInputSourcePlacement refuses an input-sourced Starter anywhere but
// the entry, and refuses every transition into one. The walk's input is what
// the walk was started with; a later state reading "the original input" would
// give input a second meaning beside the output threaded from the state before
// it — and so would the entry itself, re-entered by an edge, because by then
// the walk's input slot holds that threaded output.
//
// Transitions are the only targets a definition names (every `on` form —
// success, pushback, conditional — is a Transition, resolved by NextState), so
// checking each `to` covers them all. A cap reroute names its target at run
// time and is refused by the walk (IsInputStarter).
func validateInputSourcePlacement(d Definition) error {
	for _, s := range d.States {
		if IsInputStarter(s) && s.ID != d.Entry {
			return fmt.Errorf("team definition: state %q starter reads the walk's input, so it must be the definition's `entry` (entry is %q)", s.ID, d.Entry)
		}
	}
	for i, t := range d.Transitions {
		if to, ok := StateByID(d, t.To); ok && IsInputStarter(to) {
			return fmt.Errorf("team definition: transition[%d] from %q on %q leads into state %q, a starter that reads the walk's input — "+
				"its items are the input the walk was started with, so it cannot be re-entered; route a retry to a later state", i, t.From, t.On, t.To)
		}
	}
	return nil
}

// IsInputStarter reports whether a state is a Starter whose source is the
// walk's input.
func IsInputStarter(s State) bool {
	return s.Handler.Kind == HandlerStarter && s.Handler.Source.IsInput()
}
