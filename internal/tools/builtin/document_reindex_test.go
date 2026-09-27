package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// countingEmbedder counts Embed calls — the re-index pass must cost NO embedding call
// for a chunk that is already current.
type countingEmbedder struct {
	providers.Embedder
	n atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.n.Add(1)
	return c.Embedder.Embed(ctx, texts)
}

func docOp(t *testing.T, d *Document, ctx context.Context, v map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	r, err := d.Execute(ctx, b)
	if err != nil || r.IsError {
		t.Fatalf("%v: %v %s", v["op"], err, r.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(r.Text), &out)
	return out
}

func chunkRevision(t *testing.T, d *Document, ctx context.Context, id string) int {
	t.Helper()
	out := docOp(t, d, ctx, map[string]any{"op": "get_chunk", "id": id})
	return int(out["revision"].(float64))
}

// TestReindex_RenameReindexesTheSubtree — a heading's title is part of the index text of
// every chunk beneath it, so renaming it must re-index them. Before, a title-only update
// re-embedded nothing at all, not even the renamed chunk.
func TestReindex_RenameReindexesTheSubtree(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide")
	_, ids := indexDoc(t, d, ctx)

	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": ids["setup"],
		"revision": chunkRevision(t, d, ctx, ids["setup"]), "title": "Getting started"})

	if got := embeddedTextFor(t, vs, ids["install"]); got != "Guide — Getting started > Install\nRun the installer twice." {
		t.Errorf("a child kept its old header after its parent was renamed: %q", got)
	}
	if got := embeddedTextFor(t, vs, ids["setup"]); got != "Guide — Getting started" {
		t.Errorf("the renamed heading itself was not re-indexed: %q", got)
	}
}

// TestReindex_RenameToLetterlessHeadingUnindexesIt — a chunk whose text derives to
// nothing must not KEEP the vector of what it used to say.
func TestReindex_RenameToLetterlessHeadingUnindexesIt(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide")
	_, ids := indexDoc(t, d, ctx)
	if embeddedTextFor(t, vs, ids["setup"]) == "" {
		t.Fatal("precondition: the heading is indexed")
	}
	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": ids["setup"],
		"revision": chunkRevision(t, d, ctx, ids["setup"]), "title": "---"})

	if got := embeddedTextFor(t, vs, ids["setup"]); got != "" {
		t.Errorf("a heading renamed to %q still carries its old vector: %q", "---", got)
	}
	// Its child's header drops the letterless part rather than indexing punctuation.
	if got := embeddedTextFor(t, vs, ids["install"]); got != "Guide — Install\nRun the installer twice." {
		t.Errorf("child header after a letterless rename: %q", got)
	}
}

// TestReindex_MoveReindexesTheMovedSubtree — moving a chunk changes its heading path.
func TestReindex_MoveReindexesTheMovedSubtree(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide")
	_, ids := indexDoc(t, d, ctx)

	docOp(t, d, ctx, map[string]any{"op": "move_chunk", "id": ids["install"], "new_parent_id": ids["screen"]})

	if got := embeddedTextFor(t, vs, ids["install"]); got != "Guide — Screen > Install\nRun the installer twice." {
		t.Errorf("a moved chunk kept its old heading path: %q", got)
	}
}

// TestReindex_RootRenameReachesEveryChunkInTheBackground — a root rename is a document
// rename: every chunk's header changes. Past reindexSyncMax chunks it runs in the
// background, and its end state must still be complete.
func TestReindex_RootRenameReachesEveryChunkInTheBackground(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "section")
	out := docOp(t, d, ctx, map[string]any{"op": "create_document", "title": "Old name"})
	docID, rootID := out["document_id"].(string), out["root_chunk_id"].(string)
	var ids []string
	for i := 0; i < reindexSyncMax+8; i++ {
		c := docOp(t, d, ctx, map[string]any{"op": "create_chunk", "document_id": docID,
			"title": fmt.Sprintf("Section %d", i), "body": "section text"})
		ids = append(ids, c["id"].(string))
	}
	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": rootID,
		"revision": chunkRevision(t, d, ctx, rootID), "title": "New name"})
	d.waitReindex()

	for i, id := range ids {
		want := fmt.Sprintf("New name — Section %d\nsection text", i)
		if got := embeddedTextFor(t, vs, id); got != want {
			t.Fatalf("chunk %d after a root rename: %q, want %q", i, got, want)
		}
	}
}

