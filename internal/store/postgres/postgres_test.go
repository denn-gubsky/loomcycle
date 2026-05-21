package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

// pgDSNFromEnv returns the DSN under which the Postgres adapter test
// suite runs, or "" to skip. Set LOOMCYCLE_TEST_PG_DSN in CI / local dev:
//
//	export LOOMCYCLE_TEST_PG_DSN="postgres://loomcycle:loomcycle@127.0.0.1:5432/loomcycle_test?sslmode=disable"
//
// `make pg-up` brings up a matching ephemeral container.
func pgDSNFromEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LOOMCYCLE_TEST_PG_DSN not set; skipping Postgres adapter tests. Run `make pg-up` to start a fixture.")
	}
	return dsn
}

// freshFixture holds everything a per-test Postgres fixture needs: the
// Store, the DSN it opened with (search_path-set, for re-use by tests
// that want to call migration helpers on the same schema), and a cleanup
// closure that drops the per-test schema.
type freshFixture struct {
	store    store.Store
	storeDSN string
	cleanup  func()
}

// freshSchema spins up a per-test temporary schema, applies the
// embedded migrations into it, and returns a Store rooted there. Cleanup
// drops the schema. This isolates each test from concurrent runs without
// requiring per-test databases (which Postgres makes expensive to create
// in a tight loop).
//
// Schema isolation rather than database isolation also means CI/local
// devs only need ONE database (loomcycle_test) provisioned in advance —
// the test creates the per-test schema on the fly via search_path.
func freshSchema(t *testing.T, dsn string) freshFixture {
	return freshSchemaWithVectors(t, dsn, false)
}

// freshSchemaWithVectors is the pgvector-aware variant. Pass
// pgvectorEnabled=true to set Config.PgvectorEnabled (the test
// Postgres must have the pgvector binary installed for this path
// to succeed — see TestStoreContractWithPgvector for the gating
// env var). v0.9.0: lets the Postgres contract tests exercise the
// real vector round-trip path against pgvector instead of the
// refusal path that the default freshSchema covers.
func freshSchemaWithVectors(t *testing.T, dsn string, pgvectorEnabled bool) freshFixture {
	t.Helper()

	schema := uniqueSchemaName(t)

	// Step 1: open a short-lived pool against the public schema to
	// CREATE the per-test schema. We can't use the Store's own pool
	// for this — the Store ties its search_path to the schema we're
	// about to create, which doesn't exist yet.
	bootstrapCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	bootstrapCfg.MaxConns = 2
	bootstrap, err := pgxpool.NewWithConfig(context.Background(), bootstrapCfg)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	defer bootstrap.Close()
	if _, err := bootstrap.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	// Step 2: open the Store's own pool with search_path set so every
	// subsequent statement (including migrations) targets the per-test
	// schema. AutoMigrate=true so the schema gets the v0001 init.
	storeDSN := appendOption(dsn, "search_path", schema)
	s, err := Open(context.Background(), Config{
		DSN:             storeDSN,
		MaxOpenConns:    8,
		MinIdleConns:    0,
		AutoMigrate:     true,
		PgvectorEnabled: pgvectorEnabled,
	})
	if err != nil {
		// Best-effort cleanup on Open failure.
		_, _ = bootstrap.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		t.Fatalf("open postgres: %v", err)
	}

	cleanup := func() {
		_ = s.Close()
		// Re-open a tiny pool to drop the schema; the Store's pool
		// is gone now. CASCADE handles tables, indexes, sequences,
		// and the schema_migrations row.
		dropCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Logf("cleanup parse DSN: %v", err)
			return
		}
		dropCfg.MaxConns = 2
		drop, err := pgxpool.NewWithConfig(context.Background(), dropCfg)
		if err != nil {
			t.Logf("cleanup dial postgres: %v", err)
			return
		}
		defer drop.Close()
		if _, err := drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	}
	return freshFixture{store: s, storeDSN: storeDSN, cleanup: cleanup}
}

// schemaCounter ensures unique schema names within one process even when
// the test name + nanosecond clock collide (rare under -race).
var schemaCounter atomic.Uint64

