package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// IDsPrefixes confines the id lists to the document class of the named
// namespaces. A Memory search with no selector returns notes and facts beside
// chunk bodies and units, and a document filter on it must narrow only the
// documents: these rows have id segments too, and none of them is a chunk id.

// mixedIDSegmentFixture is idSegmentFixture plus the rows a mixed search also
// returns, each matching the query as well as the decoys do: a note (no
// provenance, not a chunk), a fact homed in a chunk (provenance), and a fact
// kept as a k/v row (provenance).
func mixedIDSegmentFixture(t *testing.T) (store.Store, string) {
	t.Helper()
	s, sid := idSegmentFixture(t)
	ctx := context.Background()
	seed := func(key, text string, prov store.MemoryProvenance) {
		v, _ := json.Marshal(text)
		var err error
		if prov.IsZero() {
			err = s.MemorySet(ctx, "", store.MemoryScopeUser, sid, key, v, 0)
		} else {
			err = s.MemorySetProvenance(ctx, "", store.MemoryScopeUser, sid, key, v, 0, prov)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, sid, key, store.MemoryEmbedding{
			Provider: "test", Model: "m", Dimension: 4, Vector: []float32{1, 0, 0, 0}, EmbedText: text, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	consolidated := store.MemoryProvenance{Origin: "consolidator", Class: "decision"}
	seed("notes/n1", "protocol 86 note", store.MemoryProvenance{})
	seed("doc.chunk:fact1", "protocol 86 fact", consolidated)
	seed("memory/decision/kv1", "protocol 86 kv fact", consolidated)
	return s, sid
}

var docNamespaces = []string{"doc.chunk:", "doc.unit:"}

// mixedWant is what an allowlist of {t1} leaves of the mixed fixture: t1's body
// and unit, and every row that is not document prose.
var mixedWant = []string{"doc.chunk:t1", "doc.unit:t1:claim:0", "notes/n1", "doc.chunk:fact1", "memory/decision/kv1"}

func TestMemoryEmbedSearch_IDsPrefixesRestrictOnlyDocumentRows(t *testing.T) {
	s, sid := mixedIDSegmentFixture(t)
	ctx := context.Background()
	got, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{IDs: []string{"t1"}, IDsPrefixes: docNamespaces}, []float32{1, 0, 0, 0}, 51)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(got); !sameKeys(k, mixedWant...) {
		t.Errorf("mixed search with IDs [t1] = %v, want %v — only document rows may be held to the list", k, mixedWant)
	}

	// The deny list, likewise: drop every decoy and its unit, keep the rest.
	excl := []string{"t2", "t3"}
	for i := 0; i < 60; i++ {
		excl = append(excl, decoyID(i))
	}
	got, err = s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{ExcludeIDs: excl, IDsPrefixes: docNamespaces}, []float32{1, 0, 0, 0}, 51)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(got); !sameKeys(k, mixedWant...) {
		t.Errorf("mixed search with ExcludeIDs = %v, want %v", k, mixedWant)
	}
}

func TestMemoryFullTextSearch_IDsPrefixesRestrictOnlyDocumentRows(t *testing.T) {
	s, sid := mixedIDSegmentFixture(t)
	ctx := context.Background()
	got, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{IDs: []string{"t1"}, IDsPrefixes: docNamespaces}, "protocol 86", 51)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(got); !sameKeys(k, mixedWant...) {
		t.Errorf("mixed full-text search with IDs [t1] = %v, want %v", k, mixedWant)
	}
}

func decoyID(i int) string {
	return "decoy" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}
