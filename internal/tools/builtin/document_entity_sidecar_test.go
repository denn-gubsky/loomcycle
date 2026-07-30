package builtin

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ---- helpers ----
//
// These reach the scope's SQL database through the Document's OWN sqlmem manager
// and resolved ScopeKey, so a test writes to exactly the database the tool writes
// to. query_chunks cannot be used for the writes: it is read-only by design
// (SELECT / WITH ... SELECT only, validator-gated), which is the correct posture
// for a model-facing escape hatch and the reason the writes go around it here.

func sidecarScope(t *testing.T, d *Document, ctx context.Context) sqlmem.ScopeKey {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	return key
}

// sidecarInsert writes one sidecar row. An EMPTY naturalKey is bound as SQL NULL,
// which is the case that matters most: almost every chunk leaves the column unset,
// so NULLs must not collide under the unique index.
func sidecarInsert(t *testing.T, d *Document, ctx context.Context, chunkID, naturalKey string) error {
	t.Helper()
	var nk any
	if naturalKey != "" {
		nk = naturalKey
	}
	// Rebind is the portable seam: pgx does NOT accept `?`, so a helper that writes
	// its own SQL has to go through it exactly as the tool does. Skipping it passed
	// on sqlite and failed on postgres with a bare syntax error — which is what the
	// parity test below exists to catch.
	stmt := d.SqlMem.Rebind(
		`INSERT INTO chunk_memory_meta (chunk_id, valid_at, created_at, class, origin, natural_key)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	_, err := d.SqlMem.Exec(ctx, sidecarScope(t, d, ctx), stmt,
		[]any{chunkID, int64(1), int64(1), "derived", "consolidator", nk}, 0)
	return err
}

// sidecarRowsFor counts sidecar rows for ONE chunk id. Preferred over a whole-table
// count when a test also seeds rows with invented chunk ids: those are legitimately
// untouched by a cascade that walks real chunks.
func sidecarRowsFor(t *testing.T, d *Document, ctx context.Context, chunkID string) int {
	t.Helper()
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT count(*) FROM chunk_memory_meta WHERE chunk_id = ?`), []any{chunkID})
	if err != nil {
		t.Fatalf("count for %s: %v", chunkID, err)
	}
	return scanCount(res.Rows)
}

func sidecarCount(t *testing.T, d *Document, ctx context.Context) int {
	t.Helper()
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx), `SELECT count(*) FROM chunk_memory_meta`, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return scanCount(res.Rows)
}

// scanCount normalizes a COUNT(*) cell across tiers — sqlite-under-modernc and
// postgres-under-pgx do not agree on whether it arrives as an integer or text.
func scanCount(rows [][]any) int {
	if len(rows) == 0 {
		return 0
	}
	switch v := rows[0][0].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case string:
		n := 0
		for _, c := range v {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		return n
	}
	return -1
}

// The entity-tier sidecar (RFC BL P4c PR1). No entity semantics yet — this PR is
// the schema, the two indexes and the cascade, so it is exercised through the
// scope's own SQL surface rather than through a new Document op.

// TestSidecar_CreatedUnconditionally: the table exists as soon as a scope has any
// document at all. Operator decision — the tier is opt-in at the level of "does
// this scope have entities", never at the level of "does the table exist", because
// a gated DDL lets a scope exist in two shapes and forces every later read to ask
// which one it got.
func TestSidecar_CreatedUnconditionally(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// An ordinary document — nothing entity-related requested.
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"plain"}`); r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT count(*) AS n FROM chunk_memory_meta"}`)
	if r.IsError {
		t.Fatalf("chunk_memory_meta should exist for any scope with a document: %s", r.Text)
	}
	if out == nil {
		t.Fatal("no result")
	}
}

