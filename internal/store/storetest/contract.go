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
// jsonEqual reports whether a stored Definition (raw JSON) is semantically
// equal to want, independent of whitespace AND object key order. Postgres
// stores Definition as jsonb and re-serializes it ({"v":"A"} -> {"v": "A"},
// keys reordered), so a byte-compare against a compact literal spuriously
// fails on the PG backend while SQLite (TEXT, verbatim) passes — the
// brittleness that kept the PG contract suite from running green in CI (and
// hid the operator-token NULL-scan bug, BUG-2). Compare decoded values.
func jsonEqual(got []byte, want string) bool {
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

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
		{"GetRunEventsSince", testGetRunEventsSince},
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
		{"ListSessions", testListSessions},
		{"ListSessionsTagEscaping", testListSessionsTagEscaping},
		{"ListSessionsTenantIsolation", testListSessionsTenantIsolation},
		{"SetSessionMeta", testSetSessionMeta},
		// RFC BE op=related — the per-session embedding index. Unlike
		// memory_embeddings this needs no vector extension (ranks in Go), so both
		// backends run the REAL round-trip — no SupportsVectors() fork.
		{"SessionEmbedUpsertSearch", testSessionEmbedUpsertSearch},
		{"SessionEmbedSearchTenantFold", testSessionEmbedSearchTenantFold},
		{"ListUsers", testListUsers},
		{"ListRunsByParentAgentID", testListRunsByParentAgentID},
		{"UpdateHeartbeat", testUpdateHeartbeat},
		{"FinishRunCancelledTerminal", testFinishRunCancelledTerminal},
		{"TranscriptOrderedAcrossRuns", testTranscriptOrderedAcrossRuns},
		{"SweepStaleRuns", testSweepStaleRuns},
		// F42 / RFC X Phase 2: a paused run is intentionally parked, not crashed —
		// the stale-run sweeper must skip it (else a restored paused run dies
		// before resume re-dispatches it). Plus runs.interactive round-trips.
		{"SweepStaleRunsSkipsPaused", testSweepStaleRunsSkipsPaused},
		{"CreateRunInteractiveRoundTrip", testCreateRunInteractiveRoundTrip},
		{"CreateRunOperatorKeyRestrictedRoundTrip", testCreateRunOperatorKeyRestrictedRoundTrip},
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
		{"MemoryDeleteScope", testMemoryDeleteScope},
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
		{"MemoryListTenantsForScope", testMemoryListTenantsForScope},
		// RFC BL P2: the background-consolidation substrate.
		{"MemorySupersedeHidesFromReads", testMemorySupersedeHidesFromReads},
		{"MemorySupersedeIsIdempotent", testMemorySupersedeIsIdempotent},
		{"MemorySupersedeRevivedByWrite", testMemorySupersedeRevivedByWrite},
		{"MemoryPendingEnqueueDrainAck", testMemoryPendingEnqueueDrainAck},
		{"MemoryCursorGetDefault", testMemoryCursorGetDefault},
		{"MemoryCursorLeaseCAS", testMemoryCursorLeaseCAS},
		{"MemoryCursorAdvanceMonotonicAndOwner", testMemoryCursorAdvanceMonotonicAndOwner},
		{"MemoryDeleteScopeCleansConsolidationRows", testMemoryDeleteScopeCleansConsolidationRows},
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
		{"MemoryEmbedSearchReturnsVectors", testMemoryEmbedSearchReturnsVectors},
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
		{"ChannelPurge", testChannelPurge},
		{"ChannelPeekDoesNotConsume", testChannelPeekDoesNotConsume},
		{"ChannelReplayFromCursorZero", testChannelReplayFromCursorZero},
		{"ChannelStatsAggregatesNonExpired", testChannelStatsAggregatesNonExpired},
		{"ChannelStatsEmptyOnNoMessages", testChannelStatsEmptyOnNoMessages},
		{"ChannelGetPointLookup", testChannelGetPointLookup},
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
		// Soft-reclaim: retiring the ACTIVE def clears its pointer (name
		// reclaimable); retiring a non-active version leaves it; list surfaces
		// live-vs-total counts. Fail-before: retire was a bare flag flip.
		{"AgentDefRetireActiveClearsPointer", testAgentDefRetireActiveClearsPointer},
		{"AgentDefRetireNonActiveLeavesPointer", testAgentDefRetireNonActiveLeavesPointer},
		{"AgentDefListNamesLiveCount", testAgentDefListNamesLiveCount},
		{"AgentDefStaticFallback", testAgentDefStaticFallback},
		// RFC N — agent definition plane tenant isolation. Fails on the
		// pre-migration single-`name`-PK schema (clobber); passes after.
		{"AgentDefTenantIsolation", testAgentDefTenantIsolation},
		{"AgentDefContentSHA256RoundTrip", testAgentDefContentSHA256RoundTrip},
		{"BackfillAgentDefContentSHA256", testBackfillAgentDefContentSHA256},
		{"BackfillAgentDefSystemPromptBaseFillsLegacyRows", testBackfillAgentDefSystemPromptBase},
		// RFC BM data retention: list + delete purgeable retired def versions.
		// Asserts the retired / non-active / keep-last-N exclusions are each
		// load-bearing, and DeleteDefVersions removes exactly the listed rows.
		{"RetentionPurgeDefVersions", testRetentionPurgeDefVersions},
		// v0.8.22 SkillDef substrate — mirror of the AgentDef tests.
		{"SkillDefCreateAndGet", testSkillDefCreateAndGet},
		{"SkillDefVersionMonotonicUnderContention", testSkillDefVersionMonotonicUnderContention},
		{"SkillDefParallelForksDistinctVersions", testSkillDefParallelForksDistinctVersions},
		{"SkillDefAppendOnlyDefinition", testSkillDefAppendOnlyDefinition},
		{"SkillDefActivePointerIdempotent", testSkillDefActivePointerIdempotent},
		{"SkillDefRetireReversible", testSkillDefRetireReversible},
		{"SkillDefListNamesLiveCount", testSkillDefListNamesLiveCount},
		{"SkillDefStaticFallback", testSkillDefStaticFallback},
		// RFC N — skill definition plane tenant isolation. Fails on the
		// pre-migration single-`name`-PK schema (clobber); passes after.
		{"SkillDefTenantIsolation", testSkillDefTenantIsolation},
		{"SkillDefContentSHA256RoundTrip", testSkillDefContentSHA256RoundTrip},
		{"BackfillSkillDefContentSHA256", testBackfillSkillDefContentSHA256},
		{"SkillDefSnapshotReadEmpty", testSkillDefSnapshotReadEmpty},
		// TeamDef substrate — mirror of the SkillDef tests.
		{"TeamDefCreateAndGet", testTeamDefCreateAndGet},
		{"TeamDefVersionMonotonicUnderContention", testTeamDefVersionMonotonicUnderContention},
		{"TeamDefActivePointerIdempotent", testTeamDefActivePointerIdempotent},
		{"TeamDefRetireReversible", testTeamDefRetireReversible},
		{"TeamDefDelete", testTeamDefDelete},
		{"TeamDefListNamesLiveCount", testTeamDefListNamesLiveCount},
		{"TeamDefStaticFallback", testTeamDefStaticFallback},
		// RFC N — team definition plane tenant isolation. Fails on the
		// pre-migration single-`name`-PK schema (clobber); passes after.
		{"TeamDefTenantIsolation", testTeamDefTenantIsolation},
		{"TeamDefContentSHA256RoundTrip", testTeamDefContentSHA256RoundTrip},
		// v0.9.x MCPServerDef substrate — mirror of the AgentDef + SkillDef tests.
		{"MCPServerDefCreateAndGet", testMCPServerDefCreateAndGet},
		{"MCPServerDefVersionMonotonic", testMCPServerDefVersionMonotonic},
		{"MCPServerDefActivePointerIdempotent", testMCPServerDefActivePointerIdempotent},
		{"MCPServerDefRetireReversible", testMCPServerDefRetireReversible},
		{"MCPServerDefListNamesLiveCount", testMCPServerDefListNamesLiveCount},
		// RFC N — MCP server definition plane tenant isolation. Fails on the
		// pre-migration single-`name`-PK schema (clobber); passes after.
		{"MCPServerDefTenantIsolation", testMCPServerDefTenantIsolation},
		{"MCPServerDefContentSHA256RoundTrip", testMCPServerDefContentSHA256RoundTrip},
		{"BackfillMCPServerDefContentSHA256", testBackfillMCPServerDefContentSHA256},
		// v1.x RFC E ScheduleDef substrate — same shape minus content_sha256.
		{"ScheduleDefCreateAndGet", testScheduleDefCreateAndGet},
		{"ScheduleDefVersionMonotonic", testScheduleDefVersionMonotonic},
		{"ScheduleDefActivePointerIdempotent", testScheduleDefActivePointerIdempotent},
		{"ScheduleDefTenantIsolation", testScheduleDefTenantIsolation},
		{"ScheduleDefRetireReversible", testScheduleDefRetireReversible},
		{"ScheduleDefParentNotFound", testScheduleDefParentNotFound},
		{"ScheduleDefListByName", testScheduleDefListByName},
		{"ScheduleDefListChildren", testScheduleDefListChildren},
		// v1.x RFC G A2A substrate — same shape as ScheduleDef, two Defs.
		{"A2AServerCardDefCreateAndGet", testA2AServerCardDefCreateAndGet},
		{"A2AServerCardDefVersionMonotonic", testA2AServerCardDefVersionMonotonic},
		{"A2AServerCardDefActivePointerIdempotent", testA2AServerCardDefActivePointerIdempotent},
		{"A2AServerCardDefTenantIsolation", testA2AServerCardDefTenantIsolation},
		{"A2AServerCardDefRetireReversible", testA2AServerCardDefRetireReversible},
		{"A2AServerCardDefParentNotFound", testA2AServerCardDefParentNotFound},
		{"A2AServerCardDefListByName", testA2AServerCardDefListByName},
		{"A2AServerCardDefListChildren", testA2AServerCardDefListChildren},
		{"A2AAgentDefCreateAndGet", testA2AAgentDefCreateAndGet},
		{"A2AAgentDefVersionMonotonic", testA2AAgentDefVersionMonotonic},
		{"A2AAgentDefActivePointerIdempotent", testA2AAgentDefActivePointerIdempotent},
		{"A2AAgentDefTenantIsolation", testA2AAgentDefTenantIsolation},
		{"A2AAgentDefRetireReversible", testA2AAgentDefRetireReversible},
		{"A2AAgentDefParentNotFound", testA2AAgentDefParentNotFound},
		{"A2AAgentDefListByName", testA2AAgentDefListByName},
		{"A2AAgentDefListChildren", testA2AAgentDefListChildren},
		{"WebhookDefCreateAndGet", testWebhookDefCreateAndGet},
		{"WebhookDefVersionMonotonic", testWebhookDefVersionMonotonic},
		{"WebhookDefActivePointerIdempotent", testWebhookDefActivePointerIdempotent},
		{"WebhookDefTenantIsolation", testWebhookDefTenantIsolation},
		{"WebhookDefRetireReversible", testWebhookDefRetireReversible},
		{"WebhookDefParentNotFound", testWebhookDefParentNotFound},
		{"WebhookDefListByName", testWebhookDefListByName},
		{"WebhookDefListChildren", testWebhookDefListChildren},
		{"MemoryBackendDefCreateAndGet", testMemoryBackendDefCreateAndGet},
		{"MemoryBackendDefVersionMonotonic", testMemoryBackendDefVersionMonotonic},
		{"MemoryBackendDefActivePointerIdempotent", testMemoryBackendDefActivePointerIdempotent},
		{"MemoryBackendDefTenantIsolation", testMemoryBackendDefTenantIsolation},
		{"MemoryBackendDefRetireReversible", testMemoryBackendDefRetireReversible},
		{"MemoryBackendDefParentNotFound", testMemoryBackendDefParentNotFound},
		{"MemoryBackendDefListByName", testMemoryBackendDefListByName},
		{"MemoryBackendDefListChildren", testMemoryBackendDefListChildren},
		// RFC AH Phase 2a VolumeDef substrate — flat (tenant, name) table.
		{"VolumeDefCreateAndGet", testVolumeDefCreateAndGet},
		{"VolumeDefTenantIsolation", testVolumeDefTenantIsolation},
		{"CredentialDefScopeIsolation", testCredentialDefScopeIsolation},
		{"VolumeDefDelete", testVolumeDefDelete},
		{"VolumeDefList", testVolumeDefList},
		{"VolumeDefCreateUpdatesDefinition", testVolumeDefCreateUpdatesDefinition},
		// RFC AL Path primitive — the dirent (path tree) substrate.
		{"DirentCreateAndGet", testDirentCreateAndGet},
		{"DirentListOneLevel", testDirentListOneLevel},
		{"DirentListUnderRecursive", testDirentListUnderRecursive},
		{"DirentDeleteAndDeleteUnder", testDirentDeleteAndDeleteUnder},
		{"DirentMoveCascade", testDirentMoveCascade},
		{"DirentScopeIsolation", testDirentScopeIsolation},
		{"DirentTenantIsolation", testDirentTenantIsolation},
		// RFC AH Phase 2b ephemeral (run-tree-scoped) volumes.
		{"EphemeralVolumeCreateListDelete", testEphemeralVolumeCreateListDelete},
		{"EphemeralVolumeSweepCandidatesTerminalOnly", testEphemeralVolumeSweepCandidatesTerminalOnly},
		{"EphemeralVolumeSweepCandidatesSkipsPaused", testEphemeralVolumeSweepCandidatesSkipsPaused},
		{"EphemeralVolumeListByTenantIsolation", testEphemeralVolumeListByTenantIsolation},
		// RFC L OSS multi-tenant authorization — OperatorTokenDef.
		{"OperatorTokenDefCreateAndLookup", testOperatorTokenDefCreateAndLookup},
		{"OperatorTokenDefCurrentByName", testOperatorTokenDefCurrentByName},
		{"OperatorTokenDefRetireAndCountAdmin", testOperatorTokenDefRetireAndCountAdmin},
		{"OperatorTokenDefListNames", testOperatorTokenDefListNames},
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
		{"InterruptFinishSetsDeclined", testInterruptFinishSetsDeclined},
		{"InterruptFinishRejectsAlreadyTerminal", testInterruptFinishRejectsAlreadyTerminal},
		{"InterruptListByRunFiltersByStatus", testInterruptListByRunFiltersByStatus},
		{"InterruptListByUserFiltersByStatus", testInterruptListByUserFiltersByStatus},
		{"InterruptListByUserFiltersByTenant", testInterruptListByUserFiltersByTenant},
		{"InterruptCountPendingByRunIsAccurateUnderConcurrency", testInterruptCountPendingByRunIsAccurateUnderConcurrency},
		{"InterruptSweepExpiredMarksOnlyExpiredPending", testInterruptSweepExpiredMarksOnlyExpiredPending},
		{"InterruptIDIsMonotonicByTime", testInterruptIDIsMonotonicByTime},
		// v0.8.21 audit-view cross-session event listing.
		{"ListEventsFilterByTypeAndRange", testListEventsFilterByTypeAndRange},
		{"ListEventsFilterByTenant", testListEventsFilterByTenant},
		{"ListEventsPaginationAndTotal", testListEventsPaginationAndTotal},
		// v0.8.21 awaited-state derivation needs last-event-per-run.
		{"GetLastEventForRunEmpty", testGetLastEventForRunEmpty},
		{"GetLastEventForRunReturnsHighestSeq", testGetLastEventForRunReturnsHighestSeq},
		// v0.12.7 provider telemetry — FinishRun must persist the
		// final-iteration provider so post-run analysis can count
		// fallback-routed runs.
		{"FinishRunPersistsProvider", testFinishRunPersistsProvider},
		{"FinishRunPersistsProviderEmpty", testFinishRunPersistsProviderEmpty},
		// RFC AV — per-call usage ledger + per-run cost/source summary.
		{"TokenUsageLedger", testTokenUsageLedger},
		{"FinishRunPersistsCostAndSource", testFinishRunPersistsCostAndSource},
		{"RunCostSummary", testRunCostSummary},
		{"UsageReport", testUsageReport},
		{"UsageReportPerCurrency", testUsageReportPerCurrency},
		{"UsageRollupAndPrune", testUsageRollupAndPrune},
		{"UsageReportArchiveWindowBoundary", testUsageReportArchiveWindowBoundary},
		// RFC AW — per-scope token budgets: upsert / get-all / delete round-trip
		// incl. nullable tiers.
		{"TokenLimits", testTokenLimits},
		{"SessionArchiver", testSessionArchiver},
		// RFC BM Phase 2: a PINNED session is exempt from PrunableAgedSessions
		// (all automated retention). Fails on the pre-fix query (no exclusion).
		{"PrunableAgedSessionsExcludesPinned", testPrunableAgedSessionsExcludesPinned},
		// RFC AF (v1.0.1): the cluster hook registry persists the
		// authoritative tenant_id. Exercises the new column's INSERT/SELECT
		// ordinals on a REAL backend (the go-postgres CI job) — SQLite skips
		// (hook DB methods are Postgres-only). Guards the BUG-2 class:
		// a shifted placeholder/scan that the SQLite path can't catch.
		{"HookTenantRoundTrip", testHookTenantRoundTrip},
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

func testGetRunEventsSince(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	for i, typ := range []string{"started", "text", "tool_call", "tool_result", "done"} {
		if err := s.AppendEvent(ctx, run.ID, typ, []byte(`{"type":"`+typ+`"}`)); err != nil {
			t.Fatalf("AppendEvent[%d]: %v", i, err)
		}
	}

	// from 0 → all events, ascending by seq.
	all, err := s.GetRunEventsSince(ctx, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("GetRunEventsSince(0) len = %d, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Errorf("seq not ascending at %d: %d -> %d", i, all[i-1].Seq, all[i].Seq)
		}
		if all[i].RunID != run.ID {
			t.Errorf("event %d run_id = %q, want %q", i, all[i].RunID, run.ID)
		}
	}

	// from a mid cursor → only newer events (the tail's incremental read).
	cursor := all[1].Seq
	tail, err := s.GetRunEventsSince(ctx, run.ID, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 3 {
		t.Fatalf("GetRunEventsSince(seq=%d) len = %d, want 3", cursor, len(tail))
	}
	if tail[0].Seq <= cursor {
		t.Errorf("first tail seq %d not > cursor %d", tail[0].Seq, cursor)
	}

	// limit caps the page.
	capped, err := s.GetRunEventsSince(ctx, run.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Errorf("GetRunEventsSince(limit=2) len = %d, want 2", len(capped))
	}

	// caught up → empty (not error).
	none, err := s.GetRunEventsSince(ctx, run.ID, all[len(all)-1].Seq, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("GetRunEventsSince past tail len = %d, want 0", len(none))
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

// testTokenUsageLedger exercises the RFC AV per-call ledger: RecordCallUsage
// inserts + TokenUsageForRun reads back, credential_source distinguishes
// operator vs tenant, cost is NULL when unpriced (empty currency) vs a genuine
// zero, and the per-run summary (Σ token_usage) matches the FinishRun rollup.
func testTokenUsageLedger(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "tenant-x", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_usage_ledger", TenantID: "tenant-x", UserID: "u1"})

	// Call 0: operator key paid, priced (USD). Call 1: a tenant override paid,
	// left unpriced (unknown model) → NULL cost.
	rows := []store.TokenUsageRow{
		{RunID: run.ID, TenantID: "tenant-x", UserID: "u1", AgentID: "a_usage_ledger", Iteration: 0,
			Provider: "anthropic", Model: "claude-opus-4-8", CredentialSource: "operator",
			InputTokens: 100, OutputTokens: 50, Cost: 0.0564, CostCurrency: "USD"},
		{RunID: run.ID, TenantID: "tenant-x", UserID: "u1", AgentID: "a_usage_ledger", Iteration: 1,
			Provider: "deepseek", Model: "deepseek-v4-flash", CredentialSource: "tenant", CredentialScopeID: "tenant-x",
			InputTokens: 200, OutputTokens: 80, Cost: 0, CostCurrency: ""}, // unpriced → NULL
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage: %v", err)
		}
	}

	got, err := s.TokenUsageForRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("TokenUsageForRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].CredentialSource != "operator" || got[1].CredentialSource != "tenant" {
		t.Errorf("sources = %q,%q want operator,tenant", got[0].CredentialSource, got[1].CredentialSource)
	}
	if got[1].CredentialScopeID != "tenant-x" {
		t.Errorf("scope_id = %q, want tenant-x", got[1].CredentialScopeID)
	}
	if got[0].CostCurrency != "USD" || got[0].Cost == 0 {
		t.Errorf("row0 cost = (%v,%q), want (0.0564,USD)", got[0].Cost, got[0].CostCurrency)
	}
	// Unpriced row: empty currency round-trips (NULL cost); Cost stays 0.
	if got[1].CostCurrency != "" || got[1].Cost != 0 {
		t.Errorf("row1 (unpriced) = (%v,%q), want (0,\"\")", got[1].Cost, got[1].CostCurrency)
	}

	// Rollup invariant: the per-run summary the loop would write (Σ of the calls)
	// must equal the sum of the ledger rows. Simulate FinishRun with those sums.
	var inSum, outSum int
	for _, r := range got {
		inSum += r.InputTokens
		outSum += r.OutputTokens
	}
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: inSum, OutputTokens: outSum, Model: "claude-opus-4-8", Provider: "anthropic"}, ""); err != nil {
		t.Fatal(err)
	}
	fin, _ := s.GetRunByAgentID(ctx, "a_usage_ledger")
	if fin.InputTokens != inSum || fin.OutputTokens != outSum {
		t.Errorf("rollup mismatch: runs=(%d,%d) Σledger=(%d,%d)", fin.InputTokens, fin.OutputTokens, inSum, outSum)
	}
}

// testFinishRunPersistsCostAndSource verifies the per-run cost + credential
// source summary round-trips, and that an UNPRICED run (empty currency) stores a
// NULL cost (GetRun.Cost == nil), distinct from a priced zero.
func testFinishRunPersistsCostAndSource(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")

	// Priced + tenant-sourced run.
	run1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_cost_priced"})
	if err := s.FinishRun(ctx, run1.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 10, Model: "m", Provider: "anthropic",
			Cost: 1.25, CostCurrency: "USD", CredentialSource: "tenant", CredentialScopeID: "tenant-x"}, ""); err != nil {
		t.Fatal(err)
	}
	got1, _ := s.GetRunByAgentID(ctx, "a_cost_priced")
	if got1.Cost == nil || *got1.Cost != 1.25 || got1.CostCurrency != "USD" {
		t.Errorf("priced cost = %v %q, want 1.25 USD", got1.Cost, got1.CostCurrency)
	}
	if got1.CredentialSource != "tenant" || got1.CredentialScopeID != "tenant-x" {
		t.Errorf("source = %q/%q, want tenant/tenant-x", got1.CredentialSource, got1.CredentialScopeID)
	}

	// Unpriced run (empty currency) → NULL cost.
	run2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_cost_unpriced"})
	if err := s.FinishRun(ctx, run2.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 10, Model: "m", Provider: "openai", CredentialSource: "operator"}, ""); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetRunByAgentID(ctx, "a_cost_unpriced")
	if got2.Cost != nil {
		t.Errorf("unpriced cost = %v, want nil (NULL)", *got2.Cost)
	}
	if got2.CredentialSource != "operator" {
		t.Errorf("source = %q, want operator", got2.CredentialSource)
	}
}

