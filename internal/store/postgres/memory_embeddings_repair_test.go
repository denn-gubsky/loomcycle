package postgres

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Regression coverage for the pgvector-installed-late upgrade path.
//
// Migration 0017 creates memory_embeddings only when `CREATE EXTENSION
// vector` succeeded. golang-migrate tracks ONE monotonic version pointer and
// only applies migrations above it, so a database that migrated past 0017
// without pgvector never re-runs 0017 — the table was unreachable forever.
// Migration 0062 is the repair path, and Open() must probe the TABLE (not
// just the extension) so the interim state degrades to the typed
// store.ErrVectorUnsupported refusal instead of raw "relation does not
// exist" errors (or, before the fix, a refusal to boot at all).

// pgvectorInstallable reports whether the test Postgres can install pgvector.
//
// Auto-detected from pg_available_extensions rather than gated behind an
// opt-in env var: these tests are the only coverage of the late-install
// upgrade path, so they must run wherever the capability actually exists
// (e.g. CI on a pgvector/pgvector image) without anyone remembering a flag.
// On a plain postgres:16-alpine fixture they skip cleanly.
func pgvectorInstallable(t *testing.T, dsn string) bool {
	t.Helper()
	pool := rawPool(t, dsn)
	defer pool.Close()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`,
	).Scan(&ok); err != nil {
		t.Fatalf("probe pg_available_extensions: %v", err)
	}
	return ok
}

// rawPool opens a short-lived pool on dsn for the direct DDL/catalog work
// these tests need (the store.Store interface deliberately exposes none of
// it). Caller must Close.
func rawPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	return pool
}

// syncBuffer is a mutex-guarded log sink — Open() is not the only thing that
// may write to the default logger while a test runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// assertVectorOpsRefuse asserts every vector op returns the TYPED
// store.ErrVectorUnsupported. Pointer identity (errors.Is against the
// sentinel) is the point: a raw pgx "relation memory_embeddings does not
// exist" would satisfy "returns an error" but fail this.
func assertVectorOpsRefuse(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	const (
		tenant  = ""
		scopeID = "agent-x"
		key     = "k1"
	)
	scope := store.MemoryScopeAgent

	if err := s.MemoryEmbedSet(ctx, tenant, scope, scopeID, key, store.MemoryEmbedding{
		Provider: "p", Model: "m", Dimension: 3, Vector: []float32{1, 2, 3}, EmbedText: "hello",
	}); !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedSet: want ErrVectorUnsupported, got %v", err)
	}
	if _, err := s.MemoryEmbedGet(ctx, tenant, scope, scopeID, key); !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedGet: want ErrVectorUnsupported, got %v", err)
	}
	if _, err := s.MemoryEmbedSearch(ctx, tenant, scope, scopeID, "", []float32{1, 2, 3}, 5); !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedSearch: want ErrVectorUnsupported, got %v", err)
	}
	if _, err := s.MemoryEmbedListByModel(ctx, tenant, scope, scopeID, "p", "m", 10); !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedListByModel: want ErrVectorUnsupported, got %v", err)
	}
	if _, err := s.MemoryEmbedStats(ctx, tenant, scope); !errors.Is(err, store.ErrVectorUnsupported) {
		t.Errorf("MemoryEmbedStats: want ErrVectorUnsupported, got %v", err)
	}
	// The full-text leg degrades to (nil, nil) by contract — the hybrid
	// ranker treats "no full-text rows" as a missing leg, not a failure.
	rows, err := s.MemoryFullTextSearch(ctx, tenant, scope, scopeID, "", "hello", 5)
	if err != nil {
		t.Errorf("MemoryFullTextSearch: want nil error, got %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("MemoryFullTextSearch: want no rows, got %d", len(rows))
	}
}

// TestOpen_ExtensionPresentButTableMissingDisablesVectorSupport is the core
// regression: with the `vector` extension loaded but memory_embeddings
// absent, Open() must SUCCEED and report both capabilities as false so every
// memory op takes the typed-refusal path.
//
// Fail-before: prior to the fix Open() hard-failed in this exact state
// ("pgvector is installed but the `memory_embeddings` table is missing"),
// blocking boot entirely and telling the operator to re-run `migrate up` —
// which can never re-apply 0017.
func TestOpen_ExtensionPresentButTableMissingDisablesVectorSupport(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector; cannot stage extension-present + table-missing")
	}

	fix := freshSchemaWithVectors(t, dsn, true)
	defer fix.cleanup()

	// Guard: the reference state must really have the table, otherwise the
	// staging below is vacuous and this test would pass on nothing.
	if !fix.store.SupportsVectors() {
		t.Fatalf("fixture did not come up with vector support; cannot stage the missing-table case")
	}
	if err := fix.store.Close(); err != nil {
		t.Fatalf("close fixture store: %v", err)
	}

	// Stage the broken deployment: extension stays loaded, table goes away.
	pool := rawPool(t, fix.storeDSN)
	if _, err := pool.Exec(context.Background(), `DROP TABLE memory_embeddings CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("drop memory_embeddings: %v", err)
	}
	var extLoaded bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&extLoaded); err != nil {
		pool.Close()
		t.Fatalf("probe pg_extension: %v", err)
	}
	pool.Close()
	if !extLoaded {
		t.Fatalf("staging invalid: `vector` extension is not loaded")
	}

	var logs syncBuffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	s, err := Open(context.Background(), Config{
		DSN:             fix.storeDSN,
		MaxOpenConns:    4,
		AutoMigrate:     false,
		PgvectorEnabled: true,
	})
	if err != nil {
		t.Fatalf("Open with extension present but table missing: want success (degrade), got error: %v", err)
	}
	defer func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close store: %v", cerr)
		}
	}()

	if s.SupportsVectors() {
		t.Error("SupportsVectors() = true; want false when memory_embeddings is missing")
	}
	if s.SupportsFullText() {
		t.Error("SupportsFullText() = true; want false when memory_embeddings is missing")
	}
	assertVectorOpsRefuse(t, s)

	// The one silent-but-degraded state must be loudly actionable.
	got := logs.String()
	for _, want := range []string{"memory_embeddings", "0062", "migrate up"} {
		if !strings.Contains(got, want) {
			t.Errorf("Open() log does not mention %q; operator has no guidance. Got: %s", want, got)
		}
	}
}

