package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A chunk body's ORIGIN is what tells a fact from prose (RFC CV P2b / OQ3).
//
// Both are `doc.chunk:<hex>` rows in the k/v plane, so the key namespace cannot
// distinguish them — and while it was the only signal, `sources=documents`
// selected every chunk body and labelled the distilled facts among them
// "document". origin is the right discriminator because it is server-stamped and
// deliberately absent from the `Memory set` input schema BECAUSE it names the
// writer, so a caller cannot claim one.
//
// It could not be a lookup instead: chunk_memory_meta lives in SQL Memory, a
// DIFFERENT database from the k/v plane, so consulting it per candidate would be a
// cross-database round trip from each of the four sites that label a row.

// TestChunkBody_AFactCarriesItsOriginAndProseDoesNot pins both directions. The
// negative half matters as much as the positive one: if ordinary prose were
// stamped too, provenance would stop meaning "a distiller wrote this" and the
// discriminator would be worthless in the other direction.
func TestChunkBody_AFactCarriesItsOriginAndProseDoesNot(t *testing.T) {
	d, ctx, st := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)

	// A FACT: the entity pair is what marks a write as one the ontology governs.
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/denn-prefers-go","title":"Denn prefers Go",
		"body":"Denn prefers Go for backend services.","type":"fact","subject":"Denn"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	factID := asStr(out["id"])

	// ORDINARY PROSE in the same document, written the ordinary way.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`",
		"title":"A section","body":"Some ordinary prose that nobody distilled."}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	proseID := asStr(out["id"])

	// The scope id and tenant come from the tool's OWN resolution, so the test
	// reads exactly the row the tool wrote rather than a guessed address.
	sk := sidecarScope(t, d, ctx)
	tenant := direntTenant(ctx)
	factProv, err := st.MemoryProvenanceGet(ctx, tenant, store.MemoryScopeUser, sk.ScopeID, "doc.chunk:"+factID)
	if err != nil {
		t.Fatalf("read fact body provenance: %v", err)
	}
	proseProv, err := st.MemoryProvenanceGet(ctx, tenant, store.MemoryScopeUser, sk.ScopeID, "doc.chunk:"+proseID)
	if err != nil {
		t.Fatalf("read prose body provenance: %v", err)
	}

	if factProv.Origin == "" {
		t.Errorf("the fact's body row carries no origin — once the chunk is the fact's only home it is indistinguishable from prose")
	}
	if proseProv.Origin != "" {
		t.Errorf("ordinary prose was stamped origin=%q — provenance would stop meaning 'a distiller wrote this'", proseProv.Origin)
	}

	// And the LABEL must follow the column, not the namespace.
	if got := store.ClassifyMemoryRow("doc.chunk:"+factID, factProv.Origin, "doc.chunk:"); got != store.MemoryRowFact {
		t.Errorf("a chunk-homed fact classifies as %q, want fact", got)
	}
	if got := store.ClassifyMemoryRow("doc.chunk:"+proseID, proseProv.Origin, "doc.chunk:"); got != store.MemoryRowDocument {
		t.Errorf("ordinary prose classifies as %q, want document", got)
	}
}
