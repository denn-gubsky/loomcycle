package sqlmem

import (
	"context"
	"strings"
	"testing"
)

// dump_test.go — RFC AA Phase 3e sqlite-tier snapshot dump tests (no external
// DB; run on every `go test`). The postgres-tier round trip lives in
// dump_postgres_test.go (gated on LOOMCYCLE_TEST_SQLMEM_PG_DSN).

func TestTier_SQLite(t *testing.T) {
	m := newTestManager(t, Config{})
	if got := m.Tier(); got != "sqlite" {
		t.Fatalf("Tier() = %q, want sqlite", got)
	}
}

// TestListScopes_SQLiteDurableOnly: agent + user scopes are enumerated; the
// ephemeral run scope is never listed.
func TestListScopes_SQLiteDurableOnly(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()

	mk := func(k ScopeKey) {
		if _, err := m.Exec(ctx, k, "CREATE TABLE t (id INTEGER)", nil, 0); err != nil {
			t.Fatalf("create %v: %v", k, err)
		}
	}
	mk(ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: "a1"})
	mk(ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"})
	mk(ScopeKey{Tenant: "t2", Scope: "agent", ScopeID: "a2"})
	// A run scope must NOT appear.
	if _, err := m.Exec(ctx, ScopeKey{Scope: "run", ScopeID: "run-xyz"}, "CREATE TABLE t (id INTEGER)", nil, 0); err != nil {
		t.Fatalf("create run scope: %v", err)
	}

	scopes, err := m.ListScopes(ctx)
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	got := map[ScopeKey]bool{}
	for _, s := range scopes {
		got[s] = true
		if s.Scope == "run" {
			t.Fatalf("ListScopes returned a run scope: %v", s)
		}
	}
	for _, want := range []ScopeKey{
		{Tenant: "t1", Scope: "agent", ScopeID: "a1"},
		{Tenant: "t1", Scope: "user", ScopeID: "u1"},
		{Tenant: "t2", Scope: "agent", ScopeID: "a2"},
	} {
		if !got[want] {
			t.Fatalf("ListScopes missing %v (got %v)", want, scopes)
		}
	}
	if len(scopes) != 3 {
		t.Fatalf("ListScopes returned %d scopes, want 3: %v", len(scopes), scopes)
	}
}

// TestDump_SQLiteRoundTrip: a scope with a table, an explicit index, a NULL, and
// a binary BLOB round-trips through Export → Restore into a fresh manager.
func TestDump_SQLiteRoundTrip(t *testing.T) {
	src := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")

	stmts := []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, tag TEXT, blob BLOB)`,
		`CREATE INDEX idx_notes_tag ON notes (tag)`,
	}
	for _, s := range stmts {
		if _, err := src.Exec(ctx, key, s, nil, 0); err != nil {
			t.Fatalf("ddl %q: %v", s, err)
		}
	}
	bin := []byte{0x00, 0xff, 0xfe, 0x10}
	rows := [][]any{
		{int64(1), "hello", "x", bin},
		{int64(2), "world", nil, nil}, // NULL tag + NULL blob
	}
	for _, r := range rows {
		if _, err := src.Exec(ctx, key, `INSERT INTO notes (id, body, tag, blob) VALUES (?,?,?,?)`, r, 0); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	dump, err := src.ExportScope(ctx, key)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	// The explicit index must be in the DDL (auto PK index is recreated by the
	// table DDL and excluded).
	var sawIndex bool
	for _, d := range dump.DDL {
		if containsAll(d, "CREATE INDEX", "idx_notes_tag") {
			sawIndex = true
		}
	}
	if !sawIndex {
		t.Fatalf("dump DDL missing the explicit index: %v", dump.DDL)
	}

	// Restore into a FRESH manager (different root).
	dst := newTestManager(t, Config{})
	if err := dst.RestoreScope(ctx, key, dump); err != nil {
		t.Fatalf("RestoreScope: %v", err)
	}

	res, err := dst.Query(ctx, key, `SELECT id, body, tag, blob FROM notes ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("Query restored: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("restored %d rows, want 2", len(res.Rows))
	}
	if got, _ := res.Rows[0][1].(string); got != "hello" {
		t.Fatalf("row0 body = %q, want hello", got)
	}
	// The binary BLOB survives byte-for-byte (collectRows hands it back as the
	// raw-byte string).
	if got, _ := res.Rows[0][3].(string); got != string(bin) {
		t.Fatalf("row0 blob = %x, want %x", got, bin)
	}
	if res.Rows[1][2] != nil {
		t.Fatalf("row1 tag = %v, want nil", res.Rows[1][2])
	}
}

// TestRestoreScope_SQLiteIdempotent: a second Restore on the same dump does not
// duplicate rows (table is already non-empty → skipped).
func TestRestoreScope_SQLiteIdempotent(t *testing.T) {
	src := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "w")
	if _, err := src.Exec(ctx, key, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`, nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := src.Exec(ctx, key, `INSERT INTO t (id, v) VALUES (?,?)`, []any{int64(i), "row"}, 0); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	dump, err := src.ExportScope(ctx, key)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	dst := newTestManager(t, Config{})
	for i := 0; i < 2; i++ {
		if err := dst.RestoreScope(ctx, key, dump); err != nil {
			t.Fatalf("RestoreScope #%d: %v", i, err)
		}
	}
	res, err := dst.Query(ctx, key, `SELECT count(*) FROM t`, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got, _ := res.Rows[0][0].(int64); got != 3 {
		t.Fatalf("after double restore count = %d, want 3 (idempotent)", got)
	}
}

// TestExportScope_RejectsRunScope: the ephemeral run scope is never exportable.
func TestExportScope_RejectsRunScope(t *testing.T) {
	m := newTestManager(t, Config{})
	if _, err := m.ExportScope(context.Background(), ScopeKey{Scope: "run", ScopeID: "r1"}); err == nil {
		t.Fatal("ExportScope(run) succeeded, want error")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
