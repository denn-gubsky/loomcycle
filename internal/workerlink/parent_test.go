package workerlink

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A worker whose parent changes (the core died; the reaper adopted it) is
// cancelled; one whose parent stays is not.
func TestWatchParent_CancelsOnceTheParentChanges(t *testing.T) {
	var ppid atomic.Int64
	ppid.Store(42)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchParent(ctx, 42, func() { close(cancelled) }, func() int { return int(ppid.Load()) }, time.Millisecond)
	}()
	select {
	case <-cancelled:
		t.Fatal("cancelled while the parent was still there")
	case <-time.After(30 * time.Millisecond):
	}
	ppid.Store(1)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled after the parent changed")
	}
	<-done
}