// testRunCostSummary is the fix-9 store-contract check: RunCostSummary sums the
// per-call ledger, reports the priced currency, and distinguishes a priced run
// from an unpriced one (currency "" ⇒ the FinishRun caller stores a NULL cost).
// Runs on both backends (the sqlite unit + the postgres CI job).
func testRunCostSummary(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Priced run: two calls at different costs → summed, currency reported, priced.
	if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "rcs1", TenantID: "t", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 10, Cost: 0.25, CostCurrency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "rcs1", TenantID: "t", Provider: "p", Model: "m2",
		Iteration: 1, CredentialSource: "operator", InputTokens: 20, Cost: 0.75, CostCurrency: "USD"}); err != nil {
		t.Fatal(err)
	}
	cost, currency, priced, err := s.RunCostSummary(ctx, "rcs1")
	if err != nil {
		t.Fatalf("RunCostSummary(priced): %v", err)
	}
	if !priced || currency != "USD" || cost < 0.999 || cost > 1.001 {
		t.Errorf("priced run = (cost=%v currency=%q priced=%v), want (1.0,USD,true)", cost, currency, priced)
	}

	// Unpriced run: a row with no currency (NULL cost) → priced=false, currency "".
	if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "rcs2", TenantID: "t", Provider: "p", Model: "x",
		CredentialSource: "operator", InputTokens: 5, CostCurrency: ""}); err != nil {
		t.Fatal(err)
	}
	cost2, currency2, priced2, err := s.RunCostSummary(ctx, "rcs2")
	if err != nil {
		t.Fatalf("RunCostSummary(unpriced): %v", err)
	}
	if priced2 || currency2 != "" || cost2 != 0 {
		t.Errorf("unpriced run = (cost=%v currency=%q priced=%v), want (0,\"\",false)", cost2, currency2, priced2)
	}

	// A run with no ledger rows at all → unpriced (never priced).
	_, cur3, priced3, err := s.RunCostSummary(ctx, "rcs-none")
	if err != nil {
		t.Fatalf("RunCostSummary(empty): %v", err)
	}
	if priced3 || cur3 != "" {
		t.Errorf("no-ledger run = (currency=%q priced=%v), want (\"\",false)", cur3, priced3)
	}
}

// testUsageReport exercises the RFC AV Phase 2 aggregation: grouping,
// tenant/window scoping, the operator-vs-tenant split, and the unpriced-call
// count. Rows are inserted directly (no run needed — token_usage has no FK).
func testUsageReport(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(tenant, source string, in int, cost float64, cur string) store.TokenUsageRow {
		return store.TokenUsageRow{
			RunID: "r_" + tenant + source, TenantID: tenant, Provider: "anthropic", Model: "m",
			CredentialSource: source, InputTokens: in, Cost: cost, CostCurrency: cur,
		}
	}
	rows := []store.TokenUsageRow{
		mk("A", "operator", 100, 1.0, "USD"),
		mk("A", "tenant", 200, 2.0, "USD"),
		mk("B", "operator", 400, 4.0, "USD"),
		// tenant A, operator, UNPRICED (empty currency) — counts tokens, no cost.
		{RunID: "r_unpriced", TenantID: "A", Provider: "openai", Model: "x",
			CredentialSource: "operator", InputTokens: 50, CostCurrency: ""},
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage: %v", err)
		}
	}

	sum := func(aggs []store.UsageAggregate, tenant, source, currency string) (in int64, cost float64, unpriced int64, found bool) {
		for _, a := range aggs {
			if a.TenantID == tenant && a.CredentialSource == source && a.Currency == currency {
				return a.InputTokens, a.Cost, a.UnpricedCalls, true
			}
		}
		return 0, 0, 0, false
	}

	// Group by (tenant, source), all tenants.
	all, err := s.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageBySource}})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	// A/operator splits by currency (fix-10 — never sum across currencies): the
	// priced USD row (100 tok, 1.0) and the unpriced '' row (50 tok, 1 unpriced) do
	// NOT merge into one 150-token row.
	if in, cost, unpriced, ok := sum(all, "A", "operator", "USD"); !ok || in != 100 || cost != 1.0 || unpriced != 0 {
		t.Errorf("A/operator/USD = (in=%d cost=%v unpriced=%d ok=%v), want (100,1,0,true)", in, cost, unpriced, ok)
	}
	if in, cost, unpriced, ok := sum(all, "A", "operator", ""); !ok || in != 50 || cost != 0 || unpriced != 1 {
		t.Errorf("A/operator/unpriced = (in=%d cost=%v unpriced=%d ok=%v), want (50,0,1,true)", in, cost, unpriced, ok)
	}
	if in, cost, _, ok := sum(all, "A", "tenant", "USD"); !ok || in != 200 || cost != 2.0 {
		t.Errorf("A/tenant = (%d,%v,%v), want (200,2,true)", in, cost, ok)
	}
	if in, cost, _, ok := sum(all, "B", "operator", "USD"); !ok || in != 400 || cost != 4.0 {
		t.Errorf("B/operator = (%d,%v,%v), want (400,4,true)", in, cost, ok)
	}

	// Tenant-scoped to A → no B rows.
	onlyA, _ := s.UsageReport(ctx, store.UsageQuery{TenantID: "A", GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageBySource}})
	for _, a := range onlyA {
		if a.TenantID == "B" {
			t.Errorf("tenant-scoped report leaked tenant B: %+v", a)
		}
	}

	// Operator bill = group by source, filter source=operator across all tenants:
	// cost 1.0 (A) + 4.0 (B) = 5.0. Sum across the operator's currency rows (only
	// the USD row carries cost; the '' unpriced row is 0).
	bySource, _ := s.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageBySource}})
	var opCost float64
	for _, a := range bySource {
		if a.CredentialSource == "operator" {
			opCost += a.Cost
		}
	}
	if opCost != 5.0 {
		t.Errorf("operator bill = %v, want 5.0", opCost)
	}
}

// testUsageReportPerCurrency is the fix-10 regression: UsageReport must never sum
// across currencies. Two priced calls in the SAME (tenant, source) bucket but with
// different currencies (USD + EUR) must yield TWO rows — one per currency, each
// with its own cost — not one row summing 1.5+2.5 under an arbitrary MAX label.
func testUsageReportPerCurrency(t *testing.T, s store.Store) {
	ctx := context.Background()
	rows := []store.TokenUsageRow{
		{RunID: "r_usd", TenantID: "MC", Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: 100, Cost: 1.5, CostCurrency: "USD"},
		{RunID: "r_eur", TenantID: "MC", Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: 200, Cost: 2.5, CostCurrency: "EUR"},
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage: %v", err)
		}
	}
	rep, err := s.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageBySource}})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	byCurrency := map[string]store.UsageAggregate{}
	for _, a := range rep {
		if a.TenantID == "MC" && a.CredentialSource == "operator" {
			byCurrency[a.Currency] = a
		}
	}
	if len(byCurrency) != 2 {
		t.Fatalf("MC/operator rows = %d, want 2 (one per currency): %+v", len(byCurrency), rep)
	}
	if a := byCurrency["USD"]; a.Cost != 1.5 || a.InputTokens != 100 {
		t.Errorf("USD row = (cost=%v in=%d), want (1.5,100)", a.Cost, a.InputTokens)
	}
	if a := byCurrency["EUR"]; a.Cost != 2.5 || a.InputTokens != 200 {
		t.Errorf("EUR row = (cost=%v in=%d), want (2.5,200)", a.Cost, a.InputTokens)
	}
}

// testUsageRollupAndPrune exercises RFC AV Phase 2b retention: old token_usage
// rows fold into usage_archive (same day+dims merge) and are deleted, the report
// unions archive + recent, and a re-run is a no-op (idempotent).
func testUsageRollupAndPrune(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	recent := time.Now()
	mk := func(in int, cost float64, ts time.Time) store.TokenUsageRow {
		return store.TokenUsageRow{
			RunID: "r", TenantID: "A", Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: in, Cost: cost, CostCurrency: "USD", TS: ts,
		}
	}
	// Two OLD rows (same day + dims → merge in archive) + one RECENT row.
	for _, r := range []store.TokenUsageRow{mk(100, 1.0, old), mk(200, 2.0, old), mk(50, 0.5, recent)} {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Now().Add(-5 * 24 * time.Hour)
	pruned, err := s.RollupAndPruneUsage(ctx, cutoff)
	if err != nil {
		t.Fatalf("RollupAndPruneUsage: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2 (the two old rows)", pruned)
	}
	// Recent detail survives; old detail is gone.
	if rows, _ := s.TokenUsageForRun(ctx, "r"); len(rows) != 1 || rows[0].InputTokens != 50 {
		t.Errorf("token_usage after prune = %+v, want the single recent row", rows)
	}
	// Report unions archive (300 tok / 3.0 cost, 2 calls) + recent (50 / 0.5, 1):
	// 350 tokens, 3.5 cost, 3 calls.
	rep, _ := s.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageBySource}})
	if len(rep) != 1 {
		t.Fatalf("report rows = %d, want 1: %+v", len(rep), rep)
	}
	a := rep[0]
	if a.InputTokens != 350 || a.Cost != 3.5 || a.CallCount != 3 {
		t.Errorf("unioned report = (in=%d cost=%v calls=%d), want (350,3.5,3)", a.InputTokens, a.Cost, a.CallCount)
	}

	// Idempotent: a second prune of the same window moves nothing (old rows gone).
	if pruned2, _ := s.RollupAndPruneUsage(ctx, cutoff); pruned2 != 0 {
		t.Errorf("second prune = %d, want 0 (idempotent)", pruned2)
	}
	// And the archive didn't double-count.
	rep2, _ := s.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageBySource}})
	if len(rep2) != 1 || rep2[0].InputTokens != 350 {
		t.Errorf("report after idempotent re-run = %+v, want unchanged 350", rep2)
	}
}

// testUsageReportArchiveWindowBoundary is the fix-3 regression: an intra-day
// `from` must still include the from-day's archived (day-bucketed) bucket. The
// archive stores period_start at UTC midnight, so comparing an intra-day `from`
// (e.g. 12:00) EXACTLY dropped the whole from-day bucket (period_start 00:00 <
// 12:00). The report floors the archive `from` bound to its UTC day. A row on an
// EARLIER day stays excluded — the floor extends only to the from-day, never
// under. Fails on the pre-fix code (from-day bucket dropped → 0 rows).
func testUsageReportArchiveWindowBoundary(t *testing.T, s store.Store) {
	ctx := context.Background()
	day := func(y int, m time.Month, d, h int) time.Time {
		return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
	}
	// fromDay row (08:00, same UTC day as the intra-day `from` at 12:00) → its
	// bucket must be INCLUDED. priorDay row (the day before) → EXCLUDED.
	rows := []store.TokenUsageRow{
		{RunID: "r_fromday", TenantID: "WB", Provider: "p", Model: "m",
			CredentialSource: "operator", InputTokens: 100, Cost: 1.0, CostCurrency: "USD", TS: day(2026, 3, 15, 8)},
		{RunID: "r_priorday", TenantID: "WB", Provider: "p", Model: "m",
			CredentialSource: "operator", InputTokens: 999, Cost: 9.0, CostCurrency: "USD", TS: day(2026, 3, 14, 8)},
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage: %v", err)
		}
	}
	// Roll both rows into the day-bucketed archive (cutoff after both).
	if _, err := s.RollupAndPruneUsage(ctx, day(2026, 3, 16, 0)); err != nil {
		t.Fatalf("RollupAndPruneUsage: %v", err)
	}

	// Query with an intra-day `from` (12:00 on the from-day) — the from-day
	// bucket must survive; the prior day must not.
	rep, err := s.UsageReport(ctx, store.UsageQuery{
		From:    day(2026, 3, 15, 12),
		GroupBy: []store.UsageDimension{store.UsageByTenant},
	})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	if len(rep) != 1 {
		t.Fatalf("report rows = %d, want 1 (the from-day bucket): %+v", len(rep), rep)
	}
	if rep[0].InputTokens != 100 {
		t.Errorf("windowed archive report = %d tokens, want 100 (from-day bucket included, prior day excluded)", rep[0].InputTokens)
	}
}

