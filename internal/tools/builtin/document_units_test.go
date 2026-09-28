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
	}, "local-medium", 3)
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

	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitDescription, Text: "installs it"}}, "m", 4); err != nil {
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
		if _, err := d.ReplaceUnits(ctx, "user", c.chunk, c.units, "m", 1); err == nil {
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
	}, "m", 1); err != nil {
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
		if _, err := d.ReplaceUnits(ctx, "user", ids[role], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "a claim"}}, "m", 1); err != nil {
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
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "runs twice"}}, "m", 1); err != nil {
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
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "a"}, {Kind: memrank.UnitClaim, Text: "b"}}, "m", 1); err != nil {
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
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "runs twice"}}, "m", 1); err != nil {
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
