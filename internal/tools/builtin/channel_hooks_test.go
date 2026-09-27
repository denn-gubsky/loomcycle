package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// list_channels names the channels this agent may use that carry hooks, so it
// knows a publish there is checked (and may be rewritten or dropped) before
// anyone sees it — and names no channel it cannot use.
func TestChannelTool_ListChannelsNamesHookedChannels(t *testing.T) {
	ctx := tools.WithChannelPolicy(context.Background(), tools.ChannelPolicyValue{
		Publish:   []string{"inbox"},
		Subscribe: []string{"plain"},
		Channels: map[string]tools.ChannelDef{
			"inbox":  {Name: "inbox", Scope: "global", Hooked: true},
			"plain":  {Name: "plain", Scope: "global"},
			"secret": {Name: "secret", Scope: "global", Hooked: true},
		},
	})
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := (&Channel{Store: st}).Execute(ctx, json.RawMessage(`{"op":"list_channels"}`))
	if err != nil || res.IsError {
		t.Fatalf("list_channels: %v %s", err, res.Text)
	}
	var out struct {
		Hooked []string `json:"hooked"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Hooked, ",") != "inbox" {
		t.Fatalf("hooked = %v, want only the hooked channel this agent may use", out.Hooked)
	}
}