// TestReindex_ImportIndexesNestedChunksUnderTheirFullPath — import_md builds the tree top
// down, so a nested chunk's ancestors exist when it is indexed.
func TestReindex_ImportIndexesNestedChunksUnderTheirFullPath(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer")
	md := "# Guide\n\n## Setup\n\n### Install\n\nRun the installer twice.\n"
	out := docOp(t, d, ctx, map[string]any{"op": "import_md", "markdown": md})
	rows := docOp(t, d, ctx, map[string]any{"op": "query_chunks", "document_id": out["document_id"]})
	found := false
	for _, c := range rows["chunks"].([]any) {
		m := c.(map[string]any)
		if m["title"] == "Install" {
			found = true
			if got := embeddedTextFor(t, vs, m["id"].(string)); got != "Guide — Setup > Install\nRun the installer twice." {
				t.Errorf("an imported nested chunk is indexed as %q", got)
			}
		}
	}
	if !found {
		t.Fatal("the imported chunk is missing")
	}
}

// TestReindexScope_BringsAnOldStoreUpToDate is the operator pass end to end: a store
// written before headers — every chunk under its bare content, the bodyless root under
// its title — is brought in line, a dry run first touching nothing, and a second pass
// costing no embedding call at all.
func TestReindexScope_BringsAnOldStoreUpToDate(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide", "login", "screen")
	_, ids := indexDoc(t, d, ctx)
	// Rewind the index to what a pre-header deployment stored.
	legacy := map[string]string{
		"install": "Run the installer twice.",
		"setup":   "Setup",
		"screen":  "image: Screen the login screen",
		"root":    "Guide",
	}
	for role, text := range legacy {
		if err := vs.MemoryEmbedSet(context.Background(), direntTenant(ctx), store.MemoryScopeUser, "u1",
			chunkBodyKey(ids[role]), store.MemoryEmbedding{Provider: "fake", Model: "m1", Dimension: 1,
				Vector: []float32{1}, EmbedText: text, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	emb := &countingEmbedder{Embedder: d.Embedder}
	d.Embedder = emb

	dry, err := d.ReindexScope(ctx, "user", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Reindexed != 3 || dry.Unindexed != 1 {
		t.Errorf("dry run: reindexed=%d unindexed=%d, want 3 and 1 (the root)", dry.Reindexed, dry.Unindexed)
	}
	if got := embeddedTextFor(t, vs, ids["install"]); got != legacy["install"] || emb.n.Load() != 0 {
		t.Errorf("the dry run wrote (text now %q, %d embed calls)", got, emb.n.Load())
	}

	rep, err := d.ReindexScope(ctx, "user", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reindexed != 3 || rep.Unindexed != 1 || rep.Failed != 0 || rep.More {
		t.Errorf("real run: %+v", rep)
	}
	if got := embeddedTextFor(t, vs, ids["install"]); got != "Guide — Setup > Install\nRun the installer twice." {
		t.Errorf("after re-index the chunk is indexed as %q", got)
	}
	if got := embeddedTextFor(t, vs, ids["root"]); got != "" {
		t.Errorf("the bodyless root still has a vector after re-index: %q", got)
	}

	calls := emb.n.Load()
	again, err := d.ReindexScope(ctx, "user", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Reindexed != 0 || again.Unindexed != 0 || emb.n.Load() != calls {
		t.Errorf("a second pass over a current store changed %d/%d and made %d embed calls — it must be idempotent and free",
			again.Reindexed, again.Unindexed, emb.n.Load()-calls)
	}
}

// TestReindexScope_PagesByCursor — `limit` bounds the chunks CHANGED per call, and the
// cursor resumes exactly where the last call stopped until nothing is left.
func TestReindexScope_PagesByCursor(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide", "login", "screen")
	_, ids := indexDoc(t, d, ctx)
	for _, role := range []string{"install", "setup", "screen"} {
		_ = vs.MemoryEmbedSet(context.Background(), direntTenant(ctx), store.MemoryScopeUser, "u1",
			chunkBodyKey(ids[role]), store.MemoryEmbedding{Provider: "fake", Model: "m1", Dimension: 1,
				Vector: []float32{1}, EmbedText: "stale", CreatedAt: time.Now()})
	}
	total, cursor := 0, ""
	for calls := 0; ; calls++ {
		if calls > 5 {
			t.Fatal("paging did not terminate")
		}
		rep, err := d.ReindexScope(ctx, "user", cursor, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		total += rep.Reindexed
		if !rep.More {
			break
		}
		cursor = rep.NextCursor
	}
	if total != 3 {
		t.Errorf("paging re-indexed %d chunks in total, want 3", total)
	}
	for _, role := range []string{"install", "setup", "screen"} {
		if got := embeddedTextFor(t, vs, ids[role]); strings.Contains(got, "stale") {
			t.Errorf("%s still stale after paging: %q", role, got)
		}
	}
}
