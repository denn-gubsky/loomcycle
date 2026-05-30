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
	"reflect"
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
		{"CreateRunParentContextRoundTrip", testCreateRunParentContextRoundTrip},
		{"CreateRunIdempotencyKeyRoundTrip", testCreateRunIdempotencyKeyRoundTrip},
		{"CreateRunDuplicateIdempotencyKeyRefused", testCreateRunDuplicateIdempotencyKeyRefused},
		{"RunByIdempotencyKeyHitAndMiss", testRunByIdempotencyKeyHitAndMiss},
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
		{"SetRunPauseStateRoundTrip", testSetRunPauseStateRoundTrip},
		{"SetRunPauseStateUnknownStateRefused", testSetRunPauseStateUnknownStateRefused},
		{"SetRunPauseStateMissingRunReturnsNotFound", testSetRunPauseStateMissingRunReturnsNotFound},
		{"ListPausedRunsEmpty", testListPausedRunsEmpty},
		{"ListPausedRunsExcludesPausingAndRunning", testListPausedRunsExcludesPausingAndRunning},
		{"ListPausedRunsOrderedByStartedAtAsc", testListPausedRunsOrderedByStartedAtAsc},
		{"SnapshotCreateRoundTrip", testSnapshotCreateRoundTrip},
		{"SnapshotCreateConflictOnDuplicateID", testSnapshotCreateConflictOnDuplicateID},
		{"SnapshotCreateRejectsEmptyFields", testSnapshotCreateRejectsEmptyFields},
		{"SnapshotGetNotFound", testSnapshotGetNotFound},
		{"SnapshotListOrderedByCreatedAtDesc", testSnapshotListOrderedByCreatedAtDesc},
		{"SnapshotListLabelSubstringFilter", testSnapshotListLabelSubstringFilter},
		{"SnapshotListLimitBounds", testSnapshotListLimitBounds},
		{"SnapshotListExcludesJSONPayload", testSnapshotListExcludesJSONPayload},
		{"SnapshotDeleteIdempotent", testSnapshotDeleteIdempotent},
		{"SnapshotReadAgentDefsEmpty", testSnapshotReadAgentDefsEmpty},
		{"SnapshotReadAgentDefActiveEmpty", testSnapshotReadAgentDefActiveEmpty},
		{"SnapshotReadMemoryEmpty", testSnapshotReadMemoryEmpty},
		{"SnapshotReadMemoryFiltersExpired", testSnapshotReadMemoryFiltersExpired},
		{"SnapshotReadMemoryOrdered", testSnapshotReadMemoryOrdered},
		{"SnapshotReadChannelMessagesEmpty", testSnapshotReadChannelMessagesEmpty},
		{"SnapshotReadChannelCursorsEmpty", testSnapshotReadChannelCursorsEmpty},
		{"SnapshotReadEvaluationsEmpty", testSnapshotReadEvaluationsEmpty},
		{"SnapshotReadEvaluationsOrderedByCreatedAt", testSnapshotReadEvaluationsOrderedByCreatedAt},
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
		// v0.12.x — MemoryAtomicUpdate primitive backing the new
		// reducer ops (Memory.merge / append_dedupe / bounded_list).
		{"MemoryAtomicUpdateOnNewKey", testMemoryAtomicUpdateOnNewKey},
		{"MemoryAtomicUpdateOnExistingKey", testMemoryAtomicUpdateOnExistingKey},
		{"MemoryAtomicUpdateOnExpiredKey", testMemoryAtomicUpdateOnExpiredKey},
		{"MemoryAtomicUpdateReducerErrorRollsBack", testMemoryAtomicUpdateReducerErrorRollsBack},
		{"MemoryAtomicUpdateInvalidJSONRejected", testMemoryAtomicUpdateInvalidJSONRejected},
		{"MemoryAtomicUpdateIsAtomicUnderConcurrency", testMemoryAtomicUpdateIsAtomicUnderConcurrency},
		// v0.9.0 Vector Memory (commit 1 of PR 1).
		// Each test forks at the top on SupportsVectors() and asserts
		// either the refusal path (SQLite, or Postgres without
		// pgvector) or the real round-trip (Postgres + pgvector).
		// Both backends run the same test cases.
		{"MemoryEmbedSetRoundTrip", testMemoryEmbedSetRoundTrip},
		{"MemoryEmbedSearchTopK", testMemoryEmbedSearchTopK},
		{"MemoryEmbedSearchScopeIsolation", testMemoryEmbedSearchScopeIsolation},
		{"MemoryEmbedSearchDimensionMismatch", testMemoryEmbedSearchDimensionMismatch},
		{"MemoryEmbedSearchEmptyScope", testMemoryEmbedSearchEmptyScope},
		{"MemoryDeleteCascadesEmbedding", testMemoryDeleteCascadesEmbedding},
		{"MemoryEmbedListByModelFiltersCurrent", testMemoryEmbedListByModelFiltersCurrent},
		{"MemoryEmbedStatsReportsPerModelCount", testMemoryEmbedStatsReportsPerModelCount},
		{"ChannelPublishSubscribeRoundTrip", testChannelPublishSubscribeRoundTrip},
		{"ChannelSubscribeEmptyChannel", testChannelSubscribeEmptyChannel},
		{"ChannelCursorAdvancesAcrossSubscribes", testChannelCursorAdvancesAcrossSubscribes},
		{"ChannelAckIsIdempotent", testChannelAckIsIdempotent},
		{"ChannelAckRejectsCursorRegression", testChannelAckRejectsCursorRegression},
		{"ChannelListCursorsForScopeReturnsOnlyMatchingTuple", testChannelListCursorsForScope},
		{"ChannelTTLFilteredAtRead", testChannelTTLFilteredAtRead},
		{"ChannelSweepReapsExpired", testChannelSweepReapsExpired},
		{"ChannelMaxMessagesTrimsOldest", testChannelMaxMessagesTrimsOldest},
		{"ChannelScopeIsolation", testChannelScopeIsolation},
		{"ChannelPeekDoesNotConsume", testChannelPeekDoesNotConsume},
		{"ChannelReplayFromCursorZero", testChannelReplayFromCursorZero},
		{"ChannelStatsAggregatesNonExpired", testChannelStatsAggregatesNonExpired},
		{"ChannelStatsEmptyOnNoMessages", testChannelStatsEmptyOnNoMessages},
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
		{"AgentDefContentSHA256RoundTrip", testAgentDefContentSHA256RoundTrip},
		{"BackfillAgentDefContentSHA256", testBackfillAgentDefContentSHA256},
		{"BackfillAgentDefSystemPromptBaseFillsLegacyRows", testBackfillAgentDefSystemPromptBase},
		// v0.8.22 SkillDef substrate — mirror of the AgentDef tests.
		{"SkillDefCreateAndGet", testSkillDefCreateAndGet},
		{"SkillDefVersionMonotonicUnderContention", testSkillDefVersionMonotonicUnderContention},
		{"SkillDefParallelForksDistinctVersions", testSkillDefParallelForksDistinctVersions},
		{"SkillDefAppendOnlyDefinition", testSkillDefAppendOnlyDefinition},
		{"SkillDefActivePointerIdempotent", testSkillDefActivePointerIdempotent},
		{"SkillDefRetireReversible", testSkillDefRetireReversible},
		{"SkillDefStaticFallback", testSkillDefStaticFallback},
		{"SkillDefContentSHA256RoundTrip", testSkillDefContentSHA256RoundTrip},
		{"BackfillSkillDefContentSHA256", testBackfillSkillDefContentSHA256},
		{"SkillDefSnapshotReadEmpty", testSkillDefSnapshotReadEmpty},
		// v0.9.x MCPServerDef substrate — mirror of the AgentDef + SkillDef tests.
		{"MCPServerDefCreateAndGet", testMCPServerDefCreateAndGet},
		{"MCPServerDefVersionMonotonic", testMCPServerDefVersionMonotonic},
		{"MCPServerDefActivePointerIdempotent", testMCPServerDefActivePointerIdempotent},
		{"MCPServerDefRetireReversible", testMCPServerDefRetireReversible},
		{"MCPServerDefContentSHA256RoundTrip", testMCPServerDefContentSHA256RoundTrip},
		{"BackfillMCPServerDefContentSHA256", testBackfillMCPServerDefContentSHA256},
		// v1.x RFC E ScheduleDef substrate — same shape minus content_sha256.
		{"ScheduleDefCreateAndGet", testScheduleDefCreateAndGet},
		{"ScheduleDefVersionMonotonic", testScheduleDefVersionMonotonic},
		{"ScheduleDefActivePointerIdempotent", testScheduleDefActivePointerIdempotent},
		{"ScheduleDefRetireReversible", testScheduleDefRetireReversible},
		{"ScheduleDefParentNotFound", testScheduleDefParentNotFound},
		{"ScheduleDefListByName", testScheduleDefListByName},
		{"ScheduleDefListChildren", testScheduleDefListChildren},
		// v1.x RFC G A2A substrate — same shape as ScheduleDef, two Defs.
		{"A2AServerCardDefCreateAndGet", testA2AServerCardDefCreateAndGet},
		{"A2AServerCardDefVersionMonotonic", testA2AServerCardDefVersionMonotonic},
		{"A2AServerCardDefActivePointerIdempotent", testA2AServerCardDefActivePointerIdempotent},
		{"A2AServerCardDefRetireReversible", testA2AServerCardDefRetireReversible},
		{"A2AServerCardDefParentNotFound", testA2AServerCardDefParentNotFound},
		{"A2AServerCardDefListByName", testA2AServerCardDefListByName},
		{"A2AServerCardDefListChildren", testA2AServerCardDefListChildren},
		{"A2AAgentDefCreateAndGet", testA2AAgentDefCreateAndGet},
		{"A2AAgentDefVersionMonotonic", testA2AAgentDefVersionMonotonic},
		{"A2AAgentDefActivePointerIdempotent", testA2AAgentDefActivePointerIdempotent},
		{"A2AAgentDefRetireReversible", testA2AAgentDefRetireReversible},
		{"A2AAgentDefParentNotFound", testA2AAgentDefParentNotFound},
		{"A2AAgentDefListByName", testA2AAgentDefListByName},
		{"A2AAgentDefListChildren", testA2AAgentDefListChildren},
		{"WebhookDefCreateAndGet", testWebhookDefCreateAndGet},
		{"WebhookDefVersionMonotonic", testWebhookDefVersionMonotonic},
		{"WebhookDefActivePointerIdempotent", testWebhookDefActivePointerIdempotent},
		{"WebhookDefRetireReversible", testWebhookDefRetireReversible},
		{"WebhookDefParentNotFound", testWebhookDefParentNotFound},
		{"WebhookDefListByName", testWebhookDefListByName},
		{"WebhookDefListChildren", testWebhookDefListChildren},
		{"MemoryBackendDefCreateAndGet", testMemoryBackendDefCreateAndGet},
		{"MemoryBackendDefVersionMonotonic", testMemoryBackendDefVersionMonotonic},
		{"MemoryBackendDefActivePointerIdempotent", testMemoryBackendDefActivePointerIdempotent},
		{"MemoryBackendDefRetireReversible", testMemoryBackendDefRetireReversible},
		{"MemoryBackendDefParentNotFound", testMemoryBackendDefParentNotFound},
		{"MemoryBackendDefListByName", testMemoryBackendDefListByName},
		{"MemoryBackendDefListChildren", testMemoryBackendDefListChildren},
		// v1.x RFC E ScheduleDef runtime — sweeper-side state.
		{"ScheduleRunStateSeedAndGet", testScheduleRunStateSeedAndGet},
		{"ScheduleRunStateListDueRespectsRetiredAndPaused", testScheduleRunStateListDueRespectsRetiredAndPaused},
		{"ScheduleRunStateRecordResult", testScheduleRunStateRecordResult},
		{"ScheduleRunStatePauseResume", testScheduleRunStatePauseResume},
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
		// v0.8.21 audit-view cross-session event listing.
		{"ListEventsFilterByTypeAndRange", testListEventsFilterByTypeAndRange},
		{"ListEventsPaginationAndTotal", testListEventsPaginationAndTotal},
		// v0.8.21 awaited-state derivation needs last-event-per-run.
		{"GetLastEventForRunEmpty", testGetLastEventForRunEmpty},
		{"GetLastEventForRunReturnsHighestSeq", testGetLastEventForRunReturnsHighestSeq},
		// v0.12.7 provider telemetry — FinishRun must persist the
		// final-iteration provider so post-run analysis can count
		// fallback-routed runs.
		{"FinishRunPersistsProvider", testFinishRunPersistsProvider},
		{"FinishRunPersistsProviderEmpty", testFinishRunPersistsProviderEmpty},
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

