package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A Document chunk body shares the Memory keyspace, so the Memory tool reaches it
// by key. Its write ops must not: an edit there skips the Document tool's
// revision check, and in the tenant scope its second grant. And a tenant
// document is read with the same two grants on either tool.

// docRowsFixture is a Memory and a Document tool on one store, granted memory and
// SQL scopes as listed, with one prose chunk in each of the tenant and user
// scopes. It returns the chunk id per scope.
func docRowsFixture(t *testing.T, sqlScopes []string) (*Memory, *Document, context.Context, map[string]string) {
	t.Helper()
	d, vs, ctx := tenantDocFixture(t, "tnt", "quorum", "board")
	ctx = tools.WithRunID(ctx, "run_doc_rows")
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}, Consolidation: true}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
	})
	ids := map[string]string{}
	for _, scope := range []string{"tenant", "user"} {
		doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": scope, "title": "Norms", "path": "/norms/" + scope})
		ids[scope] = docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": scope,
			"document_id": doc["document_id"], "title": "Quorum", "body": "quorum of the board"})["id"].(string)
	}
	// The grants under test apply to the Memory calls, made after the seeding.
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: sqlScopes}
	})
	return &Memory{Store: &classFilterStore{vs}, SqlMem: d.SqlMem, Embedder: d.Embedder, MaxValueBytes: 65536}, d, ctx, ids
}

// classFilterStore applies the class predicates the shared fake leaves to the
// real stores (namespace and document-class exclusion, provenance), so a test
// that a search leaves documents out can fail.
type classFilterStore struct{ *vectorStore }

func (c *classFilterStore) MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string,
	f store.MemorySearchFilter, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	rows, err := c.vectorStore.MemoryEmbedSearch(ctx, tenantID, scope, scopeID, f, query, topK)
	if err != nil {
		return nil, err
	}
	out := rows[:0:0]
	for _, r := range rows {
		prov, _ := c.MemoryProvenanceGet(ctx, tenantID, scope, scopeID, r.Key)
		switch {
		case f.ExcludeKeyPrefix != "" && strings.HasPrefix(r.Key, f.ExcludeKeyPrefix):
		case f.ExcludeDocumentPrefix != "" && strings.HasPrefix(r.Key, f.ExcludeDocumentPrefix) && prov.Origin == "":
		case f.Provenance == store.ProvenanceAbsent && prov.Origin != "":
		case f.Provenance == store.ProvenanceRequired && prov.Origin == "":
		default:
			out = append(out, r)
		}
	}
	return out, nil
}

func memRun(t *testing.T, m *Memory, ctx context.Context, body string) tools.Result {
	t.Helper()
	r, err := m.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute %s: %v", body, err)
	}
	return r
}

func chunkBodyNow(t *testing.T, d *Document, ctx context.Context, scope, id string) any {
	t.Helper()
	full := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
	})
	return docRun(t, d, full, map[string]any{"op": "get_chunk", "scope": scope, "id": id})["body"]
}

func wantDocRowRefusal(t *testing.T, r tools.Result, category tools.ErrorCategory, mentions string) {
	t.Helper()
	if !r.IsError || r.Error == nil || r.Error.Category != category {
		t.Fatalf("result = %s (error %+v), want a %s refusal", r.Text, r.Error, category)
	}
	if !strings.Contains(r.Text, mentions) {
		t.Errorf("refusal %q does not mention %q", r.Text, mentions)
	}
}

func TestMemorySet_RefusesADocumentChunkBody(t *testing.T) {
	m, d, ctx, ids := docRowsFixture(t, []string{"agent", "user", "tenant"})
	for _, scope := range []string{"tenant", "user"} {
		key := chunkBodyKey(ids[scope])
		for _, op := range []struct{ name, body string }{
			{"set", `{"op":"set","scope":"` + scope + `","key":"` + key + `","value":"overwritten"}`},
			{"incr", `{"op":"incr","scope":"` + scope + `","key":"` + key + `"}`},
			{"merge", `{"op":"merge","scope":"` + scope + `","key":"` + key + `","value":{"body":"overwritten"}}`},
			{"append_dedupe", `{"op":"append_dedupe","scope":"` + scope + `","key":"` + key + `","value":"overwritten"}`},
			{"bounded_list", `{"op":"bounded_list","scope":"` + scope + `","key":"` + key + `","value":"overwritten","limit":3}`},
		} {
			t.Run(scope+"/"+op.name, func(t *testing.T) {
				wantDocRowRefusal(t, memRun(t, m, ctx, op.body), tools.CategoryValidation, "Document tool")
				if got := chunkBodyNow(t, d, ctx, scope, ids[scope]); got != "quorum of the board" {
					t.Errorf("chunk body is now %v", got)
				}
			})
		}
	}
}

