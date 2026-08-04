package eval

// The consolidation eval harness — RETRIEVAL / ERASURE / DEDUP half (RFC BL P2).
//
// These are the invariants that need a real memory backend over a real store but
// NOT a real agent run. The pipeline half (watermark, lease exclusion,
// idempotence, supersession, provenance, quota, forbidden-fixture sweep) drives a
// real POST /v1/runs and lives in internal/api/http/consolidation_eval_test.go —
// see that file's header for why the harness is split.
//
// Everything here is offline and hermetic: in-memory SQLite, a stub embedder, no
// network, no API key, seconds not minutes. `make memory-eval-mock` runs this
// plus the pipeline half; plain `go test ./...` runs it too, deliberately — a
// gate that only fires behind a make target is not a gate.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

const evalUser = "alice"

// TestConsolidationEval_FixtureSettlesInDeclaredOrder pins the corpus's own
// premise: the seeded chats must come back from the store's ascending scan in
// the order the fixture declares, with the contradiction chat LAST. Every
// supersession assertion downstream depends on it, and settle order comes from
// FinishRun's wall clock — which the fixture cannot set.
func TestConsolidationEval_FixtureSettlesInDeclaredOrder(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	corpus := ConsolidationFixture()
	seeded, err := SeedConsolidationChats(context.Background(), st, "", evalUser, corpus)
	if err != nil {
		t.Fatalf("SeedConsolidationChats: %v", err)
	}
	if len(seeded) != len(corpus.Chats) {
		t.Fatalf("seeded %d chats, want %d", len(seeded), len(corpus.Chats))
	}
	// The correction chat must settle strictly after the fact it corrects.
	prefs, ok := seeded[ChatPrefs]
	if !ok {
		t.Fatalf("chat %q not seeded", ChatPrefs)
	}
	swtch, ok := seeded[ChatSwitch]
	if !ok {
		t.Fatalf("chat %q not seeded", ChatSwitch)
	}
	if !swtch.SettledAt.After(prefs.SettledAt) {
		t.Errorf("the correction chat settled at %v, not after the fact it corrects (%v) — the supersession fixture would test backwards",
			swtch.SettledAt, prefs.SettledAt)
	}
	// The secret must genuinely be reachable in the transcript, or "the secret is
	// never written" is vacuous: the pass would have had nothing to leak.
	var sawSecret bool
	for _, chat := range corpus.Chats {
		for _, turn := range chat.Turns {
			if strings.Contains(turn, FixtureSecret) {
				sawSecret = true
			}
		}
	}
	if !sawSecret {
		t.Error("no fixture transcript carries the planted secret — the not-written assertion would be vacuous")
	}
}

