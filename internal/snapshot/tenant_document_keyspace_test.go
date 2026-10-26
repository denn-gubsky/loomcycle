package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestRestoreDocumentPrefixes_MatchTheMemoryPackage(t *testing.T) {
	if restoreDocumentChunkPrefix != memrank.DocumentChunkKeyPrefix || restoreDocumentUnitPrefix != memrank.DocumentUnitKeyPrefix {
		t.Errorf("restore prefixes %q/%q drifted from the memory package's %q/%q", restoreDocumentChunkPrefix,
			restoreDocumentUnitPrefix, memrank.DocumentChunkKeyPrefix, memrank.DocumentUnitKeyPrefix)
	}
}

// TestSnapshotRestore_RewritesOldTenantDocumentRows — a snapshot captured from a
// database whose tenant document rows sit at the old SQL Memory scope id restores
// them at the tenant keyspace, embeddings included; a stray scope id, a
// non-document key and another scope restore as captured.
func TestSnapshotRestore_RewritesOldTenantDocumentRows(t *testing.T) {
	ctx := context.Background()
	srcBase, cleanupSrc := newTestStore(t)
	defer cleanupSrc()
	src := newVectorSnapshotStore(srcBase, true)
	ts := store.MemoryScopeTenant
	seed := func(tenant string, scope store.MemoryScope, scopeID, key string) {
		t.Helper()
		if err := src.Store.MemorySet(ctx, tenant, scope, scopeID, key, json.RawMessage(`"v"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Seeded after the source opened, so its open-time migration has not moved them:
	// the shape an older binary left behind.
	seed("tnt", ts, sqlmem.ScopeTenant("tnt"), "doc.chunk:a")
	if err := src.MemoryEmbedSet(ctx, "tnt", ts, sqlmem.ScopeTenant("tnt"), "doc.chunk:a", store.MemoryEmbedding{
		Provider: "p", Model: "m", Dimension: 2, Vector: []float32{1, 0}, EmbedText: "a",
	}); err != nil {
		t.Fatal(err)
	}
	seed("", ts, sqlmem.ScopeTenant(""), "doc.unit:d:summary:0")
	seed("tnt", ts, "other", "doc.chunk:stray")
	seed("tnt", ts, "tnt", "notes/x")
	seed("tnt", store.MemoryScopeUser, "tnt", "doc.chunk:u")

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dstBase, cleanupDst := newTestStore(t)
	defer cleanupDst()
	dst := newVectorSnapshotStore(dstBase, true)
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}

	present := func(tenant string, scope store.MemoryScope, scopeID, key string) bool {
		_, err := dst.Store.MemoryGet(ctx, tenant, scope, scopeID, key)
		return err == nil
	}
	for _, c := range []struct {
		tenant  string
		scope   store.MemoryScope
		scopeID string
		key     string
		want    bool
	}{
		{"tnt", ts, "", "doc.chunk:a", true},
		{"tnt", ts, "tnt", "doc.chunk:a", false},
		{"", ts, "", "doc.unit:d:summary:0", true},
		{"", ts, "default", "doc.unit:d:summary:0", false},
		{"tnt", ts, "other", "doc.chunk:stray", true},
		{"tnt", ts, "tnt", "notes/x", true},
		{"tnt", store.MemoryScopeUser, "tnt", "doc.chunk:u", true},
	} {
		if got := present(c.tenant, c.scope, c.scopeID, c.key); got != c.want {
			t.Errorf("%q/%s/%q/%s present = %v, want %v", c.tenant, c.scope, c.scopeID, c.key, got, c.want)
		}
	}
	if _, err := dst.MemoryEmbedGet(ctx, "tnt", ts, "", "doc.chunk:a"); err != nil {
		t.Errorf("the embedding did not follow its row to the tenant keyspace: %v", err)
	}
}