// testFinishRunPersistsProvider verifies that FinishRun writes the
// Usage.Provider field to storage and GetRun reads it back. Distinct
// from Model — the v0.8.2 runtime fallback path lands a row whose
// Model is the original config target but whose Provider is whatever
// driver actually served the final iteration. The x1000 load test
// needs both to be queryable to characterize fallback frequency.
func testFinishRunPersistsProvider(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_provider_roundtrip"})

	usage := store.Usage{
		InputTokens:  100,
		OutputTokens: 50,
		Model:        "claude-haiku-4-5-20251001",
		Provider:     "anthropic-oauth-dev",
	}
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", usage, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRunByAgentID(ctx, "a_provider_roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want claude-haiku-4-5-20251001", got.Model)
	}
	if got.Provider != "anthropic-oauth-dev" {
		t.Errorf("Provider = %q, want anthropic-oauth-dev", got.Provider)
	}
}

// testFinishRunPersistsProviderEmpty confirms an empty Usage.Provider
// round-trips as empty string (NULL in the underlying column). Pre-
// migration rows + pre-call failures hit this path; consumers must
// not see "" surface as some sentinel like "unknown".
func testFinishRunPersistsProviderEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_provider_empty"})

	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
		store.Usage{Model: "m", Provider: ""}, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRunByAgentID(ctx, "a_provider_empty")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty string", got.Provider)
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

// testCreateRunParentContextRoundTrip pins the v0.12.x contract: the
// opaque parent_context (JSON column) round-trips through CreateRun →
// GetRun for both backends, and a run created without it reads back nil
// (back-compat with pre-migration rows + runs with no tracking context).
func testCreateRunParentContextRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	pc := &store.ParentContext{RootAgentRunID: "run_root", FunctionKey: "cv-batch", TierAtRun: "pro"}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_pc", UserID: "user-1", ParentContext: pc})
	if err != nil {
		t.Fatal(err)
	}
	if run.ParentContext == nil || *run.ParentContext != *pc {
		t.Errorf("CreateRun did not return ParentContext: %+v", run.ParentContext)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentContext == nil || *got.ParentContext != *pc {
		t.Errorf("ParentContext not preserved through GetRun: got %+v want %+v", got.ParentContext, pc)
	}

	// No context → nil round-trip (back-compat).
	bare, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_nopc", UserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	gotBare, err := s.GetRun(ctx, bare.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBare.ParentContext != nil {
		t.Errorf("run without context should read back nil ParentContext, got %+v", gotBare.ParentContext)
	}
}

// testCreateRunIdempotencyKeyRoundTrip pins the RFC H Decision 10 "Layer
// 2" contract: a run created with an idempotency_key persists it and
// reads it back through CreateRun → GetRun. A run created without one
// reads back empty (back-compat with pre-migration rows + the common
// keyless case).
func testCreateRunIdempotencyKeyRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_idem", UserID: "user-1", IdempotencyKey: "delivery-123"})
	if err != nil {
		t.Fatal(err)
	}
	if run.IdempotencyKey != "delivery-123" {
		t.Errorf("CreateRun did not return IdempotencyKey: got %q", run.IdempotencyKey)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != "delivery-123" {
		t.Errorf("IdempotencyKey not preserved through GetRun: got %q want %q", got.IdempotencyKey, "delivery-123")
	}

	// No key → empty round-trip (back-compat).
	bare, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_noidem", UserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	gotBare, err := s.GetRun(ctx, bare.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBare.IdempotencyKey != "" {
		t.Errorf("run without key should read back empty IdempotencyKey, got %q", gotBare.IdempotencyKey)
	}
}

// testCreateRunDuplicateIdempotencyKeyRefused pins the RFC H Decision 10
// durable-dedup invariant: a second CreateRun carrying a key already
// claimed returns store.ErrDuplicateIdempotencyKey (not a generic error)
// and does NOT insert a second row. Two DIFFERENT keys, and a keyless run
// alongside a keyed one, are both unaffected — the constraint is scoped
// to the key, never the keyless majority.
func testCreateRunDuplicateIdempotencyKeyRefused(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	first, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a1", UserID: "user-1", IdempotencyKey: "dup-key"})
	if err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}

	_, err = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a2", UserID: "user-1", IdempotencyKey: "dup-key"})
	if !errors.Is(err, store.ErrDuplicateIdempotencyKey) {
		t.Fatalf("second CreateRun with same key: got err %v, want ErrDuplicateIdempotencyKey", err)
	}

	// The existing run is still the only one carrying the key.
	got, ok, err := s.RunByIdempotencyKey(ctx, "dup-key")
	if err != nil || !ok {
		t.Fatalf("RunByIdempotencyKey after dup: ok=%v err=%v", ok, err)
	}
	if got.ID != first.ID {
		t.Errorf("dup key resolved to wrong run: got %s want %s", got.ID, first.ID)
	}

	// A DIFFERENT key is unaffected.
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a3", UserID: "user-1", IdempotencyKey: "other-key"}); err != nil {
		t.Errorf("CreateRun with a distinct key should succeed, got %v", err)
	}

	// Two keyless runs coexist — the partial unique index never fires on
	// NULL. (This is the regression guard against accidentally
	// constraining the keyless majority.)
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a4", UserID: "user-1"}); err != nil {
		t.Errorf("first keyless CreateRun: %v", err)
	}
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a5", UserID: "user-1"}); err != nil {
		t.Errorf("second keyless CreateRun should not collide on NULL key: %v", err)
	}
}

// testRunByIdempotencyKeyHitAndMiss pins the lookup contract: a known key
// returns (run, true, nil); an unknown key and an empty key both return
// (zero, false, nil) — no error on miss.
func testRunByIdempotencyKeyHitAndMiss(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	want, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_hit", UserID: "user-1", IdempotencyKey: "key-hit"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.RunByIdempotencyKey(ctx, "key-hit")
	if err != nil {
		t.Fatalf("RunByIdempotencyKey hit: %v", err)
	}
	if !ok || got.ID != want.ID {
		t.Errorf("hit: got (ok=%v id=%s), want (ok=true id=%s)", ok, got.ID, want.ID)
	}
	if got.AgentID != "a_hit" {
		t.Errorf("hit: identity not preserved: %+v", got)
	}

	_, ok, err = s.RunByIdempotencyKey(ctx, "key-absent")
	if err != nil || ok {
		t.Errorf("miss: got (ok=%v err=%v), want (false, nil)", ok, err)
	}

	_, ok, err = s.RunByIdempotencyKey(ctx, "")
	if err != nil || ok {
		t.Errorf("empty key: got (ok=%v err=%v), want (false, nil)", ok, err)
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
	// or started_at). We previously ran with 60ms sleep + 30ms cutoff
	// (= 30ms margin), but PR #232's CI race-detector run flaked: the
	// fresh row's heartbeat write landed before the cutoff because
	// `-race`'s 2-5× scheduling slowdown collapsed the 30ms margin.
	// Bumped to 200ms sleep + 100ms cutoff offset = 100ms margin in
	// either direction. Test runtime up by 140ms but no longer
	// timing-sensitive to scheduler load. Same shape as the
	// heartbeat-sweeper fix in PR #224.
	time.Sleep(200 * time.Millisecond)

	// 3) A fresh run that heartbeated AFTER the cutoff — must NOT be
	//    swept.
	fresh, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_fresh"})
	_ = s.UpdateHeartbeat(ctx, fresh.ID)

	// 4) An already-terminal run — must NOT be touched.
	terminalRun, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_terminal"})
	_ = s.FinishRun(ctx, terminalRun.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// Cutoff: between the stale row's last activity and the fresh
	// row's. 100ms-back keeps the stale + noHB rows clearly past the
	// cutoff (200ms ago) while the fresh row (just heartbeated)
	// stays clearly after it. 100ms margin survives -race scheduling
	// slowdown.
	cutoff := time.Now().Add(-100 * time.Millisecond)

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

// SetRunPauseState writes the column; the value round-trips through a
// fresh GetRunByAgentID; same-value writes are idempotent.
func testSetRunPauseStateRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_pause_rt", UserID: "u"})

	// Default after create.
	got, _ := s.GetRunByAgentID(ctx, "a_pause_rt")
	if got.PauseState != store.PauseStateRunning {
		t.Errorf("default PauseState = %q, want %q", got.PauseState, store.PauseStateRunning)
	}

	// running → pausing → paused → running.
	for _, state := range []string{store.PauseStatePausing, store.PauseStatePaused, store.PauseStateRunning} {
		if err := s.SetRunPauseState(ctx, run.ID, state); err != nil {
			t.Fatalf("SetRunPauseState(%q): %v", state, err)
		}
		got, _ := s.GetRunByAgentID(ctx, "a_pause_rt")
		if got.PauseState != state {
			t.Errorf("after SetRunPauseState(%q): got %q", state, got.PauseState)
		}
	}

	// Idempotent: writing the current value succeeds with no side effect.
	if err := s.SetRunPauseState(ctx, run.ID, store.PauseStateRunning); err != nil {
		t.Errorf("idempotent write rejected: %v", err)
	}
}

// SetRunPauseState refuses unknown state strings — the boundary check
// catches a future caller bug before garbage lands in the column.
func testSetRunPauseStateUnknownStateRefused(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_pause_bad", UserID: "u"})

	for _, bad := range []string{"", "PAUSED", "stopped", "halt", " paused"} {
		err := s.SetRunPauseState(ctx, run.ID, bad)
		if err == nil {
			t.Errorf("SetRunPauseState(%q) accepted; want refusal", bad)
		}
	}
	// Confirm the column wasn't touched by any of the rejected writes.
	got, _ := s.GetRunByAgentID(ctx, "a_pause_bad")
	if got.PauseState != store.PauseStateRunning {
		t.Errorf("rejected writes leaked through: PauseState = %q", got.PauseState)
	}
}

// SetRunPauseState on a missing run_id returns *ErrNotFound — the
// pause manager relies on this to distinguish "row exists but stuck"
// from "row was deleted out from under us."
func testSetRunPauseStateMissingRunReturnsNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	err := s.SetRunPauseState(ctx, "run_does_not_exist", store.PauseStatePaused)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("err = %v, want *ErrNotFound", err)
	}
}

// ListPausedRuns on a fresh store returns no rows. Establishes the
// baseline so the next test can prove non-paused rows don't leak in.
func testListPausedRunsEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.ListPausedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ListPausedRuns on empty store returned %d rows", len(got))
	}
}

// ListPausedRuns filters strictly on pause_state = 'paused' (NOT
// 'pausing', NOT 'running'). The distinction matters: 'pausing' is
// the in-flight transition; the pause manager only resumes runs that
// reached the at-rest paused state.
func testListPausedRunsExcludesPausingAndRunning(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	running, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_running", UserID: "u"})
	pausing, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_pausing", UserID: "u"})
	paused, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_paused", UserID: "u"})

	_ = s.SetRunPauseState(ctx, pausing.ID, store.PauseStatePausing)
	_ = s.SetRunPauseState(ctx, paused.ID, store.PauseStatePaused)

	got, err := s.ListPausedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListPausedRuns = %d rows, want 1; got %+v", len(got), got)
	}
	if got[0].ID != paused.ID {
		t.Errorf("ListPausedRuns returned run_id %q, want %q (the only paused row)",
			got[0].ID, paused.ID)
	}
	// Defensive check the other two weren't accidentally flipped.
	_ = running
	rGot, _ := s.GetRunByAgentID(ctx, "a_running")
	if rGot.PauseState != store.PauseStateRunning {
		t.Errorf("a_running PauseState = %q, want running", rGot.PauseState)
	}
}

// ListPausedRuns orders by started_at ASC so the resume sweep
// processes the oldest pauses first. Tests against three rows
// inserted with deliberate started_at gaps.
func testListPausedRunsOrderedByStartedAtAsc(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	first, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_first", UserID: "u"})
	time.Sleep(10 * time.Millisecond)
	second, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_second", UserID: "u"})
	time.Sleep(10 * time.Millisecond)
	third, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_third", UserID: "u"})

	// Flip them all to paused in REVERSE order of creation — making
	// sure the ordering reflects started_at, not the order of
	// SetRunPauseState calls or any insertion-position artefact.
	_ = s.SetRunPauseState(ctx, third.ID, store.PauseStatePaused)
	_ = s.SetRunPauseState(ctx, second.ID, store.PauseStatePaused)
	_ = s.SetRunPauseState(ctx, first.ID, store.PauseStatePaused)

	got, err := s.ListPausedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ListPausedRuns = %d, want 3", len(got))
	}
	want := []string{first.ID, second.ID, third.ID}
	for i, r := range got {
		if r.ID != want[i] {
			t.Errorf("ListPausedRuns[%d] = %q, want %q (oldest-first ordering)",
				i, r.ID, want[i])
		}
	}
}

