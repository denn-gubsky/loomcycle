package tools

import (
	"context"
	"sync"
	"time"
)

// heldWatch is a run lifetime whose end reaches its background children only
// when the test lets it: context.AfterFunc hands a watch to its AfterFunc,
// which keeps it. It stands for the moment after a run is cancelled and
// before the goroutine of its watch has run.
type heldWatch struct {
	done chan struct{}

	mu      sync.Mutex
	err     error
	watches []func()
}

func newHeldWatch() *heldWatch { return &heldWatch{done: make(chan struct{})} }

func (h *heldWatch) Deadline() (time.Time, bool) { return time.Time{}, false }
func (h *heldWatch) Done() <-chan struct{}       { return h.done }
func (h *heldWatch) Value(any) any               { return nil }

func (h *heldWatch) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *heldWatch) AfterFunc(f func()) func() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watches = append(h.watches, f)
	return func() bool { return true }
}

// end ends the run without running its watches.
func (h *heldWatch) end() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = context.Canceled
		close(h.done)
	}
}

var _ context.Context = (*heldWatch)(nil)
