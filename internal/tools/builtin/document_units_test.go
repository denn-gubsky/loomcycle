package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func unitsDocFixture(t *testing.T) (*Document, *vectorStore, context.Context, map[string]string) {
	t.Helper()
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide", "reboot", "screen", "login")
	_, ids := indexDoc(t, d, ctx)
	return d, vs, ctx, ids
}

func unitKeysOf(t *testing.T, d *Document, ctx context.Context, chunkID string) []string {
	t.Helper()
	var out []string
	for _, u := range d.unitsOf(ctx, direntTenant(ctx), store.MemoryScopeUser, "u1", chunkID) {
		out = append(out, u.Key)
	}
	return out
}

// TestReplaceUnits_IndexesEachUnitUnderItsChunksHeader — a unit is found under
// the same document and section as its chunk, and a second write replaces the
// first rather than adding to it.
func TestReplaceUnits_IndexesEachUnitUnderItsChunksHeader(t *testing.T) {
	d, vs, ctx, ids := unitsDocFixture(t)
	n, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{
		{Kind: memrank.UnitQuestion, Text: "how often to reboot"},
		{Kind: memrank.UnitClaim, Text: "the installer runs twice"},
		{Kind: memrank.UnitQuestion, Text: "what does setup do"},
	}, UnitSource{Model: "local-medium", BodyRevision: 3})
	if err != nil || n != 3 {
		t.Fatalf("ReplaceUnits = %d, %v", n, err)
	}
	k := memrank.UnitKey(ids["install"], memrank.UnitQuestion, 1)
	e, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "u1", k)
	if err != nil || e.EmbedText != "Guide — Setup > Install\nwhat does setup do" {
		t.Errorf("unit %s indexed as %q (%v)", k, e.EmbedText, err)
	}
	us, _ := d.UnitsForChunk(ctx, "user", ids["install"])
	if len(us) != 3 || us[0].Model != "local-medium" || us[0].BodyRevision != 3 {
		t.Errorf("stored units = %+v", us)
	}

	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitDescription, Text: "installs it"}}, UnitSource{Model: "m", BodyRevision: 4}); err != nil {
		t.Fatal(err)
	}
	if got := unitKeysOf(t, d, ctx, ids["install"]); len(got) != 1 || got[0] != memrank.UnitKey(ids["install"], memrank.UnitDescription, 0) {
		t.Errorf("after a replace the chunk holds %v, want the one new unit", got)
	}
}

func TestReplaceUnits_RefusesWhatCouldNotStandForAChunk(t *testing.T) {
	d, _, ctx, ids := unitsDocFixture(t)
	for name, c := range map[string]struct {
		chunk string
		units []DerivedUnit
	}{
		"unknown kind":  {ids["install"], []DerivedUnit{{Kind: "summary", Text: "x"}}},
		"empty text":    {ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "  "}}},
		"missing chunk": {"no-such-chunk", []DerivedUnit{{Kind: memrank.UnitClaim, Text: "x"}}},
	} {
		if _, err := d.ReplaceUnits(ctx, "user", c.chunk, c.units, UnitSource{Model: "m", BodyRevision: 1}); err == nil {
			t.Errorf("%s: ReplaceUnits accepted it", name)
		}
	}
}

// TestDocumentSearch_FindsAChunkThroughItsUnit — the point of units: a question in
// words the chunk does not use finds it through a unit, once, with the unit that
// found it. An agent with memory_units:false searches chunk text only.
func TestDocumentSearch_FindsAChunkThroughItsUnit(t *testing.T) {
	d, _, ctx, ids := unitsDocFixture(t)
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{
		{Kind: memrank.UnitQuestion, Text: "how often to reboot"},
		{Kind: memrank.UnitQuestion, Text: "reboot after installing"},
	}, UnitSource{Model: "m", BodyRevision: 1}); err != nil {
		t.Fatal(err)
	}
	got, hit := firstChunk(t, d, ctx, "reboot", 5)
	if got != ids["install"] {
		t.Fatalf("top hit = %s, want the install chunk found through its units", got)
	}
	mu, ok := hit["matched_unit"].(map[string]any)
	if !ok || mu["kind"] != memrank.UnitQuestion || !strings.Contains(mu["text"].(string), "reboot") {
		t.Errorf("matched_unit = %v", hit["matched_unit"])
	}

	off := false
	octx := tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Units: &off})
	b, _ := json.Marshal(map[string]any{"op": "search", "query": "reboot", "limit": 5})
	out, err := d.Execute(octx, b)
	if err != nil || out.IsError {
		t.Fatalf("search: %v %s", err, out.Text)
	}
	if strings.Contains(out.Text, "matched_unit") {
		t.Errorf("an agent with memory_units:false got a unit match: %s", out.Text)
	}
}

