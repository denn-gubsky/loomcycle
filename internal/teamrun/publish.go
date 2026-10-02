package teamrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// inputMessageValue is a walk's text as a channel message VALUE: valid JSON
// is that JSON value (an object stays an object, not a string holding one),
// and anything else — including the empty string — is {"text": "<input>"}.
//
// One rule for "the input as a message", so what a downstream Starter's
// `binds` can reach does not depend on which node put it on the channel. A
// Starter whose source is the walk's input applies the same rule to non-JSON
// text, so its {{starter.message}} matches what a publish would have sent.
func inputMessageValue(input string) json.RawMessage {
	// Compact refuses exactly what is not valid JSON.
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(input)); err == nil {
		return buf.Bytes()
	}
	// Marshalling a map of one string cannot fail.
	out, _ := json.Marshal(map[string]string{"text": input})
	return out
}

// channelPayload is what a channel state publishes. The envelope is built
// exactly as it was before `payload` existed, so a definition without it puts
// byte-identical messages on its channel.
func channelPayload(st teamgraph.State, input string) (json.RawMessage, error) {
	if st.Handler.Payload == teamgraph.PayloadRaw {
		return inputMessageValue(input), nil
	}
	return json.Marshal(map[string]any{"state": st.ID, "output": input})
}

// publishInput publishes an input state's input to its `publish` channel, a
// no-op when the state declares none. It goes through the walk's channel
// executor, so the team ACL and the channel's declared scope gate it exactly
// as they gate a channel state.
func (r *agentRunner) publishInput(ctx context.Context, st teamgraph.State, input string) error {
	pub := st.Handler.Publish
	if pub == nil {
		return nil
	}
	if r.channels == nil {
		return fmt.Errorf("state %q publishes to a channel but no channel executor is wired", st.ID)
	}
	if err := r.channels.Publish(ctx, pub.Channel, inputMessageValue(input)); err != nil {
		return fmt.Errorf("state %q publish %q: %w", st.ID, pub.Channel, err)
	}
	return nil
}
