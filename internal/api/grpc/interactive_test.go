package grpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

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

	gotRetuneRunID string
	gotRetuneOv    connector.RunOverrides
	retuneCalls    int
	retuneErr      error

	gotStreamRunID string
	gotStreamFrom  int64
	streamEvents   []providers.Event
	streamRunErr   error
}

func (m *interactiveMock) SteerRun(_ context.Context, runID, text, source string) (bool, error) {
	m.gotSteerRunID, m.gotSteerText, m.gotSteerSource = runID, text, source
	return m.steerDelivered, m.steerErr
}

func (m *interactiveMock) RetuneRun(_ context.Context, runID string, ov connector.RunOverrides) error {
	m.gotRetuneRunID, m.gotRetuneOv = runID, ov
	m.retuneCalls++
	return m.retuneErr
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

// The gap a consumer reported: RunInputRequest carried run_id/text/source only,
// so a gRPC or Python caller could not retune a run at all — and RunInput
// requires text, so even once the fields existed a retune could only ride a
// turn. These cover both halves.
func TestGrpcRetuneRun_ChangesTheRunWithoutDeliveringATurn(t *testing.T) {
	mc := &interactiveMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	yes, n := true, int32(7)
	resp, err := client.RetuneRun(context.Background(), &loomcyclepb.RetuneRunRequest{
		RunId: "r_abc", Model: "model-b", MaxIterations: 40,
		// The meaningful zeros: proto3 optional precisely so "off" is
		// expressible, and a non-pointer would read as unset right here.
		RetryAttempts: proto.Int32(0), InjectToolGuide: proto.Bool(false),
		UnboundedIterations: &yes, MemoryInjectMaxTokens: &n,
	})
	if err != nil {
		t.Fatalf("RetuneRun: %v", err)
	}
	if !resp.GetRetuned() || resp.GetRunId() != "r_abc" {
		t.Errorf("resp = %+v, want retuned=true run_id=r_abc", resp)
	}
	if mc.gotRetuneOv.Model != "model-b" || mc.gotRetuneOv.MaxIterations != 40 {
		t.Errorf("connector got %+v, want model-b / 40", mc.gotRetuneOv)
	}
	if mc.gotRetuneOv.RetryAttempts == nil || *mc.gotRetuneOv.RetryAttempts != 0 {
		t.Errorf("retry_attempts = %v, want a set 0 — the meaningful zero was dropped",
			mc.gotRetuneOv.RetryAttempts)
	}
	if mc.gotRetuneOv.InjectToolGuide == nil || *mc.gotRetuneOv.InjectToolGuide {
		t.Errorf("inject_tool_guide = %v, want a set false", mc.gotRetuneOv.InjectToolGuide)
	}
	// No turn: the whole reason this RPC is separate from RunInput.
	if mc.gotSteerText != "" {
		t.Errorf("a retune delivered the turn %q — it must not steer", mc.gotSteerText)
	}
}

func TestGrpcRetuneRun_RefusesAnEmptyOverrideSet(t *testing.T) {
	mc := &interactiveMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.RetuneRun(context.Background(), &loomcyclepb.RetuneRunRequest{RunId: "r_abc"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty override set → %v, want InvalidArgument (the HTTP 422)", status.Code(err))
	}
	if mc.retuneCalls != 0 {
		t.Error("the connector was called for a request carrying no override")
	}
}

// Retune-and-speak in one call, and the ORDER matters: the loop re-reads the
// run's configuration when the operator's turn arrives, so a retune applied
// after the text lands one turn late.
func TestGrpcRunInput_AppliesOverridesBeforeDeliveringTheText(t *testing.T) {
	mc := &interactiveMock{steerDelivered: true}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	if _, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{
		RunId: "r_abc", Text: "carry on", Model: "model-b",
	}); err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if mc.gotRetuneOv.Model != "model-b" {
		t.Errorf("the retune did not reach the connector: %+v", mc.gotRetuneOv)
	}
	if mc.gotSteerText != "carry on" {
		t.Errorf("the text was not delivered: %q", mc.gotSteerText)
	}
}

// A steer with no overrides must not call the retune path at all — otherwise
// every ordinary steer pays a store write for a change nobody asked for.
func TestGrpcRunInput_WithoutOverridesDoesNotRetune(t *testing.T) {
	mc := &interactiveMock{steerDelivered: true}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	if _, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{
		RunId: "r_abc", Text: "plain steer",
	}); err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if mc.retuneCalls != 0 {
		t.Errorf("a steer with no overrides called RetuneRun %d times", mc.retuneCalls)
	}
}