// TestMigrate0062_RepairedTableMatchesFromScratchShape is the real
// correctness property: a database that reaches memory_embeddings via the
// 0062 repair must end up with a table indistinguishable from one built by
// the 0017 → 0059 → 0060 path — same columns in the same ordinal positions,
// same PK/FK, same indexes.
//
// Both shapes are taken in ONE schema, before and after replacing the table
// via the repair path. That is deliberate: it controls for schema name,
// collation and catalog defaults so the only variable is which migration
// created the table. (It also sidesteps a fixture hazard — `CREATE EXTENSION
// IF NOT EXISTS vector` is database-wide, so a second per-test schema no-ops
// on it and then cannot resolve the `vector` type from its own search_path.)
func TestMigrate0062_RepairedTableMatchesFromScratchShape(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector; cannot compare repaired vs from-scratch shape")
	}

	// Reference: built by the 0017 → 0059 → 0060 path (0062 no-opped because
	// the table already existed).
	fix := freshSchemaWithVectors(t, dsn, true)
	defer fix.cleanup()
	fromScratch := snapshotMemoryEmbeddings(t, fix.storeDSN)
	if len(fromScratch.columns) == 0 {
		t.Fatalf("fixture has no memory_embeddings table; the comparison would be vacuous")
	}

	// Now reproduce a deployment that installed pgvector after migrating past
	// 0017: drop the table and rewind the version pointer to 61, so the next
	// MigrateUp applies ONLY the 0062 repair.
	rewindToBeforeRepair(t, fix.storeDSN)
	if got := snapshotMemoryEmbeddings(t, fix.storeDSN); len(got.columns) != 0 {
		t.Fatalf("staging invalid: memory_embeddings still present before repair")
	}
	if err := MigrateUp(fix.storeDSN); err != nil {
		t.Fatalf("MigrateUp (repair): %v", err)
	}
	repaired := snapshotMemoryEmbeddings(t, fix.storeDSN)
	if len(repaired.columns) == 0 {
		t.Fatalf("0062 did not create memory_embeddings")
	}

	assertShapesEqual(t, fromScratch, repaired)
}

