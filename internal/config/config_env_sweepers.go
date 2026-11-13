package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// applyEnvSweepers reads the heartbeat, configured-run and usage sweeper
// knobs.
func applyEnvSweepers(cfg *Config) {
	if v := os.Getenv("LOOMCYCLE_HEARTBEAT_STALE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.HeartbeatStaleAfter = time.Duration(n) * time.Millisecond
		}
	}
	// RFC DI D5 configured runs: draft TTL and per-user cap. A value that does
	// not parse keeps the default; an explicit 0 turns the bound off.
	if v := os.Getenv("LOOMCYCLE_CONFIGURED_RUN_TTL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Env.ConfiguredRunTTL = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_MAX_CONFIGURED_RUNS_PER_USER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Env.MaxConfiguredRunsPerUser = n
		}
	}
}

// applyEnvAudit reads the store-backed audit stream's switch and retention.
// A typo fails the load rather than leaving an audit posture other than the
// one asked for.
func applyEnvAudit(cfg *Config) error {
	switch v := strings.TrimSpace(os.Getenv("LOOMCYCLE_AUDIT_STORE")); v {
	case "":
	case "1":
		cfg.Env.AuditStore = true
	case "0":
		cfg.Env.AuditStoreOff = true
	default:
		return fmt.Errorf("LOOMCYCLE_AUDIT_STORE: %q is not 1 or 0", v)
	}
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_AUDIT_RETENTION_DAYS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("LOOMCYCLE_AUDIT_RETENTION_DAYS: %q is not a number of days (0 keeps every row)", v)
		}
		cfg.Env.AuditRetention = time.Duration(n) * 24 * time.Hour
	}
	return nil
}

// applyEnvTraceIndex reads the conversation trace index switches. How long an
// indexed turn stays searchable is retention.trace.max_age in the yaml.
func applyEnvTraceIndex(cfg *Config) {
	if v := os.Getenv("LOOMCYCLE_MEMORY_TRACE_INDEX"); v != "" {
		cfg.Env.MemoryTraceIndexEnabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("LOOMCYCLE_MEMORY_TRACE_ASSISTANT"); v != "" {
		cfg.Env.MemoryTraceAssistantTurns = v == "1" || strings.EqualFold(v, "true")
	}
}

// applyEnvClusterSweeps reads the replica sweep cadences.
func applyEnvClusterSweeps(cfg *Config) {
	if v := os.Getenv("LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ReplicasSweepInterval = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("LOOMCYCLE_REPLICAS_STALE_AFTER_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ReplicasStaleAfter = time.Duration(n) * time.Millisecond
		}
	}
}
