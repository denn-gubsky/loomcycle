package sqlmem

import (
	"context"
	"testing"
)

// scopeExistsContract: a scope nobody has used is absent, and asking does not
// create it; once a statement has run in it, it is present — including under
// an id the sqlite tier sanitizes into its file name, which is why ListScopes
// (whose keys are those file names) cannot answer this. The run scope is
// refused.
func scopeExistsContract(t *testing.T, m *Manager) {
	t.Helper()
	ctx := context.Background()
	used := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice@example.com"}
	unused := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "bob@example.com"}

	for i := 0; i < 2; i++ { // a second ask proves the first did not provision
		if ok, err := m.ScopeExists(ctx, used); err != nil || ok {
			t.Fatalf("ask %d before use: exists=%v err=%v, want false", i, ok, err)
		}
	}
	if keys, err := m.ListScopes(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("scopes after only asking = %v (err %v); asking must not provision", keys, err)
	}

	if _, err := m.Exec(ctx, used, `CREATE TABLE t (id INTEGER)`, nil, 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if ok, err := m.ScopeExists(ctx, used); err != nil || !ok {
		t.Errorf("after use: exists=%v err=%v, want true", ok, err)
	}
	if ok, err := m.ScopeExists(ctx, unused); err != nil || ok {
		t.Errorf("an unused sibling: exists=%v err=%v, want false", ok, err)
	}
	if _, err := m.ScopeExists(ctx, ScopeKey{Tenant: "acme", Scope: runScope, ScopeID: "r1"}); err == nil {
		t.Error("the run scope was accepted; ScopeExists is for durable scopes")
	}
}

func TestScopeExists_SQLiteReportsOnlyUsedScopesWithoutProvisioning(t *testing.T) {
	scopeExistsContract(t, newTestManager(t, Config{}))
}

func TestScopeExists_PostgresReportsOnlyUsedScopesWithoutProvisioning(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	scopeExistsContract(t, m)
}
