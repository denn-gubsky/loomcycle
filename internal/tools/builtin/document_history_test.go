package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// RFC BS Phase 3a — the chunk body-change history log + backlinks.
//
// The store's Memory API is overwrite-only (no version-history read), so a
// chunk's prior BODIES live in their own append log (chunk_revisions) written on
// every body write. These tests pin: the log (create + body updates → newest-first
// revisions with the right actor), get_version's exact-body read + missing-revision
// error, diff's unified output, the BODY-CHANGE-ONLY rule (a metadata-only update
// snapshots nothing), backlinks (manual + [[name]] edges pointing at a chunk, with
// the auto flag + enriched from-title), and the delete cascades (no orphan rows).
//
// Every case touches SQL Memory, so all run on BOTH tiers via the same
// pgDocumentOrSkip harness the Phase-1 tags + Phase-2a name-link tests use; the
// postgres half is skipped without the aux DSN.

// historyBothTiers runs fn on sqlite and (if the aux DSN is set) postgres.
func historyBothTiers(t *testing.T, fn func(*testing.T, *Document, context.Context)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		fn(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		fn(t, d, ctx)
	})
}

// --- small op wrappers (user scope) ---

func histCreateDocument(t *testing.T, d *Document, ctx context.Context, title, path string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":%q}`, title, path))
	if r.IsError {
		t.Fatalf("create_document(%s): %s", title, r.Text)
	}
	return out["document_id"].(string)
}

// histCreateChunk creates a chunk (parentID "" → child of the root) and returns
// its id. %q renders the ASCII/newline test bodies as valid JSON string literals.
func histCreateChunk(t *testing.T, d *Document, ctx context.Context, docID, parentID, title, body string) string {
	t.Helper()
	req := fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q`, docID, title, body)
	if parentID != "" {
		req += fmt.Sprintf(`,"parent_id":%q`, parentID)
	}
	req += `}`
	out, r := docExec(t, d, ctx, req)
	if r.IsError {
		t.Fatalf("create_chunk(%s): %s", title, r.Text)
	}
	return out["id"].(string)
}

// histUpdateBody rewrites a chunk's body at the given current revision and returns
// the new (post-bump) revision.
func histUpdateBody(t *testing.T, d *Document, ctx context.Context, id string, rev int, body string) int {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%d,"body":%q}`, id, rev, body))
	if r.IsError {
		t.Fatalf("update_chunk(%s): %s", id, r.Text)
	}
	return int(out["revision"].(float64))
}

