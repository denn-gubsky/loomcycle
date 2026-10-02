package teamrun

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// channelPayload is what a channel state publishes. The envelope is built
// exactly as it was before `payload` existed, so a definition without it puts
// byte-identical messages on its channel.
func channelPayload(st teamgraph.State, input string) (json.RawMessage, error) {
	if st.Handler.Payload == teamgraph.PayloadRaw {
		return teamgraph.InputValue(input), nil
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
	if err := r.channels.Publish(ctx, pub.Channel, teamgraph.InputValue(input)); err != nil {
		return fmt.Errorf("state %q publish %q: %w", st.ID, pub.Channel, err)
	}
	return nil
}