// testTokenLimits exercises the RFC AW token_limits store: upsert (both tiers,
// then a re-upsert overwrite), get-all across scopes with nullable tiers, and
// delete. It also proves the NULL-vs-zero distinction survives the round-trip (a
// nil tier reads back nil, a zero tier reads back a real 0).
func testTokenLimits(t *testing.T, s store.Store) {
	ctx := context.Background()
	i64 := func(v int64) *int64 { return &v }

	all, err := s.TokenLimitsAll(ctx)
	if err != nil {
		t.Fatalf("TokenLimitsAll (empty): %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("fresh store has %d limit rows, want 0", len(all))
	}

	// Operator-global: hard only (soft unset). Tenant: both. User: soft=0 (a
	// real zero, distinct from unset) + hard nil.
	rows := []store.TokenLimitRow{
		{TenantID: "", Scope: "operator", ScopeID: "", HardLimit: i64(1_000_000), UpdatedBy: "admin"},
		{TenantID: "acme", Scope: "tenant", ScopeID: "", SoftLimit: i64(500), HardLimit: i64(800), UpdatedBy: "admin"},
		{TenantID: "acme", Scope: "user", ScopeID: "u1", SoftLimit: i64(0), UpdatedBy: "op@acme"},
	}
	for _, r := range rows {
		if err := s.TokenLimitPut(ctx, r); err != nil {
			t.Fatalf("TokenLimitPut(%s/%s): %v", r.Scope, r.ScopeID, err)
		}
	}

	byKey := func() map[string]store.TokenLimitRow {
		got, err := s.TokenLimitsAll(ctx)
		if err != nil {
			t.Fatalf("TokenLimitsAll: %v", err)
		}
		m := make(map[string]store.TokenLimitRow, len(got))
		for _, r := range got {
			m[r.TenantID+"|"+r.Scope+"|"+r.ScopeID] = r
		}
		return m
	}

	m := byKey()
	if len(m) != 3 {
		t.Fatalf("got %d rows, want 3", len(m))
	}
	op := m["|operator|"]
	if op.SoftLimit != nil {
		t.Errorf("operator soft = %v, want nil (unset)", *op.SoftLimit)
	}
	if op.HardLimit == nil || *op.HardLimit != 1_000_000 {
		t.Errorf("operator hard = %v, want 1000000", op.HardLimit)
	}
	usr := m["acme|user|u1"]
	if usr.SoftLimit == nil || *usr.SoftLimit != 0 {
		t.Errorf("user soft = %v, want a real 0 (not nil)", usr.SoftLimit)
	}
	if usr.HardLimit != nil {
		t.Errorf("user hard = %v, want nil", *usr.HardLimit)
	}

	// Upsert overwrite: raise the tenant hard tier + clear its soft tier.
	if err := s.TokenLimitPut(ctx, store.TokenLimitRow{
		TenantID: "acme", Scope: "tenant", ScopeID: "", HardLimit: i64(2000), UpdatedBy: "admin2",
	}); err != nil {
		t.Fatalf("TokenLimitPut (overwrite): %v", err)
	}
	m = byKey()
	if len(m) != 3 {
		t.Fatalf("after overwrite got %d rows, want 3 (upsert must not insert a dup)", len(m))
	}
	tn := m["acme|tenant|"]
	if tn.SoftLimit != nil {
		t.Errorf("tenant soft after overwrite = %v, want nil (cleared)", *tn.SoftLimit)
	}
	if tn.HardLimit == nil || *tn.HardLimit != 2000 {
		t.Errorf("tenant hard after overwrite = %v, want 2000", tn.HardLimit)
	}
	if tn.UpdatedBy != "admin2" {
		t.Errorf("tenant updated_by = %q, want admin2", tn.UpdatedBy)
	}

	// Delete the user row → unlimited again. Deleting a missing row is a no-op.
	if err := s.TokenLimitDelete(ctx, "acme", "user", "u1"); err != nil {
		t.Fatalf("TokenLimitDelete: %v", err)
	}
	if err := s.TokenLimitDelete(ctx, "acme", "user", "nope"); err != nil {
		t.Fatalf("TokenLimitDelete (absent) should be a no-op: %v", err)
	}
	m = byKey()
	if _, ok := m["acme|user|u1"]; ok {
		t.Error("user row still present after delete")
	}
	if len(m) != 2 {
		t.Fatalf("after delete got %d rows, want 2", len(m))
	}
}

// testSessionArchiver is the fix-4 regression: the aged-run archiver prunes by
// SESSION, not by run — a session is prunable only when EVERY run in it is
// terminal and old, and DeleteSessionCascade removes the whole session (runs +
// events + session row) while leaving token_usage (independent retention)
// intact. Pruning one aged run inside a still-active session would corrupt the
// continuation replay (GetTranscript reads the whole session), so a session with
// any running run is never prunable.
func testSessionArchiver(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Session A: two completed runs, all terminal + old → prunable wholesale.
	sessA, _ := s.CreateSession(ctx, "t", "default", "")
	runA1, _ := s.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "arch-a1"})
	runA2, _ := s.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "arch-a2"})
	for _, e := range []string{"text", "done"} {
		if err := s.AppendEvent(ctx, runA1.ID, e, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: runA1.ID, SessionID: sessA.ID, TenantID: "t", Provider: "p", Model: "m", CredentialSource: "operator", InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{runA1.ID, runA2.ID} {
		if err := s.FinishRun(ctx, id, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Session B: one completed run + one still-running run → NOT prunable (the
	// running run would lose its session's transcript).
	sessB, _ := s.CreateSession(ctx, "t", "default", "")
	runB1, _ := s.CreateRun(ctx, sessB.ID, store.RunIdentity{AgentID: "arch-b1"})
	runB2, _ := s.CreateRun(ctx, sessB.ID, store.RunIdentity{AgentID: "arch-b2"}) // stays running
	if err := s.FinishRun(ctx, runB1.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatal(err)
	}
	_ = runB2

	// A future cutoff → session A qualifies (all terminal); session B does not.
	got, err := s.PrunableAgedSessions(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("PrunableAgedSessions: %v", err)
	}
	ids := map[string]bool{}
	for _, id := range got {
		ids[id] = true
	}
	if !ids[sessA.ID] {
		t.Errorf("prunable set missing the all-terminal session A: %v", got)
	}
	if ids[sessB.ID] {
		t.Errorf("prunable set included session B, which has a RUNNING run: %v", got)
	}
	// A past cutoff → nothing (session A's runs completed just now).
	if old, _ := s.PrunableAgedSessions(ctx, time.Now().Add(-time.Hour), 100); len(old) != 0 {
		t.Errorf("past-cutoff prunable = %d, want 0", len(old))
	}

	// RunsForSession returns every run in the session (for export).
	runs, err := s.RunsForSession(ctx, sessA.ID)
	if err != nil {
		t.Fatalf("RunsForSession: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("RunsForSession(A) = %d runs, want 2", len(runs))
	}

	// DeleteSessionCascade: runs + events + session row gone, token_usage kept.
	if err := s.DeleteSessionCascade(ctx, sessA.ID); err != nil {
		t.Fatalf("DeleteSessionCascade: %v", err)
	}
	if _, err := s.GetRun(ctx, runA1.ID); err == nil {
		t.Errorf("runA1 still present after cascade delete")
	}
	if _, err := s.GetRun(ctx, runA2.ID); err == nil {
		t.Errorf("runA2 still present after cascade delete")
	}
	if _, err := s.GetSession(ctx, sessA.ID); err == nil {
		t.Errorf("session A row survived cascade delete")
	}
	if evs, _ := s.GetRunEventsSince(ctx, runA1.ID, 0, 100); len(evs) != 0 {
		t.Errorf("session A events survived cascade delete: %d", len(evs))
	}
	if usage, _ := s.TokenUsageForRun(ctx, runA1.ID); len(usage) != 1 {
		t.Errorf("token_usage for deleted session = %d, want 1 (usage retention is independent)", len(usage))
	}
	// Session B untouched.
	if _, err := s.GetRun(ctx, runB2.ID); err != nil {
		t.Errorf("session B run was affected: %v", err)
	}
}

// testPrunableAgedSessionsExcludesPinned is the RFC BM Phase 2 pinned-exemption
// regression: a PINNED session, even when all its runs are terminal + old, must
// NEVER be returned by PrunableAgedSessions — pinning is the operator's explicit
// "keep this" and exempts the chat from ALL automated retention (the chats
// sweeper AND the legacy usage archiver both consume this list). An identical
// UNpinned session in the same state IS returned. Fails on the pre-fix query
// (no pinned exclusion) — the pinned session would appear in the prunable set.
func testPrunableAgedSessionsExcludesPinned(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Two sessions, each with one completed run — identical except one is pinned.
	mkAged := func(agentID string) string {
		sess, err := s.CreateSession(ctx, "t", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
			t.Fatal(err)
		}
		return sess.ID
	}
	pinnedID := mkAged("pinned-a")
	unpinnedID := mkAged("unpinned-a")
	if err := s.SetSessionMeta(ctx, pinnedID, store.SessionMetaPatch{Pinned: boolPtr(true)}); err != nil {
		t.Fatalf("SetSessionMeta pin: %v", err)
	}

	// A future cutoff → both sessions are all-terminal and old enough to qualify;
	// only the pinned exemption should keep the pinned one out.
	got, err := s.PrunableAgedSessions(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("PrunableAgedSessions: %v", err)
	}
	ids := map[string]bool{}
	for _, id := range got {
		ids[id] = true
	}
	if ids[pinnedID] {
		t.Errorf("pinned session %s returned as prunable; pinning must exempt it from all retention: %v", pinnedID, got)
	}
	if !ids[unpinnedID] {
		t.Errorf("unpinned aged session %s missing from prunable set: %v", unpinnedID, got)
	}
}

// testHookTenantRoundTrip exercises the cluster hook-registry SQL — the RFC AF
// (v1.0.1) tenant_id column's INSERT/SELECT/Scan ordinals — on a REAL backend.
// SQLite's hook DB methods are unsupported (single-replica keeps hooks
// in-memory), so it skips; the go-postgres CI job runs the real round-trip.
// Guards the BUG-2 class: a shifted placeholder/scan that the SQLite default
// CI path can't catch would silently corrupt the authoritative tenant.
func testHookTenantRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	rowA := store.HookRow{
		ID: "hook_a", Tenant: "tenant-a", Owner: "jobs-search-web", Name: "url-gate",
		Phase: "pre", Agents: []string{"qa", "company-*"}, Tools: []string{"WebFetch"},
		CallbackURL: "https://a/cb", FailMode: "closed", TimeoutMs: 3000,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond), CreatedByReplica: "r1",
	}
	if err := s.CreateHook(ctx, rowA); err != nil {
		if errors.Is(err, store.ErrHooksUnsupported) {
			t.Skipf("backend has no DB-backed hook registry: %v", err)
		}
		t.Fatalf("CreateHook: %v", err)
	}
	// A second hook with the SAME owner+name under a DIFFERENT tenant must
	// coexist at the row level (the table keys on id; both tenants persist
	// distinctly — the registry layer enforces isolation on top).
	rowB := rowA
	rowB.ID, rowB.Tenant, rowB.CallbackURL = "hook_b", "tenant-b", "https://b/cb"
	if err := s.CreateHook(ctx, rowB); err != nil {
		t.Fatalf("CreateHook B: %v", err)
	}

	// GetHookByID round-trips every field, incl. the new tenant_id — a column
	// ordinal shift in the SELECT/Scan would surface here as a wrong field.
	got, err := s.GetHookByID(ctx, "hook_a")
	if err != nil {
		t.Fatalf("GetHookByID: %v", err)
	}
	if got.Tenant != "tenant-a" || got.Owner != "jobs-search-web" || got.Name != "url-gate" ||
		got.Phase != "pre" || got.CallbackURL != "https://a/cb" || got.FailMode != "closed" ||
		got.TimeoutMs != 3000 || got.CreatedByReplica != "r1" {
		t.Errorf("GetHookByID round-trip mismatch (column ordinal shift?): %+v", got)
	}
	if len(got.Agents) != 2 || got.Agents[0] != "qa" || got.Agents[1] != "company-*" ||
		len(got.Tools) != 1 || got.Tools[0] != "WebFetch" {
		t.Errorf("GetHookByID jsonb agents/tools mismatch: agents=%v tools=%v", got.Agents, got.Tools)
	}

	// ListHooks returns both rows, each with its own tenant.
	all, err := s.ListHooks(ctx)
	if err != nil {
		t.Fatalf("ListHooks: %v", err)
	}
	byID := map[string]store.HookRow{}
	for _, r := range all {
		byID[r.ID] = r
	}
	if byID["hook_a"].Tenant != "tenant-a" {
		t.Errorf("ListHooks hook_a tenant = %q, want tenant-a", byID["hook_a"].Tenant)
	}
	if byID["hook_b"].Tenant != "tenant-b" {
		t.Errorf("ListHooks hook_b tenant = %q, want tenant-b", byID["hook_b"].Tenant)
	}

	// Delete hook_a; hook_b (distinct tenant, same owner+name) survives.
	if err := s.DeleteHook(ctx, "hook_a"); err != nil {
		t.Fatalf("DeleteHook: %v", err)
	}
	var nf *store.ErrNotFound
	if _, err := s.GetHookByID(ctx, "hook_a"); !errors.As(err, &nf) {
		t.Errorf("GetHookByID after delete: got %v (%T), want *store.ErrNotFound", err, err)
	}
	if survivor, err := s.GetHookByID(ctx, "hook_b"); err != nil || survivor.Tenant != "tenant-b" {
		t.Errorf("hook_b should survive hook_a's delete: row=%+v err=%v", survivor, err)
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

// --- RFC BE: ListSessions + SetSessionMeta ---

func strPtr(s string) *string      { return &s }
func boolPtr(b bool) *bool         { return &b }
func tagsPtr(t []string) *[]string { return &t }

func summaryIDs(list []store.SessionSummary) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = x.SessionID
	}
	return out
}

// testListSessions exercises the RFC BE browse/search surface: every filter axis
// independently, archived exclude-by-default, pinned-first ordering, the
// token/cost/run_count aggregation, and offset pagination — across two tenants,
// two users, and two agents.
// testListSessionsTagEscaping is the RFC BE review regression: a tag containing
// a JSON-special character (a double-quote) must still be matched by the tag
// filter. Before the fix the filter searched for the raw `"tag"` needle while
// EncodeTags stored the tag JSON-escaped, so such a tag could never match its
// own chat. Runs on both backends (the fix is shared store.EncodeTagMatch).
func testListSessionsTagEscaping(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t1", "agentA", "alice")
	if err != nil {
		t.Fatal(err)
	}
	const quoted = `he said "hi"`
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{
		Tags: tagsPtr([]string{quoted, "plain"}),
	}); err != nil {
		t.Fatal(err)
	}
	// The quote-bearing tag matches its chat.
	list, _, err := s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", Tag: quoted}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SessionID != sess.ID {
		t.Fatalf("tag=%q = %v, want [%s]", quoted, summaryIDs(list), sess.ID)
	}
	// A different quoted tag does NOT match (no accidental widening).
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", Tag: `she said "bye"`}, 50, 0)
	if len(list) != 0 {
		t.Fatalf("tag=%q = %v, want []", `she said "bye"`, summaryIDs(list))
	}
}

func testListSessions(t *testing.T, s store.Store) {
	ctx := context.Background()

	// t1 / alice / agentA — 2 completed runs (tokens + cost aggregate).
	s1, _ := s.CreateSession(ctx, "t1", "agentA", "alice")
	r11, _ := s.CreateRun(ctx, s1.ID, store.RunIdentity{UserID: "alice", TenantID: "t1"})
	r12, _ := s.CreateRun(ctx, s1.ID, store.RunIdentity{UserID: "alice", TenantID: "t1"})
	if err := s.FinishRun(ctx, r11.ID, store.RunCompleted, "end_turn", store.Usage{InputTokens: 100, OutputTokens: 50, Cost: 0.5, CostCurrency: "USD", Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, r12.ID, store.RunCompleted, "end_turn", store.Usage{InputTokens: 10, OutputTokens: 5, Cost: 0.25, CostCurrency: "USD", Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionMeta(ctx, s1.ID, store.SessionMetaPatch{
		Title: strPtr("Quarterly Review"),
		Tags:  tagsPtr([]string{"urgent", "q3"}),
	}); err != nil {
		t.Fatal(err)
	}

	// t1 / bob / agentB — 1 still-running run.
	s2, _ := s.CreateSession(ctx, "t1", "agentB", "bob")
	if _, err := s.CreateRun(ctx, s2.ID, store.RunIdentity{UserID: "bob", TenantID: "t1"}); err != nil {
		t.Fatal(err)
	}

	// t2 / carol / agentA — 1 completed run (different tenant).
	s3, _ := s.CreateSession(ctx, "t2", "agentA", "carol")
	r31, _ := s.CreateRun(ctx, s3.ID, store.RunIdentity{UserID: "carol", TenantID: "t2"})
	_ = s.FinishRun(ctx, r31.ID, store.RunCompleted, "end_turn", store.Usage{InputTokens: 20, OutputTokens: 10, Cost: 0.2, CostCurrency: "USD", Model: "m", Provider: "p"}, "")

	// t1 / alice / agentA — pinned; tag "q3-plan" (quote-boundary check vs "q3").
	s4, _ := s.CreateSession(ctx, "t1", "agentA", "alice")
	r41, _ := s.CreateRun(ctx, s4.ID, store.RunIdentity{UserID: "alice", TenantID: "t1"})
	_ = s.FinishRun(ctx, r41.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, "")
	if err := s.SetSessionMeta(ctx, s4.ID, store.SessionMetaPatch{Pinned: boolPtr(true), Tags: tagsPtr([]string{"q3-plan"})}); err != nil {
		t.Fatal(err)
	}

	// t1 / alice — archived (excluded by default).
	s5, _ := s.CreateSession(ctx, "t1", "agentA", "alice")
	if err := s.SetSessionMeta(ctx, s5.ID, store.SessionMetaPatch{Archived: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	byID := func(list []store.SessionSummary) map[string]store.SessionSummary {
		m := map[string]store.SessionSummary{}
		for _, x := range list {
			m[x.SessionID] = x
		}
		return m
	}

	// 1. Tenant t1, default (archived excluded): S1, S2, S4.
	list, total, err := s.ListSessions(ctx, store.SessionFilter{TenantID: "t1"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("t1 total = %d, want 3 (%v)", total, summaryIDs(list))
	}
	m := byID(list)
	if _, ok := m[s1.ID]; !ok {
		t.Errorf("t1 list missing s1")
	}
	if _, ok := m[s3.ID]; ok {
		t.Errorf("t1 list leaked t2's s3 (tenant isolation)")
	}
	if _, ok := m[s5.ID]; ok {
		t.Errorf("t1 list included archived s5 by default")
	}
	// Pinned-first ordering.
	if len(list) == 0 || list[0].SessionID != s4.ID {
		t.Errorf("pinned session should sort first; got %v", summaryIDs(list))
	}
	// Aggregation on S1.
	agg := m[s1.ID]
	if agg.RunCount != 2 || agg.InputTokens != 110 || agg.OutputTokens != 55 {
		t.Errorf("s1 aggregate = run_count %d in %d out %d, want 2/110/55", agg.RunCount, agg.InputTokens, agg.OutputTokens)
	}
	if agg.Cost < 0.75-1e-9 || agg.Cost > 0.75+1e-9 {
		t.Errorf("s1 cost = %v, want 0.75", agg.Cost)
	}
	if agg.Status != store.RunCompleted {
		t.Errorf("s1 status = %q, want completed", agg.Status)
	}
	if agg.Title != "Quarterly Review" {
		t.Errorf("s1 title = %q, want Quarterly Review", agg.Title)
	}
	if len(agg.Tags) != 2 {
		t.Errorf("s1 tags = %v, want [urgent q3]", agg.Tags)
	}
	// S2 (running) reports a "running" derived status.
	if m[s2.ID].Status != store.RunRunning {
		t.Errorf("s2 status = %q, want running", m[s2.ID].Status)
	}

	// 2. Tenant + user filter → S1, S4 (both alice); not S2 (bob).
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", UserID: "alice"}, 50, 0)
	m = byID(list)
	if len(list) != 2 || m[s1.ID].SessionID == "" || m[s4.ID].SessionID == "" {
		t.Errorf("t1/alice = %v, want [s1 s4]", summaryIDs(list))
	}

	// 3. Agent filter.
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", AgentName: "agentB"}, 50, 0)
	if len(list) != 1 || list[0].SessionID != s2.ID {
		t.Errorf("t1/agentB = %v, want [s2]", summaryIDs(list))
	}

	// 4. Status filter.
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", Status: store.RunRunning}, 50, 0)
	if len(list) != 1 || list[0].SessionID != s2.ID {
		t.Errorf("t1/running = %v, want [s2]", summaryIDs(list))
	}
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", Status: store.RunCompleted}, 50, 0)
	if m = byID(list); len(list) != 2 || m[s1.ID].SessionID == "" || m[s4.ID].SessionID == "" {
		t.Errorf("t1/completed = %v, want [s1 s4]", summaryIDs(list))
	}

	// 5. Archived inclusion → 4 (adds S5).
	_, total, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", IncludeArchived: true}, 50, 0)
	if total != 4 {
		t.Errorf("t1 include-archived total = %d, want 4", total)
	}

	// 6. Pinned-only filter.
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", IncludePinned: true}, 50, 0)
	if len(list) != 1 || list[0].SessionID != s4.ID {
		t.Errorf("t1/pinned-only = %v, want [s4]", summaryIDs(list))
	}

	// 7. Tag filter — exact-token, quote-boundary safe ("q3" must NOT match "q3-plan").
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", Tag: "q3"}, 50, 0)
	if len(list) != 1 || list[0].SessionID != s1.ID {
		t.Errorf("t1/tag=q3 = %v, want [s1] (must not match s4's \"q3-plan\")", summaryIDs(list))
	}

	// 8. Title-contains (case-insensitive).
	list, _, _ = s.ListSessions(ctx, store.SessionFilter{TenantID: "t1", TitleContains: "quarterly"}, 50, 0)
	if len(list) != 1 || list[0].SessionID != s1.ID {
		t.Errorf("t1/title~quarterly = %v, want [s1]", summaryIDs(list))
	}

	// 9. Pagination (limit/offset + stable total).
	page1, total, _ := s.ListSessions(ctx, store.SessionFilter{TenantID: "t1"}, 2, 0)
	if len(page1) != 2 || total != 3 {
		t.Errorf("page1 = %d rows total %d, want 2/3", len(page1), total)
	}
	page2, total2, _ := s.ListSessions(ctx, store.SessionFilter{TenantID: "t1"}, 2, 2)
	if len(page2) != 1 || total2 != 3 {
		t.Errorf("page2 = %d rows total %d, want 1/3", len(page2), total2)
	}

	// 10. All-tenants (empty TenantID) spans t1 + t2 non-archived: S1,S2,S4,S3 = 4.
	_, total, _ = s.ListSessions(ctx, store.SessionFilter{}, 50, 0)
	if total != 4 {
		t.Errorf("all-tenants total = %d, want 4", total)
	}
}

// testListSessionsTenantIsolation asserts a tenant-scoped ListSessions never
// returns another tenant's chats (RFC BE — the load-bearing isolation contract).
func testListSessionsTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, _ := s.CreateSession(ctx, "A", "agentA", "u1")
	if _, err := s.CreateRun(ctx, a.ID, store.RunIdentity{UserID: "u1", TenantID: "A"}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateSession(ctx, "B", "agentB", "u2")
	if _, err := s.CreateRun(ctx, b.ID, store.RunIdentity{UserID: "u2", TenantID: "B"}); err != nil {
		t.Fatal(err)
	}

	list, total, err := s.ListSessions(ctx, store.SessionFilter{TenantID: "A"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(list) != 1 || list[0].SessionID != a.ID {
		t.Fatalf("tenant A list = %v (total %d), want only [%s]", summaryIDs(list), total, a.ID)
	}
	for _, x := range list {
		if x.SessionID == b.ID || x.TenantID == "B" {
			t.Errorf("tenant B's session %s leaked into tenant A's list", b.ID)
		}
	}
}

// testSetSessionMeta round-trips every metadata field, asserts a partial patch
// leaves the rest unchanged, archive sets/clears archived_at, a summary write
// stamps summary_updated_at, and a missing id returns ErrNotFound (RFC BE).
func testSetSessionMeta(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agentA", "alice")

	// Round-trip each field.
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{
		Title:       strPtr("My Chat"),
		Description: strPtr("a description"),
		Summary:     strPtr("a recap"),
		Tags:        tagsPtr([]string{"a", "b"}),
		Pinned:      boolPtr(true),
		Archived:    boolPtr(true),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "My Chat" || got.Description != "a description" || got.Summary != "a recap" {
		t.Errorf("meta round-trip: title=%q desc=%q summary=%q", got.Title, got.Description, got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("tags round-trip = %v, want [a b]", got.Tags)
	}
	if !got.Pinned {
		t.Errorf("pinned not set")
	}
	if got.ArchivedAt.IsZero() {
		t.Errorf("archived_at not stamped on archive")
	}
	if got.SummaryUpdatedAt.IsZero() {
		t.Errorf("summary_updated_at not stamped on summary write")
	}
	firstSummaryStamp := got.SummaryUpdatedAt

	// Partial patch: only description changes; the rest survive untouched.
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{Description: strPtr("changed")}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if got.Description != "changed" {
		t.Errorf("partial patch description = %q, want changed", got.Description)
	}
	if got.Title != "My Chat" || !got.Pinned || len(got.Tags) != 2 || got.Summary != "a recap" {
		t.Errorf("partial patch clobbered other fields: title=%q pinned=%v tags=%v summary=%q",
			got.Title, got.Pinned, got.Tags, got.Summary)
	}

	// Un-archive clears archived_at.
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{Archived: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if !got.ArchivedAt.IsZero() {
		t.Errorf("archived_at not cleared on un-archive: %v", got.ArchivedAt)
	}

	// A fresh summary write refreshes summary_updated_at (idempotent recap).
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{Summary: strPtr("newer recap")}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if got.Summary != "newer recap" {
		t.Errorf("summary re-write = %q, want 'newer recap'", got.Summary)
	}
	if got.SummaryUpdatedAt.Before(firstSummaryStamp) {
		t.Errorf("summary_updated_at went backwards on re-write: %v < %v", got.SummaryUpdatedAt, firstSummaryStamp)
	}

	// Tags can be cleared to an explicit empty set (reads back as no tags).
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{Tags: tagsPtr([]string{})}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if len(got.Tags) != 0 {
		t.Errorf("cleared tags = %v, want empty", got.Tags)
	}

	// ErrNotFound on a missing id (patch present).
	var nf *store.ErrNotFound
	if err := s.SetSessionMeta(ctx, "s_does_not_exist", store.SessionMetaPatch{Title: strPtr("x")}); !errors.As(err, &nf) {
		t.Errorf("SetSessionMeta on missing id: got %v (%T), want *store.ErrNotFound", err, err)
	}
	// ErrNotFound on a missing id (empty patch).
	if err := s.SetSessionMeta(ctx, "s_missing2", store.SessionMetaPatch{}); !errors.As(err, &nf) {
		t.Errorf("empty patch on missing id: got %v, want *store.ErrNotFound", err)
	}
	// Empty patch on an existing id is a no-op (nil error).
	if err := s.SetSessionMeta(ctx, sess.ID, store.SessionMetaPatch{}); err != nil {
		t.Errorf("empty patch on existing id: got %v, want nil", err)
	}
}

// simIDs / containsSim are the []SessionSimilar analogues of summaryIDs /
// contains for the RFC BE op=related contract tests.
func simIDs(list []store.SessionSimilar) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = x.SessionID
	}
	return out
}

func containsSim(list []store.SessionSimilar, id string) bool {
	for _, x := range list {
		if x.SessionID == id {
			return true
		}
	}
	return false
}

// testSessionEmbedUpsertSearch round-trips the RFC BE per-session embedding
// index: an upsert on a missing session is an opaque not-found; a cosine search
// ranks chats by similarity to the query (monotone-DESC scores); limit caps the
// page; an upsert REPLACES the vector; and archived chats are excluded by
// default. Hand-built 4-D unit vectors make the ordering hand-verifiable (the
// same style as the memory-embed contract). Unlike memory_embeddings this runs
// on BOTH backends — the session index ranks in Go and needs no vector extension.
func testSessionEmbedUpsertSearch(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Upsert on a missing session is an opaque not-found (no row invented).
	var nf *store.ErrNotFound
	if err := s.SessionEmbedUpsert(ctx, "s_nope", store.SessionEmbedding{
		Provider: "eval", Model: "m", Dimension: 4, Vector: floats32(1, 0, 0, 0),
	}); !errors.As(err, &nf) {
		t.Fatalf("SessionEmbedUpsert on missing session: got %v (%T), want *store.ErrNotFound", err, err)
	}

	// Three chats in one tenant, embedded in different 4-D directions.
	aligned, _ := s.CreateSession(ctx, "t1", "agentA", "alice")
	off45, _ := s.CreateSession(ctx, "t1", "agentA", "alice")
	ortho, _ := s.CreateSession(ctx, "t1", "agentA", "alice")
	seed := []struct {
		id  string
		vec []float32
	}{
		{aligned.ID, floats32(1, 0, 0, 0)},         // aligned with the query
		{off45.ID, floats32(0.7071, 0.7071, 0, 0)}, // 45° off
		{ortho.ID, floats32(0, 1, 0, 0)},           // orthogonal
	}
	for _, e := range seed {
		if err := s.SessionEmbedUpsert(ctx, e.id, store.SessionEmbedding{
			Provider: "eval", Model: "m", Dimension: 4, Vector: e.vec,
		}); err != nil {
			t.Fatalf("SessionEmbedUpsert %s: %v", e.id, err)
		}
	}

	res, err := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "t1"}, floats32(1, 0, 0, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("got %d results, want 3 (%v)", len(res), simIDs(res))
	}
	if res[0].SessionID != aligned.ID || res[1].SessionID != off45.ID || res[2].SessionID != ortho.ID {
		t.Errorf("ranking = %v, want [aligned off45 ortho]", simIDs(res))
	}
	if !(res[0].Score >= res[1].Score && res[1].Score >= res[2].Score) {
		t.Errorf("scores not monotone-DESC: %v", []float64{res[0].Score, res[1].Score, res[2].Score})
	}

	// limit caps the returned rows.
	top1, err := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "t1"}, floats32(1, 0, 0, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(top1) != 1 || top1[0].SessionID != aligned.ID {
		t.Errorf("limit=1 = %v, want [aligned]", simIDs(top1))
	}

	// An upsert REPLACES the vector: re-point `ortho` at the query and it now wins.
	if err := s.SessionEmbedUpsert(ctx, ortho.ID, store.SessionEmbedding{
		Provider: "eval", Model: "m", Dimension: 4, Vector: floats32(1, 0, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	re, _ := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "t1"}, floats32(1, 0, 0, 0), 1)
	if len(re) != 1 || re[0].SessionID != ortho.ID {
		t.Errorf("after re-embed, top = %v, want [ortho]", simIDs(re))
	}

	// Archived chats are excluded by default, included with IncludeArchived.
	if err := s.SetSessionMeta(ctx, aligned.ID, store.SessionMetaPatch{Archived: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	def, _ := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "t1"}, floats32(1, 0, 0, 0), 10)
	if containsSim(def, aligned.ID) {
		t.Errorf("archived chat must be excluded by default; got %v", simIDs(def))
	}
	inc, _ := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "t1", IncludeArchived: true}, floats32(1, 0, 0, 0), 10)
	if !containsSim(inc, aligned.ID) {
		t.Errorf("IncludeArchived should surface the archived chat; got %v", simIDs(inc))
	}
}

// testSessionEmbedSearchTenantFold is the load-bearing isolation contract for
// op=related: the SAME (closest-possible) vector stored in two tenants — a
// tenant-A similarity search must NEVER surface tenant B's chat, and a
// user-scoped search must not surface another user's chat within the tenant.
func testSessionEmbedSearchTenantFold(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, _ := s.CreateSession(ctx, "A", "agentA", "alice")
	b, _ := s.CreateSession(ctx, "B", "agentB", "bob")
	for _, id := range []string{a.ID, b.ID} {
		if err := s.SessionEmbedUpsert(ctx, id, store.SessionEmbedding{
			Provider: "eval", Model: "m", Dimension: 4, Vector: floats32(1, 0, 0, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "A"}, floats32(1, 0, 0, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].SessionID != a.ID {
		t.Fatalf("tenant A similarity search = %v, want only [%s]", simIDs(res), a.ID)
	}
	for _, r := range res {
		if r.SessionID == b.ID || r.TenantID == "B" {
			t.Errorf("tenant B chat %s leaked into tenant A similarity search", b.ID)
		}
	}

	// user-scope fold within a tenant: another user's identical-vector chat is
	// excluded when the filter carries UserID.
	carol, _ := s.CreateSession(ctx, "A", "agentA", "carol")
	if err := s.SessionEmbedUpsert(ctx, carol.ID, store.SessionEmbedding{
		Provider: "eval", Model: "m", Dimension: 4, Vector: floats32(1, 0, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	byUser, _ := s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: "A", UserID: "alice"}, floats32(1, 0, 0, 0), 10)
	if len(byUser) != 1 || byUser[0].SessionID != a.ID {
		t.Errorf("user-scope similarity fold = %v, want only alice's [%s]", simIDs(byUser), a.ID)
	}
}

func testListUsers(t *testing.T, s store.Store) {
	ctx := context.Background()
	sessAlice, _ := s.CreateSession(ctx, "t", "a", "alice")
	sessBob, _ := s.CreateSession(ctx, "t", "a", "bob")

	// alice: 2 runs, 1 still running. Tenant "t" on every run so the
	// tenant-scoped ListUsers filter has something to match.
	rA1, _ := s.CreateRun(ctx, sessAlice.ID, store.RunIdentity{AgentID: "a_alice1", UserID: "alice", TenantID: "t"})
	_, _ = s.CreateRun(ctx, sessAlice.ID, store.RunIdentity{AgentID: "a_alice2", UserID: "alice", TenantID: "t"})
	_ = s.FinishRun(ctx, rA1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// bob: 1 run, completed.
	rB1, _ := s.CreateRun(ctx, sessBob.ID, store.RunIdentity{AgentID: "a_bob1", UserID: "bob", TenantID: "t"})
	_ = s.FinishRun(ctx, rB1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// carol lives in a different tenant — must be invisible when the
	// listing is scoped to tenant "t".
	sessCarol, _ := s.CreateSession(ctx, "other", "a", "carol")
	_, _ = s.CreateRun(ctx, sessCarol.ID, store.RunIdentity{AgentID: "a_carol1", UserID: "carol", TenantID: "other"})

	// Empty-userID run should NOT show up in the listing — filtered
	// by the WHERE user_id != '' clause.
	sessAnon, _ := s.CreateSession(ctx, "t", "a", "")
	_, _ = s.CreateRun(ctx, sessAnon.ID, store.RunIdentity{AgentID: "a_anon", UserID: "", TenantID: "t"})

	// Tenant-scoped listing: only tenant "t"'s users (alice + bob), carol
	// excluded.
	users, err := s.ListUsers(ctx, "t")
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

	// All-tenants listing (tenantID "") includes carol from "other".
	all, err := s.ListUsers(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	allIDs := map[string]bool{}
	for _, u := range all {
		allIDs[u.UserID] = true
	}
	for _, want := range []string{"alice", "bob", "carol"} {
		if !allIDs[want] {
			t.Errorf("all-tenants ListUsers missing %q (got %v)", want, allIDs)
		}
	}

	// Focusing a tenant with no users returns an empty list, not an error.
	none, err := s.ListUsers(ctx, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("ListUsers(nonexistent) = %d users, want 0", len(none))
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

// testSweepStaleRunsSkipsPaused pins the F42 / RFC X Phase 2 sweeper guard:
// a run that's intentionally parked (pause_state='paused') is NEVER swept
// stale even when it has no heartbeat and an old started_at — otherwise a
// snapshotted+restored paused run would be marked failed before resume
// re-dispatches it. A stale pause_state='running' run is still swept.
//
// FAIL-BEFORE: the sweeper UPDATE had no pause_state guard, so the paused row
// (no heartbeat, old started_at) matched and was marked failed.
func testSweepStaleRunsSkipsPaused(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	// A paused run with no heartbeat — represents a restored mid-run snapshot.
	paused, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_paused_skip"})
	if err := s.SetRunPauseState(ctx, paused.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	// A normal running run with no heartbeat — must still be swept.
	_, _ = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_running_sweep"})

	// Both created before this cutoff (no heartbeat → started_at branch).
	time.Sleep(50 * time.Millisecond)
	cutoff := time.Now()

	swept, err := s.SweepStaleRuns(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 {
		t.Errorf("SweepStaleRuns swept %d, want 1 (only the running run; the paused one is skipped)", swept)
	}
	pausedAfter, _ := s.GetRunByAgentID(ctx, "a_paused_skip")
	if pausedAfter.Status != store.RunRunning {
		t.Errorf("paused run was swept: status=%q, want running (paused runs are intentionally parked)", pausedAfter.Status)
	}
	runningAfter, _ := s.GetRunByAgentID(ctx, "a_running_sweep")
	if runningAfter.Status != store.RunFailed {
		t.Errorf("stale running run not swept: status=%q, want failed", runningAfter.Status)
	}
}

// testCreateRunInteractiveRoundTrip pins that runs.interactive (F42 / RFC X
// Phase 2) is persisted at CreateRun and round-trips on read — needed so a
// restored paused run re-dispatches with the correct park-vs-complete
// semantics. Default is false.
func testCreateRunInteractiveRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	batch, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_batch"})
	if got, _ := s.GetRun(ctx, batch.ID); got.Interactive {
		t.Errorf("batch run Interactive = true, want false (default)")
	}

	inter, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_inter", Interactive: true})
	if !inter.Interactive {
		t.Errorf("CreateRun returned Interactive=false for an interactive run")
	}
	got, _ := s.GetRun(ctx, inter.ID)
	if !got.Interactive {
		t.Errorf("interactive run Interactive=false on read-back (did not persist)")
	}
}

// RFC AX: the operator_key_restricted column round-trips on both backends —
// false by default (fail-open), true when stamped, and it survives a fresh read
// (so a resumed / snapshot-restored run reconstructs its restriction).
func testCreateRunOperatorKeyRestrictedRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	allowed, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ok_default"})
	if allowed.OperatorKeyRestricted {
		t.Errorf("default OperatorKeyRestricted = true, want false (fail-open)")
	}
	if got, _ := s.GetRun(ctx, allowed.ID); got.OperatorKeyRestricted {
		t.Errorf("default run OperatorKeyRestricted = true on read-back, want false")
	}

	restricted, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ok_restricted", OperatorKeyRestricted: true})
	if !restricted.OperatorKeyRestricted {
		t.Errorf("CreateRun returned OperatorKeyRestricted=false for a restricted run")
	}
	got, _ := s.GetRun(ctx, restricted.ID)
	if !got.OperatorKeyRestricted {
		t.Errorf("restricted run OperatorKeyRestricted=false on read-back (did not persist)")
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
	if err := s.MemorySet(ctx, "", store.MemoryScope("agent"), "agentA", "live", json.RawMessage(`"hello"`), 0); err != nil {
		t.Fatal(err)
	}
	// Expired row: TTL of 1ns; wait briefly so wall-clock advances past.
	if err := s.MemorySet(ctx, "", store.MemoryScope("agent"), "agentA", "expired", json.RawMessage(`"gone"`), time.Nanosecond); err != nil {
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
		if err := s.MemorySet(ctx, "", sd.scope, sd.sid, sd.key, json.RawMessage(`"x"`), 0); err != nil {
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
	if err := s.MemorySet(ctx, "", store.MemoryScopeUser, "alice", "voice", value, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "alice", "voice")
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
	_, err := s.MemoryGet(context.Background(), "", store.MemoryScopeAgent, "qa-agent", "missing")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
}

func testMemoryOverwriteUpdatesValue(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "summary", json.RawMessage(`"v1"`), 0); err != nil {
		t.Fatal(err)
	}
	first, _ := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "summary")
	time.Sleep(2 * time.Millisecond)
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "summary", json.RawMessage(`"v2"`), 0); err != nil {
		t.Fatal(err)
	}
	second, _ := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "summary")
	if string(second.Value) == string(first.Value) {
		t.Error("overwrite did not change the stored value")
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at not advanced: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
}

func testMemoryDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "u1", "k", json.RawMessage(`1`), 0)

	deleted, err := s.MemoryDelete(ctx, "", store.MemoryScopeUser, "u1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected deleted=true on a present key")
	}
	deleted, err = s.MemoryDelete(ctx, "", store.MemoryScopeUser, "u1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected deleted=false on a missing key")
	}
}

// testMemoryDeleteScope is the RFC BM Phase 3 bulk-scope delete: it removes every
// key under one (scope, scopeID) and returns the count, without touching a
// sibling scope_id or a different scope.
func testMemoryDeleteScope(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Target scope: three keys under (agent, "reaped").
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "reaped", "k1", json.RawMessage(`1`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "reaped", "k2", json.RawMessage(`2`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "reaped", "k3", json.RawMessage(`3`), 0)
	// A sibling agent scope_id and a same-id user scope must survive.
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "kept", "k1", json.RawMessage(`9`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "reaped", "k1", json.RawMessage(`8`), 0)

	n, err := s.MemoryDeleteScope(ctx, "", store.MemoryScopeAgent, "reaped")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("MemoryDeleteScope deleted %d, want 3", n)
	}
	if got, _, _ := s.MemoryList(ctx, "", store.MemoryScopeAgent, "reaped", "", 100); len(got) != 0 {
		t.Errorf("target scope still has %d keys after delete", len(got))
	}
	// Siblings untouched.
	if got, _, _ := s.MemoryList(ctx, "", store.MemoryScopeAgent, "kept", "", 100); len(got) != 1 {
		t.Errorf("sibling agent scope_id lost keys: have %d, want 1", len(got))
	}
	if got, _, _ := s.MemoryList(ctx, "", store.MemoryScopeUser, "reaped", "", 100); len(got) != 1 {
		t.Errorf("same-id user scope lost keys: have %d, want 1", len(got))
	}
	// Idempotent: a second delete removes nothing.
	n, err = s.MemoryDeleteScope(ctx, "", store.MemoryScopeAgent, "reaped")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second MemoryDeleteScope deleted %d, want 0", n)
	}
}

// testMemoryDeleteScopeCleansConsolidationRows: reclaiming a scope must also
// remove its consolidation state. memory_pending + memory_cursors are NOT
// FK-linked to memory (no cascade), so a scope delete that touched only the
// memory table would orphan them. FAIL-BEFORE: without the two extra deletes
// the enqueued pending row is still drainable and the leased cursor row
// survives after MemoryDeleteScope.
func testMemoryDeleteScopeCleansConsolidationRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scopeID = "reap-consol"

	// The target scope: a base k/v row, a queued consolidation row, a leased cursor.
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, scopeID, "k1", json.RawMessage(`1`), 0)
	if err := s.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
		Scope:     store.MemoryScopeAgent,
		ScopeID:   scopeID,
		Payload:   json.RawMessage(`{"n":1}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue target pending: %v", err)
	}
	if _, ok, err := s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner", time.Now().UTC(), time.Hour); err != nil || !ok {
		t.Fatalf("seed cursor lease: ok=%v err=%v", ok, err)
	}

	// A sibling scope's consolidation row must survive the reclaim.
	if err := s.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
		Scope:     store.MemoryScopeAgent,
		ScopeID:   "sibling",
		Payload:   json.RawMessage(`{"n":9}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue sibling pending: %v", err)
	}

	if _, err := s.MemoryDeleteScope(ctx, "", store.MemoryScopeAgent, scopeID); err != nil {
		t.Fatalf("MemoryDeleteScope: %v", err)
	}

	// The target's consolidation queue is empty.
	pend, err := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, scopeID, 10)
	if err != nil {
		t.Fatalf("MemoryPendingDrain(target): %v", err)
	}
	if len(pend) != 0 {
		t.Errorf("MemoryDeleteScope left %d pending rows; want 0 (consolidation queue must be reclaimed)", len(pend))
	}

	// The target's cursor is back to its zero-watermark, unleased default (row gone).
	cur, err := s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID)
	if err != nil {
		t.Fatalf("MemoryCursorGet(target): %v", err)
	}
	if cur.LeasedBy != "" || !cur.LeaseExpiresAt.IsZero() || !cur.WatermarkCompletedAt.IsZero() {
		t.Errorf("MemoryDeleteScope left a cursor row: %+v; want zero-default (cursor must be reclaimed)", cur)
	}

	// The sibling scope's pending row is untouched.
	sib, err := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, "sibling", 10)
	if err != nil {
		t.Fatalf("MemoryPendingDrain(sibling): %v", err)
	}
	if len(sib) != 1 {
		t.Errorf("sibling scope lost its pending rows: have %d, want 1", len(sib))
	}
}

// testMemorySupersedeHidesFromReads is the RFC BL P2 soft-archive: a superseded
// row disappears from get + list but is RETAINED (still counted by the operator
// scope-size summary, which does not filter superseded rows).
func testMemorySupersedeHidesFromReads(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "consol", "raw-1", json.RawMessage(`"one"`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "consol", "raw-2", json.RawMessage(`"two"`), 0)

	if err := s.MemorySupersede(ctx, "", store.MemoryScopeAgent, "consol", "raw-1"); err != nil {
		t.Fatalf("MemorySupersede: %v", err)
	}

	// get: the superseded key is now invisible (surfaced as not-found).
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "consol", "raw-1"); err == nil {
		t.Error("MemoryGet returned a superseded row; want not-found")
	} else {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			t.Errorf("MemoryGet(superseded) err = %v (%T), want *store.ErrNotFound", err, err)
		}
	}
	// The non-superseded sibling is still readable.
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "consol", "raw-2"); err != nil {
		t.Errorf("MemoryGet(live sibling): %v", err)
	}

	// list: only the live key.
	got, _, err := s.MemoryList(ctx, "", store.MemoryScopeAgent, "consol", "", 100)
	if err != nil {
		t.Fatalf("MemoryList: %v", err)
	}
	if len(got) != 1 || got[0].Key != "raw-2" {
		t.Errorf("MemoryList after supersede = %+v, want only raw-2", got)
	}

	// Retention: the row is kept — the operator scope-size summary (which does
	// NOT filter superseded rows) still counts both keys.
	sums, err := s.MemoryListScopeIDs(ctx, "", store.MemoryScopeAgent)
	if err != nil {
		t.Fatalf("MemoryListScopeIDs: %v", err)
	}
	var count int
	for _, sum := range sums {
		if sum.ScopeID == "consol" {
			count = sum.KeyCount
		}
	}
	if count != 2 {
		t.Errorf("scope key_count = %d after superseding 1 of 2; want 2 (row retained)", count)
	}
}

// testMemorySupersedeIsIdempotent: re-superseding an already-superseded row and
// superseding a missing key are both clean no-ops.
func testMemorySupersedeIsIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "u-consol", "k", json.RawMessage(`1`), 0)
	if err := s.MemorySupersede(ctx, "", store.MemoryScopeUser, "u-consol", "k"); err != nil {
		t.Fatalf("first supersede: %v", err)
	}
	if err := s.MemorySupersede(ctx, "", store.MemoryScopeUser, "u-consol", "k"); err != nil {
		t.Fatalf("second supersede (idempotent) should not error: %v", err)
	}
	if err := s.MemorySupersede(ctx, "", store.MemoryScopeUser, "u-consol", "missing"); err != nil {
		t.Fatalf("supersede of a missing key should be a no-op: %v", err)
	}
}

// testMemorySupersedeRevivedByWrite: a fresh write to a superseded key REVIVES
// it (clears superseded_at). set, incr, and the atomic-update ops must never
// black-hole a live write behind a stale supersede marker — a new explicit
// write is live data. Fail-before: without superseded_at=NULL on the upsert the
// value lands but stays hidden, so the post-write MemoryGet returns not-found.
func testMemorySupersedeRevivedByWrite(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scopeID = "revive"

	// set → supersede → set: the row comes back with the NEW value.
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, scopeID, "k", json.RawMessage(`"old"`), 0)
	if err := s.MemorySupersede(ctx, "", store.MemoryScopeAgent, scopeID, "k"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, scopeID, "k", json.RawMessage(`"new"`), 0); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	got, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, scopeID, "k")
	if err != nil {
		t.Fatalf("MemoryGet after re-set must find the revived row: %v", err)
	}
	if string(got.Value) != `"new"` {
		t.Errorf("revived value = %s, want \"new\"", got.Value)
	}

	// incr also revives: supersede a counter, then incr — the row must be visible
	// again and continue from the archived value.
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, scopeID, "c", json.RawMessage(`5`), 0)
	if err := s.MemorySupersede(ctx, "", store.MemoryScopeAgent, scopeID, "c"); err != nil {
		t.Fatalf("supersede counter: %v", err)
	}
	n, err := s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, scopeID, "c", 1, 0)
	if err != nil {
		t.Fatalf("incr after supersede: %v", err)
	}
	if n != 6 {
		t.Errorf("incr revived counter = %d, want 6 (continue from archived 5)", n)
	}
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, scopeID, "c"); err != nil {
		t.Errorf("incr did not revive the row (still hidden): %v", err)
	}
}

