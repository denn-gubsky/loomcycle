package http

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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