// metadata, context and parent_context were absent from the gRPC wire entirely,
// so a gRPC or Python caller could not send agent metadata, could not set the
// per-run context block, and could not carry cost-attribution lineage — all
// three reachable from HTTP since they shipped.
func TestRunInputFromProto_CarriesMetadataContextAndLineage(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"step":{"type":"integer"}}}`)
	keep := int32(6)
	mode := "stateful"

	in := runInputFromProto(runInputProtoArgs{
		Agent:    "a",
		Metadata: metadataFromProto([]byte(`{"repo":"loomcycle","reviewers":2}`)),
		Context: contextFromProto(&loomcyclepb.Context{
			Mode: &mode, KeepLastN: &keep, StateSchema: schema,
		}),
		ParentContext: parentContextFromProto(&loomcyclepb.ParentContext{
			RootAgentRunId: "r_root", FunctionKey: "cv", TierAtRun: "pro",
		}),
	})

	if in.Metadata["repo"] != "loomcycle" {
		t.Errorf("metadata = %v, want repo=loomcycle", in.Metadata)
	}
	if in.Context == nil || in.Context.Mode == nil || *in.Context.Mode != "stateful" {
		t.Fatalf("context = %+v, want mode=stateful", in.Context)
	}
	if in.Context.KeepLastN == nil || *in.Context.KeepLastN != 6 {
		t.Errorf("keep_last_n = %v, want 6", in.Context.KeepLastN)
	}
	// The one free-form field: a stateful run validates every patch against it,
	// so losing it turns a validated mode into an unvalidated one.
	if in.Context.StateSchema["type"] != "object" {
		t.Errorf("state_schema = %v, want the decoded JSON-Schema", in.Context.StateSchema)
	}
	if in.ParentContext == nil || in.ParentContext.RootAgentRunID != "r_root" {
		t.Errorf("parent_context = %+v, want root r_root", in.ParentContext)
	}
}

// nil in, nil out. "The caller said nothing" has to reach the runner as
// inherit-the-agent's-block, never as a zero-valued Context that overrides it
// with emptiness — the meaningful-zero problem in object form.
func TestRunInputFromProto_AbsentBlocksStayNil(t *testing.T) {
	in := runInputFromProto(runInputProtoArgs{
		Agent:         "a",
		Metadata:      metadataFromProto(nil),
		Context:       contextFromProto(nil),
		ParentContext: parentContextFromProto(nil),
	})
	if in.Metadata != nil {
		t.Errorf("metadata = %v, want nil", in.Metadata)
	}
	if in.Context != nil {
		t.Errorf("context = %+v, want nil", in.Context)
	}
	if in.ParentContext != nil {
		t.Errorf("parent_context = %+v, want nil", in.ParentContext)
	}
	// An all-empty lineage block normalises to nil too, so the echo surfaces
	// omit it rather than reporting an empty object — matching handleRuns.
	if got := parentContextFromProto(&loomcyclepb.ParentContext{}); got != nil {
		t.Errorf("an all-empty parent_context = %+v, want nil", got)
	}
}

// Malformed metadata must not fail an otherwise-valid run: it is an advisory
// prompt block, and refusing the run over it is the worse trade.
func TestMetadataFromProto_MalformedIsDroppedNotFatal(t *testing.T) {
	if got := metadataFromProto([]byte(`not json`)); got != nil {
		t.Errorf("malformed metadata = %v, want nil (dropped)", got)
	}
}
