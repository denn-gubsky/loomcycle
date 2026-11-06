package sqlmem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The byte cap on an agent's SQL result. A row cap alone bounds the COUNT of
// rows, not their size: 10,000 rows of a computed 100 MB value each passed it.
// These tests run on both tiers; the postgres half skips without the aux DSN.

const testResultBudget = 64 << 10 // 64 KiB: small enough to cross with a few rows

// resultBytesTiers runs fn on a sqlite Manager and on a postgres one, both with
// the given byte budget. The postgres half skips without its DSN.
func resultBytesTiers(t *testing.T, budget int, fn func(t *testing.T, m *Manager, tier string)) {
	t.Run("sqlite", func(t *testing.T) {
		fn(t, newTestManager(t, Config{MaxRows: 10000, MaxResultBytes: budget}), "sqlite")
	})
	t.Run("postgres", func(t *testing.T) {
		m, _ := pgTestManager(t, Config{MaxRows: 10000, MaxResultBytes: budget})
		fn(t, m, "postgres")
	})
}

// fillWide makes a table of n rows, each an integer id and an 8 KiB text body.
func fillWide(t *testing.T, m *Manager, key ScopeKey, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.Exec(ctx, key, "CREATE TABLE wide (id INTEGER, body TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	ins := "INSERT INTO wide (id, body) VALUES (?, ?)"
	if m.Tier() == "postgres" {
		ins = "INSERT INTO wide (id, body) VALUES ($1, $2)"
	}
	for i := 1; i <= n; i++ {
		if _, err := m.Exec(ctx, key, ins, []any{i, strings.Repeat("x", 8192)}, 0); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
}

// resultSize measures a result the way collectRows does.
func resultSize(res *QueryResult) int {
	n := 0
	for _, row := range res.Rows {
		for _, v := range row {
			n += cellBytes(v)
		}
	}
	return n
}

// TestSqlmem_ResultBytesAreCapped: a capped read of rows totalling more than the
// budget stops before the row that would cross it and says truncated, on both
// tiers, in and out of a transaction. The uncapped read the runtime uses for its
// own statements still returns everything.
func TestSqlmem_ResultBytesAreCapped(t *testing.T) {
	resultBytesTiers(t, testResultBudget, func(t *testing.T, m *Manager, tier string) {
		ctx := context.Background()
		key := agentKey("t1", "bytes")
		fillWide(t, m, key, 20) // ~160 KiB, well over 64 KiB
		const stmt = "SELECT id, body FROM wide ORDER BY id"

		check := func(t *testing.T, res *QueryResult) {
			t.Helper()
			if !res.Truncated {
				t.Fatalf("Truncated = false with %d rows; a result over the byte budget must say truncated", len(res.Rows))
			}
			if len(res.Rows) == 0 || len(res.Rows) >= 20 {
				t.Fatalf("rows = %d, want some but not all 20", len(res.Rows))
			}
			got := resultSize(res)
			if got > testResultBudget {
				t.Fatalf("result is %d bytes, over the %d budget", got, testResultBudget)
			}
			// It stopped at the budget, not early: one more row would cross it.
			if got+resultSize(&QueryResult{Rows: res.Rows[:1]}) <= testResultBudget {
				t.Fatalf("result is %d bytes and stopped although the next row fit", got)
			}
			for i, row := range res.Rows {
				if id := toInt(row[0]); id != int64(i+1) {
					t.Fatalf("row %d has id %v, want the first rows in order", i, row[0])
				}
			}
		}

		res, err := m.QueryCapped(ctx, key, stmt, nil)
		if err != nil {
			t.Fatalf("QueryCapped: %v", err)
		}
		check(t, res)

		// The capped door validates the scope key like Query does.
		bad := ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: "a\x1fb"}
		if _, err := m.QueryCapped(ctx, bad, stmt, nil); err == nil {
			t.Fatal("QueryCapped accepted a separator-bearing scope id")
		}

		txnID := "txn-" + tier
		if _, err := m.BeginTxn(ctx, txnID, "root-"+tier, key); err != nil {
			t.Fatalf("BeginTxn: %v", err)
		}
		tres, err := m.QueryTxnCapped(ctx, txnID, stmt, nil)
		if err != nil {
			t.Fatalf("QueryTxnCapped: %v", err)
		}
		check(t, tres)
		if _, err := m.RollbackTxn(txnID); err != nil {
			t.Fatalf("RollbackTxn: %v", err)
		}

		all, err := m.Query(ctx, key, stmt, nil)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if all.Truncated || len(all.Rows) != 20 {
			t.Fatalf("uncapped Query: rows=%d truncated=%v, want all 20", len(all.Rows), all.Truncated)
		}
	})
}

// TestSqlmem_OversizedFirstRowIsRefused: a first row bigger than the whole budget
// is an error, not a result — stored or computed by the statement itself. A big
// row after smaller ones only ends the result early.
func TestSqlmem_OversizedFirstRowIsRefused(t *testing.T) {
	resultBytesTiers(t, testResultBudget, func(t *testing.T, m *Manager, tier string) {
		ctx := context.Background()
		key := agentKey("t1", "giant")
		big := strings.Repeat("y", 4*testResultBudget)
		ins, computed := "INSERT INTO cells (id, body) VALUES (?, ?)", "SELECT printf('%.*c', 1000000, 'z')"
		if tier == "postgres" {
			ins, computed = "INSERT INTO cells (id, body) VALUES ($1, $2)", "SELECT repeat('z', 1000000)"
		}
		if _, err := m.Exec(ctx, key, "CREATE TABLE cells (id INTEGER, body TEXT)", nil, 0); err != nil {
			t.Fatalf("create: %v", err)
		}
		for i, body := range []string{"small", big} {
			if _, err := m.Exec(ctx, key, ins, []any{i + 1, body}, 0); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}

		for name, stmt := range map[string]string{
			"stored":   "SELECT body FROM cells WHERE id = 2",
			"computed": computed,
		} {
			res, err := m.QueryCapped(ctx, key, stmt, nil)
			if err == nil {
				t.Fatalf("%s: a first row over the budget came back (%d rows, %d bytes); want an error",
					name, len(res.Rows), resultSize(res))
			}
			var se *ErrStatement
			if !errors.As(err, &se) || !strings.Contains(err.Error(), "result limit") {
				t.Fatalf("%s: err = %v (%T), want an ErrStatement naming the result limit", name, err, err)
			}
		}

		res, err := m.QueryCapped(ctx, key, "SELECT id, body FROM cells ORDER BY id", nil)
		if err != nil {
			t.Fatalf("small row then a big one: %v", err)
		}
		if len(res.Rows) != 1 || !res.Truncated {
			t.Fatalf("rows=%d truncated=%v, want the small row and truncated", len(res.Rows), res.Truncated)
		}

		// The runtime's own uncapped read of a large value is untouched.
		own, err := m.Query(ctx, key, "SELECT body FROM cells WHERE id = 2", nil)
		if err != nil || len(own.Rows) != 1 {
			t.Fatalf("uncapped Query of the big row: rows=%v err=%v", own, err)
		}
	})
}

// TestSqlmem_ZeroConfigStillCapsResultBytes: a Manager built without a byte
// budget — as a caller that never heard of the field builds it — caps an agent's
// result at the default rather than not at all.
func TestSqlmem_ZeroConfigStillCapsResultBytes(t *testing.T) {
	resultBytesTiers(t, 0, func(t *testing.T, m *Manager, tier string) {
		ctx := context.Background()
		key := agentKey("t1", "zero")
		over := DefaultMaxResultBytes + 1<<20
		stmt := fmt.Sprintf("SELECT printf('%%.*c', %d, 'z')", over)
		if tier == "postgres" {
			stmt = fmt.Sprintf("SELECT repeat('z', %d)", over)
		}
		res, err := m.QueryCapped(ctx, key, stmt, nil)
		if err == nil {
			t.Fatalf("a %d-byte first row came back from a zero-config Manager (%d rows); want the default budget to refuse it",
				over, len(res.Rows))
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d-byte result limit", DefaultMaxResultBytes)) {
			t.Fatalf("err = %v, want the default %d-byte limit", err, DefaultMaxResultBytes)
		}
	})
}

// TestSqlmem_ResultByteCapBoundaryIsInclusive: a result exactly at the budget is
// returned whole.
func TestSqlmem_ResultByteCapBoundaryIsInclusive(t *testing.T) {
	m := newTestManager(t, Config{MaxRows: 10000})
	ctx := context.Background()
	key := agentKey("t1", "edge")
	fillWide(t, m, key, 3)
	all, err := m.Query(ctx, key, "SELECT id, body FROM wide ORDER BY id", nil)
	if err != nil {
		t.Fatal(err)
	}
	exact := newTestManager(t, Config{Root: managerRoot(t, m), MaxRows: 10000, MaxResultBytes: resultSize(all)})
	res, err := exact.QueryCapped(ctx, key, "SELECT id, body FROM wide ORDER BY id", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || res.Truncated {
		t.Fatalf("rows=%d truncated=%v at a budget equal to the result, want all 3", len(res.Rows), res.Truncated)
	}
}

// TestSqlmem_DumpIsNotCutByResultByteCap: a scope export (the snapshot path)
// carries every row however small the result budget is — it is a copy of the
// database, not an answer to an agent.
func TestSqlmem_DumpIsNotCutByResultByteCap(t *testing.T) {
	resultBytesTiers(t, 1024, func(t *testing.T, m *Manager, tier string) {
		ctx := context.Background()
		key := agentKey("t1", "dump")
		fillWide(t, m, key, 20)
		dump, err := m.ExportScope(ctx, key)
		if err != nil {
			t.Fatalf("ExportScope: %v", err)
		}
		for _, td := range dump.Tables {
			if td.Name == "wide" {
				if len(td.Rows) != 20 {
					t.Fatalf("dump carries %d rows of wide, want all 20", len(td.Rows))
				}
				return
			}
		}
		t.Fatalf("dump has no table wide: %+v", dump.Tables)
	})
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	}
	return -1
}
