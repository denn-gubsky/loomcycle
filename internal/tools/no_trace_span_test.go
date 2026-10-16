package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

const toolSpanCanary = "TOOL-SPAN-CANARY-4a1d-no-trace"

// spanTextTool fails, either with a result whose text quotes its input or
// with a Go error that does.
type spanTextTool struct{ goErr bool }

func (s spanTextTool) Name() string                 { return "Quote" }
func (s spanTextTool) Description() string          { return "fails quoting its input" }
func (s spanTextTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s spanTextTool) Execute(context.Context, json.RawMessage) (Result, error) {
	if s.goErr {
		return Result{}, errors.New("broke on " + toolSpanCanary)
	}
	return Result{Text: "refused: " + toolSpanCanary, IsError: true}, nil
}

// The tool span of a no-trace run carries no error text: a tool's error
// quotes what the run said as readily as its result does. An ordinary run's
// span keeps it, so the check can fail.
func TestToolSpan_ANoTraceRunsErrorCarriesNoText(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	defer func() { cleanup(); _ = tp.Shutdown(context.Background()) }()

	for _, goErr := range []bool{false, true} {
		for _, retention := range []string{"none", ""} {
			exp.Reset()
			d := NewDispatcher([]Tool{spanTextTool{goErr: goErr}})
			ctx := WithRunIdentity(context.Background(), RunIdentityValue{Retention: retention})
			_, _ = d.Call(ctx, "Quote", json.RawMessage(`{}`))
			var got strings.Builder
			for _, s := range exp.GetSpans() {
				got.WriteString(s.Status.Description)
				for _, ev := range s.Events {
					for _, kv := range ev.Attributes {
						got.WriteString(" " + kv.Value.Emit())
					}
				}
			}
			has := strings.Contains(got.String(), toolSpanCanary)
			if retention == "none" && has {
				t.Errorf("goErr=%v: a no-trace run's tool span carries %q", goErr, got.String())
			}
			if retention == "" && !has {
				t.Fatalf("goErr=%v: an ordinary run's tool span carries %q: this test cannot see the text", goErr, got.String())
			}
		}
	}
}
