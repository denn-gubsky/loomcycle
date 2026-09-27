package grpc

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// publishingConnector answers a publish and a broadcast with every flag set.
type publishingConnector struct{ *mockConnector }

func (publishingConnector) PublishChannel(context.Context, connector.ChannelPublishRequest) (connector.ChannelPublishResult, error) {
	return connector.ChannelPublishResult{MsgID: "m1", Channel: "inbox", Held: true, AwaitingHooks: true, DroppedOldest: 3}, nil
}

func (publishingConnector) BroadcastChannels(context.Context, connector.ChannelBroadcastRequest) (connector.ChannelBroadcastResult, error) {
	return connector.ChannelBroadcastResult{Published: 1, Results: []connector.ChannelBroadcastEntry{
		{Channel: "inbox", MsgID: "m1", Held: true, AwaitingHooks: true}}}, nil
}

// What a publish did reaches a gRPC caller: held, awaiting hooks, and how many
// messages it trimmed. A msg_id with no further word reads as "delivered".
func TestPublishChannel_CarriesWhatThePublishDid(t *testing.T) {
	srv := &Server{connector: publishingConnector{&mockConnector{}}}
	resp, err := srv.PublishChannel(context.Background(), &loomcyclepb.PublishChannelRequest{Channel: "inbox", Scope: "global", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetHeld() || !resp.GetAwaitingHooks() || resp.GetDroppedOldest() != 3 {
		t.Fatalf("resp = %+v", resp)
	}
	bresp, err := srv.BroadcastChannels(context.Background(), &loomcyclepb.BroadcastChannelsRequest{Channels: []string{"inbox"}, Scope: "global", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if r := bresp.GetResults(); len(r) != 1 || !r[0].GetHeld() || !r[0].GetAwaitingHooks() {
		t.Fatalf("broadcast = %+v", bresp)
	}
}

// Every field of a publish result is on every transport: the proto message,
// the TS interface and the Python client's dict. Read from the connector
// structs, so a field added there fails here until each transport carries it.
func TestChannelPublishResult_ThreeWayDrift(t *testing.T) {
	for _, tc := range []struct {
		goType                 any
		proto, ts, py, pyShape string
	}{
		{connector.ChannelPublishResult{}, "message PublishChannelResponse {", "export interface ChannelPublishResult {",
			"async def publish_channel(", `"%s": resp.%s`},
		{connector.ChannelBroadcastEntry{}, "message BroadcastChannelEntry {", "export interface ChannelBroadcastEntry {",
			"async def broadcast_channels(", `"%s": r.%s`},
	} {
		rt := reflect.TypeOf(tc.goType)
		var fields []string
		for i := 0; i < rt.NumField(); i++ {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			fields = append(fields, name)
		}
		for _, surface := range []struct{ what, path, start, end string }{
			{"the proto", "../../../proto/loomcycle.proto", tc.proto, "\n}"},
			{"the TS adapter", "../../../adapters/ts/src/types.ts", tc.ts, "\n}"},
			{"the Python client", "../../../adapters/python/loomcycle/client.py", tc.py, "\n    async def "},
		} {
			b, err := os.ReadFile(surface.path)
			if err != nil {
				t.Skipf("%s not readable from here: %v", surface.path, err)
			}
			src := string(b)
			i := strings.Index(src, surface.start)
			if i < 0 {
				t.Fatalf("%s: %q not found", surface.what, surface.start)
			}
			blk := src[i+len(surface.start):]
			if j := strings.Index(blk, surface.end); j >= 0 {
				blk = blk[:j]
			}
			for _, f := range fields {
				want := []string{" " + f + " = ", " " + f + ":", " " + f + "?:", strings.ReplaceAll(tc.pyShape, "%s", f)}
				found := false
				for _, w := range want {
					found = found || strings.Contains(blk, w)
				}
				if !found {
					t.Errorf("%s does not carry %s.%s", surface.what, rt.Name(), f)
				}
			}
		}
	}
}