// uniqueSchemaName builds an identifier safe to splice directly into a
// CREATE SCHEMA statement: lowercase, [a-z0-9_], length-bounded.
//
// Postgres identifiers are 63 bytes max. We aim for ~50 to leave room
// for any future suffix.
func uniqueSchemaName(t *testing.T) string {
	n := schemaCounter.Add(1)
	// Replace anything not in [a-z0-9] with '_'.
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, t.Name())
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return fmt.Sprintf("lct_%s_%d", clean, n)
}

// appendOption splices `key=value` into the query string of a libpq DSN.
// pgxpool.ParseConfig handles both URL-form (`postgres://...?key=val`)
// and key=value-form ("host=... port=...") DSNs; we use a tiny string
// shim instead of round-tripping through the parsed config because
// pgxpool's ConnConfig.RuntimeParams isn't reflected in MaxConns/etc
// the same way after ParseConfig.
func appendOption(dsn, key, value string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	if strings.Contains(dsn, "://") {
		return dsn + sep + key + "=" + value
	}
	// keyword=value DSN
	return dsn + " " + key + "=" + value
}

// TestStoreContract runs the shared behavioural test suite against the
// Postgres adapter. Identical surface to the SQLite contract test —
// drift between the two adapters surfaces as a failed sub-test on
// whichever side regressed.
//
// v0.9.0: this variant runs with PgvectorEnabled=false, so the
// Vector Memory contract tests exercise the refusal path. The
// pgvector round-trip path is covered by TestStoreContractWithPgvector
// below, which requires LOOMCYCLE_TEST_PG_VECTOR=1 + a pgvector-
// installed Postgres.
func TestStoreContract(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	storetest.Run(t, func(t *testing.T) (store.Store, func()) {
		fix := freshSchema(t, dsn)
		return fix.store, fix.cleanup
	})
}

// TestStoreContractWithPgvector runs the same shared suite against
// Postgres with PgvectorEnabled=true. The vector contract tests
// take the round-trip path here — set + search + ordering +
// dimension-mismatch + CASCADE.
//
// Requires:
//   - LOOMCYCLE_TEST_PG_DSN set (same as the base suite)
//   - LOOMCYCLE_TEST_PG_VECTOR=1 (opt-in flag)
//   - pgvector binary installed on the test Postgres
//     (pgvector/pgvector docker image is the easy path)
//
// Without LOOMCYCLE_TEST_PG_VECTOR, this test skips and the
// refusal-path coverage in TestStoreContract is all you get.
func TestStoreContractWithPgvector(t *testing.T) {
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; skipping pgvector round-trip contract tests")
	}
	dsn := pgDSNFromEnv(t)
	storetest.Run(t, func(t *testing.T) (store.Store, func()) {
		fix := freshSchemaWithVectors(t, dsn, true)
		return fix.store, fix.cleanup
	})
}

// ---- Postgres-specific tests below this line ----

// AppendEvent under burst load — 64 goroutines × 100 events each = 6,400
// concurrent inserts. Every event must land with a unique ascending seq
// and no FK violations. SQLite SQLITE_BUSYs through this scenario;
// Postgres should sail through.
func TestAppendEventConcurrent(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	s := fix.store

	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "burst", "u")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_burst"})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 64
	const perGoroutine = 100
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				payload := []byte(fmt.Sprintf(`{"g":%d,"i":%d}`, g, i))
				if err := s.AppendEvent(ctx, run.ID, "burst", payload); err != nil {
					errCh <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("append failed: %v", err)
		}
	}

	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != goroutines*perGoroutine {
		t.Fatalf("transcript len = %d, want %d", len(transcript), goroutines*perGoroutine)
	}
	// Strictly ascending seq, no duplicates.
	for i := 1; i < len(transcript); i++ {
		if transcript[i].Seq <= transcript[i-1].Seq {
			t.Errorf("seq not ascending at %d: %d -> %d", i, transcript[i-1].Seq, transcript[i].Seq)
			break
		}
	}
}