// testMemoryPendingEnqueueDrainAck exercises the durable consolidation queue:
// enqueue oldest-first, drain (capped, does NOT mark drained), ack (idempotent),
// and re-drain (acked rows excluded).
func testMemoryPendingEnqueueDrainAck(t *testing.T, s store.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	mk := func(id string, ageSecs int, body string) store.MemoryPendingRow {
		return store.MemoryPendingRow{
			ID:        id,
			Scope:     store.MemoryScopeAgent,
			ScopeID:   "pending-agent",
			Payload:   json.RawMessage(body),
			CreatedAt: base.Add(time.Duration(ageSecs) * time.Second),
		}
	}
	// Enqueue out of order; drain must return oldest-first by created_at.
	if err := s.MemoryPendingEnqueue(ctx, mk("mp_c", 2, `{"n":3}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryPendingEnqueue(ctx, mk("mp_a", 0, `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryPendingEnqueue(ctx, mk("mp_b", 1, `{"n":2}`)); err != nil {
		t.Fatal(err)
	}

	// Drain capped at 2 → the two oldest, in order.
	batch, err := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, "pending-agent", 2)
	if err != nil {
		t.Fatalf("MemoryPendingDrain: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("drain(limit=2) returned %d rows, want 2", len(batch))
	}
	if batch[0].ID != "mp_a" || batch[1].ID != "mp_b" {
		t.Errorf("drain order = [%s, %s], want [mp_a, mp_b] (oldest-first)", batch[0].ID, batch[1].ID)
	}
	if string(batch[0].Payload) == "" || !strings.Contains(string(batch[0].Payload), `"n"`) {
		t.Errorf("payload not round-tripped: %q", batch[0].Payload)
	}

	// Drain does NOT mark rows drained (at-least-once): re-draining returns them.
	again, err := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, "pending-agent", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("re-drain before ack returned %d, want 2 (drain must not mark drained)", len(again))
	}

	// An ack under a DIFFERENT scope/scope_id must NOT touch these rows — the ack
	// is confined to (tenant, scope, scope_id) symmetric with drain, so a leaked
	// id can't be acked cross-scope. mp_a must survive this wrong-scope ack.
	if err := s.MemoryPendingAck(ctx, "", store.MemoryScopeUser, "someone-else", []string{"mp_a", "mp_b"}); err != nil {
		t.Fatalf("wrong-scope ack should be a no-op, not an error: %v", err)
	}
	stillThere, err := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, "pending-agent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillThere) != 3 {
		t.Fatalf("wrong-scope ack drained rows it shouldn't: drain returned %d, want 3", len(stillThere))
	}

	// Ack the two oldest (correct scope); the third remains.
	if err := s.MemoryPendingAck(ctx, "", store.MemoryScopeAgent, "pending-agent", []string{"mp_a", "mp_b"}); err != nil {
		t.Fatalf("MemoryPendingAck: %v", err)
	}
	rest, err := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, "pending-agent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].ID != "mp_c" {
		t.Errorf("drain after ack = %+v, want only mp_c", rest)
	}

	// Ack idempotency: re-acking already-drained ids is a clean no-op and does
	// not resurrect or re-drain anything.
	if err := s.MemoryPendingAck(ctx, "", store.MemoryScopeAgent, "pending-agent", []string{"mp_a", "mp_b"}); err != nil {
		t.Fatalf("re-ack should be a no-op: %v", err)
	}
	if err := s.MemoryPendingAck(ctx, "", store.MemoryScopeAgent, "pending-agent", nil); err != nil {
		t.Fatalf("empty ack should be a no-op: %v", err)
	}
	rest2, _ := s.MemoryPendingDrain(ctx, "", store.MemoryScopeAgent, "pending-agent", 10)
	if len(rest2) != 1 || rest2[0].ID != "mp_c" {
		t.Errorf("drain after re-ack = %+v, want still only mp_c", rest2)
	}
}

// testMemoryCursorGetDefault: an unseen target returns a zero-watermark,
// unleased row — not an error.
func testMemoryCursorGetDefault(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, "never-consolidated")
	if err != nil {
		t.Fatalf("MemoryCursorGet(default): %v", err)
	}
	if !row.WatermarkCompletedAt.IsZero() || row.WatermarkSessionID != "" {
		t.Errorf("default watermark not zero: %+v", row)
	}
	if row.LeasedBy != "" || !row.LeaseExpiresAt.IsZero() {
		t.Errorf("default lease not empty: %+v", row)
	}
	if row.ScopeID != "never-consolidated" || row.Scope != store.MemoryScopeAgent {
		t.Errorf("default row target fields not populated: %+v", row)
	}
}

// testMemoryCursorLeaseCAS: the lease is a real compare-and-set — a second
// owner fails while the lease is held, and succeeds once it has expired.
func testMemoryCursorLeaseCAS(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scopeID = "lease-agent"
	now := time.Now().UTC()

	// A acquires an unleased target.
	row, ok, err := s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.LeasedBy != "owner-A" {
		t.Fatalf("A first lease: acquired=%v leasedBy=%q, want true/owner-A", ok, row.LeasedBy)
	}

	// B cannot acquire while A holds a live lease.
	row, ok, err = s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-B", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("B acquired a lease A still holds")
	}
	if row.LeasedBy != "owner-A" {
		t.Errorf("held lease owner = %q, want owner-A", row.LeasedBy)
	}

	// A re-acquires re-entrantly.
	if _, ok, err = s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", now, time.Hour); err != nil || !ok {
		t.Fatalf("A re-entrant lease: acquired=%v err=%v, want true/nil", ok, err)
	}

	// A force-expires its own lease (negative ttl → lease_expires_at in the past).
	if _, ok, err = s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", now, -time.Hour); err != nil || !ok {
		t.Fatalf("A expire-own lease: acquired=%v err=%v", ok, err)
	}

	// B now acquires the expired lease.
	row, ok, err = s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-B", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.LeasedBy != "owner-B" {
		t.Errorf("B acquire-after-expiry: acquired=%v leasedBy=%q, want true/owner-B", ok, row.LeasedBy)
	}

	// Release by A (not the holder) is a no-op; release by B clears it.
	if err := s.MemoryCursorRelease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A"); err != nil {
		t.Fatalf("release by non-holder should be a no-op: %v", err)
	}
	if r, _ := s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID); r.LeasedBy != "owner-B" {
		t.Errorf("non-holder release cleared the lease: %+v", r)
	}
	if err := s.MemoryCursorRelease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-B"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID); r.LeasedBy != "" {
		t.Errorf("lease not cleared after holder release: %+v", r)
	}
}

