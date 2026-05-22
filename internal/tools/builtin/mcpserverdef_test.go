package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// mcpServerDefFixture builds an MCPServerDef tool over in-memory
// SQLite + a stub Config with a permissive host allowlist. Returns
// the tool + a permissive operator ctx + cleanup.
func mcpServerDefFixture(t *testing.T) (*MCPServerDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Env: config.Env{
			HTTPHostAllowlist: []string{"n8n.example.com", "internal.example", "localhost"},
		},
		MCPServers: map[string]config.MCPServer{
			"yaml-stable": {Transport: "http", URL: "https://yaml.example/mcp"},
		},
	}
	tool := &MCPServerDef{
		Store:               s,
		Cfg:                 cfg,
		Registry:            loommcp.NewDynamicRegistry(),
		Pool:                nil, // tests don't exercise the pool surface
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_admin"})
	return tool, ctx, func() { _ = s.Close() }
}

func TestMCPServerDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"yaml-stable","overlay":{"transport":"http","url":"https://n8n.example.com/mcp"}}`))
	if !res.IsError {
		t.Fatalf("create over static yaml name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.MCPServers") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestMCPServerDefTool_CreateRefusedOnStdioTransport(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-stdio","overlay":{"transport":"stdio","url":"https://n8n.example.com/mcp"}}`))
	if !res.IsError {
		t.Fatalf("create with stdio transport should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "stdio") {
		t.Errorf("refusal should mention stdio; got %s", res.Text)
	}
}

func TestMCPServerDefTool_CreateRefusedOnHostNotInAllowlist(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-host","overlay":{"transport":"http","url":"https://evil.example.org/mcp"}}`))
	if !res.IsError {
		t.Fatalf("create with disallowed host should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "allowlist") {
		t.Errorf("refusal should mention allowlist; got %s", res.Text)
	}
}

// TestMCPServerDefTool_HostAllowlistMatchesCanonical pins the contract
// that this tool's allowlist semantics MATCH the canonical hostAllowed
// helper used by HTTP + WebFetch. Specifically: a bare allowlist entry
// "n8n.example.com" must also permit subdomains ("api.n8n.example.com")
// — the same behaviour an operator gets when the agent calls the URL
// via the HTTP tool. The previous bespoke matcher in this file required
// a leading dot for subdomain expansion and produced silent allow/deny
// divergence between the two tools on identical operator config.
func TestMCPServerDefTool_HostAllowlistMatchesCanonical(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	cases := []struct {
		name      string
		url       string
		shouldOK  bool
		shouldHit string
	}{
		// Bare entry "n8n.example.com" — exact + subdomain.
		{"bare-exact", "https://n8n.example.com/mcp", true, ""},
		{"bare-subdomain", "https://api.n8n.example.com/mcp", true, ""},
		// Bare entry "internal.example" — exact + subdomain (same rule).
		{"bare-exact-2", "https://internal.example/mcp", true, ""},
		{"bare-subdomain-2", "https://api.internal.example/mcp", true, ""},
		// Not on the list.
		{"unrelated-host", "https://evil.example.org/mcp", false, "allowlist"},
		// The classic "evil-prefix" attack the canonical matcher's
		// dot-anchored suffix is designed to defeat.
		{"prefix-attack", "https://evilexample.com/mcp", false, "allowlist"},
	}
	for i, tc := range cases {
		body := []byte(`{"op":"create","name":"probe-` + tc.name + `","overlay":{"transport":"http","url":"` + tc.url + `"}}`)
		res, _ := tool.Execute(ctx, body)
		gotOK := !res.IsError
		if gotOK != tc.shouldOK {
			t.Errorf("case %d %q (url=%s): IsError=%v want shouldOK=%v body=%s",
				i, tc.name, tc.url, res.IsError, tc.shouldOK, res.Text)
		}
		if !tc.shouldOK && tc.shouldHit != "" && !strings.Contains(res.Text, tc.shouldHit) {
			t.Errorf("case %d %q: refusal should mention %q; got %s", i, tc.name, tc.shouldHit, res.Text)
		}
	}
}

func TestMCPServerDefTool_CreateHappyPath(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"n8n-mailgun","overlay":{"transport":"streamable-http","url":"https://n8n.example.com/mcp/abc","headers":{"Authorization":"Bearer ${LOOMCYCLE_N8N_TOKEN}"}},"description":"n8n via MCP Server Trigger"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if name, _ := out["name"].(string); name != "n8n-mailgun" {
		t.Errorf("name = %v, want n8n-mailgun", out["name"])
	}
	if h, _ := out["content_sha256"].(string); !strings.HasPrefix(h, "sha256:") {
		t.Errorf("content_sha256 = %v", out["content_sha256"])
	}
	if promoted, _ := out["promoted"].(bool); !promoted {
		t.Error("create should default to promoted=true")
	}
	// Registry should now hold the entry.
	spec, ok := tool.Registry.Get("n8n-mailgun")
	if !ok {
		t.Fatal("registry doesn't have the new entry")
	}
	if spec.Transport != "streamable-http" || spec.URL != "https://n8n.example.com/mcp/abc" {
		t.Errorf("registry spec wrong: %+v", spec)
	}
}

func TestMCPServerDefTool_VerifyMatchesOnSameHash(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	createRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"n8n-x","overlay":{"transport":"http","url":"https://n8n.example.com/mcp"}}`))
	deployedHash := decodeResult(t, createRes.Text)["content_sha256"].(string)

	verifyRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"n8n-x","content_sha256":"`+deployedHash+`"}`))
	if verifyRes.IsError {
		t.Fatalf("verify: %s", verifyRes.Text)
	}
	out := decodeResult(t, verifyRes.Text)
	if matches, _ := out["matches"].(bool); !matches {
		t.Errorf("matches = false: %+v", out)
	}
	if deployed, _ := out["deployed"].(bool); !deployed {
		t.Error("deployed = false")
	}
}

func TestMCPServerDefTool_VerifyFalseOnUnknownName(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"never-existed","content_sha256":"sha256:abc"}`))
	if res.IsError {
		t.Fatalf("verify: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if m, _ := out["matches"].(bool); m {
		t.Error("matches=true on unknown name")
	}
	if d, _ := out["deployed"].(bool); d {
		t.Error("deployed=true on unknown name")
	}
}

func TestMCPServerDefTool_RetireRemovesFromRegistry(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	createRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"n8n-retire","overlay":{"transport":"http","url":"https://n8n.example.com/mcp"}}`))
	defID := decodeResult(t, createRes.Text)["def_id"].(string)
	if _, ok := tool.Registry.Get("n8n-retire"); !ok {
		t.Fatal("registry should have the entry after create")
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if _, ok := tool.Registry.Get("n8n-retire"); ok {
		t.Error("registry should NOT have the entry after retiring the active version")
	}
}

func TestMCPServerDefTool_GetSurfacesContentSHA256(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	createRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"n8n-y","overlay":{"transport":"http","url":"https://n8n.example.com/mcp"}}`))
	defID := decodeResult(t, createRes.Text)["def_id"].(string)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if h, _ := decodeResult(t, res.Text)["content_sha256"].(string); !strings.HasPrefix(h, "sha256:") {
		t.Errorf("get missing content_sha256")
	}
}
