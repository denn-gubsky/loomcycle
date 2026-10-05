package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A team definition carries its own agents' definitions, so it has its own,
// larger cap rather than sharing one agent definition's.
func TestTeamDefMaxDefinitionBytes_DefaultsAndEnv(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default: { model: claude-sonnet-4-6 }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func() Env {
		t.Helper()
		cfg, err := Load(yamlPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Env
	}
	if env := load(); env.TeamDefMaxDefinitionBytes != 1048576 || env.AgentDefMaxDefinitionBytes != 131072 {
		t.Errorf("defaults: team %d, agent %d; want 1048576 and 131072", env.TeamDefMaxDefinitionBytes, env.AgentDefMaxDefinitionBytes)
	}
	// Setting one must not move the other.
	t.Setenv("LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES", "2048")
	if env := load(); env.TeamDefMaxDefinitionBytes != 2048 || env.AgentDefMaxDefinitionBytes != 131072 {
		t.Errorf("team cap from env: team %d, agent %d; want 2048 and 131072", env.TeamDefMaxDefinitionBytes, env.AgentDefMaxDefinitionBytes)
	}
	t.Setenv("LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES", "0")
	if env := load(); env.TeamDefMaxDefinitionBytes != 0 {
		t.Errorf("0 should disable the cap; got %d", env.TeamDefMaxDefinitionBytes)
	}
}
