package channelhooks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// panicRenewStore panics on every lease renewal.
type panicRenewStore struct{ store.Store }

func (panicRenewStore) ChannelHookRenew(context.Context, store.ChannelMessageKey, string, time.Time) (bool, error) {
	panic("renew bug")
}

// TestJob_APanickingRenewStopsTheJob: renewals that end in a panic stop the
// job, as a lost lease does; otherwise it would go on deciding while its lease
// lapses and a second worker claims the message.
func TestJob_APanickingRenewStopsTheJob(t *testing.T) {
	f := newFixture(t)
	f.w.cfg.Store = panicRenewStore{Store: f.st}
	f.w.cfg.Lease = 30 * time.Millisecond
	var lostCalls atomic.Int32
	lost := func() { lostCalls.Add(1) }
	j := &job{w: f.w, lost: lost}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	safego.Go(ctx, "test.renew", func() {
		defer close(done)
		j.renew(ctx, lost, time.Hour)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renew did not panic")
	}
	if lostCalls.Load() != 1 {
		t.Fatalf("lost called %d times after a panicking renew, want 1", lostCalls.Load())
	}
}
