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

// TestConfidence_RoundTripsExactlyOnBothTiers — a stored value must read back as the
// value that was stored, on either backend.
//
// `confidence` was declared REAL, which is 8-byte on sqlite and 4-byte float4 on
// postgres. So 0.9 came back as 0.9 from one tier and 0.8999999761581421 from the
// other: the two backends disagreed about a stored number, which is how a shared
// contract quietly stops being one. It also breaks the obvious query — a
// `WHERE confidence >= 0.9` filter silently excluded a row written as exactly 0.9,
// on postgres only.
//
// The sqlite half runs everywhere; the postgres half is skipped without a DSN, and
// it is the half that actually failed.
func TestConfidence_RoundTripsExactlyOnBothTiers(t *testing.T) {
	// 0.9 is not representable in binary floating point, so it is exactly the value
	// that exposes a narrower column. 0.5 would have round-tripped through float4
	// and hidden the bug.
	const want = 0.9

	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertConfidenceRoundTrip(t, d, ctx, want)
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
		if dsn == "" {
			t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres half")
		}
		raw, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		dropAllSqlmemScopes(t, raw)
		mgr, err := sqlmem.NewPostgres(context.Background(),
			sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
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
		ctx := tools.WithAgentName(context.Background(), "pg-confidence")
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
		assertConfidenceRoundTrip(t, d, ctx, want)
	})
}

// TestConfidence_WidensAPreExistingRealColumn covers the scope provisioned BEFORE the
// column was declared DOUBLE PRECISION.
//
// `CREATE TABLE IF NOT EXISTS` leaves an existing table alone, so changing the DDL
// alone fixes only new scopes and leaves older ones on float4 forever — "some scopes
// are float4" being exactly the two-shapes problem the sidecar schema argues against.
// Postgres-only: sqlite's REAL was already 8-byte and it cannot ALTER a column type.
func TestConfidence_WidensAPreExistingRealColumn(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the widening test")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(),
		sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
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
	ctx := tools.WithAgentName(context.Background(), "pg-widen")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})

	// Provision the scope, then put the column BACK to the old narrow type to stand in
	// for a scope created before the fix.
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"widen"}`); r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	key := sidecarScope(t, d, ctx)
	if _, err := mgr.Exec(context.Background(), key,
		`ALTER TABLE chunk_memory_meta ALTER COLUMN confidence TYPE REAL`, nil, 0); err != nil {
		t.Fatalf("stage the old column type: %v", err)
	}
	if got := confidenceColumnType(t, mgr, key); got != "real" {
		t.Fatalf("fixture: column type = %q, want real", got)
	}

	// Any Document op re-runs ensureSchema, which is where the widening lives.
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"trigger"}`); r.IsError {
		t.Fatalf("second create_document: %s", r.Text)
	}

	if got := confidenceColumnType(t, mgr, key); got != "double precision" {
		t.Errorf("confidence column = %q, want double precision — a pre-existing scope keeps losing precision", got)
	}
	// And it round-trips after the widening.
	assertConfidenceRoundTrip(t, d, ctx, 0.9)
}

func assertConfidenceRoundTrip(t *testing.T, d *Document, ctx context.Context, want float64) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"conf"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := asStr(out["document_id"])

	body := `{"op":"upsert_chunk","scope":"user","document_id":"` + docID + `",` +
		`"natural_key":"conf:probe","title":"probe","body":"x","confidence":0.9}`
	if _, r := docExec(t, d, ctx, body); r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}

	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	res, err := d.query(ctx, key,
		`SELECT confidence FROM chunk_memory_meta WHERE natural_key = ?`, "conf:probe")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(res.Rows))
	}
	got := asFloat64Ptr(res.Rows[0][0])
	if got == nil {
		t.Fatal("confidence read back NULL")
	}
	if *got != want {
		t.Errorf("confidence round-tripped as %.17g, want %.17g — the column is narrower than the value", *got, want)
	}
}

func confidenceColumnType(t *testing.T, mgr *sqlmem.Manager, key sqlmem.ScopeKey) string {
	t.Helper()
	res, err := mgr.Query(context.Background(), key,
		`SELECT data_type FROM information_schema.columns
		  WHERE table_schema = current_schema()
		    AND table_name = 'chunk_memory_meta' AND column_name = 'confidence'`, nil)
	if err != nil {
		t.Fatalf("column-type probe: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("no confidence column found")
	}
	return asStr(res.Rows[0][0])
}