// TestMigrate0062_IsNoOpWhenTableAlreadyExists pins the idempotency arm: on
// the overwhelmingly common deployment (0017 created the table), 0062 must
// touch nothing — same shape AND existing rows intact.
func TestMigrate0062_IsNoOpWhenTableAlreadyExists(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector; memory_embeddings never exists to no-op over")
	}

	fix := freshSchemaWithVectors(t, dsn, true)
	defer fix.cleanup()
	if !fix.store.SupportsVectors() {
		t.Fatalf("fixture did not come up with vector support")
	}

	// A real row, so a drop-and-recreate would be visible as data loss
	// rather than only as a shape difference.
	ctx := context.Background()
	scope, scopeID, key := store.MemoryScopeAgent, "agent-noop", "k1"
	if err := fix.store.MemorySet(ctx, "", scope, scopeID, key, []byte(`"v"`), 0); err != nil {
		t.Fatalf("MemorySet: %v", err)
	}
	if err := fix.store.MemoryEmbedSet(ctx, "", scope, scopeID, key, store.MemoryEmbedding{
		Provider: "p", Model: "m", Dimension: 3, Vector: []float32{1, 2, 3}, EmbedText: "hello world",
	}); err != nil {
		t.Fatalf("MemoryEmbedSet: %v", err)
	}

	before := snapshotMemoryEmbeddings(t, fix.storeDSN)
	if len(before.columns) == 0 {
		t.Fatalf("memory_embeddings missing; nothing to no-op over")
	}

	// Rewind the pointer only — leave the table in place — so 0062 re-runs
	// against an existing table.
	forceMigrationVersion(t, fix.storeDSN, 61)
	if err := MigrateUp(fix.storeDSN); err != nil {
		t.Fatalf("MigrateUp (re-run 0062 over existing table): %v", err)
	}

	assertShapesEqual(t, before, snapshotMemoryEmbeddings(t, fix.storeDSN))

	got, err := fix.store.MemoryEmbedGet(ctx, "", scope, scopeID, key)
	if err != nil {
		t.Fatalf("MemoryEmbedGet after re-running 0062: %v (0062 must not recreate the table)", err)
	}
	if got.EmbedText != "hello world" {
		t.Errorf("EmbedText = %q, want %q", got.EmbedText, "hello world")
	}
}

// TestMigrate_WithoutPgvectorAppliesFullSetAndRefusesVectorOps covers the
// tolerance arm that must stay a clean no-op: on a Postgres without pgvector
// the whole set (0017 and 0062 included) applies to the newest version, the
// table is absent, and vector ops refuse with the typed error.
func TestMigrate_WithoutPgvectorAppliesFullSetAndRefusesVectorOps(t *testing.T) {
	dsn := pgDSNFromEnv(t)

	fix := freshSchema(t, dsn) // PgvectorEnabled=false
	defer fix.cleanup()

	version, dirty, err := MigrateStatus(fix.storeDSN)
	if err != nil {
		t.Fatalf("MigrateStatus: %v", err)
	}
	if dirty {
		t.Fatalf("schema is dirty at version %d", version)
	}
	want, err := highestEmbeddedVersion()
	if err != nil {
		t.Fatalf("highestEmbeddedVersion: %v", err)
	}
	if version != want {
		t.Errorf("migrated to version %d, want %d", version, want)
	}

	if fix.store.SupportsVectors() {
		t.Error("SupportsVectors() = true with PgvectorEnabled=false")
	}
	if fix.store.SupportsFullText() {
		t.Error("SupportsFullText() = true with PgvectorEnabled=false")
	}
	assertVectorOpsRefuse(t, fix.store)

	// Only meaningful where pgvector genuinely cannot load: there, 0017 and
	// 0062 must both have skipped the table without failing the migration.
	if !pgvectorInstallable(t, dsn) {
		if shape := snapshotMemoryEmbeddings(t, fix.storeDSN); len(shape.columns) != 0 {
			t.Errorf("memory_embeddings exists on a Postgres without pgvector")
		}
	}
}

// ---- shape snapshot helpers ----

// tableShape is a normalised, comparable description of memory_embeddings.
// Every entry is schema-name-stripped so two per-test schemas compare equal.
type tableShape struct {
	columns     []string
	constraints []string
	indexes     []string
}

