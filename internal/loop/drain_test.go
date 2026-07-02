package loop

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// errorThenTrailProvider emits an EventError and then MORE events (as a driver
// may — a trailing EventDone). senderDone closes only once every event has been
// consumed, so the test can detect whether the loop drained the channel.
type errorThenTrailProvider struct {
	senderDone chan struct{}
}

func (p *errorThenTrailProvider) ID() string                  { return "trail" }
func (p *errorThenTrailProvider) Probe(context.Context) error { return nil }
func (p *errorThenTrailProvider) ListModels(context.Context) ([]string, error) {
	return []string{"m"}, nil
}
func (p *errorThenTrailProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *errorThenTrailProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event) // unbuffered → a trailing send blocks unless the loop drains
	go func() {
		defer close(p.senderDone)
		send := func(ev providers.Event) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done(): // cleanup safety net so a leaked goroutine still exits
				return false
			}
		}
		if !send(providers.Event{Type: providers.EventError, Error: "trail 400: bad request"}) {
			return
		}
		// Trailing events after the error — the loop MUST consume these.
		for i := 0; i < 5; i++ {
			if !send(providers.Event{Type: providers.EventText, Text: "x"}) {
				return
			}
		}
		_ = send(providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
		close(ch)
	}()
	return ch, nil
}

// TestRun_DrainsChannelOnTerminalError is the regression for the undrained
// terminal EventError path: the loop returned after a non-retryable, no-fallback
// in-stream error WITHOUT draining the provider channel, so a driver still
// emitting events blocked its goroutine (bounded only by ctx-cancel / idle
// timeout). The loop must drain — like the retry + fallback paths do.
func TestRun_DrainsChannelOnTerminalError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // unblock any leaked goroutine at test end (hygiene)

	p := &errorThenTrailProvider{senderDone: make(chan struct{})}
	opts := RunOptions{
		Provider:       p,
		Model:          "m",
		MaxIterations:  3,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		OnEvent:        func(providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: false}, // no fallback → terminal error path
	}
	if _, err := Run(ctx, opts); err == nil {
		t.Fatal("Run: expected a provider error")
	}

	// With the drain, the sender consumed every event and closed senderDone.
	// Without it (pre-fix), the sender blocks on the first trailing send and
	// senderDone never closes within the window.
	select {
	case <-p.senderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("provider sender goroutine did not finish — the loop abandoned the channel (undrained terminal error)")
	}
}
