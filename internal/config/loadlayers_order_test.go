package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadLayers_EnvRefusalsReportInLoadOrder pins which bad environment value
// LoadLayers reports when several are wrong at once: the first one in the
// order the env blocks are read. An operator fixes them one boot at a time, so
// the order is what they see, and a reshuffle of the env blocks would change
// it silently. Each step clears the value just reported and expects the next.
func TestLoadLayers_EnvRefusalsReportInLoadOrder(t *testing.T) {
	bad := []struct {
		env, value, want string
		fixed            string // the value that clears it; "" unsets it
	}{
		{env: "LOOMCYCLE_AGENTS_ROOT", value: filepath.Join(t.TempDir(), "absent"), want: "agents discovery:"},
		{env: "LOOMCYCLE_READ_ROOT", value: "/tmp", want: "LOOMCYCLE_READ_ROOT retired"},
		{env: "LOOMCYCLE_AUDIT_STORE", value: "yes", want: "LOOMCYCLE_AUDIT_STORE:"},
		{env: "LOOMCYCLE_AUDIT_RETENTION_DAYS", value: "-1", want: "LOOMCYCLE_AUDIT_RETENTION_DAYS:"},
		{env: "LOOMCYCLE_GIT_PUSH_MAX_BYTES", value: "lots", want: "LOOMCYCLE_GIT_PUSH_MAX_BYTES:"},
		{env: "LOOMCYCLE_FILE_MAX_BYTES", value: "0", want: "LOOMCYCLE_FILE_MAX_BYTES:"},
		{env: "LOOMCYCLE_MCP_MAX_FRAME_BYTES", value: "-3", want: "LOOMCYCLE_MCP_MAX_FRAME_BYTES:"},
		{env: "LOOMCYCLE_EXTRACT_CONCURRENCY", value: "many", want: "LOOMCYCLE_EXTRACT_CONCURRENCY:"},
		{env: "LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT", value: "60", want: "LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT:"},
		{env: "LOOMCYCLE_BATCH_JOIN_TIMEOUT", value: "60", want: "LOOMCYCLE_BATCH_JOIN_TIMEOUT:"},
		{env: "LOOMCYCLE_A2A_TENANCY_ROUTING", value: "query", want: "LOOMCYCLE_A2A_TENANCY_ROUTING:"},
		{env: "LOOMCYCLE_A2A_SERVER_CARD", value: "", want: "requires LOOMCYCLE_A2A_SERVER_CARD", fixed: "card"},
		{env: "LOOMCYCLE_REPLICA_ID", value: "bad id!", want: "LOOMCYCLE_REPLICA_ID:"},
		{env: "LOOMCYCLE_TRACE_URL_TEMPLATE", value: "https://trace.example/", want: "LOOMCYCLE_TRACE_URL_TEMPLATE:"},
	}
	t.Setenv("LOOMCYCLE_A2A_ENABLED", "1")
	for _, b := range bad {
		t.Setenv(b.env, b.value)
	}
	for _, b := range bad {
		_, err := LoadLayers(Layer{Name: "pin", Data: []byte("{}\n")})
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Fatalf("with %s and every later bad value set: err = %v, want it to name %q", b.env, err, b.want)
		}
		t.Setenv(b.env, b.fixed)
	}
	if _, err := LoadLayers(Layer{Name: "pin", Data: []byte("{}\n")}); err != nil {
		t.Fatalf("every bad value cleared: err = %v, want nil", err)
	}
}

// TestLoadLayers_ChatsRetentionInheritsTheUsageArchiverRead pins the one place
// an env block reads another's result: the chats-retention back-compat step
// adopts the legacy usage archiver's mode and age, which the usage sweeper
// block reads first. Read the other way round, the step sees no archiver and
// the operator's legacy config silently stops pruning aged chats.
func TestLoadLayers_ChatsRetentionInheritsTheUsageArchiverRead(t *testing.T) {
	t.Setenv("LOOMCYCLE_USAGE_RUN_RETENTION_MS", "86400000")
	t.Setenv("LOOMCYCLE_USAGE_RUN_RETENTION_MODE", "prune")
	t.Setenv("LOOMCYCLE_RETENTION_CHATS_MODE", "")
	cfg, err := LoadLayers(Layer{Name: "pin", Data: []byte("{}\n")})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Env.RetentionChatsMode != "prune" || cfg.Env.RetentionChatsMaxAge != 24*time.Hour {
		t.Fatalf("chats retention = (%q, %v), want (\"prune\", 24h) inherited from the usage archiver",
			cfg.Env.RetentionChatsMode, cfg.Env.RetentionChatsMaxAge)
	}
}
