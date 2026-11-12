package otel

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
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
	// the export queue was full, carried by an export call that failed, or
	// rejected by a collector that accepted the rest of their batch (an OTLP
	// partial success, which is not counted as a failed export).
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

// Stats reports the export pipeline's counters. Ended is read last: a span is
// counted ended before it can be counted exported or dropped, so reading it
// after them keeps Ended >= Exported + Dropped in what a scrape sees.
func Stats() SpanStats {
	exported := stats.exported.Load()
	dropped := stats.dropped.Load()
	failures := stats.exportFailures.Load()
	return SpanStats{
		Enabled:        stats.enabled.Load(),
		Ended:          stats.ended.Load(),
		Exported:       exported,
		Dropped:        dropped,
		ExportFailures: failures,
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
	// dropTenantOnly: tenant export is on, and a span of a trace sampled
	// only for a tenant's export is none of the operator's — neither
	// exported nor counted, as if its sampler had dropped it.
	dropTenantOnly bool
}

func (g *gateProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	g.next.OnStart(ctx, s)
}

func (g *gateProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	// The batch processor exports sampled spans only; an unsampled one is
	// neither counted nor queued.
	if !s.SpanContext().IsSampled() || (g.dropTenantOnly && tenantOnly(s.SpanContext())) {
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
		rejected, partial := partialRejected(err)
		if !partial {
			c.st.exportFailures.Add(1)
			c.st.dropped.Add(n)
			return err
		}
		// The collector answered and took the batch but for the spans it
		// rejected: an export that worked, with some spans dropped.
		r := min(uint64(max(rejected, 0)), n)
		c.st.exported.Add(n - r)
		c.st.dropped.Add(r)
		c.errs.succeeded()
		c.errs.rejected(r, err)
		// Not handed on: the batch processor would pass it to the error
		// handler as a failed export.
		return nil
	}
	c.st.exported.Add(n)
	c.errs.succeeded()
	return nil
}

// partialRejected reports whether err says only that the collector accepted
// part of a batch (an OTLP partial success), and how many spans it rejected.
//
// The OTLP exporters turn a successful response that rejected spans into an
// error of their internal type PartialSuccess (a generated copy in each
// exporter's internal package, so neither errors.As nor a shared type can
// reach it). It is recognised by its type name and its RejectedItems field;
// an error tree holding anything else is a failure.
func partialRejected(err error) (int64, bool) {
	var (
		total int64
		found bool
		walk  func(error) bool
	)
	walk = func(e error) bool {
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				if c != nil && !walk(c) {
					return false
				}
			}
			return true
		case interface{ Unwrap() error }:
			if inner := u.Unwrap(); inner != nil {
				return walk(inner)
			}
			return false
		}
		v := reflect.ValueOf(e)
		if v.Kind() != reflect.Struct || v.Type().Name() != "PartialSuccess" || !strings.HasPrefix(v.Type().PkgPath(), "go.opentelemetry.io/otel/exporters/otlp/") {
			return false
		}
		f := v.FieldByName("RejectedItems")
		if !f.IsValid() || f.Kind() != reflect.Int64 {
			return false
		}
		total += f.Int()
		found = true
		return true
	}
	if !walk(err) || !found {
		return 0, false
	}
	return total, true
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

	// Spans a collector rejected from batches it otherwise took: logged
	// at most once per errorSummaryInterval, with the count since.
	rejectedSince  uint64
	lastRejectedAt time.Time
}

func newErrorHandler(log *slog.Logger, now func() time.Time) *errorHandler {
	return &errorHandler{log: log, now: now}
}

// Handle implements otel.ErrorHandler. The error text comes from the exporter
// (status, connection error); the request's headers, where collector
// credentials live, are not part of it, and the endpoint URL it may quote is
// logged without userinfo or query values (sanitizeError).
func (h *errorHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if !h.failing {
		h.failing, h.suppressed, h.lastSummary = true, 0, now
		h.log.Warn("otel: span export failing; repeats are summarised every 5 minutes", "err", sanitizeError(err))
		return
	}
	h.suppressed++
	if now.Sub(h.lastSummary) >= errorSummaryInterval {
		h.log.Warn("otel: span export still failing", "suppressed_errors", h.suppressed, "err", sanitizeError(err))
		h.suppressed, h.lastSummary = 0, now
	}
}

// rejected notes spans a collector refused in a partial success: not an
// outage, but data lost, so it is logged, rate-limited like an outage.
func (h *errorHandler) rejected(n uint64, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rejectedSince += n
	now := h.now()
	if !h.lastRejectedAt.IsZero() && now.Sub(h.lastRejectedAt) < errorSummaryInterval {
		return
	}
	h.log.Warn("otel: the trace collector rejected spans", "rejected_spans", h.rejectedSince, "err", sanitizeError(err))
	h.rejectedSince, h.lastRejectedAt = 0, now
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
