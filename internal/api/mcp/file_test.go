package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// fileCapturingConnector records what a `file` call reaches the connector
// with: the input, and the principal and identity on its ctx.
type fileCapturingConnector struct {
	*mockConnector
	calls     int
	input     string
	principal auth.Principal
	hasPrinc  bool
	identity  tools.RunIdentityValue
}

func (c *fileCapturingConnector) File(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.calls++
	c.input = string(in)
	c.principal, c.hasPrinc = auth.PrincipalFromContext(ctx)
	c.identity = tools.RunIdentity(ctx)
	return connector.ToolResult{Text: `{"entries":[]}`}, nil
}

const fileArgs = `{"run_id":"r_1","op":"ls","path":"volume:inbox:documents"}`

const fileCallFrame = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file","arguments":` + fileArgs + `}}` + "\n"

// A principal that may read runs lists `file`, and its call reaches
// connector.File as sent, with the session's principal on ctx (the connector
// reads the run through it) and the identity mcpPrincipalCtx stamps from it.
// One without runs:read neither sees nor reaches it, as with get_run.
func TestFileMCP_ARunReaderReachesTheConnector(t *testing.T) {
	for _, c := range []struct {
		name      string
		principal *auth.Principal
		allowed   bool
	}{
		{"no principal (the stdio operator)", nil, true},
		{"an admin", &auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, true},
		{"a tenant operator", &auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}, true},
		{"a member that reads runs", &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsRead}}, true},
		{"a member that does not read runs", &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeChannelRead}}, false},
		{"an isolated user", &auth.Principal{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeUser}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			if c.principal != nil {
				ctx = auth.WithPrincipal(ctx, *c.principal)
			}
			if _, listed := listedTools(t, ctx)["file"]; listed != c.allowed {
				t.Errorf("file listed = %v, want %v", listed, c.allowed)
			}
			fc := &fileCapturingConnector{mockConnector: &mockConnector{}}
			srv := New(Config{Connector: fc, Logf: func(string, ...any) {}})
			resps, _ := driveServerCtx(t, srv, ctx, fileCallFrame)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			if !c.allowed {
				if resps[0].Error == nil || fc.calls != 0 {
					t.Fatalf("a refused call: error %+v after %d connector calls", resps[0].Error, fc.calls)
				}
				return
			}
			if resps[0].Error != nil || fc.calls != 1 {
				t.Fatalf("an allowed call: error %+v after %d connector calls", resps[0].Error, fc.calls)
			}
			if fc.input != fileArgs {
				t.Errorf("the connector received %s, want %s", fc.input, fileArgs)
			}
			if c.principal != nil {
				if !fc.hasPrinc || fc.principal.Subject != c.principal.Subject {
					t.Errorf("the connector saw principal %+v (present %v)", fc.principal, fc.hasPrinc)
				}
				if fc.identity.TenantID != c.principal.TenantID || fc.identity.UserID != c.principal.Subject {
					t.Errorf("the connector saw identity %+v, want the principal's", fc.identity)
				}
			}
		})
	}
}

// The schema is the File builtin's cut to the fields the connector takes:
// each shared property byte for byte the builtin's (bar the three whose
// wording speaks of an agent's own grants), the op enum the twin's ops, and
// run_id required.
func TestFileMCP_SchemaIsTheToolsCutToTheTwin(t *testing.T) {
	var got, base struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	var schema json.RawMessage
	for _, td := range toolDescriptors() {
		if td.Name == "file" {
			schema = td.InputSchema
		}
	}
	if schema == nil {
		t.Fatal("no file tool in the MCP surface")
	}
	if err := json.Unmarshal(schema, &got); err != nil {
		t.Fatalf("file schema is not valid JSON: %v", err)
	}
	if err := json.Unmarshal((&builtin.File{}).InputSchema(), &base); err != nil {
		t.Fatal(err)
	}
	if len(got.Properties) != len(connector.FileTwinFields) {
		t.Errorf("schema has %d properties, want the twin's %d", len(got.Properties), len(connector.FileTwinFields))
	}
	for _, name := range connector.FileTwinFields {
		prop, ok := got.Properties[name]
		if !ok {
			t.Errorf("schema lacks %q", name)
			continue
		}
		if _, overridden := fileMCPOverrides[name]; overridden || name == "op" {
			continue
		}
		if !bytes.Equal(compactJSON(t, prop), compactJSON(t, base.Properties[name])) {
			t.Errorf("property %q differs from the builtin's: %s", name, prop)
		}
	}
	var op struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(got.Properties["op"], &op); err != nil || !slices.Equal(op.Enum, connector.FileTwinOps) {
		t.Errorf("op enum = %v (%v), want %v", op.Enum, err, connector.FileTwinOps)
	}
	for _, f := range []string{"run_id", "op"} {
		if !slices.Contains(got.Required, f) {
			t.Errorf("%s is not required: %v", f, got.Required)
		}
	}
	for _, name := range []string{"url", "headers", "method", "ranged", "chunk_bytes", "max_bytes", "content_type"} {
		if _, ok := got.Properties[name]; ok {
			t.Errorf("schema offers %q, a download/upload field the twin refuses", name)
		}
	}
}

// The description is the documentation an MCP client reads: every op the twin
// takes is named in it.
func TestFileMCP_DescriptionListsEveryOp(t *testing.T) {
	desc := toolDescription(t, "file")
	for _, op := range connector.FileTwinOps {
		if !strings.Contains(desc, op+" (") {
			t.Errorf("the file description does not describe op %q", op)
		}
	}
}

// extract and import can outlast the thin client's header timeout, and a
// retry would run them twice (a second Markdown file, a second document).
func TestFileMCP_ThinClientNeverRetriesIt(t *testing.T) {
	if !longRunTools["file"] {
		t.Error("file is missing from longRunTools: a header timeout would retry an import mid-call")
	}
}
