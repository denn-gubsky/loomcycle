package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// streamingProvider behaves like a real driver as far as spans go: Call opens
// and ends one loomcycle.provider.call attempt span (at the "headers"), then
// streams its events from a goroutine. block, when set, holds the stream open
// after the first event until the call's ctx ends.
type streamingProvider struct {
	events []providers.Event
	gap    time.Duration
	block  bool
	err    error // returned by Call instead of a stream
	doneAt time.Time
}

func (p *streamingProvider) ID() string                                   { return "anthropic" }
func (p *streamingProvider) Probe(context.Context) error                  { return nil }
func (p *streamingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *streamingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *streamingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	_, attempt := lcotel.RecordProviderCall(lcotel.WithAttempt(ctx, 1, 0), lcotel.ProviderCallAttrs{Provider: "anthropic"})
	attempt.End()
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan providers.Event)
	go func() {
		defer close(ch)
		for i, ev := range p.events {
			time.Sleep(p.gap)
			if ev.Type == providers.EventDone {
				p.doneAt = time.Now()
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
			if p.block && i == 0 {
				<-ctx.Done()
				return
			}
		}
	}()
	return ch, nil
}

func traced(t *testing.T) (*tracetest.InMemoryExporter, *tracetest.SpanRecorder) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSpanProcessor(rec))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	t.Cleanup(func() { cleanup(); _ = tp.Shutdown(context.Background()) })
	return exp, rec
}

func userPrompt() []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "q"}}}}
}