// TestSidecar_HasBothTimelinesAndTheNaturalKey pins the columns the entity tier
// depends on. Two timelines is the whole point: valid_at/invalid_at is when a fact
// was true in the WORLD, created_at/expired_at is when the SYSTEM learned and
// retired it. Collapsing them would make "as of June…" unanswerable after a
// correction.
func TestSidecar_HasBothTimelinesAndTheNaturalKey(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x"}`); r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	// Selecting every column by name fails loudly if one is missing or renamed.
	const cols = "chunk_id, valid_at, invalid_at, created_at, expired_at, class, origin, confidence, session_id, run_id, event_seq, natural_key"
	if _, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT `+cols+` FROM chunk_memory_meta"}`); r.IsError {
		t.Fatalf("the sidecar is missing a column the entity tier needs: %s", r.Text)
	}
}

// TestSidecar_NaturalKeyUniquePerScope_ButNullsDoNotCollide is the portability
// assertion behind the operator's "unique per scope" decision.
//
// Each scope owns its own database, so a bare UNIQUE index on the column IS
// scope-wide. The subtlety is that the column is NULLABLE and almost every chunk
// leaves it unset: if a tier treated NULLs as equal, the SECOND ordinary chunk in
// any scope would fail to insert. Both tiers treat them as distinct — asserted
// here rather than trusted, because the failure would be immediate and total.
func TestSidecar_NaturalKeyUniquePerScope_ButNullsDoNotCollide(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x"}`); r.IsError {
		t.Fatalf("create: %s", r.Text)
	}

	// Many NULL natural_keys must coexist.
	for i, cid := range []string{"c1", "c2", "c3"} {
		if err := sidecarInsert(t, d, ctx, cid, ""); err != nil {
			t.Fatalf("NULL natural_key #%d must be allowed (both tiers treat NULLs as distinct): %v", i+1, err)
		}
	}

	// A real key inserts once...
	if err := sidecarInsert(t, d, ctx, "e1", "user:person:ada"); err != nil {
		t.Fatalf("first natural_key insert: %v", err)
	}
	// ...and a SECOND row claiming the same key must be refused. That refusal is
	// what makes upsert-by-natural-key idempotent instead of duplicating entities.
	if err := sidecarInsert(t, d, ctx, "e2", "user:person:ada"); err == nil {
		t.Error("a duplicate natural_key must be refused — without it the same entity accumulates a row per mention")
	}
}

// TestSidecar_CascadesOnChunkDelete + ...OnDocumentDelete cover BOTH cascade
// sites. Both are required and neither substitutes for the other: delete_document
// walks by document, delete_chunk walks one chunk's descendant set. A sidecar row
// reachable only through the path that was missed is an orphan nothing can see —
// no read filters it, no sweeper reaps it.
func TestSidecar_CascadesOnChunkDelete(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x"}`)
	if r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	root, _ := out["root_chunk_id"].(string)
	child, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+asStr(out["document_id"])+`","parent_id":"`+root+`","title":"child"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	cid, _ := child["id"].(string)
	if cid == "" {
		t.Fatalf("no chunk id in %v", child)
	}
	if err := sidecarInsert(t, d, ctx, cid, "user:person:grace"); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+cid+`"}`); r.IsError {
		t.Fatalf("delete_chunk: %s", r.Text)
	}
	if n := sidecarCount(t, d, ctx); n != 0 {
		t.Errorf("delete_chunk left %d orphaned sidecar row(s) — invisible to every read and every sweeper", n)
	}
}

func TestSidecar_CascadesOnDocumentDelete(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x"}`)
	if r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	docID := asStr(out["document_id"])
	root, _ := out["root_chunk_id"].(string)
	if err := sidecarInsert(t, d, ctx, root, "user:person:alan"); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+docID+`"}`); r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}
	if n := sidecarCount(t, d, ctx); n != 0 {
		t.Errorf("delete_document left %d orphaned sidecar row(s)", n)
	}
}

