package sqlmem

import (
	"context"
	"strings"
	"testing"
)

// TestTenantScope_ProvisionsThroughTheExistingPath is the load-bearing claim of
// RFC BL P4b's sqlmem half: the package is GENERIC over the scope string, so a
// tenant scope needs no new plumbing.
//
// If that is wrong, it is wrong at provisioning time — a schema/role naming or
// path-fence rejection — not at review time, which is why it is asserted rather
// than assumed.
func TestTenantScope_ProvisionsThroughTheExistingPath(t *testing.T) {
	m := newTestManager(t, Config{Root: t.TempDir()})
	ctx := context.Background()

	// ScopeID repeats the tenant: pgScopeNames rejects an empty component because
	// the value becomes half of a schema + LOGIN role name.
	key := ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: "acme"}

	if _, err := m.Exec(ctx, key, `CREATE TABLE IF NOT EXISTS house (k TEXT PRIMARY KEY, v TEXT)`, nil, 0); err != nil {
		t.Fatalf("tenant scope must provision through the existing path: %v", err)
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO house (k, v) VALUES ('style', 'tabs')`, nil, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := m.Query(ctx, key, `SELECT v FROM house WHERE k = 'style'`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0][0] != "tabs" {
		t.Fatalf("round trip failed: %+v", got.Rows)
	}
}

// TestTenantScope_IsolatedFromUserAndOtherTenants pins the two isolation axes that
// make the scope safe to grant. `{t, tenant, t}` must not alias `{t, user, t}` —
// they differ only in the scope segment of the hashed name, which is exactly the
// kind of thing a naming refactor breaks silently.
func TestTenantScope_IsolatedFromUserAndOtherTenants(t *testing.T) {
	m := newTestManager(t, Config{Root: t.TempDir()})
	ctx := context.Background()

	tenantKey := ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: "acme"}
	// Same tenant AND same scope id, differing only in the scope segment.
	userKey := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "acme"}
	otherTenant := ScopeKey{Tenant: "globex", Scope: "tenant", ScopeID: "globex"}

	for _, k := range []ScopeKey{tenantKey, userKey, otherTenant} {
		if _, err := m.Exec(ctx, k, `CREATE TABLE IF NOT EXISTS t (v TEXT)`, nil, 0); err != nil {
			t.Fatalf("create in %+v: %v", k, err)
		}
	}
	if _, err := m.Exec(ctx, tenantKey, `INSERT INTO t (v) VALUES ('tenant-row')`, nil, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}

	for _, k := range []ScopeKey{userKey, otherTenant} {
		got, err := m.Query(ctx, k, `SELECT v FROM t`, nil)
		if err != nil {
			t.Fatalf("query %+v: %v", k, err)
		}
		if len(got.Rows) != 0 {
			t.Errorf("LEAK: %+v sees the tenant scope's rows: %+v", k, got.Rows)
		}
	}
}

// TestTenantScope_EmptyScopeIDRefused: an empty component would collapse the
// derived schema/role name, so it must fail loudly rather than provision something
// degenerate. This is the guard that makes "ScopeID repeats the tenant" a rule
// instead of a convention.
func TestTenantScope_EmptyScopeIDRefused(t *testing.T) {
	if _, _, err := pgScopeNames(ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: ""}); err == nil {
		t.Fatal("an empty ScopeID must be refused for a durable scope")
	} else if !strings.Contains(err.Error(), "empty scope key component") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTenantScope_NameDiffersFromUserScope: belt-and-braces on the hash, asserted
// directly so a change to the canonical form is caught here rather than as a
// cross-scope data leak.
func TestTenantScope_NameDiffersFromUserScope(t *testing.T) {
	tSchema, tRole, err := pgScopeNames(ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	uSchema, uRole, err := pgScopeNames(ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if tSchema == uSchema || tRole == uRole {
		t.Errorf("tenant and user scopes with the same id derive the same identifiers (%s / %s) — they would share a database", tSchema, uSchema)
	}
}

// TestTenantScope_PostgresProvisionsSchemaAndRole is the verification the P4b plan
// asked for by name: the tenant scope creates a DATABASE ROLE, and getting that
// wrong fails at provisioning time with a permission error rather than at review
// time. The sqlite tier cannot show this — it provisions a file.
//
// Skipped without LOOMCYCLE_TEST_SQLMEM_PG_DSN; CI runs it in the go-postgres job.
func TestTenantScope_PostgresProvisionsSchemaAndRole(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	key := ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: "acme"}

	if _, err := m.Exec(ctx, key, `CREATE TABLE IF NOT EXISTS house (k TEXT PRIMARY KEY, v TEXT)`, nil, 0); err != nil {
		t.Fatalf("tenant scope must provision a schema + LOGIN role: %v\n"+
			"a permission error here means the DSN role lacks CREATEROLE", err)
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO house (k, v) VALUES ('style', 'tabs')`, nil, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := m.Query(ctx, key, `SELECT v FROM house WHERE k = 'style'`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0][0] != "tabs" {
		t.Fatalf("round trip failed: %+v", got.Rows)
	}

	schema, role, err := pgScopeNames(key)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.schemata WHERE schema_name = $1`, schema).Scan(&n); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if n != 1 {
		t.Errorf("schema %s was not created", schema)
	}
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = $1`, role).Scan(&n); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	if n != 1 {
		t.Errorf("LOGIN role %s was not created — the per-scope isolation is not in place", role)
	}

	// And it must be recorded for snapshot capture, or a tenant scope would be
	// silently omitted from a snapshot.
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM sqlmem_meta.scope_registry WHERE schema_name = $1`, schema).Scan(&n); err != nil {
		t.Fatalf("registry probe: %v", err)
	}
	if n != 1 {
		t.Errorf("scope %s is not in scope_registry — snapshot capture would skip it", schema)
	}
}

// TestTenantScope_PostgresDropScopeReclaimsIt: DropScope refuses an empty ScopeID,
// so the tenant scope's non-empty id is what makes it reclaimable at all.
func TestTenantScope_PostgresDropScopeReclaimsIt(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	key := ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: "acme"}

	if _, err := m.Exec(ctx, key, `CREATE TABLE IF NOT EXISTS t (v TEXT)`, nil, 0); err != nil {
		t.Fatalf("provision: %v", err)
	}
	removed, err := m.DropScope(ctx, key)
	if err != nil {
		t.Fatalf("DropScope: %v", err)
	}
	if !removed {
		t.Error("DropScope reported the tenant scope as absent")
	}
	schema, _, _ := pgScopeNames(key)
	var n int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.schemata WHERE schema_name = $1`, schema).Scan(&n); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if n != 0 {
		t.Errorf("schema %s survived DropScope", schema)
	}
}
