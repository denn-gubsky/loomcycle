package builtin

import (
	"context"
	"strings"
	"testing"
)

// TestMCPServerDefPrivateHostWarnings_ListsTenantServersTheGuardWillRefuse:
// a server a tenant registered at runtime dials through the egress guard, so
// an active one whose host is localhost or a private IP the operator has not
// vouched for is refused at every call. Boot names each, with the fix. An
// operator-authored version, a public host and a vouched private host are
// not listed.
func TestMCPServerDefPrivateHostWarnings_ListsTenantServersTheGuardWillRefuse(t *testing.T) {
	tool, done := mcpEnvRuleFixture(t)
	defer done()
	tool.Cfg.Env.HTTPPrivateHostAllowlist = []string{"10.0.0.5"}
	ip := plantMCPServerDef(t, tool, "acme", "local-ip", `{"transport":"http","url":"http://127.0.0.1:3000/mcp"}`)
	plantMCPServerDef(t, tool, "acme", "local-name", `{"transport":"http","url":"http://localhost:3000/mcp"}`)
	plantMCPServerDef(t, tool, "acme", "lan-vouched", `{"transport":"http","url":"http://10.0.0.5/mcp"}`)
	plantMCPServerDef(t, tool, "acme", "public", `{"transport":"http","url":"https://n8n.example.com/mcp"}`)
	plantMCPServerDef(t, tool, "acme", "blessed", `{"transport":"http","url":"http://127.0.0.1:3000/mcp","operator_authored":true}`)

	warns, err := tool.PrivateHostWarnings(context.Background())
	if err != nil {
		t.Fatalf("PrivateHostWarnings: %v", err)
	}
	if len(warns) != 2 {
		t.Fatalf("want local-ip and local-name, got %d: %q", len(warns), warns)
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{`"local-ip"`, `"local-name"`, ip.DefID, `tenant "acme"`, "LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST", "re-save"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q: %q", want, warns)
		}
	}
}
