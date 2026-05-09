package http

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// captureWriter is a minimal http.ResponseWriter+Flusher for tests.
// Tracks every write under a mutex so the keepalive goroutine and the
// test goroutine don't race when reading the buffer.
type captureWriter struct {
	mu      sync.Mutex
	hdr     http.Header
	status  int
	buf     bytes.Buffer
	flushes int
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{hdr: http.Header{}}
}

func (c *captureWriter) Header() http.Header {
	return c.hdr
}
func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *captureWriter) WriteHeader(s int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}
func (c *captureWriter) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushes++
}
func (c *captureWriter) Snapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// splittingWriter is captureWriter's evil twin: Write splits each
// payload into byte-by-byte appends with a runtime.Gosched() between
// every byte. Two concurrent Write calls without external
// synchronisation will visibly interleave in the buffer — not just
// race-detected, but observable in the wire output.
//
// Used by TestSSEKeepalive_ConcurrentWritesNeverCorruptFrames to
// prove the sse.mu actually prevents interleaving without depending
// on `-race`. The buffer access in Write is intentionally
// unsynchronised; rely on the sse.mu in the system-under-test, not
// on this writer, for cross-goroutine ordering.
type splittingWriter struct {
	hdr    http.Header
	buf    []byte // intentionally NOT mutex-protected
	status int
}

func newSplittingWriter() *splittingWriter {
	return &splittingWriter{hdr: http.Header{}}
}

func (s *splittingWriter) Header() http.Header { return s.hdr }
func (s *splittingWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		s.buf = append(s.buf, b)
		runtime.Gosched()
	}
	return len(p), nil
}
func (s *splittingWriter) WriteHeader(st int) { s.status = st }
func (s *splittingWriter) Flush()              {}

// SnapshotAfterQuiescing should only be called after every writer
// goroutine has stopped (e.g. cancel + wait). Reading concurrently
// with active Write calls would race on the unsynchronised buf.
func (s *splittingWriter) SnapshotAfterQuiescing() string {
	return string(s.buf)
}

func TestSSEKeepalive_EmitsCommentFramesOnIdleStream(t *testing.T) {
	// 50 ms interval keeps the test fast but is well above the noise
	// floor of goroutine scheduling on CI. We expect at least 2 frames
	// over a 200 ms window.
	w := newCaptureWriter()
	s, ok := newSSE(w)
	if !ok {
		t.Fatal("captureWriter must satisfy http.Flusher")
	}
	s.start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKeepalive(ctx, 50*time.Millisecond)

	time.Sleep(200 * time.Millisecond)
	cancel()
	// Give the goroutine a tick to observe ctx done and exit.
	time.Sleep(20 * time.Millisecond)

	got := w.Snapshot()
	count := strings.Count(got, ": keepalive\n\n")
	if count < 2 {
		t.Errorf("keepalive frames = %d, want ≥ 2 (got payload: %q)", count, got)
	}
}

func TestSSEKeepalive_DisabledWhenIntervalZero(t *testing.T) {
	w := newCaptureWriter()
	s, _ := newSSE(w)
	s.start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKeepalive(ctx, 0)

	time.Sleep(80 * time.Millisecond)
	cancel()

	if strings.Contains(w.Snapshot(), ": keepalive") {
		t.Error("keepalive frame emitted with interval=0; should be disabled")
	}
}

func TestSSEKeepalive_GoroutineExitsOnContextCancel(t *testing.T) {
	// A keepalive goroutine that outlives the request would leak across
	// streams. Verify cancelling ctx promptly stops emissions.
	w := newCaptureWriter()
	s, _ := newSSE(w)
	s.start()

	ctx, cancel := context.WithCancel(context.Background())
	s.startKeepalive(ctx, 25*time.Millisecond)

	time.Sleep(80 * time.Millisecond)
	cancel()
	// Wait for the goroutine to observe cancel + finish its current
	// iteration. After this point, no new keepalive frames should append.
	time.Sleep(50 * time.Millisecond)
	beforeQuiet := strings.Count(w.Snapshot(), ": keepalive\n\n")

	time.Sleep(120 * time.Millisecond)
	afterQuiet := strings.Count(w.Snapshot(), ": keepalive\n\n")

	if afterQuiet != beforeQuiet {
		t.Errorf("keepalive count grew after cancel: before=%d after=%d (goroutine leaked)",
			beforeQuiet, afterQuiet)
	}
}

func TestSSEKeepalive_ConcurrentWritesNeverCorruptFrames(t *testing.T) {
	// Pin the reason the sse struct carries a mutex: with two
	// goroutines writing to the same response writer (main agent loop
	// + keepalive ticker), unsynchronised writes interleave bytes
	// mid-frame. Use a writer (`splittingWriter`) that appends one
	// byte at a time with `runtime.Gosched()` between bytes — that
	// makes interleaving visible WITHOUT depending on `-race`. The
	// real `http.ResponseWriter` is not thread-safe either, so this
	// matches production conditions more closely than a mutex-guarded
	// captureWriter.
	//
	// Verified the test catches the bug: temporarily removing the
	// mutex in sse.send produces malformed frames and trips the
	// assertions below.
	w := newSplittingWriter()
	s, _ := newSSE(w)
	s.start()

	ctx, cancel := context.WithCancel(context.Background())
	s.startKeepalive(ctx, 1*time.Millisecond)

	const sends = 500
	for i := 0; i < sends; i++ {
		s.send(providers.Event{Type: providers.EventText, Text: "hi"})
	}
	cancel()
	time.Sleep(20 * time.Millisecond) // give keepalive goroutine time to exit

	got := w.SnapshotAfterQuiescing()
	frames := strings.Split(got, "\n\n")
	for i, f := range frames {
		if f == "" {
			continue // trailing empty after the final "\n\n"
		}
		if strings.HasPrefix(f, ":") {
			// comment-only keepalive frame
			if !strings.HasPrefix(f, ": keepalive") {
				t.Errorf("frame[%d] looks like a comment but isn't a keepalive: %q", i, f)
			}
			continue
		}
		// Real event must have an event: line then a data: line.
		lines := strings.Split(f, "\n")
		if len(lines) < 2 ||
			!strings.HasPrefix(lines[0], "event:") ||
			!strings.HasPrefix(lines[1], "data:") {
			t.Errorf("frame[%d] malformed (interleaved write?): %q", i, f)
		}
	}
}
