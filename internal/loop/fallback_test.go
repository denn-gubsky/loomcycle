package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// tieredProvider is a fakeProvider variant whose ID is configurable.
// The fallback tests need two distinct providers with different IDs
// so we can assert the EventProviderFallback payload carries the
// correct new_provider name.
type tieredProvider struct {
	id        string
	mu        sync.Mutex
	responses [][]providers.Event
	errors    []error // returned by Call() at the same index; nil → use responses[idx]
	calls     int
}

func (p *tieredProvider) ID() string                                     { return p.id }
func (p *tieredProvider) Probe(_ context.Context) error                  { return nil }
func (p *tieredProvider) ListModels(_ context.Context) ([]string, error) { return []string{"m"}, nil }
func (p *tieredProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *tieredProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.calls
	p.calls++
	if idx < len(p.errors) && p.errors[idx] != nil {
		return nil, p.errors[idx]
	}
	if idx >= len(p.responses) {
		return nil, fmt.Errorf("no scripted response at idx=%d for %s", idx, p.id)
	}
	ch := make(chan providers.Event, len(p.responses[idx]))
	for _, ev := range p.responses[idx] {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// successResponse returns the canonical "model said hi and stopped"
// event sequence — used as the terminal response after a fallback
// switches to the new provider.
func successResponse() []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: "hi from new provider"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1, Model: "m"}},
	}
}

// TestFallback_429SwitchesProviderAndEmitsEvent — the headline case:
// first Call returns "anthropic 429: rate limit", policy enables
// fallback, ReResolve returns a new provider; the next iteration
// succeeds against the new provider. EventProviderFallback carries
// the correct failed/new provider names.
func TestFallback_429SwitchesProviderAndEmitsEvent(t *testing.T) {
	failing := &tieredProvider{
		id:     "anthropic",
		errors: []error{fmt.Errorf("anthropic 429: rate limit exceeded")},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}

	var events []providers.Event
	var mu sync.Mutex

	opts := RunOptions{
		Provider:      failing,
		Model:         "claude-sonnet-4-6",
		MaxIterations: 5,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: FallbackPolicy{
			Enabled:      true,
			MaxAttempts:  3,
			UserTierName: "medium",
		},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-pro", "", nil
		},
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v (expected success after fallback)", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}

	mu.Lock()
	defer mu.Unlock()
	var fbEvent *providers.Event
	for i := range events {
		if events[i].Type == providers.EventProviderFallback {
			fbEvent = &events[i]
			break
		}
	}
	if fbEvent == nil {
		t.Fatal("no EventProviderFallback emitted")
	}
	if fbEvent.Fallback == nil {
		t.Fatal("EventProviderFallback emitted with nil Fallback payload")
	}
	if fbEvent.Fallback.FailedProvider != "anthropic" {
		t.Errorf("FailedProvider = %q, want anthropic", fbEvent.Fallback.FailedProvider)
	}
	if fbEvent.Fallback.NewProvider != "deepseek" {
		t.Errorf("NewProvider = %q, want deepseek", fbEvent.Fallback.NewProvider)
	}
	if fbEvent.Fallback.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", fbEvent.Fallback.Attempt)
	}
	if fbEvent.Fallback.UserTier != "medium" {
		t.Errorf("UserTier = %q, want medium", fbEvent.Fallback.UserTier)
	}
	if fbEvent.Fallback.Reason != "retryable" {
		t.Errorf("Reason = %q, want retryable", fbEvent.Fallback.Reason)
	}
	if !strings.Contains(fbEvent.Fallback.CauseError, "anthropic 429") {
		t.Errorf("CauseError = %q, want substring 'anthropic 429'", fbEvent.Fallback.CauseError)
	}
}

