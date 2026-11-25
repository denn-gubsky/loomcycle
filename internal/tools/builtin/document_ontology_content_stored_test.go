package builtin

import (
	"context"
	"fmt"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// RFC GQ P3.2 — the content version stored on every row written under it.

// storedContent reads one row's ontology_content: "" for NULL.
func storedContent(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, table, col, id string) string {
	t.Helper()
	res, err := d.query(ctx, key, `SELECT ontology_content FROM `+table+` WHERE `+col+` = ?`, id)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("read %s.ontology_content of %s: %v (rows %v)", table, id, err, res)
	}
	return asStr(res.Rows[0][0])
}

// currentContentVersion is the content version in force for ctx's tenant; it fails the
// test when there is none, since every assertion here compares against it.
func currentContentVersion(t *testing.T, d *Document, ctx context.Context) string {
	t.Helper()
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || len(read.ContentVersion) != ontologyContentVersionLen {
		t.Fatalf("no content version in force: ok=%v %q", ok, read.ContentVersion)
	}
	return read.ContentVersion
}

// rewindOntologyContent takes a scope back to before the column existed, as a scope
// written by an older release is: the column is dropped from every table and the
// process forgets it provisioned the scope, so the next op migrates it.
func rewindOntologyContent(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey) {
	t.Helper()
	for _, table := range ontologyContentTables {
		if !d.tableHasColumn(ctx, key, table, "ontology_content") {
			continue // never added: already the older shape, which a test then catches
		}
		if err := d.exec(ctx, key, `ALTER TABLE `+table+` DROP COLUMN ontology_content`); err != nil {
			t.Skipf("this tier cannot drop a column: %v", err)
		}
		if d.tableHasColumn(ctx, key, table, "ontology_content") {
			t.Fatalf("%s still has ontology_content — the test cannot reach the state it is for", table)
		}
	}
	d.docSQL().ForgetEnsured(key)
}

// TestContentVersion_StampedOnStatementFactAndAnnotation: every writer the plan names
// stamps the content version in force — a fact, a supersede's seeded row, a statement,
// and an annotation of each level (branch, concept, folder) — and a fact written while
// the tenant has no ontology document is NULL, never a made-up version.
func TestContentVersion_StampedOnStatementFactAndAnnotation(t *testing.T) {
	d, base, _ := documentFixture(t)
	userKeyOf := func(ctx context.Context) sqlmem.ScopeKey { return sidecarScope(t, d, ctx) }

	// The minimal pair: before the ontology exists the same write stores NULL.
	bareDoc, _ := newUserDoc(t, d, base)
	bare, r := docExec(t, d, base, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"title":"a","natural_key":"k:a","body":"x"}`, bareDoc))
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	if got := storedContent(t, d, base, userKeyOf(base), "chunk_memory_meta", "chunk_id", bare["id"].(string)); got != "" {
		t.Errorf("a fact written with no ontology document has ontology_content %q, want NULL", got)
	}

	f := newConceptFixture(t, d, base)
	op := f.operator
	want := currentContentVersion(t, d, op)
	tkey := mustTenantKey(t, d, op)
	ukey := userKeyOf(op)

	// A fact, and the row a supersede seeds for a chunk that had none.
	userDoc, _ := newUserDoc(t, d, op)
	fact, r := docExec(t, d, op, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"title":"b","natural_key":"k:b","body":"x"}`, userDoc))
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	plain, _ := docExec(t, d, op, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"old","body":"x"}`, userDoc))
	repl, _ := docExec(t, d, op, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"new","body":"y"}`, userDoc))
	if _, r = docExec(t, d, op, fmt.Sprintf(`{"op":"supersede_chunk","scope":"user","id":%q,"supersedes_id":%q}`, repl["id"], plain["id"])); r.IsError {
		t.Fatalf("supersede: %s", r.Text)
	}

	// A statement, and an annotation of each writer.
	doc := f.doc("tenant", "/kb/policies/a")
	from, to := f.chunk("tenant", doc), f.chunk("tenant", doc)
	link, r := docExec(t, d, op, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"related"}`, from, to))
	if r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}
	concept := f.concepts["topics/reporting"]
	var folderChunk string
	for _, q := range []string{
		fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"level":"branch","brief":"b","abstract":"a","manual":true}`, from),
		fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"level":"concept","brief":"b","abstract":"a","manual":true}`, concept),
		`{"op":"set_annotation","scope":"tenant","level":"path","path":"/kb/policies","brief":"b","abstract":"a","manual":true}`,
	} {
		out, r := docExec(t, d, op, q)
		if r.IsError {
			t.Fatalf("%s: %s", q, r.Text)
		}
		if a, _ := out["annotation"].(map[string]any); a["level"] == annotationLevelPath {
			folderChunk, _ = a["chunk_id"].(string)
		}
	}
	if folderChunk == "" {
		t.Fatal("the folder annotation did not name its record's chunk")
	}

	for name, got := range map[string]string{
		"fact":               storedContent(t, d, op, ukey, "chunk_memory_meta", "chunk_id", fact["id"].(string)),
		"supersede seed":     storedContent(t, d, op, ukey, "chunk_memory_meta", "chunk_id", plain["id"].(string)),
		"statement":          storedContent(t, d, op, tkey, "chunk_edges", "id", link["id"].(string)),
		"branch annotation":  storedContent(t, d, op, tkey, "chunk_annotations", "chunk_id", from),
		"concept annotation": storedContent(t, d, op, tkey, "chunk_annotations", "chunk_id", concept),
		"folder annotation":  storedContent(t, d, op, tkey, "chunk_annotations", "chunk_id", folderChunk),
	} {
		if got != want {
			t.Errorf("%s: ontology_content = %q, want the version in force %q", name, got, want)
		}
	}
}

// TestContentVersion_StampedOnMeasurePoints: the built-in series stamp each point they
// append with the content version it was computed under.
func TestContentVersion_StampedOnMeasurePoints(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newMeasureCorpus(t, d, base, conceptOntologyMD)
	k.doc(t, day(2026, 6, 3), k.rep)
	k.compute(t)
	want := currentContentVersion(t, d, k.f.operator)
	key, _, err := d.ontologyTenantKey(k.f.operator)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(k.f.operator, key, `SELECT coalesce(ontology_content, ''), count(*) FROM measure_points GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || asStr(res.Rows[0][0]) != want || asInt(res.Rows[0][1]) == 0 {
		t.Errorf("measure_points by ontology_content = %v, want every point stamped %s", res.Rows, want)
	}
}

