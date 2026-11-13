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
	// Usage rollup-and-prune sweeper (RFC AV Phase 2b). The New()
	// constructor applies the 720h fallback when this stays 0.
	if v := os.Getenv("LOOMCYCLE_USAGE_DETAIL_RETENTION_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.UsageDetailRetention = time.Duration(n) * time.Millisecond
		}
	}
}

// applyEnvRetention reads the retention sweeper and audit-store knobs.
func applyEnvRetention(cfg *Config) error {
	// RFC BM data-retention sweeper (opt-in; default OFF: every family's mode
	// defaults to off). The New() constructor applies the 1h interval fallback
	// when RetentionInterval stays 0.
	// RFC FZ Part E: the store-backed audit stream and its retention. A typo
	// fails the load rather than leaving an audit posture other than the one
	// asked for.
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
	if v := os.Getenv("LOOMCYCLE_RETENTION_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.RetentionInterval = time.Duration(n) * time.Millisecond
		}
	}
	cfg.Env.RetentionExportDir = os.Getenv("LOOMCYCLE_RETENTION_EXPORT_DIR")
	if v := os.Getenv("LOOMCYCLE_RETENTION_DEFS_MODE"); v != "" {
		cfg.Env.RetentionDefsMode = v
	}
	if v := os.Getenv("LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.RetentionDefsMaxAge = time.Duration(n) * time.Millisecond
		}
	}
	// keep-last-N accepts 0 (purge all); only a non-empty, non-negative value
	// overrides the default-5 set above.
	if v := os.Getenv("LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Env.RetentionDefsKeepLastN = n
		}
	}
	// RFC BM Phase 2 aged-chat archiver (opt-in; default OFF).
	if v := os.Getenv("LOOMCYCLE_RETENTION_CHATS_MODE"); v != "" {
		cfg.Env.RetentionChatsMode = v
	}
	if v := os.Getenv("LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.RetentionChatsMaxAge = time.Duration(n) * time.Millisecond
		}
	}
	// RFC BM Phase 3 retired-agent memory reclamation (opt-in; default OFF).
	if v := os.Getenv("LOOMCYCLE_RETENTION_MEM_MODE"); v != "" {
		cfg.Env.RetentionMemMode = v
	}
	if v := os.Getenv("LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.RetentionMemMaxAge = time.Duration(n) * time.Millisecond
		}
	}
	// RFC BL P4d class-aware memory-content prune (opt-in; default OFF).
	if v := os.Getenv("LOOMCYCLE_RETENTION_MEM_CONTENT_MODE"); v != "" {
		cfg.Env.RetentionMemContentMode = v
	}
	if v := os.Getenv("LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.RetentionMemContentMaxAge = time.Duration(n) * time.Millisecond
		}
	}
	// RFC CI trace index (opt-in; default OFF).
	if v := os.Getenv("LOOMCYCLE_MEMORY_TRACE_INDEX"); v != "" {
		cfg.Env.MemoryTraceIndexEnabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("LOOMCYCLE_MEMORY_TRACE_ASSISTANT"); v != "" {
		cfg.Env.MemoryTraceAssistantTurns = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.MemoryTraceMaxAge = time.Duration(n) * time.Millisecond
		}
	}
	// RFC CX-5 empty-dossier sweep (opt-in; default OFF).
	if v := os.Getenv("LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE"); v != "" {
		cfg.Env.RetentionEmptyDossierMode = v
	}
	if v := os.Getenv("LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.RetentionEmptyDossierMaxAge = time.Duration(n) * time.Millisecond
		}
	}
	cfg.Env.RetentionEnabled = retentionFamilyOn(
		cfg.Env.RetentionDefsMode,
		cfg.Env.RetentionChatsMode,
		cfg.Env.RetentionMemMode,
		cfg.Env.RetentionMemContentMode,
		cfg.Env.RetentionEmptyDossierMode,
	)
	return nil
}

// retentionFamilyOn reports whether any family's mode asks for deletion. The
// modes are the ones the sweeper acts on; anything else (unset, "off", a typo)
// is off there too, so it must not start the sweeper here.
func retentionFamilyOn(modes ...string) bool {
	for _, m := range modes {
		if m == "prune" || m == "export+prune" {
			return true
		}
	}
	return false
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
