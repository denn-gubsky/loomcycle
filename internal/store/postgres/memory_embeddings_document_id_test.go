package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The document id stored on an embedding row (migration 0112) lets a filtered
// document search name its corpus by a few document ids instead of every chunk id.
// These tests hold the column to the same before-the-LIMIT rule as the id-segment
// filter, and pin the write semantics the Document tool relies on.

const docIDTestScope = "docid"

// docIDFixture seeds sixty decoy chunks of document "dx" that match the query
// better on both legs, three target chunks of document "dt", a derived unit of a
// target and of a decoy, and one row with no document id at all.
func docIDFixture(t *testing.T) *Store {
	t.Helper()
	fix := freshSchemaWithVectorsOrSkip(t)
	s := fix.store.(*Store)
	for i := 0; i < 60; i++ {
		seedDocRow(t, s, fmt.Sprintf("doc.chunk:decoy%02d", i), "dx", "protocol 86 board protocol 86 board", []float32{1, 0, 0, 0})
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		seedDocRow(t, s, "doc.chunk:"+id, "dt", "protocol 86 norm "+id, []float32{1, 1, 0, 0})
	}
	seedDocRow(t, s, "doc.unit:t1:claim:0", "dt", "protocol 86 claim", []float32{1, 1, 0, 0})
	seedDocRow(t, s, "doc.unit:decoy00:claim:0", "dx", "protocol 86 claim", []float32{1, 0, 0, 0})
	seedDocRow(t, s, "doc.chunk:orphan", "", "protocol 86 orphan", []float32{1, 1, 0, 0})
	return s
}