// TestFallback_DisabledPolicy_PropagatesError — free tier: enabled=false
// means a 429 returns the error to the caller, no switch attempted.
// Pins the cost-cap semantic.
func TestFallback_DisabledPolicy_PropagatesError(t *testing.T) {
	failing := &tieredProvider{
		id:     "gemini",
		errors: []error{fmt.Errorf("gemini 429: free tier exhausted")},
	}
	reResolveCalls := 0
	opts := RunOptions{
		Provider:       failing,
		Model:          "gemini-2.0-flash",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:        func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: false}, // free tier
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			reResolveCalls++
			return nil, "", "", nil
		},
	}
	_, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error to propagate when fallback disabled")
	}
	if !strings.Contains(err.Error(), "gemini 429") {
		t.Errorf("error = %q, want substring 'gemini 429'", err.Error())
	}
	if reResolveCalls != 0 {
		t.Errorf("ReResolve called %d times; want 0 (policy disabled)", reResolveCalls)
	}
}

// TestFallback_BudgetExhausted_PropagatesError — policy enabled, but
// after 3 successful switches a 4th 429 should propagate. Confirms
// MaxAttempts is the cumulative cap across providers.
func TestFallback_BudgetExhausted_PropagatesError(t *testing.T) {
	// Each tieredProvider returns 429 on every call so the loop
	// keeps trying to fall back. ReResolve hands out fresh providers
	// each time; the cap stops the cycle at attempt 3.
	makeFailing := func(id string) *tieredProvider {
		return &tieredProvider{
			id:     id,
			errors: []error{fmt.Errorf("%s 429: throttled", id)},
		}
	}
	providers0 := makeFailing("anthropic")
	providers1 := makeFailing("deepseek")
	providers2 := makeFailing("gemini")
	providers3 := makeFailing("ollama")

	queue := []*tieredProvider{providers1, providers2, providers3}
	reResolveCalls := 0

	opts := RunOptions{
		Provider:      providers0,
		Model:         "m0",
		MaxIterations: 10,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:       func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{
			Enabled:      true,
			MaxAttempts:  3,
			UserTierName: "high",
		},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			next := queue[reResolveCalls]
			reResolveCalls++
			return next, "m" + next.id, "", nil
		},
	}
	_, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error after budget exhausted")
	}
	// After the 3rd switch (now on providers3), the 4th 429 should
	// NOT trigger another switch (budget exhausted). The original
	// providers3 error propagates.
	if !strings.Contains(err.Error(), "ollama 429") {
		t.Errorf("expected ollama 429 (the final provider's error); got %v", err)
	}
	if reResolveCalls != 3 {
		t.Errorf("ReResolve called %d times; want 3 (MaxAttempts cap)", reResolveCalls)
	}
}

// TestFallback_PermanentError_PropagatesRegardlessOfPolicy — a 400 /
// 401 / 403 must NOT trigger fallback even when policy is enabled.
// These are configuration errors (bad payload, bad auth); cascading
// burns through every provider's quota for no benefit.
func TestFallback_PermanentError_PropagatesRegardlessOfPolicy(t *testing.T) {
	failing := &tieredProvider{
		id:     "anthropic",
		errors: []error{fmt.Errorf("anthropic 400: invalid request shape")},
	}
	reResolveCalls := 0
	opts := RunOptions{
		Provider:       failing,
		Model:          "claude-sonnet-4-6",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:        func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			reResolveCalls++
			t.Error("ReResolve was called for a 400 — should NOT happen")
			return nil, "", "", nil
		},
	}
	_, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error to propagate for permanent class")
	}
	if !strings.Contains(err.Error(), "anthropic 400") {
		t.Errorf("error = %q, want substring 'anthropic 400'", err.Error())
	}
	if reResolveCalls != 0 {
		t.Errorf("ReResolve called %d times; want 0 (permanent error)", reResolveCalls)
	}
}

