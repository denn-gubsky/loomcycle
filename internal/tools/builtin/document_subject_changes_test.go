package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// newSubject creates a subject the way adoption and the consolidator do: a /facts
// document whose root chunk is the entity node, keyed by natural_key.
func newSubject(t *testing.T, d *Document, ctx context.Context, scope, name, typ, nk string) (docID, rootID string) {
	t.Helper()
	slug := nk[strings.Index(nk, ":")+1:]
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":%q,"title":%q,"path":"/facts/%s",
		"type":%q,"subject":%q,"natural_key":%q}`, scope, name, slug, typ, name, nk))
	if r.IsError {
		t.Fatalf("create subject %s: %s", nk, r.Text)
	}
	return out["document_id"].(string), out["root_chunk_id"].(string)
}

// setType runs update_chunk with a new type at the chunk's current revision.
func setType(t *testing.T, d *Document, ctx context.Context, scope, chunkID, typ string) (map[string]any, tools.Result) {
	t.Helper()
	g, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":%q,"id":%q}`, scope, chunkID))
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	return docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":%q,"id":%q,"type":%q,"revision":%d}`,
		scope, chunkID, typ, int(g["revision"].(float64))))
}

func withRecorder(d *Document) *recordingWriter {
	w := &recordingWriter{}
	d.Writer = w
	return w
}

// TestSubjectRetype_UpdateChunkAnnouncesOldAndNewType (RFC FR §4, P3).
//
// A relation kind's domain and range are types, so a consumer re-checks the statements
// touching a subject whose type changed. The event names both types and the subject's
// document; the natural key is kept.
func TestSubjectRetype_UpdateChunkAnnouncesOldAndNewType(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	w := withRecorder(d)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")

	out, r := setType(t, d, ctx, "user", root, "organization")
	if r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	if out["not_announced"] != nil {
		t.Errorf("not_announced = %v on an unrestricted caller", out["not_announced"])
	}
	evs := w.subjectEvents(t)
	if len(evs) != 1 {
		t.Fatalf("events = %v, want one retyped", evs)
	}
	ev := evs[0]
	if ev["op"] != "retyped" || ev["subject_natural_key"] != "person:ada" || ev["old_type"] != "person" ||
		ev["new_type"] != "organization" || ev["document_id"] != docID || ev["root_chunk_id"] != root {
		t.Errorf("event = %v, want retyped person→organization for document %s root %s", ev, docID, root)
	}
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+root+`"}`)
	if ent, _ := g["entity"].(map[string]any); ent["natural_key"] != "person:ada" {
		t.Errorf("natural key after re-type = %v, want it kept", g["entity"])
	}
}

// TestSubjectRetype_NothingAnnouncedForANonSubjectOrAnUnchangedType (RFC FR §4, P3).
//
// Only a subject root's kind is a subject's identity: a plain document root and a keyed
// fact filed under a subject are not subjects, and a write that leaves the type as it
// was changes nothing a consumer acts on.
func TestSubjectRetype_NothingAnnouncedForANonSubjectOrAnUnchangedType(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	w := withRecorder(d)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	plain, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","type":"note"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	fact, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`",
		"title":"Ada wrote the first program","natural_key":"fact:ada-program","type":"fact","subject":"Ada"}`)
	if r.IsError {
		t.Fatalf("upsert fact: %s", r.Text)
	}

	for _, c := range []struct{ name, id, typ string }{
		{"plain document root", plain["root_chunk_id"].(string), "memo"},
		{"keyed fact under a subject", fact["id"].(string), "preference"},
		{"subject root, same type", root, "person"},
		{"subject root, same type in another case", root, "Person"},
	} {
		if _, r := setType(t, d, ctx, "user", c.id, c.typ); r.IsError {
			t.Fatalf("%s: update_chunk: %s", c.name, r.Text)
		}
		if evs := w.subjectEvents(t); len(evs) != 0 {
			t.Fatalf("%s: announced %v", c.name, evs)
		}
	}
}

// TestSubjectRetype_UpsertOnAnExistingKeyAnnounces (RFC FR §4, P3).
//
// upsert_chunk on a key the scope already holds updates that chunk in place, so it is
// the other road to a subject's type; a re-upsert with the same type stays quiet.
func TestSubjectRetype_UpsertOnAnExistingKeyAnnounces(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	w := withRecorder(d)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")

	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada","type":"person"}`); r.IsError {
		t.Fatalf("same-type upsert: %s", r.Text)
	}
	if evs := w.subjectEvents(t); len(evs) != 0 {
		t.Fatalf("same-type upsert announced %v", evs)
	}
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada","type":"organization"}`)
	if r.IsError {
		t.Fatalf("re-typing upsert: %s", r.Text)
	}
	if out["created"] != false || out["not_announced"] != nil {
		t.Errorf("upsert result = %v, want an in-place update with nothing withheld", out)
	}
	evs := w.subjectEvents(t)
	if len(evs) != 1 {
		t.Fatalf("events = %v, want one retyped", evs)
	}
	if ev := evs[0]; ev["op"] != "retyped" || ev["old_type"] != "person" || ev["new_type"] != "organization" ||
		ev["document_id"] != docID || ev["root_chunk_id"] != root || ev["subject_natural_key"] != "person:ada" {
		t.Errorf("event = %v", ev)
	}
}

