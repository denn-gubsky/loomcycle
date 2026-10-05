package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The stored spelling of a team's own channel is never an address, whatever
// the caller's grants and whatever its catalog says: a plane holding every
// channel, with the name declared, is still refused on every op.
func TestChannelTool_ReservedTeamPrefixRefusedOnEveryOp(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	const name = "_team/sdlc/events"
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		AllPublish: true, AllSubscribe: true,
		Channels: map[string]tools.ChannelDef{name: {Name: name, Scope: "tenant"}},
	})
	for _, in := range []string{
		`{"op":"publish","channel":"` + name + `","value":{}}`,
		`{"op":"subscribe","channel":"` + name + `"}`,
		`{"op":"peek","channel":"` + name + `"}`,
		`{"op":"ack","channel":"` + name + `","cursor":"cur_0"}`,
		`{"op":"release","channel":"` + name + `"}`,
		`{"op":"await","channels":["` + name + `"]}`,
		`{"op":"broadcast","channels":["` + name + `"],"value":{}}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		if !res.IsError || !strings.Contains(res.Text, "reserved") {
			t.Errorf("%s: want the reserved-name refusal, got %s", in, res.Text)
		}
	}
}
