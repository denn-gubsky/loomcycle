package sqlmem

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestQueryExisting_SQLiteNeitherTouchesNorCreates — with scope GC on, a read
// through QueryExisting leaves the scope file's mtime where it was (Query moves
// it forward, which keeps a dormant scope from being reclaimed), and a scope
// with no file stays without one.
func TestQueryExisting_SQLiteNeitherTouchesNorCreates(t *testing.T) {
	m := newTestManager(t, Config{ScopeTTLMS: 3600_000})
	ctx := context.Background()
	key := agentKey("t1", "plan-read")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	path, _ := key.keyPath(managerRoot(t, m))
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	m.touchMu.Lock()
	m.lastTouch = map[string]time.Time{}
	m.touchMu.Unlock()

	res, err := m.QueryExisting(ctx, key, "SELECT count(*) FROM t", nil)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("QueryExisting: %v %+v", err, res)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("QueryExisting moved the scope mtime %v → %v", old, info.ModTime())
	}

	absent := agentKey("t1", "never-used")
	if _, err := m.QueryExisting(ctx, absent, "SELECT 1", nil); !errors.Is(err, ErrPlaneAbsent) {
		t.Errorf("absent scope: err = %v, want ErrPlaneAbsent", err)
	}
	if ok, err := m.ScopeExists(ctx, absent); err != nil || ok {
		t.Errorf("after QueryExisting the absent scope exists = %v (%v), want it still absent", ok, err)
	}
}

// TestQueryExisting_PostgresNeitherProvisionsNorTouches — a process that never
// provisioned a scope reads it through QueryExisting without running the
// provisioning DDL (a role setting changed behind its back survives) and without
// stamping the scope's last use. The same read through Query does both, which
// shows the two probes can see what they look for.
func TestQueryExisting_PostgresNeitherProvisionsNorTouches(t *testing.T) {
	first, raw := pgTestManager(t, Config{ScopeTTLMS: 3600_000})
	ctx := context.Background()
	key := agentKey("t1", "plan-read")
	if _, err := first.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	schema, role, _ := pgScopeNames(key)
	reset := func() {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `ALTER ROLE `+q(role)+` SET work_mem TO '1MB'`); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx,
			`UPDATE sqlmem_meta.scope_access SET last_used = now() - interval '2 hours' WHERE schema_name = $1`, schema); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (workMem string, stale bool) {
		t.Helper()
		var cfg string
		if err := raw.QueryRowContext(ctx, `SELECT coalesce(array_to_string(s.setconfig, ','), '')
			FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole WHERE r.rolname = $1`, role).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		for _, kv := range strings.Split(cfg, ",") {
			if v, ok := strings.CutPrefix(kv, "work_mem="); ok {
				workMem = v
			}
		}
		if err := raw.QueryRowContext(ctx, `SELECT last_used < now() - interval '1 hour'
			FROM sqlmem_meta.scope_access WHERE schema_name = $1`, schema).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		return workMem, stale
	}

	// A second Manager is a second process: its provisioned set is empty.
	second, err := NewPostgres(ctx, Config{PgDSN: os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN"), ScopeTTLMS: 3600_000,
		StatementTimeoutMS: 30000, MaxRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	reset()
	if res, err := second.QueryExisting(ctx, key, "SELECT count(*) FROM t", nil); err != nil || len(res.Rows) != 1 {
		t.Fatalf("QueryExisting: %v %+v", err, res)
	}
	if mem, stale := state(); mem != "1MB" || !stale {
		t.Errorf("after QueryExisting work_mem = %q, last use stale = %v; want 1MB and stale (no DDL, no touch)", mem, stale)
	}

	third, err := NewPostgres(ctx, Config{PgDSN: os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN"), ScopeTTLMS: 3600_000,
		StatementTimeoutMS: 30000, MaxRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	if _, err := third.Query(ctx, key, "SELECT count(*) FROM t", nil); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if mem, stale := state(); mem == "1MB" || stale {
		t.Errorf("after Query work_mem = %q, last use stale = %v; the probes cannot see provisioning or a touch", mem, stale)
	}

	absent := agentKey("t1", "never-used")
	if _, err := second.QueryExisting(ctx, absent, "SELECT 1", nil); !errors.Is(err, ErrPlaneAbsent) {
		t.Errorf("absent scope: err = %v, want ErrPlaneAbsent", err)
	}
	if ok, err := second.ScopeExists(ctx, absent); err != nil || ok {
		t.Errorf("after QueryExisting the absent scope exists = %v (%v), want it still absent", ok, err)
	}
}

// TestListTenantScopes_ListsOnlyThatTenant — on both tiers the tenant listing
// holds the tenant's durable scopes and no other tenant's.
func TestListTenantScopes_ListsOnlyThatTenant(t *testing.T) {
	check := func(t *testing.T, m *Manager) {
		ctx := context.Background()
		for _, k := range []ScopeKey{agentKey("t1", "a"), {Tenant: "t1", Scope: "user", ScopeID: "u"}, agentKey("t2", "a")} {
			if _, err := m.Exec(ctx, k, "CREATE TABLE t (x INT)", nil, 0); err != nil {
				t.Fatalf("create %v: %v", k, err)
			}
		}
		got, err := m.ListTenantScopes(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("t1 scopes = %+v, want 2", got)
		}
		for _, k := range got {
			if k.Tenant != "t1" {
				t.Errorf("listed %+v, another tenant's scope", k)
			}
		}
		if none, err := m.ListTenantScopes(ctx, "t9"); err != nil || len(none) != 0 {
			t.Errorf("a tenant with no scopes: %+v %v", none, err)
		}
	}
	t.Run("sqlite", func(t *testing.T) { check(t, newTestManager(t, Config{})) })
	t.Run("postgres", func(t *testing.T) {
		m, _ := pgTestManager(t, Config{})
		check(t, m)
	})
}
