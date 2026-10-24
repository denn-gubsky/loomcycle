package loop

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A request the provider rejects as malformed or unauthorised says nothing
// about the model: the same model serves every other run. Marking it stalled
// took it from all of them until the next probe — a 400 from one agent's
// sampling setting once left a whole tier answering 503.
func TestRun_PermanentRequestErrorDoesNotMarkStalled(t *testing.T) {
	for _, msg := range []string{
		"anthropic 400: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"`temperature` is deprecated for this model.\"}}",
		"anthropic 401: invalid x-api-key",
		"anthropic 403: permission denied",
		"openai 422: unprocessable",
	} {
		stalls := 0
		_, err := Run(context.Background(), RunOptions{
			Provider:    &erroringProvider{err: fmt.Errorf("%s", msg)},
			Model:       "claude-haiku-5-5",
			Segments:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
			MarkStalled: func(_, _, _ string) { stalls++ },
		})
		if err == nil {
			t.Fatalf("%s: Run should propagate the error", msg)
		}
		if stalls != 0 {
			t.Errorf("%s: MarkStalled called %d times, want 0", msg, stalls)
		}
	}
}

// A 404 names a model the provider does not have, which is true for every
// run, so it keeps stalling the model as it always has.
func TestRun_ModelNotFound404StillMarksStalled(t *testing.T) {
	stalls := 0
	_, _ = Run(context.Background(), RunOptions{
		Provider:    &erroringProvider{err: fmt.Errorf("anthropic 404: model: claude-nope not found")},
		Model:       "claude-nope",
		Segments:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
		MarkStalled: func(_, _, _ string) { stalls++ },
	})
	if stalls != 1 {
		t.Errorf("MarkStalled calls = %d, want 1 (a missing model is unusable for every run)", stalls)
	}
}

// The in-stream error path makes the same decision as the Call() error path.
func TestRun_StreamEventError400DoesNotMarkStalled(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{{
		{Type: providers.EventError, Error: "anthropic 400: invalid_request_error mid-stream"},
		{Type: providers.EventDone, StopReason: "error"},
	}}}
	stalls := 0
	_, _ = Run(context.Background(), RunOptions{
		Provider:    prov,
		Model:       "claude-haiku-5-5",
		Segments:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
		MarkStalled: func(_, _, _ string) { stalls++ },
	})
	if stalls != 0 {
		t.Errorf("MarkStalled calls = %d, want 0 (an in-stream 400 is a request error)", stalls)
	}
}

// The stateful loop's recovery applies the same rule: a 400 leaves the model
// alone, a 5xx still stalls it.
func TestRecoverStatefulCall_StallsOnlyOnModelFaults(t *testing.T) {
	for _, tc := range []struct {
		msg   string
		stall int
	}{
		{"anthropic 400: invalid_request_error", 0},
		{"anthropic 500: upstream timeout", 1},
	} {
		stalls := 0
		opts := &RunOptions{
			Provider:    &erroringProvider{},
			Model:       "claude-haiku-5-5",
			MarkStalled: func(_, _, _ string) { stalls++ },
		}
		_ = recoverStatefulCall(context.Background(), opts, &statefulRecovery{}, fmt.Errorf("%s", tc.msg),
			func(providers.Event) {}, nil)
		if stalls != tc.stall {
			t.Errorf("%s: MarkStalled calls = %d, want %d", tc.msg, stalls, tc.stall)
		}
	}
}

// A refusal of the operator's own key takes the whole provider out — every
// model on it shares that key — and does not stall the one model. A tenant's
// key refused touches nothing shared.
func TestRun_OperatorKeyRefusalMarksTheProviderUnreachable(t *testing.T) {
	for _, tc := range []struct {
		source      string
		unreachable int
	}{{"operator", 1}, {"tenant", 0}, {"user", 0}} {
		var stalls, down int
		var downProvider string
		_, err := Run(context.Background(), RunOptions{
			Provider:    &erroringProvider{err: providers.WithKeySource(fmt.Errorf("anthropic 401: invalid x-api-key"), tc.source)},
			Model:       "claude-haiku-5-5",
			Segments:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
			MarkStalled: func(_, _, _ string) { stalls++ },
			MarkProviderUnreachable: func(p, _ string) {
				down++
				downProvider = p
			},
		})
		if err == nil {
			t.Fatalf("%s: Run should propagate the 401", tc.source)
		}
		if down != tc.unreachable || stalls != 0 {
			t.Errorf("%s key 401: MarkProviderUnreachable=%d MarkStalled=%d, want %d and 0", tc.source, down, stalls, tc.unreachable)
		}
		if tc.unreachable == 1 && downProvider != "erroring" {
			t.Errorf("provider marked unreachable = %q, want erroring", downProvider)
		}
	}
}

// The stateful loop's recovery reports the operator-key refusal the same way.
func TestRecoverStatefulCall_OperatorKeyRefusalMarksTheProviderUnreachable(t *testing.T) {
	down := 0
	opts := &RunOptions{
		Provider:                &erroringProvider{},
		Model:                   "claude-haiku-5-5",
		MarkProviderUnreachable: func(_, _ string) { down++ },
	}
	_ = recoverStatefulCall(context.Background(), opts, &statefulRecovery{},
		providers.WithKeySource(fmt.Errorf("anthropic 403: forbidden"), "operator"), func(providers.Event) {}, nil)
	if down != 1 {
		t.Errorf("MarkProviderUnreachable calls = %d, want 1", down)
	}
}
