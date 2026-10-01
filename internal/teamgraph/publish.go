package teamgraph

// publish.go — publishing a JSON value rather than an envelope.
//
// A `channel` state wraps what it publishes as {"state", "output": "<text>"}.
// That is a fine notice and a useless work item: the payload a downstream
// Starter projects with `binds` is the envelope, whose only field is a string,
// so no bind can reach the fields of the JSON inside it. Two opt-ins publish
// the value itself instead — `publish` on an input state (the walk's input)
// and `payload: "raw"` on a channel state (whatever reached it).

import (
	"fmt"
	"strings"
)

// Payload shapes for a channel state.
const (
	PayloadEnvelope = "envelope" // {"state": "<id>", "output": "<input as a string>"} — the default
	PayloadRaw      = "raw"      // the input as a JSON value
)

// InputPublish is where an input state publishes the walk's input.
type InputPublish struct {
	// Channel is the channel to publish to. It is a publish reference (see
	// ChannelRefs), so it needs the team's channels.publish grant exactly as a
	// channel state's `channel` does.
	Channel string `json:"channel"`
}

// validatePublishing checks `publish` and `payload`, each of which belongs to
// one kind. On any other kind they would read as configured and do nothing.
func validatePublishing(stateID string, h Handler) error {
	if h.Publish != nil {
		if h.Kind != HandlerInput {
			return fmt.Errorf("team definition: state %q sets `publish` but is kind %q (input only) — "+
				"a channel state publishes to its `channel`", stateID, h.Kind)
		}
		if strings.TrimSpace(h.Publish.Channel) == "" {
			return fmt.Errorf("team definition: state %q input handler `publish` is present but names no channel", stateID)
		}
	}
	if h.Payload != "" && h.Kind != HandlerChannel {
		return fmt.Errorf("team definition: state %q sets `payload` but is kind %q (channel only)", stateID, h.Kind)
	}
	switch h.Payload {
	case "", PayloadEnvelope, PayloadRaw:
	default:
		return fmt.Errorf("team definition: state %q channel handler has invalid payload %q (want envelope|raw)", stateID, h.Payload)
	}
	return nil
}
