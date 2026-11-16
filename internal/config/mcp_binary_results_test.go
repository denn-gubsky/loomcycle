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

// binary_results_min_bytes reaches a capture server's config; absent reads as
// 0, the default minimum.
func TestLoad_MCPBinaryResultsMinBytesReachesTheServer(t *testing.T) {
	cfg, err := loadYAMLString(t, binaryResultsYAMLHead+"    binary_results: capture\n    binary_results_min_bytes: 256\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["ms365"].BinaryResultsMinBytes; got != 256 {
		t.Errorf("binary_results_min_bytes = %d, want 256", got)
	}
	cfg, err = loadYAMLString(t, binaryResultsYAMLHead+"    binary_results: capture\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["ms365"].BinaryResultsMinBytes; got != 0 {
		t.Errorf("absent binary_results_min_bytes = %d, want 0", got)
	}
}

// A minimum under the floor would make an ordinary id a candidate, and one
// set without capture does nothing: both refuse the load, naming the server,
// the key and (for the floor) the floor.
func TestLoad_MCPBinaryResultsMinBytesRefused(t *testing.T) {
	for _, tc := range []struct{ yaml, says string }{
		{"    binary_results: capture\n    binary_results_min_bytes: 100\n", "128"},
		{"    binary_results: capture\n    binary_results_min_bytes: -1\n", "128"},
		{"    binary_results_min_bytes: 256\n", "capture"},
		{"    binary_results: passthrough\n    binary_results_min_bytes: 256\n", "capture"},
	} {
		_, err := loadYAMLString(t, binaryResultsYAMLHead+tc.yaml)
		if err == nil {
			t.Errorf("%q: loaded", tc.yaml)
			continue
		}
		for _, want := range []string{"mcp_servers.ms365", "binary_results_min_bytes", tc.says} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: err = %v, want it to say %q", tc.yaml, err, want)
			}
		}
	}
	if _, err := loadYAMLString(t, binaryResultsYAMLHead+"    binary_results: capture\n    binary_results_min_bytes: 128\n"); err != nil {
		t.Errorf("the floor itself: %v", err)
	}
}
