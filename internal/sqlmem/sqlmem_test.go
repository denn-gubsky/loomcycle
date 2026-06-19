package sqlmem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestManager builds a Manager rooted in a temp dir with generous
// bounds. Individual tests override cfg fields via a returned closure when
// they need a tighter bound (quota/rows).
func newTestManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func agentKey(tenant, agent string) ScopeKey {
	return ScopeKey{Tenant: tenant, Scope: "agent", ScopeID: agent}
}

// managerRoot returns the on-disk root of a sqlite-backed Manager — the
// file-path tests below need it now that the facade delegates the root to the
// backend.
func managerRoot(t *testing.T, m *Manager) string {
	t.Helper()
	sb, ok := m.backend.(*sqliteBackend)
	if !ok {
		t.Fatalf("manager is not sqlite-backed (%T)", m.backend)
	}
	return sb.cfg.Root
}

// TestManager_RoundTripsWithinOneScope asserts CREATE TABLE + INSERT + SELECT
// flows end-to-end through one scope database.
func TestManager_RoundTripsWithinOneScope(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")

	if _, err := m.Exec(ctx, key, "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := m.Exec(ctx, key, "INSERT INTO notes (body) VALUES (?)", []any{"hello"}, 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("RowsAffected = %d, want 1", res.RowsAffected)
	}
	if res.LastInsertID != 1 {
		t.Fatalf("LastInsertID = %d, want 1", res.LastInsertID)
	}

	q, err := m.Query(ctx, key, "SELECT id, body FROM notes ORDER BY id", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := strings.Join(q.Columns, ","); got != "id,body" {
		t.Fatalf("columns = %q, want id,body", got)
	}
	if len(q.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(q.Rows))
	}
	if body, ok := q.Rows[0][1].(string); !ok || body != "hello" {
		t.Fatalf("row body = %#v, want \"hello\"", q.Rows[0][1])
	}
}

