package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// toolConnector routes the Path and Document RPCs to the real tools, granting
// the tenant scope as the HTTP connector's operator-trust ctx does, so the
// test covers what the gRPC transport does to RFC HA's arguments and fields.
type toolConnector struct {
	substrateMock
	path *builtin.Path
	doc  *builtin.Document
}

func grantTenant(ctx context.Context) context.Context {
	return tools.WithRunPolicy(ctx, tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
	})
}

func (c *toolConnector) Path(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	r, err := c.path.Execute(grantTenant(ctx), in)
	return connector.ToolResult{Text: r.Text, IsError: r.IsError}, err
}

func (c *toolConnector) Document(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	r, err := c.doc.Execute(grantTenant(ctx), in)
	return connector.ToolResult{Text: r.Text, IsError: r.IsError}, err
}

func TestGrpcPath_LsCountsAndFiltersParity(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	doc := &builtin.Document{Store: st, SqlMem: mgr}
	mc := &toolConnector{path: &builtin.Path{Store: st, Documents: doc}, doc: doc}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	call := func(rpc func(context.Context, *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error), in string) map[string]any {
		t.Helper()
		resp, err := rpc(context.Background(), &loomcyclepb.SubstrateRequest{InputJson: []byte(in)})
		if err != nil || resp.GetIsError() {
			t.Fatalf("%s: %v %s", in, err, resp.GetOutputJson())
		}
		var out map[string]any
		if err := json.Unmarshal(resp.GetOutputJson(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	documentRPC := func(ctx context.Context, r *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
		return client.Document(ctx, r)
	}
	pathRPC := func(ctx context.Context, r *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
		return client.Path(ctx, r)
	}
	for _, p := range []string{"/kb/a/x", "/kb/a/y", "/kb/b/z"} {
		call(documentRPC, `{"op":"create_document","scope":"tenant","title":"Doc `+p+`","path":"`+p+`","status":"done"}`)
	}
	call(documentRPC, `{"op":"create_document","scope":"tenant","title":"Draft","path":"/kb/b/w","status":"draft"}`)

	out := call(pathRPC, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true,"status":"draft"}`)
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("filtered entries over gRPC = %v", out)
	}
	if b := entries[0].(map[string]any); b["name"] != "b" || b["documents"] != float64(1) {
		t.Errorf("entry = %v, want b with 1", b)
	}
	if f, _ := out["filters"].(map[string]any); f["status"] != "draft" {
		t.Errorf("filters echo over gRPC = %v", out["filters"])
	}
	// The same call over the tool directly is the same bytes.
	direct, _ := mc.Path(grantTenant(context.Background()), json.RawMessage(`{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`))
	resp, err := client.Path(context.Background(), &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.GetOutputJson()) != direct.Text {
		t.Errorf("gRPC changed the answer:\n%s\n%s", resp.GetOutputJson(), direct.Text)
	}
	// query_chunks takes with_summary over gRPC.
	created := call(documentRPC, `{"op":"create_document","scope":"tenant","title":"Briefed","path":"/kb/c"}`)
	listed := call(documentRPC, `{"op":"query_chunks","scope":"tenant","document_id":"`+created["document_id"].(string)+`","with_summary":true}`)
	if chunks, _ := listed["chunks"].([]any); len(chunks) != 1 {
		t.Errorf("query_chunks with_summary over gRPC = %v", listed)
	}
}
