package builtin

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC CX-5 — an empty dossier is a RECORD, and removing it is opt-in.
//
// CV decision 5 leaves a subject document with no live facts after an erasure. The
// decision was to keep it: it records that the entity was once known, deleting it is
// not reversible, and it most often becomes empty as a side effect of an unrelated
// user's erasure. This family exists for the operator who wants a clean entity
// directory anyway, and everything about it is shaped by that: off by default, age
// gated, and refusing any dossier something still points at.

// dossierFixture makes one subject document whose root chunk is the entity node,
// with `facts` fact children under it. It returns the document and root chunk ids.
func dossierFixture(t *testing.T, d *Document, ctx context.Context, subject string, facts int) (string, string) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"`+subject+`",
		"path":"/facts/`+subject+`"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := asStr(out["document_id"])
	root := asStr(out["root_chunk_id"])
	if docID == "" || root == "" {
		t.Fatalf("create_document returned no ids: %v", out)
	}
	// The ROOT is the entity node: a sidecar row is what makes this a dossier rather
	// than an ordinary document, and it is what the prune joins on.
	if err := sidecarInsert(t, d, ctx, root, "person:"+subject); err != nil {
		t.Fatalf("seed entity sidecar: %v", err)
	}
	for i := 0; i < facts; i++ {
		if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`",
			"parent_id":"`+root+`","title":"A fact","body":"Something true."}`); r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
	}
	return docID, root
}

func pruneDossiers(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, dryRun bool) int {
	t.Helper()
	n, _, err := d.PruneEmptyDossiers(ctx, key, store.MemoryScopeUser, 1<<62, dryRun)
	if err != nil {
		t.Fatalf("PruneEmptyDossiers: %v", err)
	}
	return n
}

// TestPruneEmptyDossiers_RemovesAnEmptyOneAndKeepsAPopulatedOne is the base case.
func TestPruneEmptyDossiers_RemovesAnEmptyOneAndKeepsAPopulatedOne(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)

	emptyDoc, _ := dossierFixture(t, d, ctx, "mel", 0)
	fullDoc, _ := dossierFixture(t, d, ctx, "dave", 2)

	if n := pruneDossiers(t, d, ctx, key, false); n != 1 {
		t.Fatalf("pruned %d dossiers, want exactly 1", n)
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, emptyDoc) != 0 {
		t.Error("the empty dossier survived")
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, fullDoc) != 1 {
		t.Error("a dossier with facts in it was removed — the sweep must only take empty ones")
	}
}

// TestPruneEmptyDossiers_KeepsOneAnotherFactStillPointsAt is the case CX-5 names as
// the one that catches a wrong emptiness test, and it is the whole reason the test
// is two-part rather than a child count.
//
// Containment is single-parent, so a relation-fact ("Dave works at the shop") is
// homed under ONE of its subjects and points AT the others with an `about` edge. A
// dossier can therefore have no facts of its own and still be the thing a fact
// elsewhere is about. Deleting it breaks the reader contract that makes that fact
// reachable from every subject it names.
func TestPruneEmptyDossiers_KeepsOneAnotherFactStillPointsAt(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)

	shopDoc, shopRoot := dossierFixture(t, d, ctx, "shop", 0) // no facts of its own
	_, daveRoot := dossierFixture(t, d, ctx, "dave", 1)

	// Dave's relation-fact is ABOUT the shop.
	out, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+shopDoc+`"}`)
	if r.IsError {
		t.Fatalf("get_document: %s %v", r.Text, out)
	}
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+daveRoot+`",
		"to_id":"`+shopRoot+`","kind":"about"}`); r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}

	if n := pruneDossiers(t, d, ctx, key, false); n != 0 {
		t.Errorf("pruned %d dossiers — a dossier something still points at is not empty, "+
			"it is the subject of a fact homed somewhere else", n)
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, shopDoc) != 1 {
		t.Error("the referenced dossier was deleted, orphaning the edge that reached it")
	}
}