// TestManager_ScopeAInvisibleToScopeB asserts data written under one agent
// scope is not visible to another agent scope (different file).
func TestManager_ScopeAInvisibleToScopeB(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	a := agentKey("t1", "agentA")
	b := agentKey("t1", "agentB")

	if _, err := m.Exec(ctx, a, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := m.Exec(ctx, a, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	// Scope B has its own file — the table created in A does not exist here.
	if _, err := m.Query(ctx, b, "SELECT x FROM t", nil); err == nil {
		t.Fatal("scope B saw scope A's table; want an error (no such table)")
	}
}

// TestManager_TenantAInvisibleToTenantB asserts tenant isolation: the same
// agent name under two tenants maps to two distinct files.
func TestManager_TenantAInvisibleToTenantB(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	a := agentKey("tenantA", "shared-name")
	b := agentKey("tenantB", "shared-name")

	if _, err := m.Exec(ctx, a, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := m.Exec(ctx, a, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := m.Query(ctx, b, "SELECT x FROM t", nil); err == nil {
		t.Fatal("tenant B saw tenant A's table; want an error (separate file)")
	}
}

// TestManager_AttachRefusedAndCannotSeeOtherFile asserts a scope connection
// cannot ATTACH another file (refused by the validator) and cannot see a
// table that exists only in another scope's file.
func TestManager_AttachRefusedAndCannotSeeOtherFile(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	victim := agentKey("t1", "victim")
	attacker := agentKey("t1", "attacker")

	// Seed a secret table in the victim file.
	if _, err := m.Exec(ctx, victim, "CREATE TABLE secrets (s TEXT)", nil, 0); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if _, err := m.Exec(ctx, victim, "INSERT INTO secrets VALUES ('topsecret')", nil, 0); err != nil {
		t.Fatalf("insert victim: %v", err)
	}
	victimPath, err := victim.keyPath(managerRoot(t, m))
	if err != nil {
		t.Fatalf("keyPath: %v", err)
	}

	// The attacker scope tries to ATTACH the victim file — the validator must
	// refuse it before it reaches the driver.
	attachStmt := "ATTACH DATABASE '" + victimPath + "' AS v"
	if _, err := m.Exec(ctx, attacker, attachStmt, nil, 0); err == nil {
		t.Fatal("ATTACH was permitted; want a validator refusal")
	} else if _, ok := err.(*ErrStatement); !ok {
		t.Fatalf("ATTACH error = %T (%v); want *ErrStatement", err, err)
	}

	// And the attacker's own file does not contain the victim's table.
	if _, err := m.Query(ctx, attacker, "SELECT s FROM secrets", nil); err == nil {
		t.Fatal("attacker scope saw the victim's 'secrets' table; want no such table")
	}
}

// TestManager_QueryTruncatesAtMaxRows asserts a SELECT exceeding MaxRows
// returns Truncated=true with exactly MaxRows rows.
func TestManager_QueryTruncatesAtMaxRows(t *testing.T) {
	m := newTestManager(t, Config{MaxRows: 3})
	ctx := context.Background()
	key := agentKey("t1", "rows")

	if _, err := m.Exec(ctx, key, "CREATE TABLE n (i INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := m.Exec(ctx, key, "INSERT INTO n VALUES (?)", []any{i}, 0); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	q, err := m.Query(ctx, key, "SELECT i FROM n ORDER BY i", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !q.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(q.Rows) != 3 {
		t.Fatalf("rows = %d, want 3 (MaxRows)", len(q.Rows))
	}
}

// TestManager_WritePastQuotaRefused asserts a write that would exceed the
// quota is refused (quota checked before the write).
func TestManager_WritePastQuotaRefused(t *testing.T) {
	// 32 KiB quota — a couple of pages plus a fat row pushes past it.
	m := newTestManager(t, Config{QuotaBytes: 32 * 1024})
	ctx := context.Background()
	key := agentKey("t1", "quota")

	if _, err := m.Exec(ctx, key, "CREATE TABLE big (b TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	blob := strings.Repeat("x", 8*1024)
	var lastErr error
	// Insert until the quota refuses; bounded loop so a never-refusing bug
	// fails the test instead of hanging.
	for i := 0; i < 50; i++ {
		_, lastErr = m.Exec(ctx, key, "INSERT INTO big VALUES (?)", []any{blob}, 0)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("no write was refused; want a quota error after the file grew")
	}
	if !strings.Contains(lastErr.Error(), "quota") {
		t.Fatalf("error = %v; want a quota refusal", lastErr)
	}
}

// TestManager_QuotaOverrideTighterThanDefault asserts the per-call override
// is honored over the Manager default.
func TestManager_QuotaOverrideTighterThanDefault(t *testing.T) {
	m := newTestManager(t, Config{QuotaBytes: 0}) // no manager default
	ctx := context.Background()
	key := agentKey("t1", "override")

	if _, err := m.Exec(ctx, key, "CREATE TABLE big (b TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	blob := strings.Repeat("x", 8*1024)
	var lastErr error
	for i := 0; i < 50; i++ {
		_, lastErr = m.Exec(ctx, key, "INSERT INTO big VALUES (?)", []any{blob}, 32*1024)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "quota") {
		t.Fatalf("override quota not enforced; lastErr = %v", lastErr)
	}
}

// TestManager_RunScopeFileLifecycle asserts an ephemeral run scope file is
// created on write and removed by DropRunScope.
func TestManager_RunScopeFileLifecycle(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := ScopeKey{Scope: "run", ScopeID: "run-abc123"}

	if _, err := m.Exec(ctx, key, "CREATE TABLE scratch (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	path, err := key.keyPath(managerRoot(t, m))
	if err != nil {
		t.Fatalf("keyPath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("run-scope file should exist after write: %v", err)
	}

	removed, err := m.DropRunScope("run-abc123")
	if err != nil {
		t.Fatalf("DropRunScope: %v", err)
	}
	if !removed {
		t.Fatal("DropRunScope removed = false, want true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("run-scope file should be gone after drop; stat err = %v", err)
	}
}

// TestManager_DropRunScopeIsNoopWhenAbsent asserts dropping a never-written
// run scope is a no-op success.
func TestManager_DropRunScopeIsNoopWhenAbsent(t *testing.T) {
	m := newTestManager(t, Config{})
	removed, err := m.DropRunScope("never-existed")
	if err != nil {
		t.Fatalf("DropRunScope: %v", err)
	}
	if removed {
		t.Fatal("removed = true for an absent scope, want false")
	}
}

// TestKeyPath_MaliciousScopeIDStaysUnderRoot is the path-escape regression:
// a scope id like "../../etc/x" must NOT escape Root — the derived path must
// remain strictly inside Root after Clean.
func TestKeyPath_MaliciousScopeIDStaysUnderRoot(t *testing.T) {
	root := t.TempDir()
	for _, evil := range []string{
		"../../etc/passwd",
		"..",
		"a/../../b",
		"/abs/path",
		"foo/bar",
	} {
		key := ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: evil}
		path, err := key.keyPath(root)
		if err != nil {
			// An error is an acceptable defence (e.g. empty) — but these are
			// non-empty, so we expect a path.
			t.Fatalf("keyPath(%q) errored unexpectedly: %v", evil, err)
		}
		clean := filepath.Clean(path)
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			t.Fatalf("Rel(%q): %v", clean, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("scope id %q escaped Root: path=%q rel=%q", evil, clean, rel)
		}
	}
}

// TestKeyPath_DistinctSanitizedIDsDoNotCollide asserts two ids that map to
// the same replaced form land on distinct files (the hash suffix).
func TestKeyPath_DistinctSanitizedIDsDoNotCollide(t *testing.T) {
	root := t.TempDir()
	p1, err := ScopeKey{Tenant: "t", Scope: "agent", ScopeID: "a/b"}.keyPath(root)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ScopeKey{Tenant: "t", Scope: "agent", ScopeID: "a.b"}.keyPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("distinct ids collided on one file: %q", p1)
	}
}

// sanity: the underlying driver name matches the store's, so a scope DB is
// the same engine as the main store (just a different file).
func TestOpenScopeDB_UsesSqliteDriver(t *testing.T) {
	dir := t.TempDir()
	db, err := openScopeDB(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("openScopeDB: %v", err)
	}
	defer db.Close()
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d", one)
	}
}
