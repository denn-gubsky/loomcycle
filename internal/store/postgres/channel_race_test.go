package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestChannelSubscribe_ConcurrentPublishRace exercises the
// publish-then-notify path at high concurrency against real Postgres
// to reproduce the residual subscribe-empty race that PR #234
// papered over with a bounded retry.
//
// Reproduction strategy: each worker publishes a fresh message THEN
// subscribes from "cur_0" expecting to find its own just-published
// msg_id in the result set. Because cur_0 replays ALL messages, the
// only way to miss the row is a true MVCC visibility lag — the row
// committed but the subscribing transaction's snapshot doesn't yet
// include it. We measure both "missed on first read" (the race
// happened) and "still missing after 30ms" (a correctness bug, not
// just commit-visibility lag — would mean the row is never visible
// from this pool connection's snapshot).
//
// The test asserts `silentMissesAfterDelay == 0` (correctness). It
// only LOGS missesOnFirstRead so we can characterize the race rate
// without making the test flaky. The diagnostic log lines from
// LOOMCYCLE_CHANNEL_DEBUG=1 + the channel.go readWithRetry path
// surface the same data in production; this test is the
// reproduction harness operators can use locally.
//
// Run with -count=N to gather a histogram. At 200 workers × 10 msgs
// on a dev box (Postgres in Docker, single client) the race trips
// rarely; under sustained production load (50+ concurrent runs)
// PR #234's analysis reported ~2% trip rate, the count this test is
// scaffolded to characterize.
func TestChannelSubscribe_ConcurrentPublishRace(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fx := freshSchema(t, dsn)
	defer fx.cleanup()
	s := fx.store

	const (
		numWorkers    = 200
		msgsPerWorker = 10
	)

	var (
		missesOnFirstRead      atomic.Int64
		silentMissesAfterDelay atomic.Int64
		totalReads             atomic.Int64
		wg                     sync.WaitGroup
	)

	ctx := context.Background()
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < msgsPerWorker; j++ {
				payload := json.RawMessage(`{"w":` + strconv.Itoa(workerID) +
					`,"j":` + strconv.Itoa(j) + `}`)
				msgID, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
					Channel: "race-test",
					Scope:   store.MemoryScopeGlobal,
					ScopeID: "",
					Payload: payload,
				}, 0)
				if err != nil {
					t.Errorf("publish (worker %d msg %d): %v", workerID, j, err)
					return
				}
				totalReads.Add(1)
				msgs, _, err := s.ChannelSubscribe(ctx, "race-test",
					store.MemoryScopeGlobal, "", "cur_0", 5000)
				if err != nil {
					t.Errorf("subscribe (worker %d msg %d): %v", workerID, j, err)
					return
				}
				if !containsMsg(msgs, msgID) {
					missesOnFirstRead.Add(1)
					time.Sleep(30 * time.Millisecond)
					msgs2, _, err := s.ChannelSubscribe(ctx, "race-test",
						store.MemoryScopeGlobal, "", "cur_0", 5000)
					if err != nil {
						t.Errorf("subscribe retry (worker %d msg %d): %v",
							workerID, j, err)
						return
					}
					if !containsMsg(msgs2, msgID) {
						silentMissesAfterDelay.Add(1)
					}
				}
			}
		}(i)
	}
	wg.Wait()

	t.Logf("concurrent publish/subscribe race: workers=%d msgs_per_worker=%d total_reads=%d misses_on_first_read=%d (%.2f%%) silent_misses_after_30ms=%d",
		numWorkers, msgsPerWorker, totalReads.Load(),
		missesOnFirstRead.Load(),
		100*float64(missesOnFirstRead.Load())/float64(totalReads.Load()),
		silentMissesAfterDelay.Load())

	if silentMissesAfterDelay.Load() > 0 {
		t.Errorf("silent misses after 30ms delay: %d — the just-published row was never visible across a fresh subscribe. This indicates a correctness bug, not just commit-visibility lag; investigate before merging.",
			silentMissesAfterDelay.Load())
	}
}

