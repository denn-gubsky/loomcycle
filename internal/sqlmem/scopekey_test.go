package sqlmem

import (
	"context"
	"strings"
	"testing"
)

// TestScopeKey_SeparatorCannotCollideTwoScopes pins the identity invariant the
// postgres tier derives its schema AND its LOGIN role from.
//
// pgScopeNames hashes a 0x1f-joined triple. A component containing that byte makes
// the join ambiguous, so ("a\x1fb","c","d"), ("a","b\x1fc","d") and
// ("a","b","c\x1fd") all produced the SAME schema and the SAME role — three
// scopes sharing one database, which is the exact isolation the per-scope role
// exists to provide.
//
// The sqlite tier never had this problem: its sanitize() appends a hash of the
// original input, so distinct ids cannot converge. The two tiers made different
// guarantees about the same keys, and only the weaker one was security-critical.
func TestScopeKey_SeparatorCannotCollideTwoScopes(t *testing.T) {
	variants := []ScopeKey{
		{Tenant: "a\x1fb", Scope: "c", ScopeID: "d"},
		{Tenant: "a", Scope: "b\x1fc", ScopeID: "d"},
		{Tenant: "a", Scope: "b", ScopeID: "c\x1fd"},
	}
	for _, k := range variants {
		if _, _, err := pgScopeNames(k); err == nil {
			t.Errorf("pgScopeNames accepted %+q — it derives a schema and a LOGIN role "+
				"that another scope also derives", k)
		} else if !strings.Contains(err.Error(), "separator") {
			t.Errorf("unexpected error for %+q: %v", k, err)
		}
	}
	// A legitimate key is unaffected.
	if _, _, err := pgScopeNames(ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice"}); err != nil {
		t.Errorf("a normal key was refused: %v", err)
	}
}

// TestScopeKey_ValidatedAtTheManagerEntryPoints — the derivations that consume a
// ScopeKey are spread across three subsystems (postgres schema/role names, the GC
// debounce key, and the transaction-registry key), and only the first returns an
// error. Validating where a key ENTERS the manager is what lets the other two keep
// building plain strings and still be safe.
func TestScopeKey_ValidatedAtTheManagerEntryPoints(t *testing.T) {
	mgr, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	bad := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice\x1fagent\x1fcurator"}

	if _, err := mgr.Exec(ctx, bad, `CREATE TABLE t (a TEXT)`, nil, 0); err == nil {
		t.Error("Exec accepted a separator-bearing scope id")
	}
	if _, err := mgr.Query(ctx, bad, `SELECT 1`, nil); err == nil {
		t.Error("Query accepted a separator-bearing scope id")
	}
	if _, err := mgr.DropScope(ctx, bad); err == nil {
		t.Error("DropScope accepted a separator-bearing scope id")
	}

	// The same ops on a clean key still work, so the guard has not broken the tier.
	ok := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice"}
	if _, err := mgr.Exec(ctx, ok, `CREATE TABLE t (a TEXT)`, nil, 0); err != nil {
		t.Errorf("a normal Exec was refused: %v", err)
	}
}
