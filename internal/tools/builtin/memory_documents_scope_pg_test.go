package builtin

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestMemorySearch_DocumentsScopeOnPostgres runs the documents leg on the tier
// that runs every leg for real: pgvector plus the full-text index (so the hybrid
// RRF path), the class predicates and the id-segment filter in SQL, and the
// tenant document rows at scope_id "".
func TestMemorySearch_DocumentsScopeOnPostgres(t *testing.T) {
	st, _ := pgVectorStoreOrSkip(t)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	emb := &countingEmbedder{Embedder: newFakeEmbedder("fake", "m1", "quorum", "board", "norm", "report")}
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus(), Embedder: emb}
	m := &Memory{Store: st, Embedder: emb, SqlMem: mgr, DocumentFilters: d}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}}
	})
	chunk := func(scope, path, body string) string {
		doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": scope, "title": "Doc", "path": path})
		return docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": scope,
			"document_id": doc["document_id"], "title": "Part", "body": body})["id"].(string)
	}
	tenantNorm := chunk("tenant", "/norms/board", "quorum board norm")
	tenantReport := chunk("tenant", "/reports/r1", "quorum report")
	userDoc := chunk("user", "/norms/mine", "quorum norm")
	for _, scope := range []string{"user", "tenant"} {
		if r := memRun(t, m, ctx, `{"op":"set","scope":"`+scope+`","key":"notes/q","value":"quorum board","embed":true}`); r.IsError {
			t.Fatalf("seed %s note: %s", scope, r.Text)
		}
	}
	hits := func(ctx context.Context, body string) (map[string]string, map[string]any) {
		t.Helper()
		r := memRun(t, m, ctx, body)
		if r.IsError {
			t.Fatalf("%s: %s", body, r.Text)
		}
		return dsHits(t, r)
	}

	emb.n.Store(0)
	got, _ := hits(ctx, `{"op":"recall","scope":"user","documents_scope":"tenant","query":"quorum board","top_k":50}`)
	if emb.n.Load() != 1 {
		t.Errorf("the query was embedded %d times, want once", emb.n.Load())
	}
	if got["notes/q"] != "user" || got[chunkBodyKey(tenantNorm)] != "tenant" || got[chunkBodyKey(tenantReport)] != "tenant" {
		t.Errorf("recall hits %v", got)
	}
	if _, ok := got[chunkBodyKey(userDoc)]; ok {
		t.Errorf("recall's default returned the user's own prose: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("recall hits %v, want the user note and the two tenant documents (no tenant note)", got)
	}

	got, out := hits(ctx, `{"op":"search","scope":"user","documents_scope":"tenant","under_paths":["/norms"],"query":"quorum board","top_k":50}`)
	if got[chunkBodyKey(tenantNorm)] != "tenant" || got[chunkBodyKey(userDoc)] != "user" || got["notes/q"] != "user" {
		t.Errorf("filtered search hits %v", got)
	}
	if _, ok := got[chunkBodyKey(tenantReport)]; ok {
		t.Errorf("under_paths did not reach the tenant leg: %v", got)
	}
	if echo, _ := out["filter"].(map[string]any); echo["scope"] != "tenant" {
		t.Errorf("filter echo %v, want the tenant leg's", out["filter"])
	}

	limited := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/reports/**"}})
	})
	got, _ = hits(limited, `{"op":"search","scope":"user","documents_scope":"tenant","query":"quorum board","top_k":50}`)
	if _, ok := got[chunkBodyKey(tenantReport)]; ok || got[chunkBodyKey(tenantNorm)] != "tenant" {
		t.Errorf("ACL on the tenant leg: %v", got)
	}

	// Equal scopes: the call with documents added to sources, byte for byte.
	sugar := memRun(t, m, ctx, `{"op":"recall","scope":"user","documents_scope":"user","query":"quorum board","top_k":50}`)
	plain := memRun(t, m, ctx, `{"op":"recall","scope":"user","sources":["facts","notes","documents"],"query":"quorum board","top_k":50}`)
	if sugar.IsError || sugar.Text != plain.Text {
		t.Errorf("sugar %s\nwant %s", sugar.Text, plain.Text)
	}
}
