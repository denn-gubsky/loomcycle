// TestSSEKeepalive_ConcurrentWritesNeverCorruptFrames asserts that
// two goroutines writing to the same SSE stream never produce
// interleaved bytes mid-frame.
//
// Why this file exists separately, behind a `!race` build tag:
//
// The test deliberately uses an UNSYNCHRONISED writer
// (`splittingWriter`) that appends bytes one at a time with
// `runtime.Gosched()` between them. The point is to make
// byte-level interleaving observable in the buffer WITHOUT
// depending on `-race` to flag it. Two concurrent unsynchronised
// `Write` calls interleave; the per-frame structural assertions
// then catch the corruption.
//
// Under `-race`, that same intentional racy access on the writer's
// `buf` field trips the data-race detector — so the test fails for
// the design reason, not because the system-under-test is broken.
// The race detector is its own coverage path: if you remove the
// mutex on `sse.send`, the production response writer races, and
// `go test -race` flags it directly. We don't need this test to
// cover that case too.
//
// Same pattern as `TestBashTimeout` (also `//go:build !race`):
// some tests deliberately exercise concurrency in ways the race
// detector can't distinguish from real bugs.

//go:build !race

package http

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// splittingWriter is captureWriter's evil twin: Write splits each
// payload into byte-by-byte appends with a runtime.Gosched() between
// every byte. Two concurrent Write calls without external
// synchronisation visibly interleave in the buffer.
//
// The buffer access in Write is intentionally unsynchronised; rely
// on the sse.mu in the system-under-test, not on this writer, for
// cross-goroutine ordering.
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
func (s *splittingWriter) Flush()             {}

// SnapshotAfterQuiescing should only be called after every writer
// goroutine has stopped (e.g. cancel + wait). Reading concurrently
// with active Write calls would race on the unsynchronised buf.
func (s *splittingWriter) SnapshotAfterQuiescing() string {
	return string(s.buf)
}

func TestSSEKeepalive_ConcurrentWritesNeverCorruptFrames(t *testing.T) {
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