func spansNamed(exp *tracetest.InMemoryExporter, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func spanAttrs(s tracetest.SpanStub) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// noOpenSpans fails when a span started and never ended.
func noOpenSpans(t *testing.T, rec *tracetest.SpanRecorder) {
	t.Helper()
	if started, ended := len(rec.Started()), len(rec.Ended()); started != ended {
		t.Errorf("%d spans started, %d ended: a span leaked open", started, ended)
	}
}

func TestModelCallSpan_CoversTheStreamWithUsageAndParentsTheAttempts(t *testing.T) {
	exp, rec := traced(t)
	p := &streamingProvider{gap: 15 * time.Millisecond, events: []providers.Event{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{
			InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, CacheCreationTokens: 2, Model: "claude-x-0613"}},
	}}
	ctx := lcotel.WithTenant(context.Background(), "acme")
	_, err := Run(ctx, RunOptions{
		Provider: p, Model: "claude-x", Segments: userPrompt(), OnEvent: func(providers.Event) {},
		MaxTokens: 512,
		PriceCall: func(provider, model string, u *providers.Usage) (float64, string) {
			if provider != "anthropic" || model != "claude-x-0613" || u.OutputTokens != 7 {
				t.Errorf("PriceCall(%q, %q, %+v)", provider, model, u)
			}
			return 0.0125, "USD"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	noOpenSpans(t, rec)

	models := spansNamed(exp, lcotel.SpanModelCall)
	attempts := spansNamed(exp, lcotel.SpanProviderCall)
	iters := spansNamed(exp, lcotel.SpanIteration)
	if len(models) != 1 || len(attempts) != 1 || len(iters) != 1 {
		t.Fatalf("spans: %d model, %d attempt, %d iteration; want 1 each", len(models), len(attempts), len(iters))
	}
	m, a := models[0], attempts[0]
	if a.Parent.SpanID() != m.SpanContext.SpanID() {
		t.Error("the provider attempt span is not a child of the model-call span")
	}
	if m.Parent.SpanID() != iters[0].SpanContext.SpanID() {
		t.Error("the model-call span is not a child of the iteration span")
	}
	// It ends after the stream, not at the attempt's headers.
	if !m.EndTime.After(a.EndTime) || m.EndTime.Before(p.doneAt) {
		t.Errorf("model span ended %v; attempt ended %v, done sent %v", m.EndTime, a.EndTime, p.doneAt)
	}
	if m.Status.Code == codes.Error {
		t.Errorf("a completed call's span is an error: %q", m.Status.Description)
	}
	got := spanAttrs(m)
	for k, want := range map[string]string{
		lcotel.AttrTenant:                          "acme",
		lcotel.AttrProvider:                        "anthropic",
		lcotel.AttrModel:                           "claude-x",
		"gen_ai.operation.name":                    "chat",
		"gen_ai.provider.name":                     "anthropic",
		"gen_ai.request.model":                     "claude-x",
		"gen_ai.request.max_tokens":                "512",
		"gen_ai.response.model":                    "claude-x-0613",
		"gen_ai.response.finish_reasons":           `["end_turn"]`,
		"gen_ai.usage.input_tokens":                "11",
		"gen_ai.usage.output_tokens":               "7",
		lcotel.AttrCacheReadTokens:                 "3",
		lcotel.AttrCacheWriteTokens:                "2",
		lcotel.AttrCostUSD:                         "0.0125",
		"gen_ai.usage.cache_creation.input_tokens": "2",
		"gen_ai.usage.cache_read.input_tokens":     "3",
		lcotel.AttrStopReason:                      "end_turn",
	} {
		if v, ok := got[k]; !ok || v.Emit() != want {
			t.Errorf("%s = %q (present %v), want %q", k, v.Emit(), ok, want)
		}
	}
	if ttft, ok := got[lcotel.AttrTTFTMs]; !ok || ttft.AsInt64() <= 0 {
		t.Errorf("%s = %v (present %v), want > 0", lcotel.AttrTTFTMs, ttft.Emit(), ok)
	}
}

func TestModelCallSpan_EndsAsAnErrorWhenTheCallFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *streamingProvider
	}{
		{"call refused", &streamingProvider{err: errors.New("anthropic 400: bad request")}},
		{"in-stream error", &streamingProvider{events: []providers.Event{
			{Type: providers.EventText, Text: "par"},
			{Type: providers.EventError, Error: "anthropic 400: bad request"},
		}}},
		{"stream closed unfinished", &streamingProvider{events: []providers.Event{{Type: providers.EventText, Text: "par"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp, rec := traced(t)
			if _, err := Run(context.Background(), RunOptions{Provider: tc.p, Model: "claude-x", Segments: userPrompt(), OnEvent: func(providers.Event) {}}); err == nil {
				t.Fatal("the run succeeded")
			}
			noOpenSpans(t, rec)
			models := spansNamed(exp, lcotel.SpanModelCall)
			if len(models) != 1 {
				t.Fatalf("got %d model-call spans, want 1", len(models))
			}
			if models[0].Status.Code != codes.Error || models[0].Status.Description == "" || models[0].Status.Description == "cancelled" {
				t.Errorf("status = %v %q, want an error naming the failure", models[0].Status.Code, models[0].Status.Description)
			}
		})
	}
}

func TestModelCallSpan_EndsCancelledWhenTheRunIsCancelledMidStream(t *testing.T) {
	exp, rec := traced(t)
	p := &streamingProvider{block: true, events: []providers.Event{
		{Type: providers.EventText, Text: "partial"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = Run(ctx, RunOptions{Provider: p, Model: "claude-x", Segments: userPrompt(), OnEvent: func(ev providers.Event) {
		if ev.Type == providers.EventText {
			cancel()
		}
	}})
	cancel()
	noOpenSpans(t, rec)
	models := spansNamed(exp, lcotel.SpanModelCall)
	if len(models) != 1 {
		t.Fatalf("got %d model-call spans, want 1", len(models))
	}
	if models[0].Status.Code != codes.Error || models[0].Status.Description != "cancelled" {
		t.Errorf("status = %v %q, want Error \"cancelled\"", models[0].Status.Code, models[0].Status.Description)
	}
	if et := spanAttrs(models[0])["error.type"]; et.AsString() != lcotel.ErrorTypeCancelled {
		t.Errorf("error.type = %q", et.Emit())
	}
}

// A fallback is one hop between two model-call spans: the failed call is an
// error, and the call that follows names where the run came from.
func TestModelCallSpan_NamesTheFallbackHop(t *testing.T) {
	exp, rec := traced(t)
	failing := &tieredProvider{id: "anthropic", errors: []error{errors.New("anthropic 429: rate limit exceeded")}}
	healthy := &tieredProvider{id: "deepseek", responses: [][]providers.Event{successResponse()}}
	_, err := Run(context.Background(), RunOptions{
		Provider: failing, Model: "claude-x", MaxIterations: 5, Segments: userPrompt(), OnEvent: func(providers.Event) {},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3},
		ReResolve: func(context.Context, string, string, error) (providers.Provider, string, string, error) {
			return healthy, "deepseek-v4", "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	noOpenSpans(t, rec)
	models := spansNamed(exp, lcotel.SpanModelCall)
	if len(models) != 2 {
		t.Fatalf("got %d model-call spans, want 2", len(models))
	}
	failed, next := spanAttrs(models[0]), spanAttrs(models[1])
	if models[0].Status.Code != codes.Error {
		t.Errorf("the failed call's status = %v", models[0].Status.Code)
	}
	if _, ok := failed[lcotel.AttrFallbackFrom]; ok {
		t.Error("the first call names a fallback it did not come from")
	}
	if got := next[lcotel.AttrFallbackFrom].AsString(); got != "anthropic/claude-x" {
		t.Errorf("%s = %q, want anthropic/claude-x", lcotel.AttrFallbackFrom, got)
	}
	if got := next[lcotel.AttrProvider].AsString() + "/" + next[lcotel.AttrModel].AsString(); got != "deepseek/deepseek-v4" {
		t.Errorf("the next call ran on %q", got)
	}
}
