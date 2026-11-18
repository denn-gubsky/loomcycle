package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// placeUnder files a tenant document under a concept (a subject statement to it): a
// dependent the plan counts, and one a delete would destroy.
func (f *retiredFixture) placeUnder(conceptTag string) string {
	f.t.Helper()
	out := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Placed " + conceptTag})
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": out["root_chunk_id"],
		"to_id": conceptOfTag(f.read(), conceptTag).ChunkID, "kind": "subject"})
	return out["document_id"].(string)
}

// edgesTouching counts the statements at either end of id in the tenant scope.
func (f *retiredFixture) edgesTouching(id string) int {
	f.t.Helper()
	res, err := f.d.query(f.ctx, mustTenantKey(f.t, f.d, f.ctx), `SELECT count(*) FROM chunk_edges WHERE from_id = ? OR to_id = ?`, id, id)
	if err != nil || len(res.Rows) != 1 {
		f.t.Fatalf("count edges: %v %v", err, res)
	}
	return asInt(res.Rows[0][0])
}

// exists reports whether chunk id is still in the tenant scope.
func (f *retiredFixture) exists(id string) bool {
	f.t.Helper()
	res, err := f.d.query(f.ctx, mustTenantKey(f.t, f.d, f.ctx), `SELECT 1 FROM chunks WHERE id = ?`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(res.Rows) == 1
}

func (f *retiredFixture) deleteChunk(ctx context.Context, id string) string {
	f.t.Helper()
	_, r := f.do(ctx, map[string]any{"op": "delete_chunk", "scope": "tenant", "id": id})
	if r.IsError {
		return r.Text
	}
	return ""
}

func adminCtx(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})
}

// TestDeleteChunk_EntryWithDependentsIsRefusedForEveryCaller: a concept something is
// filed under cannot be deleted by an agent, the tenant operator or an admin (RFC GQ D3);
// the operator's refusal names the count and says to retire, and the statement stays.
func TestDeleteChunk_EntryWithDependentsIsRefusedForEveryCaller(t *testing.T) {
	f := newRetiredFixture(t)
	f.placeUnder("topics/reporting")
	concept := f.chunk("Reporting")
	callers := map[string]context.Context{
		"agent":    tools.WithCallerTier(inRunCtx(f.ctx), tools.TierRun),
		"operator": f.ctx,
		"admin":    adminCtx(f.ctx),
	}
	for name, ctx := range callers {
		text := f.deleteChunk(ctx, concept)
		if text == "" {
			t.Fatalf("%s deleted a concept with a member", name)
		}
		if name != "agent" && (!strings.Contains(text, entryHasDependentsCode) || !strings.Contains(text, `concept "Reporting" (members 1, statements 1)`) ||
			!strings.Contains(text, "Retire it instead")) {
			t.Errorf("%s: refusal %q does not name the dependents and the retire", name, text)
		}
		if !f.exists(concept) || f.edgesTouching(concept) != 1 {
			t.Errorf("%s: after the refusal the concept exists=%v with %d statement(s), want it and its 1 statement kept",
				name, f.exists(concept), f.edgesTouching(concept))
		}
	}
}

// TestDeleteChunk_EntryWithoutDependentsCanBeDeleted: an entry nothing rests on is
// deleted as before, a relation kind and a concept alike.
func TestDeleteChunk_EntryWithoutDependentsCanBeDeleted(t *testing.T) {
	f := newRetiredFixture(t)
	for _, title := range []string{"filed_under", "Reporting"} {
		id := f.chunk(title)
		if text := f.deleteChunk(f.ctx, id); text != "" {
			t.Fatalf("delete %s: %s", title, text)
		}
		if f.exists(id) {
			t.Errorf("%s is still there", title)
		}
	}
}

