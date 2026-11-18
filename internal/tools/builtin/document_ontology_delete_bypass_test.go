package builtin

import (
	"strings"
	"testing"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The ways around RFC GQ D3 a review of #2757 found: take an entry out of the read
// first (demote it, close it, move it where the reader stops), then delete it as a
// non-entry; or change the subtree between the guard's read and the delete.

// TestUpdateChunk_DemotingAnOntologyEntryOrSectionIsRefused: an entry or a section
// leaves force only by retirement; a proposal still moves between proposed and
// rejected, and the root keeps its own status words.
func TestUpdateChunk_DemotingAnOntologyEntryOrSectionIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.placeUnder("topics/reporting")
	read := f.read()
	for what, id := range map[string]string{
		"a concept in use":       f.chunk("Reporting"),
		"an unused relation":     f.chunk("filed_under"),
		"the Schemes section":    read.SectionChunks[memrank.SectionSchemes],
		"a retired entry, again": "",
	} {
		if id == "" {
			f.retire("Memo")
			id = f.chunk("Memo")
		}
		for _, status := range []string{memrank.OntologyStatusProposed, memrank.OntologyStatusRejected} {
			if text := f.setStatus(f.ctx, id, status); !strings.Contains(text, "retire it instead") {
				t.Errorf("%s -> %s: %q, want it refused with a pointer to retire", what, status, text)
			}
		}
	}
	if s := f.status(f.chunk("Reporting")); s != "" {
		t.Errorf("Reporting status = %q after refused demotions", s)
	}
	prop := f.must(map[string]any{"op": "propose_entity", "name": "Contract", "parent": "Document"})["chunk_id"].(string)
	if text := f.setStatus(f.ctx, prop, memrank.OntologyStatusRejected); text != "" {
		t.Errorf("rejecting a proposal: %s", text)
	}
	if text := f.setStatus(f.ctx, prop, memrank.OntologyStatusProposed); text != "" {
		t.Errorf("re-opening a rejection: %s", text)
	}
}

// TestSupersede_OfAnOntologyChunkIsRefused: supersede closes its target, which the
// retention prune then deletes unguarded; a chunk of the ontology is never closed.
func TestSupersede_OfAnOntologyChunkIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	doc := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Notes"})
	repl := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"], "title": "Replacement"})["id"].(string)
	concept := f.chunk("Reporting")
	_, r := f.do(f.ctx, map[string]any{"op": "supersede_chunk", "scope": "tenant", "id": repl, "supersedes_id": concept})
	if !r.IsError || !strings.Contains(r.Text, "ontology") {
		t.Errorf("supersede of an ontology concept = %q, want it refused", r.Text)
	}
	if _, _, err := f.d.retireChunk(f.ctx, mustTenantKey(t, f.d, f.ctx), concept, ""); err == nil {
		t.Error("retireChunk (Memory supersede's writer) closed an ontology chunk")
	}
	res, err := f.d.query(f.ctx, mustTenantKey(t, f.d, f.ctx), `SELECT count(*) FROM chunk_memory_meta WHERE chunk_id = ?`, concept)
	if err == nil && asInt(res.Rows[0][0]) != 0 {
		t.Error("the concept carries a closed sidecar row")
	}
}

