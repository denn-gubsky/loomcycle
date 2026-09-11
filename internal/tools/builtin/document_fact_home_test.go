package builtin

import (
	"context"
	"database/sql"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"os"
)

// The end-to-end behaviour of subject homing is pinned in internal/api/http, through
// the endpoint an operator actually calls. THESE exist for the tier: the migration
// rewrites document_id and parent_id directly, and the postgres tier is where a
// portability difference would hide — a count returned as int64, a NULL parent, a
// statement the rebind mangles. The sqlite half runs everywhere; the postgres half is
// on the allowlist the pg job runs.

// homedShape reports where every chunk ended up, as id -> (document, parent).
func homedShape(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey) map[string][2]string {
	t.Helper()
	res, err := d.query(ctx, key, `SELECT id, document_id, coalesce(parent_id, '') FROM chunks`)
	if err != nil {
		t.Fatalf("shape: %v", err)
	}
	out := map[string][2]string{}
	for _, row := range res.Rows {
		out[asStr(row[0])] = [2]string{asStr(row[1]), asStr(row[2])}
	}
	return out
}

// seedOldShape writes the pre-subject-homing layout: one shared document holding a
// subject node, a fact about it, and a fact about nobody.
func seedOldShape(t *testing.T, d *Document, ctx context.Context) (shared, subj, fact, orphan string) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Entities","path":"/memory/entities"}`)
	if r.IsError {
		t.Fatalf("create shared: %s", r.Text)
	}
	shared, _ = out["document_id"].(string)
	sub, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+shared+
		`","natural_key":"person:denn","title":"Denn","type":"person","subject":"Denn"}`)
	if r.IsError {
		t.Fatalf("subject: %s", r.Text)
	}
	subj, _ = sub["id"].(string)
	f, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+shared+
		`","natural_key":"memory/fact/denn-writes-go","title":"Denn writes Go.","body":"Denn writes Go.","type":"fact"}`)
	if r.IsError {
		t.Fatalf("fact: %s", r.Text)
	}
	fact, _ = f["id"].(string)
	o, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+shared+
		`","natural_key":"memory/fact/releases-friday","title":"Releases ship Friday.","body":"Releases ship on Friday.","type":"fact"}`)
	if r.IsError {
		t.Fatalf("orphan fact: %s", r.Text)
	}
	orphan, _ = o["id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+fact+
		`","to_id":"`+subj+`","kind":"about"}`); r.IsError {
		t.Fatalf("about edge: %s", r.Text)
	}
	return shared, subj, fact, orphan
}

// assertHomed is the postcondition, shared by both tiers: the subject IS the new
// document's root (same chunk, so the unique key never moves and the edge never has to
// be re-pointed), its fact is its child, and the fact about nobody stays where it was.
func assertHomed(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey,
	rep FactHomingReport, shared, subj, fact, orphan string) {
	t.Helper()
	if rep.SubjectsHomed != 1 || rep.FactsMoved != 1 || rep.DocumentsCreated != 1 {
		t.Fatalf("report = %+v, want one subject homed into one new document", rep)
	}
	if rep.FactsWithoutSubject != 1 {
		t.Errorf("facts_without_subject = %d, want 1", rep.FactsWithoutSubject)
	}
	shape := homedShape(t, d, ctx, key)
	subjAt, ok := shape[subj]
	if !ok {
		t.Fatalf("the subject node is gone — it must MOVE, not be re-created")
	}
	if subjAt[0] == shared {
		t.Error("the subject node did not leave the shared document")
	}
	if subjAt[1] != "" {
		t.Errorf("the subject's parent is %q, want none — it is a document root now", subjAt[1])
	}
	if got := shape[fact]; got[0] != subjAt[0] || got[1] != subj {
		t.Errorf("fact sits at %v, want document %q under the subject %q", got, subjAt[0], subj)
	}
	if got := shape[orphan]; got[0] != shared {
		t.Errorf("the subject-less fact moved to %q — it has no subject to be homed under", got[0])
	}
	// The documents row agrees: root_chunk_id is the subject, not a leftover.
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, subjAt[0])
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if len(res.Rows) != 1 || asStr(res.Rows[0][0]) != subj {
		t.Errorf("documents.root_chunk_id = %v, want the subject node %q", res.Rows, subj)
	}
	// And the key still has exactly one holder, which is the invariant that makes
	// moving the node the only workable implementation.
	kres, err := d.query(ctx, key, `SELECT chunk_id FROM chunk_memory_meta WHERE natural_key = ?`, "person:denn")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(kres.Rows) != 1 || asStr(kres.Rows[0][0]) != subj {
		t.Errorf("chunks claiming person:denn = %v, want only %q", kres.Rows, subj)
	}
}

func TestHomeFactsUnderSubjects_MovesTheSubjectAndItsFacts(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	shared, subj, fact, orphan := seedOldShape(t, d, ctx)

	rep, err := d.HomeFactsUnderSubjects(ctx, "tnt", store.MemoryScopeUser, "u1", false)
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	assertHomed(t, d, ctx, sidecarScope(t, d, ctx), rep, shared, subj, fact, orphan)
}

// TestHomeFactsUnderSubjects_PostgresTierParity runs the same postconditions against
// postgres. The migration is raw UPDATEs over document_id/parent_id and a count read
// back as an integer — all places the two tiers can differ, and none of them covered
// by the sqlite fixture.
func TestHomeFactsUnderSubjects_PostgresTierParity(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the subject-homing postgres-tier parity test")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "pg-homing")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})

	shared, subj, fact, orphan := seedOldShape(t, d, ctx)
	rep, err := d.HomeFactsUnderSubjects(ctx, "tnt", store.MemoryScopeUser, "u1", false)
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	assertHomed(t, d, ctx, sidecarScope(t, d, ctx), rep, shared, subj, fact, orphan)
}
