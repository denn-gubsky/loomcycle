package config

import (
	"strings"
	"testing"
)

const binaryResultsYAMLHead = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default: { model: claude-sonnet-4-6 }
mcp_servers:
  ms365:
    transport: stdio
    command: npx
`

// binary_results reaches the server's config as written; absent reads as ""
// (passthrough).
func TestLoad_MCPBinaryResultsReachesTheServer(t *testing.T) {
	cfg, err := loadYAMLString(t, binaryResultsYAMLHead)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["ms365"].BinaryResults; got != "" {
		t.Errorf("absent binary_results = %q, want \"\"", got)
	}
	for _, v := range []string{"passthrough", "capture"} {
		cfg, err := loadYAMLString(t, binaryResultsYAMLHead+"    binary_results: "+v+"\n")
		if err != nil {
			t.Fatalf("binary_results: %s: %v", v, err)
		}
		if got := cfg.MCPServers["ms365"].BinaryResults; got != v {
			t.Errorf("binary_results = %q, want %q", got, v)
		}
	}
}

// A binary_results value the runtime does not know refuses the load: read as
// the default it would silently leave base64 in the model's context.
func TestLoad_MCPBinaryResultsUnknownValueRefused(t *testing.T) {
	_, err := loadYAMLString(t, binaryResultsYAMLHead+"    binary_results: decode\n")
	if err == nil || !strings.Contains(err.Error(), "mcp_servers.ms365") || !strings.Contains(err.Error(), "binary_results") {
		t.Fatalf("err = %v, want a load error naming the server and the field", err)
	}
}
