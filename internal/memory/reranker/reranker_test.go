package reranker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// stubProvider answers every call with a fixed script of events, or blocks until
// the call's context ends.
type stubProvider struct {
	events []providers.Event
	block  bool
	mu     sync.Mutex
	req    providers.Request
}

func (s *stubProvider) ID() string                                   { return "stub" }
func (s *stubProvider) Capabilities() providers.Capabilities         { return providers.Capabilities{} }
func (s *stubProvider) Probe(context.Context) error                  { return nil }
func (s *stubProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (s *stubProvider) KeyEnvName() string                           { return "" }
func (s *stubProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	s.mu.Lock()
	s.req = req
	s.mu.Unlock()
	ch := make(chan providers.Event, len(s.events)+1)
	go func() {
		defer close(ch)
		if s.block {
			<-ctx.Done()
			ch <- providers.Event{Type: providers.EventError, Error: "stream: " + ctx.Err().Error()}
			return
		}
		for _, ev := range s.events {
			ch <- ev
		}
	}()
	return ch, nil
}

func textReply(text string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 6000, OutputTokens: 12}},
		{Type: providers.EventDone},
	}
}

// TestModel_CallsTheConfiguredModelAndRecordsItsUsage — the request carries the
// measured settings (temperature 0, the effort hint, the context window), the
// reply text comes back, and the tokens are handed to OnUsage attributed to the
// reranker's provider and model.
func TestModel_CallsTheConfiguredModelAndRecordsItsUsage(t *testing.T) {
	p := &stubProvider{events: textReply("[2, 1]")}
	m := New(p, config.RerankerConfig{Provider: "ollama-local", Model: "qwen3.6:latest", Effort: "low"})
	var got *providers.Usage
	m.OnUsage = func(_ context.Context, u *providers.Usage) { got = u }

	reply, err := m.Complete(context.Background(), "prompt")
	if err != nil || reply != "[2, 1]" {
		t.Fatalf("Complete = %q, %v", reply, err)
	}
	if p.req.Model != "qwen3.6:latest" || p.req.Effort != "low" || p.req.MaxContextTokens != 16384 ||
		p.req.Temperature == nil || *p.req.Temperature != 0 {
		t.Errorf("request = model %q effort %q ctx %d temp %v", p.req.Model, p.req.Effort, p.req.MaxContextTokens, p.req.Temperature)
	}
	if got == nil || got.InputTokens != 6000 || got.Provider != "ollama-local" || got.Model != "qwen3.6:latest" {
		t.Errorf("usage = %+v", got)
	}
}

// TestModel_ATimeoutIsReportedAsATimeout — a model that never answers is cut off
// at the reranker's timeout, and the error says so, which is what lets the search
// report `timeout` rather than a generic failure.
func TestModel_ATimeoutIsReportedAsATimeout(t *testing.T) {
	m := New(&stubProvider{block: true}, config.RerankerConfig{Provider: "p", Model: "m", TimeoutMs: 20})
	start := time.Now()
	_, err := m.Complete(context.Background(), "prompt")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the timeout did not bound the call")
	}
	_, rep := memory.RerankTexts(context.Background(), m, "q", []string{"a", "b"}, 100)
	if rep.Reason != memory.RerankTimeout {
		t.Errorf("reason = %q, want %q", rep.Reason, memory.RerankTimeout)
	}
}

// TestModel_AProviderErrorFailsTheCall — an error event is an error, not an empty
// reply that would parse as "no ranking".
func TestModel_AProviderErrorFailsTheCall(t *testing.T) {
	m := New(&stubProvider{events: []providers.Event{{Type: providers.EventError, Error: "401 unauthorized"}}},
		config.RerankerConfig{Provider: "p", Model: "m"})
	if _, err := m.Complete(context.Background(), "prompt"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want the provider's error", err)
	}
}

