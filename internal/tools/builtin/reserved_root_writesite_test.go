package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Third review of RFC FY P2: the reserved-root gate fails closed and is backed at the
// write sites, so an op the gate's census missed still cannot rewrite a reserved root.

// entitiesWithKeyedRoot is reservedEnv plus an operator-made entities document whose
// root carries a natural key.
func entitiesWithKeyedRoot(t *testing.T) (reservedEnv, string, string) {
	t.Helper()
	e := newReservedEnv(t)
	out, r := docExec(t, e.d, e.opCtx, `{"op":"create_document","scope":"tenant","title":"Entities","path":"/memory/entities","natural_key":"ent:entities-root","type":"object","subject":"Entities"}`)
	if r.IsError {
		t.Fatalf("entities: %s", r.Text)
	}
	doc, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	return e, doc, root
}

// alwaysFailingEntitiesLookup fails every dirent read of /memory/entities.
type alwaysFailingEntitiesLookup struct{ store.Store }

func (f alwaysFailingEntitiesLookup) DirentGet(ctx context.Context, tenant, scope, scopeID, parent, name string) (store.DirentRow, error) {
	if parent+name == "/memory/entities" {
		return store.DirentRow{}, fmt.Errorf("injected store fault")
	}
	return f.Store.DirentGet(ctx, tenant, scope, scopeID, parent, name)
}

// TestReservedRoot_AFailedLookupIsRefused: when the gate cannot resolve the document a
// call names, it must not let the call through; a lookup the handler repeats a moment
// later may succeed.
func TestReservedRoot_AFailedLookupIsRefused(t *testing.T) {
	e, _, _ := entitiesWithKeyedRoot(t)
	faulty := &Document{Store: alwaysFailingEntitiesLookup{e.d.Store}, SqlMem: e.d.SqlMem}
	key := mustTenantKey(t, e.d, e.opCtx)
	for _, op := range []string{"classify", "sync", "set_remote"} {
		if r := faulty.reservedRootRefusal(e.agentCtx, key, docInput{Op: op, Path: "/memory/entities"}); r == nil {
			t.Errorf("%s: a failed lookup of the named document let the call through", op)
		}
	}
}

// TestReservedRoot_DisagreeingIDAndDocumentIDAreRefused: on a document-addressed op an
// id and a document_id that differ leave the gate and the handler free to judge two
// different documents; the call is refused before either.
func TestReservedRoot_DisagreeingIDAndDocumentIDAreRefused(t *testing.T) {
	e, entities, _ := entitiesWithKeyedRoot(t)
	key := mustTenantKey(t, e.d, e.opCtx)
	for _, op := range []string{"classify", "sync", "set_remote", "define_type", "import_md"} {
		in := docInput{Op: op, ID: entities, DocumentID: e.decoy}
		if r := e.d.reservedRootRefusal(e.agentCtx, key, in); r == nil {
			t.Errorf("%s: id %s and document_id %s disagree and the call was not refused", op, entities, e.decoy)
		}
	}
}

// TestReservedRoot_UpsertCannotReachItAtTheWriteSite: the handler itself refuses an
// upsert whose natural key names the root of a reserved document, so an op that
// reaches upsertChunk without passing the op gate still cannot.
func TestReservedRoot_UpsertCannotReachItAtTheWriteSite(t *testing.T) {
	e, entities, root := entitiesWithKeyedRoot(t)
	key := mustTenantKey(t, e.d, e.opCtx)
	mscope := store.MemoryScopeTenant
	in := docInput{Op: "upsert_chunk", Scope: "tenant", DocumentID: entities, NaturalKey: "ent:entities-root", Title: "Mine", Body: "rewritten"}
	res, err := e.d.upsertChunk(e.agentCtx, key, mscope, in)
	if err != nil || !res.IsError {
		t.Errorf("upsertChunk rewrote the root of a reserved document: %v %s", err, res.Text)
	}
	got, _ := docExec(t, e.d, e.opCtx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	if got["title"] != "Entities" {
		t.Errorf("the root changed: %v", got["title"])
	}
	if res, err := e.d.upsertChunk(e.opCtx, key, mscope, in); err != nil || res.IsError {
		t.Errorf("the operator's upsert was refused: %v %s", err, res.Text)
	}
}

// TestReservedRoot_SyncCannotCrossDocumentsByNaturalKey: a sync pull writes keyed
// chunks into the bound document; a key a chunk of ANOTHER document holds would make
// upsert_chunk rewrite that chunk instead.
func TestReservedRoot_SyncCannotCrossDocumentsByNaturalKey(t *testing.T) {
	e, _, root := entitiesWithKeyedRoot(t)
	key := mustTenantKey(t, e.d, e.opCtx)
	w := localWriter{d: e.d, key: key, mscope: store.MemoryScopeTenant, docID: e.decoy}
	if _, err := w.createKeyed(e.agentCtx, syncChunk{nk: "ent:entities-root", title: "Mine", body: "rewritten"}); err == nil {
		t.Error("a sync write into one document rewrote another's chunk by its natural key")
	}
	got, _ := docExec(t, e.d, e.opCtx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	if got["title"] != "Entities" || got["document_id"] == e.decoy {
		t.Errorf("the other document's chunk changed: %v", got)
	}
}
