package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A FACT'S IDENTITY IS ITS NATURAL KEY, wherever the fact is stored
// (RFC CV, P2f prerequisite).
//
// The backend reports a row's own key. For a chunk-homed fact that is the opaque
// `doc.chunk:<hex>` address rather than the `memory/<class>/<slug>` name the fact
// has always had — and the two stores share one key space deliberately, because
// "one key space for both stores is what stops them drifting".
//
// Two things break if recall hands back the address. The consolidator's merge path
// writes a recalled neighbour back UNDER ITS OWN KEY, so it would file a fact under
// an address instead of a name. And SourceSpansFor keys on the natural key, so the
// source span and its run id — the reach-through P1 exists for — quietly stop
// resolving.

// TestChunkLabelsFor_CarriesTheNaturalKey pins the lookup half. The label query is
// already batched for titles, so the identity rides along on it rather than costing
// a second round trip.
func TestChunkLabelsFor_CarriesTheNaturalKey(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/denn-prefers-go","title":"Denn prefers Go",
		"body":"Denn prefers Go for backend services.","type":"fact","subject":"Denn"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	factID := asStr(out["id"])

	// Ordinary prose in the same document: it has no sidecar row, so it must carry
	// NO natural key. Inventing one would give a fact-shaped identity to a chunk
	// that has no other name.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`",
		"title":"A section","body":"Ordinary prose."}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	proseID := asStr(out["id"])

	sk := sidecarScope(t, d, ctx)
	labels := ChunkLabelsFor(ctx, d.SqlMem, direntTenant(ctx), store.MemoryScopeUser, sk.ScopeID,
		[]string{factID, proseID})
	if labels == nil {
		t.Fatal("no labels resolved")
	}
	if got := labels[factID].NaturalKey; got != "memory/fact/denn-prefers-go" {
		t.Errorf("fact chunk natural_key = %q, want memory/fact/denn-prefers-go — without it a "+
			"chunk-homed fact can only be addressed by its opaque row key", got)
	}
	if got := labels[proseID].NaturalKey; got != "" {
		t.Errorf("ordinary prose carries natural_key %q — a chunk with no sidecar has no other name", got)
	}
	// The titles must still resolve: the identity rides the SAME query.
	if labels[factID].Title == "" || labels[proseID].Title == "" {
		t.Errorf("adding the identity column cost a title: %+v / %+v", labels[factID], labels[proseID])
	}
}

// TestChunkLabelsFor_ToleratesAChunkWithNoSidecar guards the join. chunk_memory_meta
// is LEFT JOINed for the same reason the documents table is: most chunks have no row
// there, and an inner join would drop every ordinary chunk from the label lookup —
// a regression that would show as missing titles, not as an error.
func TestChunkLabelsFor_ToleratesAChunkWithNoSidecar(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	var ids []string
	for _, title := range []string{"one", "two", "three"} {
		out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`",
			"title":"`+title+`","body":"Prose."}`)
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", title, r.Text)
		}
		ids = append(ids, asStr(out["id"]))
	}
	sk := sidecarScope(t, d, ctx)
	labels := ChunkLabelsFor(ctx, d.SqlMem, direntTenant(ctx), store.MemoryScopeUser, sk.ScopeID, ids)
	if len(labels) != len(ids) {
		t.Errorf("resolved %d of %d sidecar-less chunks — an inner join would drop every ordinary chunk",
			len(labels), len(ids))
	}
	for _, id := range ids {
		if strings.TrimSpace(labels[id].Title) == "" {
			t.Errorf("chunk %s lost its title", id)
		}
	}
}

// TestSourceSpansFor_CarriesTheSessionSoThePointerIsFollowable (RFC CV P1).
//
// The span answers "what was said"; the SESSION answers "where is the rest of it",
// and only the second is followable. `History op=window` takes the pair and hands back
// the turn the fact was distilled from with its neighbours — which is what the
// answerer needs when the distilled sentence dropped the specific being asked for.
//
// This was 0% populated when the span lookup was first written, so it deliberately
// projected the span alone; the entity writer fills it now, from the consolidator and
// from the drained pending row. The plan's reach-through finally has data behind it,
// and a regression here would silently take it away again — the recall still answers,
// just with nowhere to go.
func TestSourceSpansFor_CarriesTheSessionSoThePointerIsFollowable(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)

	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/release-moved","title":"The release moved",
		"body":"The release moved.","type":"fact",
		"source_quote":"we moved the release to the 14th because Maria is out",
		"source_session_id":"s_the_chat"}`); r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}

	got := SourceSpansFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "u1",
		[]string{"memory/fact/release-moved"})
	src, ok := got["memory/fact/release-moved"]
	if !ok {
		t.Fatal("the fact reported no source at all")
	}
	if !strings.Contains(src.Span, "the 14th") {
		t.Errorf("span = %q, want the verbatim wording the fact was derived from", src.Span)
	}
	if src.SessionID != "s_the_chat" {
		t.Errorf("session = %q, want s_the_chat — without it the span is quotable but not "+
			"followable, and the surrounding turns are unreachable", src.SessionID)
	}
}
