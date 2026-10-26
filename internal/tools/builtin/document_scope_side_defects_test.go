package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// TestPlacement_TypesForSubjectReadsTheTenantTree — the conflict guard asks which
// entity types the caller's own partition already records a subject under. For a
// tenant caller the Memory scope id is "", which is not a SQL Memory scope id; the
// key must come from the same mapping the Document tool uses, or the guard reads
// no tree and can never fire for a tenant-scope pass.
func TestPlacement_TypesForSubjectReadsTheTenantTree(t *testing.T) {
	d, vs, ctx := tenantDocFixture(t, "tnt", "dave")
	doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "entities", "path": "/t/entities"})
	docRun(t, d, ctx, map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": doc["document_id"],
		"natural_key": "person:dave", "title": "Dave", "body": "Dave", "type": "person"})
	m := &Memory{Store: vs, SqlMem: d.SqlMem}
	got := m.typesForSubject(ctx, store.MemoryScopeTenant, "", "Dave")
	if len(got) != 1 || got[0] != "person" {
		t.Errorf("types recorded for Dave in the tenant tree = %v, want [person]", got)
	}
}
