package http

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestUnifiedLibrary_MCP_StaticDefinitionNeverDisclosesInterpolatedSecrets
// loads a real yaml through the config loader, so the ${NAME} references are
// expanded the way a deployment expands them, and reads the static servers back
// as an operator and as a tenant operator. Neither body may carry a value the
// yaml took from the environment. The header, env, URL and fragment secrets sit
// under names the secret redactor does not recognise, so the field masking is
// what keeps them out; the arg secret has a secret-shaped name, which is the
// redactor's case.
func TestUnifiedLibrary_MCP_StaticDefinitionNeverDisclosesInterpolatedSecrets(t *testing.T) {
	secrets := map[string]string{
		"LOOMCYCLE_LIBTEST_BRIDGE":    "hdr-value-6f1c2a",
		"LOOMCYCLE_LIBTEST_PLAIN":     "plain-hdr-value-90ab",
		"LOOMCYCLE_LIBTEST_ENV":       "env-value-3d4e5f",
		"LOOMCYCLE_LIBTEST_USER":      "url-user-7a8b",
		"LOOMCYCLE_LIBTEST_PASS":      "url-pass-1c2d",
		"LOOMCYCLE_LIBTEST_QUERY":     "query-value-5e6f",
		"LOOMCYCLE_LIBTEST_FRAG":      "frag-value-9a0b",
		"LOOMCYCLE_LIBTEST_ARG_TOKEN": "arg-token-value-4c5d",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	yaml := `
mcp_servers:
  bridge:
    transport: http
    url: "https://${LOOMCYCLE_LIBTEST_USER}:${LOOMCYCLE_LIBTEST_PASS}@bridge.example/mcp?key=${LOOMCYCLE_LIBTEST_QUERY}&v=2#${LOOMCYCLE_LIBTEST_FRAG}"
    headers:
      Authorization: "Bearer ${LOOMCYCLE_LIBTEST_BRIDGE}"
      X-Api-Key: "${LOOMCYCLE_LIBTEST_PLAIN}"
  local:
    transport: stdio
    command: node
    args: ["server.js", "--token=${LOOMCYCLE_LIBTEST_ARG_TOKEN}"]
    env:
      BRIDGE: "${LOOMCYCLE_LIBTEST_ENV}"
`
	cfg, err := config.LoadLayers(config.Layer{Name: "libtest.yaml", Data: []byte(yaml)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	// The premise: the loader expanded the references, so the config holds
	// the values a read could disclose.
	if got := cfg.MCPServers["bridge"].Headers["Authorization"]; got != "Bearer "+secrets["LOOMCYCLE_LIBTEST_BRIDGE"] {
		t.Fatalf("premise: Authorization = %q, want the expanded value", got)
	}

	srv, _, cleanup := libraryUnifiedFixture(t, nil, cfg.MCPServers)
	defer cleanup()
	srv.redactor = newAtRestRedactor(os.Environ())

	for _, p := range []auth.Principal{
		{TenantID: "default", Subject: "ops", Scopes: []string{auth.ScopeAdmin}},
		{TenantID: "acme", Subject: "acme-op", Scopes: []string{auth.ScopeTenant}},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/_library/mcp-servers", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		srv.handleListLibraryMcpServers(rec, req)
		body := rec.Body.String()
		entries := decodeLibraryEntries(t, rec)
		if len(entries) != 2 || entries[0].Name != "bridge" {
			t.Fatalf("%s: want both static servers, body=%s", p.Subject, body)
		}
		for name, v := range secrets {
			if strings.Contains(body, v) {
				t.Errorf("%s: the body discloses %s", p.Subject, name)
			}
		}
		// What stays readable: the server's shape, the auth scheme, query keys.
		for _, want := range []string{
			`"Authorization":"Bearer ****"`,
			`"X-Api-Key":"****"`,
			`"BRIDGE":"****"`,
			`"server.js"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body lacks %s: %s", p.Subject, want, body)
			}
		}
		var bridge struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(entries[0].StaticDefinition, &bridge); err != nil {
			t.Fatal(err)
		}
		if want := "https://****@bridge.example/mcp?key=****&v=****#****"; bridge.URL != want {
			t.Errorf("%s: url = %q, want %q", p.Subject, bridge.URL, want)
		}
	}
}

func TestMaskURLSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"http://graph-bridge:8081/mcp":    "http://graph-bridge:8081/mcp",
		"https://u:p@h.example/x":         "https://****@h.example/x",
		"https://tok@h.example/x":         "https://****@h.example/x",
		"https://h.example/x?a=1&a=2&b=3": "https://h.example/x?a=****&a=****&b=****",
		"https://h.example/x?flag":        "https://h.example/x?flag=****",
		"https://h.example/x#secret":      "https://h.example/x#****",
		"https://h.example/%zz":           maskedValue,
	} {
		if got := maskURLSecrets(in); got != want {
			t.Errorf("maskURLSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"Bearer abc":       "Bearer ****",
		"Basic dXNlcjpw":   "Basic ****",
		"abc":              "****",
		"Bearer ":          "****",
		"tok=abc; v=1":     "****",
		"application/json": "****",
	} {
		if got := maskHeaderValue(in); got != want {
			t.Errorf("maskHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}
