package otel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// loggingMetricExporter logs the OTLP metric exporter's failures itself and
// reports success to the periodic reader. The reader hands a failed export to
// the global OTEL error handler, which is the trace pipeline's (its messages
// name spans) or, with tracing off, the SDK default that writes every failure
// through the stdlib logger — an interval's line per failure for as long as
// the collector is down. A failed export is dropped either way: the next one
// carries the cumulative totals again, so nothing is lost but freshness.
type loggingMetricExporter struct {
	sdkmetric.Exporter
	log *metricErrorLog
}

func (e *loggingMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if err := e.Exporter.Export(ctx, rm); err != nil {
		e.log.failed(err)
		return nil
	}
	e.log.succeeded()
	return nil
}

// metricErrorLog is errorHandler's policy for metric exports: one Warn when
// exports start failing, a summary at most every errorSummaryInterval, one
// Info when they recover. The error text comes from the exporter (a status or
// a connection error), never the request's headers.
type metricErrorLog struct {
	now func() time.Time

	mu          sync.Mutex
	failing     bool
	suppressed  int
	lastSummary time.Time
}

func newMetricErrorLog(now func() time.Time) *metricErrorLog {
	return &metricErrorLog{now: now}
}

func (l *metricErrorLog) failed(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.failing {
		l.failing, l.suppressed, l.lastSummary = true, 0, now
		slog.Warn("otel: metric export failing; repeats are summarised every 5 minutes", "err", err)
		return
	}
	l.suppressed++
	if now.Sub(l.lastSummary) >= errorSummaryInterval {
		slog.Warn("otel: metric export still failing", "suppressed_errors", l.suppressed, "err", err)
		l.suppressed, l.lastSummary = 0, now
	}
}

func (l *metricErrorLog) succeeded() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.failing {
		return
	}
	slog.Info("otel: metric export recovered", "suppressed_errors", l.suppressed)
	l.failing, l.suppressed = false, 0
}