// TestPruneRetiredChunks_SkipsTheOntologyDocument: a closed sidecar row on an ontology
// chunk (written by a build without the supersede refusal) is never pruned; a closed
// fact elsewhere still is.
func TestPruneRetiredChunks_SkipsTheOntologyDocument(t *testing.T) {
	f := newRetiredFixture(t)
	key := mustTenantKey(t, f.d, f.ctx)
	concept := f.chunk("Reporting")
	doc := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Notes"})
	fact := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"], "title": "Old fact"})["id"].(string)
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	for _, id := range []string{concept, fact} {
		if err := f.d.exec(f.ctx, key, `INSERT INTO chunk_memory_meta (chunk_id, invalid_at) VALUES (?, ?)`, id, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := f.d.PruneRetiredChunks(f.ctx, key, store.MemoryScopeTenant, time.Now().UnixNano(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !f.exists(concept) {
		t.Error("the retention prune deleted an ontology concept")
	}
	if f.exists(fact) || n != 1 {
		t.Errorf("pruned %d, fact kept=%v; want the closed fact alone pruned", n, f.exists(fact))
	}
}

// TestMoveChunk_TakingAnEntryOutOfTheOntologyIsRefused: under a proposal or a grammar
// the reader no longer walks to an entry, so such a move would let a delete through.
// A move between in-force places is unchanged.
func TestMoveChunk_TakingAnEntryOutOfTheOntologyIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A memo", "type": "Memo"})
	read := f.read()
	prop := f.must(map[string]any{"op": "propose_entity", "name": "Contract", "parent": "Document"})["chunk_id"].(string)
	grammar := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.SectionChunks[memrank.SectionClasses], "title": "grammar:cites", "body": "pattern: \\d+"})["id"].(string)
	memo := f.chunk("Memo")
	_, r := f.do(f.ctx, map[string]any{"op": "move_chunk", "scope": "tenant", "id": memo, "new_parent_id": grammar})
	if !r.IsError || !strings.Contains(r.Text, `class "Memo"`) {
		t.Errorf("move under a grammar = %q, want it refused naming the entry", r.Text)
	}
	if _, ok := termByName(f.read(), "Memo"); !ok {
		t.Fatal("Memo left the ontology")
	}
	if text := f.deleteChunk(f.ctx, memo); !strings.Contains(text, entryHasDependentsCode) {
		t.Errorf("delete after the refused move: %q", text)
	}
	// Under a proposal the reader re-attaches the entry to the nearest in-force
	// ancestor, so it stays in force and the move is allowed — and the proposal can no
	// longer be deleted, since the entry in use would go with it.
	f.must(map[string]any{"op": "move_chunk", "scope": "tenant", "id": memo, "new_parent_id": prop})
	if _, ok := termByName(f.read(), "Memo"); !ok {
		t.Fatal("Memo left the ontology under a proposal")
	}
	if text := f.deleteChunk(f.ctx, prop); !strings.Contains(text, `class "Memo"`) {
		t.Errorf("delete of the proposal holding an entry in use: %q", text)
	}
	f.must(map[string]any{"op": "move_chunk", "scope": "tenant", "id": f.chunk("Reporting"), "new_parent_id": f.chunk("Risk Management")})
}

// TestOntologyDeleteRecheck_RefusesAChangedSubtree: what the guard read is re-asserted
// inside the delete's transaction — a chunk that arrived under the target, a status
// that changed, or a statement written meanwhile refuses the delete.
func TestOntologyDeleteRecheck_RefusesAChangedSubtree(t *testing.T) {
	f := newRetiredFixture(t)
	key := mustTenantKey(t, f.d, f.ctx)
	read := f.read()
	target := f.chunk("Reporting")
	recheck := func(chk *ontologyDeleteCheck, ids []string) string {
		var why string
		if err := f.d.withSqlTxn(f.ctx, key, func(txnID string) error {
			var err error
			why, err = f.d.ontologyDeleteRecheck(f.ctx, txnID, chk, ids)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return why
	}
	refused, chk := f.d.ontologyDeleteGuard(f.ctx, key, store.MemoryScopeTenant, "delete_chunk", read.DocumentID, target)
	if refused != nil || chk == nil {
		t.Fatalf("guard on an unused concept: %v %v", refused, chk)
	}
	if why := recheck(chk, []string{target}); why != "" {
		t.Fatalf("an unchanged subtree was refused: %s", why)
	}
	// An entry arrives under the target.
	moved := f.chunk("Risk Management")
	if why := recheck(chk, []string{target, moved}); why == "" {
		t.Error("a chunk that arrived under the target was not seen")
	}
	// The target's status changes.
	f.retire("Reporting")
	if why := recheck(chk, []string{target}); why == "" {
		t.Error("a changed status was not seen")
	}
	// A statement arrives.
	_, chk = f.d.ontologyDeleteGuard(f.ctx, key, store.MemoryScopeTenant, "delete_chunk", read.DocumentID, f.chunk("filed_under"))
	out := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Q"})
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": out["root_chunk_id"], "to_id": f.chunk("filed_under"), "kind": "related"})
	if why := recheck(chk, []string{f.chunk("filed_under")}); why == "" {
		t.Error("a statement written meanwhile was not seen")
	}
}

// TestDeleteOntologyChunk_RefusesWhenItCannotTellTheDocument: a lookup that fails is
// not "not an ontology chunk".
func TestDeleteOntologyChunk_RefusesWhenItCannotTellTheDocument(t *testing.T) {
	f := newRetiredFixture(t)
	concept := f.chunk("Reporting")
	// The chunk lookup fails while the ontology's own record still reads.
	if err := f.d.exec(f.ctx, mustTenantKey(t, f.d, f.ctx), `ALTER TABLE chunks RENAME TO chunks_gone`); err != nil {
		t.Fatal(err)
	}
	err := f.d.DeleteOntologyChunk(f.ctx, "tenant", concept)
	if err == nil || !strings.Contains(err.Error(), "could not tell which document chunk") {
		t.Errorf("DeleteOntologyChunk with a failing lookup = %v, want it refused at the lookup", err)
	}
}
