package loop

import (
	"context"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// sharedUsageProvider hands every call the SAME *Usage — what a test fixture
// built once, or a driver that caches its reply, does. Nothing in the Provider
// contract says the usage a call returns is the caller's to write.
type sharedUsageProvider struct {
	usage *providers.Usage
	input []byte // non-nil: answer with an emit_state tool call (stateful path)
}

func (p *sharedUsageProvider) ID() string                    { return "shared" }
func (p *sharedUsageProvider) Probe(_ context.Context) error { return nil }
func (p *sharedUsageProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"shared-model"}, nil
}
func (p *sharedUsageProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: 8192}
}
func (p *sharedUsageProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	if p.input != nil {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "t1", Name: emitStateToolName, Input: p.input}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: p.usage}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: p.usage}
	}
	close(ch)
	return ch, nil
}

// The loop stamps the serving provider, the context window and the call timing
// onto each call's usage. It must stamp a copy: the provider's value may be
// shared, and two runs writing it at once was a data race (seen under -race in
// TestProviderLane_FanoutParentAndSameProviderChildrenStayWithinTheCap, whose
// scripted provider hands every child the same *Usage). The runs here run
// concurrently so -race also sees the conflict; the equality check fails the
// unfixed code without it.
func TestRun_StampsACopyOfTheProvidersUsage(t *testing.T) {
	shared := &providers.Usage{InputTokens: 3, OutputTokens: 2}
	want := *shared
	p := &sharedUsageProvider{usage: shared}

	const runs = 4
	got := make([]*providers.Usage, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Run(context.Background(), RunOptions{
				Provider: p,
				Model:    "shared-model",
				Segments: []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
				OnEvent: func(ev providers.Event) {
					if ev.Type == providers.EventUsage {
						got[i] = ev.Usage
					}
				},
			})
			if err != nil {
				t.Errorf("run %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if *shared != want {
		t.Errorf("the provider's usage was written by the loop: %+v, want %+v", *shared, want)
	}
	for i, u := range got {
		if u == nil {
			t.Errorf("run %d: no usage event", i)
			continue
		}
		if u == shared {
			t.Errorf("run %d: the usage event carries the provider's own pointer", i)
		}
		// The stamps still land — on the event the loop emits.
		if u.Provider != "shared" || u.MaxContextTokens != 8192 || u.Timing == nil || u.InputTokens != 3 {
			t.Errorf("run %d: usage event = %+v, want it stamped with provider, window and timing", i, *u)
		}
	}
}

// The stateful loop's own call stamps the timing the same way, and must not
// stamp the provider's value either.
func TestCallForEmitState_StampsACopyOfTheProvidersUsage(t *testing.T) {
	shared := &providers.Usage{InputTokens: 5, OutputTokens: 1}
	want := *shared
	p := &sharedUsageProvider{usage: shared, input: []byte(`{"patch":{}}`)}

	const calls = 4
	got := make([]*providers.Usage, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call, err := callForEmitState(context.Background(), p, providers.Request{Model: "shared-model"}, nil)
			if err != nil {
				t.Errorf("call %d: %v", i, err)
			}
			got[i] = call.usage
		}()
	}
	wg.Wait()

	if *shared != want {
		t.Errorf("the provider's usage was written: %+v, want %+v", *shared, want)
	}
	for i, u := range got {
		if u == nil || u == shared || u.Timing == nil || u.InputTokens != 5 {
			t.Errorf("call %d: usage = %+v, want a stamped copy", i, u)
		}
	}
}
