package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	// Find the example yaml relative to repo root.
	wd, _ := os.Getwd()
	examplePath := filepath.Join(wd, "..", "..", "loomcycle.example.yaml")
	if _, err := os.Stat(examplePath); err != nil {
		t.Skip("example yaml not found")
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Defaults.Provider != "anthropic" {
		t.Errorf("defaults.provider = %q", cfg.Defaults.Provider)
	}
	if cfg.Concurrency.MaxConcurrentRuns != 8 {
		t.Errorf("concurrency.max_concurrent_runs = %d", cfg.Concurrency.MaxConcurrentRuns)
	}
	if cfg.Env.AnthropicAPIKey != "sk-test" {
		t.Errorf("env not loaded: %q", cfg.Env.AnthropicAPIKey)
	}

	provider, model, err := cfg.ResolveAgentModel("default")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if provider != "anthropic" || model != "claude-opus-4-7" {
		t.Errorf("resolved (%s, %s)", provider, model)
	}
}

func TestEnvExpansion(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default: { model: claude-sonnet-4-6 }
mcp_servers:
  brave:
    transport: stdio
    command: npx
    args: [-y, "@example/brave"]
    env: { BRAVE_API_KEY: "${BRAVE_API_KEY}" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRAVE_API_KEY", "bsa-secret")
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MCPServers["brave"].Env["BRAVE_API_KEY"] != "bsa-secret" {
		t.Errorf("env interpolation failed: %v", cfg.MCPServers["brave"].Env)
	}
}

func TestValidationRejectsBadMCP(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: x }
mcp_servers:
  bad: { transport: http }
`), 0o600)
	_, err := Load(yamlPath)
	if err == nil {
		t.Fatal("expected validation error for missing url")
	}
}
