package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// One meta-tool per access-control family, named as the family, whose op
// enum is the op table and whose description names every op (an MCP
// client reads the description as the documentation).
func TestACLTools_OnePerFamilyWithTheTablesOps(t *testing.T) {
	byName := map[string]loommcp.ToolDescriptor{}
	for _, td := range toolDescriptors() {
		byName[td.Name] = td
	}
	for _, family := range connector.ACLManagementFamilies() {
		td, ok := byName[family]
		if !ok {
			t.Errorf("no MCP tool for family %s", family)
			continue
		}
		if _, ok := handlersByName[family]; !ok {
			t.Errorf("tool %s is listed but not dispatchable", family)
		}
		var schema struct {
			Properties struct {
				Op struct {
					Enum []string `json:"enum"`
				} `json:"op"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(td.InputSchema, &schema); err != nil {
			t.Errorf("%s schema is not JSON: %v", family, err)
			continue
		}
		if want := connector.ACLManagementOps(family); !slices.Equal(schema.Properties.Op.Enum, want) {
			t.Errorf("%s op enum = %v, want the table's %v", family, schema.Properties.Op.Enum, want)
		}
		if !slices.Contains(schema.Required, "op") {
			t.Errorf("%s schema does not require op", family)
		}
		for _, op := range connector.ACLManagementOps(family) {
			if !strings.Contains(td.Description, op) {
				t.Errorf("the %s description does not mention op %s", family, op)
			}
		}
	}
}

// The tools are gated as their HTTP routes: groups, lists and bindings are
// the tenant operator's; any principal, an isolated user included, may ask
// the authz questions about itself.
func TestACLTools_GatedAsTheirRoutes(t *testing.T) {
	principals := map[string]auth.Principal{
		"admin":    {TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}},
		"operator": {TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}},
		"member":   {TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}},
		"isolated": {TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser}},
	}
	for _, c := range []struct {
		tool  string
		allow map[string]bool
	}{
		{"groupdef", map[string]bool{"admin": true, "operator": true}},
		{"acldef", map[string]bool{"admin": true, "operator": true}},
		{"aclbinding", map[string]bool{"admin": true, "operator": true}},
		{"authz", map[string]bool{"admin": true, "operator": true, "member": true, "isolated": true}},
	} {
		for who, p := range principals {
			ctx := auth.WithPrincipal(context.Background(), p)
			if got := principalMayCallTool(ctx, c.tool); got != c.allow[who] {
				t.Errorf("%s may call %s = %v, want %v", who, c.tool, got, c.allow[who])
			}
		}
	}
}
