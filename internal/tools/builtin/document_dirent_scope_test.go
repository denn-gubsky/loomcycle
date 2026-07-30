package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestDocument_TenantDirentIsReachableFromThePathTool is the cross-tool agreement
// the Path tree depends on.
//
// The Document tool writes a dirent when it names a document; the Path tool reads
// dirents. They have to key them identically or a document exists in one tool's
// view and not the other's — and nothing errors, because a missing dirent is
// indistinguishable from a document that was never named. The whole failure is an
// empty listing.
//
// Tenant scope is where they diverged. SQL Memory REQUIRES a non-empty scope id
// (it becomes half of a schema name and a database role), so the tenant scope key
// carries the tenant there; the dirent plane's convention is the opposite —
// scope_id is empty for tenant and the tenant_id column carries the identity,
// which is what Path, Memory and the store's own schema default all do. Writing
// the SQL-Memory id into a dirent leaks a storage-engine constraint into a plane
// that does not share it.
func TestDocument_TenantDirentIsReachableFromThePathTool(t *testing.T) {
	for _, scope := range []string{"agent", "user", "tenant"} {
		t.Run(scope, func(t *testing.T) {
			d, ctx, st := documentDirentFixture(t)
			p := &Path{Store: st}

			create, _ := json.Marshal(map[string]any{
				"op": "create_document", "scope": scope,
				"title": "probe", "path": "/probe",
			})
			res, err := d.Execute(ctx, create)
			if err != nil || res.IsError {
				t.Fatalf("create_document: %v %s", err, res.Text)
			}

			ls, _ := json.Marshal(map[string]any{"op": "ls", "scope": scope, "path": "/"})
			lres, err := p.Execute(ctx, ls)
			if err != nil || lres.IsError {
				t.Fatalf("path ls: %v %s", err, lres.Text)
			}
			var listing struct {
				Entries []struct {
					Name string `json:"name"`
				} `json:"entries"`
			}
			if err := json.Unmarshal([]byte(lres.Text), &listing); err != nil {
				t.Fatalf("decode listing: %v (%s)", err, lres.Text)
			}
			if len(listing.Entries) != 1 || listing.Entries[0].Name != "probe" {
				t.Fatalf("Path cannot see the document the Document tool just named at scope=%s: %s",
					scope, lres.Text)
			}
		})
	}
}

// TestDocument_TenantPathLookupFindsTheDocument: the same divergence, from the
// other side. Document resolves `path` through a dirent read of its own, so a
// dirent written at one coordinate and read at another makes a document
// unreachable by the very path it was created with — while remaining reachable by
// id, which is how this hides.
func TestDocument_TenantPathLookupFindsTheDocument(t *testing.T) {
	d, ctx, st := documentDirentFixture(t)
	p := &Path{Store: st}

	create, _ := json.Marshal(map[string]any{
		"op": "create_document", "scope": "tenant", "title": "ontology", "path": "/memory/ontology",
	})
	res, _ := d.Execute(ctx, create)
	if res.IsError {
		t.Fatalf("create_document: %s", res.Text)
	}
	var created struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &created)

	get, _ := json.Marshal(map[string]any{
		"op": "get_document", "scope": "tenant", "path": "/memory/ontology",
	})
	gres, _ := d.Execute(ctx, get)
	if gres.IsError {
		t.Fatalf("get_document by path: %s", gres.Text)
	}
	var got struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(gres.Text), &got)
	if got.DocumentID != created.DocumentID {
		t.Errorf("path lookup resolved %q, want the created %q", got.DocumentID, created.DocumentID)
	}

	// And the intermediate directory is walkable, which is what a browser does.
	ls, _ := json.Marshal(map[string]any{"op": "ls", "scope": "tenant", "path": "/memory"})
	lres, _ := p.Execute(ctx, ls)
	if lres.IsError {
		t.Fatalf("path ls /memory: %s", lres.Text)
	}
	var listing struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	_ = json.Unmarshal([]byte(lres.Text), &listing)
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "ontology" {
		t.Errorf("ls /memory did not list the document: %s", lres.Text)
	}
}

// documentDirentFixture builds a Document + a store sharing one ctx identity, so
// both tools resolve the same scopes from the same run.
func documentDirentFixture(t *testing.T) (*Document, context.Context, store.Store) {
	t.Helper()
	d, ctx, st := documentFixture(t)
	// scope=agent needs an agent name on the run; user + tenant come from the
	// RunIdentity documentFixture already stamps. Tenant scope additionally needs
	// BOTH grants — a tenant document is shared by every agent in the tenant, so it
	// is opt-in; this is the state a granted agent runs in.
	ctx = tools.WithAgentName(ctx, "curator")
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "tenant"}})
	ctx = tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{
		AllowedScopes: []string{"agent", "user", "tenant"}})
	return d, ctx, st
}
