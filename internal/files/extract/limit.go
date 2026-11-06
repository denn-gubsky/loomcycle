package extract

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

// The extraction budget (RFC FZ Part B item 3). Each extraction is bounded on
// its own: the input caps of each format, the archive's read budget, the XML
// element, depth and text caps, the table caps and the sink's output cap keep
// one file's heap to a few hundred MiB at most. What none of them bounds is how
// many run at once: every run, sub-agent and off-run call that extracts adds
// another, so a burst of large files multiplies that per-file peak by the
// burst. A Limiter turns the per-file bound into a process-wide one: at most N
// extractions hold a slot, the rest wait for one, and the process's extraction
// heap stays under N times the per-file peak.

// ErrBusy is what Acquire returns when its context ended before a slot came
// free. It wraps the context's error too, so a caller tells a cancelled call
// from a wait that ran out with errors.Is.
var ErrBusy = errors.New("extract: no extraction slot came free")

// Limiter bounds the extractions a process runs at once. One is made at boot
// and shared by every caller that extracts; it holds no file and no state of
// any extraction, only the count.
type Limiter struct {
	slots   chan struct{}
	waiting atomic.Int64
}

// NewLimiter returns a limiter of n slots. n <= 0 takes GOMAXPROCS: extraction
// is CPU work, so more at once than the process has threads only adds heap.
func NewLimiter(n int) *Limiter {
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	return &Limiter{slots: make(chan struct{}, n)}
}

// Acquire waits for a slot until ctx ends. The returned release gives the slot
// back and is safe to call more than once. A context that has already ended
// takes no slot, even a free one: the caller is gone and its work would be
// thrown away.
func (l *Limiter) Acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBusy, err)
	}
	select {
	case l.slots <- struct{}{}:
		return l.releaser(), nil
	default:
	}
	l.waiting.Add(1)
	defer l.waiting.Add(-1)
	select {
	case l.slots <- struct{}{}:
		return l.releaser(), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
}

func (l *Limiter) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { <-l.slots }) }
}

// LimiterStats is a limiter's state, for the metrics endpoint.
type LimiterStats struct {
	// Limit is the number of slots, InFlight the extractions holding one and
	// Waiting those waiting for one.
	Limit, InFlight, Waiting int
}

// Stats reports the limiter's state now.
func (l *Limiter) Stats() LimiterStats {
	return LimiterStats{Limit: cap(l.slots), InFlight: len(l.slots), Waiting: int(l.waiting.Load())}
}
