package streamhttp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowReader emits one byte at the given interval, up to total bytes.
// Used to model "stream that's slow but progressing" vs "stalled stream".
type slowReader struct {
	interval time.Duration
	total    int
	emitted  int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.emitted >= s.total {
		return 0, io.EOF
	}
	time.Sleep(s.interval)
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = byte('a' + s.emitted%26)
	s.emitted++
	return 1, nil
}

func (s *slowReader) Close() error { return nil }

// stalledReader blocks forever (until Close).
type stalledReader struct {
	closed atomic.Bool
}

func (s *stalledReader) Read(p []byte) (int, error) {
	for !s.closed.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	return 0, io.ErrClosedPipe
}

func (s *stalledReader) Close() error {
	s.closed.Store(true)
	return nil
}

// TestWrapBody_StalledStreamFiresCancel verifies that a body which
// produces no Read'd bytes within idleTimeout triggers cancel(). This
// is the headline case — the regression we're guarding against.
func TestWrapBody_StalledStreamFiresCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stall := &stalledReader{}
	body := WrapBody(stall, 50*time.Millisecond, cancel)
	defer body.Close()

	// Read in a separate goroutine so we can assert ctx fires.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// Read will block until the underlying reader returns; once cancel
		// fires, our wrapper doesn't immediately interrupt the Read (the
		// underlying reader has to notice). For this test we just confirm
		// the context was cancelled within the idle window.
		_, _ = body.Read(make([]byte, 1))
	}()

	select {
	case <-ctx.Done():
		// Expected — timer fired and called cancel.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected ctx to be cancelled by idle timer; ctx still active")
	}
	// Unblock the read goroutine for clean shutdown.
	stall.Close()
	<-readDone
}

// TestWrapBody_ActiveStreamSurvives verifies that a stream that keeps
// producing bytes (every 30ms, well under the 100ms idle limit) is NOT
// killed by the idle timer. This is the case the old Client.Timeout
// got wrong: it killed slow-but-active streams at 5 min wall-clock.
func TestWrapBody_ActiveStreamSurvives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := WrapBody(&slowReader{interval: 30 * time.Millisecond, total: 20}, 100*time.Millisecond, cancel)
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 20 {
		t.Errorf("expected 20 bytes, got %d (%q)", len(got), string(got))
	}
	// ctx should NOT have been cancelled by the idle timer.
	select {
	case <-ctx.Done():
		t.Errorf("idle timer fired on an active stream: %v", ctx.Err())
	default:
		// Expected — active stream survived.
	}
}

// TestWrapBody_CloseIsIdempotent verifies Close can be called multiple
// times without panicking on double timer.Stop(). The driver code
// often has both an explicit Close path and a defer Close path.
func TestWrapBody_CloseIsIdempotent(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := WrapBody(io.NopCloser(strings.NewReader("hello")), 1*time.Second, cancel)
	if err := body.Close(); err != nil {
		t.Errorf("first Close failed: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Errorf("second Close failed: %v", err)
	}
}

// TestWrapBody_CloseStopsTimer verifies that closing the body before
// the idle timer fires prevents a spurious cancel() afterward. Without
// this guarantee, a normally-completed stream that gets Close()d would
// still cancel the parent context if the idle timer happened to fire
// during shutdown.
func TestWrapBody_CloseStopsTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := WrapBody(io.NopCloser(strings.NewReader("ok")), 30*time.Millisecond, cancel)

	// Read a byte, then immediately close.
	buf := make([]byte, 1)
	_, _ = body.Read(buf)
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Wait past the original idle timeout. ctx must remain non-cancelled
	// because Close stopped the timer.
	time.Sleep(80 * time.Millisecond)
	select {
	case <-ctx.Done():
		t.Errorf("ctx was cancelled after Close — timer was not stopped")
	default:
		// Expected — closing the body cleaned up the timer.
	}
}

// TestNewClient_NoWallclockTimeout sanity-checks that we did NOT set
// http.Client.Timeout (which would re-introduce the bug we're fixing).
// The streaming path relies entirely on Transport.ResponseHeaderTimeout
// + the body-wrap idle detection.
func TestNewClient_NoWallclockTimeout(t *testing.T) {
	c := NewClient(60 * time.Second)
	if c.Timeout != 0 {
		t.Errorf("http.Client.Timeout=%v; expected 0 (would kill long streams)", c.Timeout)
	}
}

// TestOptions_Resolve fills zero fields with defaults; explicit values
// pass through.
func TestOptions_Resolve(t *testing.T) {
	got := Options{}.Resolve()
	if got.HeaderTimeout != DefaultHeaderTimeout {
		t.Errorf("HeaderTimeout=%v; want %v", got.HeaderTimeout, DefaultHeaderTimeout)
	}
	if got.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout=%v; want %v", got.IdleTimeout, DefaultIdleTimeout)
	}

	got = Options{HeaderTimeout: 5 * time.Second, IdleTimeout: 7 * time.Second}.Resolve()
	if got.HeaderTimeout != 5*time.Second {
		t.Errorf("HeaderTimeout=%v; want 5s", got.HeaderTimeout)
	}
	if got.IdleTimeout != 7*time.Second {
		t.Errorf("IdleTimeout=%v; want 7s", got.IdleTimeout)
	}
}

// Ensure errors.Is wires through cleanly for the cancellation case
// (sanity check used elsewhere in this codebase).
var _ = errors.Is

// flipReadCloser returns one byte per Read until Close flips it to EOF.
// Read never blocks long, so the concurrent Read/Close test churns the
// timer Reset path against Close's Stop.
type flipReadCloser struct{ closed atomic.Bool }

func (f *flipReadCloser) Read(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, nil
}

func (f *flipReadCloser) Close() error { f.closed.Store(true); return nil }

// TestIdleReadCloser_ConcurrentReadClose drives Read (which resets the idle
// timer) concurrently with Close (which stops it) — exp7 I7. The fix
// serializes both timer operations under the mutex and refuses to reset the
// timer after Close. Run under `go test -race` to catch any timer-access data
// race; the loop also exercises the resurrect-after-close window the fix
// closes (a Read must not re-arm a stopped timer).
func TestIdleReadCloser_ConcurrentReadClose(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		rc := &flipReadCloser{}
		ctx, cancel := context.WithCancel(context.Background())
		body := WrapBody(rc, time.Millisecond, cancel)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			buf := make([]byte, 8)
			for j := 0; j < 2000; j++ {
				if _, err := body.Read(buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			_ = body.Close()
		}()
		wg.Wait()
		// Double Close stays idempotent.
		if err := body.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		cancel()
		_ = ctx
	}
}
