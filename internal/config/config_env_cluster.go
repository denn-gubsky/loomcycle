package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
	cfg.Env.CancelAckTimeoutMs = 5000
	if v := os.Getenv("LOOMCYCLE_CANCEL_ACK_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.CancelAckTimeoutMs = n
		}
	}

	cfg.Env.MetricsSweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MetricsSweepInterval = 0
			} else {
				cfg.Env.MetricsSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}

	// v0.12.3 Phase 4 pause-state cache TTL. Effective only when
	// LOOMCYCLE_REPLICA_ID is set (cluster mode); single-replica
	// pause.Manager skips the DB cache entirely.
	cfg.Env.PauseCacheTTLMs = 1000 // default 1s
	if v := os.Getenv("LOOMCYCLE_PAUSE_CACHE_TTL_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.PauseCacheTTLMs = n
		}
	}

	// RFC ES F1: the shutdown pause's bound. Unset ⇒ the default; an explicit
	// 0 turns the pause off, so it is told apart from unset.
	cfg.Env.ShutdownPauseTimeoutMs = DefaultShutdownPauseTimeoutMs
	if v := os.Getenv("LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.Env.ShutdownPauseTimeoutMs = n
		}
	}

	// v0.8.17 pause manager default timeout. 0 ⇒ pause package
	// default (30s). The manager itself clamps at pause.MaxPauseTimeout
	// regardless of what we set here.
	if v := os.Getenv("LOOMCYCLE_PAUSE_DEFAULT_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.PauseDefaultTimeoutMs = n
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
	cfg.Env.DynamicAgentDefaultTTLSeconds = 86400 // 24h
	if v := os.Getenv("LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Env.DynamicAgentDefaultTTLSeconds = n
		}
	}
	cfg.Env.DynamicAgentSweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.DynamicAgentSweepInterval = 0
			} else {
				cfg.Env.DynamicAgentSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}

	// RFC AH Phase 2b ephemeral-volume crash-recovery sweeper. Default 60s;
	// 0 disables (the inline run-completion purge still runs).
	cfg.Env.EphemeralVolumeSweepInterval = 60 * time.Second
	if v := os.Getenv("LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.EphemeralVolumeSweepInterval = 0
			} else {
				cfg.Env.EphemeralVolumeSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}
}
