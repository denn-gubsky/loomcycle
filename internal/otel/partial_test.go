package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestCountingExporter_PartialSuccessIsAnExportThatDroppedSome: a collector
// that answers 200 but rejects one span of a batch leaves the rest exported,
// the one dropped, no failed export, and ends an outage.
func TestCountingExporter_PartialSuccessIsAnExportThatDroppedSome(t *testing.T) {
	clearOTELEnv(t)
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadRequest) // not retried
			return
		}
		body, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{
			PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1, ErrorMessage: "span too large"},
		})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	exp, err := newExporter(context.Background(), ProtocolHTTPProtobuf, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(context.Background()) }()

	var logs lineCounter
	now := time.Unix(1_800_000_000, 0)
	errs := newErrorHandler(slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return now })
	var st pipelineStats
	c := &countingExporter{next: exp, st: &st, errs: errs}

	failErr := c.ExportSpans(context.Background(), threeSpans(t))
	if failErr == nil {
		t.Fatal("a 400 was not a failed export")
	}
	errs.Handle(failErr) // as the batch processor would
	fail.Store(false)
	if err := c.ExportSpans(context.Background(), threeSpans(t)); err != nil {
		t.Fatalf("a partial success came back as an error: %v", err)
	}
	if st.exported.Load() != 2 || st.dropped.Load() != 3+1 || st.exportFailures.Load() != 1 {
		t.Errorf("exported=%d dropped=%d failures=%d, want 2 / 4 / 1", st.exported.Load(), st.dropped.Load(), st.exportFailures.Load())
	}
	out := strings.Join(logs.lines(), "\n")
	if !strings.Contains(out, "recovered") || !strings.Contains(out, "rejected_spans=1") {
		t.Errorf("want a recovery line and a rejection line:\n%s", out)
	}
}

func threeSpans(t *testing.T) []sdktrace.ReadOnlySpan {
	t.Helper()
	return append(append(oneSpan(t), oneSpan(t)...), oneSpan(t)...)
}

// TestPartialRejected_OnlyAPartialSuccessCounts: an error tree that is all
// partial successes is one (rejections summed); anything else in it is a
// failure.
func TestPartialRejected_OnlyAPartialSuccessCounts(t *testing.T) {
	if _, ok := partialRejected(errors.New("dial tcp: refused")); ok {
		t.Error("a plain error read as a partial success")
	}
	if _, ok := partialRejected(fmt.Errorf("traces export: %w", errors.Join(errors.New("x")))); ok {
		t.Error("a wrapped plain error read as a partial success")
	}
}
