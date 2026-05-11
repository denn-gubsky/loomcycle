// Package storetest is the shared behavioural test suite every Store adapter
// runs against. It exists so the SQLite and Postgres adapters can't drift
// silently — every contract here is asserted against both backends in CI.
//
// Adapter packages call Run(t, factory) from their own *_test.go file:
//
//	func TestStoreContract(t *testing.T) {
//	    storetest.Run(t, func(t *testing.T) (store.Store, func()) {
//	        s := newTestStore(t)
//	        return s, func() { _ = s.Close() }
//	    })
//	}
//
// The factory must return a FRESH, empty store on each call. Tests assume
// they own the schema and that no rows exist at start. Cleanup runs after
// the individual test (not the parent t.Run) so each subtest is isolated.
//
// Adapter-specific tests (idempotent re-migration, NULL column verification
// via direct SQL, advisory-lock contention, etc.) stay in the adapter's own
// _test.go file. The contract here is the abstract Store surface only.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Factory builds a fresh Store and a cleanup function. The store must be
// empty (no sessions/runs/events) and isolated from any other concurrent
// store instances the test suite might create. Cleanup releases the
// backend resources (drops temp tables, closes connection pools, etc.).
type Factory func(t *testing.T) (store.Store, func())

// Run executes the full Store contract against the factory. Call this from
// the adapter's own test file. Each subtest gets a fresh Store via the
// factory; one adapter bug surfaces as one failed subtest, not a cascade.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, store.Store)
	}{
		{"CreateAndGetSession", testCreateAndGetSession},
		{"GetSessionNotFound", testGetSessionNotFound},
		{"RunLifecycle", testRunLifecycle},
		{"AppendEventOnUnknownRun", testAppendEventOnUnknownRun},
		{"CreateRunOnUnknownSession", testCreateRunOnUnknownSession},
		{"FinishRunIdempotent", testFinishRunIdempotent},
		{"GetTranscriptEmpty", testGetTranscriptEmpty},
		{"CreateSessionUserIDRoundTrip", testCreateSessionUserIDRoundTrip},
		{"CreateRunIdentityRoundTrip", testCreateRunIdentityRoundTrip},
		{"GetRunByAgentIDNotFound", testGetRunByAgentIDNotFound},
		{"GetRunByAgentIDReturnsMostRecent", testGetRunByAgentIDReturnsMostRecent},
		{"ListActiveRunsByUser", testListActiveRunsByUser},
		{"ListUsers", testListUsers},
		{"ListRunsByParentAgentID", testListRunsByParentAgentID},
		{"UpdateHeartbeat", testUpdateHeartbeat},
		{"FinishRunCancelledTerminal", testFinishRunCancelledTerminal},
		{"TranscriptOrderedAcrossRuns", testTranscriptOrderedAcrossRuns},
		{"SweepStaleRuns", testSweepStaleRuns},
		{"MemorySetGetRoundTrip", testMemorySetGetRoundTrip},
		{"MemoryGetNotFound", testMemoryGetNotFound},
		{"MemoryOverwriteUpdatesValue", testMemoryOverwriteUpdatesValue},
		{"MemoryDelete", testMemoryDelete},
		{"MemoryListPrefix", testMemoryListPrefix},
		{"MemoryListTruncation", testMemoryListTruncation},
		{"MemoryTTLExpiry", testMemoryTTLExpiry},
		{"MemorySweepReapsExpired", testMemorySweepReapsExpired},
		{"MemoryIncrementOnNewKey", testMemoryIncrementOnNewKey},
		{"MemoryIncrementOnExistingNumber", testMemoryIncrementOnExistingNumber},
		{"MemoryIncrementOnNonNumberFails", testMemoryIncrementOnNonNumberFails},
		{"MemoryIncrementOnExpiredKey", testMemoryIncrementOnExpiredKey},
		{"MemoryScopeIsolation", testMemoryScopeIsolation},
		{"MemoryListScopeIDs", testMemoryListScopeIDs},
		{"MemoryIncrementIsAtomicUnderConcurrency", testMemoryIncrementIsAtomicUnderConcurrency},
		{"ChannelPublishSubscribeRoundTrip", testChannelPublishSubscribeRoundTrip},
		{"ChannelSubscribeEmptyChannel", testChannelSubscribeEmptyChannel},
		{"ChannelCursorAdvancesAcrossSubscribes", testChannelCursorAdvancesAcrossSubscribes},
		{"ChannelAckIsIdempotent", testChannelAckIsIdempotent},
		{"ChannelAckRejectsCursorRegression", testChannelAckRejectsCursorRegression},
		{"ChannelTTLFilteredAtRead", testChannelTTLFilteredAtRead},
		{"ChannelSweepReapsExpired", testChannelSweepReapsExpired},
		{"ChannelMaxMessagesTrimsOldest", testChannelMaxMessagesTrimsOldest},
		{"ChannelScopeIsolation", testChannelScopeIsolation},
		{"ChannelPeekDoesNotConsume", testChannelPeekDoesNotConsume},
		{"ChannelReplayFromCursorZero", testChannelReplayFromCursorZero},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, cleanup := factory(t)
			defer cleanup()
			tc.fn(t, s)
		})
	}
}

