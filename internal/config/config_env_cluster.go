package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// applyEnvCluster reads the replica id, trace link template, cancel ack,
// metrics sweep and pause knobs.
func applyEnvCluster(cfg *Config) error {
	// v0.12.0 multi-replica HA: cluster mode activates when REPLICA_ID
	// is set. Validation is by coord.ValidateReplicaID; we re-implement
	// the regex here to avoid an import cycle (coord depends on config
	// via Env propagation in main.go, not the other way around).
	cfg.Env.ReplicaID = os.Getenv("LOOMCYCLE_REPLICA_ID")
	if cfg.Env.ReplicaID != "" {
		if err := validateReplicaID(cfg.Env.ReplicaID); err != nil {
			return fmt.Errorf("LOOMCYCLE_REPLICA_ID: %w", err)
		}
	}
	cfg.Env.TraceURLTemplate = strings.TrimSpace(os.Getenv("LOOMCYCLE_TRACE_URL_TEMPLATE"))
	if err := validateTraceURLTemplate(cfg.Env.TraceURLTemplate); err != nil {
		return fmt.Errorf("LOOMCYCLE_TRACE_URL_TEMPLATE: %w", err)
	}

	// RFC ES F1: the shutdown pause's bound. Unset ⇒ the default; an explicit
	// 0 turns the pause off, so it is told apart from unset.
	cfg.Env.ShutdownPauseTimeoutMs = DefaultShutdownPauseTimeoutMs
	if v := os.Getenv("LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.Env.ShutdownPauseTimeoutMs = n
		}
	}
	return nil
}

// applyEnvMCP reads the MCP registration policy, secret redaction, dynamic
// agent and ephemeral-volume sweep knobs.
func applyEnvMCP(cfg *Config) {
	// v0.8.15 LoomCycle MCP: dynamic agent registration policy.
	cfg.Env.MCPAllowPrivilegedTools = os.Getenv("LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS") == "1"
	cfg.Env.MCPAllowDynamicStdio = os.Getenv("LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO") == "1"
	cfg.Env.MCPDialUnattributedEnv = !getenvBool("LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV", true)
	// F32: default-ON. Only an explicit "0" disables redaction; an unset var
	// keeps the secure posture (secrets masked before persistence).
	cfg.Env.RedactSecrets = os.Getenv("LOOMCYCLE_REDACT_SECRETS") != "0"
}
