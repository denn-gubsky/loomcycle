package ratelimit

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// Each attempt's span carries its number, and a retry the wait before it,
// whether the wait came from the provider's header or the schedule.
func TestDo_StampsAttemptNumberAndWaitOnEachAttemptSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()

	statuses := []int{429, 429, 200}
	n := 0
	_, err := Do(context.Background(), Config{
		ParseHeader: func(h http.Header) (time.Duration, bool) {
			if h.Get("X-Wait") != "" {
				return 3 * time.Millisecond, true
			}
			return 0, false
		},
		Schedule: []time.Duration{2 * time.Millisecond, 7 * time.Millisecond},
		OnRetry:  func(string, int, time.Duration, string) {},
	}, func(ctx context.Context) (*http.Response, error) {
		_, span := lcotel.RecordProviderCall(ctx, lcotel.ProviderCallAttrs{Provider: "p"})
		defer span.End()
		resp := &http.Response{StatusCode: statuses[n], Status: http.StatusText(statuses[n]), Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
		if n == 0 {
			resp.Header.Set("X-Wait", "1")
		}
		n++
		lcotel.SetProviderResponse(span, resp, nil)
		return resp, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	// attempt 2 waited the header's 3ms; attempt 3 the schedule's second
	// entry (7ms), as no header came back on attempt 2.
	want := []struct{ attempt, waitMs string }{{"1", ""}, {"2", "3"}, {"3", "7"}}
	for i, s := range spans {
		got := map[string]string{}
		for _, kv := range s.Attributes {
			got[string(kv.Key)] = kv.Value.Emit()
		}
		if got[lcotel.AttrAttempt] != want[i].attempt || got[lcotel.AttrRetryAfterMs] != want[i].waitMs {
			t.Errorf("span %d: attempt %q retry_after_ms %q, want %q / %q", i, got[lcotel.AttrAttempt], got[lcotel.AttrRetryAfterMs], want[i].attempt, want[i].waitMs)
		}
	}
}
