package reranker

import (
	"context"
	"errors"
	"strings"
	"sync"
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

// TestModel_HoldsAProviderSlotForTheCall — the gate is acquired before the call
// and released after it, and a refused slot fails the call without reaching the
// provider.
func TestModel_HoldsAProviderSlotForTheCall(t *testing.T) {
	p := &stubProvider{events: textReply("[1]")}
	m := New(p, config.RerankerConfig{Provider: "gpu", Model: "m"})
	var acquired, released string
	m.Acquire = func(_ context.Context, id string) (func(), error) {
		acquired = id
		return func() { released = id }, nil
	}
	if _, err := m.Complete(context.Background(), "prompt"); err != nil {
		t.Fatal(err)
	}
	if acquired != "gpu" || released != "gpu" {
		t.Errorf("slot acquired %q released %q, want gpu both", acquired, released)
	}

	p2 := &stubProvider{events: textReply("[1]")}
	m2 := New(p2, config.RerankerConfig{Provider: "gpu", Model: "m"})
	m2.Acquire = func(context.Context, string) (func(), error) { return nil, errors.New("saturated") }
	if _, err := m2.Complete(context.Background(), "prompt"); err == nil {
		t.Error("a refused slot must fail the call")
	}
	if p2.req.Model != "" {
		t.Error("a refused slot must not reach the provider")
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