// TestConsolidationEval_TotalUserErasureRemovesEverything is invariant 5, and
// carries invariant 3's retention half with it.
//
// Full erasure is a stated project decision: provenance exists so a user's
// derived memory can be found and removed, so a delete that left the
// consolidation cursor or the pending queue behind would leave a resumable trail
// of a user who asked to be forgotten — and a watermark that survives erasure
// also makes the NEXT pass skip their (now absent) history silently.
//
// The returned row count is the load-bearing observable. No Store read path
// returns a superseded row's key or value (MemoryGet / MemoryList /
// MemoryProvenanceGet all filter `superseded_at IS NULL`) — only counts see it:
// the operator scope-size summary (MemoryListScopeIDs) and this reclaim count. So
// the count is how the harness proves BOTH that the archive was retained before
// erasure and that erasure reached it.
//
// FAIL-BEFORE: adding `AND superseded_at IS NULL` to MemoryDeleteScope's memory
// DELETE drops the count to 2 (the archived row survives erasure); removing its
// memory_pending or memory_cursors DELETE leaves the queue / watermark behind.
// Both were verified by making those edits.
func TestConsolidationEval_TotalUserErasureRemovesEverything(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	const scope = store.MemoryScopeUser

	corpus := ConsolidationFixture()
	seeded, err := SeedConsolidationChats(ctx, st, "", evalUser, corpus)
	if err != nil {
		t.Fatalf("SeedConsolidationChats: %v", err)
	}

	// Consolidated facts, with provenance, exactly as a pass writes them.
	for _, f := range corpus.Facts {
		val, _ := json.Marshal(f.Text)
		if err := st.MemorySetProvenance(ctx, "", scope, evalUser, f.Key, val, 0, store.MemoryProvenance{
			Origin:          "consolidator",
			Class:           f.Class,
			SourceSessionID: seeded[f.Chat].SessionID,
			SourceRunID:     seeded[f.Chat].RunID,
		}); err != nil {
			t.Fatalf("seed fact %q: %v", f.Key, err)
		}
	}
	// One archived row: the stale fact a later pass superseded.
	if err := st.MemorySupersede(ctx, "", scope, evalUser, corpus.Supersede.StaleKey); err != nil {
		t.Fatalf("supersede %q: %v", corpus.Supersede.StaleKey, err)
	}
	// Two un-drained queue rows from Memory.add.
	for i, id := range []string{"pend_eval_1", "pend_eval_2"} {
		if err := st.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
			ID:        id,
			Scope:     scope,
			ScopeID:   evalUser,
			Payload:   json.RawMessage(`{"messages":[{"role":"user","content":"queued"}]}`),
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	// And an advanced, still-leased cursor.
	if _, acquired, err := st.MemoryCursorLease(ctx, "", scope, evalUser, "run_eval", time.Now().UTC(), time.Minute); err != nil || !acquired {
		t.Fatalf("lease: acquired=%v err=%v", acquired, err)
	}
	prefs := seeded[ChatPrefs]
	if err := st.MemoryCursorAdvance(ctx, "", scope, evalUser, "run_eval", prefs.SettledAt, prefs.SessionID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// Pre-state: the archived row is invisible to reads but still a row.
	liveBefore, _, err := st.MemoryList(ctx, "", scope, evalUser, "", 100)
	if err != nil {
		t.Fatalf("MemoryList: %v", err)
	}
	wantLive := len(corpus.Facts) - 1
	if len(liveBefore) != wantLive {
		t.Fatalf("live rows before erasure = %d, want %d (one fact archived)", len(liveBefore), wantLive)
	}

	deleted, err := st.MemoryDeleteScope(ctx, "", scope, evalUser)
	if err != nil {
		t.Fatalf("MemoryDeleteScope: %v", err)
	}
	if deleted != len(corpus.Facts) {
		t.Errorf("MemoryDeleteScope removed %d row(s), want %d — erasure must reach the ARCHIVED row too, not just the %d live ones",
			deleted, len(corpus.Facts), wantLive)
	}

	if rows, _, err := st.MemoryList(ctx, "", scope, evalUser, "", 100); err != nil {
		t.Fatalf("MemoryList after erasure: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("%d memory row(s) survived erasure: %+v", len(rows), rows)
	}
	pending, err := st.MemoryPendingDrain(ctx, "", scope, evalUser, 50)
	if err != nil {
		t.Fatalf("MemoryPendingDrain after erasure: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d pending queue row(s) survived erasure — a resumable trail of a user who asked to be forgotten", len(pending))
	}
	cursor, err := st.MemoryCursorGet(ctx, "", scope, evalUser)
	if err != nil {
		t.Fatalf("MemoryCursorGet after erasure: %v", err)
	}
	if !cursor.WatermarkCompletedAt.IsZero() || cursor.WatermarkSessionID != "" || cursor.LeasedBy != "" {
		t.Errorf("the consolidation cursor survived erasure: %+v", cursor)
	}
}

// TestConsolidationEval_HybridRetrievalSurfacesPlantedFact is invariant 6: a
// consolidated fact must come back for a NATURAL-LANGUAGE query, through the
// hybrid path, with both legs contributing.
//
// The fixture is built so a single-leg retrieval CANNOT pass it: the planted fact
// is vector-ORTHOGONAL to the query (cosine 0, so the vector leg ranks it last of
// six) while its text carries the query's keywords (so the keyword leg ranks it
// first). Only RRF fusion promotes it into a top_k of 3. This is the shape that
// matters in production — a fact whose wording matches but whose embedding sits
// far from the query is exactly what a pure-vector store loses.
//
// FAIL-BEFORE: forcing `hybrid := false` in inprocess.Search (or dropping the
// full-text leg from the FuseRRF call) leaves the planted fact at vector rank 6,
// trimmed away by top_k=3 — verified by making that edit.
func TestConsolidationEval_HybridRetrievalSurfacesPlantedFact(t *testing.T) {
	fts, closeStore, err := newConsolidationStore()
	if err != nil {
		t.Fatalf("newConsolidationStore: %v", err)
	}
	defer closeStore()

	const dim = 8
	emb := NewFixedVectorEmbedder(dim)
	ctx := context.Background()
	const scope = store.MemoryScopeUser

	// The query, and the planted fact whose TEXT answers it.
	const queryText = "deploys staging before production"
	corpus := ConsolidationFixture()
	planted, ok := corpus.FactByKey("memory/constraint/deploy-staging-first")
	if !ok {
		t.Fatal("fixture lost the deploy constraint")
	}
	// Query on axis 0; the planted fact orthogonal on axis 2 → cosine 0.
	if err := emb.Register(queryText, UnitAxis(dim, 0)); err != nil {
		t.Fatal(err)
	}
	if err := emb.Register(planted.Text, UnitAxis(dim, 2)); err != nil {
		t.Fatal(err)
	}

	// Five fillers that OUT-RANK the planted fact on cosine and share no query
	// token, so the keyword leg cannot reach them.
	fillers := []struct {
		key  string
		text string
		cos  float64
	}{
		{"memory/fact/filler-a", "Owns a bicycle painted turquoise.", 0.90},
		{"memory/fact/filler-b", "Keeps houseplants on the windowsill.", 0.80},
		{"memory/fact/filler-c", "Drinks jasmine tea in the afternoon.", 0.70},
		{"memory/fact/filler-d", "Collects vinyl records from Iceland.", 0.60},
		{"memory/fact/filler-e", "Walks the dog at sunrise.", 0.50},
	}

	backend := inprocess.New(fts, emb)
	for _, f := range fillers {
		if err := emb.Register(f.text, UnitTilt(dim, 0, 1, f.cos)); err != nil {
			t.Fatal(err)
		}
		val, _ := json.Marshal(f.text)
		if _, err := backend.Set(ctx, scope, evalUser, f.key, val, memory.SetOptions{Embed: true, EmbedText: f.text}); err != nil {
			t.Fatalf("set %s: %v", f.key, err)
		}
	}
	val, _ := json.Marshal(planted.Text)
	if _, err := backend.Set(ctx, scope, evalUser, planted.Key, val, memory.SetOptions{Embed: true, EmbedText: planted.Text}); err != nil {
		t.Fatalf("set planted: %v", err)
	}

	// Premise check: the keyword leg must actually reach the planted fact and
	// nothing else, or the fusion assertion would pass for the wrong reason.
	lex, err := fts.MemoryFullTextSearch(ctx, "", scope, evalUser, store.MemorySearchFilter{}, queryText, 10)
	if err != nil {
		t.Fatalf("keyword leg: %v", err)
	}
	if len(lex) != 1 || lex[0].Key != planted.Key {
		t.Fatalf("keyword leg returned %d row(s) (%+v), want exactly the planted fact — the fixture's fillers must share no query token", len(lex), lex)
	}

	const topK = 3
	res, err := backend.Search(ctx, scope, evalUser,
		memory.SearchQuery{QueryText: queryText, TopK: topK},
		memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) > topK {
		t.Fatalf("Search returned %d entries, want <= top_k=%d", len(res.Entries), topK)
	}
	var found bool
	keys := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		keys = append(keys, e.Key)
		if e.Key == planted.Key {
			found = true
			if !strings.Contains(string(e.Value), planted.Marker) {
				t.Errorf("planted fact came back as %q, want it to mention %q", e.Value, planted.Marker)
			}
		}
	}
	if !found {
		t.Errorf("the planted fact %q did not surface in the top %d (%v) — a lexical-only match that the vector leg ranks deep must still be promoted by fusion",
			planted.Key, topK, keys)
	}
}

// TestConsolidationEval_DedupBandsMergeNearIdenticalKeepRelated is invariant 7:
// the dedup threshold must behave as a BAND, not a blanket. A near-identical
// restatement (cosine >= 0.95) collapses into the row it duplicates; a
// related-but-distinct fact (0.85–0.95) stays its own row.
//
// Both halves matter and fail in opposite directions. A threshold set too low
// silently DESTROYS distinct facts — "deploys go through staging" and "deploys
// need two approvals" are related, not the same, and collapsing them loses one
// permanently from every future recall. A threshold too high lets three phrasings
// of one fact burn the agent's context.
//
// The cosines are pinned exactly by FixedVectorEmbedder rather than derived from
// tokenization, and the test re-derives them from the embedder first so the
// fixture's premise cannot rot silently.
//
// FAIL-BEFORE: flipping DedupResults' `>= threshold` to `> 1` drops nothing
// (DedupDropped == 0, both near-identical rows survive); hardcoding the
// comparison to `>= 0.85` collapses the related pair too. Both were verified by
// making those edits.
func TestConsolidationEval_DedupBandsMergeNearIdenticalKeepRelated(t *testing.T) {
	fts, closeStore, err := newConsolidationStore()
	if err != nil {
		t.Fatalf("newConsolidationStore: %v", err)
	}
	defer closeStore()

	const (
		dim = 8
		// The band under test. nearCos sits above it (must collapse); relatedCos
		// sits inside 0.85–0.95 (must not).
		threshold  = 0.95
		nearCos    = 0.975
		relatedCos = 0.90
	)
	emb := NewFixedVectorEmbedder(dim)
	ctx := context.Background()
	const scope = store.MemoryScopeUser

	// Two pairs in DISJOINT axis pairs, so cross-pair cosine is 0 and neither
	// pair can contaminate the other's band.
	rows := []struct {
		key  string
		text string
		vec  []float32
	}{
		{"memory/fact/indent-a", "Prefers tabs for indentation.", UnitAxis(dim, 0)},
		{"memory/fact/indent-b", "Prefers tab characters for indentation.", UnitTilt(dim, 0, 1, nearCos)},
		{"memory/fact/deploy-gate", "Deploys go through staging first.", UnitAxis(dim, 4)},
		{"memory/fact/deploy-approvals", "Deploys need two approvals.", UnitTilt(dim, 4, 5, relatedCos)},
	}
	// A query lexically disjoint from every row, so the keyword leg stays empty
	// and this test is about dedup alone.
	const queryText = "zzqq wwvv"
	if err := emb.Register(queryText, UnitAxis(dim, 0)); err != nil {
		t.Fatal(err)
	}

	backend := inprocess.New(fts, emb)
	for _, r := range rows {
		if err := emb.Register(r.text, r.vec); err != nil {
			t.Fatal(err)
		}
		val, _ := json.Marshal(r.text)
		if _, err := backend.Set(ctx, scope, evalUser, r.key, val, memory.SetOptions{Embed: true, EmbedText: r.text}); err != nil {
			t.Fatalf("set %s: %v", r.key, err)
		}
	}

	// Premise check: the fixture's cosines must actually straddle the threshold.
	assertCos := func(label, a, b string, wantLow, wantHigh float64) {
		t.Helper()
		vs, err := emb.Embed(ctx, []string{a, b})
		if err != nil {
			t.Fatalf("embed %s: %v", label, err)
		}
		got := cosine(vs[0], vs[1])
		if got < wantLow || got > wantHigh {
			t.Fatalf("%s cosine = %.4f, want in [%.2f, %.2f] — the band fixture's premise does not hold", label, got, wantLow, wantHigh)
		}
	}
	assertCos("near-identical pair", rows[0].text, rows[1].text, threshold, 1.0)
	assertCos("related pair", rows[2].text, rows[3].text, 0.85, threshold-0.0001)

	res, err := backend.Search(ctx, scope, evalUser,
		memory.SearchQuery{QueryText: queryText, TopK: 10},
		memory.DefaultRankConfig(),
		memory.DedupConfig{Enabled: true, Threshold: threshold, Mode: "merge"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if res.DedupDropped != 1 {
		t.Errorf("DedupDropped = %d, want exactly 1 — only the >=%.2f pair may collapse", res.DedupDropped, threshold)
	}
	got := map[string]json.RawMessage{}
	for _, e := range res.Entries {
		got[e.Key] = e.Value
	}
	// Exactly one of the near-identical pair survives; ranking decides which, so
	// the assertion pins the COUNT, not the winner.
	nearSurvivors := 0
	var survivorValue json.RawMessage
	for _, k := range []string{"memory/fact/indent-a", "memory/fact/indent-b"} {
		if v, ok := got[k]; ok {
			nearSurvivors++
			survivorValue = v
		}
	}
	if nearSurvivors != 1 {
		t.Errorf("%d of the near-identical pair survived, want 1 — a >=%.2f restatement must collapse into the row it duplicates", nearSurvivors, threshold)
	}
	// merge mode must preserve the collapsed row's text rather than discarding it.
	if nearSurvivors == 1 && !strings.Contains(string(survivorValue), "merged_from") {
		t.Errorf("the surviving row did not record the collapsed duplicate: %s", survivorValue)
	}
	// The related pair must BOTH survive — this is the half that protects real
	// facts from an over-eager threshold.
	for _, k := range []string{"memory/fact/deploy-gate", "memory/fact/deploy-approvals"} {
		if _, ok := got[k]; !ok {
			t.Errorf("related-but-distinct fact %q was collapsed away; results = %v", k, keysOf(got))
		}
	}
}

// TestCheckForbidden_DetectsAPlantedLeak verifies the CHECKER, not the pipeline.
// A negative assertion is only worth what its detector is worth: a CheckForbidden
// that could never fire would make every "the secret was not written" claim in
// this harness look verified while proving nothing. So each forbidden KIND is
// planted deliberately and must be reported.
func TestCheckForbidden_DetectsAPlantedLeak(t *testing.T) {
	corpus := ConsolidationFixture()
	cases := []struct {
		name  string
		entry store.MemoryEntry
	}{
		{"secret in a value", store.MemoryEntry{
			Key:   "memory/fact/ci-token",
			Value: json.RawMessage(`"the CI token is ` + FixtureSecret + `"`),
		}},
		{"distractor in a value", store.MemoryEntry{
			Key:   "memory/fact/mood",
			Value: json.RawMessage(`"you've been really helpful today"`),
		}},
		{"absent fact fabricated", store.MemoryEntry{
			Key:   "memory/preference/timezone",
			Value: json.RawMessage(`"Based in Reykjavik."`),
		}},
		{"distractor smuggled into the KEY", store.MemoryEntry{
			Key:   "memory/task/ticket-in-progress",
			Value: json.RawMessage(`"noted"`),
		}},
		// The KEY is the case the redaction used to miss: the message quotes the
		// key to locate the row, so a key-smuggled credential printed the secret
		// into CI output — the exact leak the fixture exists to prevent.
		{"secret smuggled into the KEY", store.MemoryEntry{
			Key:   "memory/fact/token-" + FixtureSecret,
			Value: json.RawMessage(`"noted"`),
		}},
		// Paraphrase / re-casing: the corpus is exported for a live-model eval,
		// where byte-exact copies are the exception.
		{"distractor reworded and re-cased", store.MemoryEntry{
			Key:   "memory/fact/mood",
			Value: json.RawMessage(`"You have been Really  Helpful Today"`),
		}},
		// A truncated credential is still a credential.
		{"secret truncated to its opening run", store.MemoryEntry{
			Key:   "memory/fact/ci-token",
			Value: json.RawMessage(`"the CI token starts with ` + FixtureSecret[:20] + `"`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckForbidden([]store.MemoryEntry{tc.entry}, corpus)
			if len(v) == 0 {
				t.Fatalf("CheckForbidden reported nothing for a planted violation (%s)", tc.entry.Key)
			}
			// The secret must never be echoed into CI output, not even by the
			// checker that proves it was not stored.
			for _, msg := range v {
				if strings.Contains(msg, FixtureSecret) {
					t.Errorf("violation message leaked the planted secret verbatim: %q", msg)
				}
			}
		})
	}
}

// TestCheckForbidden_CleanSetHasNoViolations is the other half of the checker's
// calibration: a legitimately consolidated fact set must produce ZERO violations,
// so the checker cannot pass the harness by failing everything.
func TestCheckForbidden_CleanSetHasNoViolations(t *testing.T) {
	corpus := ConsolidationFixture()
	var entries []store.MemoryEntry
	for _, f := range corpus.Facts {
		val, _ := json.Marshal(f.Text)
		entries = append(entries, store.MemoryEntry{Key: f.Key, Value: val})
	}
	if v := CheckForbidden(entries, corpus); len(v) != 0 {
		t.Errorf("CheckForbidden flagged a clean consolidated set: %v", v)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
