package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC CI P3 — the assistant half, and the archive.

// seedRunWithTurns writes a session whose transcript holds one user turn and an
// assistant reply persisted the way the loop persists it: ONE ROW PER STREAMED DELTA.
// That shape is the point — anything reading raw event rows sees fragments.
func seedRunWithTurns(t *testing.T, st store.Store, tenant, user string) (string, string) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "chat", user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := run.ID
	seg, _ := json.Marshal([]map[string]any{{"role": "user",
		"content": []map[string]any{{"type": "trusted-text", "text": "did we move the database"}}}})
	if err := st.AppendEvent(ctx, runID, "user_input", seg); err != nil {
		t.Fatalf("append user_input: %v", err)
	}
	for _, frag := range []string{"Yes — ", "it moved ", "to Postgres 18."} {
		b, _ := json.Marshal(map[string]string{"text": frag})
		if err := st.AppendEvent(ctx, runID, "text", b); err != nil {
			t.Fatalf("append text: %v", err)
		}
	}
	b, _ := json.Marshal(map[string]any{"stop_reason": "end_turn"})
	if err := st.AppendEvent(ctx, runID, "done", b); err != nil {
		t.Fatalf("append done: %v", err)
	}
	return sess.ID, runID
}

func traceKeys(t *testing.T, st store.Store, tenant, user string) []string {
	t.Helper()
	rows, _, err := st.MemoryList(context.Background(), tenant, store.MemoryScopeUser, user,
		store.TraceTurnKeyPrefix, 100)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}

// TestIndexAssistantTurns_FlushesDeltasIntoOneTurn.
//
// Assistant text is persisted one row per streamed delta. Indexing raw rows would
// file "Yes — ", "it moved " and "to Postgres 18." as three turns with no speaker,
// which is the shape that produced a live pass's empty extractions. The boundaries
// come from the SAME helper the transcript renders with.
func TestIndexAssistantTurns_FlushesDeltasIntoOneTurn(t *testing.T) {
	s, st := traceServer(t, true)
	s.cfg().Env.MemoryTraceAssistantTurns = true
	sessID, runID := seedRunWithTurns(t, st, "acme", "u1")

	s.indexAssistantTurns(context.Background(), runID, runStateMeta{
		RunID: runID, UserID: "u1", TenantID: "acme"})

	keys := traceKeys(t, st, "acme", "u1")
	if len(keys) != 1 {
		t.Fatalf("indexed %d rows for one assistant turn, want 1: %v", len(keys), keys)
	}
	if !strings.Contains(keys[0], sessID) {
		t.Errorf("key %q does not name its session", keys[0])
	}
	e, err := st.MemoryGet(context.Background(), "acme", store.MemoryScopeUser, "u1", keys[0])
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var turn traceTurnValue
	if err := json.Unmarshal(e.Value, &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if turn.Text != "Yes — it moved to Postgres 18." {
		t.Errorf("the deltas were not flushed into one turn: %q", turn.Text)
	}
	if turn.Speaker != "assistant" {
		t.Errorf("speaker = %q, want assistant", turn.Speaker)
	}
}

// TestIndexAssistantTurns_OffEvenWhenTheIndexIsOn. Two different trades: assistant
// text is the bulk of the volume and the most redundant with the fact layer, so a
// deployment that wants its own words findable is not made to pay for the model's.
func TestIndexAssistantTurns_OffEvenWhenTheIndexIsOn(t *testing.T) {
	s, st := traceServer(t, true) // trace index ON, assistant flag untouched
	_, runID := seedRunWithTurns(t, st, "acme", "u1")
	s.indexAssistantTurns(context.Background(), runID, runStateMeta{
		RunID: runID, UserID: "u1", TenantID: "acme"})
	if keys := traceKeys(t, st, "acme", "u1"); len(keys) != 0 {
		t.Errorf("assistant turns were indexed without their own flag: %v", keys)
	}
}

// TestIndexAssistantTurns_IsIdempotent. finishRun can run twice — a resumed run
// completes again — and a deterministic key makes the second pass overwrite rather
// than file the same words a second time.
func TestIndexAssistantTurns_IsIdempotent(t *testing.T) {
	s, st := traceServer(t, true)
	s.cfg().Env.MemoryTraceAssistantTurns = true
	_, runID := seedRunWithTurns(t, st, "acme", "u1")
	meta := runStateMeta{RunID: runID, UserID: "u1", TenantID: "acme"}
	s.indexAssistantTurns(context.Background(), runID, meta)
	s.indexAssistantTurns(context.Background(), runID, meta)
	if keys := traceKeys(t, st, "acme", "u1"); len(keys) != 1 {
		t.Errorf("a second completion filed the turn again: %v", keys)
	}
}

// TestIndexAssistantTurns_RedactsToo — the admission criterion applies to both halves.
// An assistant reply can quote a credential a user pasted.
func TestIndexAssistantTurns_RedactsToo(t *testing.T) {
	s, st := traceServer(t, true)
	s.cfg().Env.MemoryTraceAssistantTurns = true
	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "acme", "chat", "u1")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{})
	runID := run.ID
	b, _ := json.Marshal(map[string]string{"text": "I used sk-live-supersecret to deploy"})
	_ = st.AppendEvent(ctx, runID, "text", b)
	d, _ := json.Marshal(map[string]any{"stop_reason": "end_turn"})
	_ = st.AppendEvent(ctx, runID, "done", d)

	s.indexAssistantTurns(ctx, runID, runStateMeta{RunID: runID, UserID: "u1", TenantID: "acme"})
	keys := traceKeys(t, st, "acme", "u1")
	if len(keys) == 0 {
		t.Fatal("nothing indexed")
	}
	e, _ := st.MemoryGet(ctx, "acme", store.MemoryScopeUser, "u1", keys[0])
	if strings.Contains(string(e.Value), "sk-live-supersecret") {
		t.Errorf("an assistant turn was indexed with the credential verbatim: %s", e.Value)
	}
}