func testCreateAndGetSession(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "tenant-a", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" || sess.TenantID != "tenant-a" || sess.Agent != "default" {
		t.Errorf("session: %+v", sess)
	}
	if sess.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sess.ID || got.TenantID != "tenant-a" || got.Agent != "default" {
		t.Errorf("got: %+v, want: %+v", got, sess)
	}
}

func testGetSessionNotFound(t *testing.T, s store.Store) {
	_, err := s.GetSession(context.Background(), "s_nope")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
	if nf != nil && nf.Kind != "session" {
		t.Errorf("Kind = %q", nf.Kind)
	}
}

func testRunLifecycle(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")

	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != sess.ID || run.Status != store.RunRunning {
		t.Errorf("run: %+v", run)
	}

	for i, payload := range [][]byte{
		[]byte(`{"type":"started"}`),
		[]byte(`{"type":"text","text":"hi"}`),
		[]byte(`{"type":"done","stop_reason":"end_turn"}`),
	} {
		typ := []string{"started", "text", "done"}[i]
		if err := s.AppendEvent(ctx, run.ID, typ, payload); err != nil {
			t.Fatalf("AppendEvent[%d]: %v", i, err)
		}
	}

	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 10, OutputTokens: 5, Model: "fake-model"}, ""); err != nil {
		t.Fatal(err)
	}

	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 3 {
		t.Fatalf("transcript len = %d, want 3", len(transcript))
	}
	wantTypes := []string{"started", "text", "done"}
	for i, want := range wantTypes {
		if transcript[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, transcript[i].Type, want)
		}
		if transcript[i].SessionID != sess.ID {
			t.Errorf("event %d session_id = %q", i, transcript[i].SessionID)
		}
		if transcript[i].RunID != run.ID {
			t.Errorf("event %d run_id = %q", i, transcript[i].RunID)
		}
	}
	for i := 1; i < len(transcript); i++ {
		if transcript[i].Seq <= transcript[i-1].Seq {
			t.Errorf("seq not ascending at %d: %d -> %d", i, transcript[i-1].Seq, transcript[i].Seq)
		}
	}
}

func testAppendEventOnUnknownRun(t *testing.T, s store.Store) {
	err := s.AppendEvent(context.Background(), "r_nope", "text", []byte(`{}`))
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
	if nf != nil && nf.Kind != "run" {
		t.Errorf("Kind = %q", nf.Kind)
	}
}

func testCreateRunOnUnknownSession(t *testing.T, s store.Store) {
	_, err := s.CreateRun(context.Background(), "s_nope", store.RunIdentity{})
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
}

// FinishRun is idempotent: calling FinishRun(cancelled) on an
// already-completed run must NOT clobber the terminal status. The
// status='running' guard in the UPDATE clause is what enforces this.
func testFinishRunIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_idem"})

	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, run.ID, store.RunCancelled, "cancelled", store.Usage{}, ""); err != nil {
		t.Fatalf("idempotent FinishRun should not error: %v", err)
	}
	got, err := s.GetRunByAgentID(ctx, "a_idem")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunCompleted {
		t.Errorf("status clobbered: got %q, want completed", got.Status)
	}
}

func testGetTranscriptEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 0 {
		t.Errorf("len = %d, want 0", len(transcript))
	}
}

// Empty userID round-trips as empty-string from GetSession (NULL in the
// underlying column; verified per-adapter via direct SQL).
func testCreateSessionUserIDRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "tenant-x", "agent-x", "user-42")
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID != "user-42" {
		t.Errorf("CreateSession returned UserID %q, want user-42", sess.UserID)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != "user-42" {
		t.Errorf("GetSession UserID = %q, want user-42", got.UserID)
	}

	emptySess, _ := s.CreateSession(ctx, "t", "a", "")
	gotEmpty, err := s.GetSession(ctx, emptySess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotEmpty.UserID != "" {
		t.Errorf("empty UserID round-trip: got %q, want \"\"", gotEmpty.UserID)
	}
}

func testCreateRunIdentityRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	identity := store.RunIdentity{
		AgentID:       "a_top",
		ParentAgentID: "a_grandparent",
		ParentRunID:   "r_grandparent",
		UserID:        "user-1",
	}
	run, err := s.CreateRun(ctx, sess.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentID != "a_top" || run.UserID != "user-1" || run.ParentAgentID != "a_grandparent" || run.ParentRunID != "r_grandparent" {
		t.Errorf("CreateRun did not return the supplied identity: %+v", run)
	}

	got, err := s.GetRunByAgentID(ctx, "a_top")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != run.ID {
		t.Errorf("GetRunByAgentID returned wrong run: got %s, want %s", got.ID, run.ID)
	}
	if got.AgentID != "a_top" || got.UserID != "user-1" {
		t.Errorf("identity not preserved through GetRunByAgentID: %+v", got)
	}
}

func testGetRunByAgentIDNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"a_does_not_exist", ""} {
		_, err := s.GetRunByAgentID(ctx, id)
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			t.Errorf("agent_id %q: got %v (%T), want *store.ErrNotFound", id, err, err)
		}
	}
}