// TestOntologyDelete_ParentOfAnEntryWithDependentsIsRefused: deleting a section header
// or a class takes the entries under it, so one of them in use refuses the delete.
func TestOntologyDelete_ParentOfAnEntryWithDependentsIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.placeUnder("topics/risk-management/outsourcing")
	f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A memo", "type": "Memo"})
	read := f.read()
	for what, id := range map[string]string{
		"the Schemes section": read.SectionChunks[memrank.SectionSchemes],
		"the scheme":          f.chunk("topics"),
		"the broader concept": f.chunk("Risk Management"),
		"the parent class":    f.chunk("Document"),
	} {
		if text := f.deleteChunk(f.ctx, id); !strings.Contains(text, entryHasDependentsCode) {
			t.Errorf("deleting %s: %q, want it refused", what, text)
		}
		if !f.exists(id) {
			t.Errorf("%s was deleted", what)
		}
	}
}

// TestOntologyDelete_ClassWithTypedChunksInAUserScopeIsRefused: most of a class's
// dependents live in user scopes; the plan's counter reaches them, so the delete is
// refused though the tenant scope holds nothing of the class.
func TestOntologyDelete_ClassWithTypedChunksInAUserScopeIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.must(map[string]any{"op": "create_document", "scope": "user", "title": "A provision", "type": "Provision"})
	id := f.chunk("Provision")
	if text := f.deleteChunk(f.ctx, id); !strings.Contains(text, entryHasDependentsCode) || !strings.Contains(text, `class "Provision"`) {
		t.Errorf("delete of a class used in a user scope: %q", text)
	}
	if !f.exists(id) {
		t.Error("the class was deleted")
	}
}

// TestOntologyDelete_ProposalIsStillDeletable: a proposal is never an entry.
func TestOntologyDelete_ProposalIsStillDeletable(t *testing.T) {
	f := newRetiredFixture(t)
	prop := f.must(map[string]any{"op": "propose_entity", "name": "Contract", "parent": "Document"})["chunk_id"].(string)
	if text := f.deleteChunk(f.ctx, prop); text != "" {
		t.Fatal(text)
	}
	if f.exists(prop) {
		t.Error("the proposal is still there")
	}
}

// TestDeleteChunk_OutsideTheOntologyIsUnchanged: a chunk of another tenant document is
// deleted with its statements, as before.
func TestDeleteChunk_OutsideTheOntologyIsUnchanged(t *testing.T) {
	f := newRetiredFixture(t)
	doc := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Notes"})
	a := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"], "title": "A"})["id"].(string)
	b := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"], "title": "B"})["id"].(string)
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "related"})
	if text := f.deleteChunk(f.ctx, a); text != "" {
		t.Fatal(text)
	}
	if f.exists(a) || f.edgesTouching(b) != 0 {
		t.Errorf("chunk kept=%v, statements left on its neighbour=%d", f.exists(a), f.edgesTouching(b))
	}
}

// TestDeleteDocument_OntologyWithEntriesInUseIsRefused: the ontology document itself
// is not deleted while any entry is in use, by the operator or an admin.
func TestDeleteDocument_OntologyWithEntriesInUseIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.placeUnder("topics/reporting")
	read := f.read()
	for name, ctx := range map[string]context.Context{"operator": f.ctx, "admin": adminCtx(f.ctx)} {
		_, r := f.do(ctx, map[string]any{"op": "delete_document", "scope": "tenant", "id": read.DocumentID})
		if !r.IsError || !strings.Contains(r.Text, entryHasDependentsCode) || !strings.Contains(r.Text, "the tenant ontology has entries in use") {
			t.Errorf("%s: delete_document of the ontology = %q, want it refused", name, r.Text)
		}
	}
	if !f.exists(read.RootChunkID) || f.edgesTouching(f.chunk("Reporting")) != 1 {
		t.Error("the ontology or its statement went")
	}
}

// TestDeleteOntologyChunk_EntryWithDependentsIsRefused: the exported door the proposal
// routes use is held to the same rule; it still removes a proposal.
func TestDeleteOntologyChunk_EntryWithDependentsIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.placeUnder("topics/reporting")
	concept := f.chunk("Reporting")
	if err := f.d.DeleteOntologyChunk(f.ctx, "tenant", concept); err == nil || !strings.Contains(err.Error(), entryHasDependentsCode) {
		t.Errorf("DeleteOntologyChunk of a concept in use: %v", err)
	}
	if !f.exists(concept) {
		t.Error("the concept was deleted")
	}
	prop := f.must(map[string]any{"op": "propose_entity", "name": "Contract", "parent": "Document"})["chunk_id"].(string)
	if err := f.d.DeleteOntologyChunk(f.ctx, "tenant", prop); err != nil || f.exists(prop) {
		t.Errorf("a proposal: err=%v, still there=%v", err, f.exists(prop))
	}
}