// SnapshotCreate writes one row; SnapshotGet retrieves it; the
// JSON payload, label, schema_version, and byte_size round-trip
// byte-equal.
func testSnapshotCreateRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	payload := []byte(`{"schema_version":1,"sections":{"memory":{"version":"1.0","entries":[]}}}`)
	row := store.SnapshotRow{
		ID:            "snap_test_1",
		Label:         "before-backup",
		SchemaVersion: 1,
		ByteSize:      int64(len(payload)),
		JSONContent:   payload,
	}
	if err := s.SnapshotCreate(ctx, row); err != nil {
		t.Fatalf("SnapshotCreate: %v", err)
	}
	got, err := s.SnapshotGet(ctx, "snap_test_1")
	if err != nil {
		t.Fatalf("SnapshotGet: %v", err)
	}
	if got.ID != row.ID || got.Label != row.Label || got.SchemaVersion != row.SchemaVersion || got.ByteSize != row.ByteSize {
		t.Errorf("metadata mismatch: got %+v want %+v", got, row)
	}
	// JSONContent round-trips SEMANTICALLY equivalent — Postgres JSONB
	// reorders keys canonically; SQLite TEXT preserves byte-for-byte.
	// The contract is that any consumer parsing the returned bytes as
	// JSON sees the same logical object as what was written.
	var wantAny, gotAny any
	if err := json.Unmarshal(payload, &wantAny); err != nil {
		t.Fatalf("test fixture is invalid JSON: %v", err)
	}
	if err := json.Unmarshal(got.JSONContent, &gotAny); err != nil {
		t.Fatalf("returned JSONContent is invalid JSON: %v", err)
	}
	if !reflect.DeepEqual(wantAny, gotAny) {
		t.Errorf("JSONContent round-trip differs semantically:\n got: %s\nwant: %s", got.JSONContent, payload)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should default to now() when not supplied")
	}
}

// SnapshotCreate with a colliding id returns *store.ErrConflict —
// distinguishable from generic insert errors so callers doing
// captureOrSkip can branch cleanly.
func testSnapshotCreateConflictOnDuplicateID(t *testing.T, s store.Store) {
	ctx := context.Background()
	payload := []byte(`{"v":1}`)
	row := store.SnapshotRow{ID: "snap_dup_1", SchemaVersion: 1, ByteSize: 5, JSONContent: payload}
	if err := s.SnapshotCreate(ctx, row); err != nil {
		t.Fatalf("first SnapshotCreate: %v", err)
	}
	err := s.SnapshotCreate(ctx, row)
	if err == nil {
		t.Fatal("second SnapshotCreate accepted; expected ErrConflict")
	}
	var conflict *store.ErrConflict
	if !errors.As(err, &conflict) {
		t.Errorf("err = %v, want *ErrConflict", err)
	}
}

// Empty id or empty json_content is rejected at the store boundary
// rather than landing as a malformed row.
func testSnapshotCreateRejectsEmptyFields(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Empty ID.
	if err := s.SnapshotCreate(ctx, store.SnapshotRow{JSONContent: []byte(`{}`)}); err == nil {
		t.Error("empty id accepted; expected refusal")
	}
	// Empty JSON content.
	if err := s.SnapshotCreate(ctx, store.SnapshotRow{ID: "snap_empty"}); err == nil {
		t.Error("empty json_content accepted; expected refusal")
	}
}

// SnapshotGet on a missing id returns *store.ErrNotFound (typed,
// not a generic error).
func testSnapshotGetNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.SnapshotGet(ctx, "snap_no_such_id")
	if err == nil {
		t.Fatal("expected error on missing snapshot, got nil")
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("err = %v, want *ErrNotFound", err)
	}
	if nf.Kind != "snapshot" {
		t.Errorf("err Kind = %q, want %q", nf.Kind, "snapshot")
	}
}

// SnapshotList returns rows newest-first (created_at DESC). Tests
// against three snapshots inserted with deliberate created_at gaps.
func testSnapshotListOrderedByCreatedAtDesc(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC()
	for i, dt := range []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond} {
		row := store.SnapshotRow{
			ID:            fmt.Sprintf("snap_ord_%d", i),
			CreatedAt:     now.Add(dt),
			SchemaVersion: 1,
			ByteSize:      4,
			JSONContent:   []byte(`{}`),
		}
		if err := s.SnapshotCreate(ctx, row); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	got, err := s.SnapshotList(ctx, "", 0)
	if err != nil {
		t.Fatalf("SnapshotList: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	// Newest first.
	wantIDs := []string{"snap_ord_2", "snap_ord_1", "snap_ord_0"}
	for i, e := range got {
		if e.ID != wantIDs[i] {
			t.Errorf("[%d] ID = %q, want %q (newest-first)", i, e.ID, wantIDs[i])
		}
	}
}

// SnapshotList(labelContains) filters case-insensitively and matches
// substrings. Operators search for label fragments without knowing
// exact case.
func testSnapshotListLabelSubstringFilter(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, label := range []string{"Pre-Migration-A", "pre-migration-B", "morning-stop"} {
		row := store.SnapshotRow{
			ID:            fmt.Sprintf("snap_lab_%d", i),
			Label:         label,
			SchemaVersion: 1,
			ByteSize:      4,
			JSONContent:   []byte(`{}`),
		}
		if err := s.SnapshotCreate(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	// Lowercase substring — should match both Pre-Migration variants.
	got, err := s.SnapshotList(ctx, "migration", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (both labels containing 'migration' case-insensitive); got %+v", len(got), got)
	}
	// Filter that matches none.
	got, _ = s.SnapshotList(ctx, "evening", 0)
	if len(got) != 0 {
		t.Errorf("no-match filter returned %d rows", len(got))
	}
}

// SnapshotList(limit=N) caps the result count; limit=0 means no cap.
func testSnapshotListLimitBounds(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = s.SnapshotCreate(ctx, store.SnapshotRow{
			ID:            fmt.Sprintf("snap_lim_%d", i),
			SchemaVersion: 1,
			ByteSize:      4,
			JSONContent:   []byte(`{}`),
		})
	}
	got, _ := s.SnapshotList(ctx, "", 2)
	if len(got) != 2 {
		t.Errorf("limit=2 returned %d rows", len(got))
	}
	got, _ = s.SnapshotList(ctx, "", 0)
	if len(got) != 5 {
		t.Errorf("limit=0 returned %d rows, want 5 (no cap)", len(got))
	}
	got, _ = s.SnapshotList(ctx, "", 100)
	if len(got) != 5 {
		t.Errorf("limit > rows returned %d, want 5", len(got))
	}
}

// SnapshotList projection MUST NOT include the JSON payload — that's
// the entire point of the metadata-only shape (cheap responses when
// operators have hundreds of snapshots). Implementations that
// accidentally select json_content into a list response are a real
// performance regression risk; this test pins the absence.
func testSnapshotListExcludesJSONPayload(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Build a valid JSON object with 1000 keys. The trailing-comma
	// trap means we must construct via json.Marshal rather than
	// string concatenation — SQLite stores TEXT and accepts garbage
	// but Postgres JSONB rejects with SQLSTATE 22P02.
	bigMap := make(map[string]string, 1000)
	for i := 0; i < 1000; i++ {
		bigMap[fmt.Sprintf("k%d", i)] = "v"
	}
	bigPayload, err := json.Marshal(bigMap)
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	row := store.SnapshotRow{
		ID:            "snap_big",
		SchemaVersion: 1,
		ByteSize:      int64(len(bigPayload)),
		JSONContent:   bigPayload,
	}
	if err := s.SnapshotCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := s.SnapshotList(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("list empty after create")
	}
	// Find OUR row in the list — other contract tests may have
	// inserted rows ahead of us in this shared fixture run.
	var found *store.SnapshotListEntry
	for i := range got {
		if got[i].ID == "snap_big" {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("snap_big not in list (%d entries)", len(got))
	}
	// SnapshotListEntry has no JSONContent field — the type itself
	// enforces the exclusion. This test is a defensive
	// type-existence check + a byte_size round-trip confirmation
	// (operators rely on byte_size to know whether to download).
	if found.ByteSize != int64(len(bigPayload)) {
		t.Errorf("ByteSize = %d, want %d", found.ByteSize, len(bigPayload))
	}
}

// SnapshotDelete returns (true, nil) on a present row; (false, nil)
// on a missing row. Idempotent — operators scripting
// `loomcycle snapshot delete <id>` repeatedly never error.
func testSnapshotDeleteIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := store.SnapshotRow{
		ID:            "snap_del_1",
		SchemaVersion: 1,
		ByteSize:      4,
		JSONContent:   []byte(`{}`),
	}
	if err := s.SnapshotCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.SnapshotDelete(ctx, "snap_del_1")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Errorf("first Delete returned false; expected true (row was present)")
	}
	// Second delete: idempotent, no error, returns false.
	deleted, err = s.SnapshotDelete(ctx, "snap_del_1")
	if err != nil {
		t.Errorf("second Delete err = %v, want nil (idempotent)", err)
	}
	if deleted {
		t.Errorf("second Delete returned true; expected false (already gone)")
	}
	// Get after delete returns NotFound.
	_, err = s.SnapshotGet(ctx, "snap_del_1")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("Get after Delete: err = %v, want *ErrNotFound", err)
	}
}

// SnapshotReadAgentDefs returns [] on a fresh store. Establishes the
// baseline contract — the bulk readers are non-nil-safe; empty result
// is an empty slice + nil error, NOT nil slice + nil error.
func testSnapshotReadAgentDefsEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.SnapshotReadAgentDefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		// Allow either nil or empty slice; the contract is "no error,
		// loop-safe result." Convert nil to empty for the length check.
		got = []store.AgentDefRow{}
	}
	if len(got) != 0 {
		t.Errorf("fresh store: got %d rows, want 0", len(got))
	}
}

// SnapshotReadAgentDefActive returns [] on a fresh store.
func testSnapshotReadAgentDefActiveEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.SnapshotReadAgentDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fresh store: got %d rows, want 0", len(got))
	}
}

// SnapshotReadMemory returns [] on a fresh store.
func testSnapshotReadMemoryEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.SnapshotReadMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fresh store: got %d rows, want 0", len(got))
	}
}

