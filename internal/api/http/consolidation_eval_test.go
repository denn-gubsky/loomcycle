package http

// The consolidation eval harness — PIPELINE half (RFC BL P2).
//
// WHY HERE AND NOT IN internal/memory/eval. These invariants need a REAL agent
// run: a POST /v1/runs through the real loop, dispatching real Memory/History
// tool calls under the real grants, so that scope resolution, the consolidation
// gate, provenance stamping, the lease, and the watermark are all exercised as
// production wires them. The only machinery that can drive that is this package's
// scriptedProvider (test-only, so not importable). Duplicating the loop wiring
// under internal/memory/eval would be ~150 lines that immediately start drifting
// from the server's real construction — which is the thing under test.
//
// The FIXTURE CORPUS is imported from internal/memory/eval so both halves plant
// the same facts, distractors, and secret. See that package's
// consolidation_fixtures.go header.
//
// WHAT IS AND IS NOT PROVEN. Only the PROVIDER is stubbed: the script replays the
// tool sequence the `memory/consolidate` skill prescribes, in the order it
// prescribes, which is precisely the part a live model decides. So the pipeline's
// plumbing is real and the model's judgement is the single stand-in. Where an
// invariant depends on model judgement rather than a runtime guard, that is
// called out on the test — and the negative fixtures are checked by
// eval.CheckForbidden, which is itself tested both ways so it cannot pass by
// never firing.
//
// Runs offline in seconds: no network, no API key, no Postgres. `go test ./...`
// covers it; `make memory-eval-mock` runs it plus the retrieval half.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

const evalUserID = "alice"

// evalFixture is the seeded corpus plus the store identities the pass must relay
// as provenance.
type evalFixture struct {
	corpus eval.ConsolidationCorpus
	chats  map[string]eval.SeededChat
	// last is the newest-settled chat — the composite watermark's target.
	last eval.SeededChat
}

// seedEvalFixture ingests the corpus into env's store and pre-seeds the stale
// fact "an earlier pass" wrote, so the current pass has something to supersede.
func seedEvalFixture(t *testing.T, env *consolidationEnv) evalFixture {
	t.Helper()
	ctx := context.Background()
	corpus := eval.ConsolidationFixture()
	chats, err := eval.SeedConsolidationChats(ctx, env.store, "", evalUserID, corpus)
	if err != nil {
		t.Fatalf("seed fixture corpus: %v", err)
	}
	last, ok := chats[eval.ChatSwitch]
	if !ok {
		t.Fatalf("fixture lost its newest chat %q", eval.ChatSwitch)
	}

	// The stale fact, as an earlier pass would have left it: consolidator-origin,
	// classed, sourced from a chat that is already behind the watermark.
	sup := corpus.Supersede
	staleVal, _ := json.Marshal(sup.StaleText)
	if err := env.store.MemorySetProvenance(ctx, "", store.MemoryScopeUser, evalUserID, sup.StaleKey, staleVal, 0,
		store.MemoryProvenance{
			Origin:          "consolidator",
			Class:           sup.StaleClass,
			SourceSessionID: chats[eval.ChatPrefs].SessionID,
			SourceRunID:     chats[eval.ChatPrefs].RunID,
		}); err != nil {
		t.Fatalf("seed the stale fact an earlier pass wrote: %v", err)
	}
	return evalFixture{corpus: corpus, chats: chats, last: last}
}

// writtenFacts is what the pass distils on this run: every planted fact EXCEPT
// the one it archives (that row already exists from an earlier pass, and a pass
// that re-wrote a fact only to immediately supersede it would be modelling a
// confused pass, not the pipeline).
func (f evalFixture) writtenFacts() []eval.PlantedFact {
	out := make([]eval.PlantedFact, 0, len(f.corpus.Facts))
	for _, fact := range f.corpus.Facts {
		if fact.Key == f.corpus.Supersede.StaleKey {
			continue
		}
		out = append(out, fact)
	}
	return out
}

