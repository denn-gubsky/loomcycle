package builtin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// heldWatch is a parent run's lifetime whose end reaches its background
// children only when the test lets it: context.AfterFunc hands a watch to its
// AfterFunc, which keeps it. It stands for the moment after a run is cancelled
// and before the goroutine of its watch has run.
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

// A poll-mode child whose run ends on its parent's cancel is filed
// cancelled, not failed. The cancel stops the child's own run directly, before
// the parent's end reaches the child's ctx through its lifetime watch; held
// here, so the order is the one that misfiled it.
func TestAgentPoll_AChildEndingOnItsParentsCancelIsFiledCancelled(t *testing.T) {
	life := newHeldWatch()
	bg := tools.NewBackground(life)
	cctx, err := bg.Start(context.Background(), tools.ChildSpec{RunID: "r_c", Agent: "worker", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		life.end() // the parent is cancelled, and its cancel ends this run
		return "", context.Canceled
	}}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	a.runBackgroundChild(cctx, bg, sem, true, func() {}, "r_c", bgEntry{name: "worker", index: -1})
	v, _ := bg.Lookup("r_c")
	if v.State != tools.ChildCancelled {
		t.Errorf("child = %s (%s), want cancelled", v.State, v.Result.Error)
	}
}