// TestContentVersion_UpsertOfAnExistingFactKeepsTheColumn: a fact written by an older
// release (its scope has no column) is upserted after the upgrade. The upsert deletes
// and re-inserts the sidecar row with a fixed column list, so a column missing from that
// list would be cleared on every write; here it is stamped, and what the fact carried
// before (its key, its source span) is kept.
func TestContentVersion_UpsertOfAnExistingFactKeepsTheColumn(t *testing.T) {
	statementsBothTiers(t, upsertOfAnExistingFactKeepsTheColumn)
}

func upsertOfAnExistingFactKeepsTheColumn(t *testing.T, d *Document, base context.Context) {
	op := installTenantOntology(t, d, base, typedOntologyMD, memrank.OntologyConfirmedStatus)
	want := currentContentVersion(t, d, op)
	docID, _ := newUserDoc(t, d, op)
	write := func(body string) string {
		t.Helper()
		out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"title":"a","natural_key":"k:old","body":%q}`, docID, body))
		if r.IsError {
			t.Fatalf("upsert: %s", r.Text)
		}
		return out["id"].(string)
	}
	id := write("first")
	key := sidecarScope(t, d, op)
	if err := d.exec(op, key, `UPDATE chunk_memory_meta SET source_quote = 'the span' WHERE chunk_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	rewindOntologyContent(t, d, op, key)

	if again := write("second"); again != id {
		t.Fatalf("the upsert made a second fact %s, want %s", again, id)
	}
	if got := storedContent(t, d, op, key, "chunk_memory_meta", "chunk_id", id); got != want {
		t.Errorf("after the upsert ontology_content = %q, want %q", got, want)
	}
	res, err := d.query(op, key, `SELECT natural_key, source_quote FROM chunk_memory_meta WHERE chunk_id = ?`, id)
	if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "k:old" || asStr(res.Rows[0][1]) != "the span" {
		t.Errorf("the upsert lost what the fact carried: %v %v", res, err)
	}
}

// TestContentVersion_RowsFromBeforeReadAsUnknown: facts and statements written before
// the column existed are counted as unknown — never as older — beside the ones written
// under the version in force (current) and under another one (other).
func TestContentVersion_RowsFromBeforeReadAsUnknown(t *testing.T) {
	statementsBothTiers(t, rowsFromBeforeReadAsUnknown)
}

