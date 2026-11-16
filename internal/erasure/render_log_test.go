package erasure_test

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// The render log of the tenant's shared and agent scopes keeps its rows when
// a subject is erased, and loses the subject's name (RFC GR §4.8): a dry run
// counts them, the erasure clears them, another subject's and another
// tenant's rows are untouched, and a scope without the table is no error.
func TestService_ErasureClearsTheSubjectFromTheRenderLog(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	dp := mgr.DocPlane()
	mk := func(key sqlmem.ScopeKey, actors ...string) {
		t.Helper()
		if _, err := dp.Exec(ctx, key, `CREATE TABLE document_renders (id TEXT PRIMARY KEY, actor TEXT NOT NULL)`, nil, 0); err != nil {
			t.Fatal(err)
		}
		for i, a := range actors {
			if _, err := dp.Exec(ctx, key, `INSERT INTO document_renders (id, actor) VALUES (?, ?)`, []any{key.ScopeID + string(rune('a'+i)), a}, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	tenant := sqlmem.ScopeKey{Tenant: "acme", Scope: "tenant", ScopeID: "acme"}
	agent := sqlmem.ScopeKey{Tenant: "acme", Scope: "agent", ScopeID: "writer"}
	other := sqlmem.ScopeKey{Tenant: "evil", Scope: "tenant", ScopeID: "evil"}
	// alice, an operator, downloaded one of bob's documents: she is
	// recorded in bob's own scope.
	bobs := sqlmem.ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "bob"}
	mk(tenant, "alice", "bob", "alice")
	mk(agent, "alice")
	mk(bobs, "alice", "bob")
	mk(other, "alice")
	// A scope that predates the render log.
	if _, err := dp.Exec(ctx, sqlmem.ScopeKey{Tenant: "acme", Scope: "agent", ScopeID: "old"}, `CREATE TABLE chunks (id TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	s := &erasure.Service{Store: st, SqlMem: mgr}

	dry, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted["document_render_actors"] != 4 {
		t.Errorf("dry run counts %d render rows, want 4 (errors %v)", dry.Deleted["document_render_actors"], dry.Errors)
	}
	res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["document_render_actors"] != 4 || len(res.Errors) != 0 {
		t.Errorf("erasure cleared %d render rows (errors %v), want 4", res.Deleted["document_render_actors"], res.Errors)
	}
	count := func(key sqlmem.ScopeKey, actor string) int {
		r, err := dp.Query(ctx, key, `SELECT COUNT(*) FROM document_renders WHERE actor = ?`, []any{actor})
		if err != nil {
			t.Fatal(err)
		}
		n, _ := r.Rows[0][0].(int64)
		return int(n)
	}
	if count(tenant, "alice") != 0 || count(agent, "alice") != 0 || count(bobs, "alice") != 0 || count(tenant, "") != 2 || count(tenant, "bob") != 1 || count(bobs, "bob") != 1 {
		t.Error("the subject's name is still in the render log, or the rows were lost")
	}
	if count(other, "alice") != 1 {
		t.Error("another tenant's render log was changed")
	}
}