// TestBuild_NoBlockMeansNoReranker and an undeclared provider fails loudly.
func TestBuild_NoBlockMeansNoReranker(t *testing.T) {
	m, err := Build(&config.Config{})
	if err != nil || m != nil {
		t.Errorf("Build(no block) = %v, %v; want nil, nil", m, err)
	}
	_, err = Build(&config.Config{Memory: config.MemoryConfig{Reranker: config.RerankerConfig{Provider: "nope", Model: "m"}}})
	if err == nil || !strings.Contains(err.Error(), `"nope" is not declared`) {
		t.Errorf("undeclared provider: err = %v", err)
	}
}

// scriptedStream plays events and then, if hang is set, waits for the call's
// context to end and closes WITHOUT an error or done event — what a driver's
// ctx-aware send does when the timeout fires mid-reply.
type scriptedStream struct {
	stubProvider
	events []providers.Event
	hang   bool
	calls  atomic.Int64
}

func (s *scriptedStream) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	s.calls.Add(1)
	ch := make(chan providers.Event, len(s.events))
	go func() {
		defer close(ch)
		for _, ev := range s.events {
			ch <- ev
		}
		if s.hang {
			<-ctx.Done()
		}
	}()
	return ch, nil
}

// TestModel_AReplyCutOffByTheTimeoutIsATimeout — the text so far is not the
// answer: a stream the timeout closes with no done event must report timeout,
// not hand "[4" (or an early "passage [4]") to the parser as a ranking.
func TestModel_AReplyCutOffByTheTimeoutIsATimeout(t *testing.T) {
	p := &scriptedStream{events: []providers.Event{{Type: providers.EventText, Text: "Passage [4] looks relevant, ranking: [4, 1"}}, hang: true}
	m := New(p, config.RerankerConfig{Provider: "p", Model: "m", TimeoutMs: 30})
	reply, err := m.Complete(context.Background(), "prompt")
	if !errors.Is(err, context.DeadlineExceeded) || reply != "" {
		t.Fatalf("Complete = %q, %v; want no reply and DeadlineExceeded", reply, err)
	}
	if _, rep := memory.RerankTexts(context.Background(), m, "q", []string{"a", "b", "c", "d"}, 100); rep.Applied || rep.Reason != memory.RerankTimeout {
		t.Errorf("report = %+v, want a timeout, never an applied partial ranking", rep)
	}
}

// TestModel_AStreamThatEndsUnfinishedFails — no done event and no deadline is
// still an unfinished reply.
func TestModel_AStreamThatEndsUnfinishedFails(t *testing.T) {
	m := New(&scriptedStream{events: []providers.Event{{Type: providers.EventText, Text: "[2, 1]"}}},
		config.RerankerConfig{Provider: "p", Model: "m"})
	if _, err := m.Complete(context.Background(), "prompt"); err == nil {
		t.Error("a stream that closed without completing must not count as a reply")
	}
}

// TestModel_AParentCancellationIsNotATimeout — the caller going away is a failed
// call; only the reranker's own deadline is reported as a timeout.
func TestModel_AParentCancellationIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := New(&scriptedStream{hang: true}, config.RerankerConfig{Provider: "p", Model: "m", TimeoutMs: 60000})
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, rep := memory.RerankTexts(ctx, m, "q", []string{"a", "b"}, 100)
	if rep.Reason != memory.RerankCallFailed {
		t.Errorf("reason = %q, want %q for a cancelled caller", rep.Reason, memory.RerankCallFailed)
	}
}

// TestModel_ConcurrencyIsBoundedAndAWaitIsATimeout — with max_concurrent 1 a
// second rerank waits for the first; one that cannot get a slot before its own
// timeout never reaches the provider and reports a timeout.
func TestModel_ConcurrencyIsBoundedAndAWaitIsATimeout(t *testing.T) {
	p := &scriptedStream{hang: true}
	m := New(p, config.RerankerConfig{Provider: "p", Model: "m", TimeoutMs: 300, MaxConcurrent: 1})
	first := make(chan struct{})
	go func() { _, _ = m.Complete(context.Background(), "hold the slot"); close(first) }()
	for p.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.Complete(ctx, "wait"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a rerank waiting for a slot: err = %v, want DeadlineExceeded", err)
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("provider saw %d calls, want 1 — the waiting rerank must not reach it", n)
	}
	<-first
}