// TestUnits_GoWithTheirChunkAndDocument — deleting a chunk, or its whole
// document, deletes its units.
func TestUnits_GoWithTheirChunkAndDocument(t *testing.T) {
	d, _, ctx, ids := unitsDocFixture(t)
	for _, role := range []string{"install", "screen"} {
		if _, err := d.ReplaceUnits(ctx, "user", ids[role], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "a claim"}}, UnitSource{Model: "m", BodyRevision: 1}); err != nil {
			t.Fatal(err)
		}
	}
	docOp(t, d, ctx, map[string]any{"op": "delete_chunk", "id": ids["install"]})
	if got := unitKeysOf(t, d, ctx, ids["install"]); len(got) != 0 {
		t.Errorf("delete_chunk left units %v", got)
	}
	out := docOp(t, d, ctx, map[string]any{"op": "get_chunk", "id": ids["screen"]})
	docOp(t, d, ctx, map[string]any{"op": "delete_document", "id": out["document_id"]})
	if got := unitKeysOf(t, d, ctx, ids["screen"]); len(got) != 0 {
		t.Errorf("delete_document left units %v", got)
	}
}

// TestUnits_ARenameReindexesThem — a unit carries its chunk's header, so renaming
// a heading above it must re-index it too.
func TestUnits_ARenameReindexesThem(t *testing.T) {
	d, vs, ctx, ids := unitsDocFixture(t)
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "runs twice"}}, UnitSource{Model: "m", BodyRevision: 1}); err != nil {
		t.Fatal(err)
	}
	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": ids["setup"],
		"revision": chunkRevision(t, d, ctx, ids["setup"]), "title": "Getting started"})
	k := memrank.UnitKey(ids["install"], memrank.UnitClaim, 0)
	e, _ := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "u1", k)
	if e.EmbedText != "Guide — Getting started > Install\nruns twice" {
		t.Errorf("after the rename the unit is indexed as %q", e.EmbedText)
	}
}

// TestUnits_TheSweeperReapsAnOrphan — a unit whose chunk row is gone (the unit
// delete failed, or the chunk went by a path that does not know units) is reaped
// like an orphaned body.
func TestUnits_TheSweeperReapsAnOrphan(t *testing.T) {
	d, _, ctx, ids := unitsDocFixture(t)
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "a"}, {Kind: memrank.UnitClaim, Text: "b"}}, UnitSource{Model: "m", BodyRevision: 1}); err != nil {
		t.Fatal(err)
	}
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.exec(ctx, key, `DELETE FROM chunks WHERE id = ?`, ids["install"]); err != nil {
		t.Fatal(err)
	}
	dry, err := d.ReconcileDeadLinks(ctx, key, mscope, true)
	if err != nil || dry.Units != 2 || len(unitKeysOf(t, d, ctx, ids["install"])) != 2 {
		t.Fatalf("dry run: %+v, %v (units must still be there)", dry, err)
	}
	rep, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil || rep.Units != 2 {
		t.Fatalf("reconcile: %+v, %v", rep, err)
	}
	if got := unitKeysOf(t, d, ctx, ids["install"]); len(got) != 0 {
		t.Errorf("orphaned units survived the sweep: %v", got)
	}
}

// TestReindexScope_BringsUnitsUpToDateToo — the operator pass walks units after
// bodies, in one cursor.
func TestReindexScope_BringsUnitsUpToDateToo(t *testing.T) {
	d, vs, ctx, ids := unitsDocFixture(t)
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "runs twice"}}, UnitSource{Model: "m", BodyRevision: 1}); err != nil {
		t.Fatal(err)
	}
	k := memrank.UnitKey(ids["install"], memrank.UnitClaim, 0)
	_ = vs.MemoryEmbedSet(context.Background(), direntTenant(ctx), store.MemoryScopeUser, "u1", k,
		store.MemoryEmbedding{Provider: "fake", Model: "m1", Dimension: 1, Vector: []float32{1}, EmbedText: "stale", CreatedAt: time.Now()})
	rep, err := d.ReindexScope(ctx, "user", "", 0, false)
	if err != nil || rep.Reindexed < 1 {
		t.Fatalf("reindex: %+v, %v", rep, err)
	}
	e, _ := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "u1", k)
	if e.EmbedText != "Guide — Setup > Install\nruns twice" {
		t.Errorf("the pass left the unit as %q", e.EmbedText)
	}
	if got, _ := ChunkIndexTextForRow(ctx, d.SqlMem, direntTenant(ctx), store.MemoryScopeUser, "u1",
		store.MemoryEntry{Key: k, Value: json.RawMessage(`{"kind":"claim","text":"runs twice"}`)}); got != e.EmbedText {
		t.Errorf("the admin paths would index the unit as %q, the write path as %q", got, e.EmbedText)
	}
}