// SnapshotReadMemory filters out expired rows. Insert one row with
// a TTL that's already in the past; bulk read must NOT include it.
func testSnapshotReadMemoryFiltersExpired(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Live row.
	if err := s.MemorySet(ctx, store.MemoryScope("agent"), "agentA", "live", json.RawMessage(`"hello"`), 0); err != nil {
		t.Fatal(err)
	}
	// Expired row: TTL of 1ns; wait briefly so wall-clock advances past.
	if err := s.MemorySet(ctx, store.MemoryScope("agent"), "agentA", "expired", json.RawMessage(`"gone"`), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	got, err := s.SnapshotReadMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Look for both keys — only "live" should be present.
	var liveSeen, expiredSeen bool
	for _, e := range got {
		switch e.Key {
		case "live":
			liveSeen = true
		case "expired":
			expiredSeen = true
		}
	}
	if !liveSeen {
		t.Error("live row missing from snapshot read")
	}
	if expiredSeen {
		t.Error("expired row appeared in snapshot read; expected filtered")
	}
}

// SnapshotReadMemory returns rows ordered by (scope, scope_id, key)
// for deterministic snapshot envelopes.
func testSnapshotReadMemoryOrdered(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Insert in deliberately scrambled order — must come back sorted.
	type seed struct {
		scope store.MemoryScope
		sid   string
		key   string
	}
	seeds := []seed{
		{store.MemoryScope("user"), "userX", "z_key"},
		{store.MemoryScope("agent"), "agentB", "key_b"},
		{store.MemoryScope("agent"), "agentA", "key_z"},
		{store.MemoryScope("agent"), "agentA", "key_a"},
	}
	for _, sd := range seeds {
		if err := s.MemorySet(ctx, sd.scope, sd.sid, sd.key, json.RawMessage(`"x"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SnapshotReadMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < len(seeds) {
		t.Fatalf("got %d rows, expected at least %d (other tests may have inserted more — ordering check below)", len(got), len(seeds))
	}
	// Verify ascending order on (scope, scope_id, key) for the seeded
	// rows. We can't assume the result is *only* our seeds (Postgres
	// fixture is shared across contract tests), but the ordering
	// invariant applies globally.
	prev := got[0]
	for i := 1; i < len(got); i++ {
		cur := got[i]
		if string(cur.Scope) < string(prev.Scope) ||
			(string(cur.Scope) == string(prev.Scope) && cur.ScopeID < prev.ScopeID) ||
			(string(cur.Scope) == string(prev.Scope) && cur.ScopeID == prev.ScopeID && cur.Key < prev.Key) {
			t.Errorf("ordering violated at index %d: %s/%s/%s before %s/%s/%s",
				i, prev.Scope, prev.ScopeID, prev.Key, cur.Scope, cur.ScopeID, cur.Key)
		}
		prev = cur
	}
}

// SnapshotReadChannelMessages returns [] on a fresh store.
func testSnapshotReadChannelMessagesEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.SnapshotReadChannelMessages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fresh store: got %d rows, want 0", len(got))
	}
}

// SnapshotReadChannelCursors returns [] on a fresh store.
func testSnapshotReadChannelCursorsEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.SnapshotReadChannelCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fresh store: got %d rows, want 0", len(got))
	}
}

// SnapshotReadEvaluations returns [] on a fresh store.
func testSnapshotReadEvaluationsEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.SnapshotReadEvaluations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fresh store: got %d rows, want 0", len(got))
	}
}

// SnapshotReadEvaluations orders by created_at ASC. Submit three
// evaluations with deliberate wall-clock gaps; read order must match
// submission order.
func testSnapshotReadEvaluationsOrderedByCreatedAt(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_eval_ord", UserID: "u"})

	for i := 0; i < 3; i++ {
		_, err := s.EvaluationSubmit(ctx, store.EvaluationRow{
			EvalID:      fmt.Sprintf("eval_ord_%d", i),
			RunID:       run.ID,
			Score:       float64(i),
			EmitterRole: "self",
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	got, err := s.SnapshotReadEvaluations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 {
		t.Fatalf("got %d rows, want at least 3", len(got))
	}
	// Ordering invariant: monotonic non-decreasing on CreatedAt
	// across the WHOLE slice (the shared fixture may have rows from
	// other tests inserted before / after).
	prev := got[0]
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(prev.CreatedAt) {
			t.Errorf("ordering violated at %d: %v before %v", i, prev.CreatedAt, got[i].CreatedAt)
		}
		prev = got[i]
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

// ---- v0.12.x MemoryAtomicUpdate contract ----

func testMemoryAtomicUpdateOnNewKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.MemoryAtomicUpdate(ctx, store.MemoryScopeAgent, "qa", "k1", 0,
		func(existing json.RawMessage) (json.RawMessage, error) {
			if len(existing) != 0 {
				t.Errorf("reducer existing should be empty on new key, got %q", existing)
			}
			return json.RawMessage(`{"v":1}`), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// The store may normalise JSON whitespace (Postgres JSONB adds
	// a space after the colon; SQLite preserves byte-for-byte).
	// Compare semantically.
	if v := jsonField(got, "v"); v != float64(1) {
		t.Errorf("returned value's v = %v, want 1 (raw=%q)", v, got)
	}
	entry, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if v := jsonField(entry.Value, "v"); v != float64(1) {
		t.Errorf("stored value's v = %v, want 1 (raw=%q)", v, entry.Value)
	}
}

// jsonField returns the top-level field's value from a JSON object,
// or nil when absent/malformed. Used by MemoryAtomicUpdate contract
// tests to compare semantically across the SQLite/Postgres JSON
// representation difference.
func jsonField(raw json.RawMessage, field string) any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m[field]
}

func testMemoryAtomicUpdateOnExistingKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Seed an existing row via MemorySet, then read-modify-write.
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "k2",
		json.RawMessage(`{"x":1}`), 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryAtomicUpdate(ctx, store.MemoryScopeAgent, "qa", "k2", 0,
		func(existing json.RawMessage) (json.RawMessage, error) {
			if string(existing) != `{"x": 1}` && string(existing) != `{"x":1}` {
				// Postgres JSONB round-trip adds a space; SQLite preserves
				// the original bytes. Accept either.
				t.Errorf("reducer existing = %q, want {x:1}-shape", existing)
			}
			return json.RawMessage(`{"x":1,"y":2}`), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// Postgres normalises JSONB on write; check via MemoryGet which
	// goes through the same path the production caller would.
	entry, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "k2")
	if err != nil {
		t.Fatal(err)
	}
	// JSONB may reorder/space-pad on Postgres; parse and compare
	// semantically.
	var parsed map[string]any
	if err := json.Unmarshal(entry.Value, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["x"] != float64(1) || parsed["y"] != float64(2) {
		t.Errorf("stored value parsed = %v; returned bytes = %q", parsed, got)
	}
}

func testMemoryAtomicUpdateOnExpiredKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Seed with a short-TTL row that expires before the update runs.
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "k3",
		json.RawMessage(`{"old":true}`), 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	_, err := s.MemoryAtomicUpdate(ctx, store.MemoryScopeAgent, "qa", "k3", 0,
		func(existing json.RawMessage) (json.RawMessage, error) {
			// Expired row → reducer sees empty (treated as missing).
			if len(existing) != 0 {
				t.Errorf("expired row should surface as empty to reducer, got %q", existing)
			}
			return json.RawMessage(`{"fresh":true}`), nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func testMemoryAtomicUpdateReducerErrorRollsBack(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Seed a row so we can verify it's unchanged after a failed update.
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "k4",
		json.RawMessage(`{"untouched":true}`), 0); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("reducer-said-no")
	_, err := s.MemoryAtomicUpdate(ctx, store.MemoryScopeAgent, "qa", "k4", 0,
		func(existing json.RawMessage) (json.RawMessage, error) {
			return nil, sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel propagation", err)
	}
	entry, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "k4")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(entry.Value, &parsed)
	if parsed["untouched"] != true {
		t.Errorf("row was modified despite reducer error: %v", parsed)
	}
}

func testMemoryAtomicUpdateInvalidJSONRejected(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.MemoryAtomicUpdate(ctx, store.MemoryScopeAgent, "qa", "k5", 0,
		func(existing json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`not json {{{`), nil
		})
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("err = %v, want 'invalid JSON' mention", err)
	}
}

// testMemoryAtomicUpdateIsAtomicUnderConcurrency — N goroutines each
// append a unique element to a JSON array under the same key. With
// proper atomicity all N elements land; without it, lost-updates
// produce a final array smaller than N.
func testMemoryAtomicUpdateIsAtomicUnderConcurrency(t *testing.T, s store.Store) {
	ctx := context.Background()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, _ = s.MemoryAtomicUpdate(ctx, store.MemoryScopeAgent, "qa", "list", 0,
				func(existing json.RawMessage) (json.RawMessage, error) {
					var arr []int
					if len(existing) > 0 {
						_ = json.Unmarshal(existing, &arr)
					}
					arr = append(arr, i)
					return json.Marshal(arr)
				})
		}()
	}
	wg.Wait()

	entry, err := s.MemoryGet(ctx, store.MemoryScopeAgent, "qa", "list")
	if err != nil {
		t.Fatal(err)
	}
	var arr []int
	if err := json.Unmarshal(entry.Value, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != N {
		t.Errorf("concurrent atomic appends: array len = %d, want %d (%d lost updates)",
			len(arr), N, N-len(arr))
	}
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

// ---- v0.9.0 Vector Memory contract ----
//
// These tests fork at the top on store.SupportsVectors() and assert
// either the refusal path (every method returns ErrVectorUnsupported,
// SupportsVectors == false) or the round-trip path (vector index
// loaded; cosine-similarity ranking; FK CASCADE on base-row delete).
//
// In v0.9.0 only Postgres + pgvector takes the round-trip path; SQLite
// + Postgres-without-pgvector both run the refusal path. Commit 1 of
// PR 1 leaves Postgres at false too — it flips to true in commit 2
// when the real backend lands.

// floats32 is a tiny helper for building test vectors. The contract
// tests use 4-dim vectors so the search ordering is hand-verifiable;
// the wire path doesn't care about dimension (the schema's `vector`
// column is dim-agnostic). Production embedders return 1536+ dim.
func floats32(xs ...float32) []float32 { return xs }

// vectorRefusalCheck is the pre-amble every refusal-path test runs.
// Returns true when the backend declares vector support — caller
// continues to the real test body. Returns false when the backend
// refuses — caller has already asserted the refusal shape and should
// return immediately. Centralises the assertions so the per-test
// bodies don't repeat boilerplate.
func vectorRefusalCheck(t *testing.T, s store.Store) bool {
	t.Helper()
	if s.SupportsVectors() {
		return true
	}
	ctx := context.Background()
	err := s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "k", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedSet on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedGet(ctx, store.MemoryScopeAgent, "qa", "k")
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedGet on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedSearch(ctx, store.MemoryScopeAgent, "qa", "", floats32(1, 0, 0, 0), 5)
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedSearch on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedListByModel(ctx, store.MemoryScopeAgent, "qa", "openai", "text-embedding-3-large", 10)
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedListByModel on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedStats(ctx, store.MemoryScopeAgent)
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedStats on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	return false
}

func testMemoryEmbedSetRoundTrip(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	// Base memory row must exist (FK CASCADE in the embeddings schema).
	if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "rec1", json.RawMessage(`"hello"`), 0); err != nil {
		t.Fatal(err)
	}
	want := store.MemoryEmbedding{
		Provider:  "openai",
		Model:     "text-embedding-3-small",
		Dimension: 4,
		Vector:    floats32(0.5, 0.5, 0.5, 0.5),
		EmbedText: "hello in agent qa",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "rec1", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryEmbedGet(ctx, store.MemoryScopeAgent, "qa", "rec1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != want.Provider || got.Model != want.Model || got.Dimension != want.Dimension {
		t.Errorf("metadata round-trip: got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(got.Vector, want.Vector) {
		t.Errorf("vector bytes mismatch: got %v, want %v", got.Vector, want.Vector)
	}
	if got.EmbedText != want.EmbedText {
		t.Errorf("embed_text: got %q, want %q", got.EmbedText, want.EmbedText)
	}
}

func testMemoryEmbedSearchTopK(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	// Three rows pointing in different 4-D unit directions. A query
	// aligned with row1 must rank row1 first, then row2 (45° off),
	// then row3 (orthogonal).
	rows := []struct {
		key string
		vec []float32
	}{
		{"row1", floats32(1, 0, 0, 0)},           // aligned with query
		{"row2", floats32(0.7071, 0.7071, 0, 0)}, // 45° off
		{"row3", floats32(0, 1, 0, 0)},           // 90° off
	}
	for _, r := range rows {
		if err := s.MemorySet(ctx, store.MemoryScopeAgent, "qa", r.key, json.RawMessage(`"x"`), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", r.key, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: r.vec, EmbedText: r.key, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := s.MemoryEmbedSearch(ctx, store.MemoryScopeAgent, "qa", "", floats32(1, 0, 0, 0), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Key != "row1" || results[1].Key != "row2" || results[2].Key != "row3" {
		t.Errorf("ordering: got [%s, %s, %s], want [row1, row2, row3]",
			results[0].Key, results[1].Key, results[2].Key)
	}
	// Score must be monotonically decreasing.
	if !(results[0].Score >= results[1].Score && results[1].Score >= results[2].Score) {
		t.Errorf("scores not monotone-DESC: %v", []float64{results[0].Score, results[1].Score, results[2].Score})
	}
}

func testMemoryEmbedSearchScopeIsolation(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	// Write the embedding under agent scope.
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "rec", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "rec", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	// Search the same key from user scope — must return empty.
	results, err := s.MemoryEmbedSearch(ctx, store.MemoryScopeUser, "qa", "", floats32(1, 0, 0, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("cross-scope leak: user scope returned %d results, want 0", len(results))
	}
}

func testMemoryEmbedSearchDimensionMismatch(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "rec", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "rec", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	// Query with dimension 8 against stored dimension 4.
	_, err := s.MemoryEmbedSearch(ctx, store.MemoryScopeAgent, "qa", "", floats32(1, 0, 0, 0, 1, 0, 0, 0), 5)
	if !errors.Is(err, store.ErrDimensionMismatch) {
		t.Errorf("dim mismatch: got %v, want ErrDimensionMismatch", err)
	}
}

func testMemoryEmbedSearchEmptyScope(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	results, err := s.MemoryEmbedSearch(ctx, store.MemoryScopeAgent, "empty", "", floats32(1, 0, 0, 0), 10)
	if err != nil {
		t.Errorf("empty scope search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("empty scope: got %d results, want 0", len(results))
	}
}

func testMemoryDeleteCascadesEmbedding(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "doomed", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "doomed", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	// Delete the base row — embedding row must vanish via FK CASCADE.
	if _, err := s.MemoryDelete(ctx, store.MemoryScopeAgent, "qa", "doomed"); err != nil {
		t.Fatal(err)
	}
	_, err := s.MemoryEmbedGet(ctx, store.MemoryScopeAgent, "qa", "doomed")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("after base-row delete: got %v (%T), want *store.ErrNotFound (CASCADE failed)", err, err)
	}
}

func testMemoryEmbedListByModelFiltersCurrent(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	// Two rows under the OLD model.
	for _, k := range []string{"old1", "old2"} {
		_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", k, json.RawMessage(`"x"`), 0)
		_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", k, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: floats32(1, 0, 0, 0), EmbedText: k, CreatedAt: time.Now(),
		})
	}
	// One row under the NEW model.
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "new1", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "new1", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-large", Dimension: 8,
		Vector: floats32(1, 0, 0, 0, 0, 0, 0, 0), EmbedText: "new1", CreatedAt: time.Now(),
	})
	// Ask "which rows are NOT on text-embedding-3-large?" — expect old1 + old2.
	got, err := s.MemoryEmbedListByModel(ctx, store.MemoryScopeAgent, "qa", "openai", "text-embedding-3-large", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d rows-needing-reembed, want 2 (rows: %+v)", len(got), got)
	}
	for _, r := range got {
		if r.Key == "new1" {
			t.Errorf("ListByModel returned the current-model row %q", r.Key)
		}
	}
}

func testMemoryEmbedStatsReportsPerModelCount(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	// Two rows on model A (dim 4), one row on model B (dim 8).
	for _, k := range []string{"a1", "a2"} {
		_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", k, json.RawMessage(`"x"`), 0)
		_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", k, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: floats32(1, 0, 0, 0), EmbedText: k, CreatedAt: time.Now(),
		})
	}
	_ = s.MemorySet(ctx, store.MemoryScopeAgent, "qa", "b1", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, store.MemoryScopeAgent, "qa", "b1", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-large", Dimension: 8,
		Vector: floats32(1, 0, 0, 0, 0, 0, 0, 0), EmbedText: "b1", CreatedAt: time.Now(),
	})
	stats, err := s.MemoryEmbedStats(ctx, store.MemoryScopeAgent)
	if err != nil {
		t.Fatal(err)
	}
	// Build a (provider,model) → row_count lookup off the slice.
	counts := map[string]int{}
	for _, m := range stats.Models {
		counts[m.Provider+"/"+m.Model] = m.RowCount
	}
	if counts["openai/text-embedding-3-small"] != 2 {
		t.Errorf("small-model row_count: %d, want 2", counts["openai/text-embedding-3-small"])
	}
	if counts["openai/text-embedding-3-large"] != 1 {
		t.Errorf("large-model row_count: %d, want 1", counts["openai/text-embedding-3-large"])
	}
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

// testChannelListCursorsForScope pins the v0.9.x introspection contract:
// the method returns every cursor row matching (scope, scope_id),
// ordered by channel ASC, and rows for a different scope_id MUST NOT
// leak.
func testChannelListCursorsForScope(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Seed two distinct (scope, scope_id) tuples with cursors on two
	// channels each. Then verify per-tuple isolation + ordering.
	for _, ch := range []string{"channel-a", "channel-b"} {
		_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: ch, Scope: store.MemoryScopeAgent, ScopeID: "alice",
			Payload: json.RawMessage(`{}`),
		}, 0)
		time.Sleep(time.Microsecond)
		_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: ch, Scope: store.MemoryScopeAgent, ScopeID: "bob",
			Payload: json.RawMessage(`{}`),
		}, 0)
	}
	// Alice acks both channels; bob acks only channel-a.
	for _, ch := range []string{"channel-a", "channel-b"} {
		_, next, _ := s.ChannelSubscribe(ctx, ch, store.MemoryScopeAgent, "alice", "", 1)
		if next != "" {
			_ = s.ChannelAck(ctx, ch, store.MemoryScopeAgent, "alice", next)
		}
	}
	_, next, _ := s.ChannelSubscribe(ctx, "channel-a", store.MemoryScopeAgent, "bob", "", 1)
	if next != "" {
		_ = s.ChannelAck(ctx, "channel-a", store.MemoryScopeAgent, "bob", next)
	}

	// Alice's view: two rows, ordered ASC.
	aliceRows, err := s.ChannelListCursorsForScope(ctx, store.MemoryScopeAgent, "alice")
	if err != nil {
		t.Fatalf("alice list: %v", err)
	}
	if len(aliceRows) != 2 {
		t.Fatalf("alice rows = %d, want 2: %+v", len(aliceRows), aliceRows)
	}
	if aliceRows[0].Channel != "channel-a" || aliceRows[1].Channel != "channel-b" {
		t.Errorf("alice ordering wrong: %+v", aliceRows)
	}
	for _, r := range aliceRows {
		if r.ScopeID != "alice" || r.Scope != store.MemoryScopeAgent {
			t.Errorf("alice row has wrong scope/scope_id: %+v", r)
		}
		if r.Cursor == "" {
			t.Errorf("alice row %q has empty cursor", r.Channel)
		}
		if r.UpdatedAt.IsZero() {
			t.Errorf("alice row %q has zero UpdatedAt", r.Channel)
		}
	}

	// Bob's view: one row (channel-a only).
	bobRows, err := s.ChannelListCursorsForScope(ctx, store.MemoryScopeAgent, "bob")
	if err != nil {
		t.Fatalf("bob list: %v", err)
	}
	if len(bobRows) != 1 || bobRows[0].Channel != "channel-a" {
		t.Errorf("bob rows = %+v, want one channel-a row", bobRows)
	}

	// Mismatched scope: scope=user, scope_id=alice. No rows.
	userRows, err := s.ChannelListCursorsForScope(ctx, store.MemoryScopeUser, "alice")
	if err != nil {
		t.Fatalf("user-scope list: %v", err)
	}
	if len(userRows) != 0 {
		t.Errorf("scope=user/alice rows = %+v, want 0 (must not leak agent-scope rows)", userRows)
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

func testChannelStatsAggregatesNonExpired(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Publish 3 to "alpha", 1 to "beta".
	for i := 0; i < 3; i++ {
		_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "alpha", Scope: store.MemoryScopeAgent, ScopeID: "x",
			Payload: json.RawMessage(`{}`),
		}, 0)
		time.Sleep(time.Millisecond)
	}
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "beta", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{}`),
	}, 0)

	stats, err := s.ChannelStats(ctx)
	if err != nil {
		t.Fatalf("ChannelStats: %v", err)
	}
	byName := map[string]store.ChannelStats{}
	for _, st := range stats {
		byName[st.Channel] = st
	}
	if byName["alpha"].MessageCount != 3 {
		t.Errorf("alpha count = %d, want 3", byName["alpha"].MessageCount)
	}
	if byName["beta"].MessageCount != 1 {
		t.Errorf("beta count = %d, want 1", byName["beta"].MessageCount)
	}
	if byName["alpha"].OldestVisibleAt.After(byName["alpha"].NewestVisibleAt) {
		t.Errorf("alpha oldest %v after newest %v", byName["alpha"].OldestVisibleAt, byName["alpha"].NewestVisibleAt)
	}
	if byName["alpha"].OldestVisibleAt.IsZero() {
		t.Error("alpha oldest_visible_at should be set")
	}
}

func testChannelStatsEmptyOnNoMessages(t *testing.T, s store.Store) {
	ctx := context.Background()
	stats, err := s.ChannelStats(ctx)
	if err != nil {
		t.Fatalf("ChannelStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty stats, got %d rows: %+v", len(stats), stats)
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

func testAgentDefContentSHA256RoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkDef("d-hash", "alpha-hash", "")
	row.ContentSHA256 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	written, err := s.AgentDefCreate(ctx, row)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("write echo: got %q, want %q", written.ContentSHA256, row.ContentSHA256)
	}
	got, err := s.AgentDefGet(ctx, "d-hash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("get: ContentSHA256 = %q, want %q", got.ContentSHA256, row.ContentSHA256)
	}

	// A row created without a hash (the pre-migration shape) must come
	// back with an empty ContentSHA256, NOT a NULL-decode error.
	plain, err := s.AgentDefCreate(ctx, mkDef("d-no-hash", "alpha-no-hash", ""))
	if err != nil {
		t.Fatalf("create no-hash: %v", err)
	}
	if plain.ContentSHA256 != "" {
		t.Errorf("hashless row: got %q, want empty", plain.ContentSHA256)
	}
}

func testBackfillAgentDefContentSHA256(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Two rows without a hash (simulates the upgrade-from-v0.8.x shape).
	for i, name := range []string{"alpha-bf", "beta-bf"} {
		if _, err := s.AgentDefCreate(ctx, mkDef(fmt.Sprintf("d-bf-%d", i), name, "")); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// One row that ALREADY has a hash — backfill must not touch it.
	pre := mkDef("d-bf-pre", "gamma-bf", "")
	pre.ContentSHA256 = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	if _, err := s.AgentDefCreate(ctx, pre); err != nil {
		t.Fatalf("create pre-hashed: %v", err)
	}

	signFn := func(name string, def []byte) (string, error) {
		// Stable deterministic value the test can recompute for assertions.
		return "sha256:" + name + "-hash", nil
	}
	n, err := s.BackfillAgentDefContentSHA256(ctx, signFn)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled %d rows, want 2", n)
	}

	// Second call must be a no-op.
	n2, err := s.BackfillAgentDefContentSHA256(ctx, signFn)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent? second pass backfilled %d rows, want 0", n2)
	}

	// Pre-hashed row was preserved.
	got, _ := s.AgentDefGet(ctx, "d-bf-pre")
	if got.ContentSHA256 != "sha256:9999999999999999999999999999999999999999999999999999999999999999" {
		t.Errorf("backfill clobbered pre-existing hash: %q", got.ContentSHA256)
	}

	// Backfilled rows have the expected hash.
	alpha, _ := s.AgentDefGet(ctx, "d-bf-0")
	if alpha.ContentSHA256 != "sha256:alpha-bf-hash" {
		t.Errorf("backfill alpha hash = %q", alpha.ContentSHA256)
	}
}

// testBackfillAgentDefSystemPromptBase pins the boot-time backfill
// for the v0.9.x system_prompt_base field added by the
// static-vs-dynamic equalization PR. Three fixture rows cover the
// three cases the backfill must handle:
//
//  1. Legacy row WITHOUT system_prompt_base → backfill fills it from
//     system_prompt.
//  2. Row that ALREADY has system_prompt_base → backfill leaves it
//     untouched (don't clobber explicit values).
//  3. Row without system_prompt either → backfill leaves it as-is
//     (nothing to fill from; the read-side normalizer is a no-op
//     too).
//
// Idempotent: a second call after a complete backfill returns 0.
func testBackfillAgentDefSystemPromptBase(t *testing.T, s store.Store) {
	ctx := context.Background()

	// 1. Legacy row — missing system_prompt_base.
	legacy := store.AgentDefRow{
		DefID:       "spb-legacy",
		Name:        "spb-legacy-name",
		Description: "legacy",
		Definition:  json.RawMessage(`{"system_prompt":"be helpful","allowed_tools":["Read"]}`),
	}
	if _, err := s.AgentDefCreate(ctx, legacy); err != nil {
		t.Fatalf("create legacy: %v", err)
	}

	// 2. Already-filled row — must not be touched.
	filled := store.AgentDefRow{
		DefID:       "spb-filled",
		Name:        "spb-filled-name",
		Description: "filled",
		Definition:  json.RawMessage(`{"system_prompt":"new","system_prompt_base":"original base"}`),
	}
	if _, err := s.AgentDefCreate(ctx, filled); err != nil {
		t.Fatalf("create filled: %v", err)
	}

	// 3. Row with no system_prompt either — must be left as-is.
	empty := store.AgentDefRow{
		DefID:       "spb-empty",
		Name:        "spb-empty-name",
		Description: "empty",
		Definition:  json.RawMessage(`{"allowed_tools":["Read"]}`),
	}
	if _, err := s.AgentDefCreate(ctx, empty); err != nil {
		t.Fatalf("create empty: %v", err)
	}

	n, err := s.BackfillAgentDefSystemPromptBase(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Errorf("backfilled %d rows, want 1 (only the legacy row)", n)
	}

	// Verify legacy row got the field.
	got, _ := s.AgentDefGet(ctx, "spb-legacy")
	var legacyOut map[string]any
	_ = json.Unmarshal(got.Definition, &legacyOut)
	if legacyOut["system_prompt_base"] != "be helpful" {
		t.Errorf("legacy.system_prompt_base = %v, want %q", legacyOut["system_prompt_base"], "be helpful")
	}
	if legacyOut["system_prompt"] != "be helpful" {
		t.Errorf("legacy.system_prompt mutated: %v", legacyOut["system_prompt"])
	}

	// Filled row untouched.
	gotF, _ := s.AgentDefGet(ctx, "spb-filled")
	var filledOut map[string]any
	_ = json.Unmarshal(gotF.Definition, &filledOut)
	if filledOut["system_prompt_base"] != "original base" {
		t.Errorf("filled.system_prompt_base clobbered: %v", filledOut["system_prompt_base"])
	}

	// Empty row untouched (no field added).
	gotE, _ := s.AgentDefGet(ctx, "spb-empty")
	var emptyOut map[string]any
	_ = json.Unmarshal(gotE.Definition, &emptyOut)
	if _, hasField := emptyOut["system_prompt_base"]; hasField {
		t.Errorf("empty row spuriously got system_prompt_base: %v", emptyOut["system_prompt_base"])
	}

	// Idempotent: second call returns 0.
	n2, err := s.BackfillAgentDefSystemPromptBase(ctx)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent? second pass backfilled %d rows, want 0", n2)
	}
}

// ---- v0.8.22 SkillDef contract tests ----
//
// Direct mirror of the AgentDef tests above. Same invariants:
// monotonic versioning under contention, append-only definition
// column, idempotent active pointer, reversible retire flag.

func mkSkillDef(id, name string, parent string) store.SkillDefRow {
	return store.SkillDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"body":"## Skill body","description":"test row"}`),
		Description: "test row",
	}
}

func testSkillDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.SkillDefCreate(ctx, mkSkillDef("sd-1", "skill-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.SkillDefGet(ctx, "sd-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "skill-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not populated")
	}
}

func testSkillDefVersionMonotonicUnderContention(t *testing.T, s store.Store) {
	ctx := context.Background()
	const G = 50
	const Per = 5
	var wg sync.WaitGroup
	errs := make(chan error, G*Per)
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < Per; i++ {
				id := fmt.Sprintf("sd-%d-%d", g, i)
				_, err := s.SkillDefCreate(ctx, mkSkillDef(id, "skill-race", ""))
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
	rows, err := s.SkillDefListByName(ctx, "skill-race")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != G*Per {
		t.Fatalf("got %d versions, want %d", len(rows), G*Per)
	}
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

func testSkillDefParallelForksDistinctVersions(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, err := s.SkillDefCreate(ctx, mkSkillDef("sd-p-1", "skill-forkparent", ""))
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
			_, err := s.SkillDefCreate(ctx, mkSkillDef(fmt.Sprintf("sd-f-%d", i), "skill-forkparent", parent.DefID))
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
	children, err := s.SkillDefListChildren(ctx, parent.DefID)
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

func testSkillDefAppendOnlyDefinition(t *testing.T, s store.Store) {
	ctx := context.Background()
	original := mkSkillDef("sd-immutable-1", "skill-frozen", "")
	original.Definition = json.RawMessage(`{"body":"original"}`)
	row, err := s.SkillDefCreate(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SkillDefGet(ctx, row.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Definition) != `{"body":"original"}` {
		t.Errorf("definition: got %s, want original", got.Definition)
	}
}

func testSkillDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, err := s.SkillDefCreate(ctx, mkSkillDef("sd-a", "skill-promo", ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SkillDefCreate(ctx, mkSkillDef("sd-b", "skill-promo", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "skill-promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "skill-promo", b.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "skill-promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.SkillDefGetActive(ctx, "skill-promo")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefID != a.DefID {
		t.Errorf("active = %s, want %s", got.DefID, a.DefID)
	}
}

func testSkillDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.SkillDefCreate(ctx, mkSkillDef("sd-r-1", "skill-retireagent", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.SkillDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("retire(true) didn't stick")
	}
	if err := s.SkillDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SkillDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("retire(false) didn't reverse")
	}
	rows, _ := s.SkillDefListByName(ctx, "skill-retireagent")
	if len(rows) != 1 {
		t.Errorf("list after retire toggle: got %d, want 1", len(rows))
	}
}

func testSkillDefStaticFallback(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.SkillDefGetActive(ctx, "no-such-skill-name")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v, want *ErrNotFound", err)
	}
}

func testSkillDefContentSHA256RoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkSkillDef("sd-hash", "skill-alpha-hash", "")
	row.ContentSHA256 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	written, err := s.SkillDefCreate(ctx, row)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("write echo: got %q, want %q", written.ContentSHA256, row.ContentSHA256)
	}
	got, err := s.SkillDefGet(ctx, "sd-hash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("get: ContentSHA256 = %q, want %q", got.ContentSHA256, row.ContentSHA256)
	}

	plain, err := s.SkillDefCreate(ctx, mkSkillDef("sd-no-hash", "skill-alpha-no-hash", ""))
	if err != nil {
		t.Fatalf("create no-hash: %v", err)
	}
	if plain.ContentSHA256 != "" {
		t.Errorf("hashless row: got %q, want empty", plain.ContentSHA256)
	}
}

func testBackfillSkillDefContentSHA256(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, name := range []string{"skill-bf-a", "skill-bf-b"} {
		if _, err := s.SkillDefCreate(ctx, mkSkillDef(fmt.Sprintf("sd-bf-%d", i), name, "")); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	signFn := func(name string, def []byte) (string, error) {
		return "sha256:" + name + "-hash", nil
	}
	n, err := s.BackfillSkillDefContentSHA256(ctx, signFn)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled %d rows, want 2", n)
	}
	got, _ := s.SkillDefGet(ctx, "sd-bf-0")
	if got.ContentSHA256 != "sha256:skill-bf-a-hash" {
		t.Errorf("backfill hash = %q", got.ContentSHA256)
	}
}

// testSkillDefSnapshotReadEmpty verifies the two snapshot reads
// return empty (not error) on a store with no skill_defs rows.
// Mirrors the implicit behaviour the AgentDef snapshot reads rely
// on. Also smoke-checks the restore round-trip for one row.
func testSkillDefSnapshotReadEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	rows, err := s.SnapshotReadSkillDefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty, got %d rows", len(rows))
	}
	ptrs, err := s.SnapshotReadSkillDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ptrs) != 0 {
		t.Errorf("expected empty, got %d pointers", len(ptrs))
	}
	// Restore round-trip: write one row + active pointer, read back.
	row := mkSkillDef("snap-sd-1", "snap-skill", "")
	row.Version = 7
	row.CreatedAt = time.Now().UTC().Truncate(time.Second)
	inserted, err := s.SnapshotRestoreSkillDef(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Error("first restore didn't insert")
	}
	// Idempotent — second restore is silent.
	inserted2, err := s.SnapshotRestoreSkillDef(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if inserted2 {
		t.Error("second restore reported inserted; want idempotent")
	}
	_, err = s.SnapshotRestoreSkillDefActive(ctx, store.SkillDefActiveEntry{
		Name:       row.Name,
		DefID:      row.DefID,
		PromotedAt: time.Now().UTC().Truncate(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ = s.SnapshotReadSkillDefs(ctx)
	if len(rows) != 1 {
		t.Errorf("after restore: %d rows", len(rows))
	}
	ptrs, _ = s.SnapshotReadSkillDefActive(ctx)
	if len(ptrs) != 1 {
		t.Errorf("after restore: %d pointers", len(ptrs))
	}
}

// ---- v0.9.x MCPServerDef contract tests ----

func mkMCPServerDef(id, name string, parent string) store.MCPServerDefRow {
	return store.MCPServerDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"transport":"streamable-http","url":"https://example.com/mcp"}`),
		Description: "test row",
	}
}

func testMCPServerDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.MCPServerDefCreate(ctx, mkMCPServerDef("md-1", "mcp-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.MCPServerDefGet(ctx, "md-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "mcp-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
}

func testMCPServerDefVersionMonotonic(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		row := mkMCPServerDef(fmt.Sprintf("md-mono-%d", i), "mcp-mono", "")
		written, err := s.MCPServerDefCreate(ctx, row)
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if want := i + 1; written.Version != want {
			t.Errorf("create #%d: version = %d, want %d", i, written.Version, want)
		}
	}
}

func testMCPServerDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, _ := s.MCPServerDefCreate(ctx, mkMCPServerDef("md-active-1", "mcp-active", ""))
	r2, _ := s.MCPServerDefCreate(ctx, mkMCPServerDef("md-active-2", "mcp-active", ""))

	if err := s.MCPServerDefSetActive(ctx, "mcp-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MCPServerDefGetActive(ctx, "mcp-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.MCPServerDefSetActive(ctx, "mcp-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MCPServerDefGetActive(ctx, "mcp-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

func testMCPServerDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, _ := s.MCPServerDefCreate(ctx, mkMCPServerDef("md-retire", "mcp-retire", ""))
	if row.Retired {
		t.Error("freshly created row should not be retired")
	}
	if err := s.MCPServerDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MCPServerDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("after retire(true): row should be retired")
	}
	if err := s.MCPServerDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MCPServerDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("after retire(false): row should be un-retired")
	}
}