// TestSubjectRemoval_DeleteDocumentAnnouncesASubjectOnly (RFC FR §5, P3).
//
// Statements in the same scope go with the document; the event is for other scopes,
// whose evidence names the subject by key. The identity is read before the delete,
// since nothing records it afterwards.
func TestSubjectRemoval_DeleteDocumentAnnouncesASubjectOnly(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	w := withRecorder(d)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	plain, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}

	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+plain["document_id"].(string)+`"}`); r.IsError {
		t.Fatalf("delete plain: %s", r.Text)
	}
	if evs := w.subjectEvents(t); len(evs) != 0 {
		t.Fatalf("deleting a plain document announced %v", evs)
	}
	out, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatalf("delete subject: %s", r.Text)
	}
	if out["not_announced"] != nil {
		t.Errorf("not_announced = %v on an unrestricted caller", out["not_announced"])
	}
	evs := w.subjectEvents(t)
	if len(evs) != 1 {
		t.Fatalf("events = %v, want one removed", evs)
	}
	if ev := evs[0]; ev["op"] != "removed" || ev["subject_natural_key"] != "person:ada" ||
		ev["document_id"] != docID || ev["root_chunk_id"] != root {
		t.Errorf("event = %v, want removed for document %s root %s", ev, docID, root)
	}
}

// TestSubjectChanges_WithheldWhenReadingIsPartitioned (RFC FR §2, P3).
//
// The event names the subject to everyone on the scope's channel, so a caller whose
// rules hide part of the scope from reading publishes nothing, and each op says why.
func TestSubjectChanges_WithheldWhenReadingIsPartitioned(t *testing.T) {
	d, base, _ := documentFixture(t)
	w := withRecorder(d)
	ctx := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "delete", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	docID, root := newSubject(t, d, ctx, "tenant", "Ada", "person", "person:ada")

	out, r := setType(t, d, ctx, "tenant", root, "organization")
	if r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	if out["not_announced"] == nil {
		t.Error("update_chunk gives no reason for not announcing the re-type")
	}
	out, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"tenant","natural_key":"person:ada","title":"Ada","type":"person"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	if out["not_announced"] == nil {
		t.Error("upsert_chunk gives no reason for not announcing the re-type")
	}
	out, r = docExec(t, d, ctx, `{"op":"delete_document","scope":"tenant","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}
	if out["not_announced"] == nil {
		t.Error("delete_document gives no reason for not announcing the removal")
	}
	if evs := w.subjectEvents(t); len(evs) != 0 {
		t.Errorf("announced under a partitioned scope: %v", evs)
	}
}

// TestSubjectRetype_RefusesATypeNotInForce (RFC FR §4, P3).
//
// A subject of a kind the tenant does not declare is a node nobody finds by kind. An
// entity write was already held to the declared types; re-typing through update_chunk
// carries no subject, so it was not — and an upsert without one was not either.
func TestSubjectRetype_RefusesATypeNotInForce(t *testing.T) {
	d, base, _ := documentFixture(t)
	w := withRecorder(d)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	_, root := newSubject(t, d, ctx, "user", "Head of Compliance", "Post", "post:head-of-compliance")

	if _, r := setType(t, d, ctx, "user", root, "Starship"); !r.IsError {
		t.Fatal("update_chunk re-typed a subject to an undeclared type")
	}
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"post:head-of-compliance","title":"Head of Compliance","type":"Starship"}`); !r.IsError {
		t.Fatal("upsert_chunk re-typed a subject to an undeclared type")
	}
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+root+`"}`)
	if g["type"] != "Post" {
		t.Errorf("type after refusals = %v, want Post untouched", g["type"])
	}
	if evs := w.subjectEvents(t); len(evs) != 0 {
		t.Fatalf("a refused re-type announced %v", evs)
	}
	if _, r := setType(t, d, ctx, "user", root, "OrganizationalUnit"); r.IsError {
		t.Fatalf("re-type to a declared class: %s", r.Text)
	}
	if evs := w.subjectEvents(t); len(evs) != 1 || evs[0]["new_type"] != "OrganizationalUnit" {
		t.Errorf("events = %v, want one retyped to OrganizationalUnit", evs)
	}
}
