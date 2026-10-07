package http

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func keyed(key, user string) connector.SpawnRunRequest {
	return connector.SpawnRunRequest{Agent: "r", UserID: user, Segments: oneUserSeg("go"), IdempotencyKey: key}
}

func batchOne(t *testing.T, s *Server, ctx context.Context, mode string, timeoutMS int, req connector.SpawnRunRequest) connector.SpawnRunResult {
	t.Helper()
	res, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Mode: mode, TimeoutMS: timeoutMS, Spawns: []connector.SpawnRunRequest{req}})
	if err != nil {
		t.Fatalf("SpawnRunBatch(%s): %v", mode, err)
	}
	return res.Results[0]
}

// sessionCount is how many sessions the store holds: a retry that reached
// CreateSession before learning it was a duplicate would leave one behind.
func sessionCount(t *testing.T, st store.Store) int64 {
	t.Helper()
	_, n, err := st.ListSessions(context.Background(), store.SessionFilter{}, 1, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	return n
}

// The request's done-when: two calls with one key start one run, and both
// return its ids. The second neither waits for a slot nor leaves a session.
func TestIdempotencyKey_ASecondDetachedCallReturnsTheFirstRun(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1) // one slot, held by the first run
	ctx := context.Background()
	first := batchOne(t, s, ctx, "detach", 0, keyed("ccq-score:c1:abc", "u1"))
	if first.Status != "running" || first.RunID == "" || first.Deduplicated {
		t.Fatalf("first call = %+v, want a fresh running run", first)
	}
	sessions := sessionCount(t, s.store)

	second := batchOne(t, s, ctx, "detach", 0, keyed("ccq-score:c1:abc", "u1"))
	if !second.Deduplicated || second.RunID != first.RunID || second.AgentID != first.AgentID || second.SessionID != first.SessionID {
		t.Fatalf("second call = %+v, want the first run %+v marked deduplicated", second, first)
	}
	if second.Status != "running" {
		t.Errorf("second call status = %q, want the run's current status (running)", second.Status)
	}
	if n := sessionCount(t, s.store); n != sessions {
		t.Errorf("the duplicate left %d session(s) behind", n-sessions)
	}
	run, err := s.store.GetRun(ctx, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(run.IdempotencyKey, "run:") || !strings.HasSuffix(run.IdempotencyKey, ":u1:ccq-score:c1:abc") {
		t.Errorf("stored key = %q, want the caller's key under the run: prefix, scoped to its user", run.IdempotencyKey)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, first.RunID, store.RunCompleted)
}

// A join on a key that is already held waits for that run and answers with
// its result, read from the row.
func TestIdempotencyKey_AJoinWaitsForTheRunThatHoldsTheKey(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	ctx := context.Background()
	first := batchOne(t, s, ctx, "detach", 0, keyed("k1", "u1"))

	joined := make(chan connector.SpawnRunResult, 1)
	go func() { joined <- batchOne(t, s, ctx, "join", 0, keyed("k1", "u1")) }()
	select {
	case r := <-joined:
		t.Fatalf("the join returned while the run was still going: %+v", r)
	case <-time.After(3 * existingRunPollInterval):
	}
	gate <- struct{}{}
	select {
	case r := <-joined:
		if !r.Deduplicated || r.RunID != first.RunID || r.Status != "completed" || r.FinalText != "partial" {
			t.Errorf("join = %+v, want the first run completed with its answer, deduplicated", r)
		}
		if r.Usage == nil || r.Usage.OutputTokens == 0 {
			t.Errorf("join usage = %+v, want the run's usage from its row", r.Usage)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the join did not return after the run ended")
	}

	// And once more after the run has ended: answered at once.
	again := batchOne(t, s, ctx, "join", 0, keyed("k1", "u1"))
	if !again.Deduplicated || again.RunID != first.RunID || again.Status != "completed" || again.FinalText != "partial" {
		t.Errorf("a join after the end = %+v", again)
	}
}

// The joiner did not start the run, so its timeout ends the wait and nothing
// else: the run goes on, and is reported as running.
func TestIdempotencyKey_AJoinersTimeoutLeavesTheRunRunning(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	ctx := context.Background()
	first := batchOne(t, s, ctx, "detach", 0, keyed("k1", "u1"))

	r := batchOne(t, s, ctx, "join", 150, keyed("k1", "u1"))
	if !r.Deduplicated || r.RunID != first.RunID || r.Status != "running" {
		t.Fatalf("timed-out join = %+v, want the run reported running", r)
	}
	time.Sleep(100 * time.Millisecond)
	if run, _ := s.store.GetRun(ctx, first.RunID); run.Status != store.RunRunning {
		t.Fatalf("the run is %q after a joiner's timeout, want it still running", run.Status)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, first.RunID, store.RunCompleted)
}

// Two first calls at the same moment: the unique index lets one create the
// run, and the other is answered with it.
func TestIdempotencyKey_ConcurrentFirstCallsStartOneRun(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	ctx := context.Background()
	const n = 6
	results := make([]connector.SpawnRunResult, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = batchOne(t, s, ctx, "detach", 0, keyed("same", "u1"))
		}()
	}
	wg.Wait()
	fresh := 0
	for i, r := range results {
		if r.RunID == "" || r.RunID != results[0].RunID {
			t.Fatalf("results[%d] = %+v, want every call to name run %s", i, r, results[0].RunID)
		}
		if !r.Deduplicated {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("%d calls report a fresh run, want exactly 1", fresh)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, results[0].RunID, store.RunCompleted)
}

// A key belongs to one tenant and user. Another user's same key is its own
// run, and is never answered with someone else's.
func TestIdempotencyKey_IsScopedToTheUser(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	ctx := context.Background()
	a := batchOne(t, s, ctx, "detach", 0, keyed("k", "u1"))
	b := batchOne(t, s, ctx, "detach", 0, keyed("k", "u2"))
	if b.Deduplicated || b.RunID == "" || b.RunID == a.RunID {
		t.Fatalf("another user's call = %+v, want its own run (the first was %s)", b, a.RunID)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	waitRunStatus(t, s.store, a.RunID, store.RunCompleted)
	waitRunStatus(t, s.store, b.RunID, store.RunCompleted)
}

// A caller's key is never stored as sent, so one spelled like a schedule's or
// a webhook's key cannot take, or be refused by, theirs.
func TestIdempotencyKey_CannotCollideWithARuntimeKey(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	ctx := context.Background()
	for _, key := range []string{"sched:def_1:1700000000000000", "webhook:t:hook:delivery-1"} {
		r := batchOne(t, s, ctx, "detach", 0, keyed(key, "u1"))
		run, err := s.store.GetRun(ctx, r.RunID)
		if err != nil {
			t.Fatalf("%s: %v (result %+v)", key, err, r)
		}
		if run.IdempotencyKey == key || !strings.HasPrefix(run.IdempotencyKey, "run:") {
			t.Errorf("caller key %q stored as %q", key, run.IdempotencyKey)
		}
		if _, found, _ := s.store.RunByIdempotencyKey(ctx, key); found {
			t.Errorf("a runtime lookup of %q finds the caller's run", key)
		}
		gate <- struct{}{}
		waitRunStatus(t, s.store, r.RunID, store.RunCompleted)
	}
}

func TestIdempotencyKey_MalformedRequestsAreRefused(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		spawns []connector.SpawnRunRequest
		want   string
	}{
		"a character outside the set": {[]connector.SpawnRunRequest{keyed("a b", "u1")}, "idempotency_key must match"},
		"too long":                    {[]connector.SpawnRunRequest{keyed(strings.Repeat("k", 201), "u1")}, "idempotency_key must match"},
		"one key on two children":     {[]connector.SpawnRunRequest{keyed("k", "u1"), keyed("other", "u1"), keyed("k", "u1")}, "spawns[0] and spawns[2] carry the same idempotency_key"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Mode: "detach", Spawns: tc.spawns})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	// On a continuation the key has nothing to guard: refused, not ignored.
	req := keyed("k", "u1")
	req.SessionID = "s_whatever"
	res, err := s.SpawnRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "fresh run") {
		t.Errorf("a key with session_id = %+v, want a refusal", res)
	}
	// A key at the length limit is accepted.
	if _, ok := connector.ValidateIdempotencyKey(strings.Repeat("k", 200)); !ok {
		t.Error("a 200-character key was refused")
	}
}

func TestConfiguredRun_RefusesAnIdempotencyKey(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4)
	var d runDraft
	d.SpawnRunRequest = keyed("k", "u1")
	if _, err := s.createConfiguredRunCore(context.Background(), d); err == nil || !strings.Contains(err.Error(), "idempotency_key") {
		t.Errorf("err = %v, want the draft refused for its idempotency_key", err)
	}
}

