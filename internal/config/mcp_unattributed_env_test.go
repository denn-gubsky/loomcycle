package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_MCPUnattributedEnvRefusedUnlessOptedOut pins the default and the
// parse of LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV: unset, empty, "1" and an
// unrecognised value all refuse (the opt-out stays off — a typo must not
// re-open the dial); only an explicit false-ish value opts out.
func TestLoad_MCPUnattributedEnvRefusedUnlessOptedOut(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(yamlPath, []byte("defaults: { provider: anthropic, model: claude-sonnet-4-6 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	const name = "LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV"

	cases := []struct {
		value   string
		unset   bool
		dialing bool
	}{
		{unset: true, dialing: false},
		{value: "", dialing: false},
		{value: "1", dialing: false},
		{value: "true", dialing: false},
		{value: "maybe", dialing: false},
		{value: "0", dialing: true},
		{value: "false", dialing: true},
		{value: " OFF ", dialing: true},
	}
	for _, c := range cases {
		label := c.value
		if c.unset {
			label = "<unset>"
		}
		t.Run(label, func(t *testing.T) {
			t.Setenv(name, c.value)
			if c.unset {
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(yamlPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Env.MCPDialUnattributedEnv != c.dialing {
				t.Errorf("%s=%q: MCPDialUnattributedEnv = %v, want %v", name, label, cfg.Env.MCPDialUnattributedEnv, c.dialing)
			}
		})
	}
}