// TestMemory_AnAgentCannotWriteAUnit — a unit votes for its chunk in everyone's
// document searches, so the namespace is the generation pass's alone.
func TestMemory_AnAgentCannotWriteAUnit(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	for _, op := range []string{
		`{"op":"set","scope":"user","key":"doc.unit:abc:claim:0","value":"planted"}`,
		`{"op":"merge","scope":"user","key":"doc.unit:abc:claim:0","value":{"a":1}}`,
		`{"op":"incr","scope":"user","key":"doc.unit:abc:claim:1"}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(op))
		if !res.IsError || !strings.Contains(res.Text, "doc.unit:") {
			t.Errorf("%s: not refused: %s", op, res.Text)
		}
	}
}

// TestReplaceUnits_AReplaceLeavesExactlyTheNewSet — the new units are written over
// the old keys and the old ones they did not overwrite are gone, whichever set is
// larger; every unit records its set's size.
func TestReplaceUnits_AReplaceLeavesExactlyTheNewSet(t *testing.T) {
	d, _, ctx, ids := unitsDocFixture(t)
	id := ids["install"]
	set := func(claims, questions int) []DerivedUnit {
		out := []DerivedUnit{{Kind: memrank.UnitDescription, Text: "installs it"}}
		for i := 0; i < claims; i++ {
			out = append(out, DerivedUnit{Kind: memrank.UnitClaim, Text: "a claim"})
		}
		for i := 0; i < questions; i++ {
			out = append(out, DerivedUnit{Kind: memrank.UnitQuestion, Text: "a question"})
		}
		return out
	}
	for _, c := range []struct{ claims, questions int }{{3, 3}, {1, 0}, {2, 4}} {
		units := set(c.claims, c.questions)
		if n, err := d.ReplaceUnits(ctx, "user", id, units, UnitSource{Model: "m", BodyRevision: 1}); err != nil || n != len(units) {
			t.Fatalf("ReplaceUnits = %d, %v", n, err)
		}
		var want []string
		want = append(want, memrank.UnitKey(id, memrank.UnitClaim, 0))
		for i := 1; i < c.claims; i++ {
			want = append(want, memrank.UnitKey(id, memrank.UnitClaim, i))
		}
		want = append(want, memrank.UnitKey(id, memrank.UnitDescription, 0))
		for i := 0; i < c.questions; i++ {
			want = append(want, memrank.UnitKey(id, memrank.UnitQuestion, i))
		}
		if got := unitKeysOf(t, d, ctx, id); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("after writing %d claims and %d questions the chunk holds %v, want %v", c.claims, c.questions, got, want)
		}
		us, _ := d.UnitsForChunk(ctx, "user", id)
		for _, u := range us {
			if u.UnitCount != len(units) {
				t.Errorf("a unit records a set of %d, want %d", u.UnitCount, len(units))
			}
		}
	}
}

// TestReplaceUnits_AnOverwrittenUnitThatFailsToEmbedKeepsNoOldVector — a new unit
// is written over an old one's key; when its embed fails the key must not keep the
// old unit's vector, which would find the chunk by what it no longer says.
func TestReplaceUnits_AnOverwrittenUnitThatFailsToEmbedKeepsNoOldVector(t *testing.T) {
	d, vs, ctx, ids := unitsDocFixture(t)
	id := ids["install"]
	if _, err := d.ReplaceUnits(ctx, "user", id, []DerivedUnit{{Kind: memrank.UnitClaim, Text: "the installer runs twice"}}, UnitSource{Model: "m", BodyRevision: 1}); err != nil {
		t.Fatal(err)
	}
	k := memrank.UnitKey(id, memrank.UnitClaim, 0)
	if _, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "u1", k); err != nil {
		t.Fatalf("the first unit was not embedded: %v", err)
	}
	d.Embedder.(*fakeEmbedder).failNext = true
	if _, err := d.ReplaceUnits(ctx, "user", id, []DerivedUnit{{Kind: memrank.UnitClaim, Text: "reboot after login"}}, UnitSource{Model: "m", BodyRevision: 2}); err != nil {
		t.Fatal(err)
	}
	if e, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "u1", k); err == nil {
		t.Errorf("the unit's key still carries a vector for %q", e.EmbedText)
	}
}
