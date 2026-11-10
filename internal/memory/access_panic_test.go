package memory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// panicOnceStore panics on its first access-count write and records after.
type panicOnceStore struct {
	*bumpRecorderStore
	calls atomic.Int32
}

func (s *panicOnceStore) MemoryBumpAccessBatch(ctx context.Context, bumps []store.MemoryAccessBump) error {
	if s.calls.Add(1) == 1 {
		panic("flush bug")
	}
	return s.bumpRecorderStore.MemoryBumpAccessBatch(ctx, bumps)
}

// TestAccessFlusher_APanickingFlushLeavesTheFlusherRunning: one tick that
// panics is recovered and the next tick still flushes.
func TestAccessFlusher_APanickingFlushLeavesTheFlusherRunning(t *testing.T) {
	fs := &panicOnceStore{bumpRecorderStore: newBumpRecorderStore()}
	f := NewAccessFlusher(fs, 10*time.Millisecond, 1000)
	f.Start()
	defer f.Stop()
	now := time.Now()
	f.Record("t", store.MemoryScopeUser, "u", "k", now)
	deadline := time.Now().Add(5 * time.Second)
	for fs.calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first flush never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.Record("t", store.MemoryScopeUser, "u", "k2", now)
	for {
		if c, _ := fs.get("t", store.MemoryScopeUser, "u", "k2"); c == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the flusher stopped after one panicking flush")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
