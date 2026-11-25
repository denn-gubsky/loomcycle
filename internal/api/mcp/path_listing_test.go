package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC HA over the MCP surface: the path tool advertises the new ls arguments
// (its schema is the tool's own, verbatim) and carries them to the tool and
// its answer back unchanged; the document tool takes with_summary on
// query_chunks.

type realPathConnector struct {
	*mockConnector
	path *builtin.Path
	doc  *builtin.Document
}

func tenantGranted(ctx context.Context) context.Context {
	return tools.WithRunPolicy(ctx, tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
	})
}

func (c *realPathConnector) Path(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	r, err := c.path.Execute(tenantGranted(ctx), in)
	return connector.ToolResult{Text: r.Text, IsError: r.IsError}, err
}

func (c *realPathConnector) Document(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	r, err := c.doc.Execute(tenantGranted(ctx), in)
	return connector.ToolResult{Text: r.Text, IsError: r.IsError}, err
}

func TestMCPPath_LsCountsAndFiltersParity(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(builtinSchema("path"), &schema); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"with_counts", "name_contains", "type", "status"} {
		if _, ok := schema.Properties[arg]; !ok {
			t.Errorf("the MCP path tool does not advertise %s", arg)
		}
	}

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
	doc := &builtin.Document{Store: st, SqlMem: mgr}
	cc := &realPathConnector{mockConnector: &mockConnector{}, path: &builtin.Path{Store: st, Documents: doc}, doc: doc}
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
	call := func(tool, args string) string {
		t.Helper()
		res, err := handlersByName[tool](op, &handlerEnv{connector: cc}, json.RawMessage(args))
		if err != nil || res.IsError {
			t.Fatalf("%s %s: %v %s", tool, args, err, dirText(res))
		}
		return dirText(res)
	}
	call("document", `{"op":"create_document","scope":"tenant","title":"Architecture","path":"/kb/notes/arch"}`)
	call("document", `{"op":"create_document","scope":"tenant","title":"Other","path":"/kb/misc/other"}`)

	text := call("path", `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true,"name_contains":"architecture"}`)
	var out struct {
		Entries []map[string]any  `json:"entries"`
		Filters map[string]string `json:"filters"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	if len(out.Entries) != 1 || out.Entries[0]["name"] != "notes" || out.Entries[0]["documents"] != float64(1) {
		t.Errorf("filtered, counted ls over MCP = %s", text)
	}
	if out.Filters["name_contains"] != "architecture" {
		t.Errorf("filters echo over MCP = %v", out.Filters)
	}
	listed := call("document", `{"op":"query_chunks","scope":"tenant","under_path":"/kb","with_summary":true}`)
	if !strings.Contains(listed, `"chunks"`) {
		t.Errorf("query_chunks with_summary over MCP = %s", listed)
	}
}