// TestChannelSubscribe_CursorAdvanceRace exercises the
// non-cur_0 path that more closely mirrors the production tool-layer
// flow: each subscriber reads with a cursor, advances on success,
// reads again. Under contention with concurrent publishers a
// subscriber's cursor-advancing read should see every newly committed
// row that's > its cursor — but if the MVCC snapshot is acquired
// before the commit propagates to this pool connection, the read can
// miss the row.
//
// Distinct from the first test in that:
//   - cursor advances on each read (matches readWithRetry's call site)
//   - publish and subscribe interleave per-worker (high contention)
//   - we measure how many UNIQUE message ids the test never delivered
//     (= correctness gap; nonzero is a bug)
func TestChannelSubscribe_CursorAdvanceRace(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fx := freshSchema(t, dsn)
	defer fx.cleanup()
	s := fx.store

	const (
		numWorkers     = 50
		msgsPerWorker  = 10
		readBatchLimit = 100
	)

	var (
		publishedIDs sync.Map
		seenIDs      sync.Map
		publisherWg  sync.WaitGroup
		subscriberWg sync.WaitGroup
	)

	ctx := context.Background()

	// Phase 1: publishers run to completion. The two-phase structure
	// makes the test reliable on slow CI runners: subscribers don't
	// race the publishers' commit cadence. A row never delivered when
	// the subscriber drains AFTER all publishers committed is a true
	// storage gap, not a timing artifact. Original 2026-05-27 review
	// caught the single-WaitGroup-with-time.After variant as flaky.
	for i := 0; i < numWorkers; i++ {
		publisherWg.Add(1)
		go func(workerID int) {
			defer publisherWg.Done()
			for j := 0; j < msgsPerWorker; j++ {
				payload := json.RawMessage(`{"w":` + strconv.Itoa(workerID) +
					`,"j":` + strconv.Itoa(j) + `}`)
				id, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
					Channel: "cursor-race",
					Scope:   store.MemoryScopeGlobal,
					Payload: payload,
				}, 0)
				if err != nil {
					t.Errorf("publish: %v", err)
					return
				}
				publishedIDs.Store(id, true)
			}
		}(i)
	}
	publisherWg.Wait()

	// Phase 2: subscribers drain. With every published row committed
	// already, the only way for a subscriber to miss an id is a true
	// MVCC visibility gap on a fresh subscribe — exactly the
	// correctness signal we want to assert on. Generous deadline
	// (10s) because slow CI runners drain serially.
	subscriberDeadline := time.After(10 * time.Second)
	for s_i := 0; s_i < numWorkers/5; s_i++ {
		subscriberWg.Add(1)
		go func(subID int) {
			defer subscriberWg.Done()
			cursor := ""
			idle := 0
			for {
				select {
				case <-subscriberDeadline:
					return
				default:
				}
				msgs, next, err := s.ChannelSubscribe(ctx, "cursor-race",
					store.MemoryScopeGlobal, "", cursor, readBatchLimit)
				if err != nil {
					t.Errorf("subscribe: %v", err)
					return
				}
				if len(msgs) == 0 {
					idle++
					if idle > 5 {
						return
					}
					time.Sleep(20 * time.Millisecond)
					continue
				}
				idle = 0
				for _, m := range msgs {
					seenIDs.Store(m.ID, true)
				}
				if next != "" {
					cursor = next
				}
			}
		}(s_i)
	}
	subscriberWg.Wait()

	// Tally missing ids — published but never delivered.
	missing := 0
	publishedIDs.Range(func(k, _ any) bool {
		if _, ok := seenIDs.Load(k); !ok {
			missing++
		}
		return true
	})

	totalPublished := 0
	publishedIDs.Range(func(_, _ any) bool { totalPublished++; return true })

	t.Logf("cursor-advance race: workers=%d msgs_per_worker=%d published=%d missing=%d",
		numWorkers, msgsPerWorker, totalPublished, missing)

	if missing > 0 {
		t.Errorf("delivery gap: %d / %d messages were never returned by ANY subscriber. The cursor-advance read race delivered a correctness gap, not just a latency one. Investigate before merging.",
			missing, totalPublished)
	}
}

func containsMsg(msgs []store.ChannelMessage, id string) bool {
	for _, m := range msgs {
		if m.ID == id {
			return true
		}
	}
	return false
}