// testMemoryCursorAdvanceMonotonicAndOwner: advance requires the lease and
// never moves the composite watermark backward.
func testMemoryCursorAdvanceMonotonicAndOwner(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scopeID = "advance-agent"
	t1 := time.Now().UTC().Truncate(time.Second)
	t2 := t1.Add(time.Minute)

	// No lease yet → advance refused.
	if err := s.MemoryCursorAdvance(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", t2, "s2"); err == nil ||
		!strings.Contains(err.Error(), "not lease owner") {
		t.Errorf("advance without a lease err = %v, want 'not lease owner'", err)
	}

	// A leases the target.
	if _, ok, err := s.MemoryCursorLease(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", time.Now().UTC(), time.Hour); err != nil || !ok {
		t.Fatalf("A lease: acquired=%v err=%v", ok, err)
	}

	// B (not the lease owner) cannot advance.
	if err := s.MemoryCursorAdvance(ctx, "", store.MemoryScopeAgent, scopeID, "owner-B", t2, "s2"); err == nil ||
		!strings.Contains(err.Error(), "not lease owner") {
		t.Errorf("advance by non-owner err = %v, want 'not lease owner'", err)
	}

	// A advances to (t2, s2).
	if err := s.MemoryCursorAdvance(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", t2, "s2"); err != nil {
		t.Fatalf("A advance to t2: %v", err)
	}
	row, _ := s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID)
	if !row.WatermarkCompletedAt.Equal(t2) || row.WatermarkSessionID != "s2" {
		t.Fatalf("watermark = (%v, %q), want (t2, s2)", row.WatermarkCompletedAt, row.WatermarkSessionID)
	}

	// A backward advance (t1 < t2) is a monotonic no-op, not an error.
	if err := s.MemoryCursorAdvance(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", t1, "s1"); err != nil {
		t.Fatalf("backward advance should be a no-op, got err: %v", err)
	}
	row, _ = s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID)
	if !row.WatermarkCompletedAt.Equal(t2) || row.WatermarkSessionID != "s2" {
		t.Errorf("watermark moved backward: (%v, %q), want (t2, s2)", row.WatermarkCompletedAt, row.WatermarkSessionID)
	}

	// Same timestamp, higher session id → advances on the composite tie-break.
	if err := s.MemoryCursorAdvance(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", t2, "s3"); err != nil {
		t.Fatal(err)
	}
	row, _ = s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID)
	if row.WatermarkSessionID != "s3" {
		t.Errorf("composite advance session = %q, want s3", row.WatermarkSessionID)
	}

	// Same timestamp, lower session id → no-op.
	if err := s.MemoryCursorAdvance(ctx, "", store.MemoryScopeAgent, scopeID, "owner-A", t2, "s0"); err != nil {
		t.Fatal(err)
	}
	row, _ = s.MemoryCursorGet(ctx, "", store.MemoryScopeAgent, scopeID)
	if row.WatermarkSessionID != "s3" {
		t.Errorf("watermark session moved backward to %q, want s3", row.WatermarkSessionID)
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
		if err := s.MemorySet(ctx, "", store.MemoryScopeUser, "alice", k, json.RawMessage(v), 0); err != nil {
			t.Fatal(err)
		}
	}
	got, truncated, err := s.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "events/", 50)
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
	all, _, err := s.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "", 50)
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
		_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "key/"+intToKey(i), json.RawMessage(`1`), 0)
	}
	got, truncated, err := s.MemoryList(ctx, "", store.MemoryScopeAgent, "qa", "key/", 3)
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
	// Part 1 — expires_at is recorded when ttl > 0. Use a LONG ttl so the
	// confirming Get can never race the expiry: a short-ttl key read on a slow CI
	// runner could already be gone before the first Get (a MemorySet that itself
	// takes >ttl under load), which used to flake this test.
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "longlived", json.RawMessage(`"hi"`), time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "longlived")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("expires_at should be set when ttl > 0")
	}

	// Part 2 — a short-ttl key is gone after a sleep well past its ttl. There is
	// NO Get between the set and the sleep, so runner slowness only makes the key
	// MORE certainly expired (more wall-clock elapses); this direction can't flake.
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "warning", json.RawMessage(`"hi"`), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)

	_, err = s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "warning")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("expired key should return ErrNotFound, got %v", err)
	}

	// MemoryList must filter expired entries even before the sweeper runs.
	listed, _, err := s.MemoryList(ctx, "", store.MemoryScopeAgent, "qa", "warning", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("MemoryList returned expired row(s): %d", len(listed))
	}
}

func testMemorySweepReapsExpired(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "u", "transient", json.RawMessage(`1`), 30*time.Millisecond)
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "u", "permanent", json.RawMessage(`1`), 0)

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
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "u", "permanent"); err != nil {
		t.Errorf("permanent row was reaped: %v", err)
	}
}

func testMemoryIncrementOnNewKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, "qa", "warnings", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("incr on new key = %d, want 1", got)
	}
	got, err = s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, "qa", "warnings", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 6 {
		t.Errorf("incr by 5 on existing 1 = %d, want 6", got)
	}
}

func testMemoryIncrementOnExistingNumber(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "n", json.RawMessage(`42`), 0)
	got, err := s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, "qa", "n", -10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 32 {
		t.Errorf("incr by -10 on 42 = %d, want 32", got)
	}
}

func testMemoryIncrementOnNonNumberFails(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "obj", json.RawMessage(`{"hello":"world"}`), 0)
	_, err := s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, "qa", "obj", 1, 0)
	if !errors.Is(err, store.ErrMemoryWrongType) {
		t.Errorf("got %v, want ErrMemoryWrongType", err)
	}
}

func testMemoryIncrementOnExpiredKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "k", json.RawMessage(`100`), 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	got, err := s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, "qa", "k", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("incr on expired key should restart from 0; got %d, want 1", got)
	}
}

func testMemoryScopeIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "alice", "secret", json.RawMessage(`"alice-secret"`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "bob", "secret", json.RawMessage(`"bob-secret"`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "secret", json.RawMessage(`"qa-secret"`), 0)

	a, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "bob", "secret")
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Value) == string(b.Value) || string(a.Value) == string(q.Value) {
		t.Errorf("scope isolation broken: alice=%s bob=%s qa=%s", a.Value, b.Value, q.Value)
	}

	// Listing one (scope, scopeID) must not surface another's keys.
	list, _, _ := s.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "", 100)
	if len(list) != 1 {
		t.Errorf("alice-scope list returned %d rows, want 1", len(list))
	}
}

func testMemoryListScopeIDs(t *testing.T, s store.Store) {
	ctx := context.Background()
	// alice: 2 keys; bob: 1 key; qa-agent: 1 key.
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "alice", "voice", json.RawMessage(`"a1"`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "alice", "tone", json.RawMessage(`"a2"`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "bob", "voice", json.RawMessage(`"b1"`), 0)
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa-agent", "warnings", json.RawMessage(`5`), 0)

	users, err := s.MemoryListScopeIDs(ctx, "", store.MemoryScopeUser)
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

	agents, err := s.MemoryListScopeIDs(ctx, "", store.MemoryScopeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ScopeID != "qa-agent" {
		t.Errorf("agent-scope summary: %+v", agents)
	}

	// Expired rows must not surface in the summary.
	_ = s.MemorySet(ctx, "", store.MemoryScopeUser, "transient", "k", json.RawMessage(`1`), 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	users2, _ := s.MemoryListScopeIDs(ctx, "", store.MemoryScopeUser)
	for _, u := range users2 {
		if u.ScopeID == "transient" {
			t.Errorf("expired-only scope_id %q should not appear", u.ScopeID)
		}
	}
}

// testMemoryListTenantsForScope pins the RFC BL enumeration primitive (backing
// the retention sweeper's per-tenant base-memory fan-out): DISTINCT tenants that
// hold a row under (scope, scopeID), including the legacy "" partition, scoped
// to the given scope_id, and an empty slice (not an error) for an absent name.
func testMemoryListTenantsForScope(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "shared-agent"
	for _, tenant := range []string{"", "t-a", "t-b"} {
		if err := s.MemorySet(ctx, tenant, store.MemoryScopeAgent, name, "k", json.RawMessage(`1`), 0); err != nil {
			t.Fatalf("MemorySet(%q): %v", tenant, err)
		}
	}
	// A different scope_id must not leak into the result.
	_ = s.MemorySet(ctx, "t-c", store.MemoryScopeAgent, "other-agent", "k", json.RawMessage(`1`), 0)

	got, err := s.MemoryListTenantsForScope(ctx, store.MemoryScopeAgent, name)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, tn := range got {
		set[tn] = true
	}
	if len(got) != 3 || !set[""] || !set["t-a"] || !set["t-b"] {
		t.Errorf("tenants = %v, want exactly {\"\",t-a,t-b}", got)
	}

	empty, err := s.MemoryListTenantsForScope(ctx, store.MemoryScopeAgent, "never-written")
	if err != nil || len(empty) != 0 {
		t.Errorf("absent scope = %v err=%v, want [] nil", empty, err)
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
			_, _ = s.MemoryIncrement(ctx, "", store.MemoryScopeAgent, "qa", "counter", 1, 0)
		}()
	}
	wg.Wait()

	got, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "counter")
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
	got, err := s.MemoryAtomicUpdate(ctx, "", store.MemoryScopeAgent, "qa", "k1", 0,
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
	entry, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "k1")
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
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "k2",
		json.RawMessage(`{"x":1}`), 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryAtomicUpdate(ctx, "", store.MemoryScopeAgent, "qa", "k2", 0,
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
	entry, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "k2")
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
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "k3",
		json.RawMessage(`{"old":true}`), 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	_, err := s.MemoryAtomicUpdate(ctx, "", store.MemoryScopeAgent, "qa", "k3", 0,
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
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "k4",
		json.RawMessage(`{"untouched":true}`), 0); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("reducer-said-no")
	_, err := s.MemoryAtomicUpdate(ctx, "", store.MemoryScopeAgent, "qa", "k4", 0,
		func(existing json.RawMessage) (json.RawMessage, error) {
			return nil, sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel propagation", err)
	}
	entry, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "k4")
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
	_, err := s.MemoryAtomicUpdate(ctx, "", store.MemoryScopeAgent, "qa", "k5", 0,
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
			_, _ = s.MemoryAtomicUpdate(ctx, "", store.MemoryScopeAgent, "qa", "list", 0,
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

	entry, err := s.MemoryGet(ctx, "", store.MemoryScopeAgent, "qa", "list")
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
	err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "k", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedSet on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedGet(ctx, "", store.MemoryScopeAgent, "qa", "k")
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedGet on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedSearch(ctx, "", store.MemoryScopeAgent, "qa", "", floats32(1, 0, 0, 0), 5)
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedSearch on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedListByModel(ctx, "", store.MemoryScopeAgent, "qa", "openai", "text-embedding-3-large", 10)
	if !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedListByModel on backend without vectors: got %v, want ErrVectorUnsupported", err)
	}
	_, err = s.MemoryEmbedStats(ctx, "", store.MemoryScopeAgent)
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
	if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "rec1", json.RawMessage(`"hello"`), 0); err != nil {
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
	if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "rec1", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemoryEmbedGet(ctx, "", store.MemoryScopeAgent, "qa", "rec1")
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
		if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", r.key, json.RawMessage(`"x"`), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", r.key, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: r.vec, EmbedText: r.key, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeAgent, "qa", "", floats32(1, 0, 0, 0), 3)
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
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "rec", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "rec", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	// Search the same key from user scope — must return empty.
	results, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, "qa", "", floats32(1, 0, 0, 0), 10)
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
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "rec", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "rec", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	// Query with dimension 8 against stored dimension 4.
	_, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeAgent, "qa", "", floats32(1, 0, 0, 0, 1, 0, 0, 0), 5)
	if !errors.Is(err, store.ErrDimensionMismatch) {
		t.Errorf("dim mismatch: got %v, want ErrDimensionMismatch", err)
	}
}

func testMemoryEmbedSearchEmptyScope(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	results, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeAgent, "empty", "", floats32(1, 0, 0, 0), 10)
	if err != nil {
		t.Errorf("empty scope search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("empty scope: got %d results, want 0", len(results))
	}
}

// testMemoryEmbedSearchReturnsVectors pins the RFC I MR-5 / Decision 2
// store change: MemoryEmbedSearch must populate MemorySearchEntry.Vector
// with each row's stored embedding so the in-process backend's search-time
// dedup pass has per-entry vectors to compare. The vector is internal-only
// (json:"-") and never reaches the agent, but the store MUST hand it back.
//
// Gated like every other vector test on SupportsVectors(): runs against a
// live Postgres+pgvector store, skips (after asserting the refusal shape)
// on SQLite, which has no real vector search in any build.
func testMemoryEmbedSearchReturnsVectors(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	want := map[string][]float32{
		"east":  floats32(1, 0, 0, 0),
		"north": floats32(0, 1, 0, 0),
	}
	for key, vec := range want {
		if err := s.MemorySet(ctx, "", store.MemoryScopeAgent, "vecret", key, json.RawMessage(`"x"`), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "vecret", key, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: vec, EmbedText: key, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeAgent, "vecret", "", floats32(1, 0, 0, 0), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		w, ok := want[r.Key]
		if !ok {
			t.Fatalf("unexpected key %q", r.Key)
		}
		if !reflect.DeepEqual(r.Vector, w) {
			t.Errorf("key %q: Vector = %v, want %v (search must return the stored vector for dedup)", r.Key, r.Vector, w)
		}
	}
}

func testMemoryDeleteCascadesEmbedding(t *testing.T, s store.Store) {
	if !vectorRefusalCheck(t, s) {
		return
	}
	ctx := context.Background()
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "doomed", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "doomed", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
		Vector: floats32(1, 0, 0, 0), EmbedText: "x", CreatedAt: time.Now(),
	})
	// Delete the base row — embedding row must vanish via FK CASCADE.
	if _, err := s.MemoryDelete(ctx, "", store.MemoryScopeAgent, "qa", "doomed"); err != nil {
		t.Fatal(err)
	}
	_, err := s.MemoryEmbedGet(ctx, "", store.MemoryScopeAgent, "qa", "doomed")
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
		_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", k, json.RawMessage(`"x"`), 0)
		_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", k, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: floats32(1, 0, 0, 0), EmbedText: k, CreatedAt: time.Now(),
		})
	}
	// One row under the NEW model.
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "new1", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "new1", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-large", Dimension: 8,
		Vector: floats32(1, 0, 0, 0, 0, 0, 0, 0), EmbedText: "new1", CreatedAt: time.Now(),
	})
	// Ask "which rows are NOT on text-embedding-3-large?" — expect old1 + old2.
	got, err := s.MemoryEmbedListByModel(ctx, "", store.MemoryScopeAgent, "qa", "openai", "text-embedding-3-large", 100)
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
		_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", k, json.RawMessage(`"x"`), 0)
		_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", k, store.MemoryEmbedding{
			Provider: "openai", Model: "text-embedding-3-small", Dimension: 4,
			Vector: floats32(1, 0, 0, 0), EmbedText: k, CreatedAt: time.Now(),
		})
	}
	_ = s.MemorySet(ctx, "", store.MemoryScopeAgent, "qa", "b1", json.RawMessage(`"x"`), 0)
	_ = s.MemoryEmbedSet(ctx, "", store.MemoryScopeAgent, "qa", "b1", store.MemoryEmbedding{
		Provider: "openai", Model: "text-embedding-3-large", Dimension: 8,
		Vector: floats32(1, 0, 0, 0, 0, 0, 0, 0), EmbedText: "b1", CreatedAt: time.Now(),
	})
	stats, err := s.MemoryEmbedStats(ctx, "", store.MemoryScopeAgent)
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
	if !jsonEqual(msgs[0].Payload, `{"live":true}`) {
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
	if len(msgs) != 1 || !jsonEqual(msgs[0].Payload, `{"from":"a"}`) {
		t.Errorf("agent-a sees: %+v, want only its own message", msgs)
	}
	msgs, _, _ = s.ChannelSubscribe(ctx, "shared", store.MemoryScopeAgent, "agent-b", "", 10)
	if len(msgs) != 1 || !jsonEqual(msgs[0].Payload, `{"from":"b"}`) {
		t.Errorf("agent-b sees: %+v, want only its own message", msgs)
	}
}

