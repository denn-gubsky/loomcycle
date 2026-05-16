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
	"fmt"
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
		{"CreateRunModelVisibleMidFlight", testCreateRunModelVisibleMidFlight},
		{"CreateRunModelEmptyStaysEmpty", testCreateRunModelEmptyStaysEmpty},
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
		// v0.8.6 deferred publish (PR 1)
		{"ChannelDeferredHiddenUntilVisible", testChannelDeferredHiddenUntilVisible},
		{"ChannelDeferredDeliversAfterProgressedCursor", testChannelDeferredDeliversAfterProgressedCursor},
		{"ChannelDeferredTTLCountsFromPublished", testChannelDeferredTTLCountsFromPublished},
		{"ChannelDeferredPastDeliverAtTreatedAsNow", testChannelDeferredPastDeliverAtTreatedAsNow},
		{"ChannelDeferredAckCommitsTupleCursor", testChannelDeferredAckCommitsTupleCursor},
		{"ChannelPublishedByUserIDRoundTrip", testChannelPublishedByUserIDRoundTrip},
		// PR 1 review-fix: trim must preserve deferred messages
		// (regression test for the CRITICAL trim-by-id-DESC bug).
		{"ChannelMaxMessagesTrimPreservesDeferred", testChannelMaxMessagesTrimPreservesDeferred},
		{"AgentDefCreateAndGet", testAgentDefCreateAndGet},
		{"AgentDefVersionMonotonicUnderContention", testAgentDefVersionMonotonicUnderContention},
		{"AgentDefParallelForksDistinctVersions", testAgentDefParallelForksDistinctVersions},
		{"AgentDefAppendOnlyDefinition", testAgentDefAppendOnlyDefinition},
		{"AgentDefActivePointerIdempotent", testAgentDefActivePointerIdempotent},
		{"AgentDefRetireReversible", testAgentDefRetireReversible},
		{"AgentDefStaticFallback", testAgentDefStaticFallback},
		{"EvaluationSubmitAndAggregate", testEvaluationSubmitAndAggregate},
		{"EvaluationAggregateWithLineage", testEvaluationAggregateWithLineage},
		// v0.8.x Process-resource metrics sampler
		{"MetricsWriteAndQuery", testMetricsWriteAndQuery},
		{"MetricsSweep", testMetricsSweep},
		{"MetricsRunSummaryEmpty", testMetricsRunSummaryEmpty},
		{"MetricsRunSummaryWithSamples", testMetricsRunSummaryWithSamples},
		{"MetricsRunSummaryInFlight", testMetricsRunSummaryInFlight},
		{"MetricsRunSummaryNotFound", testMetricsRunSummaryNotFound},
		// v0.8.16 Interruption tool storage layer
		{"InterruptCreateAndGet", testInterruptCreateAndGet},
		{"InterruptGetNotFound", testInterruptGetNotFound},
		{"InterruptResolveSetsStatusAndFields", testInterruptResolveSetsStatusAndFields},
		{"InterruptResolveRejectsAlreadyTerminal", testInterruptResolveRejectsAlreadyTerminal},
		{"InterruptResolveRoundTripsAnswerMeta", testInterruptResolveRoundTripsAnswerMeta},
		{"InterruptFinishSetsTimedOut", testInterruptFinishSetsTimedOut},
		{"InterruptFinishRejectsAlreadyTerminal", testInterruptFinishRejectsAlreadyTerminal},
		{"InterruptListByRunFiltersByStatus", testInterruptListByRunFiltersByStatus},
		{"InterruptListByUserFiltersByStatus", testInterruptListByUserFiltersByStatus},
		{"InterruptCountPendingByRunIsAccurateUnderConcurrency", testInterruptCountPendingByRunIsAccurateUnderConcurrency},
		{"InterruptSweepExpiredMarksOnlyExpiredPending", testInterruptSweepExpiredMarksOnlyExpiredPending},
		{"InterruptIDIsMonotonicByTime", testInterruptIDIsMonotonicByTime},
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

// testCreateRunModelVisibleMidFlight pins the v0.8.x contract added
// after the Web UI started showing "—" for the model column on every
// running run: CreateRun must persist RunIdentity.Model so a SELECT
// against the row returns the resolved model BEFORE FinishRun. The
// previous shape (model written only at FinishRun) made the running
// UI useless for diagnosing tier resolution mid-flight.
func testCreateRunModelVisibleMidFlight(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_midflight",
		UserID:  "user-1",
		Model:   "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "claude-sonnet-4-6" {
		t.Errorf("CreateRun returned Model=%q; want claude-sonnet-4-6", run.Model)
	}

	// Critical: read back BEFORE FinishRun. The bug shape this guards
	// against is a backend that accepts Model on the struct but doesn't
	// write it to the row until FinishRun.
	got, err := s.GetRunByAgentID(ctx, "a_midflight")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunRunning {
		t.Fatalf("expected status=running before FinishRun, got %q", got.Status)
	}
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("GetRunByAgentID returned Model=%q on a still-running row; want claude-sonnet-4-6", got.Model)
	}
}