// TestFallback_AnthropicToOther_EmitsCacheInvalidated — when the
// failing provider is anthropic and the new one isn't, the loop
// must emit EventCacheInvalidated alongside EventProviderFallback.
// Anthropic's cache_control breakpoints don't transfer; this event
// is the signal to cost-retro tooling that downstream tokens for
// this run are cache-cold.
func TestFallback_AnthropicToOther_EmitsCacheInvalidated(t *testing.T) {
	failing := &tieredProvider{
		id:     "anthropic",
		errors: []error{fmt.Errorf("anthropic 503: backend unavailable")},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}
	var events []providers.Event
	var mu sync.Mutex
	opts := RunOptions{
		Provider:      failing,
		Model:         "claude-sonnet-4-6",
		MaxIterations: 5,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "high"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-pro", "", nil
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	sawCacheEvent := false
	for _, ev := range events {
		if ev.Type == providers.EventCacheInvalidated {
			sawCacheEvent = true
			if !strings.Contains(ev.Text, "deepseek") {
				t.Errorf("cache event text = %q, want substring 'deepseek'", ev.Text)
			}
			break
		}
	}
	if !sawCacheEvent {
		t.Error("no EventCacheInvalidated emitted on anthropic→deepseek switch")
	}
}

// TestFallback_NonAnthropicSwitch_NoCacheEvent — switching from
// (e.g.) gemini to deepseek must NOT emit EventCacheInvalidated.
// Only Anthropic has operator-controlled cache_control breakpoints
// today; cache invalidation isn't meaningful for other providers.
func TestFallback_NonAnthropicSwitch_NoCacheEvent(t *testing.T) {
	failing := &tieredProvider{
		id:     "gemini",
		errors: []error{fmt.Errorf("gemini 503: backend unavailable")},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}
	var events []providers.Event
	var mu sync.Mutex
	opts := RunOptions{
		Provider:      failing,
		Model:         "gemini-2.5-pro",
		MaxIterations: 5,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "high"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-pro", "", nil
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Type == providers.EventCacheInvalidated {
			t.Errorf("EventCacheInvalidated emitted on gemini→deepseek switch (no anthropic cache to invalidate)")
		}
	}
}

// TestFallback_ReResolveFailure_PropagatesOriginalError — the
// resolver couldn't find another candidate (the user_tier's list
// is exhausted). The loop must surface the ORIGINAL provider error
// to the caller, not the resolver's "no candidates" error.
// Operators read the actual provider failure; the no-candidates
// outcome is downstream context.
func TestFallback_ReResolveFailure_PropagatesOriginalError(t *testing.T) {
	failing := &tieredProvider{
		id:     "anthropic",
		errors: []error{fmt.Errorf("anthropic 429: throttled")},
	}
	opts := RunOptions{
		Provider:       failing,
		Model:          "claude-sonnet-4-6",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:        func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return nil, "", "", errors.New("user_tier candidate list exhausted")
		},
	}
	_, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error when ReResolve fails")
	}
	// Must surface the ORIGINAL "anthropic 429", not the resolver's
	// "user_tier candidate list exhausted" — operator needs to see
	// what actually broke at the provider.
	if !strings.Contains(err.Error(), "anthropic 429") {
		t.Errorf("error = %q, want substring 'anthropic 429' (original provider failure)", err.Error())
	}
}

// TestFallback_StreamMidErrorTriggers — provider opens the SSE
// stream successfully but emits an EventError mid-stream. The
// fallback path must fire from the in-stream error too, not just
// the Call-layer error. Models 5xx-ing mid-response is a common
// shape that v0.8.2 needs to handle.
func TestFallback_StreamMidErrorTriggers(t *testing.T) {
	failing := &tieredProvider{
		id: "anthropic",
		responses: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "starting..."},
				{Type: providers.EventError, Error: "anthropic 503: stream interrupted"},
			},
		},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}
	opts := RunOptions{
		Provider:       failing,
		Model:          "claude-sonnet-4-6",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:        func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "high"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-pro", "", nil
		},
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v (expected fallback to handle mid-stream error)", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
}

// recordingProvider is a tieredProvider that captures every Request
// it receives, so cross-provider tests can assert what the new
// provider got after a fallback (specifically: was the Reasoning
// field stripped from prior assistant turns).
type recordingProvider struct {
	*tieredProvider
	mu       sync.Mutex
	requests []providers.Request
}

func (p *recordingProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return p.tieredProvider.Call(ctx, req)
}

func (p *recordingProvider) snapshotRequests() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]providers.Request, len(p.requests))
	copy(out, p.requests)
	return out
}