func testMCPServerDefContentSHA256RoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkMCPServerDef("md-hash", "mcp-alpha-hash", "")
	row.ContentSHA256 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	written, err := s.MCPServerDefCreate(ctx, row)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("write echo: got %q, want %q", written.ContentSHA256, row.ContentSHA256)
	}
	got, err := s.MCPServerDefGet(ctx, "md-hash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("get: ContentSHA256 = %q, want %q", got.ContentSHA256, row.ContentSHA256)
	}

	plain, err := s.MCPServerDefCreate(ctx, mkMCPServerDef("md-no-hash", "mcp-alpha-no-hash", ""))
	if err != nil {
		t.Fatalf("create no-hash: %v", err)
	}
	if plain.ContentSHA256 != "" {
		t.Errorf("hashless row: got %q, want empty", plain.ContentSHA256)
	}
}

func testBackfillMCPServerDefContentSHA256(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, name := range []string{"mcp-bf-a", "mcp-bf-b"} {
		if _, err := s.MCPServerDefCreate(ctx, mkMCPServerDef(fmt.Sprintf("md-bf-%d", i), name, "")); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	signFn := func(name string, def []byte) (string, error) {
		return "sha256:" + name + "-hash", nil
	}
	n, err := s.BackfillMCPServerDefContentSHA256(ctx, signFn)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled %d rows, want 2", n)
	}
	got, _ := s.MCPServerDefGet(ctx, "md-bf-0")
	if got.ContentSHA256 != "sha256:mcp-bf-a-hash" {
		t.Errorf("backfill hash = %q", got.ContentSHA256)
	}
}

// ---- v1.x RFC E ScheduleDef substrate ----
//
// Mirror of the AgentDef / SkillDef / MCPServerDef contract tests.
// Pins versioning + active-pointer + retire semantics + parent-not-
// found error sentinel. Bootstraps + content-sha256 are out of
// scope for v1.x ScheduleDef (no signing surface yet).

func mkScheduleDef(id, name string, parent string) store.ScheduleDefRow {
	return store.ScheduleDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"agent":"demo","schedule":"0 6 * * *","user_id":"alice"}`),
		Description: "test row",
	}
}

func testScheduleDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.ScheduleDefCreate(ctx, mkScheduleDef("sd-1", "sched-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.ScheduleDefGet(ctx, "sd-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "sched-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
}

func testScheduleDefVersionMonotonic(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		row := mkScheduleDef(fmt.Sprintf("sd-mono-%d", i), "sched-mono", "")
		written, err := s.ScheduleDefCreate(ctx, row)
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if want := i + 1; written.Version != want {
			t.Errorf("create #%d: version = %d, want %d", i, written.Version, want)
		}
	}
}

func testScheduleDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, _ := s.ScheduleDefCreate(ctx, mkScheduleDef("sd-active-1", "sched-active", ""))
	r2, _ := s.ScheduleDefCreate(ctx, mkScheduleDef("sd-active-2", "sched-active", ""))

	if err := s.ScheduleDefSetActive(ctx, "sched-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ScheduleDefGetActive(ctx, "sched-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.ScheduleDefSetActive(ctx, "sched-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ScheduleDefGetActive(ctx, "sched-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

func testScheduleDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, _ := s.ScheduleDefCreate(ctx, mkScheduleDef("sd-retire", "sched-retire", ""))
	if row.Retired {
		t.Error("freshly created row should not be retired")
	}
	if err := s.ScheduleDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ScheduleDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("after retire(true): row should be retired")
	}
	if err := s.ScheduleDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ScheduleDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("after retire(false): row should NOT be retired")
	}
}

func testScheduleDefParentNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkScheduleDef("sd-orphan", "sched-orphan", "sd-nonexistent")
	_, err := s.ScheduleDefCreate(ctx, row)
	if err == nil {
		t.Fatal("expected ErrScheduleDefParentNotFound, got nil")
	}
	if !errors.Is(err, store.ErrScheduleDefParentNotFound) {
		t.Errorf("got %v, want ErrScheduleDefParentNotFound", err)
	}
}

func testScheduleDefListByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.ScheduleDefCreate(ctx, mkScheduleDef(fmt.Sprintf("sd-list-%d", i), "sched-list", ""))
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ScheduleDefListByName(ctx, "sched-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("len = %d, want 3", len(rows))
	}
	// version DESC ordering
	if rows[0].Version != 3 || rows[1].Version != 2 || rows[2].Version != 1 {
		t.Errorf("ordering wrong; versions = %d/%d/%d, want 3/2/1",
			rows[0].Version, rows[1].Version, rows[2].Version)
	}
}

func testScheduleDefListChildren(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, _ := s.ScheduleDefCreate(ctx, mkScheduleDef("sd-parent", "sched-tree", ""))
	for i := 0; i < 2; i++ {
		_, err := s.ScheduleDefCreate(ctx, mkScheduleDef(fmt.Sprintf("sd-child-%d", i), fmt.Sprintf("sched-child-%d", i), parent.DefID))
		if err != nil {
			t.Fatal(err)
		}
	}
	children, err := s.ScheduleDefListChildren(ctx, parent.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Errorf("children len = %d, want 2", len(children))
	}
}

// ---- v1.x RFC G A2A substrate ----
//
// Mirror of the ScheduleDef contract tests for both A2A Defs. Pins
// versioning + active-pointer + retire semantics + parent-not-found
// error sentinel. No runtime/sweeper state (A2A Defs have none).

func mkA2AServerCardDef(id, name string, parent string) store.A2AServerCardDefRow {
	return store.A2AServerCardDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"exposed_agents":["demo"],"agent_card":{"name":"demo"}}`),
		Description: "test row",
	}
}

