package grpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// interactiveMock is a programmable Connector for the RFC AI gRPC tests. Embeds
// mockConnector so every unrelated method comes for free; overrides SteerRun +
// StreamRunEvents to stage inputs/outputs.
type interactiveMock struct {
	mockConnector

	gotSteerRunID  string
	gotSteerText   string
	gotSteerSource string
	steerDelivered bool
	steerErr       error

	gotStreamRunID string
	gotStreamFrom  int64
	streamEvents   []providers.Event
	streamRunErr   error
}

func (m *interactiveMock) SteerRun(_ context.Context, runID, text, source string) (bool, error) {
	m.gotSteerRunID, m.gotSteerText, m.gotSteerSource = runID, text, source
	return m.steerDelivered, m.steerErr
}

func (m *interactiveMock) StreamRunEvents(_ context.Context, runID string, fromSeq int64, visit connector.RunEventVisitor) error {
	m.gotStreamRunID, m.gotStreamFrom = runID, fromSeq
	for _, ev := range m.streamEvents {
		if err := visit(ev); err != nil {
			if errors.Is(err, connector.ErrStopStreaming) {
				return nil
			}
			return err
		}
	}
	return m.streamRunErr
}

func TestGrpcRunInput_HappyPath(t *testing.T) {
	mc := &interactiveMock{steerDelivered: true}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{
		RunId: "r_abc", Text: "focus on the failing test", Source: "should-be-ignored",
	})
	if err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if !resp.GetDelivered() || resp.GetRunId() != "r_abc" {
		t.Errorf("resp = %+v, want delivered=true run_id=r_abc", resp)
	}
	if mc.gotSteerText != "focus on the failing test" {
		t.Errorf("connector got text %q", mc.gotSteerText)
	}
	// Source is server-stamped (API), never the wire value.
	if mc.gotSteerSource == "should-be-ignored" || mc.gotSteerSource == "" {
		t.Errorf("source must be server-stamped, got %q", mc.gotSteerSource)
	}
}

func TestGrpcRunInput_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"not-in-flight", connector.ErrRunNotInFlight, codes.NotFound},
		{"queue-full", connector.ErrSteerQueueFull, codes.ResourceExhausted},
		{"unavailable", connector.ErrSteeringUnavailable, codes.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &interactiveMock{steerErr: tc.err}
			client, cleanup := startTestServerWithConnector(t, mc)
			defer cleanup()
			_, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{RunId: "r1", Text: "x"})
			if status.Code(err) != tc.want {
				t.Errorf("code = %v, want %v (err=%v)", status.Code(err), tc.want, err)
			}
		})
	}
}

func TestGrpcRunInput_RejectsEmpty(t *testing.T) {
	client, cleanup := startTestServerWithConnector(t, &interactiveMock{})
	defer cleanup()
	if _, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{Text: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty run_id: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{RunId: "r1", Text: "  "}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("blank text: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGrpcStreamRun_StreamsInteractiveEvents(t *testing.T) {
	mc := &interactiveMock{streamEvents: []providers.Event{
		{Type: providers.EventText, Text: "working"},
		{Type: providers.EventAwaitingInput, AwaitingInput: &providers.AwaitingInputEventInfo{SinceTurn: 3}},
		{Type: providers.EventSteer, UserInput: &providers.UserInputEventInfo{Text: "ship it", Source: "replay"}},
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	stream, err := client.StreamRun(context.Background(), &loomcyclepb.StreamRunRequest{RunId: "r_abc", FromSeq: 7})
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	var got []*loomcyclepb.Event
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, ev)
	}
	if mc.gotStreamRunID != "r_abc" || mc.gotStreamFrom != 7 {
		t.Errorf("connector got run_id=%q from_seq=%d", mc.gotStreamRunID, mc.gotStreamFrom)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	// The interactive payloads must round-trip as typed sub-messages.
	if got[1].GetType() != "awaiting_input" || got[1].GetAwaitingInput().GetSinceTurn() != 3 {
		t.Errorf("awaiting_input frame wrong: %+v", got[1])
	}
	if got[2].GetType() != "steer" || got[2].GetUserInput().GetText() != "ship it" || got[2].GetUserInput().GetSource() != "replay" {
		t.Errorf("steer frame wrong: %+v", got[2].GetUserInput())
	}
}
