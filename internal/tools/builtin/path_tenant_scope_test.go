package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestPath_TenantScopeNeedsBothGrants: a tenant-scope Path call needs the same two
// grants a tenant document does. Without them an agent holding only tools:[Path]
// could rename or unname any shared document in the tenant.
func TestPath_TenantScopeNeedsBothGrants(t *testing.T) {
	p, base, s := pathFixture(t)
	seedDirent(t, s, "tnt", "tenant", "", "/notes/", "shared", "document")

	grant := func(ctx context.Context, mem, sql bool) context.Context {
		return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
			if mem {
				rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
			}
			if sql {
				rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
			}
		})
	}
	for _, c := range []struct {
		name     string
		mem, sql bool
	}{{"no grant", false, false}, {"memory_scopes only", true, false}, {"sql_scopes only", false, true}} {
		ctx := grant(base, c.mem, c.sql)
		for _, req := range []string{
			`{"op":"ls","scope":"tenant","path":"/notes"}`,
			`{"op":"rm","scope":"tenant","path":"/notes/shared"}`,
			`{"op":"mv","scope":"tenant","path":"/notes/shared","to":"/notes/moved"}`,
		} {
			_, r := pathExec(t, p, ctx, req)
			if !r.IsError {
				t.Fatalf("%s: tenant-scope Path was allowed: %s", c.name, req)
			}
			if r.Error == nil || r.Error.Category != tools.CategoryPermission {
				t.Errorf("%s: refusal is not a permission error: %+v", c.name, r)
			}
		}
	}
	if _, err := s.DirentGet(context.Background(), "tnt", "tenant", "", "/notes/", "shared"); err != nil {
		t.Fatalf("a refused call changed the tree: %v", err)
	}

	ctx := grant(base, true, true)
	if _, r := pathExec(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/notes"}`); r.IsError {
		t.Fatalf("both grants: ls refused: %s", r.Text)
	}
	if _, r := pathExec(t, p, ctx, `{"op":"mv","scope":"tenant","path":"/notes/shared","to":"/notes/moved"}`); r.IsError {
		t.Fatalf("both grants: mv refused: %s", r.Text)
	}
	// The agent and user trees stay ungated, as they have always been.
	if _, r := pathExec(t, p, base, `{"op":"ls","scope":"agent","path":"/"}`); r.IsError {
		t.Errorf("agent scope needs no grant: %s", r.Text)
	}
}

// TestPath_SchemaSaysTenantNeedsBothGrants: the model reads the scope argument's
// description before it calls; it must not learn the tenant tree is open to it.
func TestPath_SchemaSaysTenantNeedsBothGrants(t *testing.T) {
	for _, want := range []string{"memory_scopes", "sql_scopes"} {
		if !strings.Contains(pathInputSchema, want) {
			t.Errorf("Path's scope description does not say tenant needs %s", want)
		}
	}
}