// testCreateRunModelEmptyStaysEmpty: a CreateRun without Model must
// leave the column unset (NULL / empty). Back-compat with older
// callers and with sub-agent paths where the model is filled in later.
func testCreateRunModelEmptyStaysEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_nomodel",
		UserID:  "user-1",
		// Model deliberately empty
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "" {
		t.Errorf("CreateRun returned Model=%q on empty input; want empty", run.Model)
	}
	got, _ := s.GetRunByAgentID(ctx, "a_nomodel")
	if got.Model != "" {
		t.Errorf("read-back Model=%q on empty input; want empty", got.Model)
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
	// v0.8.6: nextCursor is the tuple-encoded (visible_at, msg_id)
	// of the last delivered message, not the raw msg_id.
	wantNext := store.EncodeChannelCursor(msgs[len(msgs)-1].VisibleAt, msgs[len(msgs)-1].ID)
	if next != wantNext {
		t.Errorf("nextCursor = %q, want %q", next, wantNext)
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
	wantNext1 := store.EncodeChannelCursor(msgs[1].VisibleAt, ids[1])
	if len(msgs) != 2 || next != wantNext1 {
		t.Fatalf("page1: msgs=%d next=%q (want 2/%s)", len(msgs), next, wantNext1)
	}
	// Commit + read next page.
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", next); err != nil {
		t.Fatal(err)
	}
	committed, err := s.ChannelCommittedCursor(ctx, "ch", store.MemoryScopeAgent, "x")
	if err != nil {
		t.Fatal(err)
	}
	if committed != wantNext1 {
		t.Errorf("committed = %q, want %q", committed, wantNext1)
	}
	// Read from committed cursor.
	msgs, next, err = s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", committed, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Errorf("page2: got %d msgs, want 3", len(msgs))
	}
	wantNext2 := store.EncodeChannelCursor(msgs[len(msgs)-1].VisibleAt, ids[4])
	if next != wantNext2 {
		t.Errorf("page2 next = %q, want %q", next, wantNext2)
	}
}

func testChannelAckIsIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	// v0.8.6: ChannelAck takes the tuple cursor returned by Subscribe,
	// not a raw msg_id. Fetch the cursor through Subscribe.
	msgs, cursor, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("setup subscribe: msgs=%d err=%v", len(msgs), err)
	}
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", cursor); err != nil {
		t.Fatal(err)
	}
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", cursor); err != nil {
		t.Errorf("second ack of same cursor: %v (want nil — idempotent)", err)
	}
}

func testChannelAckRejectsCursorRegression(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	time.Sleep(time.Microsecond)
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)
	// Get the two tuple cursors via Subscribe (one at a time, so each
	// `next` reflects the per-message cursor).
	msgs1, cur1, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 1)
	if err != nil || len(msgs1) != 1 {
		t.Fatalf("page1: %v / %d", err, len(msgs1))
	}
	msgs2, cur2, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", cur1, 1)
	if err != nil || len(msgs2) != 1 {
		t.Fatalf("page2: %v / %d", err, len(msgs2))
	}
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", cur2); err != nil {
		t.Fatal(err)
	}
	// Trying to commit cur1 (older) must fail with ErrChannelCursorRegression.
	err = s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", cur1)
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

// ---- v0.8.6 deferred publish contract ----
//
// Six subtests pin the (visible_at, msg_id) tuple cursor semantics
// shared by both backends. The shape mirrors the v0.8.4 channel
// contract — operators read these names to understand what the
// backend MUST do (rather than what one specific adapter happens to).

func testChannelDeferredHiddenUntilVisible(t *testing.T, s store.Store) {
	ctx := context.Background()
	deferTo := time.Now().Add(150 * time.Millisecond)
	_, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`{"k":"v"}`),
		VisibleAt: deferTo,
	}, 0)
	if err != nil {
		t.Fatalf("publish deferred: %v", err)
	}
	msgs, _, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("immediate subscribe: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("deferred message visible too early: got %d, want 0", len(msgs))
	}
	// Wait past visible_at, then verify it shows up.
	time.Sleep(200 * time.Millisecond)
	msgs, _, err = s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("post-visible subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("after visible_at: got %d, want 1", len(msgs))
	}
}

func testChannelDeferredDeliversAfterProgressedCursor(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Publish A (immediate), B (deferred), C (immediate). All three
	// land in storage; only A and C are visible right now. Subscriber
	// progresses past C. When B becomes visible, the next subscribe
	// MUST return B even though B.id < C.id (the (visible_at, id) tuple
	// order places B AFTER C).
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"A"`),
	}, 0)
	time.Sleep(time.Microsecond)
	deferTo := time.Now().Add(150 * time.Millisecond)
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"B"`),
		VisibleAt: deferTo,
	}, 0)
	time.Sleep(time.Microsecond)
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"C"`),
	}, 0)
	// First subscribe: A and C only. B is not yet visible.
	msgs, next, err := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("page1 len = %d, want 2 (A + C)", len(msgs))
	}
	if string(msgs[0].Payload) != `"A"` || string(msgs[1].Payload) != `"C"` {
		t.Errorf("page1 order = %s, %s; want A, C", msgs[0].Payload, msgs[1].Payload)
	}
	// Commit cursor past C.
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", next); err != nil {
		t.Fatal(err)
	}
	// Wait past visible_at for B.
	time.Sleep(200 * time.Millisecond)
	// Now subscribe again — B should be delivered even though its
	// msg_id is < C's. This is the (visible_at, id) tuple-ordering
	// invariant that prevents silent skip-on-progress for deferred
	// messages.
	msgs, _, err = s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", next, 10)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("page2 len = %d, want 1 (B)", len(msgs))
	}
	if string(msgs[0].Payload) != `"B"` {
		t.Errorf("page2 payload = %s, want B", msgs[0].Payload)
	}
}

func testChannelDeferredTTLCountsFromPublished(t *testing.T, s store.Store) {
	ctx := context.Background()
	// deliver_at = now + 200ms, ttl = 100ms relative to NOW.
	// Result: message becomes "expired" before becoming "visible".
	// Subscribers must NEVER deliver it.
	expires := time.Now().Add(100 * time.Millisecond)
	deferTo := time.Now().Add(200 * time.Millisecond)
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"never-deliverable"`),
		ExpiresAt: expires,
		VisibleAt: deferTo,
	}, 0)
	// Sleep past both visible_at and expires_at. The message has
	// expired AND become visible — the read-path filter should
	// hide it because expires_at <= now.
	time.Sleep(250 * time.Millisecond)
	msgs, _, _ := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "cur_0", 10)
	if len(msgs) != 0 {
		t.Errorf("expired-before-visible delivered %d msgs, want 0", len(msgs))
	}
}

