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
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
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

// TestMCPServerDefTool_CreateAllowsPrivateAllowlistHost pins the fix for the
// dynamic-loopback-registration gap. A runtime `create` whose URL host is an
// operator-blessed loopback (HTTPPrivateHostAllowlist) must succeed even when
// that host is NOT in the general HTTPHostAllowlist — a self-hosted
// `http://localhost:3000/api/mcp` callback shouldn't force the operator to
// widen the general SSRF floor. This is the create-time/dial-time alignment:
// the HTTP tool already exempts private-allowlisted hosts at dial time.
//
// Fails on the pre-fix code, where hostAllowed consulted only
// HTTPHostAllowlist, so a loopback host blessed only via the private
// allowlist was refused at create (fail-soft → no mcp__jobs__* tools).
func TestMCPServerDefTool_CreateAllowsPrivateAllowlistHost(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()
	// Loopback host blessed ONLY in the private allowlist — deliberately
	// absent from the general floor.
	tool.Cfg.Env.HTTPHostAllowlist = []string{"n8n.example.com"}
	tool.Cfg.Env.HTTPPrivateHostAllowlist = []string{"localhost", "127.0.0.1"}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"jobs","overlay":{"transport":"http","url":"http://localhost:3000/api/mcp"}}`))
	if res.IsError {
		t.Fatalf("loopback host blessed via HTTPPrivateHostAllowlist should be allowed at create; got: %s", res.Text)
	}

	// Negative control: a private host in NEITHER allowlist is still refused
	// — the SSRF floor is preserved, only the operator-declared private
	// exemption is honoured.
	res2, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"sneaky","overlay":{"transport":"http","url":"http://10.0.0.5:9000/mcp"}}`))
	if !res2.IsError {
		t.Errorf("private host in neither allowlist must still be refused; got: %s", res2.Text)
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

// TestMCPServerDefTool_CreateIdempotentOnSameContent — re-creating identical
// content (a consumer blindly re-registering on every restart) is a no-op:
// it returns the active def with deduplicated=true and mints NO new version.
func TestMCPServerDefTool_CreateIdempotentOnSameContent(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	body := `{"op":"create","name":"jobs","overlay":{"transport":"http","url":"http://internal.example/mcp","headers":{"Authorization":"Bearer ${run.credentials.jobs:-${LOOMCYCLE_X}}"}}}`
	if r, _ := tool.Execute(ctx, json.RawMessage(body)); r.IsError {
		t.Fatalf("first create: %s", r.Text)
	}
	r2, _ := tool.Execute(ctx, json.RawMessage(body))
	if r2.IsError {
		t.Fatalf("second create: %s", r2.Text)
	}
	if decodeResult(t, r2.Text)["deduplicated"] != true {
		t.Errorf("identical re-create should be a dedup no-op; got %s", r2.Text)
	}
	lr, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"jobs"}`))
	vs, _ := decodeResult(t, lr.Text)["versions"].([]any)
	if len(vs) != 1 {
		t.Errorf("identical re-create must not mint a new version; got %d", len(vs))
	}
}

// TestMCPServerDefTool_RediscoverNoopOnUnchangedTools — rediscover mints a new
// version only when the peer's tool surface actually changes. The first
// rediscover (none → check_user) mints; a second with the same tools is a
// no-op (deduplicated=true), so re-discovery on every boot doesn't spam.
func TestMCPServerDefTool_RediscoverNoopOnUnchangedTools(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	srv := mcptest.NewServer(t, mcptest.WithToolName("check_user"))
	tool.Cfg.Env.HTTPHostAllowlist = append(tool.Cfg.Env.HTTPHostAllowlist, "127.0.0.1")
	tool.Pool = loommcp.NewPool(
		func(name string) (loommcp.Caller, error) { return mcphttp.New(mcphttp.Config{URL: srv.URL}) },
		func(c loommcp.Caller) {},
	)
	t.Cleanup(tool.Pool.Close)

	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"jobs","overlay":{"transport":"http","url":"`+srv.URL+`"}}`)); r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"rediscover","name":"jobs"}`)); r.IsError {
		t.Fatalf("rediscover#1: %s", r.Text)
	}
	r2, _ := tool.Execute(ctx, json.RawMessage(`{"op":"rediscover","name":"jobs"}`))
	if r2.IsError {
		t.Fatalf("rediscover#2: %s", r2.Text)
	}
	if decodeResult(t, r2.Text)["deduplicated"] != true {
		t.Errorf("rediscover with unchanged tools should be a no-op; got %s", r2.Text)
	}
	lr, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"jobs"}`))
	vs, _ := decodeResult(t, lr.Text)["versions"].([]any)
	if len(vs) != 2 { // v1 create + v2 first-rediscover; second rediscover adds nothing
		t.Errorf("unchanged rediscover must not mint a version; got %d (want 2)", len(vs))
	}
}