// backfillReq drives the endpoint and decodes its report.
func backfillReq(t *testing.T, s *Server, query string) (int, traceBackfillReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/backfill_traces?"+query, nil)
	s.handleMemoryBackfillTraces(rec, req)
	var rep traceBackfillReport
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	return rec.Code, rep
}

// TestBackfillTraces_DryRunByDefaultAndCountsWhatItWouldDo. This one EMBEDS, so a
// mistyped command costs an operator their embedder rather than a wrong answer —
// the same reason every other sweep here defaults to a preview.
func TestBackfillTraces_DryRunByDefaultAndCountsWhatItWouldDo(t *testing.T) {
	s, st := traceServer(t, true)
	seedRunWithTurns(t, st, "acme", "u1")

	code, rep := backfillReq(t, s, "tenant=acme&user_id=u1")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !rep.DryRun {
		t.Error("the backfill ran for real without being asked to")
	}
	if rep.TurnsFound == 0 {
		t.Error("the dry run found no turns in a transcript that has one")
	}
	if rep.Indexed != 0 {
		t.Errorf("a dry run wrote %d rows", rep.Indexed)
	}
	if keys := traceKeys(t, st, "acme", "u1"); len(keys) != 0 {
		t.Errorf("the dry run left rows behind: %v", keys)
	}
}

// TestBackfillTraces_IndexesAndIsReRunnable. An operator runs it, sees what it did,
// and runs it again — so a second pass must report the work as already done rather
// than filing a second copy of the archive.
func TestBackfillTraces_IndexesAndIsReRunnable(t *testing.T) {
	s, st := traceServer(t, true)
	seedRunWithTurns(t, st, "acme", "u1")

	_, rep := backfillReq(t, s, "tenant=acme&user_id=u1&dry_run=false")
	if rep.Indexed == 0 {
		t.Fatalf("the backfill indexed nothing: %+v", rep)
	}
	first := traceKeys(t, st, "acme", "u1")

	_, rep2 := backfillReq(t, s, "tenant=acme&user_id=u1&dry_run=false")
	if rep2.Indexed != 0 {
		t.Errorf("a re-run indexed %d rows again", rep2.Indexed)
	}
	if rep2.Skipped == 0 {
		t.Error("a re-run reported nothing already-indexed, so an operator cannot tell " +
			"a completed archive from an empty one")
	}
	if got := traceKeys(t, st, "acme", "u1"); len(got) != len(first) {
		t.Errorf("the archive grew on a re-run: %d then %d", len(first), len(got))
	}
}

