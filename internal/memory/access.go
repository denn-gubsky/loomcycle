package memory

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// access.go — RFC BL hybrid retrieval, OQ #4: the batched access-count flush.
//
// On every search HIT the in-process backend records a +1 access for the
// returned entries. Writing that straight to the DB would amplify writes on
// the hot search path (one UPDATE per result per search), so instead we
// accumulate per-(tenant,scope,scope_id,key) deltas in a per-replica in-memory
// map and flush them in batches on a ticker + a size trigger + graceful
// shutdown. This mirrors the sqlmem debounced-`touch` pattern: the hot path
// only touches a mutex-guarded map; the DB write is amortised and off-path.
//
// Semantics are ADVISORY and per-replica: counts may be a little stale and,
// across replicas, are summed additively by MemoryBumpAccessBatch (the UPDATE
// adds the delta), so no count is lost to a lost-update race. A crash drops at
// most the un-flushed window — acceptable for a ranking signal.

const (
	// accessFlushDefaultInterval is the ticker period. ~30s keeps counts
	// fresh enough for ranking while bounding write rate.
	accessFlushDefaultInterval = 30 * time.Second
	// accessFlushDefaultMaxBatch triggers an early flush once this many
	// distinct rows are pending, so a burst of searches doesn't grow the map
	// unbounded between ticks.
	accessFlushDefaultMaxBatch = 512
)

type accessKey struct {
	tenant  string
	scope   string
	scopeID string
	key     string
}

type accessDelta struct {
	count      int64
	lastAccess time.Time
}

// AccessFlusher accumulates access-count bumps and flushes them to the store
// in batches. It is safe for concurrent Record from many search goroutines.
// Construct with NewAccessFlusher, call Start once, and Stop on shutdown
// (Stop performs a final flush).
type AccessFlusher struct {
	store    store.Store
	interval time.Duration
	maxBatch int

	mu      sync.Mutex
	pending map[accessKey]accessDelta

	flushSignal chan struct{}
	stop        chan struct{}
	done        chan struct{}
}

// NewAccessFlusher builds a flusher over the given store. A non-positive
// interval or maxBatch falls back to the package defaults.
func NewAccessFlusher(s store.Store, interval time.Duration, maxBatch int) *AccessFlusher {
	if interval <= 0 {
		interval = accessFlushDefaultInterval
	}
	if maxBatch <= 0 {
		maxBatch = accessFlushDefaultMaxBatch
	}
	return &AccessFlusher{
		store:       s,
		interval:    interval,
		maxBatch:    maxBatch,
		pending:     make(map[accessKey]accessDelta),
		flushSignal: make(chan struct{}, 1),
	}
}

// Record accumulates a +1 access for one entry at time `at`. Cheap: one
// mutex-guarded map touch, no DB I/O. When the pending set crosses maxBatch it
// nudges the flusher goroutine (non-blocking).
func (f *AccessFlusher) Record(tenant string, scope store.MemoryScope, scopeID, key string, at time.Time) {
	k := accessKey{tenant: tenant, scope: string(scope), scopeID: scopeID, key: key}
	f.mu.Lock()
	d := f.pending[k]
	d.count++
	if at.After(d.lastAccess) {
		d.lastAccess = at
	}
	f.pending[k] = d
	size := len(f.pending)
	f.mu.Unlock()

	if size >= f.maxBatch {
		select {
		case f.flushSignal <- struct{}{}:
		default: // a flush is already pending; coalesce
		}
	}
}

// drain atomically swaps out the pending map and returns it as a bump slice.
// Records during the subsequent DB write accumulate into the fresh map.
func (f *AccessFlusher) drain() []store.MemoryAccessBump {
	f.mu.Lock()
	if len(f.pending) == 0 {
		f.mu.Unlock()
		return nil
	}
	pend := f.pending
	f.pending = make(map[accessKey]accessDelta)
	f.mu.Unlock()

	bumps := make([]store.MemoryAccessBump, 0, len(pend))
	for k, d := range pend {
		bumps = append(bumps, store.MemoryAccessBump{
			TenantID:   k.tenant,
			Scope:      store.MemoryScope(k.scope),
			ScopeID:    k.scopeID,
			Key:        k.key,
			CountDelta: d.count,
			LastAccess: d.lastAccess,
		})
	}
	return bumps
}

// Flush drains the pending bumps and writes them additively. A no-op when
// nothing is pending. Exposed for tests and the shutdown path.
func (f *AccessFlusher) Flush(ctx context.Context) error {
	bumps := f.drain()
	if len(bumps) == 0 {
		return nil
	}
	return f.store.MemoryBumpAccessBatch(ctx, bumps)
}

// Start launches the background flush goroutine (ticker + size-trigger).
// Idempotent guard: a second Start without a Stop is a caller bug; we don't
// defend against it because main wires exactly one.
func (f *AccessFlusher) Start() {
	f.stop = make(chan struct{})
	f.done = make(chan struct{})
	stop, done := f.stop, f.done
	go func() {
		defer close(done)
		t := time.NewTicker(f.interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				f.flushLogErr()
			case <-f.flushSignal:
				f.flushLogErr()
			}
		}
	}()
}

// Stop signals the goroutine, JOINS it, then performs a final flush so a
// clean shutdown never loses the last window's counts.
func (f *AccessFlusher) Stop() {
	if f.stop == nil {
		return
	}
	close(f.stop)
	<-f.done
	f.stop = nil
	f.flushLogErr()
}

// flushLogErr flushes under a bounded context and logs (never returns) errors
// — an access-count write is advisory, so a transient store fault must not
// crash the flush loop.
func (f *AccessFlusher) flushLogErr() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.Flush(ctx); err != nil {
		log.Printf("memory: access-count flush: %v", err)
	}
}