// TestFallback_ReasoningStrippedOnProviderSwitch — the regression
// test for the 2026-05-13 production bug. A run starts on a
// provider whose previous turn populated Message.Reasoning (here we
// inject it via PriorMessages to simulate a continuation). The
// initial provider 503s. Fallback to the new provider must zero the
// Reasoning field on every assistant message before sending the
// request, so DeepSeek-style "must be passed back" 400s can't fire.
func TestFallback_ReasoningStrippedOnProviderSwitch(t *testing.T) {
	failing := &tieredProvider{
		id:     "gemini",
		errors: []error{fmt.Errorf("gemini 503: This model is currently experiencing high demand.")},
	}
	healthy := &recordingProvider{
		tieredProvider: &tieredProvider{
			id:        "deepseek",
			responses: [][]providers.Event{successResponse()},
		},
	}

	// PriorMessages carries an assistant turn with Reasoning set —
	// simulating a continuation where the prior turn ran on a
	// thinking-capable provider and that turn's reasoning_content
	// lives in the transcript.
	prior := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "first prompt"}}},
		{
			Role:      "assistant",
			Content:   []providers.ContentBlock{{Type: "text", Text: "ok"}},
			Reasoning: "step 1: consider X\nstep 2: pick Y\nstep 3: respond Z",
		},
	}

	var events []providers.Event
	var mu sync.Mutex

	opts := RunOptions{
		Provider:       failing,
		Model:          "gemini-2.5-flash",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "second prompt"}}}},
		PriorMessages:  prior,
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "high"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-flash", "", nil
		},
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}

	// The recording provider should have received exactly one Call.
	requests := healthy.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("recordingProvider got %d Calls, want 1", len(requests))
	}
	// Assert NO assistant message in the request carries Reasoning.
	for i, m := range requests[0].Messages {
		if m.Role == "assistant" && m.Reasoning != "" {
			t.Errorf("messages[%d] (assistant) reached deepseek with Reasoning=%q — strip pass failed", i, m.Reasoning)
		}
	}

	// Assert EventReasoningInvalidated was emitted.
	mu.Lock()
	defer mu.Unlock()
	var reasoningEvent *providers.Event
	for i := range events {
		if events[i].Type == providers.EventReasoningInvalidated {
			reasoningEvent = &events[i]
			break
		}
	}
	if reasoningEvent == nil {
		t.Fatal("no EventReasoningInvalidated emitted")
	}
	if !strings.Contains(reasoningEvent.Text, "gemini to deepseek") {
		t.Errorf("EventReasoningInvalidated Text = %q, expected to mention gemini→deepseek switch", reasoningEvent.Text)
	}
	if !strings.Contains(reasoningEvent.Text, "1 assistant turn") {
		t.Errorf("EventReasoningInvalidated Text = %q, expected to mention 1 cleared assistant turn", reasoningEvent.Text)
	}
}

// TestFallback_NoReasoningStrip_SameProviderFamily — guard against
// a spurious EventReasoningInvalidated when no assistant turns had
// non-empty Reasoning. The event should ONLY fire when something
// was actually stripped.
func TestFallback_NoReasoningStrip_NothingToStrip(t *testing.T) {
	failing := &tieredProvider{
		id:     "anthropic",
		errors: []error{fmt.Errorf("anthropic 429: rate limit exceeded")},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}

	// PriorMessages with assistant turns that have NO Reasoning.
	prior := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "first prompt"}}},
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "ok"}}},
		// Reasoning field intentionally empty.
	}

	var events []providers.Event
	var mu sync.Mutex
	opts := RunOptions{
		Provider:       failing,
		Model:          "claude-sonnet-4-6",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "second prompt"}}}},
		PriorMessages:  prior,
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "high"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-pro", "", nil
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Type == providers.EventReasoningInvalidated {
			t.Errorf("spurious EventReasoningInvalidated emitted (Text=%q); nothing should have been stripped", ev.Text)
		}
	}
	// Sanity: EventProviderFallback should still fire — only the
	// reasoning-invalidated one is suppressed.
	saw := false
	for _, ev := range events {
		if ev.Type == providers.EventProviderFallback {
			saw = true
			break
		}
	}
	if !saw {
		t.Error("EventProviderFallback missing — fallback should still have fired")
	}
}