// snapshotMemoryEmbeddings reads the table's columns, constraints and indexes
// out of the catalog. Returns a zero tableShape when the table is absent.
func snapshotMemoryEmbeddings(t *testing.T, storeDSN string) tableShape {
	t.Helper()
	pool := rawPool(t, storeDSN)
	defer pool.Close()
	ctx := context.Background()

	// current_schema() is the per-test schema (search_path is set on the
	// DSN), so every query below is scoped to this fixture alone.
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	norm := func(s string) string { return strings.ReplaceAll(s, schema, "<schema>") }

	var out tableShape

	// Columns, including ordinal_position — the 0017/0059/0060 append order
	// is part of the contract, so a reordered-but-equivalent table must fail.
	rows, err := pool.Query(ctx,
		`SELECT ordinal_position, column_name, data_type, udt_name, is_nullable,
		        coalesce(column_default, ''), is_generated, coalesce(generation_expression, '')
		   FROM information_schema.columns
		  WHERE table_schema = $1 AND table_name = 'memory_embeddings'
		  ORDER BY ordinal_position`, schema)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	for rows.Next() {
		var (
			pos                                                    int
			name, dataType, udt, nullable, def, generated, genExpr string
		)
		if err := rows.Scan(&pos, &name, &dataType, &udt, &nullable, &def, &generated, &genExpr); err != nil {
			rows.Close()
			t.Fatalf("scan column: %v", err)
		}
		out.columns = append(out.columns, norm(strings.Join([]string{
			strconv.Itoa(pos), name, dataType, udt, nullable, def, generated, genExpr,
		}, "|")))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}

	// Constraints: PK + FK, by name and by definition.
	rows, err = pool.Query(ctx,
		`SELECT c.conname, c.contype::text, pg_get_constraintdef(c.oid)
		   FROM pg_constraint c
		   JOIN pg_class cl ON cl.oid = c.conrelid
		   JOIN pg_namespace n ON n.oid = cl.relnamespace
		  WHERE n.nspname = $1 AND cl.relname = 'memory_embeddings'
		  ORDER BY c.conname`, schema)
	if err != nil {
		t.Fatalf("query constraints: %v", err)
	}
	for rows.Next() {
		var name, ctype, def string
		if err := rows.Scan(&name, &ctype, &def); err != nil {
			rows.Close()
			t.Fatalf("scan constraint: %v", err)
		}
		out.constraints = append(out.constraints, norm(name+"|"+ctype+"|"+def))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate constraints: %v", err)
	}

	rows, err = pool.Query(ctx,
		`SELECT indexname, indexdef FROM pg_indexes
		  WHERE schemaname = $1 AND tablename = 'memory_embeddings'
		  ORDER BY indexname`, schema)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			rows.Close()
			t.Fatalf("scan index: %v", err)
		}
		out.indexes = append(out.indexes, norm(name+"|"+def))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}

	return out
}

func assertShapesEqual(t *testing.T, want, got tableShape) {
	t.Helper()
	cmp := func(label string, a, b []string) {
		if len(a) != len(b) {
			t.Errorf("%s: count %d != %d\n want: %v\n  got: %v", label, len(a), len(b), a, b)
			return
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s[%d] mismatch:\n want: %s\n  got: %s", label, i, a[i], b[i])
			}
		}
	}
	cmp("columns", want.columns, got.columns)
	cmp("constraints", want.constraints, got.constraints)
	cmp("indexes", want.indexes, got.indexes)
}

// forceMigrationVersion rewrites golang-migrate's version pointer so a
// subsequent MigrateUp re-applies everything above `version`. This is how we
// replay a single migration against a chosen starting state.
func forceMigrationVersion(t *testing.T, storeDSN string, version int) {
	t.Helper()
	pool := rawPool(t, storeDSN)
	defer pool.Close()
	tag, err := pool.Exec(context.Background(),
		`UPDATE schema_migrations SET version = $1, dirty = false`, version)
	if err != nil {
		t.Fatalf("rewind schema_migrations to %d: %v", version, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("rewind schema_migrations: updated %d rows, want 1", tag.RowsAffected())
	}
}

// rewindToBeforeRepair reproduces "installed pgvector after migrating past
// 0017": the table is gone and the version pointer sits at 61, so the next
// MigrateUp applies only the 0062 repair.
func rewindToBeforeRepair(t *testing.T, storeDSN string) {
	t.Helper()
	pool := rawPool(t, storeDSN)
	if _, err := pool.Exec(context.Background(), `DROP TABLE memory_embeddings CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("drop memory_embeddings: %v", err)
	}
	pool.Close()
	forceMigrationVersion(t, storeDSN, 61)
}