// enqueuePending puts un-drained Memory.add rows on the target's queue, so the
// pass's drain + ack legs act on something real rather than an empty queue.
func enqueuePending(t *testing.T, env *consolidationEnv, ids ...string) {
	t.Helper()
	for i, id := range ids {
		if err := env.store.MemoryPendingEnqueue(context.Background(), store.MemoryPendingRow{
			ID:        id,
			Scope:     store.MemoryScopeUser,
			ScopeID:   evalUserID,
			Payload:   json.RawMessage(`{"messages":[{"role":"user","content":"queued for consolidation"}]}`),
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("enqueue pending %s: %v", id, err)
		}
	}
}

// evalPassScript assembles one FULL pass over the whole fixture corpus, in the
// order the skill body prescribes: lease -> cursor_get -> cursor_scan -> a
// History get PER settled chat -> pending_drain -> pending_ack -> recall -> the
// writes -> cursor_advance -> cursor_release -> report.
//
// cursor_scan (not History list) is the discovery leg, because that is the op the
// skill prescribes and the only one whose ordering guarantees the watermark can
// be advanced safely. Reading every chat's transcript is what makes the
// secret-not-written assertion non-vacuous: the credential genuinely enters the
// model's context through a real tool result.
func (f evalFixture) evalPassScript(pendingIDs []string, writes ...string) [][]providers.Event {
	scripts := [][]providers.Event{
		toolCall("tu_lease", "Memory", `{"op":"cursor_lease","scope":"user","lease_ttl_ms":600000}`),
		toolCall("tu_cursor", "Memory", `{"op":"cursor_get","scope":"user"}`),
		toolCall("tu_scan", "Memory", `{"op":"cursor_scan","scope":"user","limit":10}`),
	}
	// Settle order, so the transcripts arrive oldest-first exactly as the scan
	// hands them out.
	for _, label := range []string{eval.ChatPrefs, eval.ChatDeploy, eval.ChatSwitch} {
		scripts = append(scripts, toolCall("tu_get_"+label, "History",
			fmt.Sprintf(`{"op":"get","scope":"user","session_id":%q,"format":"markdown"}`, f.chats[label].SessionID)))
	}
	scripts = append(scripts,
		toolCall("tu_drain", "Memory", `{"op":"pending_drain","scope":"user","limit":50}`))
	if len(pendingIDs) > 0 {
		raw, _ := json.Marshal(map[string]any{"op": "pending_ack", "scope": "user", "ids": pendingIDs})
		scripts = append(scripts, toolCall("tu_ack", "Memory", string(raw)))
	}
	scripts = append(scripts,
		toolCall("tu_recall", "Memory", `{"op":"recall","scope":"user","query":"what do I know about this user","top_k":8}`))
	for i, w := range writes {
		scripts = append(scripts, toolCall(fmt.Sprintf("tu_write_%d", i), "Memory", w))
	}
	scripts = append(scripts,
		toolCall("tu_advance", "Memory", fmt.Sprintf(`{"op":"cursor_advance","scope":"user","completed_at":%q,"session_id":%q}`,
			f.last.SettledAt.UTC().Format(time.RFC3339Nano), f.last.SessionID)),
		toolCall("tu_release", "Memory", `{"op":"cursor_release","scope":"user"}`),
		finalText("consolidated 3 chats: 2 facts written, 1 stale fact archived"),
	)
	return scripts
}

// fullPassWrites renders the pass's Memory writes: a provenance-carrying `set`
// per distilled fact, then the supersede that archives the contradicted one.
func (f evalFixture) fullPassWrites() []string {
	var writes []string
	for _, fact := range f.writtenFacts() {
		writes = append(writes, setOp(fact.Key, fact.Text, fact.Class, f.chats[fact.Chat].SessionID, f.chats[fact.Chat].RunID))
	}
	writes = append(writes, fmt.Sprintf(`{"op":"supersede","scope":"user","key":%q}`, f.corpus.Supersede.StaleKey))
	return writes
}

// liveEntries is the target's live (non-superseded) memory row set.
func liveEntries(t *testing.T, env *consolidationEnv) []store.MemoryEntry {
	t.Helper()
	entries, _, err := env.store.MemoryList(context.Background(), "", store.MemoryScopeUser, evalUserID, "", 500)
	if err != nil {
		t.Fatalf("MemoryList: %v", err)
	}
	return entries
}

// runFullPass seeds, scripts, and executes one complete pass, returning the
// fixture and the run's SSE stream.
func runFullPass(t *testing.T, env *consolidationEnv, pendingIDs ...string) (evalFixture, string) {
	t.Helper()
	f := seedEvalFixture(t, env)
	if len(pendingIDs) > 0 {
		enqueuePending(t, env, pendingIDs...)
	}
	env.prov.scripts = f.evalPassScript(pendingIDs, f.fullPassWrites()...)
	return f, env.runConsolidation(evalUserID)
}

// TestConsolidationEval_FullPassIsCleanAndComplete is the harness's headline. One
// pass over the whole corpus must satisfy four invariants at once, because they
// only hold TOGETHER — a pass can capture every planted fact and still be broken
// if it also captured the secret, and it can be perfectly clean by writing
// nothing.
//
//  4. PROVENANCE COMPLETENESS — every consolidated row carries
//     origin=consolidator + a class + source_session_id + source_run_id, and the
//     source ids are REAL seeded ones (a fabricated id is untraceable and also
//     defeats the erasure story that provenance exists to enable).
//     9a. THE SECRET IS NEVER WRITTEN — asserted against a verified premise: the
//     credential must be present in the transcript the pass actually read, or
//     the claim is vacuous.
//     9b. DISTRACTORS ARE NOT WRITTEN — transient chatter must not become durable.
//     9c. KNOWN-ABSENT FACTS ARE NOT PRESENT — catches a harness that passes by
//     storing everything, and a pass that fabricates.
//
// SCOPE OF THE CLAIM: 9a-9c are properties of the pass's OUTPUT. With a scripted
// provider they verify the pipeline stores exactly what it was told and nothing
// more (no injection, no transcript bleed, no over-capture through a reducer) —
// they do not verify a live model's judgement, which no offline harness can. The
// checker they run through is itself tested both ways in
// eval.TestCheckForbidden_DetectsAPlantedLeak.
//
// FAIL-BEFORE: dropping the origin stamp in the Memory tool's set path
// (execSet's consolidator branch) fails the provenance sweep; adding any
// forbidden marker to a write fails the CheckForbidden sweep (proved directly by
// the checker's own planted-leak test).
func TestConsolidationEval_FullPassIsCleanAndComplete(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	f, stream := runFullPass(t, env, "pend_eval_a", "pend_eval_b")

	// Premise: the secret really did reach the model's context, via a real
	// History tool result. Without this the not-written assertion below is
	// vacuous — the pass would have had nothing to leak.
	if !strings.Contains(stream, eval.FixtureSecret) {
		t.Fatal("the planted secret never appeared in the pass's transcript reads — the not-written assertion would be vacuous")
	}

	live := liveEntries(t, env)
	wantKeys := map[string]eval.PlantedFact{}
	for _, fact := range f.writtenFacts() {
		wantKeys[fact.Key] = fact
	}
	// Invariants 9a-9c FIRST, before the count check below can short-circuit on
	// t.Fatalf. A leak must report AS a leak: "the secret reached memory" is a
	// diagnosis, "5 keys, want 2" is a symptom, and the symptom is what a
	// reviewer would otherwise have to decode.
	for _, v := range eval.CheckForbidden(live, f.corpus) {
		t.Error(v)
	}
	if len(live) != len(wantKeys) {
		t.Fatalf("live keys after the pass = %v, want exactly %d distilled fact(s)", keyList(live), len(wantKeys))
	}

	// Invariant 4: provenance completeness, on EVERY row.
	seededSessions := map[string]bool{}
	seededRuns := map[string]bool{}
	for _, c := range f.chats {
		seededSessions[c.SessionID] = true
		seededRuns[c.RunID] = true
	}
	for _, e := range live {
		fact, ok := wantKeys[e.Key]
		if !ok {
			t.Errorf("unexpected consolidated key %q", e.Key)
			continue
		}
		if !strings.Contains(string(e.Value), fact.Marker) {
			t.Errorf("fact at %q = %s, want it to mention %q", e.Key, e.Value, fact.Marker)
		}
		prov, err := env.store.MemoryProvenanceGet(context.Background(), "", store.MemoryScopeUser, evalUserID, e.Key)
		if err != nil {
			t.Errorf("MemoryProvenanceGet(%s): %v", e.Key, err)
			continue
		}
		if prov.Origin != "consolidator" {
			t.Errorf("%s: origin = %q, want consolidator (server-stamped from the grant, never model-supplied)", e.Key, prov.Origin)
		}
		if prov.Class == "" {
			t.Errorf("%s: class is empty — an unclassed fact cannot be swept, aged, or explained", e.Key)
		}
		if !seededSessions[prov.SourceSessionID] {
			t.Errorf("%s: source_session_id = %q is not one of the seeded chats — provenance must lead back to a real transcript", e.Key, prov.SourceSessionID)
		}
		if !seededRuns[prov.SourceRunID] {
			t.Errorf("%s: source_run_id = %q is not one of the seeded runs", e.Key, prov.SourceRunID)
		}
	}

	// The queue the pass drained is acked, so a later pass cannot re-fold it.
	pending, err := env.store.MemoryPendingDrain(context.Background(), "", store.MemoryScopeUser, evalUserID, 50)
	if err != nil {
		t.Fatalf("MemoryPendingDrain: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d queue row(s) still un-drained after the pass acked them: %+v", len(pending), pending)
	}
}

// TestConsolidationEval_WatermarkAdvancesAndLeaseExcludesASecondPass is
// invariant 1.
//
// TWO HALVES, ONE MECHANISM. The composite watermark is what makes consolidation
// resumable: a cursor left behind re-reads work already folded in, a cursor
// pushed too far skips chats forever, and the timestamp alone cannot separate two
// chats that settled in the same instant. The lease is what stops two replicas
// firing the same target's pass in one tick.
//
// The second pass here is genuinely excluded, not politely asked to stop: it
// cannot acquire the lease (a different run id is a different owner), and every
// MUTATING consolidation op it tries is refused as a non-owner — cursor_advance
// by the store's ownership check, supersede and pending_ack by the tool's.
//
// WHAT THE LEASE DOES NOT GATE, deliberately: a plain `set` is not lease-checked,
// so a second pass CAN still write facts. That is tolerable because deterministic
// subject-derived keys mean a concurrent pass re-deriving the same fact overwrites
// the same row instead of racing a duplicate into existence (invariant 2 gates
// that overwrite). The three ops that get the lease are the ones with no such
// safety net: the forward-only watermark, and the archive/ack pair whose loss is
// unrecoverable.
//
// FAIL-BEFORE: removing the `leasedBy != owner` guard from MemoryCursorAdvance
// lets the second pass move the watermark; removing the tool's
// requireConsolidationLease lets it archive a fact and drain the queue — both
// verified by making those edits.
func TestConsolidationEval_WatermarkAdvancesAndLeaseExcludesASecondPass(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	f := seedEvalFixture(t, env)
	ctx := context.Background()

	// Pass A: a full pass that leases, advances, and then DOES NOT release — the
	// shape of a pass still in flight (or one whose process died holding the
	// lease, which is why the lease has a TTL at all).
	scriptsA := f.evalPassScript(nil, f.fullPassWrites()...)
	// Drop the release leg (second-to-last, before the report).
	scriptsA = append(scriptsA[:len(scriptsA)-2], scriptsA[len(scriptsA)-1])
	env.prov.scripts = scriptsA
	env.runConsolidation(evalUserID)

	afterA, err := env.store.MemoryCursorGet(ctx, "", store.MemoryScopeUser, evalUserID)
	if err != nil {
		t.Fatalf("MemoryCursorGet after pass A: %v", err)
	}
	if afterA.WatermarkSessionID != f.last.SessionID {
		t.Fatalf("watermark session = %q, want the newest consolidated chat %q", afterA.WatermarkSessionID, f.last.SessionID)
	}
	if !afterA.WatermarkCompletedAt.Equal(f.last.SettledAt) {
		t.Fatalf("watermark completed_at = %v, want the chat's settled instant %v", afterA.WatermarkCompletedAt, f.last.SettledAt)
	}
	if afterA.LeasedBy == "" {
		t.Fatal("pass A left no lease held; the exclusion assertion below would be vacuous")
	}
	liveAfterA := len(liveEntries(t, env))

	// A NEWER chat, settling after pass A's watermark, so pass B has a genuine
	// FORWARD target. This is load-bearing: an advance to an older chat is
	// already blocked by the store's monotonicity check, so a backwards attempt
	// would leave the watermark assertion below defended by the wrong layer and
	// the lease guard could be deleted without the test noticing.
	newerSess, err := env.store.CreateSession(ctx, "", "chat", evalUserID)
	if err != nil {
		t.Fatalf("seed the forward target session: %v", err)
	}
	newerRun, err := env.store.CreateRun(ctx, newerSess.ID, store.RunIdentity{AgentID: "chat-" + newerSess.ID, UserID: evalUserID})
	if err != nil {
		t.Fatalf("seed the forward target run: %v", err)
	}
	if err := env.store.FinishRun(ctx, newerRun.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatalf("settle the forward target: %v", err)
	}
	newerSettled, _, err := env.store.SessionSettledAt(ctx, "", newerSess.ID)
	if err != nil {
		t.Fatalf("forward target settled at: %v", err)
	}
	if !newerSettled.After(afterA.WatermarkCompletedAt) {
		t.Fatalf("the forward target settled at %v, not after the watermark %v — pass B's advance would be blocked by monotonicity rather than by the lease",
			newerSettled, afterA.WatermarkCompletedAt)
	}

	// A queued item for pass B to try to ack. Pass A only DRAINED (a read), so the
	// row is still un-drained and an unauthorized ack would be observable.
	enqueuePending(t, env, "pend_nonowner")

	// Pass B, while A's lease is live: it must fail to acquire, and must be refused
	// every mutation it would otherwise be entitled to make — the advance, the
	// archive, and the ack.
	env.prov.calls.Store(0)
	// A key pass A left LIVE, so an unauthorized archive shows up in the row count
	// below (re-superseding the already-archived one would be an idempotent no-op).
	victimKey := f.writtenFacts()[0].Key
	ackB, _ := json.Marshal(map[string]any{"op": "pending_ack", "scope": "user", "ids": []string{"pend_nonowner"}})
	env.prov.scripts = [][]providers.Event{
		toolCall("tu_lease_b", "Memory", `{"op":"cursor_lease","scope":"user","lease_ttl_ms":600000}`),
		toolCall("tu_advance_b", "Memory", fmt.Sprintf(`{"op":"cursor_advance","scope":"user","completed_at":%q,"session_id":%q}`,
			newerSettled.UTC().Format(time.RFC3339Nano), newerSess.ID)),
		toolCall("tu_sup_b", "Memory", fmt.Sprintf(`{"op":"supersede","scope":"user","key":%q}`, victimKey)),
		toolCall("tu_ack_b", "Memory", string(ackB)),
		finalText("lease not acquired; standing down"),
	}
	streamB := env.runConsolidation(evalUserID)

	// The tool result is a JSON string inside the SSE data frame, so the payload's
	// own quotes arrive escaped.
	if !strings.Contains(streamB, `\"acquired\":false`) {
		t.Errorf("pass B's cursor_lease did not report acquired=false; stream:\n%s", streamB)
	}
	// One refusal per mutating op, so a single one going missing is caught: the
	// store's advance check and the tool's own gate on supersede / pending_ack.
	if n := strings.Count(streamB, "not lease owner"); n < 3 {
		t.Errorf("pass B's mutations produced %d non-owner refusal(s), want 3 (advance, supersede, pending_ack)", n)
	}
	// And the ack did not take: the queued item is still there for the owning pass.
	stillQueued, err := env.store.MemoryPendingDrain(ctx, "", store.MemoryScopeUser, evalUserID, 50)
	if err != nil {
		t.Fatalf("MemoryPendingDrain after pass B: %v", err)
	}
	if len(stillQueued) != 1 {
		t.Errorf("un-drained queue rows after the excluded pass = %d, want 1 — a non-owner ack loses those turns permanently", len(stillQueued))
	}

	afterB, err := env.store.MemoryCursorGet(ctx, "", store.MemoryScopeUser, evalUserID)
	if err != nil {
		t.Fatalf("MemoryCursorGet after pass B: %v", err)
	}
	if afterB.WatermarkSessionID != afterA.WatermarkSessionID || !afterB.WatermarkCompletedAt.Equal(afterA.WatermarkCompletedAt) {
		t.Errorf("pass B moved the watermark from (%v, %s) to (%v, %s) despite not owning the lease",
			afterA.WatermarkCompletedAt, afterA.WatermarkSessionID, afterB.WatermarkCompletedAt, afterB.WatermarkSessionID)
	}
	if afterB.LeasedBy != afterA.LeasedBy {
		t.Errorf("the lease owner changed from %q to %q — pass B stole a live lease", afterA.LeasedBy, afterB.LeasedBy)
	}
	if got := len(liveEntries(t, env)); got != liveAfterA {
		t.Errorf("live memory rows changed from %d to %d during the excluded pass", liveAfterA, got)
	}
}

// TestConsolidationEval_ReConsolidationAddsNoDuplicates is invariant 2, and the
// property that makes a failed pass safe to retry at all.
//
// It holds because the skill mints DETERMINISTIC subject-derived keys: the same
// fact re-derived lands on its own row. A pipeline that appended what it
// extracted would grow a near-duplicate on every pass, and no dedup threshold
// saves it — the duplicates are separate rows with separate keys, so they all
// survive retrieval and split the agent's attention across three phrasings of one
// fact.
//
// The re-run deliberately REWORDS each fact, which is what a second model call
// realistically produces. A harness that replayed byte-identical writes would
// pass even on a broken append-only pipeline.
//
// FAIL-BEFORE: appending a per-pass suffix to the derived key in the scripted
// writes (the shape of a pipeline that mints a fresh key per pass) doubles the
// live row count — verified by making that edit.
func TestConsolidationEval_ReConsolidationAddsNoDuplicates(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	f, _ := runFullPass(t, env)

	afterFirst := keyList(liveEntries(t, env))
	if len(afterFirst) == 0 {
		t.Fatalf("pass 1 wrote nothing; there is nothing to re-consolidate")
	}

	// Pass 2 over the same chats, same derived keys, reworded facts.
	var writes []string
	for _, fact := range f.writtenFacts() {
		writes = append(writes, setOp(fact.Key, fact.Text+" (restated on a later pass)", fact.Class,
			f.chats[fact.Chat].SessionID, f.chats[fact.Chat].RunID))
	}
	env.prov.calls.Store(0)
	env.prov.scripts = f.evalPassScript(nil, writes...)
	env.runConsolidation(evalUserID)

	afterSecond := keyList(liveEntries(t, env))
	if len(afterSecond) != len(afterFirst) {
		t.Errorf("live keys grew from %d to %d across a re-consolidation: %v — a re-run must not duplicate facts",
			len(afterFirst), len(afterSecond), afterSecond)
	}
	// And the rows were overwritten in place, not left stale.
	for _, e := range liveEntries(t, env) {
		if !strings.Contains(string(e.Value), "restated on a later pass") {
			t.Errorf("row %q was not overwritten by the second pass: %s", e.Key, e.Value)
		}
	}
	// Nothing forbidden crept in on the second pass either.
	for _, v := range eval.CheckForbidden(liveEntries(t, env), f.corpus) {
		t.Error(v)
	}
}

// TestConsolidationEval_SupersededFactIsArchivedNotDeleted is invariant 3.
//
// A contradicted fact must go SOFT: invisible to every read that returns
// content, but the row retained. A hard delete would destroy the audit trail the
// provenance columns exist to preserve, and would stop a later pass from reviving
// the fact if it turns out to be true again.
//
// WHICH OBSERVABLE DISCRIMINATES ARCHIVE FROM DELETE, and why it is the whole
// test. Every CONTENT read filters `superseded_at IS NULL` (MemoryGet /
// MemoryList / MemoryProvenanceGet), so all of them behave identically whether
// the tool archived the row or destroyed it — an assertion built on them is
// vacuous. MemoryListScopeIDs is the one Store read that does NOT filter: the
// operator scope-size summary still COUNTS an archived row. So that count is the
// load-bearing assertion here, taken immediately after the pass and before
// anything else touches the key.
//
// FAIL-BEFORE: pointing the Memory tool's supersede at the backend's hard Delete
// drops the summary's key_count to the live count — verified by making that edit.
func TestConsolidationEval_SupersededFactIsArchivedNotDeleted(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	f, _ := runFullPass(t, env)
	ctx := context.Background()
	staleKey := f.corpus.Supersede.StaleKey

	// Invisible to point reads...
	if _, err := env.store.MemoryGet(ctx, "", store.MemoryScopeUser, evalUserID, staleKey); err == nil {
		t.Errorf("superseded fact %q is still readable via MemoryGet", staleKey)
	}
	// ...and to listings.
	live := liveEntries(t, env)
	for _, e := range live {
		if e.Key == staleKey {
			t.Errorf("superseded key %q is still listed among live keys %v", staleKey, keyList(live))
		}
	}
	// The correction that replaced it landed.
	if _, err := env.store.MemoryGet(ctx, "", store.MemoryScopeUser, evalUserID, f.corpus.Supersede.NewKey); err != nil {
		t.Errorf("the correction %q did not land: %v", f.corpus.Supersede.NewKey, err)
	}

	// RETENTION, on the one read that can tell an archive from a delete: the
	// operator scope-size summary does not filter `superseded_at`, so it must still
	// count the row the pass archived alongside the live ones.
	sums, err := env.store.MemoryListScopeIDs(ctx, "", store.MemoryScopeUser)
	if err != nil {
		t.Fatalf("MemoryListScopeIDs: %v", err)
	}
	counted, found := 0, false
	for _, sum := range sums {
		if sum.ScopeID == evalUserID {
			counted, found = sum.KeyCount, true
		}
	}
	if !found {
		t.Fatalf("no scope summary for %q — the retention assertion would be vacuous", evalUserID)
	}
	if want := len(live) + 1; counted != want {
		t.Errorf("scope key_count = %d, want %d (%d live + the archived %q) — the tool's supersede did not retain the row",
			counted, want, len(live), staleKey)
	}

	// A SEPARATE property, not retention: a re-write onto a superseded key is not
	// refused, so a later pass can revive a fact that turns out to be true again.
	// MemorySetProvenance is an upsert that INSERTs a missing key, so this holds
	// whether the row survived or not — it is asserted for the revive path itself,
	// and the retention claim rests on the summary count above.
	revived, _ := json.Marshal("Back on tabs after all.")
	if err := env.store.MemorySetProvenance(ctx, "", store.MemoryScopeUser, evalUserID, staleKey, revived, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "correction", SourceSessionID: f.last.SessionID, SourceRunID: f.last.RunID}); err != nil {
		t.Fatalf("revive %q: %v", staleKey, err)
	}
	if _, err := env.store.MemoryGet(ctx, "", store.MemoryScopeUser, evalUserID, staleKey); err != nil {
		t.Errorf("a re-write of the archived key did not revive it: %v", err)
	}

	// And ERASURE reaches an archived row: archive again (at the store layer — this
	// leg is about MemoryDeleteScope's reach, not about what the tool did) and the
	// reclaim count must exceed the live count.
	if err := env.store.MemorySupersede(ctx, "", store.MemoryScopeUser, evalUserID, staleKey); err != nil {
		t.Fatalf("re-supersede: %v", err)
	}
	liveCount := len(liveEntries(t, env))
	reclaimed, err := env.store.MemoryDeleteScope(ctx, "", store.MemoryScopeUser, evalUserID)
	if err != nil {
		t.Fatalf("MemoryDeleteScope: %v", err)
	}
	if reclaimed <= liveCount {
		t.Errorf("scope reclaim removed %d row(s) but only %d were live — the archived row was not retained in the table",
			reclaimed, liveCount)
	}
}

