package builtin

import (
	"context"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The #2762 review findings: the ways around a lock that the first cut left open.

// TestOntology_SectionReReadOfALockedSubtreeIsRefused: a body-only edit of a section
// header that changes how the pre-pass reads it — an unknown kind takes its entries out
// of force, another kind reads them as that section's — is refused while the section
// holds a locked entry. An edit that keeps the reading, and one of an unlocked
// section, go through.
func TestOntology_SectionReReadOfALockedSubtreeIsRefused(t *testing.T) {
	f := newLockedFixture(t)
	relations := f.chunk("Relations")
	for _, body := range []string{"@section bogus", "@section instances", "Kinds of relation, no longer a section."} {
		wantLocked(t, "header body "+body, f.update(f.ctx, relations, map[string]any{"body": body}), `relation "co_supervises"`)
	}
	if r := f.read(); r.SectionChunks[memrank.SectionRelations] != relations {
		t.Fatalf("the Relations section moved: %v", r.SectionChunks)
	}
	if text := f.update(f.ctx, relations, map[string]any{"body": "@section relations\nHow things relate."}); text != "" {
		t.Errorf("an edit keeping the section's reading was refused: %s", text)
	}
	// The same edit of a section with no lock in it is allowed.
	if text := f.update(f.ctx, looseTopLevelClass(t, f), map[string]any{"body": "@section bogus"}); text != "" {
		t.Errorf("re-reading a section with no locked entry was refused: %s", text)
	}
}

// looseTopLevelClass adds a top-level class with nothing locked under it and returns
// its chunk: a root child the pre-pass reads as a class.
func looseTopLevelClass(t *testing.T, f *retiredFixture) string {
	t.Helper()
	read := f.read()
	out := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.RootChunkID, "title": "Loose", "body": "A class outside the sections."})
	return out["id"].(string)
}

// TestUpsertChunk_OverAnOntologyEntryIsRefusedForEveryCaller: an upsert by natural key
// rewrites a chunk past update_chunk's checks, so over an entry of the ontology it is
// refused for the operator and an admin too; a proposal may still be re-filed.
func TestUpsertChunk_OverAnOntologyEntryIsRefusedForEveryCaller(t *testing.T) {
	f := newLockedFixture(t)
	read := f.read()
	classes := read.SectionChunks[memrank.SectionClasses]
	f.must(map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": read.DocumentID, "parent_id": classes,
		"natural_key": "class:ledger", "title": "Ledger", "body": "@locked\nA ledger."})
	for name, ctx := range map[string]context.Context{"operator": f.ctx, "admin": adminCtx(f.ctx)} {
		_, r := f.do(ctx, map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": read.DocumentID,
			"natural_key": "class:ledger", "title": "Journal", "body": "A journal."})
		if !r.IsError || !strings.Contains(refusalText(r), "update_chunk") {
			t.Errorf("%s upsert over an ontology entry = %q, want refused pointing at update_chunk", name, refusalText(r))
		}
	}
	if term, ok := termByName(f.read(), "Ledger"); !ok || !term.Locked {
		t.Errorf("the entry changed under the refused upsert: %+v", term)
	}
	f.must(map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": read.DocumentID, "parent_id": classes,
		"natural_key": "proposal:note", "title": "Note", "status": memrank.OntologyStatusProposed})
	f.must(map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"natural_key": "proposal:note", "title": "Note", "status": memrank.OntologyStatusProposed, "body": "Refiled."})
}

// TestOntologyLockGuards_FailClosedOnAPartialRead: when a chunk body cannot be read the
// reader reads it as empty, so a locked entry would pass as unlocked. Every lock guard
// refuses instead.
func TestOntologyLockGuards_FailClosedOnAPartialRead(t *testing.T) {
	f := newLockedFixture(t)
	reporting, internal, co := f.chunk("Reporting"), f.chunk("Internal Policy"), f.chunk("co_supervises")
	riskMgmt, policy := f.chunk("Risk Management"), f.chunk("Policy")
	revOf := func(id string) int {
		return int(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["revision"].(float64))
	}
	revInternal, revReporting := revOf(internal), revOf(reporting)
	fs := &failingBodyStore{Store: f.d.Store, fail: true}
	d2 := &Document{Store: fs, SqlMem: f.d.SqlMem, Bus: f.d.Bus}
	run := func(ctx context.Context, v map[string]any) string {
		t.Helper()
		fx := &retiredFixture{t: t, d: d2, ctx: f.ctx}
		_, r := fx.do(ctx, v)
		return refusalText(r)
	}
	for what, v := range map[string]map[string]any{
		"delete": {"op": "delete_chunk", "scope": "tenant", "id": co},
		"move":   {"op": "move_chunk", "scope": "tenant", "id": reporting, "new_parent_id": riskMgmt},
		"retire": {"op": "update_chunk", "scope": "tenant", "id": internal, "revision": revInternal, "status": memrank.OntologyStatusRetired},
		"rename": {"op": "update_chunk", "scope": "tenant", "id": reporting, "revision": revReporting, "title": "Disclosure"},
	} {
		if text := run(f.ctx, v); !strings.Contains(text, entryLockedCode) || !strings.Contains(text, "could not be read") {
			t.Errorf("%s on a partial read = %q, want an entry_locked refusal that names the failed read", what, text)
		}
	}
	agent := tools.WithCallerTier(inRunCtx(f.ctx), tools.TierRun)
	if text := run(agent, map[string]any{"op": "propose_entity", "change": "retire", "entry": policy}); !strings.Contains(text, "could not be read") {
		t.Errorf("a retire proposal on a partial read = %q, want refused", text)
	}
	fs.fail = false
	if !f.exists(co) || f.status(internal) != "" || f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": reporting})["title"] != "Reporting" {
		t.Error("an ontology entry changed under a partial read")
	}
}

// TestOntology_SecondClassOfTheSameNameKeepsItsLock: two in-force classes may share a
// name; each is its own entry, so a lock on the second one holds.
func TestOntology_SecondClassOfTheSameNameKeepsItsLock(t *testing.T) {
	md := strings.Replace(retiredOntologyMD, "### Provision\n\n", "### Provision\n\n#### Memo\n@locked\n\n", 1)
	d, base, _ := documentFixture(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	res, err := f.d.query(f.ctx, mustTenantKey(t, f.d, f.ctx), `SELECT id FROM chunks WHERE document_id = ? AND title = 'Memo'`, f.read().DocumentID)
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("want two Memo chunks: %v %v", err, res)
	}
	locked := ""
	for _, r := range res.Rows {
		if id := asStr(r[0]); memrank.BodyLocked(f.body(id)) {
			locked = id
		}
	}
	wantLocked(t, "delete the locked Memo", f.deleteChunk(f.ctx, locked), `class "Memo" carries @locked`)
	wantLocked(t, "retire the locked Memo", f.setStatus(f.ctx, locked, memrank.OntologyStatusRetired), `class "Memo"`)
	if !f.exists(locked) {
		t.Error("the locked class of a shared name was deleted")
	}
}
