package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// runRecording runs opts and returns the result, the error and every event the
// loop emitted.
func runRecording(t *testing.T, ctx context.Context, opts RunOptions) (RunResult, error, []providers.Event) {
	t.Helper()
	var mu sync.Mutex
	var events []providers.Event
	opts.OnEvent = func(ev providers.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if opts.MaxIterations == 0 {
		opts.MaxIterations = 5
	}
	if len(opts.Segments) == 0 {
		opts.Segments = []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}}
	}
	res, err := Run(ctx, opts)
	mu.Lock()
	defer mu.Unlock()
	return res, err, append([]providers.Event(nil), events...)
}

func countEvents(events []providers.Event, typ providers.EventType) int {
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// silentStream is a call that opened its response stream and then closed it
// without a done event or an error — what a driver hands back when the server
// hung up cleanly mid-call, or when its terminal error was lost to a cancelled
// stream context.
func silentStream() []providers.Event { return []providers.Event{} }

// TestRun_StreamClosedWithoutDoneFailsTheRun: a call whose stream ends without
// a done event did not finish. The loop used to take it for an empty
// end_turn, so the run completed with no text, no stop reason, zero usage and
// no error.
func TestRun_StreamClosedWithoutDoneFailsTheRun(t *testing.T) {
	p := &tieredProvider{id: "ollama-local", responses: [][]providers.Event{silentStream()}}
	res, err, events := runRecording(t, context.Background(), RunOptions{Provider: p, Model: "gpt-oss:latest"})
	if err == nil {
		t.Fatalf("Run returned nil error (stop_reason=%q, text=%q); a stream that never finished must fail the run", res.StopReason, res.FinalText)
	}
	if !strings.Contains(err.Error(), "ollama-local") || !strings.Contains(err.Error(), "ended before the model finished") {
		t.Errorf("error = %q, want it to name the provider and say the stream ended early", err)
	}
	if n := countEvents(events, providers.EventError); n != 1 {
		t.Errorf("EventError count = %d, want 1 (the terminal error consumers read)", n)
	}
	if n := countEvents(events, providers.EventDone); n != 0 {
		t.Errorf("EventDone count = %d, want 0: a failed run must not also report done", n)
	}
}

// TestFallback_PinnedRetriesThenSilentStreamFailsTheRun replays the lab
// transcript: a pinned model (the fallback "swap" re-resolves to the same
// provider and model) returns a retryable 500 twice, then its third call's
// stream closes without finishing. The run must end failed — it used to end
// completed with zero usage after the two provider_fallback events.
func TestFallback_PinnedRetriesThenSilentStreamFailsTheRun(t *testing.T) {
	p := &tieredProvider{
		id: "ollama-local",
		errors: []error{
			fmt.Errorf(`ollama-local 500: {"error":"llama-server process has terminated: signal: aborted (core dumped)"}`),
			fmt.Errorf(`ollama-local 500: {"error":"an error was encountered while running the model: unexpected EOF"}`),
		},
		responses: [][]providers.Event{nil, nil, silentStream()},
	}
	res, err, events := runRecording(t, context.Background(), RunOptions{
		Provider:       p,
		Model:          "gpt-oss:latest",
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "default"},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return p, "gpt-oss:latest", "", nil
		},
	})
	if err == nil {
		t.Fatalf("Run returned nil error (stop_reason=%q, usage=%+v); the run must fail", res.StopReason, res.Usage)
	}
	if n := countEvents(events, providers.EventProviderFallback); n != 2 {
		t.Errorf("provider_fallback events = %d, want 2", n)
	}
	if n := countEvents(events, providers.EventDone); n != 0 {
		t.Errorf("EventDone count = %d, want 0", n)
	}
}