func TestMemoryDelete_RefusesADocumentChunkBody(t *testing.T) {
	m, d, ctx, ids := docRowsFixture(t, []string{"agent", "user", "tenant"})
	for _, scope := range []string{"tenant", "user"} {
		key := chunkBodyKey(ids[scope])
		for _, op := range []struct{ name, body string }{
			{"delete", `{"op":"delete","scope":"` + scope + `","key":"` + key + `"}`},
			{"supersede", `{"op":"supersede","scope":"` + scope + `","key":"` + key + `"}`},
		} {
			t.Run(scope+"/"+op.name, func(t *testing.T) {
				if op.name == "supersede" {
					if r := memRun(t, m, ctx, `{"op":"cursor_lease","scope":"`+scope+`","lease_ttl_ms":60000}`); r.IsError {
						t.Fatalf("cursor_lease: %s", r.Text)
					}
				}
				wantDocRowRefusal(t, memRun(t, m, ctx, op.body), tools.CategoryValidation, "Document tool")
				if got := chunkBodyNow(t, d, ctx, scope, ids[scope]); got != "quorum of the board" {
					t.Errorf("chunk body is now %v", got)
				}
			})
		}
	}
}

// A consolidated fact lives in a chunk too, and the consolidation pass retires
// and removes one by the id recall gave it. Those rows carry provenance, and the
// fact-tier operations on them keep working.
func TestMemory_FactOpsByChunkIDStillWork(t *testing.T) {
	m, d, ctx := supersedeFixture(t)
	doc := newEntityDoc(t, d, ctx)
	retireID := writeFact(t, d, ctx, doc, "memory/fact/dave-works-at-shop", "Dave works at the shop.")
	dropID := writeFact(t, d, ctx, doc, "memory/fact/dave-likes-tea", "Dave likes tea.")
	for _, id := range []string{retireID, dropID} {
		prov, err := m.Store.MemoryProvenanceGet(ctx, "tnt", store.MemoryScopeUser, "u1", chunkBodyKey(id))
		if err != nil || prov.Origin == "" {
			t.Fatalf("the fixture fact %s carries no provenance (%v): the test would not test a fact", id, err)
		}
	}
	leaseUser(t, m, ctx)
	if r := memRun(t, m, ctx, `{"op":"supersede","scope":"user","key":"`+chunkBodyKey(retireID)+`"}`); r.IsError {
		t.Errorf("supersede of a fact by its chunk id: %s", r.Text)
	}
	if r := memRun(t, m, ctx, `{"op":"delete","scope":"user","key":"`+chunkBodyKey(dropID)+`"}`); r.IsError || !strings.Contains(r.Text, `"deleted":true`) {
		t.Errorf("delete of a fact by its chunk id: %s", r.Text)
	}
}

func TestMemorySearch_TenantDocumentsNeedBothGrants(t *testing.T) {
	m, _, ctx, ids := docRowsFixture(t, []string{"agent", "user"}) // no sql_scopes: [tenant]
	tenantKey := chunkBodyKey(ids["tenant"])
	if r := memRun(t, m, ctx, `{"op":"set","scope":"tenant","key":"notes/quorum","value":"quorum of the board","embed":true}`); r.IsError {
		t.Fatalf("seed a tenant note: %s", r.Text)
	}

	t.Run("an explicit documents selector is a permission error", func(t *testing.T) {
		for _, body := range []string{
			`{"op":"search","scope":"tenant","query":"quorum board","sources":["documents"]}`,
			`{"op":"search","scope":"tenant","query":"quorum board","prefix":"doc.chunk:"}`,
			`{"op":"recall","scope":"tenant","query":"quorum board","sources":["documents"]}`,
			`{"op":"get","scope":"tenant","key":"` + tenantKey + `"}`,
			`{"op":"list","scope":"tenant","prefix":"doc.chunk:"}`,
		} {
			wantDocRowRefusal(t, memRun(t, m, ctx, body), tools.CategoryPermission, "sql_scopes: [tenant]")
		}
	})
	t.Run("a selector-free read leaves documents out", func(t *testing.T) {
		r := memRun(t, m, ctx, `{"op":"search","scope":"tenant","query":"quorum board"}`)
		if r.IsError {
			t.Fatalf("search: %s", r.Text)
		}
		if strings.Contains(r.Text, tenantKey) {
			t.Errorf("selector-free search returned the tenant document: %s", r.Text)
		}
		if !strings.Contains(r.Text, "notes/quorum") {
			t.Errorf("selector-free search lost the tenant note: %s", r.Text)
		}
		l := memRun(t, m, ctx, `{"op":"list","scope":"tenant"}`)
		if l.IsError || strings.Contains(l.Text, tenantKey) || !strings.Contains(l.Text, "notes/quorum") {
			t.Errorf("list = %s, want the note and not the document", l.Text)
		}
	})
	t.Run("user-scope documents need no SQL grant", func(t *testing.T) {
		r := memRun(t, m, ctx, `{"op":"search","scope":"user","query":"quorum board","sources":["documents"]}`)
		if r.IsError || !strings.Contains(r.Text, chunkBodyKey(ids["user"])) {
			t.Errorf("user-scope document search = %s", r.Text)
		}
	})
}
