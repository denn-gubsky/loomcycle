package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

// newTestStore opens a fresh on-disk SQLite under t.TempDir(). On-disk (vs
// :memory:) so the `cache=shared` modernc semantics don't surprise tests.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestStoreContract runs the shared behavioural test suite against the
// SQLite adapter. The same suite runs against the Postgres adapter in
// internal/store/postgres/postgres_test.go — any contract divergence
// surfaces as a failed sub-test on whichever adapter regressed.
func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (store.Store, func()) {
		s := newTestStore(t)
		return s, func() { _ = s.Close() }
	})
}

// ---- SQLite-specific tests below this line ----
//
// Tests that verify SQLite-only behaviour (the ALTER-COLUMN idempotency
// guard, NULL columns inspected via direct SQL access to s.db) live here.
// Anything that's true of every Store adapter belongs in
// storetest/contract.go instead.

// Idempotent migration: opening the same DB twice MUST NOT error. The
// "duplicate column name" tolerance in migrate() is the only thing that
// makes this safe.
//
// EMPIRICAL: removing the strings.Contains "duplicate column name" guard
// from the addColumns loop in sqlite.go makes the second Open() error.
func TestMigrate_AddsColumnsIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open should not error after schema is already in place: %v", err)
	}
	defer s2.Close()
}

// Empty userID writes NULL (not ""), so partial indexes on
// user_id IS NOT NULL stay small. Verified by direct SQL because the
// abstract Store interface only surfaces empty-vs-non-empty strings.
func TestCreateSession_EmptyUserIDIsNullInDB(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	emptySess, _ := s.CreateSession(ctx, "t", "a", "")
	var nullCheck *string
	row := s.db.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE id = ?`, emptySess.ID)
	if err := row.Scan(&nullCheck); err != nil {
		t.Fatal(err)
	}
	if nullCheck != nil {
		t.Errorf("empty userID should write NULL, got %q", *nullCheck)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}

// BenchmarkConcurrentRuns drives the storetest contract bench against
// SQLite. Run via `go test -bench=. ./internal/store/sqlite/...`.
// Operator-facing throughput numbers from this benchmark are
// captured in docs/POSTGRES.md as the SQLite baseline.
func BenchmarkConcurrentRuns(b *testing.B) {
	for i := 0; i < b.N; i++ {
		path := filepath.Join(b.TempDir(), "bench.db")
		s, err := Open(path)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		r := storetest.RunConcurrencyBench(b, s, storetest.BenchmarkConfig{})
		b.Logf("sqlite: %s", storetest.FormatResult(r))
		_ = s.Close()
	}
}