func testGetRunByAgentIDReturnsMostRecent(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	older, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_reused", UserID: "u"})
	_ = s.FinishRun(ctx, older.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	time.Sleep(2 * time.Millisecond)
	newer, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_reused", UserID: "u"})

	got, err := s.GetRunByAgentID(ctx, "a_reused")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != newer.ID {
		t.Errorf("got run %s, want most-recent %s (older was %s)", got.ID, newer.ID, older.ID)
	}
}

func testListActiveRunsByUser(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "alice")

	r1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_1", UserID: "alice"})
	r2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_2", UserID: "alice"})
	_, _ = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_other", UserID: "bob"})
	_ = s.FinishRun(ctx, r1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	active, err := s.ListActiveRunsByUser(ctx, "alice", store.RunRunning)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != r2.ID {
		ids := make([]string, len(active))
		for i, r := range active {
			ids[i] = r.ID
		}
		t.Errorf("running for alice: got %v, want [%s]", ids, r2.ID)
	}

	all, _ := s.ListActiveRunsByUser(ctx, "alice", "")
	if len(all) != 2 {
		t.Errorf("all for alice: got %d, want 2", len(all))
	}
	for _, r := range all {
		if r.UserID == "bob" {
			t.Errorf("bob's run leaked into alice's list: %s", r.ID)
		}
	}

	if got, _ := s.ListActiveRunsByUser(ctx, "", store.RunRunning); len(got) != 0 {
		t.Errorf("empty userID should return no rows, got %d", len(got))
	}
}

func testListUsers(t *testing.T, s store.Store) {
	ctx := context.Background()
	sessAlice, _ := s.CreateSession(ctx, "t", "a", "alice")
	sessBob, _ := s.CreateSession(ctx, "t", "a", "bob")

	// alice: 2 runs, 1 still running.
	rA1, _ := s.CreateRun(ctx, sessAlice.ID, store.RunIdentity{AgentID: "a_alice1", UserID: "alice"})
	_, _ = s.CreateRun(ctx, sessAlice.ID, store.RunIdentity{AgentID: "a_alice2", UserID: "alice"})
	_ = s.FinishRun(ctx, rA1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// bob: 1 run, completed.
	rB1, _ := s.CreateRun(ctx, sessBob.ID, store.RunIdentity{AgentID: "a_bob1", UserID: "bob"})
	_ = s.FinishRun(ctx, rB1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// Empty-userID run should NOT show up in the listing — filtered
	// by the WHERE user_id != '' clause.
	sessAnon, _ := s.CreateSession(ctx, "t", "a", "")
	_, _ = s.CreateRun(ctx, sessAnon.ID, store.RunIdentity{AgentID: "a_anon", UserID: ""})

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Order: most-recent activity first. Last run was bob's
	// (CreateRun ordering), then alice's. But CreateRun timestamps
	// are nanosecond-resolution and assigned in order — bob is later
	// than alice.
	wantIDs := map[string]struct {
		running int
		total   int
	}{
		"alice": {running: 1, total: 2},
		"bob":   {running: 0, total: 1},
	}
	if len(users) != len(wantIDs) {
		ids := make([]string, len(users))
		for i, u := range users {
			ids[i] = u.UserID
		}
		t.Fatalf("ListUsers returned %d users, want %d (got %v)", len(users), len(wantIDs), ids)
	}
	for _, u := range users {
		want, ok := wantIDs[u.UserID]
		if !ok {
			t.Errorf("unexpected user_id in result: %q", u.UserID)
			continue
		}
		if u.RunningCount != want.running {
			t.Errorf("%s.running = %d, want %d", u.UserID, u.RunningCount, want.running)
		}
		if u.TotalCount != want.total {
			t.Errorf("%s.total = %d, want %d", u.UserID, u.TotalCount, want.total)
		}
		if u.LastStartedAt.IsZero() {
			t.Errorf("%s.last_started_at is zero; should reflect a real timestamp", u.UserID)
		}
	}
}

func testListRunsByParentAgentID(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	c1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_child1", ParentAgentID: "a_parent", UserID: "u"})
	c2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_child2", ParentAgentID: "a_parent", UserID: "u"})
	_, _ = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_gc", ParentAgentID: "a_child1", UserID: "u"})

	got, err := s.ListRunsByParentAgentID(ctx, "a_parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (direct children only)", len(got))
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids[c1.ID] || !ids[c2.ID] {
		t.Errorf("expected children %s and %s, got %+v", c1.ID, c2.ID, got)
	}
}

func testUpdateHeartbeat(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_hb", UserID: "u"})

	if err := s.UpdateHeartbeat(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.GetRunByAgentID(ctx, "a_hb")
	if first.LastHeartbeatAt.IsZero() {
		t.Fatal("first heartbeat should be non-zero")
	}

	time.Sleep(2 * time.Millisecond)
	_ = s.UpdateHeartbeat(ctx, run.ID)
	second, _ := s.GetRunByAgentID(ctx, "a_hb")
	if !second.LastHeartbeatAt.After(first.LastHeartbeatAt) {
		t.Errorf("heartbeat not monotonic: first=%v second=%v", first.LastHeartbeatAt, second.LastHeartbeatAt)
	}

	// Finish the run, then try another heartbeat — must be a no-op.
	_ = s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")
	final, _ := s.GetRunByAgentID(ctx, "a_hb")
	time.Sleep(2 * time.Millisecond)
	_ = s.UpdateHeartbeat(ctx, run.ID)
	afterFinish, _ := s.GetRunByAgentID(ctx, "a_hb")
	if !afterFinish.LastHeartbeatAt.Equal(final.LastHeartbeatAt) {
		t.Errorf("heartbeat advanced after run terminal: %v vs %v",
			afterFinish.LastHeartbeatAt, final.LastHeartbeatAt)
	}
}

func testFinishRunCancelledTerminal(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_x", UserID: "u"})

	if err := s.FinishRun(ctx, run.ID, store.RunCancelled, "user_clicked_stop", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRunByAgentID(ctx, "a_x")
	if got.Status != store.RunCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.StopReason != "user_clicked_stop" {
		t.Errorf("stop_reason = %q", got.StopReason)
	}

	_ = s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")
	again, _ := s.GetRunByAgentID(ctx, "a_x")
	if again.Status != store.RunCancelled {
		t.Errorf("late completed should not overwrite cancelled, got %q", again.Status)
	}
}

// SweepStaleRuns flips runs whose heartbeats are older than the cutoff
// (or that never heartbeated and whose started_at is older than the
// cutoff) to status="failed" with error="heartbeat timeout". Already-
// terminal runs are not touched. Fresh runs that have heartbeated
// recently aren't touched either.
func testSweepStaleRuns(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	// 1) A stale run that heartbeated long ago.
	stale, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_stale"})
	_ = s.UpdateHeartbeat(ctx, stale.ID)

	// 2) A run that never heartbeated AND was created before the cutoff
	//    we'll pass below — must also be swept.
	_, _ = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_no_hb"})

	// Sleep enough that "now" is after both rows' (last_heartbeat_at
	// or started_at). 60ms gives enough headroom to survive
	// heavily-loaded CI runners doing dozens of parallel Postgres
	// tests against a containerised fixture; tighter values
	// (we ran 30ms briefly) flaked under load.
	time.Sleep(60 * time.Millisecond)

	// 3) A fresh run that heartbeated AFTER the cutoff — must NOT be
	//    swept.
	fresh, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_fresh"})
	_ = s.UpdateHeartbeat(ctx, fresh.ID)

	// 4) An already-terminal run — must NOT be touched.
	terminalRun, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_terminal"})
	_ = s.FinishRun(ctx, terminalRun.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// Cutoff: between the stale row's last activity and the fresh
	// row's. 30ms-back keeps the stale + noHB rows past the cutoff
	// while the fresh row stays after it. Doubling from the previous
	// 15ms gives ~30ms of margin in either direction.
	cutoff := time.Now().Add(-30 * time.Millisecond)

	swept, err := s.SweepStaleRuns(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 2 {
		t.Errorf("SweepStaleRuns returned %d, want 2 (stale + noHB)", swept)
	}

	// Verify per-row outcomes.
	staleAfter, _ := s.GetRunByAgentID(ctx, "a_stale")
	if staleAfter.Status != store.RunFailed {
		t.Errorf("stale row: status=%q, want failed", staleAfter.Status)
	}
	if staleAfter.ErrorMsg != "heartbeat timeout" {
		t.Errorf("stale row: error=%q, want \"heartbeat timeout\"", staleAfter.ErrorMsg)
	}
	noHBAfter, _ := s.GetRunByAgentID(ctx, "a_no_hb")
	if noHBAfter.Status != store.RunFailed {
		t.Errorf("no-heartbeat row: status=%q, want failed", noHBAfter.Status)
	}
	freshAfter, _ := s.GetRunByAgentID(ctx, "a_fresh")
	if freshAfter.Status != store.RunRunning {
		t.Errorf("fresh row was swept: status=%q, want running", freshAfter.Status)
	}
	terminalAfter, _ := s.GetRunByAgentID(ctx, "a_terminal")
	if terminalAfter.Status != store.RunCompleted {
		t.Errorf("terminal row was clobbered: status=%q, want completed", terminalAfter.Status)
	}

	// Idempotent: a second sweep with the same cutoff is a no-op (the
	// runs are no longer status='running').
	swept2, err := s.SweepStaleRuns(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if swept2 != 0 {
		t.Errorf("second sweep returned %d, want 0", swept2)
	}
}

// Transcript ordering across two runs in the same session — events from
// run B must come after run A's events, regardless of how the adapter
// generates seq values.
func testTranscriptOrderedAcrossRuns(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	runA, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err := s.AppendEvent(ctx, runA.ID, "text", []byte(`{"i":0}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, runA.ID, "text", []byte(`{"i":1}`)); err != nil {
		t.Fatal(err)
	}
	_ = s.FinishRun(ctx, runA.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	runB, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err := s.AppendEvent(ctx, runB.ID, "text", []byte(`{"i":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, runB.ID, "text", []byte(`{"i":3}`)); err != nil {
		t.Fatal(err)
	}

	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 4 {
		t.Fatalf("len = %d, want 4", len(transcript))
	}
	for i := 1; i < len(transcript); i++ {
		if transcript[i].Seq <= transcript[i-1].Seq {
			t.Errorf("seq not ascending at %d: %d -> %d", i, transcript[i-1].Seq, transcript[i].Seq)
		}
	}
	// First two events belong to run A, second two to run B.
	if transcript[0].RunID != runA.ID || transcript[1].RunID != runA.ID {
		t.Errorf("first two events should belong to runA")
	}
	if transcript[2].RunID != runB.ID || transcript[3].RunID != runB.ID {
		t.Errorf("last two events should belong to runB")
	}
}

// ----- Memory contract -----
//
// These tests exercise the v0.8.0 Memory tool's storage surface.
// Both backends implement identical semantics, so the suite runs
// unchanged against SQLite (TEXT-as-JSON column) and Postgres (JSONB).

func testMemorySetGetRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	value := json.RawMessage(`{"style":"concise","tone":"friendly"}`)
	if err := s.MemorySet(ctx, store.MemoryScopeUser, "alice", "voice", value, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryGet(ctx, store.MemoryScopeUser, "alice", "voice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "voice" {
		t.Errorf("key = %q, want voice", got.Key)
	}
	// Compare by parsed shape — JSONB may reorder keys / drop whitespace.
	var a, b map[string]any
	if err := json.Unmarshal(value, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got.Value, &b); err != nil {
		t.Fatalf("stored value not valid JSON: %v", err)
	}
	if a["style"] != b["style"] || a["tone"] != b["tone"] {
		t.Errorf("value round-trip diverged: stored=%v got=%v", a, b)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("created_at / updated_at must be set on a fresh write")
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("expires_at should be zero for ttl=0; got %v", got.ExpiresAt)
	}
}

func testMemoryGetNotFound(t *testing.T, s store.Store) {
	_, err := s.MemoryGet(context.Background(), store.MemoryScopeAgent, "qa-agent", "missing")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
}

func testMemoryOverwriteUpdatesValue(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "summary", json.RawMessage(`"v1"`), 0); err != nil {
		t.Fatal(err)
	}
	first, _ := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "summary")
	time.Sleep(2 * time.Millisecond)
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "summary", json.RawMessage(`"v2"`), 0); err != nil {
		t.Fatal(err)
	}
	second, _ := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "summary")
	if string(second.Value) == string(first.Value) {
		t.Error("overwrite did not change the stored value")
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at not advanced: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
}

func testMemoryDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "u1", "k", json.RawMessage(`1`), 0)

	deleted, err := s.MemoryDelete(ctx, store.MemoryScopeUser, "u1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected deleted=true on a present key")
	}
	deleted, err = s.MemoryDelete(ctx, store.MemoryScopeUser, "u1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected deleted=false on a missing key")
	}
}

func testMemoryListPrefix(t *testing.T, s store.Store) {
	ctx := context.Background()
	for k, v := range map[string]string{
		"events/2026-05-09T10:00": `"a"`,
		"events/2026-05-09T11:00": `"b"`,
		"events/2026-05-10T09:00": `"c"`,
		"prefs/voice":             `"d"`,
		"prefs/timezone":          `"e"`,
	} {
		if err := s.MemorySet(ctx, store.MemoryScopeUser, "alice", k, json.RawMessage(v), 0); err != nil {
			t.Fatal(err)
		}
	}
	got, truncated, err := s.MemoryList(ctx, store.MemoryScopeUser, "alice", "events/", 50)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Errorf("truncated unexpectedly")
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3 (events/* only)", len(got))
	}
	for _, e := range got {
		if len(e.Key) < len("events/") || e.Key[:len("events/")] != "events/" {
			t.Errorf("non-prefix-matching key in result: %q", e.Key)
		}
	}
	// Empty prefix returns everything in the scope.
	all, _, err := s.MemoryList(ctx, store.MemoryScopeUser, "alice", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("empty prefix len = %d, want 5", len(all))
	}
}

func testMemoryListTruncation(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "key/"+intToKey(i), json.RawMessage(`1`), 0)
	}
	got, truncated, err := s.MemoryList(ctx, store.MemoryScopeAgent, "qa", "key/", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("truncated should be true when more rows exist than limit")
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func testMemoryTTLExpiry(t *testing.T, s store.Store) {
	ctx := context.Background()
	// 50 ms TTL — short enough for a test, long enough to survive the
	// initial Get on a slow CI runner.
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "warning", json.RawMessage(`"hi"`), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "warning")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("expires_at should be set when ttl > 0")
	}

	time.Sleep(100 * time.Millisecond)

	_, err = s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "warning")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("expired key should return ErrNotFound, got %v", err)
	}

	// MemoryList must filter expired entries even before the sweeper
	// runs.
	listed, _, err := s.MemoryList(ctx, store.MemoryScopeAgent, "qa", "warning", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("MemoryList returned expired row(s): %d", len(listed))
	}
}

func testMemorySweepReapsExpired(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "u", "transient", json.RawMessage(`1`), 30*time.Millisecond)
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "u", "permanent", json.RawMessage(`1`), 0)

	time.Sleep(60 * time.Millisecond)
	deleted, err := s.MemorySweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted < 1 {
		t.Errorf("MemorySweep reaped %d, want >=1", deleted)
	}
	// Idempotent: second sweep right after is a no-op.
	deleted2, err := s.MemorySweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted2 != 0 {
		t.Errorf("second sweep reaped %d, want 0", deleted2)
	}
	// Permanent row must survive.
	if _, err := s.MemoryGet(ctx, store.MemoryScopeUser, "u", "permanent"); err != nil {
		t.Errorf("permanent row was reaped: %v", err)
	}
}

func testMemoryIncrementOnNewKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.MemoryIncrement(ctx, store.MemoryScopeAgent, "qa", "warnings", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("incr on new key = %d, want 1", got)
	}
	got, err = s.MemoryIncrement(ctx, store.MemoryScopeAgent, "qa", "warnings", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 6 {
		t.Errorf("incr by 5 on existing 1 = %d, want 6", got)
	}
}

func testMemoryIncrementOnExistingNumber(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "n", json.RawMessage(`42`), 0)
	got, err := s.MemoryIncrement(ctx, store.MemoryScopeAgent, "qa", "n", -10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 32 {
		t.Errorf("incr by -10 on 42 = %d, want 32", got)
	}
}

func testMemoryIncrementOnNonNumberFails(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "obj", json.RawMessage(`{"hello":"world"}`), 0)
	_, err := s.MemoryIncrement(ctx, store.MemoryScopeAgent, "qa", "obj", 1, 0)
	if !errors.Is(err, store.ErrMemoryWrongType) {
		t.Errorf("got %v, want ErrMemoryWrongType", err)
	}
}

func testMemoryIncrementOnExpiredKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "k", json.RawMessage(`100`), 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	got, err := s.MemoryIncrement(ctx, store.MemoryScopeAgent, "qa", "k", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("incr on expired key should restart from 0; got %d, want 1", got)
	}
}

func testMemoryScopeIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "alice", "secret", json.RawMessage(`"alice-secret"`), 0)
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "bob", "secret", json.RawMessage(`"bob-secret"`), 0)
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "secret", json.RawMessage(`"qa-secret"`), 0)

	a, err := s.MemoryGet(ctx, store.MemoryScopeUser, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.MemoryGet(ctx, store.MemoryScopeUser, "bob", "secret")
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Value) == string(b.Value) || string(a.Value) == string(q.Value) {
		t.Errorf("scope isolation broken: alice=%s bob=%s qa=%s", a.Value, b.Value, q.Value)
	}

	// Listing one (scope, scopeID) must not surface another's keys.
	list, _, _ := s.MemoryList(ctx, store.MemoryScopeUser, "alice", "", 100)
	if len(list) != 1 {
		t.Errorf("alice-scope list returned %d rows, want 1", len(list))
	}
}

func testMemoryListScopeIDs(t *testing.T, s store.Store) {
	ctx := context.Background()
	// alice: 2 keys; bob: 1 key; qa-agent: 1 key.
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "alice", "voice", json.RawMessage(`"a1"`), 0)
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "alice", "tone", json.RawMessage(`"a2"`), 0)
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "bob", "voice", json.RawMessage(`"b1"`), 0)
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa-agent", "warnings", json.RawMessage(`5`), 0)

	users, err := s.MemoryListScopeIDs(ctx, store.MemoryScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, u := range users {
		got[u.ScopeID] = u.KeyCount
		if u.UpdatedAt.IsZero() {
			t.Errorf("%s.UpdatedAt is zero", u.ScopeID)
		}
		if u.Bytes <= 0 {
			t.Errorf("%s.Bytes = %d, want > 0", u.ScopeID, u.Bytes)
		}
	}
	if got["alice"] != 2 || got["bob"] != 1 {
		t.Errorf("user-scope summary: %v, want alice=2 bob=1", got)
	}
	if _, ok := got["qa-agent"]; ok {
		t.Errorf("user-scope listing should not include agent-scope rows: %v", got)
	}

	agents, err := s.MemoryListScopeIDs(ctx, store.MemoryScopeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ScopeID != "qa-agent" {
		t.Errorf("agent-scope summary: %+v", agents)
	}

	// Expired rows must not surface in the summary.
	_ = s.MemorySet(ctx, store.MemoryScopeUser, "transient", "k", json.RawMessage(`1`), 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	users2, _ := s.MemoryListScopeIDs(ctx, store.MemoryScopeUser)
	for _, u := range users2 {
		if u.ScopeID == "transient" {
			t.Errorf("expired-only scope_id %q should not appear", u.ScopeID)
		}
	}
}

// testMemoryIncrementIsAtomicUnderConcurrency is a regression test
// for two adapter-level bugs caught in v0.8.0 review:
//
//   - SQLite's BeginTx(nil) gives a DEFERRED transaction, not the
//     IMMEDIATE the increment loop assumes. Two concurrent increments
//     on the same key both see the old value and both write the same
//     "next" value, losing one update.
//   - Postgres's SELECT FOR UPDATE on a non-existent row does NOT
//     block the second transaction (there's no row to lock). Both
//     transactions see ErrNoRows, both INSERT (one wins outright,
//     the second's ON CONFLICT DO UPDATE overwrites the first's
//     value with delta — losing the first's contribution).
//
// 100 concurrent +1 increments must produce exactly 100. A failing
// adapter shows a final value < 100 (number of lost updates).
func testMemoryIncrementIsAtomicUnderConcurrency(t *testing.T, s store.Store) {
	ctx := context.Background()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = s.MemoryIncrement(ctx, store.MemoryScopeAgent, "qa", "counter", 1, 0)
		}()
	}
	wg.Wait()

	got, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "counter")
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(N)
	if string(got.Value) != want {
		t.Errorf("concurrent increments: counter = %s, want %s (%d lost updates)",
			got.Value, want, N-mustParseInt(string(got.Value)))
	}
}

func mustParseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// intToKey is a tiny helper for testMemoryListTruncation — keeps the
// keys lex-sortable so the prefix LIKE behaves predictably.
func intToKey(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}
	return string(digits[i/10]) + string(digits[i%10])
}

// ---- v0.8.4 Channel tool contract ----
//
// These tests run unchanged against both backends. They pin the
// invariants the tool layer relies on: ordering by publish time,
// cursor monotonicity, TTL filtering at read, lossy-on-overflow
// trim, scope isolation, replay via cur_0.

func testChannelPublishSubscribeRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "findings", Scope: store.MemoryScopeAgent, ScopeID: "researcher",
			Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
		}, 0)
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		// MintChannelMessageID derives the prefix from t.UnixNano().
		// Sleep one nanosecond between writes so ids stay strictly
		// monotonic even on platforms with coarse time resolution.
		time.Sleep(time.Microsecond)
	}
	msgs, next, err := s.ChannelSubscribe(ctx, "findings", store.MemoryScopeAgent, "researcher", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if next == "" {
		t.Error("nextCursor empty after non-empty batch")
	}
	if next != msgs[len(msgs)-1].ID {
		t.Errorf("nextCursor = %q, want last message id %q", next, msgs[len(msgs)-1].ID)
	}
	// Order is publish-time ascending.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].ID <= msgs[i-1].ID {
			t.Errorf("ids not strictly increasing at i=%d: %q vs %q", i, msgs[i-1].ID, msgs[i].ID)
		}
	}
}

func testChannelSubscribeEmptyChannel(t *testing.T, s store.Store) {
	ctx := context.Background()
	msgs, next, err := s.ChannelSubscribe(ctx, "unused", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 || next != "" {
		t.Errorf("empty channel: got msgs=%d next=%q, want 0/\"\"", len(msgs), next)
	}
}

func testChannelCursorAdvancesAcrossSubscribes(t *testing.T, s store.Store) {
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		id, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
			Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(time.Microsecond)
	}
	// First page of 2.
	msgs, next, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || next != ids[1] {
		t.Fatalf("page1: msgs=%d next=%q (want 2/%s)", len(msgs), next, ids[1])
	}
	// Commit + read next page.
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", next); err != nil {
		t.Fatal(err)
	}
	committed, err := s.ChannelCommittedCursor(ctx, "ch", store.MemoryScopeAgent, "x")
	if err != nil {
		t.Fatal(err)
	}
	if committed != ids[1] {
		t.Errorf("committed = %q, want %q", committed, ids[1])
	}
	// Read from committed cursor.
	msgs, next, err = s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", committed, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Errorf("page2: got %d msgs, want 3", len(msgs))
	}
	if next != ids[4] {
		t.Errorf("page2 next = %q, want %q", next, ids[4])
	}
}

func testChannelAckIsIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	id, _, _ := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", id); err != nil {
		t.Fatal(err)
	}
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", id); err != nil {
		t.Errorf("second ack of same cursor: %v (want nil — idempotent)", err)
	}
}

func testChannelAckRejectsCursorRegression(t *testing.T, s store.Store) {
	ctx := context.Background()
	id1, _, _ := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	time.Sleep(time.Microsecond)
	id2, _, _ := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", id2); err != nil {
		t.Fatal(err)
	}
	// Trying to commit id1 (older) must fail with ErrChannelCursorRegression.
	err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", id1)
	if !errors.Is(err, store.ErrChannelCursorRegression) {
		t.Errorf("got %v, want ErrChannelCursorRegression", err)
	}
}

func testChannelTTLFilteredAtRead(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	// Publish a row with an already-expired TTL by setting ExpiresAt
	// in the past. The store should not return it on subscribe, even
	// without invoking the sweeper.
	_, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`{"stale":true}`),
		ExpiresAt: now.Add(-time.Hour),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// And one live row.
	_, _, err = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`{"live":true}`),
		ExpiresAt: now.Add(time.Hour),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs, want 1 (the expired one must be filtered at read)", len(msgs))
	}
	if string(msgs[0].Payload) != `{"live":true}` {
		t.Errorf("got payload %s; want only the live row", msgs[0].Payload)
	}
}

func testChannelSweepReapsExpired(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
			Payload:   json.RawMessage(`{}`),
			ExpiresAt: time.Now().Add(-time.Hour),
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ChannelSweepExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("sweep returned %d, want 3", n)
	}
	// A second sweep should be a no-op.
	if n2, _ := s.ChannelSweepExpired(ctx); n2 != 0 {
		t.Errorf("second sweep returned %d, want 0", n2)
	}
}

func testChannelMaxMessagesTrimsOldest(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Cap of 3, publish 5 — the 2 oldest get trimmed. Each publish
	// after the 3rd should report dropped >= 1.
	var droppedTotal int
	for i := 0; i < 5; i++ {
		_, dropped, err := s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
			Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
		}, 3)
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		droppedTotal += dropped
		time.Sleep(time.Microsecond)
	}
	if droppedTotal != 2 {
		t.Errorf("total trimmed = %d, want 2", droppedTotal)
	}
	msgs, _, _ := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if len(msgs) != 3 {
		t.Errorf("post-trim msgs = %d, want 3 (the cap)", len(msgs))
	}
}

func testChannelScopeIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Same channel name, different (scope, scope_id). Each subscriber
	// sees only its own slice.
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "shared", Scope: store.MemoryScopeAgent, ScopeID: "agent-a",
		Payload: json.RawMessage(`{"from":"a"}`),
	}, 0)
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "shared", Scope: store.MemoryScopeAgent, ScopeID: "agent-b",
		Payload: json.RawMessage(`{"from":"b"}`),
	}, 0)
	msgs, _, _ := s.ChannelSubscribe(ctx, "shared", store.MemoryScopeAgent, "agent-a", "", 10)
	if len(msgs) != 1 || string(msgs[0].Payload) != `{"from":"a"}` {
		t.Errorf("agent-a sees: %+v, want only its own message", msgs)
	}
	msgs, _, _ = s.ChannelSubscribe(ctx, "shared", store.MemoryScopeAgent, "agent-b", "", 10)
	if len(msgs) != 1 || string(msgs[0].Payload) != `{"from":"b"}` {
		t.Errorf("agent-b sees: %+v, want only its own message", msgs)
	}
}

func testChannelPeekDoesNotConsume(t *testing.T, s store.Store) {
	ctx := context.Background()
	id, _, _ := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	// Peek does not modify the committed cursor.
	if msgs, err := s.ChannelPeek(ctx, "ch", store.MemoryScopeAgent, "x", "", 10); err != nil || len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("peek: msgs=%+v err=%v want one msg with id %q", msgs, err, id)
	}
	committed, err := s.ChannelCommittedCursor(ctx, "ch", store.MemoryScopeAgent, "x")
	if err != nil {
		t.Fatal(err)
	}
	if committed != "" {
		t.Errorf("peek advanced committed cursor to %q (must stay empty)", committed)
	}
}

func testChannelReplayFromCursorZero(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
			Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
		}, 0)
		time.Sleep(time.Microsecond)
	}
	// Drain + commit.
	msgs, next, _ := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if len(msgs) != 3 {
		t.Fatalf("drain: got %d, want 3", len(msgs))
	}
	_ = s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", next)
	// from_cursor=cur_0 replays everything regardless of committed.
	msgs, _, _ = s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "cur_0", 10)
	if len(msgs) != 3 {
		t.Errorf("replay: got %d, want 3 (cur_0 must ignore committed cursor)", len(msgs))
	}
}
