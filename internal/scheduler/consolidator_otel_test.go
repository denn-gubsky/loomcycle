package scheduler

// Consolidation telemetry (RFC BL P2).
//
// These pin the operator-facing contract of the loomcycle.memory.consolidate
// span: the attribute KEYS (operators write Jaeger/Tempo filters against them,
// so a rename is a breaking change), that the counts come from observed store
// state rather than the pass's prose report, that a failed pass is marked as an
// error rather than reading as a success, and that the watermark lag reflects a
// STALE watermark — the signal for "this target is stuck".

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func withInMemoryExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	t.Cleanup(func() {
		cleanup()
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

// consolidateSpan returns the single loomcycle.memory.consolidate span, plus its
// attributes split by type for readable assertions.
func consolidateSpan(t *testing.T, exp *tracetest.InMemoryExporter) (tracetest.SpanStub, map[string]string, map[string]int64, map[string]bool) {
	t.Helper()
	spans := exp.GetSpans()
	var found *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == lcotel.SpanMemoryConsolidate {
			found = &spans[i]
			break
		}
	}
	if found == nil {
		names := make([]string, 0, len(spans))
		for _, s := range spans {
			names = append(names, s.Name)
		}
		t.Fatalf("no %q span recorded; got %v", lcotel.SpanMemoryConsolidate, names)
	}
	strs := map[string]string{}
	ints := map[string]int64{}
	bools := map[string]bool{}
	for _, kv := range found.Attributes {
		switch kv.Value.Type() {
		case 1: // BOOL
			bools[string(kv.Key)] = kv.Value.AsBool()
		case 2: // INT64
			ints[string(kv.Key)] = kv.Value.AsInt64()
		default:
			strs[string(kv.Key)] = kv.Value.AsString()
		}
	}
	return *found, strs, ints, bools
}

// otelFanoutFixture builds a fan-out scheduler with the given seeded target and
// returns it plus the store.
func otelFanoutFixture(t *testing.T, userID string) (*Scheduler, *fakeRunner, store.Store) {
	t.Helper()
	sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	seedSettledSession(t, st, "", userID)
	return sched, fr, st
}

// TestConsolidationTelemetry_SpanCarriesObservedOutcome pins the whole attribute
// set on a pass that actually did something.
//
// The fake pass writes its facts THROUGH THE STORE from inside RunOnce, which is
// the point: the counts on the span are a diff of observed store state, so a pass
// that reported "wrote 2 facts" while writing none would show added=0. Nothing
// here reads the pass's report.
//
// FAIL-BEFORE: without the observePass diff (or with the RecordMemoryConsolidate
// call removed) there is no span at all; with the diff reading only the AFTER
// side, added/superseded stay 0. Both were verified by making those edits.
func TestConsolidationTelemetry_SpanCarriesObservedOutcome(t *testing.T) {
	exp := withInMemoryExporter(t)
	const userID = "u-observed"
	sched, fr, st := otelFanoutFixture(t, userID)
	ctx := context.Background()

	// A pre-existing consolidated fact, and a queued add — so the pass has
	// something to update, something to archive, and something to drain.
	mustSet := func(key, val string) {
		t.Helper()
		raw, _ := json.Marshal(val)
		if err := st.MemorySetProvenance(ctx, "", store.MemoryScopeUser, userID, key, raw, 0,
			store.MemoryProvenance{Origin: "consolidator", Class: "fact"}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	mustSet("memory/fact/kept", "an existing fact the pass rewrites")
	mustSet("memory/fact/stale", "a fact the pass archives")
	if err := st.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
		ID: "pend_otel_1", Scope: store.MemoryScopeUser, ScopeID: userID,
		Payload: json.RawMessage(`{"messages":[]}`), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The pass: one add, one in-place update, one supersede, one ack — plus a
	// usage event carrying the provider/model that actually served it.
	fr.onCallbacks = func(_ runner.RunInput, cb runner.RunCallbacks) {
		mustSet("memory/fact/new", "a fact the pass added")
		// A distinct updated_at is what makes an in-place rewrite observable; the
		// store stamps wall-clock nanoseconds, so re-writing in the same
		// microsecond could otherwise read as unchanged.
		time.Sleep(2 * time.Millisecond)
		mustSet("memory/fact/kept", "the same fact, restated")
		if err := st.MemorySupersede(ctx, "", store.MemoryScopeUser, userID, "memory/fact/stale"); err != nil {
			t.Fatalf("supersede: %v", err)
		}
		if err := st.MemoryPendingAck(ctx, "", store.MemoryScopeUser, userID, []string{"pend_otel_1"}); err != nil {
			t.Fatalf("ack: %v", err)
		}
		if cb.OnEvent != nil {
			cb.OnEvent(providers.Event{Type: providers.EventUsage, Usage: &providers.Usage{
				InputTokens: 100, OutputTokens: 20, Provider: "anthropic", Model: "claude-haiku-x",
			}})
		}
	}

	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())

	span, strs, ints, bools := consolidateSpan(t, exp)

	// Identity attributes.
	for key, want := range map[string]string{
		lcotel.AttrMemoryScope: string(store.MemoryScopeUser),
		lcotel.AttrUserID:      userID,
		lcotel.AttrAgentName:   "memory/consolidator",
		lcotel.AttrProvider:    "anthropic",
		lcotel.AttrModel:       "claude-haiku-x",
	} {
		if strs[key] != want {
			t.Errorf("%s = %q, want %q", key, strs[key], want)
		}
	}

	// Observed outcome counts.
	for key, want := range map[string]int64{
		lcotel.AttrConsolidateAdded:          1,
		lcotel.AttrConsolidateUpdated:        1,
		lcotel.AttrConsolidateSuperseded:     1,
		lcotel.AttrConsolidatePendingDrained: 1,
		lcotel.AttrConsolidateSessionsRead:   1,
	} {
		if ints[key] != want {
			t.Errorf("%s = %d, want %d", key, ints[key], want)
		}
	}
	if bools[lcotel.AttrConsolidateNoop] {
		t.Errorf("%s = true on a pass that added, updated, and superseded", lcotel.AttrConsolidateNoop)
	}
	if _, ok := bools[lcotel.AttrConsolidateCountsTruncated]; ok {
		t.Errorf("%s set on a 3-key scope", lcotel.AttrConsolidateCountsTruncated)
	}
	// A successful pass must not read as an error.
	if span.Status.Code == codes.Error {
		t.Errorf("span marked Error on a successful pass: %v", span.Status.Description)
	}
	// No fact text, no transcript, no query anywhere on the span.
	for k, v := range strs {
		for _, leak := range []string{"an existing fact", "the same fact, restated", "a fact the pass added"} {
			if v == leak {
				t.Errorf("attribute %s carries memory fact text: %q", k, v)
			}
		}
	}
}

// TestConsolidationTelemetry_NoopPassIsFlagged: a pass that runs, succeeds, and
// changes NOTHING must be visible as such. This is the shape of a wedged target —
// the schedule fires, the run completes, the bill accrues, and no memory moves —
// and without the noop bit it is indistinguishable from healthy work in traces.
//
// FAIL-BEFORE: hardcoding the noop attribute to false makes this fail; so does
// computing it from the pass's report instead of the observed diff.
func TestConsolidationTelemetry_NoopPassIsFlagged(t *testing.T) {
	exp := withInMemoryExporter(t)
	sched, _, _ := otelFanoutFixture(t, "u-noop")

	sched.fireConsolidationFanout(context.Background(),
		store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())

	_, _, ints, bools := consolidateSpan(t, exp)
	if !bools[lcotel.AttrConsolidateNoop] {
		t.Errorf("%s = false on a pass that wrote nothing (added=%d updated=%d superseded=%d)",
			lcotel.AttrConsolidateNoop,
			ints[lcotel.AttrConsolidateAdded], ints[lcotel.AttrConsolidateUpdated], ints[lcotel.AttrConsolidateSuperseded])
	}
}

// TestConsolidationTelemetry_SessionsReadExcludesThePassOwnSessions guards the
// backlog gauge against the perpetual-pass trap.
//
// Every pass creates its own settled session under the TARGET's user id, and a
// pass never consolidates itself — so those sessions sit past the watermark
// forever. Counting them would make sessions_read climb by one on every tick and
// turn the backlog gauge into a tick counter: an idle target would look like a
// growing backlog, and the one metric an operator uses to decide "is consolidation
// keeping up" would answer "no" permanently.
//
// The dispatcher's has-new-work probe and the cursor_scan op both already exclude
// the pass's own agent name; this is the third place that has to, and the one
// least likely to be noticed if it drifted.
//
// FAIL-BEFORE: passing "" instead of selfAgent to ConsolidatableSessions inside
// observePass counts the consolidator's own session, giving sessions_read = 2 —
// verified by making that edit (it was in fact the original bug).
func TestConsolidationTelemetry_SessionsReadExcludesThePassOwnSessions(t *testing.T) {
	exp := withInMemoryExporter(t)
	const userID = "u-selfexcl"
	sched, _, st := otelFanoutFixture(t, userID)

	// A settled session authored by the CONSOLIDATOR itself, under the target's
	// user id — exactly what a previous pass leaves behind.
	seedSettledSessionAs(t, st, "", userID, "memory/consolidator")

	sched.fireConsolidationFanout(context.Background(),
		store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())

	_, _, ints, _ := consolidateSpan(t, exp)
	if got := ints[lcotel.AttrConsolidateSessionsRead]; got != 1 {
		t.Errorf("%s = %d, want 1 — the pass's OWN past session must not count as backlog, or the gauge becomes a tick counter",
			lcotel.AttrConsolidateSessionsRead, got)
	}
}

// TestConsolidationTelemetry_WatermarkLagReflectsAStaleWatermark is the
// operability headline: the lag must measure how far behind now() the target's
// watermark actually sits.
//
// A target whose lag grows without bound is stuck — the pass fires, the run
// succeeds, and the watermark does not move. That is exactly the livelock the
// ascending scan and the iteration cap exist to prevent, and the lag is the only
// signal that surfaces it, because every other indicator (schedule status, run
// state, token spend) looks healthy.
//
// FAIL-BEFORE: taking the lag from the BEFORE observation, or dropping the
// AttrConsolidateWatermarkLagMs stamp, leaves the lag at 0 on a stale watermark
// — verified by making those edits.
func TestConsolidationTelemetry_WatermarkLagReflectsAStaleWatermark(t *testing.T) {
	exp := withInMemoryExporter(t)
	const userID = "u-stale"
	sched, _, st := otelFanoutFixture(t, userID)
	ctx := context.Background()

	// A watermark left FAR in the past: the target consolidated something two
	// hours ago and has not moved since, while its chat has settled in the
	// meantime. That is the stuck shape — there is work past the watermark, the
	// pass will fire, and the pass will not advance it.
	//
	// The stale point is written through the STORE, not the Memory tool: the tool's
	// cursor_advance deliberately verifies the (completed_at, session_id) pair
	// against a real settled session within a 1s skew, so it cannot express a
	// two-hour-old watermark — which is exactly the state this test needs.
	staleAt := time.Now().UTC().Add(-2 * time.Hour)
	if _, acquired, err := st.MemoryCursorLease(ctx, "", store.MemoryScopeUser, userID, "seed", time.Now().UTC(), time.Minute); err != nil || !acquired {
		t.Fatalf("lease: acquired=%v err=%v", acquired, err)
	}
	if err := st.MemoryCursorAdvance(ctx, "", store.MemoryScopeUser, userID, "seed", staleAt, "s_long_gone"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := st.MemoryCursorRelease(ctx, "", store.MemoryScopeUser, userID, "seed"); err != nil {
		t.Fatalf("release: %v", err)
	}

	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())

	_, _, ints, _ := consolidateSpan(t, exp)
	lagMs, ok := ints[lcotel.AttrConsolidateWatermarkLagMs]
	if !ok {
		t.Fatalf("%s absent — the single most useful 'is consolidation keeping up' signal", lcotel.AttrConsolidateWatermarkLagMs)
	}
	// Assert the lag TRACKS the seeded staleness rather than pinning a magic
	// number: at least the 2h that has genuinely elapsed, minus a generous margin
	// for test wall-clock, and not absurdly beyond it.
	wantAtLeast := (2*time.Hour - time.Minute).Milliseconds()
	wantAtMost := (2*time.Hour + time.Minute).Milliseconds()
	if lagMs < wantAtLeast || lagMs > wantAtMost {
		t.Errorf("%s = %dms, want ~%dms (the watermark's real staleness) — the lag must reflect how far behind the target actually is",
			lcotel.AttrConsolidateWatermarkLagMs, lagMs, (2 * time.Hour).Milliseconds())
	}

	// The lag is also a span EVENT, which is what a downstream connector turns
	// into a gauge (the in-process /metrics endpoint is substrate-only).
	span, _, _, _ := consolidateSpan(t, exp)
	var sawEvent bool
	for _, ev := range span.Events {
		if ev.Name == lcotel.EventConsolidateWatermarkLag {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Errorf("no %q span event; events = %+v", lcotel.EventConsolidateWatermarkLag, span.Events)
	}
}

// TestConsolidationTelemetry_FailedPassMarksSpanErrored: a pass that failed must
// not read as a success in traces, or the derived error series silently
// under-reports exactly the passes an operator needs to find. Mirrors the
// same floor on loomcycle.memory.search and Dispatcher.Execute.
//
// FAIL-BEFORE: dropping ConsolidateOutcome.Err (or the SetSpanError call in
// SetMemoryConsolidateResult) leaves the span at the default Unset status —
// verified by making that edit.
func TestConsolidationTelemetry_FailedPassMarksSpanErrored(t *testing.T) {
	exp := withInMemoryExporter(t)
	sched, fr, _ := otelFanoutFixture(t, "u-failed")
	fr.runErr = errors.New("provider exploded mid-pass")

	sched.fireConsolidationFanout(context.Background(),
		store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())

	span, _, _, _ := consolidateSpan(t, exp)
	if span.Status.Code != codes.Error {
		t.Errorf("span Status.Code = %v, want Error (a failed pass must not read as a success)", span.Status.Code)
	}
}

// TestConsolidationTelemetry_ObservationSkippedWhenNotRecording pins the COST
// floor.
//
// The observation is STORE READS — four bounded queries per target, twice per
// pass. Paying for them to feed a no-op tracer would tax every operator who never
// enabled tracing, so observePass is gated on span.IsRecording(). This asserts the
// gate directly (an unobserved pair yields a zero outcome with no lag and no
// counts) plus the end-to-end property that a pass still dispatches normally with
// telemetry off — so a future refactor cannot quietly ungate it.
func TestConsolidationTelemetry_ObservationSkippedWhenNotRecording(t *testing.T) {
	sched, fr, st := otelFanoutFixture(t, "u-untraced")
	ctx := context.Background()
	target := consolidationTarget{Scope: store.MemoryScopeUser, UserID: "u-untraced"}

	// Seed state the observation WOULD have picked up, so a leaked read is
	// visible as a non-zero count rather than as an indistinguishable zero.
	raw, _ := json.Marshal("a fact")
	if err := st.MemorySetProvenance(ctx, "", store.MemoryScopeUser, "u-untraced", "memory/fact/x", raw, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "fact"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	off := sched.observePass(ctx, target, "memory/consolidator", false)
	if off.observed || off.keys != nil {
		t.Errorf("observePass ran with recording=false: %+v", off)
	}
	out := off.diff(off, "", "", nil)
	if out.Added != 0 || out.Updated != 0 || out.Superseded != 0 || out.PendingDrained != 0 || out.WatermarkLag != 0 {
		t.Errorf("an unobserved pass produced a non-zero outcome: %+v", out)
	}

	// And with no tracer configured at all, the pass still runs.
	var ran bool
	fr.onCallbacks = func(_ runner.RunInput, _ runner.RunCallbacks) { ran = true }
	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())
	if !ran {
		t.Error("the pass did not dispatch with tracing off")
	}
}