func seedDocRow(t *testing.T, s *Store, key, documentID, text string, vec []float32) {
	t.Helper()
	ctx := context.Background()
	v, _ := json.Marshal(text)
	if err := s.MemorySet(ctx, "", store.MemoryScopeUser, docIDTestScope, key, v, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, docIDTestScope, key, store.MemoryEmbedding{
		Provider: "test", Model: "m", Dimension: len(vec), Vector: vec, EmbedText: text,
		CreatedAt: time.Now(), DocumentID: documentID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySearch_DocumentIDsRestrictBothLegsBeforeTheLimit(t *testing.T) {
	s := docIDFixture(t)
	ctx := context.Background()
	for _, prefix := range []string{"doc.chunk:", "doc."} {
		f := store.MemorySearchFilter{KeyPrefix: prefix, DocumentIDs: []string{"dt"}}
		want := []string{"doc.chunk:t1", "doc.chunk:t2", "doc.chunk:t3"}
		if prefix == "doc." {
			want = append(want, "doc.unit:t1:claim:0")
		}
		vec, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, docIDTestScope, f, []float32{1, 0, 0, 0}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if got := keysOf(vec); !sameKeys(got, want...) {
			t.Errorf("prefix %q vector leg = %v, want %v — the sixty closer decoys of another document must not fill the pool", prefix, got, want)
		}
		ft, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, docIDTestScope, f, "protocol 86", 10)
		if err != nil {
			t.Fatal(err)
		}
		if got := keysOf(ft); !sameKeys(got, want...) {
			t.Errorf("prefix %q full-text leg = %v, want %v", prefix, got, want)
		}
	}
}

func TestMemorySearch_ExcludeDocumentIDsKeepsRowsWithoutADocument(t *testing.T) {
	s := docIDFixture(t)
	ctx := context.Background()
	f := store.MemorySearchFilter{KeyPrefix: "doc.chunk:", ExcludeDocumentIDs: []string{"dx"}}
	want := []string{"doc.chunk:t1", "doc.chunk:t2", "doc.chunk:t3", "doc.chunk:orphan"}
	vec, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, docIDTestScope, f, []float32{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(vec); !sameKeys(got, want...) {
		t.Errorf("vector leg = %v, want %v", got, want)
	}
	ft, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, docIDTestScope, f, "protocol 86", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(ft); !sameKeys(got, want...) {
		t.Errorf("full-text leg = %v, want %v", got, want)
	}
}

func TestMemoryEmbedSet_KeepsTheDocumentIDWhenNoneIsStated(t *testing.T) {
	fix := freshSchemaWithVectorsOrSkip(t)
	s := fix.store.(*Store)
	ctx := context.Background()
	seedDocRow(t, s, "doc.chunk:c1", "d1", "first", []float32{1, 0})
	// A re-embed that knows nothing about documents (an admin re-embed, a snapshot
	// restore) must not erase the attribution.
	seedDocRow(t, s, "doc.chunk:c1", "", "second", []float32{0, 1})
	got, err := s.MemoryEmbedGet(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.chunk:c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.DocumentID != "d1" || got.EmbedText != "second" {
		t.Fatalf("after an unattributed re-embed: document_id=%q text=%q, want d1 / second", got.DocumentID, got.EmbedText)
	}
	seedDocRow(t, s, "doc.chunk:c1", "d2", "third", []float32{1, 1})
	if got, _ = s.MemoryEmbedGet(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.chunk:c1"); got.DocumentID != "d2" {
		t.Fatalf("a stated document id must win: got %q, want d2", got.DocumentID)
	}
}

func TestMemoryEmbedSetDocumentID_ReachesTheChunkAndItsUnits(t *testing.T) {
	s := docIDFixture(t)
	ctx := context.Background()
	n, err := s.MemoryEmbedSetDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.chunk:", []string{"t1", "t2"}, "moved")
	if err != nil || n != 2 {
		t.Fatalf("chunk rows: n=%d err=%v, want 2", n, err)
	}
	if n, err = s.MemoryEmbedSetDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.unit:", []string{"t1", "t2"}, "moved"); err != nil || n != 1 {
		t.Fatalf("unit rows: n=%d err=%v, want 1 (t1 has one unit, t2 none)", n, err)
	}
	// Idempotent: rows already right are not rewritten.
	if n, _ = s.MemoryEmbedSetDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.chunk:", []string{"t1", "t2"}, "moved"); n != 0 {
		t.Fatalf("re-run changed %d rows, want 0", n)
	}
	rows, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, docIDTestScope,
		store.MemorySearchFilter{KeyPrefix: "doc.", DocumentIDs: []string{"moved"}}, []float32{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(rows); !sameKeys(got, "doc.chunk:t1", "doc.chunk:t2", "doc.unit:t1:claim:0") {
		t.Fatalf("search by the new document = %v", got)
	}
	// Clearing: an empty document id stores none.
	if n, _ = s.MemoryEmbedSetDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.chunk:", []string{"t1"}, ""); n != 1 {
		t.Fatalf("clear changed %d rows, want 1", n)
	}
	got, _ := s.MemoryEmbedGet(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.chunk:t1")
	if got.DocumentID != "" {
		t.Fatalf("cleared row still carries %q", got.DocumentID)
	}
}

func TestMemoryEmbedMissingDocumentID_SeesOnlyTheNamedPrefixes(t *testing.T) {
	fix := freshSchemaWithVectorsOrSkip(t)
	s := fix.store.(*Store)
	ctx := context.Background()
	prefixes := []string{"doc.chunk:", "doc.unit:"}
	missing := func() bool {
		t.Helper()
		m, err := s.MemoryEmbedMissingDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, prefixes)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	seedDocRow(t, s, "memory/fact/1", "", "a fact has no document", []float32{1, 0})
	seedDocRow(t, s, "doc.chunk:c1", "d1", "body", []float32{1, 0})
	if missing() {
		t.Fatal("a fact row without a document id must not count — only document rows do")
	}
	seedDocRow(t, s, "doc.unit:c1:claim:0", "", "unit", []float32{1, 0})
	if !missing() {
		t.Fatal("a unit row without a document id must count")
	}
	if _, err := s.MemoryEmbedSetDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, "doc.unit:", []string{"c1"}, "d1"); err != nil {
		t.Fatal(err)
	}
	if missing() {
		t.Fatal("every document row carries a document id now")
	}
	if m, _ := s.MemoryEmbedMissingDocumentID(ctx, "", store.MemoryScopeUser, docIDTestScope, nil); !m {
		t.Fatal("no prefixes must report missing, so a caller never takes the fast path on nothing")
	}
}

func TestMigrate0112_DocumentIDIsIdempotentAndReversible(t *testing.T) {
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; needs a pgvector-enabled Postgres")
	}
	fix := freshSchemaWithVectors(t, pgDSNFromEnv(t), true)
	t.Cleanup(fix.cleanup)
	s := fix.store.(*Store)
	ctx := context.Background()
	up, err := migrationsFS.ReadFile("migrations/0112_memory_embeddings_document_id.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := migrationsFS.ReadFile("migrations/0112_memory_embeddings_document_id.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Already applied by Open; a second run, a down and an up again must all succeed.
	for i, stmt := range []string{string(up), string(down), string(down), string(up)} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if !hasDocumentIDColumn(ctx, t, s) {
		t.Fatal("document_id column absent after the final up")
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname IN
		('memory_embeddings_by_document', 'memory_embeddings_document_id_missing')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("indexes after the final up: %d (err %v), want 2", n, err)
	}
}
