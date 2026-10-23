package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The setting a call renders with is the one of the server the pool dials
// for that tenant: a static stdio server's own, a tenant's dynamic shadow over
// the shared base, the shared registration otherwise.
func TestMCPResultPolicy_FollowsTheServerThePoolDials(t *testing.T) {
	cfg := &config.Config{
		Env: config.Env{FileBase64MinBytes: 8192},
		MCPServers: map[string]config.MCPServer{
			"stdio-capture": {Transport: "stdio", Command: "x", BinaryResults: "capture"},
			"http-plain":    {Transport: "http", URL: "https://a.example/mcp"},
			"http-capture":  {Transport: "http", URL: "https://f.example/mcp", BinaryResults: "capture"},
		},
	}
	reg := mcp.NewDynamicRegistry()
	// A tenant registration never shadows a static stdio server at dial.
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "stdio-capture", Transport: "http", URL: "https://b.example/mcp"})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "http-plain", Transport: "http", URL: "https://c.example/mcp", BinaryResults: "capture"})
	reg.Set(mcp.DynamicMCPServerSpec{Name: "shared", Transport: "http", URL: "https://d.example/mcp", BinaryResults: "passthrough"})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "shared", Transport: "http", URL: "https://e.example/mcp", BinaryResults: "capture"})
	pol := mcpResultPolicy(cfg, mcpLookupView{reg})

	for _, c := range []struct {
		tenant, name string
		want         bool
	}{
		{"", "stdio-capture", true},
		{"acme", "stdio-capture", true},
		{"", "http-plain", false},
		{"acme", "http-plain", true},
		{"", "http-capture", true},
		{"acme", "http-capture", true},
		{"", "shared", false},
		{"acme", "shared", true},
		{"acme", "unknown", false},
	} {
		got := pol(c.tenant, c.name)
		if got.CaptureBase64 != c.want {
			t.Errorf("(%q, %q): capture = %v, want %v", c.tenant, c.name, got.CaptureBase64, c.want)
		}
		if got.Base64MinBytes != 8192 {
			t.Errorf("(%q, %q): min = %d, want the configured 8192", c.tenant, c.name, got.Base64MinBytes)
		}
	}
}