// TestSidecar_ReverseEdgeIndexExists: chunk_edges' primary key (from_id, to_id,
// kind) serves a FORWARD walk only, so without this index every reverse hop of a
// graph expansion scans the scope's whole edge table. It degrades only once the
// graph is big — which is to say, only in production.
func TestSidecar_ReverseEdgeIndexExists(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x"}`); r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	out, r := docExec(t, d, ctx,
		`{"op":"query_chunks","scope":"user","sql":"SELECT name FROM sqlite_master WHERE type='index' AND name='chunk_edges_to_kind'"}`)
	if r.IsError {
		t.Fatalf("index probe: %s", r.Text)
	}
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 {
		t.Errorf("chunk_edges_to_kind index missing — every reverse graph hop would table-scan; got %v", out)
	}
}

// TestSidecar_PostgresTierParity re-runs the two tier-sensitive assertions against
// a REAL postgres aux database.
//
// The NULL-distinctness of a UNIQUE index is a per-tier property, and the whole
// "unique per scope" decision rests on it: if postgres treated NULLs as equal, the
// SECOND ordinary chunk in any scope would fail to insert — an immediate, total
// break that the sqlite tier would never reveal. The reverse-edge index probe is
// also tier-specific (sqlite_master vs pg_indexes), so it is asserted here in its
// postgres form rather than assumed portable.
//
// Skipped without LOOMCYCLE_TEST_SQLMEM_PG_DSN; CI runs it in the go-postgres job.
func TestSidecar_PostgresTierParity(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the sidecar postgres-tier parity test")
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
	ctx := tools.WithAgentName(context.Background(), "pg-sidecar")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"PG sidecar"}`)
	if r.IsError {
		t.Fatalf("create_document(pg): %s", r.Text)
	}
	docID := asStr(out["document_id"])

	// NULLs must remain distinct on this tier too.
	for i, cid := range []string{"c1", "c2", "c3"} {
		if err := sidecarInsert(t, d, ctx, cid, ""); err != nil {
			t.Fatalf("postgres: NULL natural_key #%d must be allowed: %v", i+1, err)
		}
	}
	if err := sidecarInsert(t, d, ctx, "e1", "user:person:ada"); err != nil {
		t.Fatalf("postgres: first natural_key: %v", err)
	}
	if err := sidecarInsert(t, d, ctx, "e2", "user:person:ada"); err == nil {
		t.Error("postgres: a duplicate natural_key must be refused")
	}

	// The reverse-edge index, in its postgres form.
	res, err := mgr.Query(ctx, sidecarScope(t, d, ctx),
		`SELECT indexname FROM pg_indexes WHERE indexname = 'chunk_edges_to_kind'`, nil)
	if err != nil {
		t.Fatalf("postgres index probe: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Error("postgres: chunk_edges_to_kind index missing — every reverse graph hop would seq-scan")
	}

	// And the cascade, on the tier where a real transaction is involved.
	//
	// Asserted against a row keyed to the document's REAL root chunk, not against
	// the table emptying. The rows above carry invented chunk ids that were never
	// attached to a chunk, so the cascade correctly leaves them — a first draft of
	// this test asserted count==0 and "failed" on the cascade doing the right thing.
	root := asStr(out["root_chunk_id"])
	if root == "" {
		t.Fatal("no root chunk id")
	}
	if err := sidecarInsert(t, d, ctx, root, "user:person:grace"); err != nil {
		t.Fatalf("postgres: seed sidecar on the real root chunk: %v", err)
	}
	if n := sidecarRowsFor(t, d, ctx, root); n != 1 {
		t.Fatalf("postgres: seeded row not found (got %d)", n)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+docID+`"}`); r.IsError {
		t.Fatalf("delete_document(pg): %s", r.Text)
	}
	if n := sidecarRowsFor(t, d, ctx, root); n != 0 {
		t.Errorf("postgres: delete_document left the root chunk's sidecar row orphaned (%d)", n)
	}
}
