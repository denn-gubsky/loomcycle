package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// moveToolTurn is a response that asks for one tool call, so the loop reaches
// another iteration — another call boundary.
func moveToolTurn(id string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "Noop", Input: json.RawMessage(`{}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1, Model: "m"}},
	}
}

// moveRun runs a three-call conversation: busy answers the first call with a
// tool call, and the run's remaining calls go wherever MoveForQueue sends it.
func moveRun(t *testing.T, policy FallbackPolicy, move func(context.Context, providers.Provider, string) (providers.Provider, string, string, bool), segments []PromptSegment) (busy, free *tieredProvider, asked int, events []providers.Event) {
	t.Helper()
	busy = &tieredProvider{id: "busy", responses: [][]providers.Event{moveToolTurn("t1"), moveToolTurn("t2"), successResponse()}}
	free = &tieredProvider{id: "free", responses: [][]providers.Event{moveToolTurn("t2"), successResponse()}}
	var mu sync.Mutex
	opts := RunOptions{
		Provider:      busy,
		Model:         "m-busy",
		MaxIterations: 6,
		Segments:      segments,
		Dispatcher: tools.NewDispatcherWithFallback(nil, func(_ context.Context, _ string, _ json.RawMessage) (tools.Result, bool) {
			return tools.Result{Text: `{"ok":true}`}, true
		}),
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: policy,
		MoveForQueue: func(ctx context.Context, cur providers.Provider, model string) (providers.Provider, string, string, bool) {
			mu.Lock()
			asked++
			mu.Unlock()
			return move(ctx, cur, model)
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return busy, free, asked, events
}

func userText(s string) []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: s}}}}
}

// At the first call boundary after the start, the run moves to where
// MoveForQueue sends it, announces it as a provider_fallback with reason
// queue_wait, and is never asked again: a run moves at most once.
func TestRun_QueueAwareMoveHappensOnceAtACallBoundary(t *testing.T) {
	target := &tieredProvider{id: "free", responses: [][]providers.Event{moveToolTurn("t2"), successResponse()}}
	busy, _, asked, events := moveRun(t, FallbackPolicy{UserTierName: "default"},
		func(_ context.Context, _ providers.Provider, _ string) (providers.Provider, string, string, bool) {
			return target, "m-free", "", true
		}, userText("go"))
	if busy.calls != 1 || target.calls != 2 {
		t.Errorf("calls busy=%d free=%d, want 1 then the remaining 2 on free", busy.calls, target.calls)
	}
	if busy.calls != 1 {
		t.Errorf("busy served %d calls, want 1 (the first, before any boundary)", busy.calls)
	}
	if asked != 1 {
		t.Errorf("MoveForQueue asked %d times, want once (a run moves at most once)", asked)
	}
	var fb *providers.FallbackInfo
	for _, ev := range events {
		if ev.Type == providers.EventProviderFallback {
			fb = ev.Fallback
		}
	}
	if fb == nil || fb.Reason != fallbackReasonQueueWait || fb.FailedProvider != "busy" || fb.NewProvider != "free" || fb.NewModel != "m-free" {
		t.Fatalf("move event = %+v, want busy → free/m-free with reason queue_wait", fb)
	}
}

// moveRunTo is moveRun whose MoveForQueue always moves to the run's "free"
// provider; it returns how many calls each provider served.
func moveRunTo(t *testing.T, policy FallbackPolicy, segments []PromptSegment) (busyCalls, freeCalls, asked int, events []providers.Event) {
	t.Helper()
	target := &tieredProvider{id: "free", responses: [][]providers.Event{moveToolTurn("t2"), successResponse()}}
	busy, _, asked, events := moveRun(t, policy,
		func(_ context.Context, _ providers.Provider, _ string) (providers.Provider, string, string, bool) {
			return target, "m-free", "", true
		}, segments)
	return busy.calls, target.calls, asked, events
}

// An operator who pins runs to their provider after the first success
// (PinAfterSuccess) pins them against a queue-aware move too.
func TestRun_QueueAwareMoveRespectsPinAfterSuccess(t *testing.T) {
	busyCalls, freeCalls, _, _ := moveRunTo(t, FallbackPolicy{PinAfterSuccess: true}, userText("go"))
	if busyCalls != 3 || freeCalls != 0 {
		t.Fatalf("calls busy=%d free=%d, want all 3 on busy (pinned after the first success)", busyCalls, freeCalls)
	}
}

// A run carrying an image does not move to a provider that cannot take it.
func TestRun_QueueAwareMoveRefusesATargetWithoutVision(t *testing.T) {
	img := []PromptSegment{{Role: "user", Content: []PromptContentBlock{
		{Type: "trusted-text", Text: "look"},
		{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgo="},
	}}}
	busy := &tieredProvider{id: "busy", supportsVision: true, responses: [][]providers.Event{moveToolTurn("t1"), moveToolTurn("t2"), successResponse()}}
	textOnly := &tieredProvider{id: "text-only", responses: [][]providers.Event{successResponse()}}
	var suppressed bool
	opts := RunOptions{
		Provider: busy, Model: "m", MaxIterations: 6, Segments: img,
		Dispatcher: tools.NewDispatcherWithFallback(nil, func(_ context.Context, _ string, _ json.RawMessage) (tools.Result, bool) {
			return tools.Result{Text: `{"ok":true}`}, true
		}),
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventFallbackSuppressed {
				suppressed = true
			}
		},
		MoveForQueue: func(context.Context, providers.Provider, string) (providers.Provider, string, string, bool) {
			return textOnly, "m", "", true
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if textOnly.calls != 0 || busy.calls != 3 || !suppressed {
		t.Fatalf("busy=%d text-only=%d suppressed=%v, want every call on busy and the move suppressed", busy.calls, textOnly.calls, suppressed)
	}
}