// TestImportMD_ReimportOntoTheOntologyIsRefused: an edition would replace changed
// entries under new ids, so the ontology takes no reimport; adding entries under it
// without reimport stays allowed (additions are direct, D2).
func TestImportMD_ReimportOntoTheOntologyIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	read := f.read()
	_, r := f.do(f.ctx, map[string]any{"op": "import_md", "scope": "tenant", "reimport": true, "document_id": read.DocumentID,
		"markdown": retiredOntologyMD})
	if !r.IsError || !strings.Contains(r.Text, "reimport onto the tenant ontology is refused") {
		t.Errorf("reimport onto the ontology = %q", r.Text)
	}
	f.must(map[string]any{"op": "import_md", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.SectionChunks[memrank.SectionClasses], "markdown": "# Ticket\n- `title` text\n"})
	if _, ok := termByName(f.read(), "Ticket"); !ok {
		t.Error("an import under the ontology without reimport added no class")
	}
}

// TestCopyAndMoveDocuments_OntologyIsRefusedForTheOperator: neither copies nor moves the
// ontology for any caller (planCopy), so neither is a way around D3.
func TestCopyAndMoveDocuments_OntologyIsRefusedForTheOperator(t *testing.T) {
	f := newRetiredFixture(t)
	read := f.read()
	for _, op := range []string{"copy_documents", "move_documents"} {
		out := f.must(map[string]any{"op": op, "scope": "tenant", "to_scope": "user", "document_ids": []string{read.DocumentID}})
		if b, _ := json.Marshal(out); !strings.Contains(string(b), copyIsOntology) {
			t.Errorf("%s of the ontology was not refused: %v", op, out)
		}
	}
	if !f.exists(read.RootChunkID) {
		t.Error("the ontology went")
	}
}

// TestOntologyEntryGainedStatements_FindsAStatementAtEitherEnd: the in-transaction
// re-check delete_chunk runs after the plan sees a statement at either end.
func TestOntologyEntryGainedStatements_FindsAStatementAtEitherEnd(t *testing.T) {
	f := newRetiredFixture(t)
	key := mustTenantKey(t, f.d, f.ctx)
	reporting, filed := f.chunk("Reporting"), f.chunk("filed_under")
	check := func(ids ...string) bool {
		var found bool
		if err := f.d.withSqlTxn(f.ctx, key, func(txnID string) error {
			var err error
			found, err = f.d.ontologyEntryGainedStatements(f.ctx, txnID, ids)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if check(reporting, filed) {
		t.Fatal("found a statement where there is none")
	}
	doc := f.placeUnder("topics/reporting")
	if !check(reporting) {
		t.Error("missed a statement INTO the entry")
	}
	root := f.must(map[string]any{"op": "get_document", "scope": "tenant", "id": doc})["root_chunk_id"].(string)
	if !check(root) {
		t.Error("missed a statement FROM the chunk")
	}
}

// TestOntologyDelete_RefusedOnPostgres: the refusal and the allowed delete on the
// postgres tier (the development tenant's).
func TestOntologyDelete_RefusedOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}
	f.placeUnder("topics/reporting")
	f.must(map[string]any{"op": "create_document", "scope": "user", "title": "A provision", "type": "Provision"})
	for _, title := range []string{"Reporting", "Provision"} {
		if text := f.deleteChunk(f.ctx, f.chunk(title)); !strings.Contains(text, entryHasDependentsCode) {
			t.Errorf("postgres: delete of %s in use = %q", title, text)
		}
	}
	id := f.chunk("filed_under")
	if text := f.deleteChunk(f.ctx, id); text != "" || f.exists(id) {
		t.Errorf("postgres: an unused relation: %q, still there %v", text, f.exists(id))
	}
}
