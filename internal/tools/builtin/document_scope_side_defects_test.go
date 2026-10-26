package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestDeriveUnits_TenantDocumentsAreFoundByTheirPathNames — the opt-in
// subtrees name documents by their Path-tree paths, and a tenant document's
// dirent lives at the dirent plane's tenant scope id (""), not the SQL Memory
// one. Listing at the SQL id found no names, so no tenant document was ever
// opted in.
func TestDeriveUnits_TenantDocumentsAreFoundByTheirPathNames(t *testing.T) {
	d, _, ctx := tenantDocFixture(t, "tnt", "policy", "leave")
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	})
	out := docRun(t, d, ctx, map[string]any{"op": "import_md", "scope": "tenant", "markdown": "# Leave\n\n## A\n\nsome policy text\n"})
	docRun(t, d, ctx, map[string]any{"op": "set_path", "scope": "tenant", "id": out["document_id"], "path": "/policies/leave"})

	rep, err := d.DeriveUnits(ctx, "tenant", &countingGenerator{}, DeriveUnitsOptions{Generator: config.UnitGeneratorConfig{
		Subtrees: []config.UnitSubtree{{Path: "/policies"}}}})
	if err != nil {
		t.Fatalf("DeriveUnits: %v", err)
	}
	if rep.DocumentsOptedIn != 1 || rep.Generated != 1 {
		t.Errorf("the tenant document named /policies/leave was not opted in: %+v", rep)
	}
}
