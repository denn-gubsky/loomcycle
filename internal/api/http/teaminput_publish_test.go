package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// runInputPublish runs one input state that publishes to c2 through the
// walk's real channel executor, under the given team ACL.
func runInputPublish(t *testing.T, srv *Server, acl *teamgraph.TeamChannels, input string) error {
	t.Helper()
	ctx := context.Background()
	d := teamgraph.Definition{Channels: acl}
	r := teamrun.NewAgentRunner(nil, teamrun.WithChannels(srv.newTeamChannelIO(ctx, d)))
	st := teamgraph.State{ID: "form", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerInput, Publish: &teamgraph.InputPublish{Channel: "c2"}}}
	_, err := r.RunHandler(ctx, st, &teamrun.Task{Input: input})
	return err
}

func peekC2(t *testing.T, srv *Server) []store.ChannelMessage {
	t.Helper()
	msgs, err := srv.store.ChannelPeek(context.Background(), "", "c2", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	return msgs
}

// Through the real executor and store: the message on the channel is the
// form's object, not a string holding it.
func TestTeamInputPublish_StoresTheObject(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	if err := runInputPublish(t, srv, &teamgraph.TeamChannels{Publish: []string{"c2"}}, `{"document_id":"d","chunk_id":"c"}`); err != nil {
		t.Fatalf("input publish: %v", err)
	}
	msgs := peekC2(t, srv)
	if len(msgs) != 1 {
		t.Fatalf("c2 holds %d messages, want 1", len(msgs))
	}
	var obj map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &obj); err != nil {
		t.Fatalf("stored payload %s is not a JSON object: %v", msgs[0].Payload, err)
	}
	if obj["chunk_id"] != "c" || obj["document_id"] != "d" {
		t.Errorf("stored object = %v, want the input's fields", obj)
	}
}

// The publish is gated by the TEAM's ACL at run, as a channel state's is — a
// definition stored without the grant (one written before the preflight, or
// straight to the store) still cannot publish.
func TestTeamInputPublish_RefusedWithoutThePublishGrant(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	for name, acl := range map[string]*teamgraph.TeamChannels{
		"no channels block": nil,
		"another channel":   {Publish: []string{"c3"}},
		"subscribe only":    {Subscribe: []string{"c2"}},
	} {
		err := runInputPublish(t, srv, acl, `{"part":"cpu"}`)
		if err == nil || !strings.Contains(err.Error(), `"c2"`) {
			t.Errorf("%s: err = %v, want the publish refused naming the channel", name, err)
		}
	}
	if msgs := peekC2(t, srv); len(msgs) != 0 {
		t.Errorf("a refused publish left %d message(s) on c2", len(msgs))
	}
}
