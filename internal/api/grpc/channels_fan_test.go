package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// RFC S client twins over gRPC — a real in-process round-trip through the
// generated stubs, asserting the connector→pb mapping (the map + slice
// result shapes). Business logic is covered by the HTTP/connector tests;
// these pin the gRPC wire mapping.

func TestGrpcAwaitChannels_MapsResult(t *testing.T) {
	client, cleanup := startTestServerWithConnector(t, &n8nMock{})
	defer cleanup()

	resp, err := client.AwaitChannels(context.Background(), &loomcyclepb.AwaitChannelsRequest{
		Channels: []string{"c1"},
		Mode:     "any",
	})
	if err != nil {
		t.Fatalf("AwaitChannels: %v", err)
	}
	if !resp.GetSatisfied() || resp.GetTimedOut() {
		t.Errorf("satisfied=%v timed_out=%v, want true/false", resp.GetSatisfied(), resp.GetTimedOut())
	}
	if resp.GetMode() != "any" || resp.GetTotalMessages() != 1 {
		t.Errorf("mode=%q total=%d, want any/1", resp.GetMode(), resp.GetTotalMessages())
	}
	if got := resp.GetFired(); len(got) != 1 || got[0] != "c1" {
		t.Errorf("fired=%v, want [c1]", got)
	}
	entry := resp.GetResults()["c1"]
	if entry == nil || len(entry.GetMessages()) != 1 {
		t.Fatalf("results[c1] = %+v, want 1 message", entry)
	}
	if entry.GetMessages()[0].GetId() != "m1" || entry.GetNextCursor() != "m1" {
		t.Errorf("entry = %+v, want msg id/next_cursor m1", entry)
	}
}

func TestGrpcBroadcastChannels_MapsResult(t *testing.T) {
	client, cleanup := startTestServerWithConnector(t, &n8nMock{})
	defer cleanup()

	resp, err := client.BroadcastChannels(context.Background(), &loomcyclepb.BroadcastChannelsRequest{
		Channels: []string{"c1", "c2"},
		Payload:  []byte(`{"x":1}`),
	})
	if err != nil {
		t.Fatalf("BroadcastChannels: %v", err)
	}
	if resp.GetPublished() != 2 || resp.GetFailed() != 0 {
		t.Errorf("published=%d failed=%d, want 2/0", resp.GetPublished(), resp.GetFailed())
	}
	results := resp.GetResults()
	if len(results) != 2 || results[0].GetChannel() != "c1" || results[1].GetChannel() != "c2" {
		t.Errorf("results=%+v, want [c1 c2]", results)
	}
	if results[0].GetMsgId() != "m1" {
		t.Errorf("results[0].msg_id=%q, want m1", results[0].GetMsgId())
	}
}