// TestOpen_RejectsKeywordValueDSN asserts that a libpq keyword=value
// DSN is rejected with a clear message — golang-migrate's pgx5 driver
// only registers for URL-form DSNs, so without this guard the operator
// sees a confusing "unknown driver" error from deep inside
// golang-migrate when the embedded migrations are checked.
//
// EMPIRICAL: removing the !strings.Contains("://") check in postgres.go's
// Open() makes this test fail (Open returns the migrate-init error
// from golang-migrate instead of the upfront refusal).
func TestOpen_RejectsKeywordValueDSN(t *testing.T) {
	// We don't even need the live PG fixture here — Open returns the
	// DSN-form error before dialing.
	_, err := Open(context.Background(), Config{
		DSN: "host=localhost user=loomcycle dbname=loomcycle",
	})
	if err == nil {
		t.Fatal("Open should reject keyword=value DSN; got nil error")
	}
	if !strings.Contains(err.Error(), "URL form") {
		t.Errorf("error doesn't mention URL form: %v", err)
	}
}

// TestVerifySchemaCurrent_FreshDB asserts that opening a Postgres with no
// schema applied AND AutoMigrate=false returns a clear error message
// pointing at the fix (run loomcycle migrate up). Without this, a misconfig
// surfaces as random "table does not exist" runtime errors.
func TestVerifySchemaCurrent_FreshDB(t *testing.T) {
	dsn := pgDSNFromEnv(t)

	// Create a fresh schema with no migrations applied.
	schema := uniqueSchemaName(t)
	bootstrap, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bootstrap.Close()
	if _, err := bootstrap.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer bootstrap.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)

	s, err := Open(context.Background(), Config{
		DSN:         appendOption(dsn, "search_path", schema),
		AutoMigrate: false,
	})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open should refuse a fresh DB when AutoMigrate=false; got nil error")
	}
	if !strings.Contains(err.Error(), "schema not initialised") &&
		!strings.Contains(err.Error(), "loomcycle migrate up") {
		t.Errorf("error doesn't mention the fix; got: %v", err)
	}
}

// TestMigrateUp_Idempotent — running MigrateUp twice on the same schema
// must be a no-op the second time (no errors, no duplicate-row issues).
// golang-migrate handles this via its schema_migrations bookkeeping
// table; this test is the canary that we're using its API correctly.
func TestMigrateUp_Idempotent(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()

	// freshSchema already ran AutoMigrate=true via Open(). A second
	// MigrateUp on the same schema-rooted DSN must return nil
	// (ErrNoChange is mapped to nil internally).
	if err := MigrateUp(fix.storeDSN); err != nil {
		t.Fatalf("second MigrateUp: %v", err)
	}
}

// BenchmarkConcurrentRuns drives the storetest contract bench against
// Postgres. Run via:
//
//	make pg-up
//	LOOMCYCLE_TEST_PG_DSN="postgres://..." \
//	  go test -bench=. -benchtime=1x ./internal/store/postgres/...
//
// Operator-facing throughput numbers from this benchmark are captured
// in docs/POSTGRES.md alongside the SQLite baseline.
func BenchmarkConcurrentRuns(b *testing.B) {
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		b.Skip("LOOMCYCLE_TEST_PG_DSN not set; skipping Postgres bench")
	}
	for i := 0; i < b.N; i++ {
		// Per-iteration fresh schema avoids cross-bench pollution.
		schema := uniqueBenchSchemaName(b)
		bootstrap, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		if _, err := bootstrap.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
			bootstrap.Close()
			b.Fatalf("create schema: %v", err)
		}
		bootstrap.Close()

		scopedDSN := appendOption(dsn, "search_path", schema)
		s, err := Open(context.Background(), Config{
			DSN:          scopedDSN,
			MaxOpenConns: 32,
			AutoMigrate:  true,
		})
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		r := storetest.RunConcurrencyBench(b, s, storetest.BenchmarkConfig{})
		b.Logf("postgres: %s", storetest.FormatResult(r))
		_ = s.Close()

		// Cleanup schema.
		drop, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			_, _ = drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			drop.Close()
		}
	}
}

// uniqueBenchSchemaName mirrors uniqueSchemaName but uses the
// benchmark name (not a *testing.T name) for the prefix.
func uniqueBenchSchemaName(b *testing.B) string {
	n := schemaCounter.Add(1)
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, b.Name())
	if len(clean) > 30 {
		clean = clean[:30]
	}
	return fmt.Sprintf("lcb_%s_%d", clean, n)
}