// TestBackfillTraces_AssistantIsSeparatelyOptIn — independently of the live flag. An
// operator running the index on user turns only may still want one archive pass with
// both, and the reverse is just as reasonable.
func TestBackfillTraces_AssistantIsSeparatelyOptIn(t *testing.T) {
	s, st := traceServer(t, true)
	seedRunWithTurns(t, st, "acme", "u1")

	_, userOnly := backfillReq(t, s, "tenant=acme&user_id=u1&dry_run=false")
	if userOnly.Indexed == 0 {
		t.Fatal("the user-only pass indexed nothing")
	}
	// ASSERT ON THE SPEAKER, not on the counts. Comparing two totals says the second
	// pass wrote MORE rows, which a second user turn would satisfy just as well — the
	// question is whether the assistant's words are in there.
	if speakers := indexedSpeakers(t, st, "acme", "u1"); speakers["assistant"] != 0 {
		t.Errorf("the user-only pass indexed assistant turns: %v", speakers)
	}

	_, withAsst := backfillReq(t, s, "tenant=acme&user_id=u1&dry_run=false&assistant=1")
	if withAsst.Indexed == 0 {
		t.Fatal("assistant=1 indexed nothing beyond the user-only pass")
	}
	speakers := indexedSpeakers(t, st, "acme", "u1")
	if speakers["assistant"] == 0 {
		t.Error("assistant=1 wrote rows but none of them is an assistant turn")
	}
	if speakers["user"] == 0 {
		t.Error("the assistant pass lost the user turns the first pass had indexed")
	}
}

// indexedSpeakers counts the indexed rows by who said them.
func indexedSpeakers(t *testing.T, st store.Store, tenant, user string) map[string]int {
	t.Helper()
	rows, _, err := st.MemoryList(context.Background(), tenant, store.MemoryScopeUser, user,
		store.TraceTurnKeyPrefix, 100)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	out := map[string]int{}
	for _, r := range rows {
		var turn traceTurnValue
		if json.Unmarshal(r.Value, &turn) == nil {
			out[turn.Speaker]++
		}
	}
	return out
}

// TestBackfillTraces_RefusesWithoutTheFeatureOrAUser. Backfilling into an index nobody
// reads writes a user's whole history into a second place for no benefit — and the
// posture argument that made the index opt-in applies with more force to a bulk write.
func TestBackfillTraces_RefusesWithoutTheFeatureOrAUser(t *testing.T) {
	off, _ := traceServer(t, false)
	if code, _ := backfillReq(t, off, "tenant=acme&user_id=u1"); code != http.StatusConflict {
		t.Errorf("backfill with the index disabled = %d, want 409", code)
	}
	on, _ := traceServer(t, true)
	if code, _ := backfillReq(t, on, "tenant=acme"); code != http.StatusBadRequest {
		t.Errorf("backfill with no user = %d, want 400 — a turn is indexed under the "+
			"person who typed it, and the blast radius of a wrong default is every user", code)
	}
}

// TestBackfillTraces_SaysWhenItStoppedAtTheLimit. The limit caps chats EXAMINED, so a
// truncated run has to say so — an operator reading "complete" against a partial sweep
// would never run it again.
func TestBackfillTraces_SaysWhenItStoppedAtTheLimit(t *testing.T) {
	s, st := traceServer(t, true)
	seedRunWithTurns(t, st, "acme", "u1")
	seedRunWithTurns(t, st, "acme", "u1")

	_, rep := backfillReq(t, s, "tenant=acme&user_id=u1&limit=1")
	if rep.StopReason != "limit" {
		t.Errorf("stop_reason = %q, want limit — the sweep examined only part of the archive", rep.StopReason)
	}
	_, full := backfillReq(t, s, "tenant=acme&user_id=u1&limit=50")
	if full.StopReason != "complete" {
		t.Errorf("stop_reason = %q on a sweep that saw everything", full.StopReason)
	}
}
