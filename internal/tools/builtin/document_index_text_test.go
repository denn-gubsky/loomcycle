package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// indexDoc builds a small document through the tool — the same write path an agent
// uses — and returns the chunk ids by role.
//
//	Guide                    (root, no body)
//	├── Setup                (bodyless heading)
//	│   └── Install          (prose)
//	├── ---                  (heading with no letter in it, no body)
//	├── Raw                  (untyped chunk whose body is a data-URL image)
//	└── Screen               (image chunk, captioned)
func indexDoc(t *testing.T, d *Document, ctx context.Context) (docID string, ids map[string]string) {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Guide"}`))
	if err != nil || res.IsError {
		t.Fatalf("create_document: %v %s", err, res.Text)
	}
	docID = resultField(res, "document_id")
	ids = map[string]string{"root": resultField(res, "root_chunk_id")}
	mk := func(role, parent string, fields map[string]any) {
		t.Helper()
		fields["op"] = "create_chunk"
		fields["document_id"] = docID
		if parent != "" {
			fields["parent_id"] = ids[parent]
		}
		b, _ := json.Marshal(fields)
		r, err := d.Execute(ctx, b)
		if err != nil || r.IsError {
			t.Fatalf("create_chunk %s: %v %s", role, err, r.Text)
		}
		ids[role] = resultField(r, "id")
	}
	mk("setup", "", map[string]any{"title": "Setup", "body": ""})
	mk("install", "setup", map[string]any{"title": "Install", "body": "Run the installer twice."})
	mk("letterless", "", map[string]any{"title": "---", "body": ""})
	mk("raw", "", map[string]any{"title": "Raw", "body": "![x](data:image/png;base64,iVBORw0KGgo=)"})
	mk("screen", "", map[string]any{"title": "Screen", "type": "image", "body": "the login screen"})
	return docID, ids
}

// TestChunkIndexText_Rules pins each rule of the one definition.
func TestChunkIndexText_Rules(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide", "login", "screen")
	_, ids := indexDoc(t, d, ctx)

	for _, tc := range []struct{ role, want, why string }{
		{"install", "Guide — Setup > Install\nRun the installer twice.",
			"a chunk with content is indexed under its full heading path"},
		{"setup", "Guide — Setup",
			"a bodyless heading is indexed under its header alone (the generalised title fallback)"},
		{"root", "",
			"a bodyless ROOT is not indexed: it outranked every section on a query naming the document"},
		{"letterless", "",
			"a heading with no letter is not indexed; the header must not make it a row matching only the document name"},
		{"raw", "",
			"a non-mermaid media body is never indexed, header or not"},
		{"screen", "Guide — Screen\nimage: Screen the login screen",
			"an image keeps its title+caption text, under the header"},
	} {
		if got := embeddedTextFor(t, vs, ids[tc.role]); got != tc.want {
			t.Errorf("%s: embedded %q, want %q — %s", tc.role, got, tc.want, tc.why)
		}
	}
}

// TestChunkIndexText_OneRuleForEveryPath is the census the design rests on: the admin
// backfill, re-embed and stale-embedding purge derive a chunk's text through
// ChunkIndexTextForRow, and it must reproduce EXACTLY what the write path stored — for
// every chunk, indexed or not. Before this, the admin paths composed their own text and
// drifted (re-embed indexed the raw JSON envelope; both dropped image descriptions), so
// what a scope returned depended on which path last touched a row.
func TestChunkIndexText_OneRuleForEveryPath(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "installer", "setup", "guide", "login", "screen")
	_, ids := indexDoc(t, d, ctx)
	// An image's generated description reaches the index through the write path's
	// re-embed; the admin paths must derive it too.
	if err := d.SetAssetDescription(ctx, "user", ids["screen"], "two text fields and a button"); err != nil {
		t.Logf("SetAssetDescription: %v (no asset on this chunk; the caption path still applies)", err)
	}

	for role, id := range ids {
		// Bodies key on the RUN tenant (the fixture's "tnt"), which is what the admin
		// paths' listing hands them. A chunk may have no body row at all (a bodyless
		// heading); derive from an empty envelope then, as a caller holding only the key
		// would.
		row, err := vs.MemoryGet(context.Background(), direntTenant(ctx), store.MemoryScopeUser, "u1", chunkBodyKey(id))
		if err != nil {
			row = store.MemoryEntry{Key: chunkBodyKey(id), Value: json.RawMessage(`{"body":""}`)}
		}
		derived, isChunk := ChunkIndexTextForRow(context.Background(), d.SqlMem, "tnt",
			store.MemoryScopeUser, "u1", row)
		if !isChunk {
			t.Fatalf("%s: %s not recognised as a chunk body", role, row.Key)
		}
		if stored := embeddedTextFor(t, vs, id); stored != derived {
			t.Errorf("%s: write path stored %q but the admin paths derive %q — the two must be one rule",
				role, stored, derived)
		}
	}
}

