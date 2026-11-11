package config

import (
	"strings"
	"testing"
)

// TestLoadLayers_EnvRefusalsReportInLoadOrder pins which bad environment value
// LoadLayers reports when several are wrong at once: the first one in the
// order the env blocks are read. An operator fixes them one boot at a time, so
// the order is what they see, and a reshuffle of the env blocks would change
// it silently. Each step clears the value just reported and expects the next.
func TestLoadLayers_EnvRefusalsReportInLoadOrder(t *testing.T) {
	bad := []struct {
		env, value, want string
	}{
		{"LOOMCYCLE_READ_ROOT", "/tmp", "LOOMCYCLE_READ_ROOT retired"},
		{"LOOMCYCLE_AUDIT_STORE", "yes", "LOOMCYCLE_AUDIT_STORE:"},
		{"LOOMCYCLE_AUDIT_RETENTION_DAYS", "-1", "LOOMCYCLE_AUDIT_RETENTION_DAYS:"},
		{"LOOMCYCLE_GIT_PUSH_MAX_BYTES", "lots", "LOOMCYCLE_GIT_PUSH_MAX_BYTES:"},
		{"LOOMCYCLE_FILE_MAX_BYTES", "0", "LOOMCYCLE_FILE_MAX_BYTES:"},
		{"LOOMCYCLE_MCP_MAX_FRAME_BYTES", "-3", "LOOMCYCLE_MCP_MAX_FRAME_BYTES:"},
		{"LOOMCYCLE_EXTRACT_CONCURRENCY", "many", "LOOMCYCLE_EXTRACT_CONCURRENCY:"},
		{"LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT", "60", "LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT:"},
		{"LOOMCYCLE_BATCH_JOIN_TIMEOUT", "60", "LOOMCYCLE_BATCH_JOIN_TIMEOUT:"},
		{"LOOMCYCLE_A2A_TENANCY_ROUTING", "query", "LOOMCYCLE_A2A_TENANCY_ROUTING:"},
		{"LOOMCYCLE_REPLICA_ID", "bad id!", "LOOMCYCLE_REPLICA_ID:"},
		{"LOOMCYCLE_TRACE_URL_TEMPLATE", "https://trace.example/", "LOOMCYCLE_TRACE_URL_TEMPLATE:"},
	}
	t.Setenv("LOOMCYCLE_A2A_ENABLED", "1")
	t.Setenv("LOOMCYCLE_A2A_SERVER_CARD", "card")
	for _, b := range bad {
		t.Setenv(b.env, b.value)
	}
	for _, b := range bad {
		_, err := LoadLayers(Layer{Name: "pin", Data: []byte("{}\n")})
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Fatalf("with %s and every later bad value set: err = %v, want it to name %q", b.env, err, b.want)
		}
		t.Setenv(b.env, "")
	}
	if _, err := LoadLayers(Layer{Name: "pin", Data: []byte("{}\n")}); err != nil {
		t.Fatalf("every bad value cleared: err = %v, want nil", err)
	}
}
