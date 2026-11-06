package erasure_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// On sqlite a scope that holds documents is two files, <scope>.db and
// <scope>.doc.db, and the documents live only in the second. Erasing the subject
// must take both, and count the scope once.
func TestService_ErasureRemovesBothFilesOfASqliteScope(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	mgr, err := sqlmem.New(sqlmem.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	s := &erasure.Service{Store: st, SqlMem: mgr}
	ctx := context.Background()

	key := sqlmem.ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice"}
	if _, err := mgr.Exec(ctx, key, `CREATE TABLE notes (body TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.DocPlane().Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY, title TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.DocPlane().Exec(ctx, key, `INSERT INTO chunks VALUES ('c1', 'about alice')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	// Mark alice's document file as her scope's, then leave rows in her agent
	// file under a document table's name: the move quarantines them in a file
	// beside the scope, which the erasure must take too.
	if err := mgr.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY, title TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Exec(ctx, key, `INSERT INTO chunks VALUES ('late', 'about alice, later')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	// A scope that only ever held documents has no agent file at all.
	bob := sqlmem.ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "bob"}
	if _, err := mgr.DocPlane().Exec(ctx, bob, `CREATE TABLE chunks (id TEXT PRIMARY KEY)`, nil, 0); err != nil {
		t.Fatal(err)
	}

	files := func(id string) []string {
		var out []string
		_ = filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
			if err == nil && !de.IsDir() && strings.HasPrefix(filepath.Base(p), id+".") {
				out = append(out, filepath.Base(p))
			}
			return nil
		})
		return out
	}
	hasDoc := func(names []string) bool {
		for _, n := range names {
			if strings.HasSuffix(n, ".doc.db") {
				return true
			}
		}
		return false
	}
	if f := files("alice"); !hasDoc(f) || !strings.Contains(strings.Join(f, " "), ".leftover-") {
		t.Fatalf("alice's files before the erasure = %v; want a document file and a quarantine file", f)
	}

	for _, subject := range []string{"alice", "bob"} {
		rep, err := s.Report(ctx, "acme", subject)
		if err != nil {
			t.Fatal(err)
		}
		if got := rep.Tier1.Counts["sql_memory_scopes"]; got != 1 {
			t.Errorf("%s: report counts %d SQL scopes, want 1 (one scope, whatever its files)", subject, got)
		}
		res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: subject, DryRun: false, Confirm: subject})
		if err != nil {
			t.Fatal(err)
		}
		if res.Deleted["sql_memory_scopes"] != 1 {
			t.Errorf("%s: deleted sql_memory_scopes = %d, want 1; errors=%v", subject, res.Deleted["sql_memory_scopes"], res.Errors)
		}
		if f := files(subject); len(f) != 0 {
			t.Errorf("%s: files left after the erasure: %v", subject, f)
		}
	}
}
