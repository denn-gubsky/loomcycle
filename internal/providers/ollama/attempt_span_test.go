package ollama

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// The request after a runner death is the call's next attempt: its span
// continues the numbering (it does not restart at 1) and carries the
// runner-death wait as loomcycle.retry_after_ms.
func TestCall_AttemptNumbersContinueAcrossARunnerDeathRetry(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()

	srv, _ := chatServer(t, func(n int, w http.ResponseWriter) {
		if n == 0 {
			crash(w, 500, crashAborted)
			return
		}
		fmt.Fprint(w, retryChatDone)
	})
	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	d.runnerRetryWaits = []time.Duration{7 * time.Millisecond}
	ch, err := d.Call(context.Background(), chatRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	var got []map[string]string
	for _, s := range exp.GetSpans() {
		if s.Name != lcotel.SpanProviderCall {
			continue
		}
		m := map[string]string{}
		for _, kv := range s.Attributes {
			m[string(kv.Key)] = kv.Value.Emit()
		}
		got = append(got, m)
	}
	if len(got) != 2 {
		t.Fatalf("got %d attempt spans, want 2", len(got))
	}
	if got[0][lcotel.AttrAttempt] != "1" || got[0]["http.response.status_code"] != "500" {
		t.Errorf("runner-death attempt: %v", got[0])
	}
	if got[1][lcotel.AttrAttempt] != "2" || got[1][lcotel.AttrRetryAfterMs] != "7" {
		t.Errorf("retry after the runner death: attempt %q retry_after_ms %q, want 2 / 7",
			got[1][lcotel.AttrAttempt], got[1][lcotel.AttrRetryAfterMs])
	}
}
