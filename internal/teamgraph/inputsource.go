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
// the entry. The walk's input is what the walk was started with; a later state
// reading "the original input" would give input a second meaning beside the
// output threaded from the state before it.
func validateInputSourcePlacement(d Definition) error {
	for _, s := range d.States {
		if s.Handler.Kind == HandlerStarter && s.Handler.Source.IsInput() && s.ID != d.Entry {
			return fmt.Errorf("team definition: state %q starter reads the walk's input, so it must be the definition's `entry` (entry is %q)", s.ID, d.Entry)
		}
	}
	return nil
}
