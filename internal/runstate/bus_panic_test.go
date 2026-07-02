package runstate

import (
	"sync"
	"testing"
)

// TestBus_PublishConcurrentCloseNoPanic is the regression for the
// send-on-closed-channel panic: publishLocal snapshotted the subscribers,
// released the lock, then sent — so unsubscribe()'s close() could interleave
// and the send would panic (the select default does NOT save a send on a closed
// channel). On the backplane goroutine (no recover) that crashed the process.
//
// This hammers Publish from several goroutines while a subscriber rapidly
// Subscribe/Close-cycles the SAME user key, forcing the send-vs-close race. On
// the pre-fix code it panics "send on closed channel" (crashing the test); with
// delivery under the lock it completes cleanly. Run under -race in CI.
func TestBus_PublishConcurrentCloseNoPanic(t *testing.T) {
	b := NewBus()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(RunStateEvent{UserID: "u"})
				}
			}
		}()
	}

	// Churn subscriptions on the same key the publishers target: each Subscribe
	// makes the sub visible to publishLocal; each Close removes + closes its
	// channel — the exact interleaving that panicked.
	for i := 0; i < 5000; i++ {
		sub := b.Subscribe("u")
		sub.Close()
	}

	close(stop)
	wg.Wait()
}