// TestFallback_PartialStreamReasoning_NeverReachesMessages — the
// in-stream error invariant: if a provider's stream begins emitting
// before erroring, any partial iterReasoning accumulator is dropped
// by the drain-and-continue path. The fallback's strip pass therefore
// never needs to handle partial-accumulation; we assert via the
// captured request that no Reasoning leaked through.
//
// Scenario: failing provider emits EventText then EventError mid-
// stream. Healthy provider records the request it receives. Even
// without any prior Reasoning in PriorMessages, the captured request
// must have empty Reasoning on all assistant messages (there should
// be none, since the failing turn's stream errored before EventDone).
func TestFallback_PartialStreamReasoning_NeverReachesMessages(t *testing.T) {
	failing := &tieredProvider{
		id: "gemini",
		responses: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "starting..."},
				{Type: providers.EventError, Error: "gemini 503: stream interrupted mid-response"},
			},
		},
	}
	healthy := &recordingProvider{
		tieredProvider: &tieredProvider{
			id:        "deepseek",
			responses: [][]providers.Event{successResponse()},
		},
	}
	opts := RunOptions{
		Provider:       failing,
		Model:          "gemini-2.5-flash",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:        func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "high"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-flash", "", nil
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	requests := healthy.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("recordingProvider got %d Calls, want 1", len(requests))
	}
	for i, m := range requests[0].Messages {
		if m.Role == "assistant" && m.Reasoning != "" {
			t.Errorf("messages[%d] reached deepseek with partial Reasoning=%q — drain-and-continue path leaked", i, m.Reasoning)
		}
	}
}

// TestFallback_PinAfterSuccess_SuppressesFallbackOnTurnTwo —
// headline regression for the 2026-05-13 cv-batch-adapter failure.
// Provider A turn 1 succeeds (appends an assistant message). Turn 2
// returns a retryable error. With PinAfterSuccess=true, the
// fallback path MUST NOT switch providers — the original error
// propagates, and EventFallbackSuppressed is emitted for observability.
//
// Without PinAfterSuccess this is the existing happy-path of
// TestFallback_StreamMidErrorTriggers, which we don't break.
func TestFallback_PinAfterSuccess_SuppressesFallbackOnTurnTwo(t *testing.T) {
	failing := &tieredProvider{
		id: "gemini",
		responses: [][]providers.Event{
			// Turn 1 succeeds with a tool_call. Loop appends the
			// assistant message; firstTurnSucceeded flips true.
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu-1", Name: "Read", Input: []byte(`{"path":"x"}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 5, Model: "gemini-2.5-flash"}},
			},
		},
		// Turn 2: scripted error — gemini 503.
		errors: []error{nil, fmt.Errorf("gemini 503: This model is currently experiencing high demand.")},
	}
	healthy := &recordingProvider{
		tieredProvider: &tieredProvider{
			id:        "deepseek",
			responses: [][]providers.Event{successResponse()},
		},
	}

	var events []providers.Event
	var mu sync.Mutex

	opts := RunOptions{
		Provider:      failing,
		Model:         "gemini-2.5-flash",
		MaxIterations: 5,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		// A fallback-only dispatcher that returns a stub tool_result
		// so the loop reaches a second iteration after turn 1.
		Dispatcher: tools.NewDispatcherWithFallback(nil, func(_ context.Context, _ string, _ json.RawMessage) (tools.Result, bool) {
			return tools.Result{Text: `{"ok":true}`}, true
		}),
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		FallbackPolicy: FallbackPolicy{
			Enabled:         true,
			MaxAttempts:     3,
			UserTierName:    "high",
			PinAfterSuccess: true, // the policy under test
		},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-flash", "", nil
		},
	}
	_, err := Run(context.Background(), opts)
	// Run must fail with the gemini 503 (NOT recover via fallback).
	if err == nil {
		t.Fatal("expected Run to fail with gemini 503; got nil (fallback fired despite PinAfterSuccess)")
	}
	if !strings.Contains(err.Error(), "gemini 503") {
		t.Errorf("error = %q, want substring 'gemini 503'", err.Error())
	}

	// recordingProvider must have received ZERO calls — the
	// fallback was suppressed.
	if requests := healthy.snapshotRequests(); len(requests) != 0 {
		t.Errorf("recordingProvider got %d calls, want 0 (fallback should have been suppressed)", len(requests))
	}

	// EventFallbackSuppressed must have been emitted.
	mu.Lock()
	defer mu.Unlock()
	var suppressedEvent *providers.Event
	for i := range events {
		if events[i].Type == providers.EventFallbackSuppressed {
			suppressedEvent = &events[i]
			break
		}
	}
	if suppressedEvent == nil {
		t.Fatal("no EventFallbackSuppressed emitted")
	}
	if !strings.Contains(suppressedEvent.Text, "gemini") {
		t.Errorf("EventFallbackSuppressed Text = %q, expected to mention the pinned gemini provider", suppressedEvent.Text)
	}
	// EventProviderFallback must NOT have been emitted — the
	// suppression intercepts before the switch.
	for _, ev := range events {
		if ev.Type == providers.EventProviderFallback {
			t.Errorf("EventProviderFallback emitted despite PinAfterSuccess + firstTurnSucceeded; payload=%+v", ev.Fallback)
		}
	}
}

