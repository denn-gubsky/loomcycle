package grpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// n8nMock embeds mockConnector and overrides ListChannels +
// StreamUserRunStates for the v0.9.x tests.
type n8nMock struct {
	mockConnector

	listChannelsResp connector.ListChannelsResponse
	listChannelsErr  error
	streamEvents     []connector.RunStateEvent
	streamErr        error
	lastStreamReq    connector.StreamUserRunStatesRequest
}

func (m *n8nMock) ListChannels(context.Context) (connector.ListChannelsResponse, error) {
	return m.listChannelsResp, m.listChannelsErr
}

func (m *n8nMock) StreamUserRunStates(_ context.Context, req connector.StreamUserRunStatesRequest, visit connector.RunStateVisitor) error {
	m.lastStreamReq = req
	for _, evt := range m.streamEvents {
		if err := visit(evt); err != nil {
			if errors.Is(err, connector.ErrStopStreaming) {
				return nil
			}
			return err
		}
	}
	return m.streamErr
}

func TestGrpcListChannels_HappyPath(t *testing.T) {
	mc := &n8nMock{
		listChannelsResp: connector.ListChannelsResponse{
			Channels: []connector.ChannelDescriptor{
				{Name: "alpha", MessageCount: 3, OldestVisibleAt: "2026-05-20T00:00:00Z"},
				{Name: "beta", MessageCount: 0},
			},
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.ListChannels(context.Background(), &loomcyclepb.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(resp.GetChannels()) != 2 {
		t.Fatalf("got %d channels, want 2: %+v", len(resp.GetChannels()), resp.GetChannels())
	}
	if resp.GetChannels()[0].GetName() != "alpha" || resp.GetChannels()[0].GetMessageCount() != 3 {
		t.Errorf("channel[0] = %+v", resp.GetChannels()[0])
	}
}

func TestGrpcStreamUserRunStates_StreamsAllEvents(t *testing.T) {
	mc := &n8nMock{
		streamEvents: []connector.RunStateEvent{
			{RunID: "r1", UserID: "user-a", Status: "running"},
			{RunID: "r2", UserID: "user-a", Status: "completed", StopReason: "end_turn"},
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	stream, err := client.StreamUserRunStates(context.Background(), &loomcyclepb.StreamUserRunStatesRequest{
		UserId:   "user-a",
		Statuses: []string{"running", "completed"},
	})
	if err != nil {
		t.Fatalf("StreamUserRunStates: %v", err)
	}

	var got []*loomcyclepb.RunStateEvent
	for {
		evt, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, evt)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].GetRunId() != "r1" || got[1].GetStatus() != "completed" {
		t.Errorf("events = %+v", got)
	}
	if mc.lastStreamReq.UserID != "user-a" || len(mc.lastStreamReq.Statuses) != 2 {
		t.Errorf("connector req = %+v", mc.lastStreamReq)
	}
}

func TestGrpcStreamUserRunStates_RejectsMissingUserID(t *testing.T) {
	client, cleanup := startTestServerWithConnector(t, &n8nMock{})
	defer cleanup()

	stream, err := client.StreamUserRunStates(context.Background(), &loomcyclepb.StreamUserRunStatesRequest{})
	if err != nil {
		t.Fatalf("StreamUserRunStates(no user_id): %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for empty user_id")
	}
}
