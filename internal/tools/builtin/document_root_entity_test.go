package builtin

import (
	"testing"
)

// A document's ROOT can be the entity itself (RFC CV decision 1, the P3 primitive).
//
// Subject-homing collapses the subject node and the document root into ONE object:
// `/facts/<subject>` IS the subject, and its facts are its children. Two objects
// would mean two titles, two types, and two places to correct either — and "what
// do we know about X" would go back to being a traversal rather than one read.

// TestCreateDocument_RootBecomesTheEntityWhenNamed.
func TestCreateDocument_RootBecomesTheEntityWhenNamed(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Denn",
		"path":"/facts/denn","type":"person","subject":"Denn","natural_key":"person:denn"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	root := asStr(out["root_chunk_id"])
	if root == "" {
		t.Fatal("no root_chunk_id returned")
	}

	// The sidecar is what makes a chunk an entity — without it the root is an
	// ordinary container and the fact surfaces cannot see it.
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT coalesce(natural_key,''), coalesce(subject,''), coalesce(origin,'')
		                   FROM chunk_memory_meta WHERE chunk_id = ?`), []any{root})
	if err != nil {
		t.Fatalf("sidecar read: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("the root carries no entity sidecar, so the document is a container and not the subject")
	}
	if got := asStr(res.Rows[0][0]); got != "person:denn" {
		t.Errorf("root natural_key = %q, want person:denn", got)
	}
	if got := asStr(res.Rows[0][1]); got != "Denn" {
		t.Errorf("root subject = %q, want Denn", got)
	}
	if asStr(res.Rows[0][2]) == "" {
		t.Error("the root entity carries no origin — it is a write like any other and must say who made it")
	}

	// And it reads back as an entity through the ordinary surface.
	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+root+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	ent, _ := got["entity"].(map[string]any)
	if ent == nil {
		t.Fatalf("get_chunk on the root returned no entity block: %v", got)
	}
	if ent["natural_key"] != "person:denn" {
		t.Errorf("entity block natural_key = %v", ent["natural_key"])
	}
}

// TestCreateDocument_AnOrdinaryRootIsNotAnEntity is the other half, and the one
// that keeps every existing caller unchanged: a document created without the pair
// must NOT acquire a sidecar, or every ordinary document's root would start
// appearing in the fact surfaces.
func TestCreateDocument_AnOrdinaryRootIsNotAnEntity(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","path":"/docs/notes"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT count(*) FROM chunk_memory_meta WHERE chunk_id = ?`),
		[]any{asStr(out["root_chunk_id"])})
	if err != nil {
		t.Fatalf("sidecar count: %v", err)
	}
	if scanCount(res.Rows) != 0 {
		t.Error("an ordinary document's root became an entity — every document would then " +
			"list as a fact")
	}
}

// TestCreateDocument_HalfThePairIsNotAnEntity. type+subject is what marks an
// entity; a natural_key alone is what document SYNC uses to reconcile ordinary
// chunks, so treating it as an entity marker would turn every synced document's
// root into a subject.
func TestCreateDocument_HalfThePairIsNotAnEntity(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Synced",
		"path":"/docs/synced","natural_key":"doc:synced"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT count(*) FROM chunk_memory_meta WHERE chunk_id = ?`),
		[]any{asStr(out["root_chunk_id"])})
	if err != nil {
		t.Fatalf("sidecar count: %v", err)
	}
	if scanCount(res.Rows) != 0 {
		t.Error("a natural_key alone made the root an entity — document sync keys ordinary " +
			"chunks that way, so every synced document would become a subject")
	}
}
