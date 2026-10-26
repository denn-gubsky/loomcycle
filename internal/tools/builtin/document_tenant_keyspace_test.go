package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tenant document's chunk bodies, units and embeddings belong in the Memory
// plane's tenant keyspace — scope_id "", with tenant_id carrying the identity —
// because that is where a tenant-scope Memory read looks. An older binary wrote
// them at the SQL Memory scope id (the tenant, or "default" for the empty
// tenant), where the Memory tool never reads.

// tenantDocFixture is mermaidDocFixture run as tenant `tenant`, with both tenant
// grants (a tenant document needs memory_scopes AND sql_scopes).
func tenantDocFixture(t *testing.T, tenant string, vocab ...string) (*Document, *vectorStore, context.Context) {
	t.Helper()
	d, vs, ctx := mermaidDocFixture(t, vocab...)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: tenant})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
	})
	return d, vs, ctx
}

func docRun(t *testing.T, d *Document, ctx context.Context, v map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	r, err := d.Execute(ctx, b)
	if err != nil || r.IsError {
		t.Fatalf("%v: %v %s", v["op"], err, r.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(r.Text), &out)
	return out
}

// tenantChunk creates a tenant document with one content chunk and returns the
// chunk id.
func tenantChunk(t *testing.T, d *Document, ctx context.Context, body string) string {
	t.Helper()
	doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "Board norms", "path": "/norms/board"})
	return docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": "tenant",
		"document_id": doc["document_id"], "title": "Quorum", "body": body})["id"].(string)
}

func TestDocumentTenantBodies_LiveWhereMemoryReadsTenant(t *testing.T) {
	for _, tc := range []struct{ name, tenant string }{
		{"named tenant", "tnt"},
		// The default tenant: SQL Memory keys it "default", the Memory plane "".
		{"default tenant", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, vs, ctx := tenantDocFixture(t, tc.tenant, "quorum", "board")
			id := tenantChunk(t, d, ctx, "quorum of the board")
			bodyKey := chunkBodyKey(id)
			legacy := sqlmem.ScopeTenant(tc.tenant)

			if _, err := vs.MemoryGet(context.Background(), tc.tenant, store.MemoryScopeTenant, "", bodyKey); err != nil {
				t.Errorf("body not at the Memory tenant keyspace (%q, tenant, \"\"): %v", tc.tenant, err)
			}
			if _, err := vs.MemoryGet(context.Background(), tc.tenant, store.MemoryScopeTenant, legacy, bodyKey); err == nil {
				t.Errorf("body still written at the SQL Memory scope id %q", legacy)
			}
			if _, err := vs.MemoryEmbedGet(context.Background(), tc.tenant, store.MemoryScopeTenant, "", bodyKey); err != nil {
				t.Errorf("embedding not at the Memory tenant keyspace: %v", err)
			}

			// And the Memory tool, reading its own tenant scope, finds it.
			m := &Memory{Store: vs, Embedder: d.Embedder}
			r, err := m.Execute(ctx, json.RawMessage(`{"op":"get","scope":"tenant","key":"`+bodyKey+`"}`))
			if err != nil || r.IsError {
				t.Fatalf("Memory get of the tenant chunk body: %v %s", err, r.Text)
			}
			if !strings.Contains(r.Text, "quorum of the board") {
				t.Errorf("Memory get returned %s, want the chunk body", r.Text)
			}
		})
	}
}