// TestConsolidationEval_QuotaRefusedWriteFailsLoudly is invariant 8.
//
// A consolidation write that cannot land must FAIL VISIBLY. This is the harness's
// most important negative: consolidation runs unattended on a schedule, its
// output is a side effect rather than a returned value, and nobody reads its
// report. A quota refusal that came back as a silent success would drop the fact
// AND advance the watermark past the chat it came from — losing it permanently,
// with no error anywhere. The refusal is what makes the loss recoverable.
//
// The quota is the runtime's own per-scope byte cap (memory_quota_bytes /
// LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES), enforced server-side at write time — not a
// harness stub.
//
// FAIL-BEFORE: making checkQuota return nil (or dropping the error return from
// execSet's quota check) lands the fact and the refusal disappears from the
// stream — verified by making that edit.
func TestConsolidationEval_QuotaRefusedWriteFailsLoudly(t *testing.T) {
	// A cap far below the fixture's fact text, so the very first consolidated
	// write is refused.
	env := newConsolidationEnv(t, nil, func(m *builtin.Memory) { m.DefaultQuotaBytes = 24 })
	f := seedEvalFixture(t, env)

	// The stale fact seeded directly at the store layer bypasses the tool's
	// quota check, so clear it: the assertion must be about the pass's OWN write
	// being refused, not about a scope that was already over budget.
	if _, err := env.store.MemoryDelete(context.Background(), "", store.MemoryScopeUser, evalUserID, f.corpus.Supersede.StaleKey); err != nil {
		t.Fatalf("clear the pre-seeded fact: %v", err)
	}

	fact := f.writtenFacts()[0]
	env.prov.scripts = f.evalPassScript(nil,
		setOp(fact.Key, fact.Text, fact.Class, f.chats[fact.Chat].SessionID, f.chats[fact.Chat].RunID))
	stream := env.runConsolidation(evalUserID)

	if !strings.Contains(stream, "quota") {
		t.Errorf("a quota-refused consolidation write did not surface a refusal; stream:\n%s", stream)
	}
	if _, err := env.store.MemoryGet(context.Background(), "", store.MemoryScopeUser, evalUserID, fact.Key); err == nil {
		t.Errorf("the refused fact %q landed anyway — a refusal that still writes is worse than either outcome", fact.Key)
	}
}

func keyList(entries []store.MemoryEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}