// TestChunkIndexText_UnreadableTreeFallsBackToContent — when the chunk tree cannot be
// read, the text is the content alone, never "". The stale-embedding purge deletes a
// vector whose text derives to empty, so failing to "" on a read error would un-index
// real content (caught by the purge tests while building this).
func TestChunkIndexText_UnreadableTreeFallsBackToContent(t *testing.T) {
	row := store.MemoryEntry{Key: chunkBodyKey("no-such-chunk"),
		Value: json.RawMessage(`{"body":"Real prose that must stay searchable."}`)}

	// No SQL Memory at all.
	got, isChunk := ChunkIndexTextForRow(context.Background(), nil, "", store.MemoryScopeUser, "u1", row)
	if !isChunk || got != "Real prose that must stay searchable." {
		t.Errorf("no SQL Memory: got (%q, %v), want the content alone", got, isChunk)
	}

	// SQL Memory present, but the chunk row is gone.
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	got, _ = ChunkIndexTextForRow(context.Background(), mgr, "", store.MemoryScopeUser, "u1", row)
	if got != "Real prose that must stay searchable." {
		t.Errorf("missing chunk row: got %q, want the content alone", got)
	}

	// A non-chunk row is not the builder's to decide.
	if _, isChunk := ChunkIndexTextForRow(context.Background(), mgr, "", store.MemoryScopeUser, "u1",
		store.MemoryEntry{Key: "memory/fact/x", Value: json.RawMessage(`"x"`)}); isChunk {
		t.Error("a non-chunk row was claimed as a chunk body")
	}
}

// TestChunkIndexText_PostgresTier runs the lineage read — a recursive CTE — on the
// Postgres SQL Memory tier, where production documents live. The file tier passing says
// nothing about the Postgres dialect.
func TestChunkIndexText_PostgresTier(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres-tier index-text test")
	}
	base, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	vs := newVectorStore(base)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("sqlmem.NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	// A subject unique to this run, so a shared Postgres never hands the test a scope
	// left behind by an earlier one.
	subject := fmt.Sprintf("idx-%d", time.Now().UnixNano())
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: subject, TenantID: "tnt"})
	d := &Document{Store: vs, SqlMem: mgr, Bus: channels.NewBus(),
		Embedder: newFakeEmbedder("fake", "m1", "installer", "setup", "guide")}
	_, ids := indexDoc(t, d, ctx)

	e, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, subject, chunkBodyKey(ids["install"]))
	if err != nil {
		t.Fatalf("no embedding for the nested chunk: %v", err)
	}
	if want := "Guide — Setup > Install\nRun the installer twice."; e.EmbedText != want {
		t.Errorf("postgres tier indexed %q, want %q", e.EmbedText, want)
	}
	if _, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, subject, chunkBodyKey(ids["root"])); err == nil {
		t.Error("the bodyless root was indexed on the postgres tier")
	}

	// The subtree walk after a rename is a second recursive query (descendants, not
	// ancestors); it has to hold on this dialect too.
	get := docOp(t, d, ctx, map[string]any{"op": "get_chunk", "id": ids["setup"]})
	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": ids["setup"],
		"revision": int(get["revision"].(float64)), "title": "Getting started"})
	e, err = vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, subject, chunkBodyKey(ids["install"]))
	if err != nil || e.EmbedText != "Guide — Getting started > Install\nRun the installer twice." {
		t.Errorf("postgres tier: after renaming its parent the child is indexed as %q (%v)", e.EmbedText, err)
	}
}