// TestFallback_PinAfterSuccess_TurnOneStillFallsBack — the
// initial-pick resilience case. With PinAfterSuccess=true, a
// retryable error on TURN ZERO (before any assistant message has
// been appended) MUST still fall back. This is the stale-probe
// safety net: if the resolver picked a provider that's actually
// stalled at request time, the run should survive the first call.
func TestFallback_PinAfterSuccess_TurnOneStillFallsBack(t *testing.T) {
	failing := &tieredProvider{
		id:     "anthropic",
		errors: []error{fmt.Errorf("anthropic 429: rate limit exceeded")},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}

	opts := RunOptions{
		Provider:      failing,
		Model:         "claude-haiku-4-5",
		MaxIterations: 5,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:       func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{
			Enabled:         true,
			MaxAttempts:     3,
			UserTierName:    "high",
			PinAfterSuccess: true,
		},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-flash", "", nil
		},
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v (expected success after turn-zero fallback)", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
}

// TestFallback_PinAfterSuccess_FlagOff_PreservesV082Behavior —
// belt-and-suspenders. When PinAfterSuccess=false (the v0.8.x
// default), the loop's behavior is identical to v0.8.2: a
// retryable error on turn N>1 still fires fallback. Guards
// against accidentally enabling pinning for everyone.
func TestFallback_PinAfterSuccess_FlagOff_PreservesV082Behavior(t *testing.T) {
	failing := &tieredProvider{
		id: "gemini",
		responses: [][]providers.Event{
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu-1", Name: "Read", Input: []byte(`{"path":"x"}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 5, Model: "gemini-2.5-flash"}},
			},
		},
		errors: []error{nil, fmt.Errorf("gemini 503: high demand")},
	}
	healthy := &tieredProvider{
		id:        "deepseek",
		responses: [][]providers.Event{successResponse()},
	}

	opts := RunOptions{
		Provider:      failing,
		Model:         "gemini-2.5-flash",
		MaxIterations: 5,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		Dispatcher: tools.NewDispatcherWithFallback(nil, func(_ context.Context, _ string, _ json.RawMessage) (tools.Result, bool) {
			return tools.Result{Text: `{"ok":true}`}, true
		}),
		OnEvent: func(ev providers.Event) {},
		FallbackPolicy: FallbackPolicy{
			Enabled:         true,
			MaxAttempts:     3,
			UserTierName:    "high",
			PinAfterSuccess: false, // explicitly off
		},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4-flash", "", nil
		},
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v (expected success — fallback should fire when flag is off)", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
}

