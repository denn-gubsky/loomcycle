package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const iterSpanCanary = "ITER-SPAN-CANARY-9b2f-no-trace"

// quotingProvider fails the way a provider's 400 does: with its error text
// quoting the prompt, either from Call or as an in-stream error frame.
type quotingProvider struct{ inStream bool }

func (p *quotingProvider) ID() string                                   { return "quoting" }
func (p *quotingProvider) Probe(context.Context) error                  { return nil }
func (p *quotingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *quotingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *quotingProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	msg := "invalid request, prompt was: " + iterSpanCanary
	if !p.inStream {
		return nil, errors.New(msg)
	}
	ch := make(chan providers.Event, 1)
	ch <- providers.Event{Type: providers.EventError, Error: msg}
	close(ch)
	return ch, nil
}

// The iteration span of a no-trace run carries no provider error text: a
// provider's error can quote the prompt. An ordinary run's span keeps it, so
// the check can fail.
func TestIterationSpan_ANoTraceRunsProviderErrorCarriesNoText(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	defer func() { cleanup(); _ = tp.Shutdown(context.Background()) }()

	for _, inStream := range []bool{false, true} {
		for _, retention := range []string{"none", ""} {
			exp.Reset()
			ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{Retention: retention})
			_, _ = Run(ctx, RunOptions{
				Provider: &quotingProvider{inStream: inStream},
				Model:    "x",
				Segments: []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "q"}}}},
				OnEvent:  func(providers.Event) {},
			})
			var got strings.Builder
			for _, s := range exp.GetSpans() {
				got.WriteString(s.Status.Description)
				for _, ev := range s.Events {
					for _, kv := range ev.Attributes {
						got.WriteString(" " + kv.Value.Emit())
					}
				}
			}
			has := strings.Contains(got.String(), iterSpanCanary)
			if retention == "none" && has {
				t.Errorf("inStream=%v: a no-trace run's iteration span carries %q", inStream, got.String())
			}
			if retention == "" && !has {
				t.Fatalf("inStream=%v: an ordinary run's iteration span carries %q: this test cannot see the text", inStream, got.String())
			}
		}
	}
}