// TestCanonicalTools_OrderAndWhitespaceInsensitive pins the rediscover-dedup
// comparison: tool order and input_schema JSON formatting/key-order must not
// register as a change, but a genuinely different schema must.
func TestCanonicalTools_OrderAndWhitespaceInsensitive(t *testing.T) {
	a := []toolDescriptor{
		{Name: "b", InputSchema: json.RawMessage(`{"type":"object","x":1}`)},
		{Name: "a", InputSchema: json.RawMessage(`{ "type" : "object" }`)},
	}
	b := []toolDescriptor{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", InputSchema: json.RawMessage(`{"x":1,"type":"object"}`)},
	}
	if canonicalTools(a) != canonicalTools(b) {
		t.Errorf("reordered + reformatted identical tools should be canonically equal:\n a=%s\n b=%s", canonicalTools(a), canonicalTools(b))
	}
	c := []toolDescriptor{{Name: "a", InputSchema: json.RawMessage(`{"type":"string"}`)}}
	if canonicalTools(a) == canonicalTools(c) {
		t.Error("genuinely different tool sets must not compare equal")
	}
}

// TestMCPServerDefTool_CreateExpandsInnerLoomcycleEnv pins the dynamic-vs-yaml
// env-expansion symmetry. A yaml MCP server's header is expanded at
// config.Load (the whole document passes through expandEnv); a dynamically-
// registered one never passes through Load. Without expansion at create, the
// inner ${LOOMCYCLE_*} in a header like
//
//	Bearer ${run.credentials.jobs:-${LOOMCYCLE_JOBS_SEARCH_API_TOKEN}}
//
// is stored verbatim, and the request-time substituter's lazy `.*?` fallback
// (internal/tools/mcp/http/substitute.go) then truncates on the inner `}` and
// sends `Bearer ${LOOMCYCLE_…}` as a literal → 401 upstream.
//
// Fails on the pre-fix code, which stored the nested-brace template verbatim:
// the want-strings below would not match.
func TestMCPServerDefTool_CreateExpandsInnerLoomcycleEnv(t *testing.T) {
	t.Setenv("LOOMCYCLE_JOBS_SEARCH_API_TOKEN", "tok-abc123")
	t.Setenv("LOOMCYCLE_JOBS_MCP_HOST", "internal.example") // in the fixture allowlist

	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	body := `{"op":"create","name":"jobs","overlay":{` +
		`"transport":"http",` +
		`"url":"https://${LOOMCYCLE_JOBS_MCP_HOST}/mcp",` +
		`"headers":{"Authorization":"Bearer ${run.credentials.jobs:-${LOOMCYCLE_JOBS_SEARCH_API_TOKEN}}"}}}`
	res, _ := tool.Execute(ctx, json.RawMessage(body))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	// Read the stored definition back and assert the inner LOOMCYCLE var was
	// resolved while the outer ${run.credentials.*} token survived for
	// request-time substitution — i.e. the stored header is now FLAT (no
	// nested brace), which is exactly what substitute.go's lazy regex needs.
	active, err := tool.Store.MCPServerDefGetActive(ctx, "jobs")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	var ov mcpServerOverlay
	if err := json.Unmarshal(active.Definition, &ov); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}

	if want := "https://internal.example/mcp"; ov.URL != want {
		t.Errorf("URL not expanded: got %q, want %q", ov.URL, want)
	}
	gotHdr := ov.Headers["Authorization"]
	if want := "Bearer ${run.credentials.jobs:-tok-abc123}"; gotHdr != want {
		t.Fatalf("Authorization header:\n got: %q\nwant: %q", gotHdr, want)
	}
	// Belt-and-suspenders: no nested brace remains, and the secret is not a
	// literal placeholder anymore.
	if strings.Contains(gotHdr, "${LOOMCYCLE_") {
		t.Errorf("inner LOOMCYCLE var left unresolved in stored header: %q", gotHdr)
	}
	if !strings.Contains(gotHdr, "${run.credentials.jobs:-") {
		t.Errorf("outer run-credentials token did not survive expansion: %q", gotHdr)
	}
}