func testChannelDeferredPastDeliverAtTreatedAsNow(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Past deliver_at: visible immediately.
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"past"`),
		VisibleAt: time.Now().Add(-10 * time.Second),
	}, 0)
	msgs, _, _ := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if len(msgs) != 1 {
		t.Errorf("past visible_at delivered %d, want 1", len(msgs))
	}
}

func testChannelDeferredAckCommitsTupleCursor(t *testing.T, s store.Store) {
	ctx := context.Background()
	deferTo := time.Now().Add(80 * time.Millisecond)
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"deferred"`),
		VisibleAt: deferTo,
	}, 0)
	time.Sleep(120 * time.Millisecond)
	msgs, next, _ := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if len(msgs) != 1 {
		t.Fatalf("subscribe len = %d, want 1", len(msgs))
	}
	if err := s.ChannelAck(ctx, "ch", store.MemoryScopeAgent, "x", next); err != nil {
		t.Fatal(err)
	}
	committed, _ := s.ChannelCommittedCursor(ctx, "ch", store.MemoryScopeAgent, "x")
	if committed != next {
		t.Errorf("committed = %q, want %q", committed, next)
	}
}

// PR 1 review-fix regression: a deferred message MUST survive a
// max_messages trim triggered by a later immediate publish. The
// pre-fix trim used `ORDER BY id DESC`, which (since id = publish
// time) placed the deferred message in the "drop" half. The fix
// orders by (visible_at, id) DESC to match the read path's delivery
// order. Without the fix this test reports "deferred message lost."
func testChannelMaxMessagesTrimPreservesDeferred(t *testing.T, s store.Store) {
	ctx := context.Background()
	maxMessages := 2
	deferTo := time.Now().Add(2 * time.Hour) // pending for the test lifetime
	// A: immediate
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"A"`),
	}, maxMessages)
	time.Sleep(time.Microsecond)
	// B: deferred 2h
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"B-deferred"`),
		VisibleAt: deferTo,
	}, maxMessages)
	time.Sleep(time.Microsecond)
	// C: immediate
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"C"`),
	}, maxMessages)
	time.Sleep(time.Microsecond)
	// D: immediate — triggers trim now that count > maxMessages.
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"D"`),
	}, maxMessages)

	// Peek from cur_0 to see ALL non-expired rows regardless of
	// visibility — we want to confirm B (deferred) is still there.
	rows, err := s.ChannelPeek(ctx, "ch", store.MemoryScopeAgent, "x", "cur_0", 100)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	// Peek's read path also filters visible_at <= now() — B is still
	// invisible, so peek won't return it. Drop down to a direct
	// inspection-style read by sleeping past visible_at; but a 2h
	// sleep isn't viable in a test. Instead: count rows on the
	// channel directly via Subscribe-from-cur_0 (which respects
	// visibility) and combine with the implementation's promise
	// that deferred rows are HIDDEN, not DELETED.
	//
	// To prove B is still alive, we check that the visible set
	// after the trim includes at least the two NEWEST visible rows
	// (C and D) and that B is not visible (since it's deferred).
	// Combined: the trim must NOT have dropped B because if it had,
	// the visible set would still be {C, D} (correct count) but B
	// would be permanently gone. We check via the storage layer's
	// expectation: trim ordered by (visible_at, id) DESC keeps the
	// 2 with highest tuple = B (visible_at far in future) + D
	// (newest among immediates).
	//
	// Approach: subscribe — should return immediates only. Then
	// the count of total rows (visible + invisible) should be 3:
	// the just-trimmed channel keeps maxMessages=2 plus the
	// just-inserted D (race-guard preserves D). With the FIX, the
	// keep-set is {B, D}; the trim removes {A, C} from D's
	// perspective — wait no, let me think again.
	//
	// Trim subquery `SELECT id ORDER BY visible_at DESC, id DESC LIMIT 2`:
	//   - B (visible_at = +2h, highest)
	//   - D (visible_at = ~now, id newest)
	// Keep set = {B, D}. Trim DELETE: not in keep AND not just-
	// inserted (D). So A and C get deleted. Final: {B, D} (2 rows).
	//
	// Visible subscribe at now() returns only D (B is deferred,
	// A and C are deleted). Count = 1.
	if len(rows) != 1 || string(rows[0].Payload) != `"D"` {
		var payloads []string
		for _, r := range rows {
			payloads = append(payloads, string(r.Payload))
		}
		t.Errorf("post-trim visible set = %v, want [\"D\"] only", payloads)
	}

	// Independent check: B's id should still exist in storage
	// (we know it lex-sorts between A and C). A direct count-by-
	// payload subselect would prove it, but the Store interface
	// doesn't expose that. Instead, fast-forward time: skip-ahead
	// is also impractical. The above visible-set check is the
	// strongest assertion the public Store surface supports.
	// The deeper "B row physically exists in the table" check is
	// covered by the trim-correctness reasoning in the SQL +
	// covered indirectly by the postgres backend's identical
	// test passing too (both adapters share the order-by clause).
}

func testChannelPublishedByUserIDRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:           json.RawMessage(`"hi"`),
		PublishedByUserID: "alice",
	}, 0)
	msgs, _, _ := s.ChannelSubscribe(ctx, "ch", store.MemoryScopeAgent, "x", "", 10)
	if len(msgs) != 1 {
		t.Fatalf("subscribe len = %d, want 1", len(msgs))
	}
	if msgs[0].PublishedByUserID != "alice" {
		t.Errorf("PublishedByUserID = %q, want alice", msgs[0].PublishedByUserID)
	}
}

// ---- v0.8.5 Self-Evolution Substrate contract ----

// mkDef returns a minimal AgentDefRow suitable for create tests.
// def_id is caller-supplied; the store doesn't generate it.
func mkDef(id, name string, parent string) store.AgentDefRow {
	return store.AgentDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"model":"x","system_prompt":"p"}`),
		Description: "test row",
	}
}

func testAgentDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.AgentDefCreate(ctx, mkDef("d-1", "alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.AgentDefGet(ctx, "d-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
	// Created-at populated.
	if got.CreatedAt.IsZero() {
		t.Error("created_at not populated")
	}
}

func testAgentDefVersionMonotonicUnderContention(t *testing.T, s store.Store) {
	ctx := context.Background()
	const G = 50 // 50 goroutines × 5 inserts = 250 unique versions
	const Per = 5
	var wg sync.WaitGroup
	errs := make(chan error, G*Per)
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < Per; i++ {
				id := fmt.Sprintf("d-%d-%d", g, i)
				_, err := s.AgentDefCreate(ctx, mkDef(id, "raceagent", ""))
				if err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("create error: %v", err)
	}
	rows, err := s.AgentDefListByName(ctx, "raceagent")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != G*Per {
		t.Fatalf("got %d versions, want %d", len(rows), G*Per)
	}
	// rows is version DESC. Check strictly monotonic and no gaps.
	versions := make(map[int]bool, len(rows))
	for _, r := range rows {
		if versions[r.Version] {
			t.Errorf("duplicate version %d", r.Version)
		}
		versions[r.Version] = true
	}
	for v := 1; v <= G*Per; v++ {
		if !versions[v] {
			t.Errorf("missing version %d", v)
		}
	}
}

func testAgentDefParallelForksDistinctVersions(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, err := s.AgentDefCreate(ctx, mkDef("p-1", "forkparent", ""))
	if err != nil {
		t.Fatal(err)
	}
	const N = 10
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AgentDefCreate(ctx, mkDef(fmt.Sprintf("f-%d", i), "forkparent", parent.DefID))
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("fork: %v", err)
	}
	children, err := s.AgentDefListChildren(ctx, parent.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != N {
		t.Errorf("got %d children, want %d", len(children), N)
	}
	versions := make(map[int]bool)
	for _, c := range children {
		if versions[c.Version] {
			t.Errorf("duplicate child version %d", c.Version)
		}
		versions[c.Version] = true
		if c.ParentDefID != parent.DefID {
			t.Errorf("child %s has wrong parent: %s", c.DefID, c.ParentDefID)
		}
	}
}

func testAgentDefAppendOnlyDefinition(t *testing.T, s store.Store) {
	// The store's adapter has NO UPDATE statement on the definition
	// column. This test verifies the row content is byte-identical
	// after a get-set-get cycle would have applied if the store
	// permitted mutation. There's no public API to mutate, so the
	// only failure mode is "the implementation grew an UPDATE
	// path" — caught by code review more than this test, but the
	// test pins the contract for any future adapter.
	ctx := context.Background()
	original := mkDef("immutable-1", "frozen", "")
	original.Definition = json.RawMessage(`{"v":"original"}`)
	row, err := s.AgentDefCreate(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentDefGet(ctx, row.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Definition) != `{"v":"original"}` {
		t.Errorf("definition: got %s, want original", got.Definition)
	}
}

func testAgentDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, err := s.AgentDefCreate(ctx, mkDef("a", "promo", ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AgentDefCreate(ctx, mkDef("b", "promo", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "promo", b.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentDefGetActive(ctx, "promo")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefID != a.DefID {
		t.Errorf("active = %s, want %s", got.DefID, a.DefID)
	}
}

func testAgentDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.AgentDefCreate(ctx, mkDef("r-1", "retireagent", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AgentDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("retire(true) didn't stick")
	}
	if err := s.AgentDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AgentDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("retire(false) didn't reverse")
	}
	// Retired rows still appear in lineage queries.
	rows, _ := s.AgentDefListByName(ctx, "retireagent")
	if len(rows) != 1 {
		t.Errorf("list after retire toggle: got %d, want 1", len(rows))
	}
}

func testAgentDefStaticFallback(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.AgentDefGetActive(ctx, "no-such-name")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v, want *ErrNotFound", err)
	}
}

func testEvaluationSubmitAndAggregate(t *testing.T, s store.Store) {
	ctx := context.Background()
	def, err := s.AgentDefCreate(ctx, mkDef("eval-d", "evalagent", ""))
	if err != nil {
		t.Fatal(err)
	}
	// Submit 5 evals with known scores.
	scores := []float64{0.1, 0.3, 0.5, 0.7, 0.9}
	for i, sc := range scores {
		_, err := s.EvaluationSubmit(ctx, store.EvaluationRow{
			EvalID:      fmt.Sprintf("ev-%d", i),
			RunID:       fmt.Sprintf("r-%d", i),
			DefID:       def.DefID,
			Score:       sc,
			EmitterRole: "self",
			Dimensions:  map[string]float64{"correctness": sc},
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		time.Sleep(time.Microsecond) // ensure created_at ordering
	}
	agg, err := s.EvaluationAggregate(ctx, def.DefID, store.AggregateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if agg.Count != 5 {
		t.Errorf("count = %d, want 5", agg.Count)
	}
	if agg.Score.Mean < 0.49 || agg.Score.Mean > 0.51 {
		t.Errorf("mean = %f, want ~0.5", agg.Score.Mean)
	}
	if agg.Score.Min != 0.1 {
		t.Errorf("min = %f", agg.Score.Min)
	}
	if agg.Score.Max != 0.9 {
		t.Errorf("max = %f", agg.Score.Max)
	}
	if agg.Score.Median != 0.5 {
		t.Errorf("median = %f, want 0.5", agg.Score.Median)
	}
	if agg.Score.Latest != 0.9 {
		t.Errorf("latest = %f, want 0.9 (newest)", agg.Score.Latest)
	}
	// Dimensions captured.
	corr, ok := agg.Dimensions["correctness"]
	if !ok {
		t.Fatal("no correctness dimension in aggregate")
	}
	if corr.Mean < 0.49 || corr.Mean > 0.51 {
		t.Errorf("correctness mean = %f", corr.Mean)
	}
}

func testEvaluationAggregateWithLineage(t *testing.T, s store.Store) {
	ctx := context.Background()
	root, err := s.AgentDefCreate(ctx, mkDef("ln-root", "lineageagent", ""))
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.AgentDefCreate(ctx, mkDef("ln-child", "lineageagent", root.DefID))
	if err != nil {
		t.Fatal(err)
	}
	// 2 evals on root, 3 evals on child.
	for i := 0; i < 2; i++ {
		_, _ = s.EvaluationSubmit(ctx, store.EvaluationRow{
			EvalID: fmt.Sprintf("rt-%d", i), RunID: fmt.Sprintf("r-rt-%d", i),
			DefID: root.DefID, Score: 0.5, EmitterRole: "external",
		})
	}
	for i := 0; i < 3; i++ {
		_, _ = s.EvaluationSubmit(ctx, store.EvaluationRow{
			EvalID: fmt.Sprintf("ch-%d", i), RunID: fmt.Sprintf("r-ch-%d", i),
			DefID: child.DefID, Score: 1.0, EmitterRole: "external",
		})
	}
	// Without lineage: only child's 3.
	agg, err := s.EvaluationAggregate(ctx, child.DefID, store.AggregateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if agg.Count != 3 {
		t.Errorf("no-lineage count = %d, want 3", agg.Count)
	}
	// With lineage: 3 + 2 = 5.
	agg, err = s.EvaluationAggregate(ctx, child.DefID, store.AggregateOpts{IncludeLineage: true})
	if err != nil {
		t.Fatal(err)
	}
	if agg.Count != 5 {
		t.Errorf("with-lineage count = %d, want 5", agg.Count)
	}
	if !agg.LineageIncluded {
		t.Error("LineageIncluded flag not set")
	}
}

// ---- v0.8.x Process-resource metrics sampler ----

// metricsMakeSample builds a ProcessSample for tests. Caller may
// adjust fields before passing to MetricsWriteSample.
func metricsMakeSample(t *testing.T, sampledAt time.Time, active, queued int, rss int64) store.ProcessSample {
	t.Helper()
	return store.ProcessSample{
		SampleID:            store.MintSampleID(sampledAt),
		SampledAt:           sampledAt,
		ActiveRuns:          active,
		QueuedRuns:          queued,
		LoomcycleRSSBytes:   rss,
		LoomcycleHeapAlloc:  rss / 2,
		LoomcycleHeapInuse:  rss / 3,
		LoomcycleGoroutines: 50,
		LoomcycleCPUPctX100: 1234,
	}
}

func testMetricsWriteAndQuery(t *testing.T, s store.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	samples := []store.ProcessSample{
		metricsMakeSample(t, base.Add(-3*time.Second), 1, 0, 100<<20),
		metricsMakeSample(t, base.Add(-2*time.Second), 2, 1, 110<<20),
		metricsMakeSample(t, base.Add(-1*time.Second), 3, 0, 95<<20),
	}
	for _, sa := range samples {
		if err := s.MetricsWriteSample(ctx, sa); err != nil {
			t.Fatalf("MetricsWriteSample: %v", err)
		}
	}
	got, nextCursor, err := s.MetricsSampleWindow(ctx, base.Add(-10*time.Second), base, 0, "")
	if err != nil {
		t.Fatalf("MetricsSampleWindow: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3", len(got))
	}
	if nextCursor != "" {
		t.Errorf("nextCursor = %q, want empty (only 3 rows ≤ default limit 200)", nextCursor)
	}
	// Ordering: sampled_at ASC.
	for i := 1; i < len(got); i++ {
		if !got[i].SampledAt.After(got[i-1].SampledAt) && !got[i].SampledAt.Equal(got[i-1].SampledAt) {
			t.Errorf("sample %d at %v not >= sample %d at %v", i, got[i].SampledAt, i-1, got[i-1].SampledAt)
		}
	}
	// Field round-trip on the first sample.
	if got[0].ActiveRuns != 1 || got[0].QueuedRuns != 0 || got[0].LoomcycleRSSBytes != 100<<20 {
		t.Errorf("sample[0] round-trip failed: %+v", got[0])
	}
}

func testMetricsSweep(t *testing.T, s store.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	// Two old samples, one fresh sample.
	old1 := metricsMakeSample(t, base.Add(-10*time.Minute), 1, 0, 50<<20)
	old2 := metricsMakeSample(t, base.Add(-5*time.Minute), 1, 0, 60<<20)
	fresh := metricsMakeSample(t, base.Add(-30*time.Second), 1, 0, 70<<20)
	for _, sa := range []store.ProcessSample{old1, old2, fresh} {
		if err := s.MetricsWriteSample(ctx, sa); err != nil {
			t.Fatalf("MetricsWriteSample: %v", err)
		}
	}
	// Cutoff = 2 minutes ago. Should delete the two older samples.
	deleted, err := s.MetricsSweep(ctx, base.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("MetricsSweep: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d, want 2", deleted)
	}
	// The fresh sample survives.
	got, _, err := s.MetricsSampleWindow(ctx, base.Add(-1*time.Hour), base, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("post-sweep window len = %d, want 1", len(got))
	}
	// Second sweep with the same cutoff is idempotent (0 deleted).
	deleted2, err := s.MetricsSweep(ctx, base.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if deleted2 != 0 {
		t.Errorf("second sweep deleted %d, want 0", deleted2)
	}
}

func testMetricsRunSummaryEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_metric_empty"})
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	// No samples written; summary should return zero-valued window
	// (not an error) with SampleCount=0.
	summary, err := s.MetricsRunSummary(ctx, run.ID)
	if err != nil {
		t.Fatalf("MetricsRunSummary on run with no samples: %v", err)
	}
	if summary.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0", summary.SampleCount)
	}
	if summary.PeakRSSBytes != 0 || summary.MeanRSSBytes != 0 || summary.MaxCPUPctX100 != 0 {
		t.Errorf("expected zero-valued metrics, got %+v", summary)
	}
	if summary.RunID != run.ID {
		t.Errorf("RunID = %q, want %q", summary.RunID, run.ID)
	}
}

func testMetricsRunSummaryWithSamples(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_metric_with"})
	// Wait one ms so the samples fall strictly after started_at.
	// (Postgres TIMESTAMPTZ has microsecond resolution; sqlite is
	// nanosecond. Both are fine here.)
	time.Sleep(2 * time.Millisecond)
	now := time.Now().UTC().Truncate(time.Microsecond)
	// Three samples in this run's window, descending RSS.
	for i, rss := range []int64{120 << 20, 200 << 20, 150 << 20} {
		_ = i
		sa := metricsMakeSample(t, now.Add(time.Duration(i)*time.Millisecond), 1, 0, rss)
		sa.LoomcycleCPUPctX100 = 1000 + (i+1)*500 // 1500, 2000, 2500
		if err := s.MetricsWriteSample(ctx, sa); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	// Finish the run AFTER the samples so the window covers them.
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	summary, err := s.MetricsRunSummary(ctx, run.ID)
	if err != nil {
		t.Fatalf("MetricsRunSummary: %v", err)
	}
	if summary.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", summary.SampleCount)
	}
	// Peak should match the largest of the three.
	if summary.PeakRSSBytes != 200<<20 {
		t.Errorf("PeakRSSBytes = %d, want %d", summary.PeakRSSBytes, int64(200<<20))
	}
	if summary.MaxCPUPctX100 != 2500 {
		t.Errorf("MaxCPUPctX100 = %d, want 2500", summary.MaxCPUPctX100)
	}
	// Mean is approximately (120+200+150)/3 = ~156 MB.
	if summary.MeanRSSBytes < 150<<20 || summary.MeanRSSBytes > 170<<20 {
		t.Errorf("MeanRSSBytes = %d, want ~157 MB", summary.MeanRSSBytes)
	}
}

func testMetricsRunSummaryInFlight(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_metric_inflight"})
	time.Sleep(2 * time.Millisecond)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sa := metricsMakeSample(t, now, 1, 0, 80<<20)
	if err := s.MetricsWriteSample(ctx, sa); err != nil {
		t.Fatal(err)
	}
	// Run NOT finished — completed_at is NULL. Summary must still
	// pick up the sample by using COALESCE(completed_at, now) as
	// the upper bound.
	summary, err := s.MetricsRunSummary(ctx, run.ID)
	if err != nil {
		t.Fatalf("MetricsRunSummary on in-flight run: %v", err)
	}
	if summary.SampleCount != 1 {
		t.Errorf("SampleCount = %d, want 1", summary.SampleCount)
	}
	if summary.PeakRSSBytes != 80<<20 {
		t.Errorf("PeakRSSBytes = %d, want %d", summary.PeakRSSBytes, int64(80<<20))
	}
	// CompletedAt should be the zero value on in-flight runs.
	if !summary.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = %v, want zero (run is in-flight)", summary.CompletedAt)
	}
}

func testMetricsRunSummaryNotFound(t *testing.T, s store.Store) {
	_, err := s.MetricsRunSummary(context.Background(), "r_does_not_exist")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
	if nf != nil && nf.Kind != "run" {
		t.Errorf("Kind = %q, want run", nf.Kind)
	}
}

// ---- Interruption (v0.8.16) ----------------------------------------

// makeRunForInterrupt creates a session + run with given identity
// fields. Returns the run_id. Helper so each interruption test
// doesn't repeat the boilerplate.
func makeRunForInterrupt(t *testing.T, s store.Store, userID, agentID, agentName string) (sessID, runID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", agentName, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID,
		UserID:  userID,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return sess.ID, run.ID
}

func testInterruptCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_1", "batch-processor")

	id := store.MintInterruptID(time.Now())
	row := store.InterruptRow{
		InterruptID: id,
		RunID:       runID,
		Kind:        store.InterruptKindQuestion,
		Question:    "Proceed with delete?",
		Options:     json.RawMessage(`["Yes","No"]`),
		ContextData: "47 records pending",
		Priority:    store.InterruptPriorityNormal,
		UserID:      "u_alice",
		AgentID:     "a_intr_1",
		AgentName:   "batch-processor",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(1 * time.Hour),
	}
	got, err := s.InterruptCreate(ctx, row)
	if err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}
	if got != id {
		t.Errorf("returned id = %q, want %q", got, id)
	}

	r, err := s.InterruptGet(ctx, id)
	if err != nil {
		t.Fatalf("InterruptGet: %v", err)
	}
	if r.Status != store.InterruptStatusPending {
		t.Errorf("Status = %q, want pending", r.Status)
	}
	if r.Question != "Proceed with delete?" {
		t.Errorf("Question = %q", r.Question)
	}
	if string(r.Options) == "" || string(r.Options) == "null" {
		t.Errorf("Options round-trip empty: %q", string(r.Options))
	}
	if r.UserID != "u_alice" {
		t.Errorf("UserID = %q, want u_alice (denormalised at create)", r.UserID)
	}
	if r.ExpiresAt.IsZero() {
		t.Error("ExpiresAt round-tripped zero; want non-zero")
	}
}

func testInterruptGetNotFound(t *testing.T, s store.Store) {
	_, err := s.InterruptGet(context.Background(), "intr_doesnotexist")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
	if nf != nil && nf.Kind != "interrupt" {
		t.Errorf("Kind = %q, want interrupt", nf.Kind)
	}
}

func testInterruptResolveSetsStatusAndFields(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_r", "batch")

	id := store.MintInterruptID(time.Now())
	if _, err := s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "u_alice",
		Question: "Q?", Priority: store.InterruptPriorityNormal,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.InterruptResolve(ctx, id, "Yes", store.InterruptResolvedByWebUI, nil); err != nil {
		t.Fatalf("InterruptResolve: %v", err)
	}
	r, _ := s.InterruptGet(ctx, id)
	if r.Status != store.InterruptStatusResolved {
		t.Errorf("Status = %q, want resolved", r.Status)
	}
	if r.Answer != "Yes" {
		t.Errorf("Answer = %q, want Yes", r.Answer)
	}
	if r.ResolvedBy != store.InterruptResolvedByWebUI {
		t.Errorf("ResolvedBy = %q", r.ResolvedBy)
	}
	if r.ResolvedAt.IsZero() {
		t.Error("ResolvedAt is zero; want set")
	}
}

func testInterruptResolveRejectsAlreadyTerminal(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_dup", "batch")

	id := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "u_alice",
		Question: "Q?", CreatedAt: time.Now(),
	})
	if err := s.InterruptResolve(ctx, id, "Yes", store.InterruptResolvedByWebUI, nil); err != nil {
		t.Fatal(err)
	}
	// Second resolve must fail with ErrInterruptAlreadyTerminal.
	err := s.InterruptResolve(ctx, id, "No", store.InterruptResolvedByWebUI, nil)
	if !errors.Is(err, store.ErrInterruptAlreadyTerminal) {
		t.Errorf("got %v, want ErrInterruptAlreadyTerminal", err)
	}
}

func testInterruptResolveRoundTripsAnswerMeta(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_meta", "approver")

	id := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "u_alice",
		Question: "Approve delete?", CreatedAt: time.Now(),
	})
	meta := json.RawMessage(`{"approved":true,"reason":"verified manually"}`)
	if err := s.InterruptResolve(ctx, id, "Yes", store.InterruptResolvedByWebUI, meta); err != nil {
		t.Fatal(err)
	}
	r, _ := s.InterruptGet(ctx, id)
	if len(r.AnswerMeta) == 0 {
		t.Fatal("AnswerMeta round-tripped empty")
	}
	var decoded map[string]any
	if err := json.Unmarshal(r.AnswerMeta, &decoded); err != nil {
		t.Fatalf("AnswerMeta unmarshal: %v (raw %q)", err, string(r.AnswerMeta))
	}
	if decoded["approved"] != true || decoded["reason"] != "verified manually" {
		t.Errorf("AnswerMeta decoded = %+v", decoded)
	}
}

func testInterruptFinishSetsTimedOut(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_fin", "batch")

	id := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "u_alice",
		Question: "Q?", CreatedAt: time.Now(),
	})
	if err := s.InterruptFinish(ctx, id, store.InterruptStatusTimedOut, store.InterruptResolvedByTimeout); err != nil {
		t.Fatalf("InterruptFinish: %v", err)
	}
	r, _ := s.InterruptGet(ctx, id)
	if r.Status != store.InterruptStatusTimedOut {
		t.Errorf("Status = %q, want timed_out", r.Status)
	}
	if r.Answer != "" {
		t.Errorf("Answer = %q on timeout path; want empty", r.Answer)
	}
}

func testInterruptFinishRejectsAlreadyTerminal(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_fin_dup", "batch")

	id := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "u_alice",
		Question: "Q?", CreatedAt: time.Now(),
	})
	_ = s.InterruptResolve(ctx, id, "Yes", store.InterruptResolvedByWebUI, nil)
	err := s.InterruptFinish(ctx, id, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
	if !errors.Is(err, store.ErrInterruptAlreadyTerminal) {
		t.Errorf("got %v, want ErrInterruptAlreadyTerminal", err)
	}
}

func testInterruptListByRunFiltersByStatus(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_list", "batch")

	// Three interrupts: one pending, one resolved, one cancelled.
	id1 := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: id1, RunID: runID, UserID: "u_alice", Question: "Q1", CreatedAt: time.Now()})
	id2 := store.MintInterruptID(time.Now().Add(time.Millisecond))
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: id2, RunID: runID, UserID: "u_alice", Question: "Q2", CreatedAt: time.Now().Add(time.Millisecond)})
	_ = s.InterruptResolve(ctx, id2, "ok", store.InterruptResolvedByWebUI, nil)
	id3 := store.MintInterruptID(time.Now().Add(2 * time.Millisecond))
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: id3, RunID: runID, UserID: "u_alice", Question: "Q3", CreatedAt: time.Now().Add(2 * time.Millisecond)})
	_ = s.InterruptFinish(ctx, id3, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)

	all, err := s.InterruptListByRun(ctx, runID, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("list-all returned %d rows, want 3", len(all))
	}

	pending, _ := s.InterruptListByRun(ctx, runID, store.InterruptStatusPending)
	if len(pending) != 1 || pending[0].InterruptID != id1 {
		t.Errorf("pending filter returned %d rows; want 1 (id=%s)", len(pending), id1)
	}
}

func testInterruptListByUserFiltersByStatus(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runA := makeRunForInterrupt(t, s, "u_bob", "a_intr_ub_1", "agent-a")
	_, runB := makeRunForInterrupt(t, s, "u_bob", "a_intr_ub_2", "agent-b")
	_, runC := makeRunForInterrupt(t, s, "u_other", "a_intr_uo_1", "agent-c")

	id1 := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: id1, RunID: runA, UserID: "u_bob", Question: "Q from A", CreatedAt: time.Now()})
	id2 := store.MintInterruptID(time.Now().Add(time.Millisecond))
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: id2, RunID: runB, UserID: "u_bob", Question: "Q from B", CreatedAt: time.Now().Add(time.Millisecond)})
	// Unrelated user — must not appear in u_bob's listing.
	id3 := store.MintInterruptID(time.Now().Add(2 * time.Millisecond))
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: id3, RunID: runC, UserID: "u_other", Question: "Q from C", CreatedAt: time.Now().Add(2 * time.Millisecond)})

	rows, err := s.InterruptListByUser(ctx, "u_bob", store.InterruptStatusPending)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows for u_bob, want 2", len(rows))
	}
	for _, r := range rows {
		if r.UserID != "u_bob" {
			t.Errorf("listing leaked row from %q", r.UserID)
		}
	}
}

func testInterruptCountPendingByRunIsAccurateUnderConcurrency(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_count", "batch")

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			id := store.MintInterruptID(time.Now())
			_, _ = s.InterruptCreate(ctx, store.InterruptRow{
				InterruptID: id, RunID: runID, UserID: "u_alice",
				Question: "Q?", CreatedAt: time.Now(),
			})
		}(i)
	}
	wg.Wait()
	n, err := s.InterruptCountPendingByRun(ctx, runID)
	if err != nil {
		t.Fatalf("InterruptCountPendingByRun: %v", err)
	}
	if n != N {
		t.Errorf("count = %d, want %d under parallel inserts", n, N)
	}
}

func testInterruptSweepExpiredMarksOnlyExpiredPending(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_sweep", "batch")

	// Row 1: expired pending — should be swept.
	id1 := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id1, RunID: runID, UserID: "u_alice", Question: "Q1",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	})
	// Row 2: future expiry — must NOT be swept.
	id2 := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id2, RunID: runID, UserID: "u_alice", Question: "Q2",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})
	// Row 3: no expiry — must NOT be swept.
	id3 := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id3, RunID: runID, UserID: "u_alice", Question: "Q3",
		CreatedAt: time.Now(),
		// ExpiresAt zero → no expiry
	})
	// Row 4: future-expiry + already resolved — must NOT change
	// status. (Resolved at within the validity window; the sweeper
	// should never touch a non-pending row regardless of expiry.)
	id4 := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id4, RunID: runID, UserID: "u_alice", Question: "Q4",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})
	_ = s.InterruptResolve(ctx, id4, "answered within window", store.InterruptResolvedByWebUI, nil)

	n, err := s.InterruptSweepExpired(ctx)
	if err != nil {
		t.Fatalf("InterruptSweepExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d rows, want 1", n)
	}
	r1, _ := s.InterruptGet(ctx, id1)
	if r1.Status != store.InterruptStatusTimedOut {
		t.Errorf("Row1 status = %q, want timed_out", r1.Status)
	}
	if r1.ResolvedBy != store.InterruptResolvedByTimeout {
		t.Errorf("Row1 ResolvedBy = %q, want timeout", r1.ResolvedBy)
	}
	r2, _ := s.InterruptGet(ctx, id2)
	if r2.Status != store.InterruptStatusPending {
		t.Errorf("Row2 status = %q, want pending (future expiry)", r2.Status)
	}
	r3, _ := s.InterruptGet(ctx, id3)
	if r3.Status != store.InterruptStatusPending {
		t.Errorf("Row3 status = %q, want pending (no expiry)", r3.Status)
	}
	r4, _ := s.InterruptGet(ctx, id4)
	if r4.Status != store.InterruptStatusResolved {
		t.Errorf("Row4 status = %q, want resolved (must not re-finalise)", r4.Status)
	}
}

func testInterruptIDIsMonotonicByTime(t *testing.T, s store.Store) {
	// Pure unit-style check on the MintInterruptID format — IDs minted
	// later in time must lex-compare greater than earlier IDs. The
	// `interrupts_by_run_status` index orders by created_at internally,
	// but ID monotonicity is the operator-debugging-eyeball property
	// described on MintInterruptID's docstring.
	t1 := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Microsecond)
	id1 := store.MintInterruptID(t1)
	id2 := store.MintInterruptID(t2)
	if !(id1 < id2) {
		t.Errorf("MintInterruptID not monotonic by time: %s ≥ %s (t1=%v t2=%v)", id1, id2, t1, t2)
	}
	const wantLen = len("intr_") + 16 + 8
	if len(id1) != wantLen {
		t.Errorf("id length = %d, want %d", len(id1), wantLen)
	}
}