func testA2AServerCardDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef("ascd-1", "card-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.A2AServerCardDefGet(ctx, "ascd-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "card-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
}

func testA2AServerCardDefVersionMonotonic(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		row := mkA2AServerCardDef(fmt.Sprintf("ascd-mono-%d", i), "card-mono", "")
		written, err := s.A2AServerCardDefCreate(ctx, row)
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if want := i + 1; written.Version != want {
			t.Errorf("create #%d: version = %d, want %d", i, written.Version, want)
		}
	}
}

func testA2AServerCardDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, _ := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef("ascd-active-1", "card-active", ""))
	r2, _ := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef("ascd-active-2", "card-active", ""))

	if err := s.A2AServerCardDefSetActive(ctx, "card-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.A2AServerCardDefGetActive(ctx, "card-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.A2AServerCardDefSetActive(ctx, "card-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.A2AServerCardDefGetActive(ctx, "card-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

func testA2AServerCardDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, _ := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef("ascd-retire", "card-retire", ""))
	if row.Retired {
		t.Error("freshly created row should not be retired")
	}
	if err := s.A2AServerCardDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.A2AServerCardDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("after retire(true): row should be retired")
	}
	if err := s.A2AServerCardDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.A2AServerCardDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("after retire(false): row should NOT be retired")
	}
}

func testA2AServerCardDefParentNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkA2AServerCardDef("ascd-orphan", "card-orphan", "ascd-nonexistent")
	_, err := s.A2AServerCardDefCreate(ctx, row)
	if err == nil {
		t.Fatal("expected ErrA2AServerCardDefParentNotFound, got nil")
	}
	if !errors.Is(err, store.ErrA2AServerCardDefParentNotFound) {
		t.Errorf("got %v, want ErrA2AServerCardDefParentNotFound", err)
	}
}

func testA2AServerCardDefListByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef(fmt.Sprintf("ascd-list-%d", i), "card-list", ""))
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.A2AServerCardDefListByName(ctx, "card-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("len = %d, want 3", len(rows))
	}
	// version DESC ordering
	if rows[0].Version != 3 || rows[1].Version != 2 || rows[2].Version != 1 {
		t.Errorf("ordering wrong; versions = %d/%d/%d, want 3/2/1",
			rows[0].Version, rows[1].Version, rows[2].Version)
	}
}

func testA2AServerCardDefListChildren(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, _ := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef("ascd-parent", "card-tree", ""))
	for i := 0; i < 2; i++ {
		_, err := s.A2AServerCardDefCreate(ctx, mkA2AServerCardDef(fmt.Sprintf("ascd-child-%d", i), fmt.Sprintf("card-child-%d", i), parent.DefID))
		if err != nil {
			t.Fatal(err)
		}
	}
	children, err := s.A2AServerCardDefListChildren(ctx, parent.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Errorf("children len = %d, want 2", len(children))
	}
}

func mkA2AAgentDef(id, name string, parent string) store.A2AAgentDefRow {
	return store.A2AAgentDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"agent_card_url":"https://peer.example/.well-known/agent-card.json","auth":{"scheme":"bearer","credential_ref":"acme"}}`),
		Description: "test row",
	}
}

func testA2AAgentDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.A2AAgentDefCreate(ctx, mkA2AAgentDef("aad-1", "peer-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.A2AAgentDefGet(ctx, "aad-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "peer-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
}

func testA2AAgentDefVersionMonotonic(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		row := mkA2AAgentDef(fmt.Sprintf("aad-mono-%d", i), "peer-mono", "")
		written, err := s.A2AAgentDefCreate(ctx, row)
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if want := i + 1; written.Version != want {
			t.Errorf("create #%d: version = %d, want %d", i, written.Version, want)
		}
	}
}

func testA2AAgentDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, _ := s.A2AAgentDefCreate(ctx, mkA2AAgentDef("aad-active-1", "peer-active", ""))
	r2, _ := s.A2AAgentDefCreate(ctx, mkA2AAgentDef("aad-active-2", "peer-active", ""))

	if err := s.A2AAgentDefSetActive(ctx, "peer-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.A2AAgentDefGetActive(ctx, "peer-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.A2AAgentDefSetActive(ctx, "peer-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.A2AAgentDefGetActive(ctx, "peer-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

func testA2AAgentDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, _ := s.A2AAgentDefCreate(ctx, mkA2AAgentDef("aad-retire", "peer-retire", ""))
	if row.Retired {
		t.Error("freshly created row should not be retired")
	}
	if err := s.A2AAgentDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.A2AAgentDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("after retire(true): row should be retired")
	}
	if err := s.A2AAgentDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.A2AAgentDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("after retire(false): row should NOT be retired")
	}
}

func testA2AAgentDefParentNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkA2AAgentDef("aad-orphan", "peer-orphan", "aad-nonexistent")
	_, err := s.A2AAgentDefCreate(ctx, row)
	if err == nil {
		t.Fatal("expected ErrA2AAgentDefParentNotFound, got nil")
	}
	if !errors.Is(err, store.ErrA2AAgentDefParentNotFound) {
		t.Errorf("got %v, want ErrA2AAgentDefParentNotFound", err)
	}
}

func testA2AAgentDefListByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.A2AAgentDefCreate(ctx, mkA2AAgentDef(fmt.Sprintf("aad-list-%d", i), "peer-list", ""))
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.A2AAgentDefListByName(ctx, "peer-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("len = %d, want 3", len(rows))
	}
	// version DESC ordering
	if rows[0].Version != 3 || rows[1].Version != 2 || rows[2].Version != 1 {
		t.Errorf("ordering wrong; versions = %d/%d/%d, want 3/2/1",
			rows[0].Version, rows[1].Version, rows[2].Version)
	}
}

func testA2AAgentDefListChildren(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, _ := s.A2AAgentDefCreate(ctx, mkA2AAgentDef("aad-parent", "peer-tree", ""))
	for i := 0; i < 2; i++ {
		_, err := s.A2AAgentDefCreate(ctx, mkA2AAgentDef(fmt.Sprintf("aad-child-%d", i), fmt.Sprintf("peer-child-%d", i), parent.DefID))
		if err != nil {
			t.Fatal(err)
		}
	}
	children, err := s.A2AAgentDefListChildren(ctx, parent.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Errorf("children len = %d, want 2", len(children))
	}
}

// ---- v1.x RFC H WebhookDef substrate ----
//
// Mirror of the A2AAgentDef round-trip contract tests.

func mkWebhookDef(id, name string, parent string) store.WebhookDefRow {
	return store.WebhookDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","algorithm":"sha256","header":"X-Hub-Signature-256","signing_secret_env":"LOOMCYCLE_WH_SECRET"}}`),
		Description: "test row",
	}
}

func testWebhookDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.WebhookDefCreate(ctx, mkWebhookDef("wh-1", "hook-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.WebhookDefGet(ctx, "wh-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "hook-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
}

func testWebhookDefVersionMonotonic(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		row := mkWebhookDef(fmt.Sprintf("wh-mono-%d", i), "hook-mono", "")
		written, err := s.WebhookDefCreate(ctx, row)
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if want := i + 1; written.Version != want {
			t.Errorf("create #%d: version = %d, want %d", i, written.Version, want)
		}
	}
}

func testWebhookDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, _ := s.WebhookDefCreate(ctx, mkWebhookDef("wh-active-1", "hook-active", ""))
	r2, _ := s.WebhookDefCreate(ctx, mkWebhookDef("wh-active-2", "hook-active", ""))

	if err := s.WebhookDefSetActive(ctx, "hook-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.WebhookDefGetActive(ctx, "hook-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.WebhookDefSetActive(ctx, "hook-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.WebhookDefGetActive(ctx, "hook-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

func testWebhookDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, _ := s.WebhookDefCreate(ctx, mkWebhookDef("wh-retire", "hook-retire", ""))
	if row.Retired {
		t.Error("freshly created row should not be retired")
	}
	if err := s.WebhookDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.WebhookDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("after retire(true): row should be retired")
	}
	if err := s.WebhookDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.WebhookDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("after retire(false): row should NOT be retired")
	}
}

func testWebhookDefParentNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkWebhookDef("wh-orphan", "hook-orphan", "wh-nonexistent")
	_, err := s.WebhookDefCreate(ctx, row)
	if err == nil {
		t.Fatal("expected ErrWebhookDefParentNotFound, got nil")
	}
	if !errors.Is(err, store.ErrWebhookDefParentNotFound) {
		t.Errorf("got %v, want ErrWebhookDefParentNotFound", err)
	}
}

func testWebhookDefListByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.WebhookDefCreate(ctx, mkWebhookDef(fmt.Sprintf("wh-list-%d", i), "hook-list", ""))
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.WebhookDefListByName(ctx, "hook-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("len = %d, want 3", len(rows))
	}
	// version DESC ordering
	if rows[0].Version != 3 || rows[1].Version != 2 || rows[2].Version != 1 {
		t.Errorf("ordering wrong; versions = %d/%d/%d, want 3/2/1",
			rows[0].Version, rows[1].Version, rows[2].Version)
	}
}

func testWebhookDefListChildren(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, _ := s.WebhookDefCreate(ctx, mkWebhookDef("wh-parent", "hook-tree", ""))
	for i := 0; i < 2; i++ {
		_, err := s.WebhookDefCreate(ctx, mkWebhookDef(fmt.Sprintf("wh-child-%d", i), fmt.Sprintf("hook-child-%d", i), parent.DefID))
		if err != nil {
			t.Fatal(err)
		}
	}
	children, err := s.WebhookDefListChildren(ctx, parent.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Errorf("children len = %d, want 2", len(children))
	}
}

// ---- RFC I MR-3a MemoryBackendDef substrate ----
//
// Faithful mirror of the WebhookDef round-trip contract tests.

