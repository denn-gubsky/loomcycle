package concurrency

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAcquireForUser_NoRaceWithCapSet is the regression for the unlocked read of
// s.maxPerUser in AcquireForUser: WithPerUserCap writes it under s.mu, so the
// bare read raced the setter (the API doc promises it's safe to call after the
// semaphore is in use). Run under `go test -race`: this hammers AcquireForUser
// concurrently with WithPerUserCap — the race detector fails the test on the
// pre-fix code and passes once the read is taken under the lock.
func TestAcquireForUser_NoRaceWithCapSet(t *testing.T) {
	s := New(8, 16, time.Second)
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				s.WithPerUserCap(n % 5) // runtime cap changes (the doc's "safe after in use")
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				rel, err := s.AcquireForUser(context.Background(), "u")
				if err == nil && rel != nil {
					rel()
				}
			}
		}()
	}
	wg.Wait()
}
