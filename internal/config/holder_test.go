package config

import (
	"sync"
	"testing"
)

// TestHolder_ConcurrentLoadStore is the RFC CK holder contract: concurrent
// Load()s against a Store() are race-free (atomic.Pointer) — under -race this
// fails if the swap is not atomic. Every Load returns a non-nil, internally
// consistent config.
func TestHolder_ConcurrentLoadStore(t *testing.T) {
	h := NewHolder(&Config{ProviderPriority: []string{"a"}})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				if c := h.Load(); c == nil || len(c.ProviderPriority) != 1 {
					t.Errorf("Load returned inconsistent config: %+v", c)
					return
				}
			}
		}()
	}
	for i := 0; i < 5000; i++ {
		h.Store(&Config{ProviderPriority: []string{"b"}})
		h.Store(&Config{ProviderPriority: []string{"a"}})
	}
	wg.Wait()
}