func rowsFromBeforeReadAsUnknown(t *testing.T, d *Document, base context.Context) {
	op := installTenantOntology(t, d, base, typedOntologyMD, memrank.OntologyConfirmedStatus)
	want := currentContentVersion(t, d, op)
	docID, _ := newUserDoc(t, d, op)
	fact := func(n string) string {
		t.Helper()
		out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"title":%q,"natural_key":"k:%s","body":"x"}`, docID, n, n))
		if r.IsError {
			t.Fatalf("upsert %s: %s", n, r.Text)
		}
		return out["id"].(string)
	}
	link := func(from, to string) string {
		t.Helper()
		out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"related"}`, from, to))
		if r.IsError {
			t.Fatalf("link_chunks: %s", r.Text)
		}
		return out["id"].(string)
	}
	// Written by the older release: two facts and a statement between them.
	old1, old2 := fact("old1"), fact("old2")
	link(old1, old2)
	key := sidecarScope(t, d, op)
	rewindOntologyContent(t, d, op, key)

	// Written after the upgrade: one under the version in force, one under another.
	cur, other := fact("cur"), fact("other")
	link(cur, other)
	otherLink := link(other, cur)
	if err := d.exec(op, key, `UPDATE chunk_memory_meta SET ontology_content = 'ffffffffffffffff' WHERE chunk_id = ?`, other); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(op, key, `UPDATE chunk_edges SET ontology_content = 'ffffffffffffffff' WHERE id = ?`, otherLink); err != nil {
		t.Fatal(err)
	}

	u, err := d.OntologyContentUsageIn(op, "user", want)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ContentCount{Current: 1, Other: 1, Unknown: 2}); u.Facts != want {
		t.Errorf("facts = %+v, want %+v", u.Facts, want)
	}
	if want := (ContentCount{Current: 1, Other: 1, Unknown: 1}); u.Statements != want {
		t.Errorf("statements = %+v, want %+v", u.Statements, want)
	}
	if _, err := d.OntologyContentUsageIn(op, "user", ""); err == nil {
		t.Error("counting against no version answered; with no version no row is current")
	}
}

// TestContentVersion_MigrationAddsTheColumnToAnExistingScope: a scope provisioned before
// the column gets it on every table on its next op, on both tiers, with the rows already
// there left NULL.
func TestContentVersion_MigrationAddsTheColumnToAnExistingScope(t *testing.T) {
	run := func(t *testing.T, d *Document, base context.Context) {
		op := installTenantOntology(t, d, base, typedOntologyMD, memrank.OntologyConfirmedStatus)
		docID, _ := newUserDoc(t, d, op)
		out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"title":"a","natural_key":"k:a","body":"x"}`, docID))
		if r.IsError {
			t.Fatalf("upsert: %s", r.Text)
		}
		key := sidecarScope(t, d, op)
		rewindOntologyContent(t, d, op, key)

		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"get_document","scope":"user","id":%q}`, docID)); r.IsError {
			t.Fatalf("get_document: %s", r.Text)
		}
		for _, table := range ontologyContentTables {
			if !d.tableHasColumn(op, key, table, "ontology_content") {
				t.Errorf("%s has no ontology_content after the scope's next op", table)
			}
		}
		if got := storedContent(t, d, op, key, "chunk_memory_meta", "chunk_id", out["id"].(string)); got != "" {
			t.Errorf("a row from before the column reads %q, want NULL", got)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		d, base, _ := documentFixture(t)
		run(t, d, base)
	})
	t.Run("postgres", func(t *testing.T) {
		d, base := pgDocumentOrSkip(t)
		run(t, d, base)
	})
}

// TestContentVersion_StatementCopiesCarryTheStoredValue: a statement's copy — a
// reimport's revision of it, a merge's re-pointed copy — is the same statement, so it
// carries the content version its source was written under, as it carries
// ontology_version, rather than being stamped with the one in force at the copy.
func TestContentVersion_StatementCopiesCarryTheStoredValue(t *testing.T) {
	const stored = "0123456789abcdef"
	t.Run("reimport", func(t *testing.T) {
		statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
			key := userKey(t, d, ctx)
			_, v1 := rulingFixture(t, d, ctx, "law:ua/content-1")
			held := seedFromArticle1(t, d, ctx, v1, v1["article-3"].id, "implements", edgeStatusAsserted, "Two and Three")
			if err := d.exec(ctx, key, `UPDATE chunk_edges SET ontology_content = ? WHERE id = ?`, stored, held); err != nil {
				t.Fatal(err)
			}
			reimport(t, d, ctx, rulingEd2, "law:ua/content-1", 2_000_000)
			copies := copiesOf(t, d, ctx, key, held)
			if len(copies) != 1 {
				t.Fatalf("copies of the held statement = %d, want 1", len(copies))
			}
			if got := storedContent(t, d, ctx, key, "chunk_edges", "id", copies[0].ID); got != stored {
				t.Errorf("the reimport's copy has ontology_content %q, want its source's %q", got, stored)
			}
		})
	})
	t.Run("merge", func(t *testing.T) {
		f := newMergeFixture(t)
		key := mustTenantKey(t, f.d, f.ctx)
		if err := f.d.exec(f.ctx, key, `UPDATE chunk_edges SET ontology_content = ? WHERE id = ?`, stored, f.cites); err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.MergeSubjects(f.ctx, "tenant", "person:ada-king", f.survDoc); err != nil {
			t.Fatalf("merge: %v", err)
		}
		cp, err := f.d.statementByKey(f.ctx, key, f.reportChunk, f.survRoot, "cites", nil, "")
		if err != nil || cp == nil {
			t.Fatalf("no copy on the survivor: %v", err)
		}
		if got := storedContent(t, f.d, f.ctx, key, "chunk_edges", "id", cp.ID); got != stored {
			t.Errorf("the merge's copy has ontology_content %q, want its source's %q", got, stored)
		}
	})
}
