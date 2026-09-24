package grpc

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC DJ over gRPC: the verdict RPC, the review arming on every run path, and
// the held event's payload.

func TestReviewRun_DispatchesTheVerdict(t *testing.T) {
	mc := &parityMock{reviewDelivered: true}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.ReviewRun(context.Background(), &loomcyclepb.ReviewRunRequest{
		RunId: "run-1", Decision: "reject", Feedback: "cover the rollback",
	})
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	want := reviewCall{"run-1", "reject", "cover the rollback", store.InterruptResolvedByAPI}
	if mc.lastReview != want {
		t.Errorf("connector saw %+v, want %+v", mc.lastReview, want)
	}
	if !resp.GetDelivered() || resp.GetRunId() != "run-1" || resp.GetDecision() != "reject" {
		t.Errorf("resp = %+v", resp)
	}
}

// Each refusal reaches gRPC as the code matching its HTTP status.
func TestReviewRun_RefusalsMapToCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{connector.ErrRunNotInFlight, codes.NotFound},
		{connector.ErrRunNotHeld, codes.FailedPrecondition},
		{fmt.Errorf("%w: feedback is only read with reject", connector.ErrInvalidReviewDecision), codes.InvalidArgument},
		{connector.ErrSteerQueueFull, codes.ResourceExhausted},
		{connector.ErrSteeringUnavailable, codes.Unavailable},
	} {
		mc := &parityMock{reviewErr: tc.err}
		client, cleanup := startTestServerWithConnector(t, mc)
		_, err := client.ReviewRun(context.Background(), &loomcyclepb.ReviewRunRequest{RunId: "run-1", Decision: "approve"})
		cleanup()
		if status.Code(err) != tc.want {
			t.Errorf("%v → %s, want %s", tc.err, status.Code(err), tc.want)
		}
	}
	mc := &parityMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	if _, err := client.ReviewRun(context.Background(), &loomcyclepb.ReviewRunRequest{RunId: "bad id!", Decision: "approve"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad run_id → %s, want InvalidArgument", status.Code(err))
	}
	if mc.lastReview.runID != "" {
		t.Error("a malformed run_id reached the connector")
	}
}

// A verdict is a run write, gated like steering.
func TestReviewRun_IsRunsCreate(t *testing.T) {
	if got, ok := grpcConsumerScopes["ReviewRun"]; !ok || got != grpcConsumerScopes["RunInput"] {
		t.Errorf("ReviewRun scope = %q (mapped %v), want RunInput's %q", got, ok, grpcConsumerScopes["RunInput"])
	}
}

// review reaches the runner from both run RPCs, and a draft or spawn from the
// spawn shape.
func TestRun_ReviewReachesTheRunner(t *testing.T) {
	fr := &fakeRunner{registered: registrationFrame{AgentID: "a", RunID: "r", SessionID: "s"}}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()
	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default", Review: true})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	if !fr.lastInput.Review {
		t.Error("Run: review did not reach RunInput")
	}

	cs, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{SessionId: "s", Review: true})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, cs)
	if !fr.lastInput.Review {
		t.Error("Continue: review did not reach RunInput")
	}

	if r := spawnRequestFromProto(&loomcyclepb.RunRequest{Review: true}); r.Review == nil || !*r.Review {
		t.Errorf("spawnRequestFromProto dropped review: %v", r.Review)
	}
	if r := spawnRequestFromProto(&loomcyclepb.RunRequest{}); r.Review != nil {
		t.Errorf("an unarmed spawn carries review %v, want absent", *r.Review)
	}
}

// review is an override on both retune paths, with false (disarm, which
// releases a held run) distinct from unset.
func TestGrpcRetune_CarriesReview(t *testing.T) {
	mc := &interactiveMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	no := false
	if _, err := client.RetuneRun(context.Background(), &loomcyclepb.RetuneRunRequest{RunId: "r_abc", Review: &no}); err != nil {
		t.Fatalf("RetuneRun: %v", err)
	}
	if mc.gotRetuneOv.Review == nil || *mc.gotRetuneOv.Review {
		t.Errorf("retune review = %v, want a set false", mc.gotRetuneOv.Review)
	}
	yes := true
	if _, err := client.RunInput(context.Background(), &loomcyclepb.RunInputRequest{RunId: "r_abc", Text: "go on", Review: &yes}); err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if mc.gotRetuneOv.Review == nil || !*mc.gotRetuneOv.Review {
		t.Errorf("run-input review = %v, want a set true", mc.gotRetuneOv.Review)
	}
}

func TestEventToProto_CarriesTheAwaitingReviewPayload(t *testing.T) {
	out := eventToProto(providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: 4, Round: 2}})
	if out.GetType() != "awaiting_review" || out.GetAwaitingReview().GetSinceTurn() != 4 || out.GetAwaitingReview().GetRound() != 2 {
		t.Errorf("proto = %+v", out)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "x"}).GetAwaitingReview() != nil {
		t.Error("a text frame carries an awaiting_review payload")
	}
}

// The review deadline reaches the runner from both run RPCs and the spawn
// shape, and the held event carries when it expires.
func TestRun_ReviewDeadlineReachesTheRunner(t *testing.T) {
	fr := &fakeRunner{registered: registrationFrame{AgentID: "a", RunID: "r", SessionID: "s"}}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()
	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default", Review: true, ReviewTtlSeconds: 90})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	if fr.lastInput.ReviewTTLSeconds != 90 {
		t.Errorf("Run: review_ttl_seconds = %d, want 90", fr.lastInput.ReviewTTLSeconds)
	}
	cs, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{SessionId: "s", ReviewTtlSeconds: 45})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, cs)
	if fr.lastInput.ReviewTTLSeconds != 45 {
		t.Errorf("Continue: review_ttl_seconds = %d, want 45", fr.lastInput.ReviewTTLSeconds)
	}
	if r := spawnRequestFromProto(&loomcyclepb.RunRequest{ReviewTtlSeconds: 30}); r.ReviewTTLSeconds != 30 {
		t.Errorf("spawnRequestFromProto: review_ttl_seconds = %d", r.ReviewTTLSeconds)
	}
	out := eventToProto(providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{Round: 1, ExpiresAt: "2026-09-24T12:00:00Z"}})
	if out.GetAwaitingReview().GetExpiresAt() != "2026-09-24T12:00:00Z" {
		t.Errorf("expires_at = %q", out.GetAwaitingReview().GetExpiresAt())
	}
}
