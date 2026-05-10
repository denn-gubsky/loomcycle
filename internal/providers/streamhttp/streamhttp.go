// Package streamhttp provides HTTP client + body-wrapping utilities for
// long-running streaming responses (LLM SSE streams). It replaces the
// "http.Client.Timeout = 5 * time.Minute" wall-clock pattern with two
// finer timeouts:
//
//   - ResponseHeaderTimeout, set on the Transport: caps time-to-first-byte.
//     The model has this long to start emitting after we POST.
//   - Per-byte idle timeout, applied to the response body via WrapBody:
//     caps stalls *between* body bytes. Long but actively-emitting streams
//     are allowed to run as long as they keep producing — only stalled
//     streams get killed.
//
// Why the change: a single Client.Timeout cancels the entire request,
// including the body read. For long final turns where the model is
// streaming a large structured response (e.g. job-searcher building a
// 25-position ingest payload), the wall-clock timeout fires mid-stream
// and the agent run fails. Per-byte idle is the right shape — it
// distinguishes "model is generating, just slowly" from "model has
// stopped emitting and we should give up."
package streamhttp

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Default timeout values applied when Options is zero.
const (
	// DefaultHeaderTimeout is generous because cold-start LLM endpoints
	// (Anthropic during a regional load spike, Ollama warming a 70b
	// model) can legitimately take 30+ seconds to deliver headers.
	DefaultHeaderTimeout = 60 * time.Second

	// DefaultIdleTimeout is the gap-between-body-bytes ceiling. With
	// reasoning-model thinking blocks (Anthropic extended thinking,
	// DeepSeek-R1 chain-of-thought), pauses of 30-60 seconds between
	// tokens are normal. 90 seconds gives a safety margin without
	// keeping a fully-stalled connection forever.
	DefaultIdleTimeout = 90 * time.Second
)

// Options configures the timeouts a driver applies to its provider HTTP
// calls. A zero field falls back to the matching Default*. Driver
// constructors should resolve the defaults at New() time so the Driver
// struct holds the real value (no surprise behaviour change if a future
// caller mutates this constant).
type Options struct {
	HeaderTimeout time.Duration
	IdleTimeout   time.Duration
}

// Resolve returns a copy with zero fields replaced by their defaults.
// Drivers call this once in New() so subsequent Call()s see concrete values.
func (o Options) Resolve() Options {
	if o.HeaderTimeout <= 0 {
		o.HeaderTimeout = DefaultHeaderTimeout
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	return o
}

// NewClient returns an *http.Client suitable for LLM streaming. The
// client has NO Timeout (would cap mid-stream); the Transport carries
// ResponseHeaderTimeout. Per-byte idle detection on the response body
// is the caller's job — wrap resp.Body with WrapBody in each Call().
//
// We build a fresh Transport rather than cloning http.DefaultTransport.
// Sharing the default pool across drivers means one stalled connection
// (e.g. a hung Anthropic session) can starve another driver's reuse —
// observed in production. A per-driver Transport keeps the failure
// domains separate.
func NewClient(headerTimeout time.Duration) *http.Client {
	if headerTimeout <= 0 {
		headerTimeout = DefaultHeaderTimeout
	}
	return &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: headerTimeout,
			// Defaults from http.DefaultTransport for the rest:
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// WrapBody wraps an HTTP response body so that it cancels the request's
// context after idleTimeout elapses without a Read returning bytes.
// Each Read with n>0 resets the timer; Close stops the timer and is
// idempotent.
//
// Use this in driver Call() implementations:
//
//	ctx, cancel := context.WithCancel(parentCtx)
//	req, _ := http.NewRequestWithContext(ctx, ...)
//	resp, err := client.Do(req)
//	if err != nil {
//	    cancel()
//	    return nil, err
//	}
//	resp.Body = streamhttp.WrapBody(resp.Body, idleTimeout, cancel)
//	// ... read resp.Body in the SSE loop. defer resp.Body.Close().
//	// defer cancel() at the top of Call() is the standard ctx cleanup.
//
// On idle, cancel() fires; the next Read on the underlying body returns
// context.Canceled (or wraps it via net/http). The SSE parser surfaces
// this as the existing "stream read: context deadline exceeded" error
// shape, so callers don't need to handle a new error type.
//
// There's a small race window: if a Read is in flight when the timer
// fires, cancel() runs even though bytes were arriving. The
// consequence is a spurious cancel right at the idle threshold — rare,
// benign, and the loop will retry the request via the regular
// rate-limit / retry path.
func WrapBody(rc io.ReadCloser, idleTimeout time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &idleReadCloser{
		rc:     rc,
		idle:   idleTimeout,
		cancel: cancel,
		timer:  time.AfterFunc(idleTimeout, cancel),
	}
}

// idleReadCloser is the WrapBody implementation. The timer is created
// in WrapBody (so we don't need a separate Start step) and torn down
// in Close. Concurrent Close vs Read is allowed; the mutex makes Close
// idempotent and prevents a double Stop on the timer.
type idleReadCloser struct {
	rc     io.ReadCloser
	idle   time.Duration
	cancel context.CancelFunc
	timer  *time.Timer

	mu     sync.Mutex
	closed bool
}

func (i *idleReadCloser) Read(p []byte) (int, error) {
	n, err := i.rc.Read(p)
	if n > 0 {
		// Reset returns false when the timer was already fired/stopped;
		// we don't care — if it already fired, cancel() ran and the next
		// Read will see the context error. If it was stopped (Close),
		// we're shutting down anyway.
		i.timer.Reset(i.idle)
	}
	return n, err
}

func (i *idleReadCloser) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()
	i.timer.Stop()
	return i.rc.Close()
}
