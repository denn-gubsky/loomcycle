package otel

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SpanStats are the export pipeline's counters since the process started.
// Every counter only grows. A sampled span that ended is, at any moment,
// exported, dropped, or still on its way (queued or in an export call), so
// Ended >= Exported + Dropped.
type SpanStats struct {
	// Enabled reports whether an exporter is installed; all counters stay 0
	// when it is not.
	Enabled bool
	// Ended: sampled spans that ended and entered the export pipeline.
	Ended uint64
	// Exported: spans in batches the collector accepted.
	Exported uint64
	// Dropped: spans that will never reach the collector — discarded because
	// the export queue was full, or carried by an export call that failed.
	Dropped uint64
	// ExportFailures: export calls (batches) that failed.
	ExportFailures uint64
}

// pipelineStats holds the counters. Process-wide like the tracer provider
// they describe, which the OTEL API makes global: /metrics reads them without
// a handle on the provider.
type pipelineStats struct {
	enabled        atomic.Bool
	ended          atomic.Uint64
	exported       atomic.Uint64
	dropped        atomic.Uint64
	exportFailures atomic.Uint64
	// inFlight: spans handed to the batch processor and not yet accounted for
	// by an export call. The gate keeps it at or below the queue size, so the
	// batch processor itself never drops a span uncounted.
	inFlight atomic.Int64
}

var stats pipelineStats

// Stats reports the export pipeline's counters.
func Stats() SpanStats {
	return SpanStats{
		Enabled:        stats.enabled.Load(),
		Ended:          stats.ended.Load(),
		Exported:       stats.exported.Load(),
		Dropped:        stats.dropped.Load(),
		ExportFailures: stats.exportFailures.Load(),
	}
}

// gateProcessor counts sampled spans as they end and drops them itself once
// maxInFlight are waiting, in front of the batch processor. The SDK's batch
// processor would drop them too (it never blocks unless told to) but keeps no
// count anyone can read; dropping here first is what makes Dropped exact.
type gateProcessor struct {
	next        sdktrace.SpanProcessor
	st          *pipelineStats
	maxInFlight int64
}

func (g *gateProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	g.next.OnStart(ctx, s)
}

func (g *gateProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	// The batch processor exports sampled spans only; an unsampled one is
	// neither counted nor queued.
	if !s.SpanContext().IsSampled() {
		return
	}
	g.st.ended.Add(1)
	if g.st.inFlight.Add(1) > g.maxInFlight {
		g.st.inFlight.Add(-1)
		g.st.dropped.Add(1)
		return
	}
	g.next.OnEnd(s)
}

func (g *gateProcessor) Shutdown(ctx context.Context) error   { return g.next.Shutdown(ctx) }
func (g *gateProcessor) ForceFlush(ctx context.Context) error { return g.next.ForceFlush(ctx) }

// countingExporter accounts each export call's spans and tells the error
// handler when exports succeed again.
type countingExporter struct {
	next sdktrace.SpanExporter
	st   *pipelineStats
	errs *errorHandler
}

func (c *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := c.next.ExportSpans(ctx, spans)
	n := uint64(len(spans))
	c.st.inFlight.Add(-int64(n))
	if err != nil {
		c.st.exportFailures.Add(1)
		c.st.dropped.Add(n)
		return err
	}
	c.st.exported.Add(n)
	c.errs.succeeded()
	return nil
}

func (c *countingExporter) Shutdown(ctx context.Context) error { return c.next.Shutdown(ctx) }

// errorSummaryInterval is how often a lasting outage repeats itself in the log.
const errorSummaryInterval = 5 * time.Minute

// errorHandler is the OTEL error handler: a collector outage is one Warn, then
// a summary at most every errorSummaryInterval with how many errors it hid,
// and one Info when an export succeeds again. The SDK's default handler
// writes every failed batch through the stdlib log package instead, a line
// per batch for as long as the collector is down.
type errorHandler struct {
	log *slog.Logger
	now func() time.Time

	mu          sync.Mutex
	failing     bool
	suppressed  int
	lastSummary time.Time
}

func newErrorHandler(log *slog.Logger, now func() time.Time) *errorHandler {
	return &errorHandler{log: log, now: now}
}

// Handle implements otel.ErrorHandler. The error text comes from the exporter
// (status, connection error); the request's headers, where collector
// credentials live, are not part of it.
func (h *errorHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if !h.failing {
		h.failing, h.suppressed, h.lastSummary = true, 0, now
		h.log.Warn("otel: span export failing; repeats are summarised every 5 minutes", "err", err)
		return
	}
	h.suppressed++
	if now.Sub(h.lastSummary) >= errorSummaryInterval {
		h.log.Warn("otel: span export still failing", "suppressed_errors", h.suppressed, "err", err)
		h.suppressed, h.lastSummary = 0, now
	}
}

func (h *errorHandler) succeeded() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.failing {
		return
	}
	h.log.Info("otel: span export recovered", "suppressed_errors", h.suppressed)
	h.failing, h.suppressed = false, 0
}