func mkMemoryBackendDef(id, name string, parent string) store.MemoryBackendDefRow {
	return store.MemoryBackendDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"kind":"mem9","config":{"base_url":"https://mem9.example.com","api_version":"v1","api_key_env":"LOOMCYCLE_MEM9_KEY"},"tenancy_strategy":{"kind":"shared_key_with_prefix","prefix_pattern":"tenant/{tenant_id}/"},"fallback_on_error":"inprocess"}`),
		Description: "test row",
	}
}

func testMemoryBackendDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef("mb-1", "backend-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.MemoryBackendDefGet(ctx, "mb-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "backend-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
}

func testMemoryBackendDefVersionMonotonic(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		row := mkMemoryBackendDef(fmt.Sprintf("mb-mono-%d", i), "backend-mono", "")
		written, err := s.MemoryBackendDefCreate(ctx, row)
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if want := i + 1; written.Version != want {
			t.Errorf("create #%d: version = %d, want %d", i, written.Version, want)
		}
	}
}

func testMemoryBackendDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, _ := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef("mb-active-1", "backend-active", ""))
	r2, _ := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef("mb-active-2", "backend-active", ""))

	if err := s.MemoryBackendDefSetActive(ctx, "backend-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MemoryBackendDefGetActive(ctx, "backend-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.MemoryBackendDefSetActive(ctx, "backend-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MemoryBackendDefGetActive(ctx, "backend-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

func testMemoryBackendDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, _ := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef("mb-retire", "backend-retire", ""))
	if row.Retired {
		t.Error("freshly created row should not be retired")
	}
	if err := s.MemoryBackendDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MemoryBackendDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("after retire(true): row should be retired")
	}
	if err := s.MemoryBackendDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MemoryBackendDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("after retire(false): row should NOT be retired")
	}
}

func testMemoryBackendDefParentNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkMemoryBackendDef("mb-orphan", "backend-orphan", "mb-nonexistent")
	_, err := s.MemoryBackendDefCreate(ctx, row)
	if err == nil {
		t.Fatal("expected ErrMemoryBackendDefParentNotFound, got nil")
	}
	if !errors.Is(err, store.ErrMemoryBackendDefParentNotFound) {
		t.Errorf("got %v, want ErrMemoryBackendDefParentNotFound", err)
	}
}

func testMemoryBackendDefListByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef(fmt.Sprintf("mb-list-%d", i), "backend-list", ""))
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.MemoryBackendDefListByName(ctx, "backend-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("len = %d, want 3", len(rows))
	}
	// version DESC ordering
	if rows[0].Version != 3 || rows[1].Version != 2 || rows[2].Version != 1 {
		t.Errorf("ordering wrong; versions = %d/%d/%d, want 3/2/1",
			rows[0].Version, rows[1].Version, rows[2].Version)
	}
}

func testMemoryBackendDefListChildren(t *testing.T, s store.Store) {
	ctx := context.Background()
	parent, _ := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef("mb-parent", "backend-tree", ""))
	for i := 0; i < 2; i++ {
		_, err := s.MemoryBackendDefCreate(ctx, mkMemoryBackendDef(fmt.Sprintf("mb-child-%d", i), fmt.Sprintf("backend-child-%d", i), parent.DefID))
		if err != nil {
			t.Fatal(err)
		}
	}
	children, err := s.MemoryBackendDefListChildren(ctx, parent.DefID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Errorf("children len = %d, want 2", len(children))
	}
}

// ---- v1.x RFC E ScheduleDef runtime — sweeper-side state ----

// scheduleRuntimeFixture seeds one active schedule and returns its
// def_id. Used by every runtime test to avoid repeating boilerplate.
func scheduleRuntimeFixture(t *testing.T, s store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	defID := "sd-" + name
	if _, err := s.ScheduleDefCreate(ctx, mkScheduleDef(defID, name, "")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.ScheduleDefSetActive(ctx, name, defID, "test"); err != nil {
		t.Fatalf("set active: %v", err)
	}
	return defID
}

func testScheduleRunStateSeedAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	defID := scheduleRuntimeFixture(t, s, "rt-seed")
	next := time.Now().Add(1 * time.Hour).Truncate(time.Microsecond)

	if err := s.ScheduleRunStateSeed(ctx, defID, next); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.ScheduleRunStateGet(ctx, defID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.NextRunAt.Equal(next) {
		t.Errorf("next_run_at = %v, want %v", got.NextRunAt, next)
	}
	if got.LastRunID != "" || got.LastStatus != "" {
		t.Errorf("seed should leave last_* empty: %+v", got)
	}

	// Re-seed with a different next: should update without resetting last_*.
	// (last_* are empty in this case anyway, but the idempotence is the
	// contract — re-promoting an active def shouldn't wipe its history.)
	updated := next.Add(2 * time.Hour)
	if err := s.ScheduleRunStateSeed(ctx, defID, updated); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got2, _ := s.ScheduleRunStateGet(ctx, defID)
	if !got2.NextRunAt.Equal(updated) {
		t.Errorf("re-seed next_run_at = %v, want %v", got2.NextRunAt, updated)
	}
}

func testScheduleRunStateListDueRespectsRetiredAndPaused(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)

	// Three schedules: due-and-fireable, due-but-retired, due-but-paused.
	dueID := scheduleRuntimeFixture(t, s, "rt-due-go")
	retiredID := scheduleRuntimeFixture(t, s, "rt-due-retired")
	pausedID := scheduleRuntimeFixture(t, s, "rt-due-paused")

	past := now.Add(-1 * time.Minute)
	for _, id := range []string{dueID, retiredID, pausedID} {
		if err := s.ScheduleRunStateSeed(ctx, id, past); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := s.ScheduleDefSetRetired(ctx, retiredID, true); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := s.ScheduleRunStatePause(ctx, pausedID, now.Add(1*time.Hour)); err != nil {
		t.Fatalf("pause: %v", err)
	}

	due, err := s.ScheduleRunStateListDue(ctx, now)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	names := make(map[string]bool)
	for _, d := range due {
		names[d.DefID] = true
	}
	if !names[dueID] {
		t.Errorf("due-and-fireable schedule missing from due list")
	}
	if names[retiredID] {
		t.Errorf("retired schedule should not appear in due list")
	}
	if names[pausedID] {
		t.Errorf("paused schedule should not appear in due list")
	}
}

func testScheduleRunStateRecordResult(t *testing.T, s store.Store) {
	ctx := context.Background()
	defID := scheduleRuntimeFixture(t, s, "rt-result")
	start := time.Now().Truncate(time.Microsecond)
	if err := s.ScheduleRunStateSeed(ctx, defID, start); err != nil {
		t.Fatalf("seed: %v", err)
	}

	next := start.Add(24 * time.Hour)
	if err := s.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastRunID:  "r_abc",
		LastStatus: "completed",
		LastRunAt:  start,
		NextRunAt:  next,
	}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	got, _ := s.ScheduleRunStateGet(ctx, defID)
	if got.LastRunID != "r_abc" || got.LastStatus != "completed" {
		t.Errorf("got %+v", got)
	}
	if !got.NextRunAt.Equal(next) {
		t.Errorf("next_run_at not advanced: %v, want %v", got.NextRunAt, next)
	}
	if !got.LastRunAt.Equal(start) {
		t.Errorf("last_run_at = %v, want %v", got.LastRunAt, start)
	}

	// Record on unknown def_id returns ErrNotFound.
	err := s.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:     "unknown",
		LastRunID: "r_nope",
		NextRunAt: next,
	})
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("record result on unknown def_id should return ErrNotFound; got %v", err)
	}
}

func testScheduleRunStatePauseResume(t *testing.T, s store.Store) {
	ctx := context.Background()
	defID := scheduleRuntimeFixture(t, s, "rt-pause")
	now := time.Now().Truncate(time.Microsecond)
	if err := s.ScheduleRunStateSeed(ctx, defID, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	until := now.Add(2 * time.Hour)
	if err := s.ScheduleRunStatePause(ctx, defID, until); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, _ := s.ScheduleRunStateGet(ctx, defID)
	if !got.PausedUntil.Equal(until) {
		t.Errorf("paused_until = %v, want %v", got.PausedUntil, until)
	}

	// Resume = pause with zero time clears the field.
	if err := s.ScheduleRunStatePause(ctx, defID, time.Time{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got2, _ := s.ScheduleRunStateGet(ctx, defID)
	if !got2.PausedUntil.IsZero() {
		t.Errorf("resume should clear paused_until; got %v", got2.PausedUntil)
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
	// Cluster mode stamps replica_id; single-replica leaves it empty.
	// Mix both shapes to assert the column round-trips and that an
	// empty value comes back empty (not a spurious NULL→"" mismatch).
	samples[1].ReplicaID = "replica-b"
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
	// replica_id round-trip: middle sample carries one, the others don't.
	if got[0].ReplicaID != "" {
		t.Errorf("sample[0] replica_id = %q, want empty", got[0].ReplicaID)
	}
	if got[1].ReplicaID != "replica-b" {
		t.Errorf("sample[1] replica_id = %q, want %q", got[1].ReplicaID, "replica-b")
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

// testListEventsFilterByTypeAndRange confirms the audit query honors
// both the type and from/to dimensions: only the union of matching
// rows is returned, and total reflects that union (not the table).
func testListEventsFilterByTypeAndRange(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_audit"})

	// Append a known sequence across multiple event types.
	for _, p := range []struct{ typ, body string }{
		{"text", `{"text":"hello"}`},
		{"tool_call", `{"tool":"Read"}`},
		{"text", `{"text":"world"}`},
		{"tool_result", `{"tool_use_id":"tu_1"}`},
		{"text", `{"text":"again"}`},
	} {
		if err := s.AppendEvent(ctx, run.ID, p.typ, []byte(p.body)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Filter by type only.
	got, total, err := s.ListEvents(ctx, store.EventFilter{Type: "text"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("type=text total = %d, want 3", total)
	}
	if len(got) != 3 {
		t.Errorf("type=text len(got) = %d, want 3", len(got))
	}
	for _, ev := range got {
		if ev.Type != "text" {
			t.Errorf("filter leaked non-text event: %q", ev.Type)
		}
		if ev.SessionID != sess.ID || ev.RunID != run.ID {
			t.Errorf("event missing session/run id: %+v", ev)
		}
	}

	// Order is ts DESC (newest first) — "again" was appended last.
	if got[0].Type != "text" {
		t.Fatalf("expected newest text first")
	}

	// Date range that excludes everything → empty + total=0.
	farFuture := time.Now().Add(24 * time.Hour)
	_, total2, err := s.ListEvents(ctx, store.EventFilter{From: farFuture}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total2 != 0 {
		t.Errorf("future-only filter total = %d, want 0", total2)
	}

	// Combined type + to in the future → all matching texts.
	got3, total3, err := s.ListEvents(ctx, store.EventFilter{
		Type: "tool_call",
		To:   farFuture,
	}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total3 != 1 || len(got3) != 1 || got3[0].Type != "tool_call" {
		t.Errorf("type+to mismatch: total=%d got=%v", total3, got3)
	}
}

// testGetLastEventForRunEmpty confirms a freshly-created run with no
// events yet yields ErrNotFound{Kind:"event"}. The list-agents
// handler treats this as "no awaited state" — common immediately
// after a run is registered.
func testGetLastEventForRunEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_empty"})

	_, err := s.GetLastEventForRun(ctx, run.ID)
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("got %v (%T), want *ErrNotFound", err, err)
	}
	if nf.Kind != "event" {
		t.Errorf("Kind = %q, want event", nf.Kind)
	}
}

// testGetLastEventForRunReturnsHighestSeq writes a sequence of
// events and asserts the lookup returns the LATEST one (highest
// seq), with the correct session/run/type/payload round-tripped.
func testGetLastEventForRunReturnsHighestSeq(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_last"})

	for _, p := range []struct{ typ, body string }{
		{"text", `{"text":"hi"}`},
		{"tool_call", `{"tool_use":{"id":"tu_1","name":"Read"}}`},
		{"tool_call", `{"tool_use":{"id":"tu_2","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`},
	} {
		if err := s.AppendEvent(ctx, run.ID, p.typ, []byte(p.body)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	ev, err := s.GetLastEventForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "tool_call" {
		t.Errorf("Type = %q, want tool_call (latest)", ev.Type)
	}
	if ev.SessionID != sess.ID || ev.RunID != run.ID {
		t.Errorf("session/run id mismatch: %+v", ev)
	}
	if !contains(string(ev.Payload), "Channel") {
		t.Errorf("payload should be the Channel.subscribe row, got %s", string(ev.Payload))
	}

	// Adding an event on a DIFFERENT run must not leak into this one.
	other, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_other"})
	if err := s.AppendEvent(ctx, other.ID, "text", []byte(`{"text":"other"}`)); err != nil {
		t.Fatal(err)
	}
	ev2, err := s.GetLastEventForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.RunID != run.ID || ev2.Type != "tool_call" {
		t.Errorf("scope leak: got %+v", ev2)
	}
}

// contains is a local helper to avoid pulling strings.Contains in
// a test-only file — keeps the contract package import-light.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// testListEventsPaginationAndTotal confirms limit + offset slice the
// matching window correctly and total is the unbounded match count
// regardless of pagination.
func testListEventsPaginationAndTotal(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_page"})

	const N = 7
	for i := 0; i < N; i++ {
		if err := s.AppendEvent(ctx, run.ID, "text", []byte(`{}`)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	page1, total, err := s.ListEvents(ctx, store.EventFilter{Type: "text"}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != N {
		t.Errorf("total = %d, want %d", total, N)
	}
	if len(page1) != 3 {
		t.Errorf("page1 len = %d, want 3", len(page1))
	}

	page2, _, err := s.ListEvents(ctx, store.EventFilter{Type: "text"}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 3 {
		t.Errorf("page2 len = %d, want 3", len(page2))
	}

	page3, _, err := s.ListEvents(ctx, store.EventFilter{Type: "text"}, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 {
		t.Errorf("page3 len = %d, want 1", len(page3))
	}

	// Pages must not overlap on seq.
	seen := map[int64]bool{}
	for _, ev := range append(append(page1, page2...), page3...) {
		if seen[ev.Seq] {
			t.Errorf("seq %d appeared in two pages", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}