// histRevisions returns the history op's revision maps (newest first).
func histRevisions(t *testing.T, d *Document, ctx context.Context, chunkID string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"history","scope":"user","id":%q}`, chunkID))
	if r.IsError {
		t.Fatalf("history(%s): %s", chunkID, r.Text)
	}
	raw, _ := out["revisions"].([]any)
	revs := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		revs = append(revs, e.(map[string]any))
	}
	return revs
}

// --- tests ---

// TestDocumentHistory_LogListsBodyChangesNewestFirst pins the log + get_version +
// diff: two body updates on a chunk produce three revisions (newest first, correct
// actor), each revision reads back its exact body, a missing revision errors, and a
// diff between the first and last body shows the changed lines.
func TestDocumentHistory_LogListsBodyChangesNewestFirst(t *testing.T) {
	historyBothTiers(t, assertHistoryLog)
}

func assertHistoryLog(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	docID := histCreateDocument(t, d, ctx, "Hist Doc", "/hist/log")

	rev1Body := "alpha\nbeta\n"
	rev2Body := "alpha\nbeta-EDIT\n"
	rev3Body := "alpha\nGAMMA\n"
	cid := histCreateChunk(t, d, ctx, docID, "", "Chapter", rev1Body)
	if got := histUpdateBody(t, d, ctx, cid, 1, rev2Body); got != 2 {
		t.Fatalf("revision after 1st update = %d, want 2", got)
	}
	if got := histUpdateBody(t, d, ctx, cid, 2, rev3Body); got != 3 {
		t.Fatalf("revision after 2nd update = %d, want 3", got)
	}

	// history: 3 revisions, newest first, actor u1 (the fixture's UserID).
	revs := histRevisions(t, d, ctx, cid)
	if len(revs) != 3 {
		t.Fatalf("history len = %d, want 3", len(revs))
	}
	wantOrder := []int{3, 2, 1}
	for i, rmap := range revs {
		if got := int(rmap["revision"].(float64)); got != wantOrder[i] {
			t.Errorf("history[%d] revision = %d, want %d", i, got, wantOrder[i])
		}
		if got, _ := rmap["actor"].(string); got != "u1" {
			t.Errorf("history[%d] actor = %q, want u1", i, got)
		}
		if _, ok := rmap["created_at"]; !ok {
			t.Errorf("history[%d] missing created_at", i)
		}
	}

	// get_version returns each revision's exact body.
	for rev, want := range map[int]string{1: rev1Body, 2: rev2Body, 3: rev3Body} {
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_version","scope":"user","id":%q,"revision":%d}`, cid, rev))
		if r.IsError {
			t.Fatalf("get_version rev %d: %s", rev, r.Text)
		}
		if got := out["body"].(string); got != want {
			t.Errorf("get_version rev %d body = %q, want %q", rev, got, want)
		}
	}
	// A revision that was never recorded is a clear error, not an empty body.
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_version","scope":"user","id":%q,"revision":99}`, cid)); !r.IsError {
		t.Errorf("get_version rev 99 should error (no such revision)")
	}

	// diff rev1 → rev3 shows the changed line (removed beta, added GAMMA).
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff","scope":"user","id":%q,"from_revision":1,"to_revision":3}`, cid))
	if r.IsError {
		t.Fatalf("diff: %s", r.Text)
	}
	diff := out["diff"].(string)
	if !strings.Contains(diff, "-beta\n") {
		t.Errorf("diff missing removed line -beta; got:\n%s", diff)
	}
	if !strings.Contains(diff, "+GAMMA\n") {
		t.Errorf("diff missing added line +GAMMA; got:\n%s", diff)
	}
	// diff against a missing revision errors rather than diffing against "".
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff","scope":"user","id":%q,"from_revision":1,"to_revision":99}`, cid)); !r.IsError {
		t.Errorf("diff to a missing revision should error")
	}
}

// TestDocumentHistory_MetadataOnlyUpdateDoesNotSnapshot pins the body-change-only
// rule: a status-only update bumps the chunk's revision but records NO new history
// row (its body is unchanged), so history lists only the body-change revisions and
// no body snapshot exists at the metadata-bumped revision.
func TestDocumentHistory_MetadataOnlyUpdateDoesNotSnapshot(t *testing.T) {
	historyBothTiers(t, assertMetadataOnlyNoSnapshot)
}

func assertMetadataOnlyNoSnapshot(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	docID := histCreateDocument(t, d, ctx, "Meta Doc", "/hist/meta")
	cid := histCreateChunk(t, d, ctx, docID, "", "Section", "seed\n") // rev 1 snapshot
	if got := histUpdateBody(t, d, ctx, cid, 1, "seed-2\n"); got != 2 {
		t.Fatalf("revision after body update = %d, want 2", got)
	}

	// A metadata-only update (status, no body) bumps the chunk to revision 3 but
	// must write NO history row.
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":2,"status":"reviewed"}`, cid))
	if r.IsError {
		t.Fatalf("metadata-only update: %s", r.Text)
	}
	if got := int(out["revision"].(float64)); got != 3 {
		t.Fatalf("revision after metadata update = %d, want 3", got)
	}

	// history lists only the two body-change revisions [2,1], NOT [3,2,1].
	revs := histRevisions(t, d, ctx, cid)
	if len(revs) != 2 {
		t.Fatalf("history len = %d, want 2 (a metadata-only update must not snapshot)", len(revs))
	}
	if r0, r1 := int(revs[0]["revision"].(float64)), int(revs[1]["revision"].(float64)); r0 != 2 || r1 != 1 {
		t.Errorf("history revisions = [%d %d], want [2 1]", r0, r1)
	}
	// No body snapshot exists at the metadata-bumped revision 3.
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_version","scope":"user","id":%q,"revision":3}`, cid)); !r.IsError {
		t.Errorf("get_version rev 3 should error — the metadata-only update wrote no body snapshot")
	}
}

// TestDocumentBacklinks_ReturnsManualAndNameLinkSources pins backlinks: a chunk
// linked to both manually (link_chunks) and via an inline [[name]] link reports
// both sources, each with the correct auto flag and enriched from-title.
func TestDocumentBacklinks_ReturnsManualAndNameLinkSources(t *testing.T) {
	historyBothTiers(t, assertBacklinks)
}

