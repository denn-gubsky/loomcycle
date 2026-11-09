package otel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	genai "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// AttrAttempt is a provider request's attempt number within its call,
	// 1-based: a call retried after a 429 has spans with 1, 2, ….
	AttrAttempt = "loomcycle.attempt"
	// AttrRetryAfterMs is how long the call waited before sending this
	// attempt: the provider's Retry-After when it sent one, else the retry
	// schedule's wait. Absent on a first attempt.
	AttrRetryAfterMs = "loomcycle.retry_after_ms"
)

// Values of error.type on a failed provider attempt, so a 429 is filtered by
// attribute rather than by searching error text.
const (
	ErrorTypeRateLimited = "rate_limited" // 429
	ErrorTypeOverloaded  = "overloaded"   // 529 (Anthropic) or 503
	ErrorTypeTimeout     = "timeout"      // 408, 504, or a request deadline
	ErrorTypeNetwork     = "network"      // no response: connection, DNS, TLS
	ErrorTypeCancelled   = "cancelled"    // the caller gave up on the request
	ErrorTypeClientError = "client_error" // any other 4xx
	ErrorTypeServerError = "server_error" // any other 5xx
)

type attemptKey struct{}

type attemptInfo struct {
	n      int
	waited time.Duration
}

// WithAttempt returns ctx naming the attempt number of the provider request
// made under it, and how long the caller waited before making it. The retry
// loop sets it; RecordProviderCall stamps it, so a driver's attempt closure
// needs no parameter for it.
func WithAttempt(ctx context.Context, n int, waited time.Duration) context.Context {
	return context.WithValue(ctx, attemptKey{}, attemptInfo{n: n, waited: waited})
}

// AttemptSeq numbers the attempts of one provider call across retry layers:
// a driver that wraps the 429 retry loop in a retry of its own (Ollama's
// runner-death retry) puts one on ctx, so the inner loop's attempts keep
// counting instead of starting again at 1. Attempts are sequential, so it is
// used from one goroutine and holds no lock.
type AttemptSeq struct {
	n    int
	wait time.Duration
}

type attemptSeqKey struct{}

// WithAttemptSeq returns ctx carrying a new attempt sequence, and the
// sequence.
func WithAttemptSeq(ctx context.Context) (context.Context, *AttemptSeq) {
	s := &AttemptSeq{}
	return context.WithValue(ctx, attemptSeqKey{}, s), s
}

// AttemptSeqFrom returns the sequence on ctx, or nil.
func AttemptSeqFrom(ctx context.Context) *AttemptSeq {
	s, _ := ctx.Value(attemptSeqKey{}).(*AttemptSeq)
	return s
}

// Waited records a wait an outer retry layer slept before the next attempt;
// that attempt's span reports it in loomcycle.retry_after_ms.
func (s *AttemptSeq) Waited(d time.Duration) { s.wait += d }

// Next returns ctx for the next attempt (WithAttempt): its number in the
// sequence, and waited plus any wait an outer layer recorded since the last
// attempt.
func (s *AttemptSeq) Next(ctx context.Context, waited time.Duration) context.Context {
	s.n++
	waited, s.wait = waited+s.wait, 0
	return WithAttempt(ctx, s.n, waited)
}

func attemptAttrs(ctx context.Context, kvs []attribute.KeyValue) []attribute.KeyValue {
	a, ok := ctx.Value(attemptKey{}).(attemptInfo)
	if !ok {
		return kvs
	}
	kvs = append(kvs, attribute.Int(AttrAttempt, a.n))
	if a.waited > 0 {
		kvs = append(kvs, attribute.Int64(AttrRetryAfterMs, a.waited.Milliseconds()))
	}
	return kvs
}

// ErrorTypeForStatus is error.type for an HTTP status a provider answered
// with, "" for a success.
func ErrorTypeForStatus(code int) string {
	switch {
	case code < 400:
		return ""
	case code == http.StatusTooManyRequests:
		return ErrorTypeRateLimited
	case code == 529 || code == http.StatusServiceUnavailable:
		return ErrorTypeOverloaded
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		return ErrorTypeTimeout
	case code < 500:
		return ErrorTypeClientError
	}
	return ErrorTypeServerError
}

// errorTypeForErr is error.type for a request that got no response.
func errorTypeForErr(err error) string {
	if errors.Is(err, context.Canceled) {
		return ErrorTypeCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTypeTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrorTypeTimeout
	}
	return ErrorTypeNetwork
}

// SetProviderResponse records how one provider attempt ended: its HTTP status
// and, on failure, error.type and the span's error status. resp and err are
// what the HTTP client returned; either may be nil.
func SetProviderResponse(span trace.Span, resp *http.Response, err error) {
	if span == nil || !span.IsRecording() {
		return
	}
	if err != nil {
		span.SetAttributes(genai.ErrorTypeKey.String(errorTypeForErr(err)))
		SetSpanError(span, err)
		return
	}
	if resp == nil {
		return
	}
	span.SetAttributes(genai.HTTPResponseStatusCodeKey.Int(resp.StatusCode))
	if et := ErrorTypeForStatus(resp.StatusCode); et != "" {
		span.SetAttributes(genai.ErrorTypeKey.String(et))
		SetSpanErrorMessage(span, "http "+resp.Status)
	}
}