// TestPruneEmptyDossiers_KeepsADossierHoldingOnlyRetiredFacts.
//
// A retired child is still a record — `include_retired` exists to read it back — so
// a dossier holding only retired facts is not empty, it is historical. It becomes
// eligible naturally once the retired-content prune reaps those children, which is
// two independently-gated families each doing its own job rather than one family
// reaching past its remit.
func TestPruneEmptyDossiers_KeepsADossierHoldingOnlyRetiredFacts(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	docID, root := dossierFixture(t, d, ctx, "mel", 1)

	var childID string
	res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE document_id = ? AND id <> ?`, docID, root)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("read the child: %v", err)
	}
	childID = asStr(res.Rows[0][0])
	if err := sidecarInsert(t, d, ctx, childID, ""); err != nil {
		t.Fatalf("seed child sidecar: %v", err)
	}
	if err := d.exec(ctx, key,
		`UPDATE chunk_memory_meta SET expired_at = ? WHERE chunk_id = ?`, 1000, childID); err != nil {
		t.Fatalf("retire the child: %v", err)
	}

	if n := pruneDossiers(t, d, ctx, key, false); n != 0 {
		t.Errorf("pruned %d — a dossier whose only facts are RETIRED still holds history "+
			"that include_retired reads; it empties when the content prune reaps them", n)
	}
}

// TestPruneEmptyDossiers_OnlyTakesEntityDocuments: an ordinary empty document is
// someone's prose, not a dossier. Sweeping it because a retention interval elapsed
// would be a different feature with a different argument.
func TestPruneEmptyDossiers_OnlyTakesEntityDocuments(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Scratch","path":"/n/scratch"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	plain := asStr(out["document_id"])

	if n := pruneDossiers(t, d, ctx, key, false); n != 0 {
		t.Errorf("pruned %d — an ordinary document with no entity sidecar is not a dossier", n)
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, plain) != 1 {
		t.Error("an ordinary empty document was deleted")
	}
}

// TestPruneEmptyDossiers_RespectsTheAgeCutoff. "Empty" is a state a dossier passes
// THROUGH — mid-consolidation, or between an erasure and a re-derivation — so a
// sweep landing in that window must not take a record that was about to be filled.
func TestPruneEmptyDossiers_RespectsTheAgeCutoff(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	docID, _ := dossierFixture(t, d, ctx, "mel", 0)

	// A cutoff in the distant past: nothing is old enough.
	n, _, err := d.PruneEmptyDossiers(ctx, key, store.MemoryScopeUser, 1000, false)
	if err != nil {
		t.Fatalf("PruneEmptyDossiers: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d under a cutoff nothing qualifies for", n)
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, docID) != 1 {
		t.Error("a freshly-emptied dossier was taken despite the age gate")
	}
}

// TestPruneEmptyDossiers_DryRunCountsWithoutDeleting, and reports the record so an
// operator can read what a real run would remove.
func TestPruneEmptyDossiers_DryRunCountsWithoutDeleting(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	docID, _ := dossierFixture(t, d, ctx, "mel", 0)

	n, record, err := d.PruneEmptyDossiers(ctx, key, store.MemoryScopeUser, 1<<62, true)
	if err != nil {
		t.Fatalf("PruneEmptyDossiers: %v", err)
	}
	if n != 1 {
		t.Fatalf("dry run reported %d, want 1", n)
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, docID) != 1 {
		t.Error("the dry run deleted the dossier")
	}
	rows, ok := record.([]EmptyDossier)
	if !ok || len(rows) != 1 {
		t.Fatalf("dry run returned no readable record: %#v", record)
	}
	if rows[0].NaturalKey != "person:mel" {
		t.Errorf("the record does not name the entity: %+v — an operator reading the export "+
			"has to be able to tell WHICH entity stopped being known", rows[0])
	}
}

// TestPruneEmptyDossiers_LeavesNoOrphansBehind: the removal goes through the shared
// chunk cascade, so the root chunk's sidecar and body go with the document row.
func TestPruneEmptyDossiers_LeavesNoOrphansBehind(t *testing.T) {
	d, ctx, st := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	docID, root := dossierFixture(t, d, ctx, "mel", 0)

	if n := pruneDossiers(t, d, ctx, key, false); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	for _, probe := range []struct {
		table string
		stmt  string
		arg   string
	}{
		{"documents", `SELECT COUNT(*) FROM documents WHERE id = ?`, docID},
		{"chunks", `SELECT COUNT(*) FROM chunks WHERE id = ?`, root},
		{"chunk_memory_meta", `SELECT COUNT(*) FROM chunk_memory_meta WHERE chunk_id = ?`, root},
		{"chunk_revisions", `SELECT COUNT(*) FROM chunk_revisions WHERE chunk_id = ?`, root},
	} {
		if n := countRows(t, d, ctx, probe.stmt, probe.arg); n != 0 {
			t.Errorf("%s still holds %d row(s) for the removed dossier", probe.table, n)
		}
	}
	if _, err := st.MemoryGet(ctx, direntTenant(ctx), store.MemoryScopeUser, key.ScopeID,
		chunkBodyKey(root)); err == nil {
		t.Error("the root chunk's body survived in the k/v plane")
	}
	// And the Path name, so `ls /facts` does not show an entry that resolves to nothing.
	rows, err := st.DirentListUnder(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), "/")
	if err != nil {
		t.Fatalf("list dirents: %v", err)
	}
	for _, row := range rows {
		if row.Name == "mel" {
			t.Error("the dossier's Path name outlived the document it pointed at")
		}
	}
}

// TestPruneEmptyDossiers_NeverTakesATenantRegistryEntry — RFC CV decision 11.
//
// Two things that are each correct alone compose into an un-adoption.
//
// Adoption mints `person:dave` in the tenant plane and deliberately does NOT backfill
// the facts already learned about that subject (decision 10), so a freshly adopted
// dossier has no children and no inbound edges. That is its DESIGNED state, not the
// end of its life. The empty-dossier sweep walks tenant scopes and would read it as
// garbage — and removing it makes subjectKnownToTenant false again, so the curator
// gate starts refusing placement and the operator's decision is silently reversed by
// a retention interval nobody would connect to it.
//
// Latent while the family ships off by default, and live the moment anyone enables it.
func TestPruneEmptyDossiers_NeverTakesATenantRegistryEntry(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	gctx := grantedTenantCtx(ctx)

	// Built the way ADOPTION builds it: the root IS the entity, carrying the pair plus
	// the key. Seeding it any other way would test a shape the registry never holds.
	out, r := docExec(t, d, gctx, `{"op":"create_document","scope":"tenant","title":"Dave",
		"path":"/facts/dave","type":"person","subject":"Dave","natural_key":"person:dave"}`)
	if r.IsError {
		t.Fatalf("create tenant dossier: %s", r.Text)
	}
	docID := asStr(out["document_id"])
	key, mscope, err := d.resolveScope(gctx, "tenant")
	if err != nil {
		t.Fatalf("resolveScope(tenant): %v", err)
	}

	n, _, err := d.PruneEmptyDossiers(gctx, key, mscope, 1<<62, false)
	if err != nil {
		t.Fatalf("PruneEmptyDossiers(tenant): %v", err)
	}
	if n != 0 {
		t.Errorf("the sweep took %d tenant registry entries — an adopted subject with no "+
			"facts yet is EXACTLY this shape, and removing it un-adopts it", n)
	}
	res, err := d.query(gctx, key, `SELECT COUNT(*) FROM documents WHERE id = ?`, docID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got, _ := asInt64(res.Rows[0][0]); got != 1 {
		t.Error("the adopted subject's registry entry was deleted; the curator gate will " +
			"start refusing placement again and nothing will say why")
	}
}

// TestPruneEmptyDossiers_StillTakesAUserScopeOne pins that the fix above is a carve-out
// for the registry and not a blanket disable. A user's own emptied dossier is what the
// family exists for.
func TestPruneEmptyDossiers_StillTakesAUserScopeOne(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	docID, _ := dossierFixture(t, d, ctx, "mel", 0)

	if n := pruneDossiers(t, d, ctx, key, false); n != 1 {
		t.Fatalf("pruned %d user-scope dossiers, want 1 — the tenant carve-out disabled "+
			"the whole family", n)
	}
	if countRows(t, d, ctx, `SELECT COUNT(*) FROM documents WHERE id = ?`, docID) != 0 {
		t.Error("the user-scope empty dossier survived")
	}
}
