package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// startRunAndRead starts one run over HTTP and returns its SSE body and the
// get_run response for it.
func startRunAndRead(t *testing.T) (sse string, got agentResponse) {
	t.Helper()
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(runBody("default")))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sse = string(body)
	const marker = "event: agent\ndata: "
	i := strings.Index(sse, marker)
	if i < 0 {
		t.Fatalf("no agent frame in:\n%s", sse)
	}
	line := sse[i+len(marker):]
	line = line[:strings.Index(line, "\n")]
	var frame struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatal(err)
	}
	r, err := http.Get(ts.URL + "/v1/runs/" + frame.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return sse, got
}

// TestGetRun_ReturnsTheSampledRunsTraceID: a sampled run stores its span's
// trace id, get_run returns it, and the run's started event carries it.
func TestGetRun_ReturnsTheSampledRunsTraceID(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()

	sse, got := startRunAndRead(t)
	var want string
	for _, s := range exp.GetSpans() {
		if s.Name == lcotel.SpanRun {
			want = s.SpanContext.TraceID().String()
		}
	}
	if want == "" {
		t.Fatal("no run span recorded")
	}
	if got.TraceID != want {
		t.Errorf("get_run trace_id = %q, want the run span's %q", got.TraceID, want)
	}
	if !strings.Contains(sse, `"type":"started","trace_id":"`+want+`"`) {
		t.Errorf("started event does not carry trace_id %s:\n%s", want, sse)
	}
}

// TestGetRun_NoTraceIDWhenUnsampledOrOff: nothing exported, nothing to link.
func TestGetRun_NoTraceIDWhenUnsampledOrOff(t *testing.T) {
	_, got := startRunAndRead(t) // tracing off: the default no-op provider
	if got.TraceID != "" {
		t.Errorf("tracing off: trace_id = %q", got.TraceID)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()
	sse, got := startRunAndRead(t)
	if got.TraceID != "" || strings.Contains(sse, `"trace_id"`) {
		t.Errorf("unsampled: trace_id = %q", got.TraceID)
	}
}

// TestStoreRunToConnector_CarriesTraceID: MCP get_run and the connector
// reads return the run's trace id.
func TestStoreRunToConnector_CarriesTraceID(t *testing.T) {
	const tid = "4bf92f3577b34da6a3ce929d0e0e4736"
	if got := storeRunToConnector(store.Run{ID: "r", TraceID: tid}).TraceID; got != tid {
		t.Errorf("connector.Run.TraceID = %q, want %q", got, tid)
	}
}