func TestMemorySearch_TenantScopeReturnsTenantDocuments(t *testing.T) {
	d, vs, ctx := tenantDocFixture(t, "tnt", "quorum", "board")
	id := tenantChunk(t, d, ctx, "quorum of the board")
	m := &Memory{Store: vs, Embedder: d.Embedder}
	r, err := m.Execute(ctx, json.RawMessage(`{"op":"search","scope":"tenant","query":"quorum board","sources":["documents"]}`))
	if err != nil || r.IsError {
		t.Fatalf("search: %v %s", err, r.Text)
	}
	var out struct {
		Entries []struct {
			Key     string `json:"key"`
			Kind    string `json:"kind"`
			ChunkID string `json:"chunk_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(r.Text), &out); err != nil {
		t.Fatal(err)
	}
	for _, e := range out.Entries {
		if e.ChunkID == id && e.Kind == string(store.MemoryRowDocument) {
			return
		}
	}
	t.Errorf("a tenant-scope Memory search over documents did not return the tenant chunk %s: %s", id, r.Text)
}

// moveToLegacyKeyspace puts a body row back where an older binary wrote it.
func moveToLegacyKeyspace(t *testing.T, vs *vectorStore, tenant, key string, prov store.MemoryProvenance) {
	t.Helper()
	bg := context.Background()
	e, err := vs.MemoryGet(bg, tenant, store.MemoryScopeTenant, "", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := vs.MemorySetProvenance(bg, tenant, store.MemoryScopeTenant, sqlmem.ScopeTenant(tenant), key, e.Value, 0, prov); err != nil {
		t.Fatal(err)
	}
	if _, err := vs.MemoryDelete(bg, tenant, store.MemoryScopeTenant, "", key); err != nil {
		t.Fatal(err)
	}
}

// During a rolling upgrade a replica on the old binary still writes the old
// keyspace, so for one release a tenant body read that misses retries there.
func TestDocumentTenantBodyRead_FallsBackToTheLegacyScopeID(t *testing.T) {
	for _, tc := range []struct{ name, tenant string }{{"named tenant", "tnt"}, {"default tenant", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			d, vs, ctx := tenantDocFixture(t, tc.tenant, "quorum", "board")
			id := tenantChunk(t, d, ctx, "quorum of the board")
			moveToLegacyKeyspace(t, vs, tc.tenant, chunkBodyKey(id), store.MemoryProvenance{Origin: "consolidator"})

			got := docRun(t, d, ctx, map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
			if got["body"] != "quorum of the board" {
				t.Fatalf("get_chunk body = %v, want the body still at the old scope id", got["body"])
			}

			// A body rewrite lands in the new keyspace and keeps the provenance the
			// old row carried, so a fact edited mid-upgrade stays a fact.
			rev := got["revision"]
			docRun(t, d, ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": rev, "body": "quorum of two thirds"})
			prov, err := vs.MemoryProvenanceGet(context.Background(), tc.tenant, store.MemoryScopeTenant, "", chunkBodyKey(id))
			if err != nil {
				t.Fatalf("rewritten body not in the new keyspace: %v", err)
			}
			if prov.Origin != "consolidator" {
				t.Errorf("rewritten body origin = %q, want the old row's %q", prov.Origin, "consolidator")
			}
		})
	}
}

// The admin re-embed, backfill and purge read a tenant scope's rows at the
// Memory tenant keyspace ("") and derive each chunk's index text from its SQL
// Memory tree, which is keyed by the tenant. Building the SQL key from the
// Memory scope id would address no tree, and a tenant chunk would be re-embedded
// without the header naming where it sits.
func TestChunkIndexTextForRow_TenantRowReadsTheTenantTree(t *testing.T) {
	d, vs, ctx := tenantDocFixture(t, "tnt", "quorum", "board")
	id := tenantChunk(t, d, ctx, "quorum of the board")
	row, err := vs.MemoryGet(context.Background(), "tnt", store.MemoryScopeTenant, "", chunkBodyKey(id))
	if err != nil {
		t.Fatal(err)
	}
	want, err := vs.MemoryEmbedGet(context.Background(), "tnt", store.MemoryScopeTenant, "", chunkBodyKey(id))
	if err != nil {
		t.Fatal(err)
	}
	got, isChunk := ChunkIndexTextForRow(context.Background(), d.SqlMem, "tnt", store.MemoryScopeTenant, "", row)
	if !isChunk || got != want.EmbedText {
		t.Errorf("index text for a tenant row = %q (chunk %v), want what the write path embedded: %q", got, isChunk, want.EmbedText)
	}
}

// TestGraphRecall_TenantChunkCarriesItsBody — graph_recall's body excerpt reads a
// tenant chunk's body from the Memory tenant keyspace, where it now lives.
func TestGraphRecall_TenantChunkCarriesItsBody(t *testing.T) {
	d, _, ctx := tenantDocFixture(t, "tnt", "quorum", "board")
	doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "Board", "path": "/norms/graph"})
	seed := docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"],
		"title": "Board", "body": "the board"})["id"].(string)
	note := docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"],
		"title": "Quorum", "body": "quorum of the board"})["id"].(string)
	docRun(t, d, ctx, map[string]any{"op": "link_chunks", "scope": "tenant", "document_id": doc["document_id"],
		"from_id": seed, "to_id": note, "kind": "notes"})
	out := docRun(t, d, ctx, map[string]any{"op": "graph_recall", "scope": "tenant", "seed_ids": []string{seed}, "hops": 1})
	rows, _ := out["chunks"].([]any)
	for _, r := range rows {
		if row := r.(map[string]any); row["title"] == "Quorum" {
			if row["body"] != "quorum of the board" {
				t.Errorf("tenant chunk body in graph_recall = %v, want the chunk's text", row["body"])
			}
			return
		}
	}
	t.Fatalf("the walk did not reach the tenant chunk: %v", out)
}
