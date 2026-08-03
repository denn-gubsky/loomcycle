package sqlmem

import (
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
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

// TestScopeKey_EveryEntryPointValidates is a drift test over the SOURCE, and it
// exists because the first attempt at this guard covered three entry points out of
// seven.
//
// The separator invariant is enforced at the doors rather than in one choke point
// (there isn't one), which makes it exactly the kind of rule that rots: a new
// Manager method taking a ScopeKey is easy to add and easy to add without the
// check. Reviewing found the gap once; this makes the next one a test failure.
//
// A method may opt out only by appearing in scopeKeyValidationExempt WITH a reason,
// so skipping the check is a deliberate, reviewable act.
func TestScopeKey_EveryEntryPointValidates(t *testing.T) {
	// touch is called only from Query / Exec / BeginTxn, all of which validate
	// first, so it inherits the guarantee rather than restating it — and it returns
	// nothing, so it could not report a violation anyway.
	scopeKeyValidationExempt := map[string]string{
		"touch": "internal; reached only from Query/Exec/BeginTxn, which validate first",
	}

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			// Only methods on *Manager — the public surface a ScopeKey enters through.
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Manager" {
				continue
			}
			var takesKey bool
			for _, p := range fn.Type.Params.List {
				if id, ok := p.Type.(*ast.Ident); ok && id.Name == "ScopeKey" {
					takesKey = true
				}
			}
			if !takesKey {
				continue
			}
			checked++
			if reason, exempt := scopeKeyValidationExempt[fn.Name.Name]; exempt {
				t.Logf("%s: exempt (%s)", fn.Name.Name, reason)
				continue
			}
			var body strings.Builder
			if err := printer.Fprint(&body, fset, fn.Body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.String(), ".validate()") {
				t.Errorf("Manager.%s takes a ScopeKey but never calls validate() — a "+
					"separator-bearing key would derive storage shared with another scope. "+
					"Add the check, or add %q to scopeKeyValidationExempt with a reason.",
					fn.Name.Name, fn.Name.Name)
			}
		}
	}
	// Guard the guard: if the AST walk silently matched nothing, the test would
	// pass while checking nothing at all.
	if checked < 5 {
		t.Fatalf("only found %d Manager methods taking a ScopeKey — the AST walk is "+
			"probably broken, so this test is not actually checking anything", checked)
	}
}

// TestScopeKey_ProvisioningPathsRefuse checks the three entry points the first
// version of this guard missed, behaviourally rather than by source shape.
//
// BeginTxn and RestoreScope PROVISION a scope (schema + LOGIN role on postgres);
// ExportScope reads one, and a collision there is the worst of the three — it
// dumps another scope's rows into this scope's archive, so the wrong data lands in
// a backup and is replayed on restore.
func TestScopeKey_ProvisioningPathsRefuse(t *testing.T) {
	mgr, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	bad := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice\x1fuser\x1fbob"}

	if _, err := mgr.BeginTxn(ctx, BuildTxnID("r1", bad.Scope, bad.ScopeID), "r1", bad); err == nil {
		t.Error("BeginTxn accepted a separator-bearing key and provisioned the scope")
	}
	if _, err := mgr.ExportScope(ctx, bad); err == nil {
		t.Error("ExportScope accepted a separator-bearing key — another scope's rows " +
			"could be dumped into this archive")
	}
	if err := mgr.RestoreScope(ctx, bad, &ScopeDump{}); err == nil {
		t.Error("RestoreScope accepted a separator-bearing key")
	}

	// A clean key still opens a transaction, so the guard has not broken the path.
	ok := ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice"}
	depth, err := mgr.BeginTxn(ctx, BuildTxnID("r2", ok.Scope, ok.ScopeID), "r2", ok)
	if err != nil {
		t.Fatalf("a normal BeginTxn was refused: %v", err)
	}
	if depth != 1 {
		t.Errorf("depth = %d, want 1", depth)
	}
	if _, err := mgr.RollbackTxn(BuildTxnID("r2", ok.Scope, ok.ScopeID)); err != nil {
		t.Errorf("rollback: %v", err)
	}
}