// TestFallback_PinnedEveryAttemptFailsEndsWithTheLastError: every call of a
// pinned model returns a retryable 500. Once the fallback budget is spent the
// run fails with the last call's error.
func TestFallback_PinnedEveryAttemptFailsEndsWithTheLastError(t *testing.T) {
	var errs []error
	for i := 1; i <= 4; i++ {
		errs = append(errs, fmt.Errorf(`ollama-local 500: {"error":"crash %d"}`, i))
	}
	p := &tieredProvider{id: "ollama-local", errors: errs}
	_, err, events := runRecording(t, context.Background(), RunOptions{
		Provider:       p,
		Model:          "gpt-oss:latest",
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return p, "gpt-oss:latest", "", nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "crash 4") {
		t.Fatalf("err = %v, want the fourth (last) call's error", err)
	}
	if n := countEvents(events, providers.EventProviderFallback); n != 3 {
		t.Errorf("provider_fallback events = %d, want 3 (the budget)", n)
	}
	if n := countEvents(events, providers.EventDone); n != 0 {
		t.Errorf("EventDone count = %d, want 0", n)
	}
}

// TestFallback_CascadeAllFailingEndsFailed: a tier cascade of three distinct
// candidates, every one failing — two with a 503, the last with a stream that
// closes unfinished. The run ends failed.
func TestFallback_CascadeAllFailingEndsFailed(t *testing.T) {
	first := &tieredProvider{id: "anthropic", errors: []error{fmt.Errorf("anthropic 503: overloaded")}}
	second := &tieredProvider{id: "deepseek", errors: []error{fmt.Errorf("deepseek 503: busy")}}
	third := &tieredProvider{id: "ollama-local", responses: [][]providers.Event{silentStream()}}
	next := []providers.Provider{second, third}
	_, err, events := runRecording(t, context.Background(), RunOptions{
		Provider:       first,
		Model:          "m1",
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			if len(next) == 0 {
				return nil, "", "", errors.New("no candidate left")
			}
			p := next[0]
			next = next[1:]
			return p, "m", "", nil
		},
	})
	if err == nil {
		t.Fatal("Run returned nil error; a cascade whose every candidate failed must fail the run")
	}
	if !strings.Contains(err.Error(), "ollama-local") {
		t.Errorf("err = %q, want the last candidate's failure", err)
	}
	if n := countEvents(events, providers.EventProviderFallback); n != 2 {
		t.Errorf("provider_fallback events = %d, want 2", n)
	}
	if n := countEvents(events, providers.EventDone); n != 0 {
		t.Errorf("EventDone count = %d, want 0", n)
	}
}

// TestFallback_PinnedOneFailureThenSuccessCompletes: the guard must leave a
// recovered run alone — one 500, then a call that finishes normally.
func TestFallback_PinnedOneFailureThenSuccessCompletes(t *testing.T) {
	p := &tieredProvider{
		id:        "ollama-local",
		errors:    []error{fmt.Errorf(`ollama-local 500: {"error":"crash"}`)},
		responses: [][]providers.Event{nil, successResponse()},
	}
	res, err, events := runRecording(t, context.Background(), RunOptions{
		Provider:       p,
		Model:          "gpt-oss:latest",
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3},
		ReResolve: func(_ context.Context, _, _ string, _ error) (providers.Provider, string, string, error) {
			return p, "gpt-oss:latest", "", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v (want success after one fallback)", err)
	}
	if res.StopReason != "end_turn" || res.FinalText != "hi from new provider" {
		t.Errorf("result = (%q, %q), want (end_turn, the second call's text)", res.StopReason, res.FinalText)
	}
	if n := countEvents(events, providers.EventError); n != 0 {
		t.Errorf("EventError count = %d, want 0", n)
	}
}

// hangingProvider opens a stream that closes, with no events, only when the
// call's context ends — a model mid-generation when the run is cancelled.
type hangingProvider struct {
	tieredProvider
	called chan struct{}
}

func (p *hangingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event)
	close(p.called)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestRun_CancelledMidStreamIsNotAProviderFailure: a whole-run cancel closes
// the stream without a done event too. That is a cancellation, which the
// server records from the run's context; the guard must not turn it into a
// provider failure.
func TestRun_CancelledMidStreamIsNotAProviderFailure(t *testing.T) {
	p := &hangingProvider{tieredProvider: tieredProvider{id: "ollama-local"}, called: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.called:
			cancel()
		case <-time.After(5 * time.Second):
		}
	}()
	_, err, events := runRecording(t, ctx, RunOptions{Provider: p, Model: "m"})
	if err != nil && strings.Contains(err.Error(), "ended before the model finished") {
		t.Fatalf("err = %v: a cancelled run was reported as a provider failure", err)
	}
	for _, ev := range events {
		if ev.Type == providers.EventError && strings.Contains(ev.Error, "ended before the model finished") {
			t.Errorf("a cancelled run reported a provider failure: %q", ev.Error)
		}
	}
}
