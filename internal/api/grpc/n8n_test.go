package grpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

func (m *n8nMock) Config(context.Context) ([]byte, error) {
	return nil, errors.New("not implemented")
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

// TestGrpcStreamUserRunStates_ForwardsTheWalkFilter pins the half of the
// contract the CLIENT controls: walk_id must reach the connector.
//
// It is the wiring that goes wrong silently. An unforwarded filter does not
// error — the stream simply carries every walk's runs, which looks exactly
// like a walk that spawned a lot of agents. So the assertion is on what the
// connector RECEIVED, not on what came back.
func TestGrpcStreamUserRunStates_ForwardsTheWalkFilter(t *testing.T) {
	mc := &n8nMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	stream, err := client.StreamUserRunStates(context.Background(), &loomcyclepb.StreamUserRunStatesRequest{
		UserId: "user-a",
		WalkId: "run_walk_123",
	})
	if err != nil {
		t.Fatalf("StreamUserRunStates: %v", err)
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("Recv: %v", err)
		}
	}
	if mc.lastStreamReq.WalkID != "run_walk_123" {
		t.Errorf("connector received WalkID %q, want %q — an unforwarded filter streams "+
			"every walk, which is indistinguishable from a busy one",
			mc.lastStreamReq.WalkID, "run_walk_123")
	}
}

// TestGrpcStreamUserRunStates_CarriesTheParentContext pins the half the SERVER
// controls: the lineage the filter matches on has to reach the caller too.
//
// walk_id says a run is INSIDE a walk. wave_id and wave_index say where — a
// fan-out of N dispatched from one channel read shares a wave_id and differs
// only by index — so a client that can filter but cannot read these can list a
// walk's agents and not draw it.
//
// wave_index 0 is a REAL first position. proto3 cannot distinguish 0 from
// unset on a scalar, which is why parent_context is a MESSAGE: absence is
// carried by the message, not by its fields.
func TestGrpcStreamUserRunStates_CarriesTheParentContext(t *testing.T) {
	mc := &n8nMock{
		streamEvents: []connector.RunStateEvent{
			{
				RunID: "r0", AgentID: "ag0", Agent: "researcher", UserID: "user-a",
				Status: "running", TS: "2026-09-12T00:00:00Z",
				ParentContext: &store.ParentContext{
					RootAgentRunID: "root-1",
					FunctionKey:    "triage",
					WalkID:         "run_walk_123",
					WaveID:         "wave_1",
					WaveIndex:      0,
				},
			},
			// A run with no lineage at all — the message must be absent, not an
			// empty one a caller would read as "walk_id is blank".
			{
				RunID: "r1", AgentID: "ag1", Agent: "solo", UserID: "user-a",
				Status: "completed", TS: "2026-09-12T00:00:01Z",
			},
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	stream, err := client.StreamUserRunStates(context.Background(), &loomcyclepb.StreamUserRunStatesRequest{
		UserId: "user-a",
	})
	if err != nil {
		t.Fatalf("StreamUserRunStates: %v", err)
	}
	var got []*loomcyclepb.RunStateEvent
	for {
		evt, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, evt)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}

	pc := got[0].GetParentContext()
	if pc == nil {
		t.Fatalf("parent_context was dropped — a gRPC subscriber cannot place the run in its walk")
	}
	if pc.GetWalkId() != "run_walk_123" || pc.GetWaveId() != "wave_1" {
		t.Errorf("walk/wave = %q/%q, want run_walk_123/wave_1", pc.GetWalkId(), pc.GetWaveId())
	}
	if pc.GetWaveIndex() != 0 {
		t.Errorf("wave_index = %d, want 0", pc.GetWaveIndex())
	}
	if pc.GetRootAgentRunId() != "root-1" || pc.GetFunctionKey() != "triage" {
		t.Errorf("the pre-existing lineage fields did not survive: %+v", pc)
	}

	if got[1].GetParentContext() != nil {
		t.Errorf("a run with no lineage got an EMPTY parent_context; absence must stay "+
			"distinguishable from a blank walk_id: %+v", got[1].GetParentContext())
	}
}