// testChannelPurge pins the ChannelPurge contract: it drains every buffered
// message for a channel name and returns the count, is idempotent on an
// empty/unknown channel (0, nil — NOT ErrNotFound), and leaves the channel
// usable for new publishes (it drains the queue, it does not tear the
// channel down).
func testChannelPurge(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: "purge-ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
			Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
		}, 0); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	n, err := s.ChannelPurge(ctx, "purge-ch")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 3 {
		t.Errorf("purge returned %d, want 3", n)
	}
	if msgs, _, _ := s.ChannelSubscribe(ctx, "purge-ch", store.MemoryScopeAgent, "x", "", 10); len(msgs) != 0 {
		t.Errorf("post-purge msgs = %d, want 0 (queue drained)", len(msgs))
	}

	// Idempotent on an already-empty channel.
	n2, err := s.ChannelPurge(ctx, "purge-ch")
	if err != nil {
		t.Fatalf("purge empty: %v", err)
	}
	if n2 != 0 {
		t.Errorf("purge empty returned %d, want 0", n2)
	}
	// Idempotent on a never-seen channel (existence is the caller's concern).
	if n3, err := s.ChannelPurge(ctx, "never-existed"); err != nil || n3 != 0 {
		t.Errorf("purge unknown channel = (%d, %v), want (0, nil)", n3, err)
	}

	// The channel still accepts new messages — purge drained, didn't delete.
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "purge-ch", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`{"i":99}`),
	}, 0); err != nil {
		t.Fatalf("publish after purge: %v", err)
	}
	if msgs, _, _ := s.ChannelSubscribe(ctx, "purge-ch", store.MemoryScopeAgent, "x", "", 10); len(msgs) != 1 {
		t.Errorf("post-purge-republish msgs = %d, want 1", len(msgs))
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

// testChannelGetPointLookup pins exp7 I5's new ChannelGet: an existing
// runtime-declared channel resolves by name with its persisted fields; a
// missing name returns a typed *store.ErrNotFound (so the connector can
// distinguish "not declared" from a real store fault).
func testChannelGetPointLookup(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.ChannelsCreate(ctx, store.ChannelRow{
		Name: "cg-point", Description: "d", Scope: "global", Semantic: "queue",
		DefaultTTL: 90, MaxMessages: 11, Publisher: "system", Period: "1m",
	}); err != nil {
		t.Fatalf("ChannelsCreate: %v", err)
	}

	got, err := s.ChannelGet(ctx, "cg-point")
	if err != nil {
		t.Fatalf("ChannelGet(existing): %v", err)
	}
	if got.Name != "cg-point" || got.MaxMessages != 11 || got.DefaultTTL != 90 ||
		got.Scope != "global" || got.Semantic != "queue" || got.Publisher != "system" || got.Period != "1m" {
		t.Errorf("ChannelGet round-trip mismatch: %+v", got)
	}

	_, err = s.ChannelGet(ctx, "cg-absent")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("ChannelGet(missing): got %v (%T), want *store.ErrNotFound", err, err)
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
	if !jsonEqual(msgs[0].Payload, `"A"`) || string(msgs[1].Payload) != `"C"` {
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
	if !jsonEqual(msgs[0].Payload, `"B"`) {
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
	if !jsonEqual(got.Definition, `{"v":"original"}`) {
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
	if err := s.AgentDefSetActive(ctx, "", "promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "", "promo", b.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "", "promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentDefGetActive(ctx, "", "promo")
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

// testAgentDefRetireActiveClearsPointer pins the soft-reclaim contract:
// retiring the CURRENTLY-ACTIVE def clears its active pointer, so the name
// becomes reclaimable and runs stop resolving a retired def. FAIL-BEFORE:
// retire was a bare `UPDATE ... SET retired` that left the pointer, so
// AgentDefGetActive still returned the retired row.
func testAgentDefRetireActiveClearsPointer(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.AgentDefCreate(ctx, mkDef("rca-1", "rca-agent", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "", "rca-agent", row.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	// Active pointer cleared → reclaimable / no longer served.
	_, err = s.AgentDefGetActive(ctx, "", "rca-agent")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("GetActive after retiring the active def = %v, want *ErrNotFound (pointer should be cleared)", err)
	}
	// The row itself survives, flagged retired (audit lineage preserved).
	got, _ := s.AgentDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("retired flag didn't stick on the row")
	}
}

// testAgentDefRetireNonActiveLeavesPointer pins that retiring a NON-active
// version leaves the active pointer untouched (the `def_id` guard).
func testAgentDefRetireNonActiveLeavesPointer(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.AgentDefCreate(ctx, mkDef("rna-1", "rna-agent", ""))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.AgentDefCreate(ctx, mkDef("rna-2", "rna-agent", v1.DefID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "", "rna-agent", v2.DefID, ""); err != nil {
		t.Fatal(err)
	}
	// Retire the OLD, non-active v1 — the active pointer must still be v2.
	if err := s.AgentDefSetRetired(ctx, v1.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentDefGetActive(ctx, "", "rna-agent")
	if err != nil {
		t.Fatalf("GetActive after retiring a non-active version: %v", err)
	}
	if got.DefID != v2.DefID {
		t.Errorf("active = %s, want %s (retiring non-active must not touch the pointer)", got.DefID, v2.DefID)
	}
}

// testAgentDefListNamesLiveCount pins the additive list fields: total
// VersionCount includes retired rows, LiveVersionCount excludes them, and
// retiring the active def clears ActiveDefID for that name.
func testAgentDefListNamesLiveCount(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.AgentDefCreate(ctx, mkDef("lc-1", "lc-agent", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AgentDefCreate(ctx, mkDef("lc-2", "lc-agent", v1.DefID)); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetRetired(ctx, v1.DefID, true); err != nil {
		t.Fatal(err)
	}
	findName := func() store.AgentDefNameSummary {
		rows, lerr := s.AgentDefListNames(ctx)
		if lerr != nil {
			t.Fatal(lerr)
		}
		for _, r := range rows {
			if r.Name == "lc-agent" && r.TenantID == "" {
				return r
			}
		}
		t.Fatal("lc-agent not in list")
		return store.AgentDefNameSummary{}
	}
	sum := findName()
	if sum.VersionCount != 2 {
		t.Errorf("VersionCount = %d, want 2 (retired rows still counted)", sum.VersionCount)
	}
	if sum.LiveVersionCount != 1 {
		t.Errorf("LiveVersionCount = %d, want 1 (retired excluded)", sum.LiveVersionCount)
	}
}

// testSkillDefListNamesLiveCount mirrors the agent live-count test for skills:
// VersionCount counts every version, LiveVersionCount excludes retired, and
// ActiveRetired is true when the active pointer references a retired def. Drives
// the Web UI Library "Hide retired" filter for the skills tab.
func testSkillDefListNamesLiveCount(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.SkillDefCreate(ctx, mkSkillDef("slc-1", "slc-skill", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SkillDefCreate(ctx, mkSkillDef("slc-2", "slc-skill", v1.DefID)); err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetRetired(ctx, v1.DefID, true); err != nil {
		t.Fatal(err)
	}
	find := func() store.SkillDefNameSummary {
		rows, lerr := s.SkillDefListNames(ctx)
		if lerr != nil {
			t.Fatal(lerr)
		}
		for _, r := range rows {
			if r.Name == "slc-skill" && r.TenantID == "" {
				return r
			}
		}
		t.Fatal("slc-skill not in list")
		return store.SkillDefNameSummary{}
	}
	sum := find()
	if sum.VersionCount != 2 {
		t.Errorf("VersionCount = %d, want 2 (retired rows still counted)", sum.VersionCount)
	}
	if sum.LiveVersionCount != 1 {
		t.Errorf("LiveVersionCount = %d, want 1 (retired excluded)", sum.LiveVersionCount)
	}
	// Pointing active at the retired v1 surfaces ActiveRetired.
	if err := s.SkillDefSetActive(ctx, "", "slc-skill", v1.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if !find().ActiveRetired {
		t.Errorf("ActiveRetired = false, want true (active points at a retired def)")
	}
}

// testMCPServerDefListNamesLiveCount mirrors the skill/agent live-count test for
// the mcp-servers tab.
func testMCPServerDefListNamesLiveCount(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.MCPServerDefCreate(ctx, mkMCPServerDef("mlc-1", "mlc-mcp", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MCPServerDefCreate(ctx, mkMCPServerDef("mlc-2", "mlc-mcp", v1.DefID)); err != nil {
		t.Fatal(err)
	}
	if err := s.MCPServerDefSetRetired(ctx, v1.DefID, true); err != nil {
		t.Fatal(err)
	}
	find := func() store.MCPServerDefNameSummary {
		rows, lerr := s.MCPServerDefListNames(ctx)
		if lerr != nil {
			t.Fatal(lerr)
		}
		for _, r := range rows {
			if r.Name == "mlc-mcp" && r.TenantID == "" {
				return r
			}
		}
		t.Fatal("mlc-mcp not in list")
		return store.MCPServerDefNameSummary{}
	}
	sum := find()
	if sum.VersionCount != 2 {
		t.Errorf("VersionCount = %d, want 2 (retired rows still counted)", sum.VersionCount)
	}
	if sum.LiveVersionCount != 1 {
		t.Errorf("LiveVersionCount = %d, want 1 (retired excluded)", sum.LiveVersionCount)
	}
	if err := s.MCPServerDefSetActive(ctx, "", "mlc-mcp", v1.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if !find().ActiveRetired {
		t.Errorf("ActiveRetired = false, want true (active points at a retired def)")
	}
}

func testAgentDefStaticFallback(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.AgentDefGetActive(ctx, "", "no-such-name")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v, want *ErrNotFound", err)
	}
}

// testAgentDefTenantIsolation pins the RFC N boundary: the SAME agent
// name registered under two tenants must resolve to each tenant's OWN
// definition + active pointer + dynamic_agents row — no cross-tenant
// clobber, no cross-tenant read.
//
// FAIL-BEFORE: on the pre-migration schema (agent_def_active PK = (name),
// agent_defs UNIQUE = (name, version), dynamic_agents PK = (name)),
// tenant B's writes overwrite tenant A's (last-writer-wins on the single
// global pointer / row), so the GetActive(A) assertion reads back B's
// def_id and the test fails. The composite (tenant_id, name) PK is what
// makes the two rows coexist.
func testAgentDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "summarize"

	// agent_defs + agent_def_active isolation.
	aDef := mkDef("ti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"v":"A"}`)
	aRow, err := s.AgentDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	bDef := mkDef("ti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"v":"B"}`)
	bRow, err := s.AgentDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.AgentDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.AgentDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.AgentDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"v":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s, want A's own def",
			gotA.DefID, gotA.TenantID, gotA.Definition)
	}

	gotB, err := s.AgentDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"v":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s, want B's own def",
			gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	// A def can only be promoted within its own tenant — promoting A's
	// def under tenant-b must be refused.
	if err := s.AgentDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
	}

	// dynamic_agents isolation — same name, two tenants, distinct bodies.
	if err := s.DynamicAgentUpsert(ctx, store.DynamicAgent{
		TenantID:   "tenant-a",
		Name:       name,
		Definition: json.RawMessage(`{"dyn":"A"}`),
	}); err != nil {
		t.Fatalf("dyn upsert A: %v", err)
	}
	if err := s.DynamicAgentUpsert(ctx, store.DynamicAgent{
		TenantID:   "tenant-b",
		Name:       name,
		Definition: json.RawMessage(`{"dyn":"B"}`),
	}); err != nil {
		t.Fatalf("dyn upsert B: %v", err)
	}
	dynA, err := s.DynamicAgentGet(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("dyn get A: %v", err)
	}
	if !jsonEqual(dynA.Definition, `{"dyn":"A"}`) || dynA.TenantID != "tenant-a" {
		t.Errorf("dynamic_agents tenant-a clobbered: got tenant=%q def=%s", dynA.TenantID, dynA.Definition)
	}
	dynB, err := s.DynamicAgentGet(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("dyn get B: %v", err)
	}
	if !jsonEqual(dynB.Definition, `{"dyn":"B"}`) || dynB.TenantID != "tenant-b" {
		t.Errorf("dynamic_agents tenant-b clobbered: got tenant=%q def=%s", dynB.TenantID, dynB.Definition)
	}

	// dynamic_agents DELETE isolation (exp7 C1) — deleting tenant-a's row
	// must not touch tenant-b's same-named row. FAIL-BEFORE: when the delete
	// filtered on (name) alone, this wiped BOTH tenants' rows, so the
	// tenant-b GetActive below would 404.
	deleted, err := s.DynamicAgentDelete(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("dyn delete A: %v", err)
	}
	if !deleted {
		t.Error("dyn delete A: reported no row deleted, want true")
	}
	if _, err := s.DynamicAgentGet(ctx, "tenant-a", name); err == nil {
		t.Error("dyn get A after delete: row still present, want not-found")
	}
	survivor, err := s.DynamicAgentGet(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("dyn get B after deleting A: tenant-b's row was wiped by a cross-tenant delete: %v", err)
	}
	if !jsonEqual(survivor.Definition, `{"dyn":"B"}`) || survivor.TenantID != "tenant-b" {
		t.Errorf("dynamic_agents tenant-b altered by tenant-a delete: got tenant=%q def=%s", survivor.TenantID, survivor.Definition)
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
		Definition:  json.RawMessage(`{"system_prompt":"be helpful","tools":["Read"]}`),
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
		Definition:  json.RawMessage(`{"tools":["Read"]}`),
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

// testRetentionPurgeDefVersions exercises the RFC BM retention store surface on
// the agent def-type (all 9 families share one uniform schema, so agent is
// representative). Setup: 5 versions under one (tenant, name); retire the 4
// oldest, then promote a RETIRED version as active. Each exclusion is made
// load-bearing:
//   - the ACTIVE version (rt-3, retired + active) must be excluded even though
//     it is retired + old — proving the NOT EXISTS active guard, not just the
//     retired filter, is what protects it;
//   - the keepLastN=1 newest qualifying version (rt-4) must survive;
//   - the LIVE version (rt-5) must never appear.
//
// Then DeleteDefVersions must remove exactly the listed rows and nothing else.
func testRetentionPurgeDefVersions(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "retain-agent"
	ids := []string{"rt-1", "rt-2", "rt-3", "rt-4", "rt-5"}

	// 5 monotonic versions v1..v5 under one (tenant="", name).
	for _, id := range ids {
		if _, err := s.AgentDefCreate(ctx, mkDef(id, name, "")); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// Retire the 4 oldest (rt-1..rt-4); rt-5 stays live.
	for _, id := range ids[:4] {
		if err := s.AgentDefSetRetired(ctx, id, true); err != nil {
			t.Fatalf("retire %s: %v", id, err)
		}
	}
	// Promote a RETIRED version (rt-3) as active — AFTER retiring, so the
	// retire-of-active clear-pointer path doesn't fire. This yields the
	// "ActiveRetired" corrupt-legacy state the active guard must survive.
	if err := s.AgentDefSetActive(ctx, "", name, "rt-3", ""); err != nil {
		t.Fatalf("promote rt-3: %v", err)
	}

	future := time.Now().Add(time.Hour) // age cutoff in the future → all rows qualify by age
	got, err := s.ListPurgeableRetiredDefVersions(ctx, "agent", future, 1, 100)
	if err != nil {
		t.Fatalf("list purgeable: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, r := range got {
		gotIDs[r.DefID] = true
		if r.DefType != "agent" {
			t.Errorf("ref.DefType = %q, want agent", r.DefType)
		}
		if r.Name != name {
			t.Errorf("ref.Name = %q, want %q", r.Name, name)
		}
		if len(r.Definition) == 0 {
			t.Errorf("ref %s: empty Definition (export would lose the body)", r.DefID)
		}
	}
	// Expected purgeable: rt-1, rt-2 only.
	want := map[string]bool{"rt-1": true, "rt-2": true}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("purgeable ids = %v, want %v (rt-3 active, rt-4 kept-by-N, rt-5 live)", gotIDs, want)
	}

	// DeleteDefVersions removes exactly those two.
	deleted, err := s.DeleteDefVersions(ctx, "agent", []string{"rt-1", "rt-2"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	// rt-1, rt-2 gone; rt-3, rt-4, rt-5 survive.
	for _, id := range []string{"rt-1", "rt-2"} {
		if _, err := s.AgentDefGet(ctx, id); err == nil {
			t.Errorf("%s still present after delete", id)
		}
	}
	for _, id := range []string{"rt-3", "rt-4", "rt-5"} {
		if _, err := s.AgentDefGet(ctx, id); err != nil {
			t.Errorf("%s wrongly deleted: %v", id, err)
		}
	}
	// A second list finds nothing new purgeable (rt-4 kept, rest gone/excluded).
	again, err := s.ListPurgeableRetiredDefVersions(ctx, "agent", future, 1, 100)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("after delete, still purgeable: %+v, want none", again)
	}

	// Empty id list is a no-op.
	if n, err := s.DeleteDefVersions(ctx, "agent", nil); err != nil || n != 0 {
		t.Errorf("delete empty = (%d, %v), want (0, nil)", n, err)
	}
	// Unknown def-type is rejected on BOTH methods (allowlist guard) — never a
	// silent no-op that would let a caller string reach SQL.
	if _, err := s.ListPurgeableRetiredDefVersions(ctx, "not_a_def_type", future, 1, 100); err == nil {
		t.Error("list with unknown def-type: want error, got nil")
	}
	if _, err := s.DeleteDefVersions(ctx, "not_a_def_type", []string{"x"}); err == nil {
		t.Error("delete with unknown def-type: want error, got nil")
	}

	// Parent-of-survivor exclusion (RFC BM leaf-only rule): a retired+old version
	// that is still the parent_def_id of a SURVIVING version must NOT be purgeable
	// — purging it would orphan the child's lineage and FK-violate on postgres
	// (parent_def_id REFERENCES <defs>(def_id)). Use a fresh name to isolate.
	const pname = "retain-lineage"
	if _, err := s.AgentDefCreate(ctx, mkDef("pa-1", pname, "")); err != nil {
		t.Fatalf("create pa-1: %v", err)
	}
	if _, err := s.AgentDefCreate(ctx, mkDef("pa-2", pname, "pa-1")); err != nil { // child of pa-1
		t.Fatalf("create pa-2: %v", err)
	}
	// Retire the PARENT (pa-1) only; pa-2 (child) stays live + becomes active.
	if err := s.AgentDefSetRetired(ctx, "pa-1", true); err != nil {
		t.Fatalf("retire pa-1: %v", err)
	}
	if err := s.AgentDefSetActive(ctx, "", pname, "pa-2", ""); err != nil {
		t.Fatalf("promote pa-2: %v", err)
	}
	// keepLastN=0 so keep-N doesn't mask the parent-exclusion: pa-1 is retired,
	// old, non-active, beyond-N — purgeable ONLY the leaf rule protects it.
	pg, err := s.ListPurgeableRetiredDefVersions(ctx, "agent", future, 0, 100)
	if err != nil {
		t.Fatalf("list (lineage): %v", err)
	}
	for _, r := range pg {
		if r.DefID == "pa-1" {
			t.Errorf("pa-1 (retired parent of surviving pa-2) must NOT be purgeable — would orphan lineage / FK-violate")
		}
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
	if !jsonEqual(got.Definition, `{"body":"original"}`) {
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
	if err := s.SkillDefSetActive(ctx, "", "skill-promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "", "skill-promo", b.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "", "skill-promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.SkillDefGetActive(ctx, "", "skill-promo")
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
	_, err := s.SkillDefGetActive(ctx, "", "no-such-skill-name")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v, want *ErrNotFound", err)
	}
}

// testSkillDefTenantIsolation pins the RFC N boundary: the SAME skill
// name registered under two tenants must resolve to each tenant's OWN
// definition + active pointer — no cross-tenant clobber, no cross-tenant
// read. Skills have no dynamic tier (static skills.Set + the substrate
// only), so this covers skill_defs + skill_def_active + the cross-tenant
// promote refusal.
//
// FAIL-BEFORE: on the pre-migration schema (skill_def_active PK = (name),
// skill_defs UNIQUE = (name, version)), tenant B's writes overwrite
// tenant A's (last-writer-wins on the single global pointer), so the
// GetActive(A) assertion reads back B's def_id and the test fails. The
// composite (tenant_id, name) PK is what makes the two rows coexist.
func testSkillDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "summarize-skill"

	aDef := mkSkillDef("sti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"body":"A"}`)
	aRow, err := s.SkillDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	bDef := mkSkillDef("sti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"body":"B"}`)
	bRow, err := s.SkillDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.SkillDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.SkillDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.SkillDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"body":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s, want A's own def",
			gotA.DefID, gotA.TenantID, gotA.Definition)
	}

	gotB, err := s.SkillDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"body":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s, want B's own def",
			gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	// A def can only be promoted within its own tenant — promoting A's
	// def under tenant-b must be refused.
	if err := s.SkillDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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

// ---- TeamDef contract tests ----
//
// Direct mirror of the SkillDef tests above. Same invariants:
// monotonic versioning under contention, idempotent active pointer,
// reversible retire flag, per-tenant isolation, content-hash
// round-trip. The Definition payload is an opaque workflow-graph blob.

func mkTeamDef(id, name string, parent string) store.TeamDefRow {
	return store.TeamDefRow{
		DefID:       id,
		Name:        name,
		ParentDefID: parent,
		Definition:  json.RawMessage(`{"graph":{"nodes":["a"]},"description":"test row"}`),
		Description: "test row",
	}
}

// testTeamDefListNamesLiveCount mirrors the skill live-count test for teams:
// VersionCount counts every version, LiveVersionCount excludes retired, and
// ActiveRetired is true when the active pointer references a retired def. Drives
// the Web UI Library "Hide retired" filter for the teams tab.
func testTeamDefListNamesLiveCount(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.TeamDefCreate(ctx, mkTeamDef("tlc-1", "tlc-team", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamDefCreate(ctx, mkTeamDef("tlc-2", "tlc-team", v1.DefID)); err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetRetired(ctx, v1.DefID, true); err != nil {
		t.Fatal(err)
	}
	find := func() store.TeamDefNameSummary {
		rows, lerr := s.TeamDefListNames(ctx)
		if lerr != nil {
			t.Fatal(lerr)
		}
		for _, r := range rows {
			if r.Name == "tlc-team" && r.TenantID == "" {
				return r
			}
		}
		t.Fatal("tlc-team not in list")
		return store.TeamDefNameSummary{}
	}
	sum := find()
	if sum.VersionCount != 2 {
		t.Errorf("VersionCount = %d, want 2 (retired rows still counted)", sum.VersionCount)
	}
	if sum.LiveVersionCount != 1 {
		t.Errorf("LiveVersionCount = %d, want 1 (retired excluded)", sum.LiveVersionCount)
	}
	// Pointing active at the retired v1 surfaces ActiveRetired.
	if err := s.TeamDefSetActive(ctx, "", "tlc-team", v1.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if !find().ActiveRetired {
		t.Errorf("ActiveRetired = false, want true (active points at a retired def)")
	}
}

func testTeamDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.TeamDefCreate(ctx, mkTeamDef("td-1", "team-alpha", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("first version = %d, want 1", row.Version)
	}
	got, err := s.TeamDefGet(ctx, "td-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "team-alpha" || got.Version != 1 {
		t.Errorf("got %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not populated")
	}
}

func testTeamDefVersionMonotonicUnderContention(t *testing.T, s store.Store) {
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
				id := fmt.Sprintf("td-%d-%d", g, i)
				_, err := s.TeamDefCreate(ctx, mkTeamDef(id, "team-race", ""))
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
	rows, err := s.TeamDefListByName(ctx, "team-race")
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

func testTeamDefActivePointerIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, err := s.TeamDefCreate(ctx, mkTeamDef("td-a", "team-promo", ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.TeamDefCreate(ctx, mkTeamDef("td-b", "team-promo", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetActive(ctx, "", "team-promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetActive(ctx, "", "team-promo", b.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetActive(ctx, "", "team-promo", a.DefID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.TeamDefGetActive(ctx, "", "team-promo")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefID != a.DefID {
		t.Errorf("active = %s, want %s", got.DefID, a.DefID)
	}
}

func testTeamDefRetireReversible(t *testing.T, s store.Store) {
	ctx := context.Background()
	row, err := s.TeamDefCreate(ctx, mkTeamDef("td-r-1", "team-retireagent", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.TeamDefGet(ctx, row.DefID)
	if !got.Retired {
		t.Error("retire(true) didn't stick")
	}
	if err := s.TeamDefSetRetired(ctx, row.DefID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.TeamDefGet(ctx, row.DefID)
	if got.Retired {
		t.Error("retire(false) didn't reverse")
	}
	rows, _ := s.TeamDefListByName(ctx, "team-retireagent")
	if len(rows) != 1 {
		t.Errorf("list after retire toggle: got %d, want 1", len(rows))
	}
}

// testTeamDefDelete: delete removes ALL versions of a name + its active pointer,
// scoped to the tenant, leaving other teams untouched; re-delete is a no-op.
func testTeamDefDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.TeamDefCreate(ctx, mkTeamDef("tdel-1", "team-del", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamDefCreate(ctx, mkTeamDef("tdel-2", "team-del", v1.DefID)); err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetActive(ctx, "", "team-del", v1.DefID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamDefCreate(ctx, mkTeamDef("tkeep-1", "team-keep", "")); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.TeamDefDelete(ctx, "", "team-del")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("delete reported nothing removed")
	}
	if rows, _ := s.TeamDefListByName(ctx, "team-del"); len(rows) != 0 {
		t.Errorf("after delete: %d versions remain, want 0", len(rows))
	}
	if _, err := s.TeamDefGetActive(ctx, "", "team-del"); err == nil {
		t.Error("after delete: active pointer still resolves")
	}
	if rows, _ := s.TeamDefListByName(ctx, "team-keep"); len(rows) != 1 {
		t.Errorf("bystander team-keep: %d versions, want 1", len(rows))
	}
	// Re-deleting a now-missing team → (false, nil).
	if d, err := s.TeamDefDelete(ctx, "", "team-del"); err != nil || d {
		t.Errorf("re-delete: got (%v,%v), want (false,nil)", d, err)
	}
}

func testTeamDefStaticFallback(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.TeamDefGetActive(ctx, "", "no-such-team-name")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v, want *ErrNotFound", err)
	}
}

// testTeamDefTenantIsolation pins the RFC N boundary: the SAME team
// name registered under two tenants must resolve to each tenant's OWN
// definition + active pointer — no cross-tenant clobber, no cross-tenant
// read. Covers teamdefs + teamdef_active + the cross-tenant promote refusal.
//
// FAIL-BEFORE: on a single-`name`-PK schema (teamdef_active PK = (name),
// teamdefs UNIQUE = (name, version)), tenant B's writes overwrite tenant
// A's (last-writer-wins on the single global pointer), so the GetActive(A)
// assertion reads back B's def_id and the test fails. The composite
// (tenant_id, name) PK is what makes the two rows coexist.
func testTeamDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "summarize-team"

	aDef := mkTeamDef("tti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"body":"A"}`)
	aRow, err := s.TeamDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	bDef := mkTeamDef("tti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"body":"B"}`)
	bRow, err := s.TeamDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.TeamDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.TeamDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.TeamDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"body":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s, want A's own def",
			gotA.DefID, gotA.TenantID, gotA.Definition)
	}

	gotB, err := s.TeamDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"body":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s, want B's own def",
			gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	// A def can only be promoted within its own tenant — promoting A's
	// def under tenant-b must be refused.
	if err := s.TeamDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
	}
}

func testTeamDefContentSHA256RoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := mkTeamDef("td-hash", "team-alpha-hash", "")
	row.ContentSHA256 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	written, err := s.TeamDefCreate(ctx, row)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("write echo: got %q, want %q", written.ContentSHA256, row.ContentSHA256)
	}
	got, err := s.TeamDefGet(ctx, "td-hash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ContentSHA256 != row.ContentSHA256 {
		t.Errorf("get: ContentSHA256 = %q, want %q", got.ContentSHA256, row.ContentSHA256)
	}

	plain, err := s.TeamDefCreate(ctx, mkTeamDef("td-no-hash", "team-alpha-no-hash", ""))
	if err != nil {
		t.Fatalf("create no-hash: %v", err)
	}
	if plain.ContentSHA256 != "" {
		t.Errorf("hashless row: got %q, want empty", plain.ContentSHA256)
	}

	// The snapshot read must preserve content_sha256 — it feeds capture→restore,
	// and dropping it here silently nulls the hash on every restore, breaking
	// verify-or-fork for every restored team (no boot backfill recovers it).
	snap, err := s.SnapshotReadTeamDefs(ctx)
	if err != nil {
		t.Fatalf("snapshot read: %v", err)
	}
	var found bool
	for _, r := range snap {
		if r.DefID == "td-hash" {
			found = true
			if r.ContentSHA256 != row.ContentSHA256 {
				t.Errorf("snapshot read dropped ContentSHA256: got %q, want %q", r.ContentSHA256, row.ContentSHA256)
			}
		}
	}
	if !found {
		t.Fatalf("snapshot read did not return td-hash")
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

	if err := s.MCPServerDefSetActive(ctx, "", "mcp-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MCPServerDefGetActive(ctx, "", "mcp-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.MCPServerDefSetActive(ctx, "", "mcp-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MCPServerDefGetActive(ctx, "", "mcp-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

// testMCPServerDefTenantIsolation pins the RFC N boundary: the SAME MCP
// server name registered under two tenants must resolve to each tenant's
// OWN definition + active pointer — no cross-tenant clobber, no
// cross-tenant read, no cross-tenant promote.
//
// FAIL-BEFORE: on the pre-migration schema (mcp_server_def_active PK =
// (name), mcp_server_defs UNIQUE = (name, version)), tenant B's promote
// overwrites tenant A's single global pointer (last-writer-wins), so the
// GetActive(A) assertion reads back B's def_id and the test fails. The
// composite (tenant_id, name) PK is what makes the two rows coexist.
func testMCPServerDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "n8n"

	aDef := mkMCPServerDef("mti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"transport":"http","url":"https://a.example.com/mcp"}`)
	aRow, err := s.MCPServerDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	bDef := mkMCPServerDef("mti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"transport":"http","url":"https://b.example.com/mcp"}`)
	bRow, err := s.MCPServerDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.MCPServerDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.MCPServerDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.MCPServerDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"transport":"http","url":"https://a.example.com/mcp"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s, want A's own def",
			gotA.DefID, gotA.TenantID, gotA.Definition)
	}

	gotB, err := s.MCPServerDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"transport":"http","url":"https://b.example.com/mcp"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s, want B's own def",
			gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	// A def can only be promoted within its own tenant — promoting A's
	// def under tenant-b must be refused.
	if err := s.MCPServerDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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

	if err := s.ScheduleDefSetActive(ctx, "", "sched-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ScheduleDefGetActive(ctx, "", "sched-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.ScheduleDefSetActive(ctx, "", "sched-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ScheduleDefGetActive(ctx, "", "sched-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

// testScheduleDefTenantIsolation mirrors testMemoryBackendDefTenantIsolation
// for the Schedule plane.
func testScheduleDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "shared-sched"

	aDef := mkScheduleDef("sdti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"agent":"x","schedule":"0 0 * * *","v":"A"}`)
	aRow, err := s.ScheduleDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bDef := mkScheduleDef("sdti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"agent":"x","schedule":"0 0 * * *","v":"B"}`)
	bRow, err := s.ScheduleDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.ScheduleDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.ScheduleDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.ScheduleDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"agent":"x","schedule":"0 0 * * *","v":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s", gotA.DefID, gotA.TenantID, gotA.Definition)
	}
	gotB, err := s.ScheduleDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"agent":"x","schedule":"0 0 * * *","v":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s", gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	if err := s.ScheduleDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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

	if err := s.A2AServerCardDefSetActive(ctx, "", "card-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.A2AServerCardDefGetActive(ctx, "", "card-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.A2AServerCardDefSetActive(ctx, "", "card-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.A2AServerCardDefGetActive(ctx, "", "card-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

// testA2AServerCardDefTenantIsolation mirrors the other isolation tests for
// the A2A server-card plane.
func testA2AServerCardDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "shared-card"

	aDef := mkA2AServerCardDef("ascti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"v":"A"}`)
	aRow, err := s.A2AServerCardDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bDef := mkA2AServerCardDef("ascti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"v":"B"}`)
	bRow, err := s.A2AServerCardDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.A2AServerCardDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.A2AServerCardDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.A2AServerCardDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"v":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s", gotA.DefID, gotA.TenantID, gotA.Definition)
	}
	gotB, err := s.A2AServerCardDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"v":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s", gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	if err := s.A2AServerCardDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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

	if err := s.A2AAgentDefSetActive(ctx, "", "peer-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.A2AAgentDefGetActive(ctx, "", "peer-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.A2AAgentDefSetActive(ctx, "", "peer-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.A2AAgentDefGetActive(ctx, "", "peer-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

// testA2AAgentDefTenantIsolation mirrors testMemoryBackendDefTenantIsolation
// for the A2A remote-peer plane.
func testA2AAgentDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "shared-peer"

	aDef := mkA2AAgentDef("aadti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"agent_card_url":"https://a.example","v":"A"}`)
	aRow, err := s.A2AAgentDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bDef := mkA2AAgentDef("aadti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"agent_card_url":"https://b.example","v":"B"}`)
	bRow, err := s.A2AAgentDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.A2AAgentDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.A2AAgentDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.A2AAgentDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"agent_card_url":"https://a.example","v":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s", gotA.DefID, gotA.TenantID, gotA.Definition)
	}
	gotB, err := s.A2AAgentDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"agent_card_url":"https://b.example","v":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s", gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	if err := s.A2AAgentDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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

	if err := s.WebhookDefSetActive(ctx, "", "hook-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.WebhookDefGetActive(ctx, "", "hook-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.WebhookDefSetActive(ctx, "", "hook-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.WebhookDefGetActive(ctx, "", "hook-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

// testWebhookDefTenantIsolation mirrors the other isolation tests for the
// Webhook plane.
func testWebhookDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "shared-hook"

	aDef := mkWebhookDef("whti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"delivery":"spawn","agent":"x","v":"A"}`)
	aRow, err := s.WebhookDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bDef := mkWebhookDef("whti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"delivery":"spawn","agent":"x","v":"B"}`)
	bRow, err := s.WebhookDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.WebhookDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.WebhookDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.WebhookDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"delivery":"spawn","agent":"x","v":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s", gotA.DefID, gotA.TenantID, gotA.Definition)
	}
	gotB, err := s.WebhookDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"delivery":"spawn","agent":"x","v":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s", gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	if err := s.WebhookDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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

	if err := s.MemoryBackendDefSetActive(ctx, "", "backend-active", r1.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MemoryBackendDefGetActive(ctx, "", "backend-active")
	if got.DefID != r1.DefID {
		t.Errorf("active = %s, want %s", got.DefID, r1.DefID)
	}
	if err := s.MemoryBackendDefSetActive(ctx, "", "backend-active", r2.DefID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MemoryBackendDefGetActive(ctx, "", "backend-active")
	if got.DefID != r2.DefID {
		t.Errorf("after re-promote: active = %s, want %s", got.DefID, r2.DefID)
	}
}

// testMemoryBackendDefTenantIsolation mirrors testAgentDefTenantIsolation
// (minus the dynamic_* tier MemoryBackend doesn't have): two tenants own
// the same name with distinct bodies, each GetActive returns its own, and
// cross-tenant promote is refused. Fails before the 0040 migration /
// tenant-scoped store methods (a single global active pointer would have
// tenant-b's promote clobber tenant-a's).
func testMemoryBackendDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	const name = "shared-backend"

	aDef := mkMemoryBackendDef("mbti-a", name, "")
	aDef.TenantID = "tenant-a"
	aDef.Definition = json.RawMessage(`{"kind":"inprocess","v":"A"}`)
	aRow, err := s.MemoryBackendDefCreate(ctx, aDef)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bDef := mkMemoryBackendDef("mbti-b", name, "")
	bDef.TenantID = "tenant-b"
	bDef.Definition = json.RawMessage(`{"kind":"inprocess","v":"B"}`)
	bRow, err := s.MemoryBackendDefCreate(ctx, bDef)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := s.MemoryBackendDefSetActive(ctx, "tenant-a", name, aRow.DefID, ""); err != nil {
		t.Fatalf("promote A: %v", err)
	}
	if err := s.MemoryBackendDefSetActive(ctx, "tenant-b", name, bRow.DefID, ""); err != nil {
		t.Fatalf("promote B: %v", err)
	}

	gotA, err := s.MemoryBackendDefGetActive(ctx, "tenant-a", name)
	if err != nil {
		t.Fatalf("get active A: %v", err)
	}
	if gotA.DefID != aRow.DefID || gotA.TenantID != "tenant-a" || !jsonEqual(gotA.Definition, `{"kind":"inprocess","v":"A"}`) {
		t.Errorf("tenant-a clobbered: got def_id=%q tenant=%q def=%s", gotA.DefID, gotA.TenantID, gotA.Definition)
	}
	gotB, err := s.MemoryBackendDefGetActive(ctx, "tenant-b", name)
	if err != nil {
		t.Fatalf("get active B: %v", err)
	}
	if gotB.DefID != bRow.DefID || gotB.TenantID != "tenant-b" || !jsonEqual(gotB.Definition, `{"kind":"inprocess","v":"B"}`) {
		t.Errorf("tenant-b clobbered: got def_id=%q tenant=%q def=%s", gotB.DefID, gotB.TenantID, gotB.Definition)
	}

	// A def can only be promoted within its own tenant.
	if err := s.MemoryBackendDefSetActive(ctx, "tenant-b", name, aRow.DefID, ""); err == nil {
		t.Error("cross-tenant promote (A's def under tenant-b) unexpectedly succeeded")
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
	if err := s.ScheduleDefSetActive(ctx, "", name, defID, "test"); err != nil {
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
	// RFC S / F36: the first record had CountAsFire unset → fire_count
	// stays 0 (a non-fire advance must not consume a max_fires budget).
	if got.FireCount != 0 {
		t.Errorf("fire_count = %d after CountAsFire=false record, want 0", got.FireCount)
	}
	// CountAsFire=true increments by exactly one per call.
	for want := 1; want <= 2; want++ {
		if err := s.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
			DefID:       defID,
			LastRunID:   "r_fire",
			LastStatus:  "completed",
			LastRunAt:   start,
			NextRunAt:   next,
			CountAsFire: true,
		}); err != nil {
			t.Fatalf("record fire %d: %v", want, err)
		}
		got, _ = s.ScheduleRunStateGet(ctx, defID)
		if got.FireCount != want {
			t.Errorf("fire_count = %d after %d fires, want %d", got.FireCount, want, want)
		}
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

// makeRunForInterruptTenant is makeRunForInterrupt with an explicit tenant
// stamped onto the run row (CreateRun denormalises identity.TenantID — the
// column InterruptListByUser's tenant filter reads). Returns the run id.
func makeRunForInterruptTenant(t *testing.T, s store.Store, tenant, userID, agentID, agentName string) (runID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, tenant, agentName, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID:  agentID,
		UserID:   userID,
		TenantID: tenant,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
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

// testInterruptFinishSetsDeclined pins RFC BH P2: InterruptFinish accepts the
// new "declined" terminal status (it was previously whitelist-rejected as an
// invalid terminal status) and records it answer-less with the resolver source.
func testInterruptFinishSetsDeclined(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, runID := makeRunForInterrupt(t, s, "u_alice", "a_intr_dec", "batch")

	id := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "u_alice",
		Question: "Q?", CreatedAt: time.Now(),
	})
	if err := s.InterruptFinish(ctx, id, store.InterruptStatusDeclined, store.InterruptResolvedByWebUI); err != nil {
		t.Fatalf("InterruptFinish(declined): %v", err)
	}
	r, _ := s.InterruptGet(ctx, id)
	if r.Status != store.InterruptStatusDeclined {
		t.Errorf("Status = %q, want declined", r.Status)
	}
	if r.Answer != "" {
		t.Errorf("Answer = %q on decline path; want empty", r.Answer)
	}
	if r.ResolvedBy != store.InterruptResolvedByWebUI {
		t.Errorf("ResolvedBy = %q, want webui", r.ResolvedBy)
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

	// tenantID "" = all tenants (this test exercises the user filter, not the
	// tenant filter — see testInterruptListByUserFiltersByTenant for that).
	rows, err := s.InterruptListByUser(ctx, "u_bob", "", store.InterruptStatusPending)
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

// testInterruptListByUserFiltersByTenant locks RFC L/N whole-tenant isolation
// on the user-scoped interrupt inbox: when a tenant is passed, interrupts on
// another tenant's runs are excluded even for the SAME user_id (user_ids are
// not secret). "" = all tenants. Fails on the pre-tenant signature where the
// listing keyed on user_id alone and leaked across tenants.
func testInterruptListByUserFiltersByTenant(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Same user_id "u_shared" in two tenants — the cross-tenant leak surface.
	runAcme := makeRunForInterruptTenant(t, s, "acme", "u_shared", "a_acme_intr", "agent-acme")
	runEvil := makeRunForInterruptTenant(t, s, "evil", "u_shared", "a_evil_intr", "agent-evil")

	idA := store.MintInterruptID(time.Now())
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: idA, RunID: runAcme, UserID: "u_shared", Question: "acme Q", CreatedAt: time.Now()})
	idE := store.MintInterruptID(time.Now().Add(time.Millisecond))
	_, _ = s.InterruptCreate(ctx, store.InterruptRow{InterruptID: idE, RunID: runEvil, UserID: "u_shared", Question: "evil Q", CreatedAt: time.Now().Add(time.Millisecond)})

	// Scoped to acme: only the acme interrupt, never evil's.
	acme, err := s.InterruptListByUser(ctx, "u_shared", "acme", "")
	if err != nil {
		t.Fatalf("ListByUser(acme): %v", err)
	}
	if len(acme) != 1 || acme[0].InterruptID != idA {
		t.Fatalf("acme-scoped listing = %d rows (want 1, id=%s) — tenant filter leaked", len(acme), idA)
	}

	// Scoped to evil: only evil's.
	evil, err := s.InterruptListByUser(ctx, "u_shared", "evil", "")
	if err != nil {
		t.Fatalf("ListByUser(evil): %v", err)
	}
	if len(evil) != 1 || evil[0].InterruptID != idE {
		t.Fatalf("evil-scoped listing = %d rows (want 1, id=%s)", len(evil), idE)
	}

	// "" = all tenants (super-admin) → both.
	all, err := s.InterruptListByUser(ctx, "u_shared", "", "")
	if err != nil {
		t.Fatalf("ListByUser(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all-tenants listing = %d rows, want 2", len(all))
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

// testListEventsFilterByTenant pins the RFC AS tenant-scoped audit: EventFilter
// .TenantID restricts the result to events whose owning session belongs to that
// tenant (events carry no tenant column — the filter JOINs sessions). Empty
// TenantID returns every tenant's events.
func testListEventsFilterByTenant(t *testing.T, s store.Store) {
	ctx := context.Background()
	acme, _ := s.CreateSession(ctx, "acme", "default", "alice")
	acmeRun, _ := s.CreateRun(ctx, acme.ID, store.RunIdentity{AgentID: "a_acme"})
	globex, _ := s.CreateSession(ctx, "globex", "default", "bob")
	globexRun, _ := s.CreateRun(ctx, globex.ID, store.RunIdentity{AgentID: "a_globex"})
	for i := 0; i < 3; i++ {
		if err := s.AppendEvent(ctx, acmeRun.ID, "text", []byte(`{"t":"acme"}`)); err != nil {
			t.Fatalf("AppendEvent acme: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.AppendEvent(ctx, globexRun.ID, "text", []byte(`{"t":"globex"}`)); err != nil {
			t.Fatalf("AppendEvent globex: %v", err)
		}
	}

	// Tenant filter → only that tenant's events (via the owning session).
	got, total, err := s.ListEvents(ctx, store.EventFilter{TenantID: "acme"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(got) != 3 {
		t.Fatalf("tenant=acme total=%d len=%d, want 3/3", total, len(got))
	}
	for _, ev := range got {
		if ev.SessionID != acme.ID {
			t.Errorf("tenant=acme leaked event from session %q (want %q)", ev.SessionID, acme.ID)
		}
	}
	// No tenant filter → both tenants (5 total).
	if _, totalAll, err := s.ListEvents(ctx, store.EventFilter{}, 50, 0); err != nil {
		t.Fatal(err)
	} else if totalAll != 5 {
		t.Errorf("no tenant filter total = %d, want 5 (both tenants)", totalAll)
	}
	// Tenant + type compose (still tenant-scoped).
	if _, totalCombo, err := s.ListEvents(ctx, store.EventFilter{TenantID: "globex", Type: "text"}, 50, 0); err != nil {
		t.Fatal(err)
	} else if totalCombo != 2 {
		t.Errorf("tenant=globex type=text total = %d, want 2", totalCombo)
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

// ---- RFC L OperatorTokenDef contract tests ----

func mkOperatorTokenDef(defID, name, tenant, subject, hash string, scopes []string) store.OperatorTokenDefRow {
	return store.OperatorTokenDefRow{
		DefID:         defID,
		Name:          name,
		TenantID:      tenant,
		Subject:       subject,
		TokenHash:     hash,
		AllowedScopes: scopes,
	}
}

func testOperatorTokenDefCreateAndLookup(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("ot-1", "alice", "acme", "alice", "hash-aaa", []string{"runs:create", "runs:read"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Hot-path lookup by hash.
	got, err := s.OperatorTokenDefGetByTokenHash(ctx, "hash-aaa")
	if err != nil {
		t.Fatalf("get-by-hash: %v", err)
	}
	if got.DefID != "ot-1" || got.TenantID != "acme" || got.Subject != "alice" {
		t.Errorf("got %+v", got)
	}
	if len(got.AllowedScopes) != 2 || got.AllowedScopes[0] != "runs:create" {
		t.Errorf("scopes round-trip wrong: %v", got.AllowedScopes)
	}
	// Lookup by def_id.
	if _, err := s.OperatorTokenDefGet(ctx, "ot-1"); err != nil {
		t.Errorf("get by def_id: %v", err)
	}
	// Miss → ErrNotFound.
	_, err = s.OperatorTokenDefGetByTokenHash(ctx, "no-such-hash")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("missing hash should be ErrNotFound, got %v", err)
	}
}

func testOperatorTokenDefCurrentByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("ot-cur-1", "svc", "acme", "svc", "hash-cur-1", []string{"runs:create"})); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	cur, err := s.OperatorTokenDefGetCurrentByName(ctx, "svc")
	if err != nil || cur.DefID != "ot-cur-1" {
		t.Fatalf("current should be ot-cur-1 (err=%v)", err)
	}
	// Retire it (immediate, past) → no current.
	if err := s.OperatorTokenDefSetRetiredAt(ctx, "ot-cur-1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	_, err = s.OperatorTokenDefGetCurrentByName(ctx, "svc")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("after retire, current-by-name should be ErrNotFound, got %v", err)
	}
}

func testOperatorTokenDefRetireAndCountAdmin(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, _ = s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("ot-adm", "root", "acme", "ops", "hash-adm", []string{"substrate:admin"}))
	_, _ = s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("ot-narrow", "app", "acme", "app", "hash-narrow", []string{"runs:create"}))
	n, err := s.OperatorTokenDefCountActiveAdmin(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("active admin count = %d, want 1 (only the substrate:admin token)", n)
	}
	// A FUTURE retired_at (rotation grace) must still count as active.
	if err := s.OperatorTokenDefSetRetiredAt(ctx, "ot-adm", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("grace retire: %v", err)
	}
	if n, _ := s.OperatorTokenDefCountActiveAdmin(ctx); n != 1 {
		t.Errorf("admin token in grace window must still count active; got %d", n)
	}
	// A PAST retired_at must drop it.
	if err := s.OperatorTokenDefSetRetiredAt(ctx, "ot-adm", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("past retire: %v", err)
	}
	if n, _ := s.OperatorTokenDefCountActiveAdmin(ctx); n != 0 {
		t.Errorf("retired admin token must not count; got %d", n)
	}
}

func testOperatorTokenDefListNames(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, _ = s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("ot-n1", "alice", "acme", "alice", "h-n1", []string{"runs:read"}))
	_, _ = s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("ot-n2", "bob", "acme", "bob", "h-n2", []string{"runs:read"}))
	names, err := s.OperatorTokenDefListNames(ctx)
	if err != nil {
		t.Fatalf("list names: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d names, want 2", len(names))
	}
	for _, n := range names {
		if n.TenantID != "acme" || n.Subject == "" || !n.HasCurrent || n.TokenCount != 1 {
			t.Errorf("summary wrong: %+v", n)
		}
	}
}

// ---- RFC AH Phase 2a VolumeDef contract tests ----

func mkVolumeDef(tenantID, name, path, mode string) store.VolumeDefRow {
	return store.VolumeDefRow{
		TenantID:   tenantID,
		Name:       name,
		Definition: json.RawMessage(fmt.Sprintf(`{"path":%q,"mode":%q}`, path, mode)),
	}
}

func testVolumeDefCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.VolumeDefCreate(ctx, mkVolumeDef("", "repo-a", "/pool/_shared/repo-a", "rw"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.VolumeDefGetByName(ctx, "", "repo-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "repo-a" || !jsonEqual(got.Definition, `{"path":"/pool/_shared/repo-a","mode":"rw"}`) {
		t.Errorf("got %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", got)
	}
	// A miss returns *ErrNotFound.
	_, err = s.VolumeDefGetByName(ctx, "", "nope")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("miss err = %v, want *ErrNotFound", err)
	}
}

// testVolumeDefTenantIsolation pins the RFC N boundary: the SAME volume
// name created under two tenants resolves to each tenant's OWN row with no
// clobber. The (tenant_id, name) PK is what makes the two coexist.
func testVolumeDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.VolumeDefCreate(ctx, mkVolumeDef("tenant-a", "work", "/pool/tenant-a/work", "rw")); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := s.VolumeDefCreate(ctx, mkVolumeDef("tenant-b", "work", "/pool/tenant-b/work", "ro")); err != nil {
		t.Fatalf("create B: %v", err)
	}
	gotA, err := s.VolumeDefGetByName(ctx, "tenant-a", "work")
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if !jsonEqual(gotA.Definition, `{"path":"/pool/tenant-a/work","mode":"rw"}`) {
		t.Errorf("tenant A clobbered: %s", gotA.Definition)
	}
	gotB, err := s.VolumeDefGetByName(ctx, "tenant-b", "work")
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if !jsonEqual(gotB.Definition, `{"path":"/pool/tenant-b/work","mode":"ro"}`) {
		t.Errorf("tenant B clobbered: %s", gotB.Definition)
	}
	// Deleting A's row leaves B's untouched.
	found, err := s.VolumeDefDelete(ctx, "tenant-a", "work")
	if err != nil || !found {
		t.Fatalf("delete A: found=%v err=%v", found, err)
	}
	if _, err := s.VolumeDefGetByName(ctx, "tenant-b", "work"); err != nil {
		t.Errorf("tenant B's row vanished after A's delete: %v", err)
	}
}

func mkCredentialDef(tenant, scope, scopeID, name, value string) store.CredentialDefRow {
	return store.CredentialDefRow{
		TenantID: tenant, Scope: scope, ScopeID: scopeID, Name: name,
		Backend:    "inline",
		Definition: json.RawMessage(fmt.Sprintf(`{"value":%q}`, value)),
	}
}

// testCredentialDefScopeIsolation pins the RFC AR isolation boundary across both
// the tenant axis AND the user/agent scope axis: user A's per-user token must
// never collide with user B's, a per-user override coexists with a tenant
// default of the same name, and list is scoped to a single bucket.
func testCredentialDefScopeIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Tenant isolation: the same (scope, name) in two tenants don't clobber.
	if _, err := s.CredentialDefPut(ctx, mkCredentialDef("tenant-a", "tenant", "", "serper", "sealed-a")); err != nil {
		t.Fatalf("put tenant-a serper: %v", err)
	}
	if _, err := s.CredentialDefPut(ctx, mkCredentialDef("tenant-b", "tenant", "", "serper", "sealed-b")); err != nil {
		t.Fatalf("put tenant-b serper: %v", err)
	}
	gotA, err := s.CredentialDefGet(ctx, "tenant-a", "tenant", "", "serper")
	if err != nil || !jsonEqual(gotA.Definition, `{"value":"sealed-a"}`) {
		t.Errorf("tenant A serper = (%s, %v), want sealed-a", gotA.Definition, err)
	}

	// User-scope isolation: two users in the SAME tenant, SAME name → distinct.
	if _, err := s.CredentialDefPut(ctx, mkCredentialDef("tenant-a", "user", "userA", "telegram", "tokA")); err != nil {
		t.Fatalf("put userA telegram: %v", err)
	}
	if _, err := s.CredentialDefPut(ctx, mkCredentialDef("tenant-a", "user", "userB", "telegram", "tokB")); err != nil {
		t.Fatalf("put userB telegram: %v", err)
	}
	uA, err := s.CredentialDefGet(ctx, "tenant-a", "user", "userA", "telegram")
	if err != nil || !jsonEqual(uA.Definition, `{"value":"tokA"}`) {
		t.Errorf("userA telegram = (%s, %v), want tokA", uA.Definition, err)
	}
	uB, err := s.CredentialDefGet(ctx, "tenant-a", "user", "userB", "telegram")
	if err != nil || !jsonEqual(uB.Definition, `{"value":"tokB"}`) {
		t.Errorf("userB telegram = (%s, %v), want tokB (user A's token leaked to B)", uB.Definition, err)
	}

	// Scope shadowing: a tenant default of the same name coexists with the
	// per-user overrides (distinct rows, no clobber).
	if _, err := s.CredentialDefPut(ctx, mkCredentialDef("tenant-a", "tenant", "", "telegram", "teamTok")); err != nil {
		t.Fatalf("put tenant telegram default: %v", err)
	}
	if uA2, _ := s.CredentialDefGet(ctx, "tenant-a", "user", "userA", "telegram"); !jsonEqual(uA2.Definition, `{"value":"tokA"}`) {
		t.Errorf("userA telegram override clobbered by the tenant default: %s", uA2.Definition)
	}

	// List is scoped to a single (tenant, scope, scope_id) bucket.
	listUA, err := s.CredentialDefList(ctx, "tenant-a", "user", "userA")
	if err != nil {
		t.Fatalf("list userA: %v", err)
	}
	if len(listUA) != 1 || listUA[0].Name != "telegram" {
		t.Errorf("list userA = %d rows %v, want exactly [telegram] (leaked another bucket)", len(listUA), listUA)
	}

	// Delete A's tenant serper leaves tenant B's untouched.
	found, err := s.CredentialDefDelete(ctx, "tenant-a", "tenant", "", "serper")
	if err != nil || !found {
		t.Fatalf("delete tenant-a serper: found=%v err=%v", found, err)
	}
	if _, err := s.CredentialDefGet(ctx, "tenant-b", "tenant", "", "serper"); err != nil {
		t.Errorf("tenant B's serper vanished after A's delete: %v", err)
	}

	// A miss is *ErrNotFound.
	_, err = s.CredentialDefGet(ctx, "tenant-a", "user", "userA", "nope")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("get of a missing credential = %v, want *ErrNotFound", err)
	}
}

func testVolumeDefDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.VolumeDefCreate(ctx, mkVolumeDef("", "scratch", "/pool/_shared/scratch", "rw")); err != nil {
		t.Fatalf("create: %v", err)
	}
	found, err := s.VolumeDefDelete(ctx, "", "scratch")
	if err != nil || !found {
		t.Fatalf("delete: found=%v err=%v", found, err)
	}
	if _, err := s.VolumeDefGetByName(ctx, "", "scratch"); err == nil {
		t.Error("row still present after delete")
	}
	// Idempotent: deleting a missing row returns (false, nil).
	found, err = s.VolumeDefDelete(ctx, "", "scratch")
	if err != nil || found {
		t.Errorf("second delete: found=%v err=%v, want (false, nil)", found, err)
	}
}

func testVolumeDefList(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, n := range []string{"beta", "alpha", "gamma"} {
		if _, err := s.VolumeDefCreate(ctx, mkVolumeDef("t1", n, "/pool/t1/"+n, "rw")); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	// A different tenant's row must NOT appear in t1's list.
	if _, err := s.VolumeDefCreate(ctx, mkVolumeDef("t2", "other", "/pool/t2/other", "rw")); err != nil {
		t.Fatalf("create t2: %v", err)
	}
	rows, err := s.VolumeDefList(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("list returned %d rows, want 3", len(rows))
	}
	// Ordered by name.
	if rows[0].Name != "alpha" || rows[1].Name != "beta" || rows[2].Name != "gamma" {
		t.Errorf("list order = %s,%s,%s want alpha,beta,gamma", rows[0].Name, rows[1].Name, rows[2].Name)
	}
}

// testVolumeDefCreateUpdatesDefinition pins the tool's "re-create with a
// different mode updates the existing row" semantics (idempotent repoint).
func testVolumeDefCreateUpdatesDefinition(t *testing.T, s store.Store) {
	ctx := context.Background()
	first, err := s.VolumeDefCreate(ctx, mkVolumeDef("", "data", "/pool/_shared/data", "rw"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := s.VolumeDefCreate(ctx, mkVolumeDef("", "data", "/pool/_shared/data", "ro"))
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if !jsonEqual(second.Definition, `{"path":"/pool/_shared/data","mode":"ro"}`) {
		t.Errorf("definition not updated: %s", second.Definition)
	}
	// created_at preserved across the update; only one row exists.
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed on update: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	rows, _ := s.VolumeDefList(ctx, "")
	if len(rows) != 1 {
		t.Errorf("update minted a new row: %d rows", len(rows))
	}
}

// ---- RFC AH Phase 2b ephemeral volume contract tests ----

func mkEphemeralVolume(rootRunID, name, tenantID, path, mode string) store.EphemeralVolumeDefRow {
	return store.EphemeralVolumeDefRow{
		RootRunID:  rootRunID,
		Name:       name,
		TenantID:   tenantID,
		Definition: json.RawMessage(fmt.Sprintf(`{"path":%q,"mode":%q}`, path, mode)),
	}
}

// testEphemeralVolumeCreateListDelete pins the create/list/delete-by-run
// round-trip AND the load-bearing isolation property: the SAME name under
// two DIFFERENT root runs coexists (the (root_run_id, name) PK), so two
// concurrent runs never clobber each other's `work`.
func testEphemeralVolumeCreateListDelete(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Two distinct root runs each own a `work` volume — no clobber.
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume("run-1", "work", "", "/pool/_ephemeral/run-1/work", "rw")); err != nil {
		t.Fatalf("create run-1/work: %v", err)
	}
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume("run-1", "scratch", "", "/pool/_ephemeral/run-1/scratch", "ro")); err != nil {
		t.Fatalf("create run-1/scratch: %v", err)
	}
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume("run-2", "work", "", "/pool/_ephemeral/run-2/work", "rw")); err != nil {
		t.Fatalf("create run-2/work: %v", err)
	}

	// List is scoped to one root run, ordered by name.
	rows, err := s.EphemeralVolumeListByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("list run-1: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "scratch" || rows[1].Name != "work" {
		t.Fatalf("list run-1 = %+v, want [scratch, work]", rows)
	}
	if !jsonEqual(rows[1].Definition, `{"path":"/pool/_ephemeral/run-1/work","mode":"rw"}`) {
		t.Errorf("run-1/work body = %s", rows[1].Definition)
	}
	if rows[1].CreatedAt.IsZero() {
		t.Errorf("created_at not stamped: %+v", rows[1])
	}

	// Delete-by-run removes ALL of run-1's rows, returns the count, and
	// leaves run-2's row untouched.
	n, err := s.EphemeralVolumeDeleteByRun(ctx, "run-1")
	if err != nil || n != 2 {
		t.Fatalf("delete run-1: n=%d err=%v, want (2, nil)", n, err)
	}
	if rows, _ := s.EphemeralVolumeListByRun(ctx, "run-1"); len(rows) != 0 {
		t.Errorf("run-1 rows survived delete: %+v", rows)
	}
	if rows, _ := s.EphemeralVolumeListByRun(ctx, "run-2"); len(rows) != 1 {
		t.Errorf("run-2's row vanished after run-1's delete: %+v", rows)
	}

	// Idempotent: deleting a run that owns nothing returns (0, nil).
	if n, err := s.EphemeralVolumeDeleteByRun(ctx, "run-1"); err != nil || n != 0 {
		t.Errorf("second delete run-1: n=%d err=%v, want (0, nil)", n, err)
	}
}

// testEphemeralVolumeSweepCandidatesTerminalOnly verifies the sweeper's work
// list contains a run whose owning row is TERMINAL but EXCLUDES one that is
// still running.
func testEphemeralVolumeSweepCandidatesTerminalOnly(t *testing.T, s store.Store) {
	ctx := context.Background()

	// A terminal (completed) run with an ephemeral volume → a candidate.
	doneSess, _ := s.CreateSession(ctx, "tnt-done", "a", "u")
	doneRun, _ := s.CreateRun(ctx, doneSess.ID, store.RunIdentity{AgentID: "a_ev_done", TenantID: "tnt-done"})
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume(doneRun.ID, "work", "tnt-done", "/pool/_ephemeral/"+doneRun.ID+"/work", "rw")); err != nil {
		t.Fatalf("create done ephemeral: %v", err)
	}
	if err := s.FinishRun(ctx, doneRun.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatalf("finish done run: %v", err)
	}

	// A still-running run with an ephemeral volume → NOT a candidate.
	liveSess, _ := s.CreateSession(ctx, "tnt-live", "a", "u")
	liveRun, _ := s.CreateRun(ctx, liveSess.ID, store.RunIdentity{AgentID: "a_ev_live", TenantID: "tnt-live"})
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume(liveRun.ID, "work", "tnt-live", "/pool/_ephemeral/"+liveRun.ID+"/work", "rw")); err != nil {
		t.Fatalf("create live ephemeral: %v", err)
	}

	cands, err := s.EphemeralVolumeSweepCandidates(ctx)
	if err != nil {
		t.Fatalf("sweep candidates: %v", err)
	}
	got := map[string]string{}
	for _, c := range cands {
		got[c.RootRunID] = c.TenantID
	}
	if tnt, ok := got[doneRun.ID]; !ok || tnt != "tnt-done" {
		t.Errorf("terminal run not a candidate (or wrong tenant): got=%v", got)
	}
	if _, ok := got[liveRun.ID]; ok {
		t.Errorf("still-running run wrongly appeared as a sweep candidate")
	}
}

// testEphemeralVolumeSweepCandidatesSkipsPaused is the fail-before guard for
// the paused-skip: a PAUSED run is parked (it will resume + keep using its
// volumes), so it must NEVER be a sweep candidate even though it is not
// "running". Dropping the COALESCE(pause_state) NOT IN (paused,pausing)
// clause makes this run appear → the test fails.
func testEphemeralVolumeSweepCandidatesSkipsPaused(t *testing.T, s store.Store) {
	ctx := context.Background()

	sess, _ := s.CreateSession(ctx, "tnt-paused", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ev_paused", TenantID: "tnt-paused"})
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume(run.ID, "work", "tnt-paused", "/pool/_ephemeral/"+run.ID+"/work", "rw")); err != nil {
		t.Fatalf("create paused ephemeral: %v", err)
	}
	// A snapshotted+restored paused run is TERMINAL-status'd... no — a paused
	// run stays status=running with pause_state=paused. Mark it failed too to
	// prove the pause_state guard (not just the status filter) is what keeps
	// it out: a terminal status with pause_state=paused must STILL be skipped.
	if err := s.FinishRun(ctx, run.ID, store.RunFailed, "", store.Usage{}, "stale-style terminal"); err != nil {
		t.Fatalf("finish paused run: %v", err)
	}
	if err := s.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatalf("set paused: %v", err)
	}

	cands, err := s.EphemeralVolumeSweepCandidates(ctx)
	if err != nil {
		t.Fatalf("sweep candidates: %v", err)
	}
	for _, c := range cands {
		if c.RootRunID == run.ID {
			t.Fatalf("PAUSED run appeared as a sweep candidate — its volumes would be wrongly purged before resume")
		}
	}
}

// testEphemeralVolumeListByTenantIsolation pins the RFC AH Phase 4 read path:
// EphemeralVolumeListByTenant returns ONLY the asked-for tenant's live
// ephemeral rows (across all of that tenant's runs), never another tenant's.
// This is the store-boundary guarantee the Web UI's ephemeral view relies on.
func testEphemeralVolumeListByTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Tenant A owns two runs, each with an ephemeral volume.
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume("run-a1", "work", "tnt-a", "/pool/_ephemeral/run-a1/work", "rw")); err != nil {
		t.Fatalf("create a1: %v", err)
	}
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume("run-a2", "scratch", "tnt-a", "/pool/_ephemeral/run-a2/scratch", "ro")); err != nil {
		t.Fatalf("create a2: %v", err)
	}
	// Tenant B owns one — must NOT appear in A's list.
	if _, err := s.EphemeralVolumeCreate(ctx, mkEphemeralVolume("run-b1", "work", "tnt-b", "/pool/_ephemeral/run-b1/work", "rw")); err != nil {
		t.Fatalf("create b1: %v", err)
	}

	rowsA, err := s.EphemeralVolumeListByTenant(ctx, "tnt-a")
	if err != nil {
		t.Fatalf("list tnt-a: %v", err)
	}
	if len(rowsA) != 2 {
		t.Fatalf("tnt-a list = %d rows, want 2: %+v", len(rowsA), rowsA)
	}
	for _, r := range rowsA {
		if r.TenantID != "tnt-a" {
			t.Errorf("tnt-a list leaked a %q row: %+v", r.TenantID, r)
		}
	}
	// Ordered by (root_run_id, name): run-a1/work then run-a2/scratch.
	if rowsA[0].RootRunID != "run-a1" || rowsA[0].Name != "work" {
		t.Errorf("tnt-a[0] = %s/%s, want run-a1/work", rowsA[0].RootRunID, rowsA[0].Name)
	}
	if !jsonEqual(rowsA[0].Definition, `{"path":"/pool/_ephemeral/run-a1/work","mode":"rw"}`) {
		t.Errorf("tnt-a[0] body = %s", rowsA[0].Definition)
	}

	rowsB, err := s.EphemeralVolumeListByTenant(ctx, "tnt-b")
	if err != nil {
		t.Fatalf("list tnt-b: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].RootRunID != "run-b1" {
		t.Fatalf("tnt-b list = %+v, want one run-b1 row", rowsB)
	}
}

// ── RFC AL Path primitive — dirent (path tree) substrate ────────────────────

func mkDirent(tenantID, scope, scopeID, parentPath, name, kind, ref string) store.DirentRow {
	return store.DirentRow{
		TenantID: tenantID, Scope: scope, ScopeID: scopeID,
		ParentPath: parentPath, Name: name, Kind: kind,
		ResourceRef: json.RawMessage(ref),
	}
}

func testDirentCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.DirentCreate(ctx, mkDirent("", "user", "u1", "/", "docs", "directory", `{}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.DirentGet(ctx, "", "user", "u1", "/", "docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "docs" || got.Kind != "directory" {
		t.Errorf("got %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", got)
	}
	var nf *store.ErrNotFound
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/", "nope"); !errors.As(err, &nf) {
		t.Errorf("miss err = %v, want *ErrNotFound", err)
	}
	// An empty resource_ref must round-trip as valid JSON on BOTH backends
	// (sqlite TEXT accepts ""; postgres jsonb rejects "" — DirentCreate
	// defaults empty to "{}"). Review finding #2 / backend-parity guard.
	got2, err := s.DirentCreate(ctx, store.DirentRow{Scope: "user", ScopeID: "u1", ParentPath: "/", Name: "noref", Kind: "directory"})
	if err != nil {
		t.Fatalf("create with empty ref: %v", err)
	}
	if !jsonEqual(got2.ResourceRef, `{}`) {
		t.Errorf("empty ref = %s, want {}", got2.ResourceRef)
	}
}

func testDirentListOneLevel(t *testing.T, s store.Store) {
	ctx := context.Background()
	mustCreate := func(parent, name string) {
		if _, err := s.DirentCreate(ctx, mkDirent("", "user", "u1", parent, name, "directory", `{}`)); err != nil {
			t.Fatalf("create %s%s: %v", parent, name, err)
		}
	}
	mustCreate("/", "docs")
	mustCreate("/docs/", "a")
	mustCreate("/docs/", "b")
	mustCreate("/docs/a/", "deep") // must NOT show in a one-level listing of /docs/
	got, err := s.DirentList(ctx, "", "user", "u1", "/docs/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("one-level list of /docs/ = %+v, want [a b]", names(got))
	}
}

func testDirentListUnderRecursive(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, d := range [][2]string{{"/", "docs"}, {"/docs/", "a"}, {"/docs/a/", "b"}, {"/", "other"}} {
		if _, err := s.DirentCreate(ctx, mkDirent("", "user", "u1", d[0], d[1], "directory", `{}`)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	got, err := s.DirentListUnder(ctx, "", "user", "u1", "/docs/")
	if err != nil {
		t.Fatalf("list under: %v", err)
	}
	// a (parent /docs/) + b (parent /docs/a/); NOT docs itself (parent /), NOT other.
	if len(got) != 2 {
		t.Fatalf("recursive list of /docs/ = %+v, want 2 (a, b)", names(got))
	}
}

func testDirentDeleteAndDeleteUnder(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, d := range [][2]string{{"/", "docs"}, {"/docs/", "a"}, {"/docs/a/", "b"}} {
		if _, err := s.DirentCreate(ctx, mkDirent("", "user", "u1", d[0], d[1], "directory", `{}`)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	n, err := s.DirentDeleteUnder(ctx, "", "user", "u1", "/docs/")
	if err != nil {
		t.Fatalf("delete under: %v", err)
	}
	if n != 2 { // a + b; the /docs entry (parent /) is NOT a descendant
		t.Errorf("deleteUnder count = %d, want 2", n)
	}
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/", "docs"); err != nil {
		t.Errorf("the /docs entry should survive deleteUnder: %v", err)
	}
	found, err := s.DirentDelete(ctx, "", "user", "u1", "/", "docs")
	if err != nil || !found {
		t.Fatalf("delete /docs: found=%v err=%v", found, err)
	}
	found, _ = s.DirentDelete(ctx, "", "user", "u1", "/", "docs")
	if found {
		t.Error("second delete should be idempotent (found=false)")
	}
}

func testDirentMoveCascade(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, d := range [][2]string{{"/", "docs"}, {"/docs/", "a"}, {"/docs/a/", "b"}} {
		if _, err := s.DirentCreate(ctx, mkDirent("", "user", "u1", d[0], d[1], "directory", `{}`)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// mv /docs -> /archive (a dir with descendants → cascade).
	moved, err := s.DirentMove(ctx, "", "user", "u1", "/", "docs", "/", "archive")
	if err != nil || !moved {
		t.Fatalf("move: moved=%v err=%v", moved, err)
	}
	// The entry itself + every descendant must live under /archive now.
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/", "archive"); err != nil {
		t.Errorf("moved entry /archive missing: %v", err)
	}
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/archive/", "a"); err != nil {
		t.Errorf("cascade: /archive/a missing: %v", err)
	}
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/archive/a/", "b"); err != nil {
		t.Errorf("cascade: /archive/a/b missing: %v", err)
	}
	// Old coordinates must be gone.
	var nf *store.ErrNotFound
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/", "docs"); !errors.As(err, &nf) {
		t.Errorf("old /docs still present after move: %v", err)
	}
	if _, err := s.DirentGet(ctx, "", "user", "u1", "/docs/", "a"); !errors.As(err, &nf) {
		t.Errorf("old /docs/a still present after move: %v", err)
	}
	// Moving an absent source returns found=false.
	moved, err = s.DirentMove(ctx, "", "user", "u1", "/", "ghost", "/", "x")
	if err != nil || moved {
		t.Errorf("move of absent source: moved=%v err=%v, want false/nil", moved, err)
	}
}

func testDirentScopeIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	// Same path /notes in agent scope vs tenant scope → distinct dirents.
	if _, err := s.DirentCreate(ctx, mkDirent("", "agent", "a1", "/", "notes", "memory_entry", `{"k":"agent"}`)); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := s.DirentCreate(ctx, mkDirent("", "tenant", "", "/", "notes", "memory_entry", `{"k":"tenant"}`)); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	gotA, err := s.DirentGet(ctx, "", "agent", "a1", "/", "notes")
	if err != nil || !jsonEqual(gotA.ResourceRef, `{"k":"agent"}`) {
		t.Errorf("agent /notes = %+v err=%v", gotA, err)
	}
	// The agent-scope listing must not see the tenant-scope dirent.
	list, err := s.DirentList(ctx, "", "agent", "a1", "/")
	if err != nil {
		t.Fatalf("list agent: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("agent / listing leaked across scope: %+v", names(list))
	}
}

func testDirentTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.DirentCreate(ctx, mkDirent("tnt-a", "user", "u1", "/", "x", "directory", `{"t":"a"}`)); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := s.DirentCreate(ctx, mkDirent("tnt-b", "user", "u1", "/", "x", "directory", `{"t":"b"}`)); err != nil {
		t.Fatalf("create B: %v", err)
	}
	gotB, err := s.DirentGet(ctx, "tnt-b", "user", "u1", "/", "x")
	if err != nil || !jsonEqual(gotB.ResourceRef, `{"t":"b"}`) {
		t.Errorf("tenant B /x = %+v err=%v", gotB, err)
	}
	// Deleting A's dirent leaves B's untouched.
	if _, err := s.DirentDelete(ctx, "tnt-a", "user", "u1", "/", "x"); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if _, err := s.DirentGet(ctx, "tnt-b", "user", "u1", "/", "x"); err != nil {
		t.Errorf("tenant B vanished after A delete: %v", err)
	}
}

func names(rows []store.DirentRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}
