package grpc

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// An interruption_pending frame carries the question (RFC HG D6), so a gRPC
// client can show what the run asked and offer an "other" answer — before it
// the frame had no question at all.
func TestEventToProto_InterruptionPendingCarriesTheQuestion(t *testing.T) {
	expires := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	out := eventToProto(providers.Event{Type: providers.EventInterruptionPending, Interruption: &providers.InterruptionEventInfo{
		InterruptID: "intr_1", Kind: "question", Question: "Which?",
		Options: json.RawMessage(`["A","B"]`), AllowOther: true,
		Context: "two left", Priority: "high", ExpiresAt: expires.Format(time.RFC3339Nano),
	}})
	in := out.GetInterruption()
	if out.GetType() != "interruption_pending" || in == nil {
		t.Fatalf("event = %+v", out)
	}
	if in.GetInterruptId() != "intr_1" || in.GetKind() != "question" || in.GetQuestion() != "Which?" ||
		!slices.Equal(in.GetOptions(), []string{"A", "B"}) || !in.GetAllowOther() ||
		in.GetContext() != "two left" || in.GetPriority() != "high" || !in.GetExpiresAt().AsTime().Equal(expires) {
		t.Errorf("interruption = %+v", in)
	}

	free := eventToProto(providers.Event{Type: providers.EventInterruptionPending, Interruption: &providers.InterruptionEventInfo{
		InterruptID: "intr_2", Kind: "question", Question: "Why?",
	}}).GetInterruption()
	if len(free.GetOptions()) != 0 || free.GetAllowOther() || free.GetExpiresAt() != nil {
		t.Errorf("free-text question = %+v, want no options, no allow_other, no expiry", free)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "hi"}).GetInterruption() != nil {
		t.Error("a text frame carries an interruption")
	}
}
