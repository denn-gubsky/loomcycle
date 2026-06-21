package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func documentFixture(t *testing.T) (*Document, context.Context, store.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	return &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}, ctx, s
}

func docExec(t *testing.T, d *Document, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		_ = json.Unmarshal([]byte(res.Text), &out)
	}
	return out, res
}

func TestDocument_EndToEnd(t *testing.T) {
	d, ctx, s := documentFixture(t)

	// create_document with a Path-tree name.
	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"Launch plan","path":"/docs/launch"}`)
	if res.IsError {
		t.Fatalf("create_document: %q", res.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	if docID == "" || root == "" || out["path"] != "/docs/launch" {
		t.Fatalf("create_document out = %s", res.Text)
	}
	// The Path-tree dirent exists (agent scope).
	if _, derr := s.DirentGet(context.Background(), "tnt", "agent", "doc-agent", "/docs/", "launch"); derr != nil {
		t.Errorf("document dirent not registered: %v", derr)
	}

	// create a chunk under the root.
	out, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"agent","document_id":"`+docID+`","parent_id":"`+root+`","type":"publication","title":"Day 1","body":"# hi","status":"draft"}`)
	if res.IsError {
		t.Fatalf("create_chunk: %q", res.Text)
	}
	chunkID, _ := out["id"].(string)
	if out["title"] != "Day 1" || out["type"] != "publication" || out["body"] != "# hi" || int(out["revision"].(float64)) != 1 {
		t.Errorf("chunk out = %s", res.Text)
	}

	// query by type+status.
	out, res = docExec(t, d, ctx, `{"op":"query_chunks","scope":"agent","type":"publication","status":"draft"}`)
	if res.IsError {
		t.Fatalf("query_chunks: %q", res.Text)
	}
	if rows, _ := out["chunks"].([]any); len(rows) != 1 {
		t.Errorf("query by type/status = %s", res.Text)
	}

	// query by under_path (the Path glue).
	out, res = docExec(t, d, ctx, `{"op":"query_chunks","scope":"agent","under_path":"/docs"}`)
	if res.IsError {
		t.Fatalf("query under_path: %q", res.Text)
	}
	if rows, _ := out["chunks"].([]any); len(rows) != 2 { // root + Day 1
		t.Errorf("under_path /docs = %d chunks, want 2: %s", len(rows), res.Text)
	}

	// get_document by path.
	out, res = docExec(t, d, ctx, `{"op":"get_document","scope":"agent","path":"/docs/launch"}`)
	if res.IsError || out["document_id"] != docID {
		t.Errorf("get_document by path = %s", res.Text)
	}
	_ = chunkID
}

func TestDocument_UpdateRevisionConflict(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"c","body":"x"}`)
	cid := out["id"].(string)

	// Correct revision (1) succeeds, bumps to 2.
	out, res := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cid+`","revision":1,"body":"y"}`)
	if res.IsError || int(out["revision"].(float64)) != 2 || out["body"] != "y" {
		t.Fatalf("update rev1: %s", res.Text)
	}
	// Stale revision (1 again) conflicts.
	_, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cid+`","revision":1,"body":"z"}`)
	if !res.IsError || !strings.Contains(res.Text, "revision conflict") {
		t.Errorf("stale revision should conflict; got %q", res.Text)
	}
}

func TestDocument_DeleteChunkCascade(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"parent","body":"p"}`)
	parent := out["id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+parent+`","title":"child","body":"c"}`)
	child := out["id"].(string)

	out, res := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+parent+`"}`)
	if res.IsError || int(out["cascade_deleted_descendants"].(float64)) != 1 {
		t.Fatalf("delete cascade: %s", res.Text)
	}
	// Both parent + child gone (rows AND bodies).
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+parent+`"}`); !r.IsError {
		t.Errorf("parent should be gone")
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+child+`"}`); !r.IsError {
		t.Errorf("child should be gone (cascade)")
	}
}

func TestDocument_EdgesAndTypes(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	mk := func(title string) string {
		o, _ := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"`+title+`","body":""}`)
		return o["id"].(string)
	}
	a, b := mk("a"), mk("b")
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+a+`","to_id":"`+b+`","kind":"promotes"}`); r.IsError {
		t.Fatalf("link: %q", r.Text)
	}
	// Verify via the sql escape hatch.
	out, res := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT from_id, to_id, kind FROM chunk_edges"}`)
	if res.IsError {
		t.Fatalf("sql edge query: %q", res.Text)
	}
	if rows, _ := out["rows"].([]any); len(rows) != 1 {
		t.Errorf("expected 1 edge, got %s", res.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"unlink_chunks","scope":"user","from_id":"`+a+`","to_id":"`+b+`","kind":"promotes"}`); r.IsError {
		t.Fatalf("unlink: %q", r.Text)
	}
	// types
	if _, r := docExec(t, d, ctx, `{"op":"define_type","scope":"user","name":"publication","fields":[{"name":"platform","type":"string"}]}`); r.IsError {
		t.Fatalf("define_type: %q", r.Text)
	}
	out, res = docExec(t, d, ctx, `{"op":"list_types","scope":"user"}`)
	if res.IsError {
		t.Fatalf("list_types: %q", res.Text)
	}
	if ts, _ := out["types"].([]any); len(ts) != 1 {
		t.Errorf("expected 1 type, got %s", res.Text)
	}
}

// Tenant + scope isolation: a doc in tenant "tnt" agent scope is invisible to
// another tenant (separate SQL Memory scope DB), and to the user scope.
func TestDocument_ScopeTenantIsolation(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"secret"}`)
	docID := out["document_id"].(string)

	// Different tenant → its agent-scope DB doesn't have the doc.
	ctxB := tools.WithAgentName(context.Background(), "doc-agent")
	ctxB = tools.WithRunIdentity(ctxB, tools.RunIdentityValue{AgentID: "a", TenantID: "other"})
	if _, r := docExec(t, d, ctxB, `{"op":"get_document","scope":"agent","id":"`+docID+`"}`); !r.IsError {
		t.Errorf("cross-tenant get_document must 404; got %q", r.Text)
	}
	// Same tenant, user scope → different DB, doesn't have the agent-scope doc.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`); !r.IsError {
		t.Errorf("cross-scope get_document must 404; got %q", r.Text)
	}
	// The owner sees it.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"agent","id":"`+docID+`"}`); r.IsError {
		t.Errorf("owner should see its doc: %q", r.Text)
	}
}

func TestDocument_TenantScopeRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"x"}`); !r.IsError || !strings.Contains(r.Text, "not yet supported") {
		t.Errorf("scope=tenant should be refused; got %q", r.Text)
	}
}

func TestDocument_SqlEscapeHatchGated(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`) // ensure schema exists
	// ATTACH must be refused by the SQL Memory validator.
	if _, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"ATTACH DATABASE 'x' AS y"}`); !r.IsError {
		t.Errorf("ATTACH via the sql escape hatch must be refused; got %q", r.Text)
	}
}

func TestDocument_RequiresSqlMem(t *testing.T) {
	d := &Document{} // no Store, no SqlMem
	res, _ := d.Execute(context.Background(), json.RawMessage(`{"op":"create_document","title":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "SQL Memory") {
		t.Errorf("Document without SQL Memory should refuse; got %q", res.Text)
	}
}
