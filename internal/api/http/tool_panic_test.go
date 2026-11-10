package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// panickyTool panics on every call.
type panickyTool struct{}

func (panickyTool) Name() string                 { return "Document" }
func (panickyTool) Description() string          { return "" }
func (panickyTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (panickyTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	var p *tools.Result
	return *p, nil
}

// Both dispatchers the server builds — a run's, and the one-off dispatcher
// behind the off-run tool routes (MCP, HTTP, gRPC) — recover a panicking tool
// into a failed call and count it on /metrics by tool.
func TestServer_APanickingToolIsRecoveredAndCountedOnEveryDispatcher(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	srv.toolPanics = tools.NewPanicCounter()

	if r := srv.newDispatcher([]tools.Tool{panickyTool{}}).Execute(context.Background(), "Document", json.RawMessage(`{}`)); !strings.HasPrefix(r.Text, "tool_panicked:") {
		t.Errorf("run dispatcher: %+v", r)
	}
	r, err := srv.execBuiltin(context.Background(), panickyTool{}, json.RawMessage(`{}`))
	if err != nil || !r.IsError || !strings.HasPrefix(r.Text, "tool_panicked:") {
		t.Errorf("off-run dispatcher: %+v, %v", r, err)
	}

	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "# TYPE loomcycle_tool_panics_total counter") || !strings.Contains(body, `loomcycle_tool_panics_total{tool="Document"} 2`) {
		t.Errorf("metrics body lacks the panic count:\n%s", body)
	}
}

// A background goroutine recovered by safego is counted on /metrics by name.
func TestServer_ARecoveredGoroutinePanicIsCountedOnMetrics(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	// The counter is process-wide, so compare against its value before the
	// panic (a repeated run, -count=2, starts above zero).
	want := safego.Panics()["test.metrics_goroutine"] + 1
	safego.Run(context.Background(), "test.metrics_goroutine", func() { panic("boom") })

	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "# TYPE loomcycle_goroutine_panics_total counter") || !strings.Contains(body, fmt.Sprintf(`loomcycle_goroutine_panics_total{goroutine="test.metrics_goroutine"} %d`, want)) {
		t.Errorf("metrics body lacks the goroutine panic count:\n%s", body)
	}
}