func assertBacklinks(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	docID := histCreateDocument(t, d, ctx, "BL Doc", "/hist/bl")
	target := histCreateChunk(t, d, ctx, docID, "", "Target", "the target chunk\n")
	manual := histCreateChunk(t, d, ctx, docID, "", "Manual Source", "plain body\n")

	// A manual edge manual → target (auto=0).
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"references"}`, manual, target)); r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}
	// A parser edge auto → target from the inline [[Target]] name-link (auto=1).
	auto := histCreateChunk(t, d, ctx, docID, "", "Auto Source", "see [[Target]] here\n")

	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"backlinks","scope":"user","id":%q}`, target))
	if r.IsError {
		t.Fatalf("backlinks: %s", r.Text)
	}
	raw, _ := out["backlinks"].([]any)
	byFrom := map[string]map[string]any{}
	for _, e := range raw {
		m := e.(map[string]any)
		byFrom[m["from_id"].(string)] = m
	}
	if len(byFrom) != 2 {
		t.Fatalf("backlinks count = %d, want 2; got %v", len(byFrom), raw)
	}

	m := byFrom[manual]
	if m == nil {
		t.Fatalf("no backlink from the manual source")
	}
	if auto, _ := m["auto"].(bool); auto {
		t.Errorf("manual edge auto = true, want false")
	}
	if ft, _ := m["from_title"].(string); ft != "Manual Source" {
		t.Errorf("manual from_title = %q, want Manual Source", ft)
	}
	if k, _ := m["kind"].(string); k != "references" {
		t.Errorf("manual kind = %q, want references", k)
	}

	a := byFrom[auto]
	if a == nil {
		t.Fatalf("no backlink from the auto source (the [[Target]] name-link did not materialize an edge)")
	}
	if got, _ := a["auto"].(bool); !got {
		t.Errorf("name-link edge auto = false, want true")
	}
	if ft, _ := a["from_title"].(string); ft != "Auto Source" {
		t.Errorf("auto from_title = %q, want Auto Source", ft)
	}
}

// TestDocumentHistory_DeleteCascadesLeaveNoOrphans pins the cascade: delete_chunk
// (with a descendant) and delete_document both remove the chunk_revisions rows for
// every affected chunk — no orphaned history survives its chunk.
func TestDocumentHistory_DeleteCascadesLeaveNoOrphans(t *testing.T) {
	historyBothTiers(t, assertHistoryCascade)
}

func assertHistoryCascade(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()

	// delete_chunk: a parent with two body revisions + a child with one.
	doc1 := histCreateDocument(t, d, ctx, "Cascade One", "/hist/cascade1")
	parent := histCreateChunk(t, d, ctx, doc1, "", "Parent", "p-1\n")
	histUpdateBody(t, d, ctx, parent, 1, "p-2\n") // parent now has 2 revisions
	child := histCreateChunk(t, d, ctx, doc1, parent, "Child", "c-1\n")

	if got := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_revisions WHERE chunk_id = ? OR chunk_id = ?`, parent, child); got != 3 {
		t.Fatalf("pre-delete_chunk revision rows = %d, want 3", got)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_chunk","scope":"user","id":%q}`, parent)); r.IsError {
		t.Fatalf("delete_chunk: %s", r.Text)
	}
	if got := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_revisions WHERE chunk_id = ? OR chunk_id = ?`, parent, child); got != 0 {
		t.Errorf("delete_chunk left %d orphan revision rows, want 0", got)
	}

	// delete_document: a chunk with two body revisions.
	doc2 := histCreateDocument(t, d, ctx, "Cascade Two", "/hist/cascade2")
	only := histCreateChunk(t, d, ctx, doc2, "", "Only", "a\n")
	histUpdateBody(t, d, ctx, only, 1, "b\n") // 2 revisions

	if got := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_revisions WHERE chunk_id = ?`, only); got != 2 {
		t.Fatalf("pre-delete_document revision rows = %d, want 2", got)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_document","scope":"user","id":%q}`, doc2)); r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}
	if got := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_revisions WHERE chunk_id = ?`, only); got != 0 {
		t.Errorf("delete_document left %d orphan revision rows, want 0", got)
	}
}