// A run held for a review verdict does not end on its own. A caller joined to
// it by key is told so at once instead of waiting for a person.
func TestIdempotencyKey_AJoinDoesNotWaitOnARunHeldForReview(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	s.SetSteerRegistry(steer.NewRegistry(0)) // a hold needs somewhere to take its verdict
	ctx := context.Background()
	held := keyed("k", "u1")
	review := true
	held.Review = &review
	go func() { _, _ = s.SpawnRun(ctx, held) }() // blocks until a verdict
	defer cancelAllRuns(s)
	gate <- struct{}{}

	var runID string
	waitFor(t, "the run to be held for review", func() bool {
		run, found, _ := s.store.RunByIdempotencyKey(ctx, clientRunKey("", "u1", "k"))
		if !found {
			return false
		}
		runID = run.ID
		state, _ := awaited.ForRun(ctx, s.store, run.ID)
		return state == awaited.Review
	})

	done := make(chan connector.SpawnRunResult, 1)
	go func() { done <- batchOne(t, s, ctx, "join", 0, keyed("k", "u1")) }()
	select {
	case r := <-done:
		if !r.Deduplicated || r.RunID != runID || r.Status != "running" {
			t.Errorf("join on a held run = %+v, want it reported running at once", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the join waited on a run that is held for review")
	}
}

// missLookup hides the key from one lookup, which is what a request sees when
// another with the same key creates its run a moment later. It is installed
// before any run starts — a server's store is read by its running runs — and
// the test picks which lookup misses.
type missLookup struct {
	store.Store
	miss    int32 // the lookup, counted from 1, that finds nothing
	lookups atomic.Int32
}

func (m *missLookup) RunByIdempotencyKey(ctx context.Context, key string) (store.Run, bool, error) {
	if m.lookups.Add(1) == m.miss {
		return store.Run{}, false, nil
	}
	return m.Store.RunByIdempotencyKey(ctx, key)
}

// The lookup before admission can miss a run that is created right after it.
// The unique index then refuses the second run, and the request is answered
// with the first instead of failing.
func TestIdempotencyKey_ARequestRefusedByTheIndexIsAnsweredWithTheWinner(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	// Lookup 1 is the first request's own; lookup 2, the second request's, is
	// the one that misses.
	blind := &missLookup{Store: s.store, miss: 2}
	s.store = blind
	ctx := context.Background()
	first := batchOne(t, s, ctx, "detach", 0, keyed("k", "u1"))
	sessions := sessionCount(t, s.store)

	second := batchOne(t, s, ctx, "detach", 0, keyed("k", "u1"))
	if blind.lookups.Load() < 3 {
		t.Fatalf("the requests made %d lookup(s); the index refusal was not reached", blind.lookups.Load())
	}
	if !second.Deduplicated || second.RunID != first.RunID {
		t.Fatalf("second call = %+v, want the first run %s, deduplicated", second, first.RunID)
	}
	// This path does reach CreateSession before the index refuses the run.
	if n := sessionCount(t, s.store) - sessions; n > 1 {
		t.Errorf("the refused request left %d sessions behind, want at most its own one", n)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, first.RunID, store.RunCompleted)
}

// The run a key resolves to is handed to the caller, so the lookup applies the
// same ownership gate as every other read of a run's content. An isolated
// member is refused another user's run even when the identity it was looked
// up under matches the row.
func TestIdempotencyKey_TheLookupAppliesTheRunOwnershipGate(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	ctx := context.Background()
	first := batchOne(t, s, ctx, "detach", 0, keyed("k", "u1"))
	key := clientRunKey("", "u1", "k")

	if dup, err := s.runHoldingClientKey(ctx, key, "", "u1", false); err != nil || dup == nil || dup.RunID != first.RunID {
		t.Fatalf("the owner's lookup = %+v, %v; want run %s", dup, err, first.RunID)
	}
	stranger := auth.WithPrincipal(ctx, auth.Principal{TenantID: "", Subject: "u2", Scopes: []string{auth.ScopeUser}})
	dup, err := s.runHoldingClientKey(stranger, key, "", "u1", false)
	if dup != nil || err == nil || !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("an isolated member's lookup of another user's run = %+v, %v; want a refusal", dup, err)
	}
	if err != nil && strings.Contains(err.Error(), first.RunID) {
		t.Errorf("the refusal names the run: %v", err)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, first.RunID, store.RunCompleted)
}
